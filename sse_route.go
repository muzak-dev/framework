package muzak

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Defaults applied to an event stream when [SSEOptions] leaves them unset.
// Each of them bounds something a client can hold on to, so none of them
// defaults to zero.
const (
	// DefaultSSEKeepAlive is how often a comment is written to an otherwise
	// idle stream, at fifteen seconds. The HTML specification suggests exactly
	// this, because a proxy that sees nothing on a connection for long enough
	// closes it, and a stream that says nothing for minutes at a time is
	// indistinguishable from one that has died.
	DefaultSSEKeepAlive = 15 * time.Second
	// DefaultSSEWriteTimeout bounds how long one event may take to reach the
	// client, at ten seconds. It is what stops a client that has stopped
	// reading from pinning a goroutine and a buffer for as long as it likes.
	DefaultSSEWriteTimeout = 10 * time.Second
	// DefaultSSEMaxStreams is how many event streams one application serves at
	// once by default, at 1024. Each stream holds a connection and a goroutine
	// for as long as the client cares to keep it, so the file descriptor
	// budget is what runs out first.
	DefaultSSEMaxStreams = 1024
	// DefaultSSEMaxStreamsPerIP is how many event streams a single client
	// address holds open at once by default, at 64, for the same reasons as
	// [DefaultWSMaxConnectionsPerIP]: generous for a legitimate browser
	// session, but small enough that one address can never take more than a
	// slice of the process-wide budget.
	DefaultSSEMaxStreamsPerIP = 64
)

// sseStreamLimit resolves how many streams an application will serve at once,
// where zero asks for the default and a negative value removes the limit.
func sseStreamLimit(configured int) int {
	switch {
	case configured == 0:
		return DefaultSSEMaxStreams
	case configured < 0:
		return 0
	default:
		return configured
	}
}

// sseStreamsPerIPLimit resolves how many streams a single client address may
// hold at once, where zero asks for the default and a negative value removes
// the limit.
func sseStreamsPerIPLimit(configured int) int {
	switch {
	case configured == 0:
		return DefaultSSEMaxStreamsPerIP
	case configured < 0:
		return 0
	default:
		return configured
	}
}

// SSEOptions configures the event streams of a route.
//
// It can be set application-wide through [AppOptions.SSE] and narrowed for a
// router or a single route with [WithSSE]. Layering works field by field:
// whatever a narrower scope leaves at its zero value it inherits, so a route
// that only lengthens the keepalive keeps the application's write timeout.
//
// The zero value is usable and safe: writes are bounded, an idle stream is
// held open by a periodic keepalive, and the number of streams one application
// serves at once is capped.
type SSEOptions struct {
	// KeepAlive is how often a comment is written to a stream that has sent
	// nothing, defaulting to [DefaultSSEKeepAlive]. A comment is ignored by
	// every client and is what keeps a proxy from closing a connection it
	// believes to be idle. A negative value turns keepalive off, which is only
	// appropriate for a stream that is never quiet.
	KeepAlive time.Duration

	// WriteTimeout bounds how long one event may take to reach the client,
	// defaulting to [DefaultSSEWriteTimeout]. It is the bound that matters
	// most here: a client that opens a stream and never reads it costs a
	// goroutine, a connection and a growing socket buffer until something
	// gives up, and this is what gives up. A negative value removes it, which
	// is only appropriate when something else imposes one.
	//
	// It is enforced through the response's write deadline, which a response
	// writer wrapped by middleware reaches only if every wrapper implements
	// Unwrap() http.ResponseWriter. A stream whose response cannot carry
	// deadlines is still served, because a writer supplied by a test cannot
	// either, but its timeout is not enforced and the first such stream on a
	// route logs a warning saying so. The bound covers one write: it says
	// nothing about how long a stream may last, which is set by the client, by
	// the handler and by [SSEOptions.MaxStreams] and MaxStreamsPerIP.
	WriteTimeout time.Duration

	// Retry is the reconnection delay advertised to the client at the start of
	// every stream, sent as the retry field of the event stream. A browser's
	// EventSource reconnects on its own after a stream ends, and this is the
	// only say a server has in how soon. It is unset by default, which leaves
	// the client's own default in place.
	Retry time.Duration

	// MaxStreams is how many event streams the application serves at once,
	// defaulting to [DefaultSSEMaxStreams]. A request arriving once the limit
	// is reached is refused with 503 and a Retry-After header rather than
	// accepted into a process that has no room for it.
	//
	// Unlike every other field here it may only be set on the application: the
	// resource it protects is the process, not a route, so a router or a route
	// that sets it is refused when the application is built. A negative value
	// removes the limit, which is only appropriate where something else is
	// counting.
	MaxStreams int

	// MaxStreamsPerIP is how many event streams a single client address may
	// hold open at once, defaulting to [DefaultSSEMaxStreamsPerIP]. A request
	// that would exceed it is refused with 503 and a Retry-After header, the
	// same as MaxStreams, but for one client rather than for the process.
	//
	// MaxStreams alone bounds the process; it does not bound one client
	// within it, so a single client opening streams in a tight loop can hold
	// every one of MaxStreams' slots itself, leaving 503 for everyone else
	// until it disconnects. This is what stops that: no matter how many
	// streams the process has room for, one address can never hold more than
	// this many of them.
	//
	// The address used is the one [Context.ClientIP] resolves; see
	// [ClientIPOptions] to configure it behind a proxy. By default an IPv4
	// address is counted exactly and an IPv6 address by its /56, for the
	// reason given on [WSOptions.MaxConnectionsPerIP];
	// [ClientIPOptions.ConnectionIPv6Prefix] and
	// [ClientIPOptions.ConnectionIPv4Prefix] choose how widely addresses are
	// grouped. Like MaxStreams, this
	// may only be set on the application, because the dimension it bounds is
	// a client's share of the process, not of one route: a router or a route
	// that sets it is refused when the application is built. A negative
	// value removes the limit, which is only appropriate where something
	// else is counting per client, such as a reverse proxy already capping
	// connections per source address.
	MaxStreamsPerIP int
}

// overlay layers a narrower scope's options on top of a wider one's, leaving
// whatever the narrower scope did not set alone.
func (o SSEOptions) overlay(over SSEOptions) SSEOptions {
	if over.KeepAlive != 0 {
		o.KeepAlive = over.KeepAlive
	}
	if over.WriteTimeout != 0 {
		o.WriteTimeout = over.WriteTimeout
	}
	if over.Retry != 0 {
		o.Retry = over.Retry
	}
	if over.MaxStreams != 0 {
		o.MaxStreams = over.MaxStreams
	}
	if over.MaxStreamsPerIP != 0 {
		o.MaxStreamsPerIP = over.MaxStreamsPerIP
	}
	return o
}

// withDefaults fills in every unset option, so that a stream reads them
// without repeating fallbacks.
func (o SSEOptions) withDefaults() SSEOptions {
	o.KeepAlive = orDefaultDuration(o.KeepAlive, DefaultSSEKeepAlive)
	o.WriteTimeout = orDefaultDuration(o.WriteTimeout, DefaultSSEWriteTimeout)
	if o.Retry < 0 {
		o.Retry = 0
	}
	return o
}

// WithSSE configures the event streams of a route, or of every route beneath a
// router.
//
//	live := muzak.NewRouter()
//	live.SSE("/items/stream", streamItems)
//	app.Include(live, muzak.WithSSE(muzak.SSEOptions{
//		KeepAlive: 5 * time.Second,
//		Retry:     2 * time.Second,
//	}))
//
// Options layer field by field on top of [AppOptions.SSE] and on top of
// whatever an enclosing router declared, so a route can change one bound
// without restating the rest.
func WithSSE(opts SSEOptions) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.sse = &opts },
		router: func(c *routerConfig) { c.sse = &opts },
	}
}

// SSEHandler is the shape every Muzak server-sent events handler takes.
//
// In is bound from the request exactly as it is for any other route. Out is
// the model each event carries: the compiler enforces that nothing else is
// sent, and the generated document describes the stream with it, which is the
// same contract an ordinary handler's return type gives.
//
// There is no return value beyond the error, because what an SSE route
// produces is a stream rather than a body. Returning nil ends the stream
// normally. Returning an error ends it too, with nothing about the error
// disclosed to the client, since by then the response has long since begun.
type SSEHandler[In, Out any] func(ctx *Context, in In, stream *SSEStream[Out]) error

// sseConfig is an event stream route's resolved configuration.
type sseConfig struct {
	opts SSEOptions
	// noDeadlines makes the warning about a response that cannot carry write
	// deadlines a once-per-route one: it is the same fact for every stream the
	// route serves, and a line for each would bury it.
	noDeadlines sync.Once
}

// SSE registers a server-sent events handler for GET requests at the given
// path template.
//
// The request is an ordinary one, so everything that applies to a route
// applies here: middleware runs, guards run, dependencies resolve, and the
// input struct is bound and validated before a single byte of the stream is
// written. A request that fails any of that is answered with the usual JSON
// error and never becomes a stream at all.
//
//	type StreamIn struct {
//		Room string `path:"room"`
//	}
//
//	r.SSE("/rooms/{room}/stream", func(ctx *muzak.Context, in StreamIn, stream *muzak.SSEStream[MessageOut]) error {
//		for message := range room(in.Room).Messages(stream.Context()) {
//			if err := stream.Send(message); err != nil {
//				return err
//			}
//		}
//		return nil
//	})
//
// The stream is closed when the handler returns, so a handler owns it for as
// long as it runs and never has to arrange the teardown itself. Watch
// [SSEStream.Context] for the client going away or the server shutting down;
// it is cancelled for both, and every send after it reports
// [ErrSSEStreamEnded], so a handler that only sends ends on its own.
//
// The response header is written before the handler is called, which is what
// lets a client see the stream open immediately. A failure that should be
// answered with a status belongs in a guard or a dependency, where there is
// still a response to write it into.
//
// Use [Router.SSEHandle] for a stream reached by a method other than GET, and
// [WithSSE] to configure the stream itself.
func (r *Router) SSE[In, Out any](path string, h SSEHandler[In, Out], opts ...RouteOption) *Route {
	return registerSSE(r, http.MethodGet, path, h, opts)
}

// SSEHandle registers a server-sent events handler for an arbitrary HTTP
// method, which is what a protocol that streams its answer to a posted
// document needs:
//
//	r.SSEHandle(http.MethodPost, "/chat/stream", streamChat)
//
// Unlike a WebSocket handshake, the request may carry a body, so the input
// type binds one exactly as it would for any other route. The method is
// upper-cased before use; everything else behaves as [Router.SSE].
func (r *Router) SSEHandle[In, Out any](method, path string, h SSEHandler[In, Out], opts ...RouteOption) *Route {
	return registerSSE(r, strings.ToUpper(method), path, h, opts)
}

// registerSSE builds the Route value both registration methods share.
//
// It is a free function rather than a method for the same reason register is:
// it needs the type parameters the caller inferred, which keeps each method
// above a one-line delegation.
func registerSSE[In, Out any](r *Router, method, path string, h SSEHandler[In, Out], opts []RouteOption) *Route {
	r.mustBeOpen("registering SSE " + method + " " + path)
	rt := &Route{
		Method:   method,
		rawPath:  path,
		inType:   reflect.TypeFor[In](),
		outType:  reflect.TypeFor[Out](),
		registry: r,
		sse:      &sseConfig{},
	}
	for _, opt := range opts {
		opt.applyRoute(&rt.cfg)
	}
	if h == nil {
		r.errs = append(r.errs, fmt.Errorf("muzak: SSE %s %s: handler is nil", method, path))
		return rt
	}
	if !strings.HasPrefix(path, "/") {
		r.errs = append(r.errs, fmt.Errorf("muzak: SSE %s %s: path must begin with %q", method, path, "/"))
		return rt
	}

	rt.invoke = func(c *Context) error {
		var in In
		if !rt.plan.empty {
			if rt.plan.multipart {
				// A form body may have spilled to temporary files, which stay
				// readable for as long as the handler runs and no longer.
				defer releaseUpload(c.r)
			}
			if err := rt.plan.bind(c, reflect.ValueOf(&in).Elem(), rt); err != nil {
				return err
			}
		} else {
			discardBody(c.r)
		}
		core, err := c.app.acceptSSE(c, rt.sse)
		if err != nil {
			return err
		}
		stream := &SSEStream[Out]{core: core}
		return c.app.serveSSE(c, core, func() error { return h(c, in, stream) })
	}
	r.routes = append(r.routes, rt)
	return rt
}

// resolveSSE completes an event stream route once the configuration it
// inherits is known. It is called from [Route.resolve] and reports the two
// mistakes such a route can be declared with.
func (rt *Route) resolveSSE(in inherited) error {
	if rt.cfg.status != 0 {
		return fmt.Errorf("muzak: SSE %s %s: Status cannot be declared on an event stream route, which always answers %d and then streams",
			rt.Method, rt.Path, http.StatusOK)
	}
	opts := in.sse
	if rt.cfg.sse != nil {
		opts = opts.overlay(*rt.cfg.sse)
	}
	if opts.MaxStreams != in.sseMaxStreams {
		return fmt.Errorf("muzak: SSE %s %s: MaxStreams may only be set on the application, because the streams it bounds belong to the process rather than to one route", rt.Method, rt.Path)
	}
	if opts.MaxStreamsPerIP != in.sseMaxStreamsPerIP {
		return fmt.Errorf("muzak: SSE %s %s: MaxStreamsPerIP may only be set on the application, because the streams it bounds belong to the process rather than to one route", rt.Method, rt.Path)
	}
	rt.sse.opts = opts.withDefaults()
	return nil
}

// errSSEShuttingDown reports a request that arrived while the server was
// draining.
var errSSEShuttingDown = NewHTTPError(http.StatusServiceUnavailable,
	"the server is shutting down and is not accepting new event streams")

// errSSETooManyStreams reports a request refused because the application is
// already serving as many streams as it is allowed to.
var errSSETooManyStreams = NewHTTPError(http.StatusServiceUnavailable,
	"the server is serving as many event streams as it is configured to; try again shortly")

// errSSETooManyStreamsFromClient reports a request refused because the
// requesting client already holds as many streams as
// [SSEOptions.MaxStreamsPerIP] allows, distinct from the server as a whole
// being full.
var errSSETooManyStreamsFromClient = NewHTTPError(http.StatusServiceUnavailable,
	"your client is already holding as many event streams as this server allows per client; close one before opening another")

// acceptSSE opens the stream for a request, returning an *[HTTPError] for one
// the application has no room to serve.
func (a *App) acceptSSE(c *Context, cfg *sseConfig) (*sseStream, error) {
	if c.w.written {
		// Something in the chain already answered, so there is no response
		// left to stream into.
		return nil, NewHTTPError(http.StatusInternalServerError,
			"the response had already started, so the event stream could not be opened")
	}
	key, err := perClientKey(c, a.streams.perKeyLimit)
	if err != nil {
		return nil, err
	}
	stream := newSSEStream(c, cfg.opts)
	// The stream is admitted before its header is written, because a refusal
	// has to be an ordinary error response and there is no way back to one
	// afterwards. Admitting and recording under one lock is also what keeps
	// two requests arriving together from both passing a limit of one.
	switch a.streams.add(stream, key) {
	case registryDraining:
		return nil, errSSEShuttingDown
	case registryFull:
		c.w.Header().Set("Retry-After", "5")
		return nil, errSSETooManyStreams
	case registryKeyFull:
		c.w.Header().Set("Retry-After", "5")
		return nil, errSSETooManyStreamsFromClient
	case admitted:
	}
	if err := stream.open(c); err != nil {
		// Finishing here matters as much as it does when a handler returns: a
		// stream that got as far as watching the request must stop watching it
		// before the response goes back to net/http, or the arrangement that
		// interrupts a write would reach for a connection that by then belongs
		// to somebody else's request.
		stream.finish()
		a.streams.remove(stream)
		return nil, err
	}
	if cfg.opts.WriteTimeout > 0 && !stream.carriesDeadlines() {
		// The write timeout is the bound that keeps a client that has stopped
		// reading from pinning this stream, and it can only be enforced through
		// the response's write deadline. Streaming without it works until the
		// first such client, so this is the one moment anybody is told.
		cfg.noDeadlines.Do(func() {
			a.logger.WarnContext(c.Context(), "muzak: an event stream's response cannot carry write deadlines, so its write timeout is not enforced; a client that stops reading holds its stream until the server shuts down. A middleware that wraps the response writer must implement Unwrap() http.ResponseWriter",
				slog.String("route", c.route.Path),
				slog.String("writer", fmt.Sprintf("%T", innermostWriter(c.w))))
		})
	}
	if cfg.opts.KeepAlive > 0 {
		go stream.keepalive(cfg.opts.KeepAlive)
	}
	return stream, nil
}

// serveSSE runs a handler over an open stream and ends the stream afterwards,
// whatever the handler did.
func (a *App) serveSSE(c *Context, stream *sseStream, call func() error) error {
	defer func() {
		// Finishing first is what guarantees that no goroutine the handler
		// left behind can still be writing when net/http takes the response
		// back and hands the connection to the next request.
		stream.finish()
		a.streams.remove(stream)
		if recovered := recover(); recovered != nil {
			panic(recovered)
		}
	}()

	if err := call(); err != nil {
		if errors.Is(err, ErrSSEStreamEnded) {
			// The client went away, or the server is shutting down. That is
			// how a stream ends rather than something to report as a failure,
			// but it is worth a line for anyone asking why one stopped.
			a.logger.DebugContext(c.Context(), "muzak: an event stream ended",
				slog.String("route", c.route.Path),
				slog.String(RequestIDKey, c.RequestID()),
				slog.String("reason", err.Error()))
		} else {
			// Nothing derived from the error reaches the client: it may name a
			// query, a path or a driver failure, none of which is theirs. The
			// response began long ago in any case, so there is no status left
			// to change.
			a.logger.ErrorContext(c.Context(), "muzak: an event stream handler failed",
				slog.String("route", c.route.Path),
				slog.String(RequestIDKey, c.RequestID()),
				slog.String("error", err.Error()))
		}
	}
	// The header is long since written, so there is no response left for the
	// router to write.
	return nil
}
