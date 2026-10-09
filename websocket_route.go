package muzak

import (
	"bufio"
	"bytes"
	"context"
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
	// DefaultWSPingInterval is how often a connection is pinged when
	// [WSOptions.PingInterval] does not say, at thirty seconds.
	DefaultWSPingInterval = 30 * time.Second
	// DefaultWSPongTimeout is how long a keepalive ping waits for its answer,
	// at ten seconds. It applies unless [WSOptions.PingInterval] turned
	// keepalive off.
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
// The zero value is usable: messages are bounded, writes are bounded, a peer
// that stops answering pings is closed, and a cross-origin handshake is
// refused.
//
// It does not bound how long a connection lasts. A peer that answers every
// ping and sends nothing is alive as far as a keepalive can tell, and with
// MaxLifetime unset nothing ends it, so MaxConnections divided by
// MaxConnectionsPerIP is how many client addresses can hold every slot: 1024 /
// 64 = 16 with the defaults, sixteen IPv4 addresses or a single IPv6 /52 at the
// default /56 grouping. A route that clients one does not control can open is
// worth a guard that authenticates them, a MaxLifetime in minutes, and
// [ClientIPOptions.ConnectionIPv6Prefix] at 48, as [SSEOptions] describes for
// event streams.
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
	// because waiting is what most connections are for. Keepalive, on by
	// default, notices a peer that has stopped answering at all. A negative value
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
	// succeed (only a browser sends one, and the origin check exists to stop
	// browser-based hijacking, not to authenticate a client), so a single
	// unauthenticated, non-browser client can open connections in a tight
	// loop and hold every one of MaxConnections' slots itself, leaving 503
	// for everyone else until it disconnects. This is what stops that: no
	// matter how many connections the process has room for, one address can
	// never hold more than this many of them.
	//
	// The address used is the one [Context.ClientIP] resolves, the same
	// spoof-resistant resolution the rate limiter's default [IPTracker] uses;
	// see [ClientIPOptions] to configure it behind a proxy. By default an
	// IPv4 address is counted exactly and an IPv6 address by its /56, because
	// a client is delegated a whole IPv6 range, can open each connection from
	// a different address in it, and would otherwise get a fresh allowance
	// from every one; [ClientIPOptions.ConnectionIPv6Prefix] and
	// [ClientIPOptions.ConnectionIPv4Prefix] choose how widely addresses are
	// grouped, for this cap and [SSEOptions.MaxStreamsPerIP] together. Like
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
	// the close frame instead of finding the connection reset: the server
	// stops sending, and reads and drops what the peer is still sending, at
	// most 1 MiB of it, until the peer's close frame, the end of its stream or
	// this period, whichever comes first. A negative value closes immediately.
	//
	// A connection opened with [WSDial] uses it to wait for the server to
	// hang up, which RFC 6455 asks the server to do first: once the close
	// frames have crossed, the client reads on to the end of the stream for up
	// to this long before closing its own end.
	CloseGracePeriod time.Duration

	// PingInterval is how often the server pings a connection, defaulting to
	// [DefaultWSPingInterval]. It closes the connection when the peer stops
	// answering, which is what notices a connection dropped by a network that
	// told nobody, and what stops a peer that connects and goes silent from
	// holding a connection slot for as long as it likes. A negative value turns
	// keepalive off.
	//
	// A pong is consumed by a read like any other frame, so the connection is
	// closed for an unanswered ping only when the handler was reading during
	// it. A handler that only ever writes is never closed by keepalive, and
	// should ping by hand with [WSConn.Ping] if it wants that.
	//
	// Anything that arrives from the peer after a ping answers it as well as a
	// pong would, because what is being asked is whether the peer is still
	// there. A peer part way through sending a large message cannot send its
	// pong until the frame it is in is finished, and is kept alive by the
	// bytes of that frame arriving; one that stops sending part way through is
	// closed once a pong timeout passes with nothing at all from it.
	PingInterval time.Duration

	// MaxLifetime is the longest a connection stays open, unset by default,
	// which leaves a connection open for as long as its peer answers pings and
	// its handler runs. When it has passed the connection is closed with
	// [WSStatusGoingAway], the status of a server that is done with a
	// connection rather than one that failed, which the handler sees as the
	// close it reads, and the connection's slot in MaxConnections and
	// MaxConnectionsPerIP is released once the transport is closed. A client
	// that reconnects on going away, as a browser application usually does,
	// carries on.
	//
	// Keepalive closes a peer that stops answering. It cannot close one that
	// answers every ping and never says anything, which costs that peer a TCP
	// connection and holds a slot, a goroutine and a file descriptor here, so
	// it is worth setting on any route a client one does not control can open.
	// A negative value removes the bound a wider scope set.
	MaxLifetime time.Duration

	// PongTimeout is how long a keepalive ping waits for its answer, defaulting
	// to [DefaultWSPongTimeout], which is also what a negative value means: no
	// answer is not a bound worth having. It means nothing once PingInterval
	// has turned keepalive off.
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
	// bytes. Pings, pongs and empty fragments are not messages, and a
	// connection is closed for sending more than 65536 of them in a minute
	// whatever it does otherwise. A peer that goes over is closed with
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
	// An origin is compared as a browser serializes it, a scheme and a host
	// with nothing after it: a value carrying credentials, a path, a query or
	// a fragment is not the server's own origin however its host reads, and a
	// handshake with more than one Origin header is refused as malformed. An
	// entry of this list is compared with the value as it arrived, without
	// regard to case. An entry that could never match, one with a trailing
	// slash, a path, a pattern other than "*" or the scheme's default port, is
	// a build error, and so is "null", which a sandboxed iframe sends and so
	// any page can present.
	//
	// The server's own origin is any whose host is the request's Host, which is
	// whatever the client wrote there. That is enough for a browser that
	// reached the server by its name, and it is not for one that was pointed at
	// it by somebody else's: in a DNS-rebinding attack a page served from the
	// attacker's name, made to resolve to this server, sends that name as both
	// Origin and Host, and reads as same-origin. A server that answers to a
	// known set of names, or that listens on a loopback or private address
	// where such a page is a real threat, lists them in AllowedHosts.
	//
	// The check exists because a WebSocket handshake is not subject to the
	// same-origin policy and is not preflighted: without it, any page on the
	// internet could open an authenticated connection to this server from a
	// visitor's browser, cookies and all. Note that [AppOptions.CORS] has no
	// bearing on it, for exactly that reason.
	AllowedOrigins []string

	// AllowedHosts lists the Host header values this route answers to, such as
	// "app.example.com" or "app.example.com:8443", compared without regard to
	// case and with any port written as the client wrote it. An entry that
	// carries a scheme, a path, credentials or a pattern can never equal a Host
	// header and is a build error. It is unset by
	// default, which leaves the server's own origin to be any whose host is the
	// request's own Host, as described on [WSOptions.AllowedOrigins].
	//
	// When it is set, a handshake counts as same-origin only if its Host is
	// listed, so an Origin that merely repeats a Host the server was never
	// meant to serve, which is how DNS rebinding presents itself, is refused
	// with 403 unless AllowedOrigins or AllowOriginFunc names it. A handshake
	// with no Origin header is not affected, since only a browser sends one.
	// This bounds the WebSocket handshake only: an application that wants every
	// request held to its own names checks Host in a middleware as well.
	AllowedHosts []string

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
	//
	// Set on the application or a router, it covers every route beneath, and
	// a route or router beneath that does need the check turns it back on with
	// EnforceOriginCheck.
	InsecureSkipOriginCheck bool

	// EnforceOriginCheck turns the origin check back on for a router or a
	// route beneath a scope that set InsecureSkipOriginCheck, so that one
	// cookie-authenticated route is not left open to every origin because the
	// rest of the application authenticates by token. It is needed only for
	// that: the check is on by default, and a false value here changes
	// nothing, which is what keeps an option that relaxes security from being
	// the one that a narrower scope can never take back.
	//
	// A scope beneath can set InsecureSkipOriginCheck again, and the narrowest
	// scope that set either wins. One that sets both at once keeps the check,
	// because that is the safe reading of a contradiction.
	EnforceOriginCheck bool
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
	if over.MaxLifetime != 0 {
		o.MaxLifetime = over.MaxLifetime
	}
	if over.MessageLimits != nil {
		o.MessageLimits = over.MessageLimits
	}
	if over.AllowedHosts != nil {
		o.AllowedHosts = over.AllowedHosts
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
	// The two origin switches are one setting with three states, kept as two
	// bools so that InsecureSkipOriginCheck reads as it always has. Whichever
	// the narrower scope set replaces what the wider one chose, and the check
	// wins a scope that set both.
	switch {
	case over.EnforceOriginCheck:
		o.InsecureSkipOriginCheck, o.EnforceOriginCheck = false, true
	case over.InsecureSkipOriginCheck:
		o.InsecureSkipOriginCheck, o.EnforceOriginCheck = true, false
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
	o.PingInterval = orDefaultDuration(o.PingInterval, DefaultWSPingInterval)
	if o.MaxLifetime < 0 {
		o.MaxLifetime = 0
	}
	if o.EnforceOriginCheck {
		// The application's own options are never overlaid on anything, so a
		// contradiction there is settled here, the same way overlay settles
		// one in a narrower scope.
		o.InsecureSkipOriginCheck = false
	}
	if o.PingInterval > 0 {
		if o.PongTimeout <= 0 {
			o.PongTimeout = DefaultWSPongTimeout
		}
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
//
// An error that is the handler's own context ending the conversation is not a
// failure, and is logged at debug level rather than as one. That is an error
// from a read, a write or a wait on conn that the context passed to it ended,
// such as a per-read deadline that bounds how long the peer may stay silent,
// or the error of ctx.Context() itself once it has ended. The peer has been
// told already when the connection ended on such a read or write; one still
// open is closed with [WSStatusGoingAway]. A deadline that ran out anywhere
// else, on a query for instance, is still a failure.
//
// ctx.Context() ends when the connection does, whichever side ends it and
// whether or not the handler is reading at the time, with the connection's
// error as its cause. A handler that waits for events to forward from
// elsewhere can therefore select on it and be released when its peer leaves or
// the server shuts down.
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
// never becomes a connection at all. A handshake's origin and the connection
// limits are checked first, before the guards and dependencies, so that a
// handshake bound to be refused for either runs none of them.
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
	r.mustBeOpen("registering WS " + path)
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
		// handshake is refused. Unlike on the other routes this is done first
		// and on purpose: no handler reads a handshake's body, and one that
		// slipped past the refusal would sit on the transport and be taken for
		// frames once it was upgraded.
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
	r.addRoute(rt)
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
	if err := checkWSOriginLists(rt.Path, rt.websocket.opts); err != nil {
		return err
	}
	if limits := rt.websocket.opts.MessageLimits; len(limits) > 0 && !rt.skipRateLimit {
		messages, err := newRateLimitConfig(rt.rateLimitOpts, limits)
		if err != nil {
			return fmt.Errorf("muzak: WS %s: MessageLimits: %w", rt.Path, err)
		}
		rt.websocket.messages = messages
	}
	rt.websocket.allowOrigin = wsOriginPolicy(rt.websocket.opts)
	// What a handshake can be refused with, so that a client generated from the
	// document expects it. A code the route declared for itself keeps its own
	// wording.
	rt.documentRefusals(
		responseDoc{code: http.StatusBadRequest, description: "The handshake is malformed: the key, the version, an Origin header sent twice, or a body."},
		responseDoc{code: http.StatusUpgradeRequired, description: "The request is not a WebSocket handshake."},
		responseDoc{code: http.StatusServiceUnavailable, description: "The server is shutting down or is holding as many connections as it allows, overall or for this client. The Retry-After header says when to try again."},
	)
	if !rt.websocket.opts.InsecureSkipOriginCheck {
		rt.documentRefusals(responseDoc{code: http.StatusForbidden, description: "The Origin of the handshake may not open a connection here."})
	}
	return nil
}

// documentRefusals adds the responses a route answers on its own account to
// its document, leaving alone any status the route declared for itself: a
// description written for a route is more specific than one written for every
// route of its kind.
func (rt *Route) documentRefusals(docs ...responseDoc) {
	for _, doc := range docs {
		declared := slices.ContainsFunc(rt.responses, func(d responseDoc) bool { return d.code == doc.code })
		if !declared {
			rt.responses = append(rt.responses, doc)
		}
	}
}

// checkWSOriginLists reports every entry of a route's AllowedOrigins and
// AllowedHosts that can never do what it appears to, joined into one error.
//
// The origin list is held to the rules [CORSOptions.AllowedOrigins] is, for
// the same reasons: an entry with a trailing slash, a path, a pattern or the
// scheme's default port never equals the Origin a browser sends, so it lets
// nobody in and shows itself only as a browser refused, and "null" lets in
// every page that arranges to send it, which for a WebSocket means a
// connection opened with the visitor's cookies from anywhere. The single entry
// "*", which allows any origin, is the one pattern the list documents. Case is
// not a mistake here, since the list is compared without regard to it.
//
// A host is compared with the Host header, which never carries a scheme, a
// path, credentials or a pattern, so an entry holding any of them never
// matches either.
func checkWSOriginLists(path string, opts WSOptions) error {
	var errs []error
	for _, entry := range opts.AllowedOrigins {
		if entry == "*" {
			continue
		}
		if err := checkOriginEntry("WS "+path+": AllowedOrigins entry", entry, true); err != nil {
			errs = append(errs, err)
		}
	}
	for _, host := range opts.AllowedHosts {
		if host == "" || strings.ContainsAny(host, "/@*?# \t") {
			errs = append(errs, fmt.Errorf("muzak: WS %s: AllowedHosts entry %q never matches, because it is compared "+
				"with the Host header, which is a host and an optional port such as %q and nothing else",
				path, host, "app.example.com:8443"))
		}
	}
	return errors.Join(errs...)
}

// wsOriginPolicy builds the function that decides which browser origins may
// open a connection.
//
// Only the host is compared for the same-origin case, not the scheme, because
// a server behind a proxy that terminates TLS sees a plain request and cannot
// tell which scheme the browser used. The host is compared with the request's
// Host, which the client chose, so a route that lists AllowedHosts trusts only
// the ones it names.
func wsOriginPolicy(opts WSOptions) func(*http.Request, string) bool {
	allowAny := slices.Contains(opts.AllowedOrigins, "*")
	allowed := opts.AllowedOrigins
	hosts := opts.AllowedHosts
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
		if host, ok := wsOriginHost(origin); ok && strings.EqualFold(host, r.Host) &&
			(len(hosts) == 0 || slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, r.Host) })) {
			return true
		}
		return dynamic != nil && dynamic(r, origin)
	}
}

// wsOriginHost returns the host of an origin, and reports whether the value is
// an origin at all.
//
// An origin is what a browser serializes: a scheme, "://", and a host with an
// optional port, and nothing after it. A lenient URL parse finds a host in
// values that are not one, such as "http://attacker@host", "http://host/path"
// and "//host", and no browser sends any of them. Reading a host out of them
// would let a proxy or a client that is not a browser pass for the server's own
// origin, so they have no host here and are left to the allow list and the
// dynamic policy, which see the value exactly as it arrived.
func wsOriginHost(origin string) (string, bool) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" ||
		parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || strings.Contains(origin, "#") {
		return "", false
	}
	return parsed.Host, true
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
	// Room is taken now, not when the connection is recorded, because the
	// upgrade tells the peer it is connected and a second handshake arriving
	// before the first is counted would be admitted past a limit the first had
	// filled. The key is resolved once and kept for the recording below, so
	// the same client is counted against the same budget in both places.
	connKey, err := a.admitWebSocket(c, true)
	if err != nil {
		return nil, err
	}
	reserved := true
	defer func() {
		if reserved {
			a.websockets.unreserve(connKey)
		}
	}()
	subprotocol := wsSubprotocol(c.r, cfg.opts.Subprotocols)
	conn, err := a.upgrade(c, cfg, accept, subprotocol)
	if err != nil {
		return nil, err
	}
	// Recording the connection gives the room back, whether or not it is kept.
	reserved = false
	if a.websockets.addReserved(conn, connKey) != admitted {
		// coverage: this is the losing side of a race between a handshake and
		// a shutdown, narrowed to the microseconds between the reservation
		// above and the upgrade, so it is reasoned about rather than
		// provoked. The connection is told to go away rather than left
		// unaccounted for; there is no response left to refuse it with by now.
		// It is not waited on for a close frame of its own, because nothing
		// counts it while it lingers.
		_ = conn.sendClose(WSStatusGoingAway, "the server is shutting down")
		_ = conn.fail(&WSCloseError{Status: WSStatusGoingAway, Reason: "the server is shutting down"})
		return nil, errWSShuttingDown
	}
	if cfg.messages != nil {
		conn.messages = &wsMessageLimiter{cfg: cfg.messages, key: messageKey, logger: c.logger, requestID: c.RequestID()}
	}
	return conn, nil
}

// admitWebSocket refuses a handshake the application has no room for and
// returns the key the connection will be counted under.
//
// Refusing before the upgrade is what lets a client shut out by a draining or
// a full server read an ordinary error response. With reserve false it is only
// a check, which is enough to turn a handshake away early. With reserve true
// it takes the room the connection will occupy, under the register's own lock,
// which is what keeps two handshakes arriving together from both passing a
// limit of one; the caller then owes the register either the connection or the
// room back.
func (a *App) admitWebSocket(c *Context, reserve bool) (string, error) {
	connKey, err := perClientKey(c, a.websockets.perKeyLimit)
	if err != nil {
		return "", err
	}
	var verdict admission
	if reserve {
		verdict = a.websockets.reserve(connKey)
	} else {
		verdict = a.websockets.admits(connKey)
	}
	switch verdict {
	case registryDraining:
		return "", errWSShuttingDown
	case registryFull:
		c.w.Header().Set("Retry-After", "5")
		return "", errWSTooManyConnections
	case registryKeyFull:
		c.w.Header().Set("Retry-After", "5")
		return "", errWSTooManyConnectionsFromClient
	}
	return connKey, nil
}

// refuseWebSocket answers a handshake that is going to be refused for who sent
// it or because the application is full, which is everything that can be known
// about it without running the route's guards and dependencies.
//
// They run after it, not before, because they are where a session is looked
// up, an expiry slid or a one-shot token consumed, and a cross-site page can
// make a visitor's browser send the cookie those resolve from. A handshake that
// is bound to be answered 403 or 503 should not have cost any of that, and a
// full server should not spend a store round trip on every handshake it turns
// away. The checks are repeated when the connection is accepted, which is
// cheap and is what keeps the answer right if the register filled in between.
//
// A request that does not ask to upgrade is left to the guards and to the
// handshake check that follows them, so that a client without credentials is
// told to authenticate rather than told what kind of route this is.
func (a *App) refuseWebSocket(c *Context, cfg *wsConfig) error {
	if !headerHasToken(c.r.Header, "Connection", "upgrade") || !headerHasToken(c.r.Header, "Upgrade", "websocket") {
		return nil
	}
	if err := cfg.checkOrigin(c.r); err != nil {
		return err
	}
	_, err := a.admitWebSocket(c, false)
	return err
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
	if len(r.Header.Values("Origin")) > 1 {
		// One handshake names one origin. Two answers to the same question
		// invite this end and whatever is in front of it to read different
		// ones, and a browser never sends two.
		return NewHTTPError(http.StatusBadRequest, "the Origin header was sent more than once")
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
	// buy so much as an allocation. Decoding first would hand a stranger a
	// buffer three quarters the size of whatever it sent, up to the header
	// limit, on every handshake it attempted.
	if len(key) != 24 {
		return "", errWSBadKey()
	}
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		return "", errWSBadKey()
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // see the comment above
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// errWSBadKey reports a Sec-WebSocket-Key that is not one.
func errWSBadKey() error {
	return NewHTTPError(http.StatusBadRequest,
		"the Sec-WebSocket-Key header is missing or is not sixteen base64 encoded bytes")
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
	// The handler's context ends with the connection, not only when the
	// handler returns; see [WSConn.cancelOnEnd].
	handlerCtx, cancel := context.WithCancelCause(c.r.Context())
	defer cancel(nil)
	conn.cancelOnEnd(cancel)
	c.r = c.r.WithContext(handlerCtx)

	// The context watched is the handler's own rather than the request's it
	// derives from. It still ends when the request does, and it is the one a
	// handler passes to nearly every read and write it makes, which is what
	// lets those skip an arrangement of their own; watching the request's
	// context instead matched no operation a handler ever made. It is watched
	// before the keepalive starts, so the pings see it arranged too.
	conn.watch(handlerCtx)
	if opts := c.route.websocket.opts; opts.PingInterval > 0 {
		go conn.keepalive(handlerCtx, opts.PingInterval, opts.PongTimeout)
	}

	// The connection's slot is returned only once its transport is closed, and
	// this is the first deferred call so that it runs after the closing one
	// below, including when that one is re-raising a panic. Closing is not
	// instant: it waits for the peer's own close frame, and for the write half
	// when another goroutine is stuck writing to a peer that does not read. A
	// slot freed before that would let a peer that withholds its close frame
	// hold sockets and goroutines the caps say nobody may.
	defer a.websockets.remove(conn)

	if lifetime := c.route.websocket.opts.MaxLifetime; lifetime > 0 {
		// The close is the ordinary one, so the handler blocked in a read is
		// woken by it and returns, and the peer is told why in the way it is
		// told everything else.
		defer time.AfterFunc(lifetime, func() {
			_ = conn.Close(WSStatusGoingAway, "the connection reached its maximum lifetime")
		}).Stop()
	}

	status, reason := WSStatusNormalClosure, ""
	defer func() {
		conn.stopWatching()
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
		switch {
		case errors.As(err, &closed):
			// The handler either reported the closure it read or chose one of
			// its own, and either way it has already said what to send.
			status, reason = closed.Status, closed.Reason
		case wsEndedByContext(err, handlerCtx):
			// The handler's own context ended the conversation: a deadline it
			// gave its peer passed, it cancelled an operation, or the
			// connection ended and took the context with it. That is how a
			// connection ends rather than a failure, and the peer has usually
			// been told already, but it is worth a line for anyone asking
			// why one stopped.
			status, reason = wsContextClosure(err, false)
			a.logger.DebugContext(c.Context(), "muzak: a websocket connection ended",
				slog.String("route", c.route.Path),
				slog.String(RequestIDKey, c.RequestID()),
				slog.String("reason", err.Error()))
		default:
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

// wsEndedByContext reports whether an error a handler returned is its own
// context ending the conversation rather than something going wrong.
//
// Two shapes count. One is an error from a read, a write or a wait on the
// connection that the context passed to it ended, which is what a handler
// that bounds how long its peer may stay silent returns. The other is the
// handler's own context's error once that context has ended, which is what a
// handler waiting on the context rather than on a read returns. A deadline that
// ran out anywhere else, on a query the handler made for instance, is neither,
// and is reported as the failure it is.
func wsEndedByContext(err error, handlerCtx context.Context) bool {
	var ended *wsContextError
	if errors.As(err, &ended) {
		return true
	}
	ctxErr := handlerCtx.Err()
	return ctxErr != nil && errors.Is(err, ctxErr)
}
