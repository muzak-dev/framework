package muzak

import (
	"bufio"
	"bytes"
	"crypto/sha1" //nolint:gosec // RFC 6455 defines the handshake digest; see wsAcceptKey
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Defaults applied to a WebSocket connection when [WSOptions] leaves them
// unset. Each of them bounds work a connected peer can ask the server to do,
// so none of them defaults to zero.
const (
	// DefaultWSReadLimit is the largest message accepted on a connection that
	// does not override it, at one mebibyte.
	DefaultWSReadLimit int64 = 1 << 20
	// DefaultWSWriteTimeout bounds how long a single message may take to
	// reach a peer, at ten seconds.
	DefaultWSWriteTimeout = 10 * time.Second
	// DefaultWSCloseGracePeriod is how long a closing connection waits for the
	// peer's own close frame before the transport goes, at 250 milliseconds.
	DefaultWSCloseGracePeriod = 250 * time.Millisecond
	// DefaultWSPongTimeout is how long a keepalive ping waits for its answer,
	// at ten seconds. It applies only when [WSOptions.PingInterval] asks for
	// keepalive at all.
	DefaultWSPongTimeout = 10 * time.Second
	// DefaultWSReadTimeout bounds how long one message may take to arrive once
	// it has begun, at thirty seconds.
	DefaultWSReadTimeout = 30 * time.Second
	// DefaultWSMaxConnections is how many WebSocket connections one
	// application holds open at once by default, at 1024. It is close to the
	// file descriptor budget a process is usually given, which is the resource
	// that runs out first.
	DefaultWSMaxConnections = 1024
	// DefaultWSMaxConnectionsPerIP is how many WebSocket connections a single
	// client address holds open at once by default, at 64. It is far above
	// what a legitimate browser session needs, even one holding dozens of
	// tabs each open to their own connection, and far below
	// DefaultWSMaxConnections, so a single misbehaving or attacking address
	// can take a meaningful slice of the process budget but never all of it.
	DefaultWSMaxConnectionsPerIP = 64
)

// wsConnectionLimit resolves how many connections an application will hold at
// once, where zero asks for the default and a negative value removes the limit.
func wsConnectionLimit(configured int) int {
	switch {
	case configured == 0:
		return DefaultWSMaxConnections
	case configured < 0:
		return 0
	default:
		return configured
	}
}

// wsConnectionsPerIPLimit resolves how many connections a single client
// address may hold at once, where zero asks for the default and a negative
// value removes the limit.
func wsConnectionsPerIPLimit(configured int) int {
	switch {
	case configured == 0:
		return DefaultWSMaxConnectionsPerIP
	case configured < 0:
		return 0
	default:
		return configured
	}
}

// wsGUID is the constant RFC 6455 appends to the client's key before hashing
// it. It has no cryptographic role; it exists so that a server which merely
// echoes headers cannot be mistaken for one that speaks WebSocket.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WSOptions configures WebSocket connections.
//
// It can be set application-wide through [AppOptions.WebSocket] and narrowed
// for a router or a single route with [WithWebSocket]. Layering works field by
// field: whatever a narrower scope leaves at its zero value it inherits, so a
// route that raises only the read limit keeps the application's origin policy.
//
// The zero value is usable and safe: messages are bounded, writes are bounded,
// and a cross-origin handshake is refused.
type WSOptions struct {
	// ReadLimit is the largest message accepted, in bytes, defaulting to
	// [DefaultWSReadLimit]. A message that would exceed it is refused with
	// [WSStatusMessageTooBig] before any of it is buffered, so a peer can
	// never choose how much memory the server spends. There is deliberately no
	// way to remove the limit.
	ReadLimit int64

	// WriteTimeout bounds how long one message may take to reach the peer,
	// defaulting to [DefaultWSWriteTimeout]. It is what stops a peer that has
	// stopped reading from pinning a goroutine and a send buffer forever. A
	// negative value removes the bound, which is only appropriate when
	// something else imposes one.
	WriteTimeout time.Duration

	// ReadTimeout bounds how long one message may take to arrive once its
	// first frame has, defaulting to [DefaultWSReadTimeout]. It is what stops
	// a peer dribbling a message out a byte at a time and holding a goroutine
	// for as long as it cares to.
	//
	// It does not bound how long a connection may sit idle between messages,
	// because waiting is what most connections are for. Use PingInterval to
	// notice a peer that has stopped answering at all. A negative value
	// removes the bound.
	ReadTimeout time.Duration

	// MaxConnections is how many WebSocket connections the application will
	// hold open at once, defaulting to [DefaultWSMaxConnections]. A handshake
	// arriving once the limit is reached is refused with 503 and a Retry-After
	// header rather than accepted into a process that has no room for it.
	//
	// Unlike every other field here it may only be set on the application: the
	// resource it protects is the process, not a route, so a router or a route
	// that sets it is refused when the application is built. A negative value
	// removes the limit, which is only appropriate where something else is
	// counting.
	MaxConnections int

	// MaxConnectionsPerIP is how many WebSocket connections a single client
	// address may hold open at once, defaulting to
	// [DefaultWSMaxConnectionsPerIP]. A handshake that would exceed it is
	// refused with 503 and a Retry-After header, the same as MaxConnections,
	// but for one client rather than for the process.
	//
	// MaxConnections alone bounds the process; it does not bound one client
	// within it. A WebSocket handshake needs no Origin header at all to
	// succeed - only a browser sends one, and the origin check exists to stop
	// browser-based hijacking, not to authenticate a client - so a single
	// unauthenticated, non-browser client can open connections in a tight
	// loop and hold every one of MaxConnections' slots itself, leaving 503
	// for everyone else until it disconnects. This is what stops that: no
	// matter how many connections the process has room for, one address can
	// never hold more than this many of them.
	//
	// The address used is the one [Context.ClientIP] resolves, the same
	// spoof-resistant resolution the rate limiter's default [IPTracker] uses;
	// see [ClientIPOptions] to configure it behind a proxy. An IPv4 address
	// is counted exactly and an IPv6 address by its /64, because a client
	// holding a /64 can open each connection from a different address in it
	// and would otherwise get a fresh allowance from every one. Like
	// MaxConnections, this may only be set on the application, because the
	// dimension it bounds is a client's share of the process, not of one
	// route: a router or a route that sets it is refused when the
	// application is built. A negative value removes the limit, which is
	// only appropriate where something else is counting per client, such as
	// a reverse proxy already capping connections per source address.
	MaxConnectionsPerIP int

	// CloseGracePeriod is how long a closing connection waits for the peer's
	// close frame before the transport is torn down, defaulting to
	// [DefaultWSCloseGracePeriod]. Waiting briefly is what lets the peer read
	// the close frame instead of finding the connection reset. A negative
	// value closes immediately.
	CloseGracePeriod time.Duration

	// PingInterval turns on keepalive: the server pings this often and closes
	// the connection when the peer stops answering, which is what notices a
	// connection dropped by a network that told nobody. It is off by default.
	//
	// Keepalive only works while the handler is reading, because a pong is
	// consumed by a read like any other frame. A handler that only ever writes
	// should ping by hand instead, with [WSConn.Ping].
	PingInterval time.Duration

	// PongTimeout is how long a keepalive ping waits for its answer, defaulting
	// to [DefaultWSPongTimeout]. It is meaningful only alongside PingInterval.
	PongTimeout time.Duration

	// MessageLimits bounds how fast a peer may send messages, using the same
	// quotas, storage and tracker as [RateLimitOptions] does for requests. It
	// is unset by default, which leaves a connected peer free to send as fast
	// as it likes within ReadLimit and ReadTimeout.
	//
	// ReadLimit bounds what one message costs and MaxConnections bounds how
	// many peers there are, but neither bounds a peer that stays inside both
	// and simply never pauses. This is what does:
	//
	//	muzak.WithWebSocket(muzak.WSOptions{
	//		MessageLimits: []muzak.Quota{{Name: "ws-messages", Window: time.Second, Limit: 20}},
	//	})
	//
	// Messages are what is counted, one per message the handler reads, which
	// bounds the scheduling a chatty peer costs; ReadLimit is what bounds the
	// bytes. A peer that goes over is closed with
	// [WSStatusPolicyViolation] rather than left connected and ignored,
	// because a message silently dropped is a protocol nobody can debug. The
	// count happens after the message has been read, so the limit bounds a
	// sustained rate rather than refusing the message that crossed it.
	//
	// Counting a message costs whatever a storage round trip costs, so a
	// shared storage on a chatty connection is a real expense; prefer a window
	// long enough that the count is not the conversation's bottleneck. The
	// quotas share a namespace with those of [RateLimitOptions], so give them
	// names of their own unless a shared budget is what is wanted. A route
	// marked [SkipRateLimit] counts no messages either.
	MessageLimits []Quota

	// Subprotocols lists the subprotocols the route can speak, such as
	// "graphql-transport-ws". The client's own list is in preference order, so
	// the first of its choices that appears here is the one negotiated, and a
	// client asking for something else is answered without the header, which
	// tells it to give up.
	Subprotocols []string

	// AllowedOrigins lists the browser origins permitted to open a connection,
	// such as "https://app.example.com", in addition to the server's own
	// origin, which is always allowed. The single entry "*" allows any origin.
	//
	// The check exists because a WebSocket handshake is not subject to the
	// same-origin policy and is not preflighted: without it, any page on the
	// internet could open an authenticated connection to this server from a
	// visitor's browser, cookies and all. Note that [AppOptions.CORS] has no
	// bearing on it, for exactly that reason.
	AllowedOrigins []string

	// AllowOriginFunc decides dynamically whether an origin may connect. It is
	// consulted only for an origin that AllowedOrigins and the same-origin rule
	// did not already allow, and it runs on every handshake, so it must be
	// cheap and free of side effects.
	AllowOriginFunc func(r *http.Request, origin string) bool

	// InsecureSkipOriginCheck accepts a handshake from any origin, including a
	// browser page hosted anywhere on the internet.
	//
	// It is safe only for a connection that carries no ambient authority: one
	// authenticated by a token the client has to present explicitly, never by
	// a cookie, since a browser attaches cookies to a cross-origin handshake
	// without being asked.
	InsecureSkipOriginCheck bool
}

// overlay layers a narrower scope's options on top of a wider one's, leaving
// whatever the narrower scope did not set alone.
func (o WSOptions) overlay(over WSOptions) WSOptions {
	if over.ReadLimit != 0 {
		o.ReadLimit = over.ReadLimit
	}
	if over.WriteTimeout != 0 {
		o.WriteTimeout = over.WriteTimeout
	}
	if over.ReadTimeout != 0 {
		o.ReadTimeout = over.ReadTimeout
	}
	if over.MaxConnections != 0 {
		o.MaxConnections = over.MaxConnections
	}
	if over.MaxConnectionsPerIP != 0 {
		o.MaxConnectionsPerIP = over.MaxConnectionsPerIP
	}
	if over.CloseGracePeriod != 0 {
		o.CloseGracePeriod = over.CloseGracePeriod
	}
	if over.PingInterval != 0 {
		o.PingInterval = over.PingInterval
	}
	if over.PongTimeout != 0 {
		o.PongTimeout = over.PongTimeout
	}
	if over.MessageLimits != nil {
		o.MessageLimits = over.MessageLimits
	}
	if over.Subprotocols != nil {
		o.Subprotocols = over.Subprotocols
	}
	if over.AllowedOrigins != nil {
		o.AllowedOrigins = over.AllowedOrigins
	}
	if over.AllowOriginFunc != nil {
		o.AllowOriginFunc = over.AllowOriginFunc
	}
	if over.InsecureSkipOriginCheck {
		o.InsecureSkipOriginCheck = true
	}
	return o
}

// withDefaults fills in every unset option, so that the connection reads them
// without repeating fallbacks.
func (o WSOptions) withDefaults() WSOptions {
	if o.ReadLimit <= 0 {
		o.ReadLimit = DefaultWSReadLimit
	}
	if o.ReadLimit > math.MaxInt {
		// coverage: a limit larger than a slice can be is no limit at all, and
		// the read path counts in int. On a 64 bit platform the two are the
		// same size and this cannot happen, so it is only reachable where int
		// is 32 bits.
		o.ReadLimit = math.MaxInt
	}
	o.WriteTimeout = orDefaultDuration(o.WriteTimeout, DefaultWSWriteTimeout)
	o.ReadTimeout = orDefaultDuration(o.ReadTimeout, DefaultWSReadTimeout)
	o.CloseGracePeriod = orDefaultDuration(o.CloseGracePeriod, DefaultWSCloseGracePeriod)
	if o.PingInterval < 0 {
		o.PingInterval = 0
	}
	if o.PingInterval > 0 {
		o.PongTimeout = orDefaultDuration(o.PongTimeout, DefaultWSPongTimeout)
	}
	return o
}

// WithWebSocket configures the WebSocket connections of a route, or of every
// route beneath a router.
//
//	chat := muzak.NewRouter()
//	chat.WS("/rooms/{room}/ws", joinRoom)
//	app.Include(chat, muzak.WithWebSocket(muzak.WSOptions{
//		ReadLimit:      64 << 10,
//		PingInterval:   30 * time.Second,
//		AllowedOrigins: []string{"https://app.example.com"},
//	}))
//
// Options layer field by field on top of [AppOptions.WebSocket] and on top of
// whatever an enclosing router declared, so a route can raise one limit
// without restating the rest.
func WithWebSocket(opts WSOptions) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.ws = &opts },
		router: func(c *routerConfig) { c.ws = &opts },
	}
}

// WSHandler is the shape every Muzak WebSocket handler takes.
//
// In is bound from the request exactly as it is for any other route, from the
// path, query string, headers and cookies of the handshake. There is no output
// type: what a WebSocket route produces is a conversation, written to conn.
//
// Returning nil closes the connection normally. Returning a *[WSCloseError]
// closes it with the status and reason it carries, which is how a handler
// rejects a peer on its own terms. Any other error closes the connection with
// [WSStatusInternalError] and is logged, with nothing about it disclosed to the
// peer.
type WSHandler[In any] func(ctx *Context, in In, conn *WSConn) error

// wsConfig is a WebSocket route's resolved configuration.
type wsConfig struct {
	opts        WSOptions
	allowOrigin func(r *http.Request, origin string) bool
	// messages is the resolved policy for [WSOptions.MessageLimits], and is
	// nil for a route that does not bound how fast a peer may send.
	messages *rateLimitConfig
}

// WS registers a WebSocket handler at the given path template.
//
// The handshake is an ordinary GET, so everything that applies to a route
// applies here: middleware runs, guards run, dependencies resolve, and the
// input struct is bound and validated before a single byte is upgraded. A
// request that fails any of that is answered with the usual JSON error and
// never becomes a connection at all.
//
//	type WSItemIn struct {
//		ItemID string `path:"item_id"`
//		Q      *int   `query:"q"`
//	}
//
//	r.WS("/items/{item_id}/ws", func(ctx *muzak.Context, in WSItemIn, conn *muzak.WSConn) error {
//		token := muzak.From[SessionOrToken](ctx)
//		for {
//			message, err := conn.ReadText(ctx.Context())
//			if err != nil {
//				return nil
//			}
//			if err := conn.WriteText(ctx.Context(), "you said: "+message); err != nil {
//				return err
//			}
//			_ = token
//		}
//	}, muzak.Needs(GetSessionOrToken))
//
// The connection is closed when the handler returns, so a handler owns its
// connection for as long as it runs and never has to arrange for the teardown
// itself. The [Context] belongs to the request and must not outlive the
// handler either; pass [Context.Context] to anything that will.
//
// Because a handshake carries no body, an input type with a body field is a
// registration error rather than a request that mysteriously never arrives.
// Configure the connection itself with [WithWebSocket].
func (r *Router) WS[In any](path string, h WSHandler[In], opts ...RouteOption) *Route {
	rt := &Route{
		Method:    http.MethodGet,
		rawPath:   path,
		inType:    reflect.TypeFor[In](),
		outType:   emptyType,
		registry:  r,
		websocket: &wsConfig{},
	}
	for _, opt := range opts {
		opt.applyRoute(&rt.cfg)
	}
	if h == nil {
		r.errs = append(r.errs, fmt.Errorf("muzak: WS %s: handler is nil", path))
		return rt
	}
	if !strings.HasPrefix(path, "/") {
		r.errs = append(r.errs, fmt.Errorf("muzak: WS %s: path must begin with %q", path, "/"))
		return rt
	}

	rt.invoke = func(c *Context) error {
		// A handshake carries no body, so whatever arrived with it is drained
		// rather than read, which keeps the connection reusable if the
		// handshake is refused.
		discardBody(c.r)
		var in In
		if !rt.plan.empty {
			if err := rt.plan.bind(c, reflect.ValueOf(&in).Elem(), rt); err != nil {
				return err
			}
		}
		conn, err := c.app.acceptWebSocket(c, rt.websocket)
		if err != nil {
			return err
		}
		return c.app.serveWebSocket(c, conn, func() error { return h(c, in, conn) })
	}
	r.routes = append(r.routes, rt)
	return rt
}

// resolveWebSocket completes a WebSocket route once the configuration it
// inherits is known. It is called from [Route.resolve] and reports the two
// mistakes a WebSocket route can be declared with.
func (rt *Route) resolveWebSocket(in inherited) error {
	if rt.cfg.status != 0 {
		return fmt.Errorf("muzak: WS %s: Status cannot be declared on a websocket route, which always answers %d",
			rt.Path, http.StatusSwitchingProtocols)
	}
	if rt.plan.body != nil || rt.plan.multipart {
		return fmt.Errorf("muzak: WS %s: a websocket handshake carries no request body, so every field of %s must be tagged path, query, header or cookie",
			rt.Path, rt.inType)
	}
	opts := in.ws
	if rt.cfg.ws != nil {
		opts = opts.overlay(*rt.cfg.ws)
	}
	if opts.MaxConnections != in.wsMaxConnections {
		return fmt.Errorf("muzak: WS %s: MaxConnections may only be set on the application, because the connections it bounds belong to the process rather than to one route", rt.Path)
	}
	if opts.MaxConnectionsPerIP != in.wsMaxConnectionsPerIP {
		return fmt.Errorf("muzak: WS %s: MaxConnectionsPerIP may only be set on the application, because the connections it bounds belong to the process rather than to one route", rt.Path)
	}
	rt.websocket.opts = opts.withDefaults()
	for _, name := range rt.websocket.opts.Subprotocols {
		// A subprotocol is echoed into the handshake response, so one that is
		// not a token could carry a line break into the header block. It is
		// refused here rather than sanitised, because a name that needs
		// sanitising is a mistake to fix rather than to paper over.
		if !isHTTPToken(name) {
			return fmt.Errorf("muzak: WS %s: subprotocol %q is not a valid token", rt.Path, wsShorten(name))
		}
	}
	if limits := rt.websocket.opts.MessageLimits; len(limits) > 0 && !rt.skipRateLimit {
		messages, err := newRateLimitConfig(rt.rateLimitOpts, limits)
		if err != nil {
			return fmt.Errorf("muzak: WS %s: MessageLimits: %w", rt.Path, err)
		}
		rt.websocket.messages = messages
	}
	rt.websocket.allowOrigin = wsOriginPolicy(rt.websocket.opts)
	rt.responses = append(rt.responses, responseDoc{
		code:        http.StatusUpgradeRequired,
		description: "The request is not a WebSocket handshake.",
	})
	return nil
}

// wsOriginPolicy builds the function that decides which browser origins may
// open a connection.
//
// Only the host is compared for the same-origin case, not the scheme, because
// a server behind a proxy that terminates TLS sees a plain request and cannot
// tell which scheme the browser used.
func wsOriginPolicy(opts WSOptions) func(*http.Request, string) bool {
	allowAny := slices.Contains(opts.AllowedOrigins, "*")
	allowed := opts.AllowedOrigins
	dynamic := opts.AllowOriginFunc
	return func(r *http.Request, origin string) bool {
		if allowAny {
			return true
		}
		for _, candidate := range allowed {
			if strings.EqualFold(candidate, origin) {
				return true
			}
		}
		if parsed, err := url.Parse(origin); err == nil && strings.EqualFold(parsed.Host, r.Host) {
			return true
		}
		return dynamic != nil && dynamic(r, origin)
	}
}

// acceptWebSocket checks the handshake and upgrades the connection, returning
// an *[HTTPError] for a request that is not a handshake this route will serve.
func (a *App) acceptWebSocket(c *Context, cfg *wsConfig) (*WSConn, error) {
	accept, err := cfg.checkHandshake(c)
	if err != nil {
		return nil, err
	}
	// Whose budget the messages are counted against is settled here, while
	// there is still a response to refuse the handshake with: a tracker that
	// insists on a credential has no way to say so once the connection has
	// been upgraded.
	messageKey := ""
	if cfg.messages != nil {
		if messageKey, err = cfg.messages.key(c); err != nil {
			return nil, err
		}
	}
	// The per-client key is resolved once here and reused for both the
	// pre-upgrade check and the post-upgrade recording below, so the same
	// client is counted against the same budget in both places.
	connKey, err := perClientKey(c, a.websockets.perKeyLimit)
	if err != nil {
		return nil, err
	}
	// Refusing before the upgrade is what lets a client shut out by a draining
	// or a full server read an ordinary error response.
	switch a.websockets.admits(connKey) {
	case registryDraining:
		return nil, errWSShuttingDown
	case registryFull:
		c.w.Header().Set("Retry-After", "5")
		return nil, errWSTooManyConnections
	case registryKeyFull:
		c.w.Header().Set("Retry-After", "5")
		return nil, errWSTooManyConnectionsFromClient
	case admitted:
	}
	subprotocol := wsSubprotocol(c.r, cfg.opts.Subprotocols)
	conn, err := a.upgrade(c, cfg, accept, subprotocol)
	if err != nil {
		return nil, err
	}
	if a.websockets.add(conn, connKey) != admitted {
		// coverage: this is the losing side of a race between a handshake and
		// a shutdown or a full register, narrowed to the microseconds between
		// the check above and the upgrade, so it is reasoned about rather than
		// provoked. The connection is told to go away rather than left
		// unaccounted for; there is no response left to refuse it with by now.
		_ = conn.Close(WSStatusGoingAway, "the server is shutting down")
		return nil, errWSShuttingDown
	}
	if cfg.messages != nil {
		conn.messages = &wsMessageLimiter{cfg: cfg.messages, key: messageKey, logger: c.logger, requestID: c.RequestID()}
	}
	// The request's cancellation is watched once here rather than once per
	// message, and it is watched before anything else can touch the
	// connection, so the keepalive below sees a connection already arranged.
	conn.watch(c.Context())
	if cfg.opts.PingInterval > 0 {
		go conn.keepalive(c.Context(), cfg.opts.PingInterval, cfg.opts.PongTimeout)
	}
	return conn, nil
}

// wsCloseGoingAway ends a connection because the server is going away, which
// is what the register calls for every connection it holds. A peer is told
// rather than left to discover a connection that stopped answering.
func wsCloseGoingAway(conn *WSConn) {
	_ = conn.Close(WSStatusGoingAway, "the server is shutting down")
}

// errWSShuttingDown reports a handshake that arrived while the server was
// draining.
var errWSShuttingDown = NewHTTPError(http.StatusServiceUnavailable,
	"the server is shutting down and is not accepting new websocket connections")

// errWSTooManyConnections reports a handshake refused because the application
// is already holding as many connections as it is allowed to.
var errWSTooManyConnections = NewHTTPError(http.StatusServiceUnavailable,
	"the server is holding as many websocket connections as it is configured to; try again shortly")

// errWSTooManyConnectionsFromClient reports a handshake refused because the
// requesting client already holds as many connections as
// [WSOptions.MaxConnectionsPerIP] allows, distinct from the server as a whole
// being full.
var errWSTooManyConnectionsFromClient = NewHTTPError(http.StatusServiceUnavailable,
	"your client is already holding as many websocket connections as this server allows per client; close one before opening another")

// checkHandshake verifies that the request is a WebSocket handshake this route
// will serve, and returns the value for the Sec-WebSocket-Accept header.
func (cfg *wsConfig) checkHandshake(c *Context) (string, error) {
	// The method needs no check: a WebSocket route is registered for GET
	// alone, and the router neither answers HEAD from it nor lets any other
	// method reach it.
	r := c.r
	if r.ProtoMajor != 1 || r.ProtoMinor < 1 {
		wsUpgradeHeaders(c.w.Header())
		return "", NewHTTPErrorf(http.StatusUpgradeRequired,
			"a websocket handshake requires HTTP/1.1, and this request used HTTP/%d.%d", r.ProtoMajor, r.ProtoMinor)
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", "websocket") {
		wsUpgradeHeaders(c.w.Header())
		return "", NewHTTPError(http.StatusUpgradeRequired,
			"this route serves websocket connections; the request must ask to upgrade to one")
	}
	if version := r.Header.Get("Sec-WebSocket-Version"); version != "13" {
		wsUpgradeHeaders(c.w.Header())
		return "", NewHTTPErrorf(http.StatusUpgradeRequired,
			"websocket version %q is not supported; this server speaks version 13", wsShorten(version))
	}
	for _, name := range [...]string{"Sec-WebSocket-Key", "Sec-WebSocket-Version"} {
		if len(r.Header.Values(name)) > 1 {
			// One handshake describes itself once. Two answers to the same
			// question invite this end and whatever is in front of it to read
			// different ones.
			return "", NewHTTPErrorf(http.StatusBadRequest, "the %s header was sent more than once", name)
		}
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		// A handshake carries no body. Accepting one would leave whatever went
		// unread on the connection, to be taken for frames the moment it is
		// upgraded, which is a way of writing a peer's frames for it.
		return "", NewHTTPError(http.StatusBadRequest,
			"a websocket handshake cannot carry a request body")
	}
	if err := cfg.checkOrigin(r); err != nil {
		return "", err
	}
	return wsAcceptKey(r.Header.Get("Sec-WebSocket-Key"))
}

// checkOrigin applies the route's origin policy.
func (cfg *wsConfig) checkOrigin(r *http.Request) error {
	if cfg.opts.InsecureSkipOriginCheck {
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Only a browser sends an Origin, and only a browser attaches a user's
		// cookies to a request the user never asked for. A handshake without
		// one is not the attack this check exists to stop.
		return nil
	}
	if cfg.allowOrigin(r, origin) {
		return nil
	}
	return NewHTTPErrorf(http.StatusForbidden,
		"the origin %q may not open a websocket connection here", wsShorten(origin))
}

// wsShorten bounds a client-supplied value on its way into an error message.
// The value is escaped by the encoder that writes it, so what is left to guard
// against is the size: a header the size of the header limit should not become
// a response body the size of the header limit.
func wsShorten(value string) string {
	const most = 128
	if len(value) <= most {
		return value
	}
	return value[:most] + "..."
}

// wsUpgradeHeaders describes what the request should have asked for, so that a
// client which got the handshake wrong is told how to get it right.
func wsUpgradeHeaders(h http.Header) {
	h.Set("Upgrade", "websocket")
	h.Set("Connection", "Upgrade")
	h.Set("Sec-WebSocket-Version", "13")
}

// wsAcceptKey turns the client's Sec-WebSocket-Key into the value the server
// answers with.
//
// The digest is SHA-1 because RFC 6455 says so. It makes no security claim:
// the key is not a secret, the hash is not a signature, and the whole exchange
// exists to prove that both ends understood the handshake rather than to
// protect anything.
func wsAcceptKey(key string) (string, error) {
	// Sixteen bytes of base64 are exactly twenty four characters, so the length
	// is checked before anything is decoded and a header of any size cannot
	// buy so much as an allocation.
	raw, err := base64.StdEncoding.DecodeString(key)
	if len(key) != 24 || err != nil || len(raw) != 16 {
		return "", NewHTTPError(http.StatusBadRequest,
			"the Sec-WebSocket-Key header is missing or is not sixteen base64 encoded bytes")
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // see the comment above
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// wsSubprotocol picks the subprotocol to answer with, honouring the client's
// order of preference. An empty result means the header is left out, which
// tells a client that required one to give up.
func wsSubprotocol(r *http.Request, offered []string) string {
	if len(offered) == 0 {
		return ""
	}
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for candidate := range strings.SplitSeq(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if slices.Contains(offered, candidate) {
				return candidate
			}
		}
	}
	return ""
}

// isHTTPToken reports whether a string is a token as RFC 9110 defines one,
// which is what a header value made of a bare name has to be.
func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// headerHasToken reports whether a comma separated header carries a token,
// which is how Connection and Upgrade have to be read: both may list several
// values, in any case, spread over repeated header lines.
func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for candidate := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

// wsHandshakeHeaders are the response headers the handshake writes itself, and
// which are therefore not copied from whatever the middleware chain set.
var wsHandshakeHeaders = map[string]bool{
	"Upgrade":                true,
	"Connection":             true,
	"Sec-Websocket-Accept":   true,
	"Sec-Websocket-Protocol": true,
	"Content-Length":         true,
	"Content-Type":           true,
	"Transfer-Encoding":      true,
}

// upgrade takes the connection over from net/http and writes the handshake
// response onto it.
func (a *App) upgrade(c *Context, cfg *wsConfig, accept, subprotocol string) (*WSConn, error) {
	if c.w.written {
		// Something in the chain already answered, so there is no longer a
		// connection to take over.
		return nil, NewHTTPError(http.StatusInternalServerError,
			"the response had already started, so the connection could not be upgraded")
	}
	netConn, brw, err := http.NewResponseController(c.w).Hijack()
	if err != nil {
		return nil, NewHTTPError(http.StatusInternalServerError,
			"this connection cannot be upgraded to a websocket").Wrap(err)
	}
	markHijacked(c.w)

	// The listener's read and write timeouts were meant for one request. A
	// WebSocket outlives that by design, so they are cleared here and every
	// deadline from now on is set per operation.
	_ = netConn.SetDeadline(time.Time{})

	var response bytes.Buffer
	response.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	response.WriteString(accept)
	response.WriteString("\r\n")
	if subprotocol != "" {
		response.WriteString("Sec-WebSocket-Protocol: ")
		response.WriteString(subprotocol)
		response.WriteString("\r\n")
	}
	// Whatever the middleware and the guards set still belongs on the
	// response, which is how the request identifier reaches the client.
	if err := c.w.Header().WriteSubset(&response, wsHandshakeHeaders); err != nil {
		// coverage: WriteSubset only fails when the writer does, and a
		// bytes.Buffer does not.
		_ = netConn.Close()
		return nil, NewHTTPError(http.StatusInternalServerError, "the handshake response could not be built").Wrap(err)
	}
	response.WriteString("\r\n")

	if cfg.opts.WriteTimeout > 0 {
		// A write timeout of zero means the caller disabled it, and adding it
		// to the current time would set a deadline that has already passed.
		_ = netConn.SetWriteDeadline(time.Now().Add(cfg.opts.WriteTimeout))
	}
	if err := writeAndFlush(brw.Writer, response.Bytes()); err != nil {
		_ = netConn.Close()
		return nil, NewHTTPError(http.StatusInternalServerError, "the handshake response could not be sent").Wrap(err)
	}
	_ = netConn.SetWriteDeadline(time.Time{})

	reader, err := wsHijackedReader(brw.Reader, netConn)
	if err != nil {
		// coverage: the reader only fails if bufio cannot peek what it has
		// already buffered, which the standard library does not do.
		_ = netConn.Close()
		return nil, NewHTTPError(http.StatusInternalServerError, "the upgraded connection could not be read").Wrap(err)
	}
	return newWSConn(netConn, reader, false, subprotocol, cfg.opts), nil
}

// writeAndFlush writes a buffer through a buffered writer and pushes it out,
// reporting the first thing that went wrong. The handshake response is the
// last thing written through net/http's own writer, so it has to reach the
// socket before the connection changes hands.
func writeAndFlush(w *bufio.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.Flush()
}

// wsReadBufferSize is the buffer a connection reads frames through. It is
// large enough to hold a header and a small message in one read, and a larger
// message is read straight into the buffer being assembled rather than through
// here.
const wsReadBufferSize = 4 << 10

// wsHijackedReader builds the reader a WebSocket reads its frames from.
//
// The reader handed back by a hijack cannot simply be kept. It reads through
// net/http's own connection reader, which treats every read failure as the end
// of the request and cancels the request context, so one read that timed out
// would cancel the context the handler passes to everything else it does. What
// that reader already buffered still matters, though: a client is allowed to
// send its first frames in the same packet as the handshake. So the buffered
// bytes are taken over and everything after them is read from the socket.
func wsHijackedReader(buffered *bufio.Reader, conn net.Conn) (*bufio.Reader, error) {
	pending := buffered.Buffered()
	if pending == 0 {
		return bufio.NewReaderSize(conn, wsReadBufferSize), nil
	}
	head, err := buffered.Peek(pending)
	if err != nil {
		// coverage: Peek of exactly what is buffered cannot fail, and the
		// branch exists so that a future change to bufio cannot lose bytes
		// silently.
		return nil, err
	}
	return bufio.NewReaderSize(io.MultiReader(bytes.NewReader(bytes.Clone(head)), conn), wsReadBufferSize), nil
}

// serveWebSocket runs a handler over an open connection and closes it
// afterwards, whatever the handler did.
func (a *App) serveWebSocket(c *Context, conn *WSConn, call func() error) error {
	status, reason := WSStatusNormalClosure, ""
	defer func() {
		conn.stopWatching()
		a.websockets.remove(conn)
		if recovered := recover(); recovered != nil {
			// The connection is closed before the panic continues, because the
			// recovery above cannot write a response onto a hijacked socket.
			_ = conn.Close(WSStatusInternalError, "the handler failed")
			panic(recovered)
		}
		_ = conn.Close(status, reason)
	}()

	if err := call(); err != nil {
		var closed *WSCloseError
		if errors.As(err, &closed) {
			// The handler either reported the closure it read or chose one of
			// its own, and either way it has already said what to send.
			status, reason = closed.Status, closed.Reason
		} else {
			// Nothing derived from the error reaches the peer: it may name a
			// query, a path or a driver failure, none of which is theirs.
			status, reason = WSStatusInternalError, "the handler failed"
			a.logger.ErrorContext(c.Context(), "muzak: a websocket handler failed",
				slog.String("route", c.route.Path),
				slog.String(RequestIDKey, c.RequestID()),
				slog.String("error", err.Error()))
		}
	}
	// The handshake is long since answered, so there is no response left for
	// the router to write.
	return nil
}
