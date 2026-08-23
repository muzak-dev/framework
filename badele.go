package badele

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"badele/internal/radix"
)

// Errors reported for configurations Badele refuses to serve.
var (
	// ErrCORSWildcardCredentials reports a CORS policy that pairs a wildcard
	// origin with credentials. Browsers reject that combination, so accepting
	// it here would only hide the mistake until it reached a browser.
	ErrCORSWildcardCredentials = errors.New("badele: a wildcard CORS origin cannot be combined with AllowCredentials")

	// ErrNotBuilt reports an operation attempted on an application whose
	// routes failed to build.
	ErrNotBuilt = errors.New("badele: the application could not be built")
)

// Default limits applied when [AppOptions] leaves them unset. Every one of
// them is a bound on work an unauthenticated client can ask the server to do,
// so none of them defaults to zero.
const (
	// DefaultMaxBodySize is the largest request body accepted by a route that
	// does not override it, at one mebibyte.
	DefaultMaxBodySize int64 = 1 << 20
	// DefaultMaxUploadSize is the largest form body accepted by a route that
	// binds files or form values and does not override it, at 32 mebibytes.
	DefaultMaxUploadSize int64 = 32 << 20
	// DefaultMaxHeaderBytes is the largest request header block accepted, at
	// one mebibyte.
	DefaultMaxHeaderBytes = 1 << 20
)

// AppOptions configures an application.
//
// The zero value is usable: it produces an application that listens on :8080,
// serves its OpenAPI document and documentation UI, applies the default
// timeouts and body limit, and logs to standard error. Because
// [OpenAPIOptions] is embedded, its fields can be set inline:
//
//	app := badele.New(badele.AppOptions{
//		Title:   "Bigger Applications Example",
//		Version: "1.0.0",
//		Addr:    ":8080",
//	})
type AppOptions struct {
	// OpenAPIOptions describes the API in the generated document. Its fields
	// may be set directly in an AppOptions literal.
	OpenAPIOptions

	// Addr is the TCP address the server listens on, defaulting to ":8080".
	Addr string

	// ServerOptions carries the listener timeouts and shutdown behaviour. Its
	// fields may be set directly in an AppOptions literal.
	ServerOptions

	// MaxBodySize is the default request body limit in bytes, overridable per
	// route with [MaxBodySize]. It defaults to [DefaultMaxBodySize]. A
	// negative value removes the limit, which is only appropriate behind a
	// proxy that imposes its own.
	MaxBodySize int64

	// MaxUploadSize is the default limit in bytes on a form body, overridable
	// per route with [MaxUploadSize]. It applies to every route that binds
	// `form` or `file` fields, in place of MaxBodySize, and defaults to
	// [DefaultMaxUploadSize]. A negative value removes the limit, which is
	// only appropriate behind a proxy that imposes its own.
	MaxUploadSize int64

	// MaxFileSize is the default limit in bytes on any single uploaded file,
	// overridable per route with [MaxFileSize]. Zero, the default, leaves each
	// file bounded only by MaxUploadSize.
	MaxFileSize int64

	// Logger is the logger the application uses. When nil, one is built from
	// LoggerOptions.
	Logger *slog.Logger

	// LoggerOptions configures the logger built when Logger is nil.
	LoggerOptions LoggerOptions

	// ErrorRenderer converts errors into responses. It defaults to
	// [DefaultErrorRenderer]; set it to change the error envelope.
	ErrorRenderer ErrorRenderer

	// DocsPath is where the documentation UI is served, defaulting to
	// "/docs". Set [AppOptions.DisableDocs] to serve neither it nor the
	// OpenAPI document.
	DocsPath string

	// OpenAPIPath is where the OpenAPI document is served, defaulting to
	// "/openapi.json".
	OpenAPIPath string

	// DisableDocs stops the OpenAPI document and the documentation UI from
	// being served, for deployments that must not describe themselves.
	DisableDocs bool

	// DisableAccessLog stops the per-request access log from being installed.
	DisableAccessLog bool

	// AccessLogOptions configures the access log when it is installed.
	AccessLogOptions AccessLogOptions

	// TrustRequestIDHeader accepts a client-supplied X-Request-Id when it
	// parses as a UUID, instead of always generating one. See
	// [RequestIDOptions] for why this is off by default.
	TrustRequestIDHeader bool

	// DisableSecurityHeaders stops [SecurityHeaders] from being installed.
	DisableSecurityHeaders bool

	// CORS configures cross-origin sharing. The zero value denies every
	// cross-origin request, and no CORS middleware is installed unless an
	// origin or an origin function is configured.
	CORS CORSOptions

	// WebSocket configures the WebSocket connections of every route registered
	// with [Router.WS], and is narrowed for a router or a route with
	// [WithWebSocket]. The zero value bounds message and write sizes and
	// refuses a cross-origin handshake.
	WebSocket WSOptions

	// SSE configures the event streams of every route registered with
	// [Router.SSE], and is narrowed for a router or a route with [WithSSE].
	// The zero value bounds writes, holds an idle stream open with a periodic
	// keepalive, and caps how many streams the application serves at once.
	SSE SSEOptions

	// RateLimit bounds how fast a client may make requests, and is narrowed
	// for a router or a route with [WithRateLimit], [RateLimit] and
	// [SkipRateLimit]. The zero value limits nothing: a policy has to name at
	// least one [Quota] before any request is counted.
	RateLimit RateLimitOptions

	// ClientIP decides which address a request is attributed to, which
	// matters wherever a decision is made per client rather than per request.
	// The zero value believes no forwarding header, so behind a proxy it
	// attributes every request to the proxy until the proxy is named in
	// [ClientIPOptions.TrustedProxies].
	ClientIP ClientIPOptions
}

// App is a Badele application: a root router plus the server, middleware,
// generated documentation and error handling that turn it into something that
// serves HTTP.
//
// App embeds *Router, so routes can be registered on it directly with the same
// generic methods any nested router uses, and other routers can be mounted
// with [Router.Include].
type App struct {
	*Router

	opts        AppOptions
	logger      *slog.Logger
	renderError ErrorRenderer
	middleware  []Middleware

	tree    *radix.Tree[*pathEntry]
	entries map[string]*pathEntry
	routes  []*Route
	// frontends are the static mounts, ordered longest path first so that the
	// most specific mount answers a path two of them could both serve.
	frontends []*frontend
	routers   int
	spec      *Document

	lifecycle *lifecycleManager

	// clientIP answers which address a request came from, with the trusted
	// proxy policy parsed once. clientIPErr holds the reason a policy could
	// not be parsed, reported when the application is built rather than
	// swallowed by New, which never fails.
	clientIP    *clientIPResolver
	clientIPErr error

	// websockets tracks the open WebSocket connections, which net/http cannot
	// do for us because a hijacked connection is no longer one of its own.
	websockets liveRegistry[*WSConn]

	// streams tracks the open server-sent event streams. net/http does know
	// about those, which is the problem: a shutdown would wait for every one
	// of them until its deadline, because a handler that is still streaming is
	// a handler that has not returned.
	streams liveRegistry[*sseStream]

	buildOnce sync.Once
	buildErr  error
	handler   http.Handler

	ctxPool sync.Pool

	// server is stored atomically because Addr and Shutdown are documented to
	// be callable from a different goroutine than the one running the server,
	// which is exactly what a caller asking for port ":0" has to do.
	server atomic.Pointer[serverRunner]
}

// buildState collects everything discovered while walking the router tree, so
// that one pass gathers the errors and the lifecycle components together.
type buildState struct {
	errs       []error
	lifecycles []Lifecycle
	frontends  []*frontend
}

// pathEntry holds every method served at one path template, so that a single
// tree walk answers both "does this path exist" and "is this method allowed".
type pathEntry struct {
	methods map[string]*Route
	allow   string
}

// New creates an application.
//
// Options given after the AppOptions value configure the root router and are
// inherited by every route and every included router, which is how an
// application-wide guard is declared:
//
//	app := badele.New(badele.AppOptions{
//		Title:   "Bigger Applications Example",
//		Version: "1.0.0",
//		Addr:    ":8080",
//	}, badele.WithDependencies(GetQueryToken))
//
// New never fails. Problems with the routes, such as a duplicate path or an
// unbindable input type, are reported by [App.Build] and by the methods that
// call it.
func New(opts AppOptions, routerOpts ...RouterOption) *App {
	opts = opts.withDefaults()
	logger := opts.Logger
	if logger == nil {
		logger = NewLogger(opts.LoggerOptions)
	}
	app := &App{
		Router:      NewRouter(routerOpts...),
		opts:        opts,
		logger:      logger,
		renderError: opts.ErrorRenderer,
		tree:        radix.New[*pathEntry](),
		entries:     make(map[string]*pathEntry),
	}
	app.lifecycle = &lifecycleManager{logger: Scoped(logger, ScopeServer)}
	app.clientIP, app.clientIPErr = newClientIPResolver(opts.ClientIP)
	app.ctxPool.New = func() any { return new(Context) }
	app.installDefaultMiddleware()
	return app
}

// withDefaults fills in every unset option, so the rest of the package can
// read AppOptions without repeating fallbacks.
func (o AppOptions) withDefaults() AppOptions {
	if o.Addr == "" {
		o.Addr = ":8080"
	}
	if o.MaxBodySize == 0 {
		o.MaxBodySize = DefaultMaxBodySize
	}
	if o.MaxUploadSize == 0 {
		o.MaxUploadSize = DefaultMaxUploadSize
	}
	if o.ErrorRenderer == nil {
		o.ErrorRenderer = DefaultErrorRenderer
	}
	if o.DocsPath == "" {
		o.DocsPath = "/docs"
	}
	if o.OpenAPIPath == "" {
		o.OpenAPIPath = "/openapi.json"
	}
	o.ServerOptions = o.ServerOptions.withDefaults()
	return o
}

// installDefaultMiddleware assembles the standard chain. Order matters:
// request identification comes first so that everything after it can log the
// identifier, recovery sits above the access log so a panic is still recorded
// as a response, and the access log wraps the router so it observes the final
// status.
func (a *App) installDefaultMiddleware() {
	a.middleware = append(a.middleware, RequestID(RequestIDOptions{
		TrustInboundHeader: a.opts.TrustRequestIDHeader,
	}))
	if !a.opts.DisableSecurityHeaders {
		a.middleware = append(a.middleware, SecurityHeaders())
	}
	a.middleware = append(a.middleware, Recovery(Scoped(a.logger, ScopeServer)))
	if !a.opts.DisableAccessLog {
		a.middleware = append(a.middleware, AccessLog(a.logger, a.opts.AccessLogOptions))
	}
}

// Use installs middleware that runs for every request, including those for the
// documentation UI and the OpenAPI document.
//
// Middleware installed here runs inside the built-in chain, so it already has
// a request identifier available and is already covered by panic recovery.
// Calls to Use after the application has been built have no effect, because
// the chain is assembled once.
func (a *App) Use(middleware ...Middleware) {
	a.middleware = append(a.middleware, middleware...)
}

// Logger returns the application's logger. Derive a scoped child from it for
// each subsystem with [Scoped].
func (a *App) Logger() *slog.Logger { return a.logger }

// Options applies further router options to the application after it was
// created, which is how a dependency discovered later is published:
//
//	app.Options(badele.WithSingleton(models, badele.LifecycleFunc("ml-model", start, stop)))
//
// Options must be called before the application is built. Calls made
// afterwards have no effect, because the routing tree and the dependency
// chains are resolved once.
func (a *App) Options(opts ...RouterOption) {
	for _, opt := range opts {
		opt.applyRouter(&a.cfg)
	}
}

// Config returns the fully defaulted options the application was built with,
// which is the reliable way to read a value such as the listen address after
// New has filled in the blanks.
func (a *App) Config() AppOptions { return a.opts }

// Build resolves the routing tree, compiles every binding plan and generates
// the OpenAPI document.
//
// It is called automatically by [App.ServeHTTP] and by the run methods, so
// calling it explicitly is only necessary to surface configuration errors
// early, which is what a test or a start-up check wants. Building is
// idempotent: the work happens once and later calls return the same result.
//
// The returned error joins every problem found, so a misconfigured application
// reports all of them at once rather than one per attempt.
func (a *App) Build() error {
	a.buildOnce.Do(a.build)
	return a.buildErr
}

// build performs the one-time resolution behind [App.Build].
func (a *App) build() {
	startup := Scoped(a.logger, ScopeServer)
	startup.Info("Starting Badele application...")

	state := &buildState{}
	seenOperationIDs := make(map[string]string)

	emit := func(rt *Route) error {
		entry, err := a.entryFor(rt.Path)
		if err != nil {
			return err
		}
		if existing, taken := entry.methods[rt.Method]; taken {
			return fmt.Errorf("badele: %s %s is registered twice (the first registration returned %s)", rt.Method, rt.Path, existing.OperationID)
		}
		if previous, taken := seenOperationIDs[rt.OperationID]; taken {
			return fmt.Errorf("badele: operation id %q is used by both %s and %s %s; set a unique one with badele.OperationID", rt.OperationID, previous, rt.Method, rt.Path)
		}
		seenOperationIDs[rt.OperationID] = rt.Method + " " + rt.Path
		entry.methods[rt.Method] = rt
		a.routes = append(a.routes, rt)
		return nil
	}

	a.routers = countRouters(a.Router)
	a.finalize(inherited{
		maxBodySize:           a.opts.MaxBodySize,
		maxUploadSize:         a.opts.MaxUploadSize,
		maxFileSize:           a.opts.MaxFileSize,
		ws:                    a.opts.WebSocket,
		wsMaxConnections:      a.opts.WebSocket.MaxConnections,
		wsMaxConnectionsPerIP: a.opts.WebSocket.MaxConnectionsPerIP,
		sse:                   a.opts.SSE,
		sseMaxStreams:         a.opts.SSE.MaxStreams,
		sseMaxStreamsPerIP:    a.opts.SSE.MaxStreamsPerIP,
		rateLimit:             a.opts.RateLimit,
	}, emit, state)
	a.websockets.limit = wsConnectionLimit(a.opts.WebSocket.MaxConnections)
	a.websockets.perKeyLimit = wsConnectionsPerIPLimit(a.opts.WebSocket.MaxConnectionsPerIP)
	a.streams.limit = sseStreamLimit(a.opts.SSE.MaxStreams)
	a.streams.perKeyLimit = sseStreamsPerIPLimit(a.opts.SSE.MaxStreamsPerIP)
	if a.clientIPErr != nil {
		state.errs = append(state.errs, a.clientIPErr)
	}
	// Rate limiting is completed once every route is known, because a quota
	// name means the same thing everywhere and a storage nobody named is
	// created once and shared.
	a.resolveRateLimiting(state)
	a.lifecycle.components = state.lifecycles
	a.frontends = state.frontends
	slices.SortStableFunc(a.frontends, func(x, y *frontend) int {
		return len(y.path) - len(x.path)
	})

	if len(state.errs) > 0 {
		a.buildErr = errors.Join(state.errs...)
		startup.Error("badele: the application could not be built", slog.String("error", a.buildErr.Error()))
		return
	}

	for _, entry := range a.entries {
		entry.allow = allowHeader(entry.methods)
	}

	Scoped(a.logger, ScopeRouter).Info(
		fmt.Sprintf("Registered %d %s across %d %s",
			len(a.routes), plural(len(a.routes), "route"),
			a.routers, plural(a.routers, "router")))

	if !a.opts.DisableDocs {
		a.spec = a.buildDocument()
		Scoped(a.logger, ScopeDocs).Info("Serving API documentation",
			slog.String("openapi", a.opts.OpenAPIPath),
			slog.String("docs", a.opts.DocsPath))
	}
	a.handler = a.buildHandler()
}

// entryFor returns the path entry for a template, inserting it into the tree
// the first time the template is seen. Entries are kept in a map keyed by the
// template as well, because several methods share one entry and the Allow
// header cannot be computed until every one of them is registered.
func (a *App) entryFor(path string) (*pathEntry, error) {
	if entry, ok := a.entries[path]; ok {
		return entry, nil
	}
	entry := &pathEntry{methods: make(map[string]*Route, 4)}
	if err := a.tree.Insert(path, entry); err != nil {
		return nil, fmt.Errorf("badele: %w", err)
	}
	a.entries[path] = entry
	return entry, nil
}

// countRouters counts the routers in a subtree, for the start-up summary.
func countRouters(r *Router) int {
	n := 1
	for _, inc := range r.includes {
		n += countRouters(inc.child)
	}
	return n
}

// plural returns word with an "s" when n is not one, so log lines read
// naturally without a format directive per message.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// allowHeader builds the Allow header value for a path, including the methods
// Badele answers automatically.
func allowHeader(methods map[string]*Route) string {
	names := make([]string, 0, len(methods)+2)
	for method := range methods {
		names = append(names, method)
	}
	// A GET route answers HEAD automatically, unless it is one whose body is a
	// conversation: there is no way to upgrade a HEAD, and answering one from
	// an event stream would run a handler whose every write is discarded until
	// it gave up, so promising either would be a lie.
	if get, hasGet := methods[http.MethodGet]; hasGet && get.answersHead() {
		if _, hasHead := methods[http.MethodHead]; !hasHead {
			names = append(names, http.MethodHead)
		}
	}
	if _, hasOptions := methods[http.MethodOptions]; !hasOptions {
		names = append(names, http.MethodOptions)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// buildHandler wraps the router in the middleware chain and the documentation
// routes.
func (a *App) buildHandler() http.Handler {
	var handler http.Handler = http.HandlerFunc(a.dispatch)
	if !a.opts.DisableDocs {
		handler = a.withDocs(handler)
	}
	if cors, ok := a.corsMiddleware(); ok {
		handler = cors(handler)
	}
	for i := len(a.middleware) - 1; i >= 0; i-- {
		handler = a.middleware[i](handler)
	}
	return handler
}

// corsMiddleware builds the CORS middleware when a policy was configured. A
// policy that cannot be served safely is reported as a build error rather than
// silently applied.
func (a *App) corsMiddleware() (Middleware, bool) {
	opts := a.opts.CORS
	if len(opts.AllowedOrigins) == 0 && opts.AllowOriginFunc == nil {
		return nil, false
	}
	mw, err := CORS(opts)
	if err != nil {
		a.buildErr = errors.Join(a.buildErr, err)
		return nil, false
	}
	return mw, true
}

// ServeHTTP implements http.Handler, building the application on first use.
//
// A build failure is reported as a 500 for every request, with the reason
// logged once at error level rather than sent to clients. Use [App.Build] to
// detect the failure before serving.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := a.Build(); err != nil {
		id, _ := RequestIDFromContext(r.Context())
		writeMinimalError(w, id)
		return
	}
	a.handler.ServeHTTP(w, r)
}

// dispatch matches a request to a route and runs it.
func (a *App) dispatch(w http.ResponseWriter, r *http.Request) {
	rw := asResponseWriter(w)
	c := a.acquire(rw, r)
	defer a.release(c)

	entry, found := a.tree.Lookup(r.URL.EscapedPath(), &c.params)
	if !found {
		// Every route is matched before any frontend is consulted, so a
		// frontend mounted at the root cannot shadow an API.
		if mount, relative, served := a.frontendFor(r.URL.Path); served {
			a.serveFrontend(c, mount, relative)
			return
		}
		a.fail(c, NewHTTPErrorf(http.StatusNotFound, "no route matches %s %s", r.Method, r.URL.Path))
		return
	}

	route, ok := entry.methods[r.Method]
	if !ok {
		a.dispatchFallback(c, entry)
		return
	}
	a.run(c, route)
}

// dispatchFallback answers a request whose path exists but whose method has no
// handler, covering automatic HEAD and OPTIONS before reporting 405.
func (a *App) dispatchFallback(c *Context, entry *pathEntry) {
	switch c.r.Method {
	case http.MethodHead:
		if route, ok := entry.methods[http.MethodGet]; ok && route.answersHead() {
			// net/http discards the body of a HEAD response, so running the
			// GET handler yields correct headers with no body.
			a.run(c, route)
			return
		}
	case http.MethodOptions:
		c.w.Header().Set("Allow", entry.allow)
		c.w.WriteHeader(http.StatusNoContent)
		return
	}
	c.w.Header().Set("Allow", entry.allow)
	a.fail(c, NewHTTPErrorf(http.StatusMethodNotAllowed, "%s is not allowed here; allowed methods are %s", c.r.Method, entry.allow))
}

// run executes a matched route: percent-decoding its captured parameters,
// resolving its dependencies, and invoking its handler.
func (a *App) run(c *Context, route *Route) {
	c.route = route
	c.status = route.Status
	if err := unescapeParams(&c.params); err != nil {
		// coverage: net/http normalises the request URL before a handler runs,
		// so EscapedPath never yields an escape that PathUnescape rejects. The
		// branch guards against a caller driving dispatch directly, and the
		// decoding itself is covered by TestUnescapeParams.
		a.fail(c, err)
		return
	}
	defer a.recoverRoute(c)

	// The count comes before the dependencies by default, so that a client
	// past its limit is refused before anything expensive is done on its
	// behalf, and so that a request rejected by a guard is still counted.
	limits := route.rateLimit
	if limits != nil && !limits.afterDependencies {
		if err := limits.check(c); err != nil {
			a.fail(c, err)
			return
		}
	}
	if err := route.resolveDependencies(c); err != nil {
		a.fail(c, err)
		return
	}
	if limits != nil && limits.afterDependencies {
		if err := limits.check(c); err != nil {
			a.fail(c, err)
			return
		}
	}
	if err := route.invoke(c); err != nil {
		a.fail(c, err)
	}
}

// recoverRoute turns a panic inside a handler or a dependency into the normal
// error response, so that the request identifier and the configured error
// envelope still apply. The stack trace is logged, never sent.
func (a *App) recoverRoute(c *Context) {
	recovered := recover()
	if recovered == nil {
		return
	}
	// recover returns any, not error, so errors.Is does not apply here. This is
	// the same identity comparison net/http performs on the sentinel.
	if recovered == http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
		panic(recovered)
	}
	a.logger.ErrorContext(c.Context(), "badele: recovered from a panic in a handler",
		slog.Any("panic", recovered),
		slog.String("method", c.r.Method),
		slog.String("route", c.route.Path),
		slog.String(RequestIDKey, c.RequestID()),
		slog.String("stack", string(debug.Stack())))
	a.fail(c, errPanic)
}

// errPanic stands in for a recovered panic so that the error renderer sees an
// ordinary error and produces the standard opaque 500.
var errPanic = errors.New("badele: handler panicked")

// fail renders an error into the response using the configured renderer.
func (a *App) fail(c *Context, err error) {
	if cause := logCause(err); cause != nil {
		a.logger.ErrorContext(c.Context(), "badele: request failed",
			slog.String("method", c.r.Method),
			slog.String("path", c.r.URL.Path),
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("error", cause.Error()))
	}
	status, body := a.renderError(c, err)
	if c.w.written {
		return
	}
	if body == nil {
		c.w.WriteHeader(status)
		return
	}
	c.status = status
	if writeErr := c.writeResponse(body); writeErr != nil {
		// The renderer produced something that cannot be serialized. Fall back
		// to the fixed envelope so the client still receives valid JSON.
		a.logger.ErrorContext(c.Context(), "badele: the error renderer produced an unserializable body",
			slog.String("error", writeErr.Error()))
		writeMinimalError(c.w, c.RequestID())
	}
}

// acquire takes a Context from the pool and binds it to the request.
func (a *App) acquire(w *responseWriter, r *http.Request) *Context {
	c := a.ctxPool.Get().(*Context)
	c.w = w
	c.r = r
	c.app = a
	c.logger = a.logger
	c.status = http.StatusOK
	c.requestID, _ = RequestIDFromContext(r.Context())
	return c
}

// release clears the Context and returns it to the pool. Clearing is what
// makes pooling safe: a Context that kept a previous request's dependencies
// could hand them to the next request that borrowed it.
func (a *App) release(c *Context) {
	c.reset()
	a.ctxPool.Put(c)
}

// unescapeParams percent-decodes the captured path parameters in place. The
// tree matches against the escaped path so that an encoded separator cannot
// split a segment; decoding afterwards gives handlers the literal value.
func unescapeParams(params *radix.Params) error {
	for i := range params.Len() {
		name, value := params.At(i)
		if !strings.ContainsRune(value, '%') {
			continue
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return NewHTTPErrorf(http.StatusBadRequest, "path parameter %q is not correctly percent-encoded", name).Wrap(err)
		}
		params.SetValue(i, decoded)
	}
	return nil
}

// responseBufferPool recycles the buffers used to serialize responses.
// Responses are encoded fully before anything is written, so that a failure
// partway through encoding still produces a clean error response instead of a
// truncated body.
var responseBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// writeResponse serializes a value as the response body.
func (c *Context) writeResponse(v any) error {
	if c.w.written {
		// The handler wrote the response itself, which is a supported way to
		// stream; there is nothing left to encode.
		return nil
	}
	status := clampStatus(c.status)
	if status == http.StatusNoContent || status == http.StatusNotModified {
		c.w.WriteHeader(status)
		return nil
	}
	if document, isHTML := v.(HTML); isHTML {
		return c.writeHTML(status, document)
	}

	buf := responseBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBodyBuffer {
			responseBufferPool.Put(buf)
		}
	}()

	if err := json.MarshalWrite(buf, v); err != nil {
		return fmt.Errorf("badele: encoding the response of %s %s failed: %w", c.r.Method, c.route.pathOrRequest(c.r), err)
	}
	header := c.w.Header()
	setIfAbsent(header, "Content-Type", "application/json; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(buf.Len()))
	c.w.WriteHeader(status)
	_, err := c.w.Write(buf.Bytes())
	return err
}

// pathOrRequest names a route for an error message, falling back to the
// request path for failures that happen before a route is matched.
func (rt *Route) pathOrRequest(r *http.Request) string {
	if rt == nil {
		return r.URL.Path
	}
	return rt.Path
}
