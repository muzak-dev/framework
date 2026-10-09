package muzak

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"muzak.dev/framework/internal/radix"
)

// Errors reported for configurations Muzak refuses to serve.
var (
	// ErrCORSWildcardCredentials reports a CORS policy that pairs a wildcard
	// origin with credentials. Browsers reject that combination, so accepting
	// it here would only hide the mistake until it reached a browser.
	ErrCORSWildcardCredentials = errors.New("muzak: a wildcard CORS origin cannot be combined with AllowCredentials")

	// ErrNotBuilt reports an operation attempted on an application whose
	// routes failed to build.
	ErrNotBuilt = errors.New("muzak: the application could not be built")
)

// Default limits applied when [AppOptions] leaves them unset. Every one of
// them is a bound on work an unauthenticated client can ask the server to do,
// so none of them defaults to zero.
const (
	// DefaultMaxBodySize is the largest request body accepted by a route that
	// does not override it, at one mebibyte.
	DefaultMaxBodySize int64 = 1 << 20
	// DefaultMaxUploadSize is the largest multipart body accepted by a route
	// that binds files and does not override it, at 32 mebibytes.
	DefaultMaxUploadSize int64 = 32 << 20
	// DefaultMaxHeaderBytes is the largest request header block accepted, at
	// 64 KiB. net/http's own default is a mebibyte, and a client that never
	// finishes a header block holds all of what it has sent for as long as
	// ReadHeaderTimeout allows; a browser's cookies and an ordinary token fit
	// in a small fraction of this.
	DefaultMaxHeaderBytes = 64 << 10
)

// AppOptions configures an application.
//
// The zero value is usable: it produces an application that listens on :8080,
// serves its OpenAPI document and documentation UI, applies the default
// timeouts and body limit, and logs to standard error. Because
// [OpenAPIOptions] is embedded, its fields can be set inline:
//
//	app := muzak.New(muzak.AppOptions{
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

	// MaxUploadSize is the default limit in bytes on a multipart body,
	// overridable per route with [MaxUploadSize]. It applies to every route
	// that binds `file` fields, in place of MaxBodySize, and defaults to
	// [DefaultMaxUploadSize]. A route that binds only `form` values, whether it
	// is sent a urlencoded or a multipart body, carries no file and is bounded
	// by MaxBodySize instead. A negative value removes the limit, which is
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

	// DocsUI is the documentation dashboard served at [AppOptions.DocsPath],
	// and is nil by default: an application describes itself with the OpenAPI
	// document at [AppOptions.OpenAPIPath], and adds a UI when it wants one.
	//
	//	import "muzak.dev/openapi/ui"
	//
	//	muzak.AppOptions{DocsUI: ui.Files()}
	//
	// The dashboard is a module of its own so that a service which does not
	// want one does not carry it: Go downloads and links a module only when
	// something imports it, so leaving this unset costs a binary nothing at
	// all rather than embedding a page it will never serve. Nothing is
	// fetched at run time either way; a UI that is configured is one that is
	// already in the binary.
	//
	// Any [fs.FS] meeting the contract works, which is how a service serves a
	// dashboard of its own: an index.html at the root, every absolute URL in
	// it written under "/__muzak_docs__/" and the OpenAPI document fetched
	// from "/__muzak_spec__", both of which are rewritten to the configured
	// paths when the application is built. No other file may carry an
	// absolute URL, because only the page is rewritten.
	//
	// With no UI configured, DocsPath answers as any other unknown path does.
	//
	// The page, its assets and the OpenAPI document are answered ahead of
	// routing, but not ahead of the application: the guards and providers
	// given to [New] run first, and a refusal is rendered as it would be for
	// a route. An application-wide token therefore covers the documentation
	// too. To publish the documentation while the API stays private, declare
	// the guard on an included router rather than on New, or set
	// [AppOptions.DisableDocs].
	DocsUI fs.FS

	// DocsPath is where the documentation UI is served, defaulting to
	// "/docs". It is an absolute path on this application's own origin, so
	// it begins with a slash and is matched exactly:
	//
	//	muzak.AppOptions{DocsPath: "/reference", OpenAPIPath: "/reference/openapi.json"}
	//
	// Both paths are reported when the server starts listening, as URLs that
	// can be opened from the terminal. A path that is not absolute, that
	// collides with the other one, or that an application route already
	// answers is a build error rather than a page nobody can reach. Set
	// [AppOptions.DisableDocs] to serve neither.
	DocsPath string

	// OpenAPIPath is where the OpenAPI document is served, defaulting to
	// "/openapi.json". It follows the same rules as [AppOptions.DocsPath],
	// and the page reads the document from wherever this puts it.
	OpenAPIPath string

	// DisableDocs stops the OpenAPI document and the documentation UI from
	// being served, for deployments that must not describe themselves.
	// Neither path is registered and no document is generated, so
	// [App.Document] returns nil and both paths answer as any other unknown
	// path does.
	//
	// Documentation that is served runs the application-wide rate limit,
	// guards and providers given to [New] before anything is sent, so leaving
	// this unset does not publish an API its own guards keep private, nor
	// offer a place to test credentials that the rate limit does not count;
	// a guarded document is then marked private to caches as well. Guards and
	// limits declared on an included router or a route do not apply to it,
	// because the document describes every router at once.
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

	// Versioning configures how requests declare which version of the API
	// they want. The zero value leaves versioning off, which is what every
	// route gets unless this is set; see [VersioningOptions] and
	// [WithVersion].
	Versioning VersioningOptions

	// I18n configures how a request's locale is decided and where the
	// translations come from. The zero value leaves internationalization off:
	// no middleware is installed and every message the framework produces reads
	// exactly as it does without this feature existing. See [I18nOptions].
	I18n I18nOptions

	// Health serves the liveness and readiness endpoints a platform probes.
	// The zero value serves neither; see [HealthOptions].
	Health HealthOptions

	// Background sizes the worker pool that runs the tasks handlers register
	// with [Context.AfterResponse]. The zero value is a small pool that starts
	// no goroutine until a task is registered; see [BackgroundOptions].
	Background BackgroundOptions

	// Tracing starts a server span for every request through a [Tracer],
	// continues the W3C trace context a request carries, and joins the access
	// log and [Context.Logger] to the trace. The zero value leaves it off and
	// costs nothing; see [TracingOptions].
	Tracing TracingOptions

	// Observer is told about every request once its response is written,
	// which is where request metrics are recorded. Nil, the default, costs
	// nothing; see [RequestObserver].
	Observer RequestObserver
}

// App is a Muzak application: a root router plus the server, middleware,
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
	// userMiddleware is what [App.Use] installed, kept apart from the
	// built-in chain because the CORS policy sits between the two.
	userMiddleware []Middleware

	tree    *radix.Tree[*pathEntry]
	entries map[string]*pathEntry
	routes  []*Route
	// frontends are the static mounts, ordered longest path first so that the
	// most specific mount answers a path two of them could both serve.
	frontends []*frontend
	routers   int
	spec      *Document
	// docsLimits is the stand-in route carrying the rate limit the
	// documentation is served under; see [App.resolveDocsRateLimit]. It is
	// nil when the documentation is disabled.
	docsLimits *Route

	lifecycle *lifecycleManager

	// overrides are the providers [App.Override] substitutes when the
	// application is built; see [App.finishDependencies].
	overrides []*dependencyOverride

	// clientIP answers which address a request came from, with the trusted
	// proxy policy parsed once. clientIPErr holds the reason a policy could
	// not be parsed, reported when the application is built rather than
	// swallowed by New, which never fails.
	clientIP    *clientIPResolver
	clientIPErr error

	// obs is the tracing and request observer state, nil when neither is
	// configured; see [observability].
	obs *observability

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

	// server records the run in progress, and the one before it for Addr. It
	// is guarded because Addr and Shutdown are documented to be callable from
	// a different goroutine than the one running the server, which is exactly
	// what a caller asking for port ":0" has to do; see [runState].
	server runState

	// readiness is what the readiness endpoint reports before its checks,
	// health answers the health endpoints and is nil when they are not
	// enabled, and background runs the tasks of [Context.AfterResponse].
	readiness  readinessState
	health     *healthEndpoints
	background *backgroundPool
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
//
// A method maps to a slice rather than a single Route because versioning can
// register more than one route at the same method and path, distinguished
// only by the version a request declares; see [Route.versionVariants] and
// [selectVersion]. An application that never enables versioning never sees a
// slice longer than one, so the indirection costs it nothing beyond the
// slice header itself.
type pathEntry struct {
	methods map[string][]*Route
	allow   string
}

// New creates an application.
//
// Options given after the AppOptions value configure the root router and are
// inherited by every route and every included router, which is how an
// application-wide guard is declared:
//
//	app := muzak.New(muzak.AppOptions{
//		Title:   "Bigger Applications Example",
//		Version: "1.0.0",
//		Addr:    ":8080",
//	}, muzak.WithDependencies(GetQueryToken))
//
// The guards, providers and rate limit declared here also run before the
// OpenAPI document and the documentation UI are served, since those describe
// the whole application; see [AppOptions.DisableDocs].
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
	app.lifecycle = &lifecycleManager{logger: Scoped(logger, ScopeServer), stopTimeout: opts.ShutdownTimeout}
	app.background = newBackgroundPool(opts.Background, Scoped(logger, ScopeServer))
	app.clientIP, app.clientIPErr = newClientIPResolver(opts.ClientIP)
	app.obs = newObservability(opts, app.clientIP, logger)
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
	o.I18n = o.I18n.withDefaults()
	o.ServerOptions = o.ServerOptions.withDefaults()
	o.Health = o.Health.withDefaults()
	o.Background = o.Background.withDefaults()
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
	if a.obs != nil {
		// Above recovery and the access log; see [observability.middleware].
		a.middleware = append(a.middleware, a.obs.middleware)
	}
	if !a.opts.DisableSecurityHeaders {
		a.middleware = append(a.middleware, SecurityHeaders())
	}
	a.middleware = append(a.middleware, Recovery(Scoped(a.logger, ScopeServer)))
	// Probes are answered here, before anything a probe has no use for and
	// anything that could refuse it; see HealthOptions.
	if a.opts.Health.Enabled {
		a.middleware = append(a.middleware, a.healthMiddleware)
	}
	if a.opts.I18n.enabled() {
		a.middleware = append(a.middleware, Locale(a.opts.I18n))
	}
	if !a.opts.DisableAccessLog {
		a.middleware = append(a.middleware, AccessLog(a.logger, a.opts.AccessLogOptions))
	}
}

// Use installs middleware that runs for every request, including those for the
// documentation UI and the OpenAPI document. The health endpoints are the
// exception, answered before it; see [HealthOptions].
//
// Middleware installed here runs inside the built-in chain, so it already has
// a request identifier available and is already covered by panic recovery. It
// also runs inside the CORS policy when one is configured: a preflight is
// answered by the policy without reaching it, so an authenticating middleware
// need not let OPTIONS through, and a response it writes itself, a 401 or a
// 429, carries the Access-Control-Allow-Origin a browser needs to read it.
// Use must be called before the application is built, whether explicitly by
// [App.Build] or implicitly by [App.ServeHTTP], [App.Document] or a run
// method, because the chain is assembled once. A call made afterwards panics
// rather than leave a middleware, possibly one that denies access, silently
// uninstalled.
func (a *App) Use(middleware ...Middleware) {
	a.mustBeOpen("App.Use")
	a.userMiddleware = append(a.userMiddleware, middleware...)
}

// Logger returns the application's logger. Derive a scoped child from it for
// each subsystem with [Scoped].
func (a *App) Logger() *slog.Logger { return a.logger }

// Options applies further router options to the application after it was
// created, which is how a dependency discovered later is published:
//
//	app.Options(muzak.WithSingleton(models, muzak.LifecycleFunc("ml-model", start, stop)))
//
// Options must be called before the application is built, whether explicitly
// by [App.Build] or implicitly by [App.ServeHTTP], [App.Document] or a run
// method, because the routing tree and the dependency chains are resolved
// once. A call made afterwards panics rather than leave a guard silently
// unapplied. Registering a route or including a router after the build panics
// for the same reason.
func (a *App) Options(opts ...RouterOption) {
	a.mustBeOpen("App.Options")
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
// It is called automatically by [App.ServeHTTP], [App.Document] and the run
// methods, so calling it explicitly is only necessary to surface
// configuration errors early, which is what a test or a start-up check wants.
// Building is idempotent: the work happens once and later calls return the
// same result. Building also freezes the configuration: [App.Use],
// [App.Options], registering a route and including a router all panic once it
// has happened, since none of them could take effect any more.
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
	startup.Info("Starting Muzak application...")

	state := &buildState{}
	seenOperationIDs := make(map[string]string)

	emit := func(rt *Route) error {
		variants := rt.versionVariants(a.opts.Versioning)
		if len(variants) == 0 && a.opts.Versioning.enabled() {
			Scoped(a.logger, ScopeRouter).Warn(fmt.Sprintf(
				"%s %s declares no version and no default version applies; it will answer no request while versioning is enabled",
				rt.Method, rt.Path))
		}
		for _, variant := range variants {
			entry, err := a.entryFor(variant.Path)
			if err != nil {
				return err
			}
			for _, existing := range entry.methods[variant.Method] {
				// Without versioning there is no dimension to distinguish two
				// routes at the same method and path by, so any second
				// registration conflicts; with it, only an overlapping
				// version does, since disjoint versions are exactly what
				// lets more than one route share a method and path.
				if !a.opts.Versioning.enabled() || versionsOverlap(existing.Versions, variant.Versions) {
					return fmt.Errorf("muzak: %s %s is registered twice (the first registration returned %s)",
						variant.Method, variant.Path, existing.OperationID)
				}
			}
			if previous, taken := seenOperationIDs[variant.OperationID]; taken {
				return fmt.Errorf("muzak: operation id %q is used by both %s and %s %s; set a unique one with muzak.OperationID", variant.OperationID, previous, variant.Method, variant.Path)
			}
			seenOperationIDs[variant.OperationID] = variant.Method + " " + variant.Path
			entry.methods[variant.Method] = append(entry.methods[variant.Method], variant)
			a.routes = append(a.routes, variant)
		}
		return nil
	}

	if err := a.opts.I18n.validate(); err != nil {
		state.errs = append(state.errs, err)
	}
	if err := a.opts.Versioning.validate(); err != nil {
		state.errs = append(state.errs, err)
	}
	if err := a.opts.validate(); err != nil {
		state.errs = append(state.errs, err)
	}
	if err := a.opts.validateSecuritySchemes(); err != nil {
		state.errs = append(state.errs, err)
	}
	if err := a.opts.Tracing.validate(); err != nil {
		state.errs = append(state.errs, err)
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
		versions:              a.opts.Versioning.DefaultVersion,
	}, emit, state)
	a.finishDependencies(state)
	a.websockets.limit = wsConnectionLimit(a.opts.WebSocket.MaxConnections)
	a.websockets.perKeyLimit = wsConnectionsPerIPLimit(a.opts.WebSocket.MaxConnectionsPerIP)
	a.streams.limit = sseStreamLimit(a.opts.SSE.MaxStreams)
	a.streams.perKeyLimit = sseStreamsPerIPLimit(a.opts.SSE.MaxStreamsPerIP)
	if !a.opts.Versioning.enabled() {
		// A resolved version can only exist here because [WithVersion] was
		// called somewhere, since nothing else can produce one without
		// AppOptions.Versioning.DefaultVersion, which validate() above
		// already refused to let exist without Type set too. Registering
		// such a route anyway, silently ignoring the version it declared,
		// would be a much easier mistake to ship than to notice.
		for _, rt := range a.routes {
			if len(rt.Versions) > 0 {
				state.errs = append(state.errs, fmt.Errorf(
					"muzak: %s %s: a version is declared but versioning is not enabled; set AppOptions.Versioning to enable it",
					rt.Method, rt.Path))
			}
		}
	}
	// The documentation paths are checked once every route is known, because
	// one of the things that can be wrong with them is colliding with a route.
	a.validateDocsPaths(state)
	if err := a.validateSecurity(); err != nil {
		state.errs = append(state.errs, err)
	}
	if a.clientIPErr != nil {
		state.errs = append(state.errs, a.clientIPErr)
	}
	// Rate limiting is completed once every route is known, because a quota
	// name means the same thing everywhere and a storage nobody named is
	// created once and shared. The documentation's limit is resolved first so
	// that it is completed with the rest.
	a.resolveDocsRateLimit(state)
	a.resolveRateLimiting(state)
	a.lifecycle.components = observabilityComponents(state.lifecycles, a.opts.Tracing.Tracer, a.opts.Observer)
	a.frontends = state.frontends
	// Two mounts at one path cannot both answer, and the one registered first
	// would take every request, whatever guards the second was given. That is
	// a mistake to report rather than a precedence to apply quietly.
	mounted := make(map[string]bool, len(a.frontends))
	for _, f := range a.frontends {
		if mounted[f.path] {
			state.errs = append(state.errs, fmt.Errorf("muzak: %s at %q: a mount is registered at this path more than once", f.kind, f.mountPath()))
		}
		mounted[f.path] = true
	}
	slices.SortStableFunc(a.frontends, func(x, y *frontend) int {
		return len(y.path) - len(x.path)
	})
	a.linkNestedMounts()
	a.validateOperations(state)

	// Built with the other checks, not while the handler is assembled, so a
	// policy that cannot be served is reported with them: after that point the
	// failure had already been logged as a success and nothing said why every
	// request was refused.
	cors, err := a.corsMiddleware()
	if err != nil {
		state.errs = append(state.errs, err)
	}

	if len(state.errs) > 0 {
		a.buildErr = errors.Join(state.errs...)
		startup.Error("muzak: the application could not be built", slog.String("error", a.buildErr.Error()))
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
		Scoped(a.logger, ScopeDocs).Debug("Documentation prepared",
			slog.String("docs", a.opts.DocsPath),
			slog.String("openapi", a.opts.OpenAPIPath))
	}
	a.handler = a.buildHandler(cors)
}

// entryFor returns the path entry for a template, inserting it into the tree
// the first time the template is seen. Entries are kept in a map keyed by the
// template as well, because several methods share one entry and the Allow
// header cannot be computed until every one of them is registered.
func (a *App) entryFor(path string) (*pathEntry, error) {
	if entry, ok := a.entries[path]; ok {
		return entry, nil
	}
	entry := &pathEntry{methods: make(map[string][]*Route, 4)}
	if err := a.tree.Insert(path, entry); err != nil {
		return nil, fmt.Errorf("muzak: %w", err)
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
// Muzak answers automatically.
func allowHeader(methods map[string][]*Route) string {
	names := make([]string, 0, len(methods)+2)
	for method := range methods {
		names = append(names, method)
	}
	// A GET route answers HEAD automatically, unless it is one whose body is a
	// conversation: there is no way to upgrade a HEAD, and answering one from
	// an event stream would run a handler whose every write is discarded until
	// it gave up, so promising either would be a lie. Versioning can register
	// more than one GET route here; HEAD is offered if any of them would
	// answer it, since which one actually does is a per-request decision
	// this header cannot make.
	if get, hasGet := methods[http.MethodGet]; hasGet && anyAnswersHead(get) {
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

// anyAnswersHead reports whether any of a method's registered routes answers
// a HEAD request; see [Route.answersHead].
func anyAnswersHead(routes []*Route) bool {
	for _, rt := range routes {
		if rt.answersHead() {
			return true
		}
	}
	return false
}

// buildHandler wraps the router in the middleware chain and the documentation
// routes. The built-in chain is outermost, then the CORS policy when one was
// configured, then the middleware [App.Use] installed, so that a response one
// of those writes itself still carries the policy's headers.
func (a *App) buildHandler(cors Middleware) http.Handler {
	var handler http.Handler = http.HandlerFunc(a.dispatch)
	if !a.opts.DisableDocs {
		handler = a.withDocs(handler)
	}
	for i := len(a.userMiddleware) - 1; i >= 0; i-- {
		handler = a.userMiddleware[i](handler)
	}
	if cors != nil {
		handler = cors(handler)
	}
	for i := len(a.middleware) - 1; i >= 0; i-- {
		handler = a.middleware[i](handler)
	}
	// Outermost, so everything above -- the access log included -- can read the
	// template dispatch writes.
	return withRouteHolder(handler)
}

// corsMiddleware builds the CORS middleware when a policy was configured, and
// returns nil when none was. A policy that cannot be served safely is returned
// as an error for the caller to report as a build error rather than silently
// applied.
func (a *App) corsMiddleware() (Middleware, error) {
	opts := a.opts.CORS
	if len(opts.AllowedOrigins) == 0 && opts.AllowOriginFunc == nil {
		return nil, nil
	}
	return CORS(opts)
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
	// A response with nothing to write is committed by net/http from the
	// header as it stands when this returns, so what was declared about Vary
	// is merged in before then.
	defer rw.commitVary()

	entry, found := a.tree.Lookup(r.URL.EscapedPath(), &c.params)
	if !found {
		// Every route is matched before any frontend is consulted, so a
		// frontend mounted at the root cannot shadow an API.
		if mount, relative, served := a.frontendFor(r.URL.Path); served {
			a.serveFrontend(c, mount, relative)
			a.settleServed(c)
			return
		}
		a.fail(c, noRouteError(r))
		return
	}

	candidates, ok := entry.methods[r.Method]
	if !ok {
		a.dispatchFallback(c, entry)
		return
	}
	route := a.matchVersion(c, candidates)
	if route == nil {
		// The path and method both exist, but nothing registered for them
		// answers the version this request declared, which the framework
		// treats exactly as it would a path nothing matches at all: see
		// [selectVersion].
		a.fail(c, noRouteError(r))
		return
	}
	a.run(c, route)
}

// matchVersion picks the candidate that answers r's declared version, among
// every route registered for the same method and path.
//
// Versioning off guarantees exactly one candidate, since [WithVersion] may
// not be used anywhere without it: that candidate is returned directly,
// without resolving a version from the request at all, which is what keeps
// an application that never enables versioning paying nothing for it.
// [VersioningURI] is answered the same way, for a different reason: the
// version there is part of the path, so reaching this method's slot for a
// given path already settled which version matched, and there is nothing
// left to extract from the request itself. Every other type keeps more than
// one candidate at one path and resolves the version from the request to
// choose between them; see [selectVersion].
//
// Choosing by a request header makes the response depend on that header, so
// it is added to Vary first: a shared cache that stored the answer for one
// version without it would hand it to a client asking for another. It is
// added before a candidate is chosen, because the 404 for a version nothing
// answers depends on the header just as much. The one exception is a path
// whose only candidate is version-neutral, which answers every request the
// same way.
func (a *App) matchVersion(c *Context, candidates []*Route) *Route {
	versioning := a.opts.Versioning
	if !versioning.enabled() || versioning.Type == VersioningURI {
		return candidates[0]
	}
	if field := versioning.varyField(); field != "" && (len(candidates) > 1 || !candidates[0].isVersionNeutral()) {
		// Declared rather than added, so a handler that sets a Vary of its own
		// cannot replace it; see [responseWriter].
		c.w.varyOn(field)
	}
	return selectVersion(candidates, versioning.requestedVersions(c.r))
}

// dispatchFallback answers a request whose path exists but whose method has no
// handler, covering automatic HEAD and OPTIONS before reporting 405.
//
// OPTIONS and the 405 are answered from the route table without running any
// route's guards, providers or rate limit, because no route is called: they
// disclose that the path exists and the methods it takes, which is what the
// documentation lists too, and nothing a route returns. Middleware installed
// with [App.Use] still wraps them. A HEAD answered by a GET route is a call to
// that route and runs everything the GET does.
func (a *App) dispatchFallback(c *Context, entry *pathEntry) {
	switch c.r.Method {
	case http.MethodHead:
		if candidates, ok := entry.methods[http.MethodGet]; ok {
			if route := a.matchVersion(c, candidates); route != nil && route.answersHead() {
				// net/http discards the body of a HEAD response, so running
				// the GET handler yields correct headers with no body.
				a.run(c, route)
				return
			}
		}
	case http.MethodOptions:
		c.w.Header().Set("Allow", entry.allow)
		c.w.WriteHeader(http.StatusNoContent)
		return
	}
	c.w.Header().Set("Allow", entry.allow)
	a.fail(c, NewHTTPErrorf(http.StatusMethodNotAllowed, "%s is not allowed here; allowed methods are %s", quotableMethod(c.r.Method), entry.allow))
}

// run executes a matched route: percent-decoding its captured parameters,
// resolving its dependencies, and invoking its handler.
func (a *App) run(c *Context, route *Route) {
	c.route = route
	c.status = route.Status
	// Published for instrumentation, into the holder installed on the way in.
	// Middleware above keeps the request it was handed, so a new context made
	// here would never reach it; writing through the holder does. It is done
	// here rather than in dispatch so that a HEAD answered by a GET route is
	// named after that route too, in the access log, the span and the
	// observer alike.
	if holder, ok := c.r.Context().Value(routeContextKey{}).(*routeHolder); ok {
		holder.template = route.Path
	}
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
	// A WebSocket handshake that is going to be refused for its shape, its
	// origin or a full server is refused before anything runs on its behalf.
	if route.websocket != nil {
		if err := a.refuseWebSocket(c, route.websocket); err != nil {
			a.fail(c, err)
			return
		}
	}
	if route.timeout > 0 {
		// After the rate limit and the refusals above, which are the
		// server's own decisions, and before everything the route itself
		// runs, so that a guard, a provider and the binder all see the
		// deadline the handler is held to.
		defer c.startDeadline(route.timeout)()
	}
	// Before the dependencies, so a guard verifying a signature over the bytes
	// sees them, and after the rate limit above, so a client past its budget is
	// refused without the server buffering a body on its behalf.
	if route.captureBody {
		if err := captureRequestBody(c, route); err != nil {
			a.fail(c, route.timedOut(c, err))
			return
		}
	}
	if err := route.resolveDependencies(c); err != nil {
		a.fail(c, route.timedOut(c, err))
		return
	}
	if limits != nil && limits.afterDependencies {
		if err := limits.check(c); err != nil {
			a.fail(c, route.timedOut(c, err))
			return
		}
	}
	if route.isGuarded() && !c.endsItsOwnResponse() {
		// Set before the handler runs rather than when the response is
		// written, so it covers a handler that writes its own body too, and
		// only if absent, so a middleware's policy stands. A handler that
		// sets its own Cache-Control replaces it; one serving something
		// genuinely public says so there.
		setIfAbsent(c.w.Header(), "Cache-Control", privateCacheControl)
	}
	if err := route.invoke(c); err != nil {
		a.fail(c, route.timedOut(c, err))
	}
}

// privateCacheControl is what a guarded response is marked with unless its
// handler says otherwise; see [Route.isGuarded].
const privateCacheControl = "private, no-cache"

// isGuarded reports whether the route runs any guard or request-scoped
// provider, declared on it or inherited from a router or the application. A
// singleton alone does not count; see [answersPerClient].
//
// Such a response is presumed to depend on who asked: a guard decides whether
// this client may see it and a provider typically resolves the client's own
// session or account. Sent with no Cache-Control, a shared cache may store it
// heuristically and hand one user's response to the next, so it is marked
// "private, no-cache", which keeps it out of shared caches and has the
// client's own cache revalidate it. It is the same value the guarded OpenAPI
// document is served with. An event stream already sends its own
// "no-cache, no-transform", which this would otherwise displace, and a
// WebSocket upgrade is never cached, so both are left to their own headers.
func (rt *Route) isGuarded() bool {
	return answersPerClient(rt.guards, rt.providers)
}

// recoverRoute turns a panic inside a handler or a dependency into the normal
// error response, so that the request identifier and the configured error
// envelope still apply. The stack trace is logged, never sent.
func (a *App) recoverRoute(c *Context) {
	recovered := recover()
	if recovered == nil {
		return
	}
	// Releases learn of the panic first, the abort below included; see
	// [Context.settle].
	_ = c.settle(panicFailure(recovered))
	// recover returns any, not error, so errors.Is does not apply here. This is
	// the same identity comparison net/http performs on the sentinel.
	if recovered == http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
		panic(recovered)
	}
	// Something panicked after all, if not the handler then while its
	// response was written, so the tasks it registered do not run.
	c.handled = false
	a.logger.ErrorContext(c.Context(), "muzak: recovered from a panic in a handler",
		slog.String("panic", panicValue(recovered)),
		slog.String("method", c.r.Method),
		slog.String("route", c.route.Path),
		slog.String(RequestIDKey, c.RequestID()),
		slog.String("stack", string(debug.Stack())))
	a.fail(c, errPanic)
}

// errPanic stands in for a recovered panic so that the error renderer sees an
// ordinary error and produces the standard opaque 500.
var errPanic = errors.New("muzak: handler panicked")

// fail renders an error into the response using the configured renderer.
//
// A request that fails after its response has started, whether its handler
// returned an error or panicked, is logged and then aborted rather than
// rendered: see [abortStartedResponse]. The same applies to a returned error
// as to a panic, because a handler that streams (rows written as they are
// read, say) reports a failure halfway through by returning one, and the
// client of that stream is owed the same signal either way. A handler that
// wrote a complete response and then returned an error loses nothing it could
// have kept: net/http never learns the response was meant to be final, and
// the error says it was not. A hijacked connection is left alone, and so is
// a WebSocket or event stream route; see [Context.endsItsOwnResponse].
func (a *App) fail(c *Context, err error) {
	a.observeFailure(c, err)
	// Releases see the failure before anything is written, and returning it
	// unchanged is settle's contract when one is given.
	_ = c.settle(err)
	if c.w.written && !c.w.hijacked && !c.endsItsOwnResponse() {
		// Every failure is logged here, even the deliberate 4xx logCause
		// would leave out, because the response the client sees says nothing
		// about why the connection dropped.
		cause := logCause(err)
		if cause == nil {
			cause = err
		}
		a.logger.ErrorContext(c.Context(), "muzak: request failed after its response had started, so the connection was aborted",
			slog.String("method", c.r.Method),
			slog.String("path", truncateForMessage(c.r.URL.Path)),
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("error", cause.Error()))
		abortStartedResponse(c.w)
	}
	if cause := logCause(err); cause != nil {
		a.logger.ErrorContext(c.Context(), "muzak: request failed",
			slog.String("method", c.r.Method),
			slog.String("path", truncateForMessage(c.r.URL.Path)),
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("error", cause.Error()))
	}
	var location []string
	if !c.w.written {
		// A Location belongs to a redirect, which an application's own error
		// can be, so it is set aside and put back below if that is what the
		// renderer answers with. Every other status has no use for one.
		location = c.w.Header().Values("Location")
		// Before the renderer runs rather than after, so a renderer that sets
		// a header of its own, a problem+json Content-Type say, keeps it.
		resetForError(c.w.Header(), c.locale)
	}
	status, body := a.renderError(c, err)
	if c.w.written {
		// Only a hijacked connection or a streaming route reaches this, and
		// either one has already ended its response in its own way.
		return
	}
	if status >= 300 && status < 400 && len(location) > 0 && c.w.Header().Get("Location") == "" {
		c.w.Header()["Location"] = location
	}
	if body == nil {
		// Clamped here as writeResponse clamps a status sent with a body: a
		// renderer that forgot to set one returns 0, and net/http panics on a
		// code outside 100 to 999 rather than writing it, which would cost the
		// client its connection instead of answering it.
		c.w.WriteHeader(clampStatus(status))
		return
	}
	c.status = status
	if writeErr := c.writeResponse(body); writeErr != nil {
		// The renderer produced something that cannot be serialized. Fall back
		// to the fixed envelope so the client still receives valid JSON.
		a.logger.ErrorContext(c.Context(), "muzak: the error renderer produced an unserializable body",
			slog.String("error", writeErr.Error()))
		writeMinimalError(c.w, c.RequestID())
	}
}

// successEntityHeaders are the response headers that describe a body, or how
// long a cache may keep it, and so are only true of the body they were set
// for; see [resetForError].
var successEntityHeaders = [...]string{
	"Content-Type",
	"Content-Length",
	"Content-Disposition",
	"Content-Encoding",
	"Content-Range",
	"ETag",
	"Last-Modified",
	"Cache-Control",
	"Expires",
	// Integrity, range and framing declarations about a body that was never
	// sent: a checksum of other bytes, an invitation to a Range request the
	// envelope cannot serve, a trailer that will not arrive.
	"Content-Digest",
	"Repr-Digest",
	"Digest",
	"Content-MD5",
	"Accept-Ranges",
	"Trailer",
	// Where the entity the handler meant to send lives. A Location names the
	// resource a success created or moved, and is kept only for the one
	// status it belongs to; see [App.fail].
	"Content-Location",
	"Location",
}

// resetForError clears what a handler declared about the success response it
// never sent, before an error is written in its place.
//
// A handler sets its headers first and fails afterwards, and every one of
// them used to reach the error envelope. The envelope then went out as
// whatever the handler meant to send: a 404 quoting client input served as
// text/html, an attachment to be saved to disk, a transient 503 a shared cache
// could keep for a day under the success response's Cache-Control, or an ETag
// and Last-Modified that let a later conditional request revalidate the error
// as though it were the resource. The rule is that a header describing the
// body, its integrity, its ranges or its caching goes, and every other header stays: Vary (the error
// depends on the same inputs the success would have), the security headers,
// Retry-After, Allow, a WWW-Authenticate a guard set to name its scheme,
// Set-Cookie, the request identifier and anything else middleware or the
// handler declared about the exchange rather than the entity. Content-Language
// is put back to the locale the error is rendered in, because a handler may
// have changed it for content that is no longer being sent.
//
// An error is never worth storing, so it is sent with "Cache-Control:
// no-store". The header is set here, before the error renderer runs, so a
// renderer that wants a failure cached can say so.
func resetForError(header http.Header, locale string) {
	for _, name := range successEntityHeaders {
		header.Del(name)
	}
	if locale != "" {
		header.Set(HeaderContentLanguage, locale)
	} else {
		header.Del(HeaderContentLanguage)
	}
	header.Set("Cache-Control", "no-store")
}

// endsItsOwnResponse reports whether the request was routed to a WebSocket or
// event stream route, whose failures fail leaves to the route rather than
// aborting.
//
// Both protocols frame every message they carry, so a stream that stops early
// cannot be mistaken for a complete document the way a truncated JSON array
// or CSV can, and both already report a failure in their own terms: a
// WebSocket with a close frame, an event stream by ending, which an
// EventSource answers by reconnecting. A refusal written after something else
// in the chain had already answered is left as that answer, too.
func (c *Context) endsItsOwnResponse() bool {
	return c.route != nil && (c.route.websocket != nil || c.route.sse != nil)
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
	c.locale, _ = LocaleFromContext(r.Context())
	c.i18n = a.opts.I18n.Store
	return c
}

// release clears the Context and returns it to the pool. Clearing is what
// makes pooling safe: a Context that kept a previous request's dependencies
// could hand them to the next request that borrowed it.
//
// It also removes whatever a multipart form spilled to disk, whoever parsed
// it. The binder removes its own form's files, but a guard or handler that
// calls FormValue on [Context.Request] parses the form itself, and
// net/http's own cleanup cannot see that form: it removes the files of the
// request it handed to the server's handler, and the request here is a copy
// the middleware made with WithContext, whose form is set on the copy alone.
// Every request that parsed a multipart body that way left its files behind
// for good, as large as the client cared to make them.
func (a *App) release(c *Context) {
	// Every path through a route settles its releases before it returns; one
	// still pending here was left by a panic outside a route.
	_ = c.settle(errReleaseAbandoned)
	releaseUpload(c.r)
	a.settleTasks(c)
	c.reset()
	a.ctxPool.Put(c)
}

// noRouteError reports a request nothing answers, naming it in a form the
// error envelope can always carry.
func noRouteError(r *http.Request) error {
	return NewHTTPErrorf(http.StatusNotFound, "no route matches %s %s", quotableMethod(r.Method), quotablePath(r))
}

// quotablePath returns the request path as a message may quote it.
//
// The decoded path is whatever bytes the client percent-encoded, so GET /%ff
// decodes to a byte that is not UTF-8. Quoting that in the message made the
// JSON envelope unencodable: the client got a 500 in place of a 404, and the
// fallback logged the path. A path of printable ASCII, which is nearly every
// path, is quoted as it is; anything else is quoted in its escaped form, which
// is ASCII by construction and is what the client put on the wire.
//
// Either form is cut to [maxQuotedLength]; see [truncateForMessage].
func quotablePath(r *http.Request) string {
	if isPrintableASCII(r.URL.Path) {
		return truncateForMessage(r.URL.Path)
	}
	return truncateForMessage(r.URL.EscapedPath())
}

// quotableMethod returns a request method as a message may quote it. net/http
// only admits a token, so this matters solely for a request built by hand and
// passed to ServeHTTP, which is still not allowed to turn a 404 into a 500. A
// token can still be as long as the request line, so it is cut the same way a
// path is.
func quotableMethod(method string) string {
	if isPrintableASCII(method) {
		return truncateForMessage(method)
	}
	return truncateForMessage(strconv.QuoteToASCII(method))
}

// maxQuotedLength bounds how much of a client-chosen path or method a message
// or a log line quotes. It is far longer than any path an application routes,
// so a real one is never cut.
const maxQuotedLength = 1024

// truncatedMarker ends a value [truncateForMessage] cut, so that a reader can
// tell it from a path that really ended there.
const truncatedMarker = "...(truncated)"

// truncateForMessage returns s as it is when it is at most [maxQuotedLength]
// bytes long, and otherwise its first [maxQuotedLength] bytes followed by
// [truncatedMarker].
//
// net/http admits a request line of about a mebibyte, and every byte of it the
// client chose to be a path was quoted in full: into the 404 message, three
// bytes for each one that needed escaping, and into the access log line,
// where each invalid byte becomes a six-byte JSON escape. A single request
// could make the server write several times what it sent, to the client and
// to the log store. Nothing an application routes comes near the bound, so a
// real path reads exactly as before.
//
// The cut is moved back to the start of a UTF-8 sequence, at most three bytes,
// so a well-formed path is not left ending in half a character; the bound on
// the walk keeps a run of bytes that are not UTF-8 at all from moving it
// further.
func truncateForMessage(s string) string { return truncateTo(s, maxQuotedLength) }

// truncateTo is [truncateForMessage] for a limit of the caller's choosing.
func truncateTo(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for i := 0; i < utf8.UTFMax-1 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut] + truncatedMarker
}

// isPrintableASCII reports whether s holds only printable ASCII, the text a
// message can quote without escaping anything.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// unescapeParams percent-decodes the captured path parameters in place. The
// tree matches against the escaped path so that an encoded separator cannot
// split a segment; decoding afterwards gives handlers the literal value. The
// tree compares static segments decoded as well, so a value captured here can
// never be the spelling of a static sibling, which is what keeps a parameter
// from answering a request a guarded static route was registered for.
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
//
// The request's [Acquire] releases run once the body is known to be writable
// and before any of it is, which is what lets a commit that fails replace the
// success with an error; every branch below settles them before its first
// write. With none pending, settling is a length check.
func (c *Context) writeResponse(v any) error {
	// Bytes, Stream, Redirect and FileResponse are written as they are; see
	// responses.go.
	if typed, err := c.writeTyped(v); typed {
		return err
	}
	if c.w.written {
		// The handler wrote the response itself, which is a supported way to
		// stream; there is nothing left to encode.
		return c.settle(nil)
	}
	status := clampStatus(c.status)
	if status == http.StatusNoContent || status == http.StatusNotModified {
		if err := c.settle(nil); err != nil {
			return err
		}
		c.w.WriteHeader(status)
		return nil
	}
	if document, isHTML := v.(HTML); isHTML {
		if err := c.settle(nil); err != nil {
			return err
		}
		return c.writeHTML(status, document)
	}

	buf := responseBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBodyBuffer {
			responseBufferPool.Put(buf)
		}
	}()

	if err := json.MarshalWrite(buf, v, c.jsonOptions(status)); err != nil {
		return fmt.Errorf("muzak: encoding the response of %s %s failed: %w", c.r.Method, c.route.pathOrRequest(c.r), err)
	}
	if err := c.settle(nil); err != nil {
		return err
	}
	header := c.w.Header()
	setIfAbsent(header, "Content-Type", "application/json; charset=utf-8")
	if c.notModifiedBytes(status, buf.Bytes()) {
		return nil
	}
	header.Set("Content-Length", strconv.Itoa(buf.Len()))
	c.w.WriteHeader(status)
	_, err := c.w.Write(buf.Bytes())
	return err
}

// pathOrRequest names a route for an error message, falling back to the
// request path for failures that happen before a route is matched.
func (rt *Route) pathOrRequest(r *http.Request) string {
	if rt == nil {
		return truncateForMessage(r.URL.Path)
	}
	return rt.Path
}
