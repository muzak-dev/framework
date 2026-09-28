package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
)

// Handler is the shape every Muzak route handler takes.
//
// In is the fully typed request: its fields are bound from the path, query
// string, headers and JSON body according to their struct tags, and it is
// [Empty] for routes that read nothing. Out is the response body, serialized
// exactly as returned: there is no wrapper type and no runtime filtering, so
// the handler's return type is the response model and the compiler enforces
// it. Returning a non-nil error abandons Out and produces an error response
// instead; see [HTTPError] for choosing the status.
type Handler[In, Out any] func(ctx *Context, in In) (Out, error)

// RouteOption configures a single route at registration time.
type RouteOption interface {
	applyRoute(*routeConfig)
}

// RouterOption configures a router, an application, or the point at which one
// router is included into another.
type RouterOption interface {
	applyRouter(*routerConfig)
}

// SharedOption is an option that is meaningful both for a whole router and for
// an individual route, such as [WithTags] or [WithDependencies]. Applying one
// to a router makes it apply to every route beneath it.
type SharedOption interface {
	RouteOption
	RouterOption
}

// routeOptionFunc adapts a function into a [RouteOption].
type routeOptionFunc func(*routeConfig)

func (f routeOptionFunc) applyRoute(c *routeConfig) { f(c) }

// routerOptionFunc adapts a function into a [RouterOption].
type routerOptionFunc func(*routerConfig)

func (f routerOptionFunc) applyRouter(c *routerConfig) { f(c) }

// sharedOption carries the two adaptations an option needs to work at both
// levels.
type sharedOption struct {
	route  func(*routeConfig)
	router func(*routerConfig)
}

func (o sharedOption) applyRoute(c *routeConfig)   { o.route(c) }
func (o sharedOption) applyRouter(c *routerConfig) { o.router(c) }

// responseDoc is one documented response outcome.
//
// model is the type the body carries, and is nil for an outcome declared with
// [WithResponseDoc], which is documented as the standard error envelope
// because that is what an error returned from a handler actually produces.
type responseDoc struct {
	code        int
	description string
	model       reflect.Type
}

// routerConfig accumulates the settings a router contributes to every route
// beneath it.
type routerConfig struct {
	prefix             string
	tags               []string
	guards             []Guard
	providers          []*provider
	responses          []responseDoc
	lifecycles         []Lifecycle
	maxBodySize        int64
	maxUploadSize      int64
	maxFileSize        int64
	ws                 *WSOptions
	sse                *SSEOptions
	rateLimit          *RateLimitOptions
	allowUnknownFields *bool
	captureBody        *bool
	versions           []Version
	versionsSet        bool
	deprecated         bool
	hidden             bool
	skipRateLimit      bool
}

// routeConfig accumulates the settings declared on a single route.
type routeConfig struct {
	tags               []string
	guards             []Guard
	providers          []*provider
	responses          []responseDoc
	status             int
	summary            string
	description        string
	operationID        string
	maxBodySize        int64
	maxUploadSize      int64
	maxFileSize        int64
	ws                 *WSOptions
	sse                *SSEOptions
	rateLimit          *RateLimitOptions
	allowUnknownFields *bool
	captureBody        *bool
	versions           []Version
	versionsSet        bool
	skipValidation     bool
	deprecated         bool
	hidden             bool
	skipRateLimit      bool
}

// WithPrefix mounts a router under a path prefix.
//
// The prefix must begin with '/' and must not end with one, so that
// WithPrefix("/admin") combined with a route registered at "/" yields
// "/admin/" and one registered at "/reports" yields "/admin/reports". Prefixes
// nest: including a router that is itself included somewhere concatenates both
// prefixes.
func WithPrefix(prefix string) RouterOption {
	return routerOptionFunc(func(c *routerConfig) { c.prefix = prefix })
}

// WithTags adds OpenAPI tags to a router or a single route, which is what
// groups operations in the generated documentation.
//
// A router's tags are inherited by every route beneath it, and a route's own
// tags add to what it inherited rather than replacing it, so an operation
// appears under every group it names:
//
//	admin := muzak.NewRouter(muzak.WithTags("admin"))
//	admin.Post("/actions", act, muzak.WithTags("audit"))  // admin and audit
//
// A route under a router that declares no tags at all is grouped by its own
// tags alone. Duplicates are removed while first-seen order is preserved, and
// the groups themselves are described and ordered by [OpenAPIOptions.Tags].
func WithTags(tags ...string) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.tags = append(c.tags, tags...) },
		router: func(c *routerConfig) { c.tags = append(c.tags, tags...) },
	}
}

// WithVersion declares the version(s) a router or route answers, for an
// application with versioning enabled through [AppOptions.Versioning].
//
// Declaring it on a route overrides whatever an enclosing router declared,
// exactly as [Status] overrides a router's declared default; declaring it on
// a router applies to every route beneath it that does not declare its own.
// A route or router that never declares one falls back to
// [VersioningOptions.DefaultVersion], and if that is unset too, answers no
// request at all while versioning is enabled: an application has to opt a
// route into being unversioned deliberately, with [VersionNeutral], rather
// than by omission.
//
//	admin := muzak.NewRouter(muzak.WithVersion("1"))
//	admin.Get("/cats", findAllV1)
//	admin.Get("/cats", findAllV2, muzak.WithVersion("2"))
//	admin.Get("/health", health, muzak.WithVersion(muzak.VersionNeutral))
//
// Calling it with more than one version answers every one of them; calling
// it with no versions at all is a build error, since there would be nothing
// left for the declaration to mean. [VersionNeutral] cannot be combined with
// another version in the same call, because it already answers every
// request the narrower version would.
func WithVersion(versions ...Version) SharedOption {
	return sharedOption{
		route: func(c *routeConfig) {
			c.versions, c.versionsSet = versions, true
		},
		router: func(c *routerConfig) {
			c.versions, c.versionsSet = versions, true
		},
	}
}

// WithResponseDoc documents an additional response the route may produce,
// which is recorded in the OpenAPI document but has no effect at runtime. Use
// it for outcomes a handler produces through an error rather than through its
// return type, as in WithResponseDoc(418, "I'm a teapot"). Applying it to a
// router documents the response for every route beneath it.
//
// The body is described as the standard error envelope, [ErrorResponse], since
// that is what an error returned from a handler produces. A route that answers
// some status with a body of its own describes it with [WithResponseModel]
// instead. An empty description falls back to the status code's standard
// reason phrase, so WithResponseDoc(404, "") reads as "Not Found".
func WithResponseDoc(code int, description string) SharedOption {
	doc := responseDoc{code: code, description: description}
	return sharedOption{
		route:  func(c *routeConfig) { c.responses = append(c.responses, doc) },
		router: func(c *routerConfig) { c.responses = append(c.responses, doc) },
	}
}

// WithResponseModel documents an additional response and the model its body
// carries, so that one operation can describe a different schema per status
// code. Like [WithResponseDoc] it is recorded in the OpenAPI document and has
// no effect at runtime.
//
// The handler's return type describes the success response and nothing else,
// which leaves every other outcome undescribed unless it is declared here. The
// type argument is the response model, written exactly as the handler's own Out
// type would be:
//
//	r.Get("/items/{item_id}", handlers.ReadItem,
//		muzak.WithResponseModel[schemas.ItemError](http.StatusNotFound, "The item does not exist"),
//		muzak.WithResponseModel[schemas.ItemError](http.StatusGone, "The item was deleted"))
//
// The rules that apply to a handler's Out type apply here too: a named struct
// is referenced from the components section and described once however many
// operations mention it, [Empty] describes a response with no body at all, and
// [HTML] describes one carrying text/html.
//
// An empty description falls back to the status code's standard reason phrase.
// Applying it to a router documents the response for every route beneath it,
// and the last declaration of a status code wins, so a route may replace what
// it inherited, including the response derived from its own return type when it
// names the status the route succeeds with.
func WithResponseModel[T any](code int, description string) SharedOption {
	doc := responseDoc{code: code, description: description, model: reflect.TypeFor[T]()}
	return sharedOption{
		route:  func(c *routeConfig) { c.responses = append(c.responses, doc) },
		router: func(c *routerConfig) { c.responses = append(c.responses, doc) },
	}
}

// Status declares the status code written when the handler returns without an
// error. It defaults to 200, and 201 is the conventional choice for a creating
// route. A handler that must vary its status at runtime calls
// [Context.SetStatus] instead; the two are orthogonal, and the imperative call
// wins when both are used.
func Status(code int) RouteOption {
	return routeOptionFunc(func(c *routeConfig) { c.status = code })
}

// Summary sets the short, one-line description of the operation shown beside
// it in the generated documentation.
func Summary(summary string) RouteOption {
	return routeOptionFunc(func(c *routeConfig) { c.summary = summary })
}

// Description sets the long-form description of the operation. The generated
// OpenAPI document carries it verbatim, and CommonMark is rendered by the
// documentation UI.
func Description(description string) RouteOption {
	return routeOptionFunc(func(c *routeConfig) { c.description = description })
}

// OperationID sets the operation's unique identifier in the OpenAPI document,
// which client generators use to name the method they emit. When unset, Muzak
// derives one from the method and path. Identifiers must be unique across the
// application; a collision is reported when the application is built.
func OperationID(id string) RouteOption {
	return routeOptionFunc(func(c *routeConfig) { c.operationID = id })
}

// Deprecated marks a route, or every route beneath a router, as deprecated in
// the OpenAPI document. It changes nothing at runtime.
func Deprecated() SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.deprecated = true },
		router: func(c *routerConfig) { c.deprecated = true },
	}
}

// Hidden omits a route, or every route beneath a router, from the OpenAPI
// document and the documentation UI while leaving it fully routable. Use it
// for health checks and internal endpoints.
func Hidden() SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.hidden = true },
		router: func(c *routerConfig) { c.hidden = true },
	}
}

// MaxBodySize overrides the maximum accepted request body size, in bytes, for
// a route or for every route beneath a router. A request whose body exceeds
// the limit is rejected with 413 before the handler runs. The application-wide
// default comes from [AppOptions.MaxBodySize].
func MaxBodySize(bytes int64) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.maxBodySize = bytes },
		router: func(c *routerConfig) { c.maxBodySize = bytes },
	}
}

// MaxUploadSize overrides the maximum accepted size, in bytes, of a form body
// for a route or for every route beneath a router.
//
// It is what bounds a route that binds `form` or `file` fields, in place of
// [MaxBodySize], because an upload is expected to be larger than a JSON
// document and the two limits should not have to be traded off against each
// other. A body that exceeds it is rejected with 413 while it is being read,
// so the server never buffers more than the limit. The application-wide
// default comes from [AppOptions.MaxUploadSize]; a negative value removes an
// inherited limit, which is only appropriate behind a proxy that imposes its
// own.
func MaxUploadSize(bytes int64) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.maxUploadSize = bytes },
		router: func(c *routerConfig) { c.maxUploadSize = bytes },
	}
}

// MaxFileSize limits the size, in bytes, of any single uploaded file for a
// route or for every route beneath a router.
//
// A request carrying a file larger than the limit is rejected with 413 before
// the handler runs. The limit is checked once the body has been read, so it
// bounds what a handler is handed rather than what the server accepts;
// [MaxUploadSize] is what bounds the latter and should be set alongside it.
// The application-wide default comes from [AppOptions.MaxFileSize], and zero
// leaves each file bounded only by the upload limit.
func MaxFileSize(bytes int64) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.maxFileSize = bytes },
		router: func(c *routerConfig) { c.maxFileSize = bytes },
	}
}

// AllowUnknownFields relaxes JSON decoding so that members with no
// corresponding field are ignored rather than rejected.
//
// Muzak rejects unknown members by default, which turns a client's typo into
// an immediate 422 instead of a silently dropped value. Opt out only where
// forward compatibility with clients that send extra members matters more.
// Duplicate object members and invalid UTF-8 remain rejected regardless.
func AllowUnknownFields() SharedOption {
	yes := true
	return sharedOption{
		route:  func(c *routeConfig) { c.allowUnknownFields = &yes },
		router: func(c *routerConfig) { c.allowUnknownFields = &yes },
	}
}

// CaptureBody keeps the request body as it arrived, so that a guard, a provider
// or the handler can read it with [Context.RawBody].
//
// It exists for one problem, and it is a common one: a request whose
// authentication covers its bytes. A webhook signature is computed over exactly
// what was sent, and verifying a re-encoding verifies a different document --
// a decoder that reorders members, normalises a number or drops insignificant
// whitespace produces bytes the client never signed, and every signature fails.
// Stripe, GitHub, Slack and Apple's SKAdNetwork postbacks all sign the raw body.
//
// Without it the body is consumed: the binder reads it to decode the input, and
// a route declared with [Empty] drains it so the connection can be reused.
//
//	r.Post("/webhooks/stripe", handlers.Stripe,
//	    muzak.CaptureBody(),
//	    muzak.Needs(core.VerifyStripeSignature))
//
// Capture happens before dependencies resolve, so a guard sees the bytes and can
// reject an unsigned request before anything expensive runs. It happens after
// the rate limit is counted, so a client past its budget is refused without the
// server buffering anything on its behalf.
//
// The body is bounded by the route's own [MaxBodySize] and needs no second
// limit. One over it is refused with 413 here rather than truncated: a truncated
// body would fail its signature check, which reads as an attack rather than as
// the oversized request it is.
//
// It is a shared option, so a router of webhook receivers can declare it once.
// Do not reach for it application-wide. Buffering costs memory for every request
// in flight, and a route with nothing to verify has nothing to gain.
func CaptureBody() SharedOption {
	yes := true
	return sharedOption{
		route:  func(c *routeConfig) { c.captureBody = &yes },
		router: func(c *routerConfig) { c.captureBody = &yes },
	}
}

// SkipValidation stops a route from running its input model's Validate method.
//
// Validation is otherwise automatic: a model that declares rules has them
// applied, with no option to remember and so no way to leave a model
// unvalidated by forgetting one. Reach for this only where a route must accept
// input the model itself would reject, such as an administrative endpoint that
// repairs bad data.
func SkipValidation() RouteOption {
	return routeOptionFunc(func(c *routeConfig) { c.skipValidation = true })
}

// Route is a single registered operation: one HTTP method at one path
// template, with the handler, dependencies and documentation attached to it.
//
// A *Route is returned by the registration methods so that it can be inspected
// or referenced later. Its fields are filled in when the application is built
// and must be treated as read-only from that point on.
type Route struct {
	// Method is the uppercase HTTP method the route answers.
	Method string
	// Path is the full path template, including every prefix contributed by
	// the routers the route was included through.
	Path string
	// Summary is the one-line description shown in the documentation.
	Summary string
	// Description is the long-form description shown in the documentation.
	Description string
	// OperationID uniquely identifies the operation in the OpenAPI document.
	OperationID string
	// Tags group the operation in the documentation.
	Tags []string
	// Deprecated marks the operation as deprecated in the OpenAPI document.
	Deprecated bool
	// Hidden omits the operation from the OpenAPI document.
	Hidden bool
	// Status is the status code written when the handler succeeds without
	// calling [Context.SetStatus].
	Status int
	// Versions lists the versions this route answers, resolved from
	// [WithVersion] and [VersioningOptions.DefaultVersion]. It is empty for
	// an application that never enables versioning, and also, deliberately,
	// for a route that answers no request at all because versioning is
	// enabled but neither it nor anything it is declared under named a
	// version; see [WithVersion].
	Versions []Version

	guards             []Guard
	providers          []*provider
	responses          []responseDoc
	maxBodySize        int64
	maxUploadSize      int64
	maxFileSize        int64
	allowUnknownFields bool
	captureBody        bool
	skipValidation     bool

	// rateLimit is the route's resolved request policy, and is nil for a route
	// that is not rate limited. rateLimitOpts is the resolved configuration it
	// was built from, kept even when no quota was declared because a
	// WebSocket route's message limits are counted with the same storage and
	// tracker.
	rateLimit     *rateLimitConfig
	rateLimitOpts RateLimitOptions
	skipRateLimit bool

	inType  reflect.Type
	outType reflect.Type
	plan    *bindPlan
	invoke  func(*Context) error

	// websocket is non-nil for a route registered with [Router.WS], and holds
	// the configuration its connections are built with.
	websocket *wsConfig

	// sse is non-nil for a route registered with [Router.SSE], and holds the
	// configuration its event streams are built with.
	sse *sseConfig

	// cfg is the configuration declared directly on the route, retained until
	// the application is built and the inherited configuration is known.
	cfg      routeConfig
	rawPath  string
	registry *Router
}

// include records a child router and the options supplied at the point of
// inclusion.
type include struct {
	child *Router
	cfg   routerConfig
}

// Router groups related routes under a shared prefix, tag set and dependency
// chain.
//
// Routers are built independently and composed with [Router.Include]: a package
// exports a NewRouter function returning its own routes, and the application
// decides where to mount them and what guards apply.
//
//	func NewRouter() *muzak.Router {
//		r := muzak.NewRouter(muzak.WithTags("users"))
//		r.Get("/users/me", currentUser)
//		return r
//	}
//
// A Router is not safe for concurrent registration, which is not a limitation
// in practice: routes are declared during start-up from a single goroutine and
// only read afterwards.
type Router struct {
	cfg       routerConfig
	routes    []*Route
	frontends []*frontend
	includes  []include
	errs      []error
	mounted   bool
}

// NewRouter returns a router configured by the given options.
//
// Options that apply to a whole subtree, namely [WithTags],
// [WithDependencies], [Needs], [WithResponseDoc] and [WithResponseModel], take
// effect for every route registered on this router and on any router included
// into it.
func NewRouter(opts ...RouterOption) *Router {
	r := &Router{}
	for _, opt := range opts {
		opt.applyRouter(&r.cfg)
	}
	return r
}

// Include mounts a child router into this one, optionally adjusting it with
// options supplied at the point of inclusion.
//
// The child keeps its own configuration and inherits everything from the
// parent, with the include's options layered in between:
//
//	app.Include(admin.NewRouter(),
//		muzak.WithPrefix("/admin"),
//		muzak.WithTags("admin"),
//		muzak.WithDependencies(GetTokenHeader),
//		muzak.WithResponseDoc(418, "I'm a teapot"),
//	)
//
// Guards contributed by the parent run before those contributed here, which
// run before the child's own. A router may be included only once; including it
// twice is reported as an error when the application is built, because a route
// has a single resolved path.
func (r *Router) Include(child *Router, opts ...RouterOption) {
	if child == nil {
		r.errs = append(r.errs, errors.New("muzak: Include was given a nil router"))
		return
	}
	if child == r {
		r.errs = append(r.errs, errors.New("muzak: a router cannot include itself"))
		return
	}
	cfg := routerConfig{}
	for _, opt := range opts {
		opt.applyRouter(&cfg)
	}
	r.includes = append(r.includes, include{child: child, cfg: cfg})
}

// Routes returns the routes registered directly on this router, excluding
// those of any included router. Before the application is built the paths are
// the ones supplied at registration, without inherited prefixes.
func (r *Router) Routes() []*Route {
	return slices.Clone(r.routes)
}

// Get registers a handler for GET requests at the given path template.
//
// The input and output types are inferred from the handler literal, so the
// type arguments are never written at the call site:
//
//	r.Get("/users/{username}", func(ctx *muzak.Context, in Params) (UserOut, error) {
//		return UserOut{Username: in.Username}, nil
//	})
//
// The path is relative to whatever prefixes the router is eventually mounted
// under, and may contain "{name}" parameters and one trailing "{name...}"
// wildcard. Registration errors (an unbindable input type, a duplicate route,
// or a path parameter no field binds) are collected and reported when the
// application is built.
func (r *Router) Get[In, Out any](path string, h Handler[In, Out], opts ...RouteOption) *Route {
	return register(r, http.MethodGet, path, h, opts)
}

// Post registers a handler for POST requests at the given path template. It
// behaves exactly like [Router.Get] except for the method; see that method for
// how In and Out are inferred and bound.
func (r *Router) Post[In, Out any](path string, h Handler[In, Out], opts ...RouteOption) *Route {
	return register(r, http.MethodPost, path, h, opts)
}

// Put registers a handler for PUT requests at the given path template. It
// behaves exactly like [Router.Get] except for the method.
func (r *Router) Put[In, Out any](path string, h Handler[In, Out], opts ...RouteOption) *Route {
	return register(r, http.MethodPut, path, h, opts)
}

// Patch registers a handler for PATCH requests at the given path template. It
// behaves exactly like [Router.Get] except for the method.
func (r *Router) Patch[In, Out any](path string, h Handler[In, Out], opts ...RouteOption) *Route {
	return register(r, http.MethodPatch, path, h, opts)
}

// Delete registers a handler for DELETE requests at the given path template.
// It behaves exactly like [Router.Get] except for the method.
func (r *Router) Delete[In, Out any](path string, h Handler[In, Out], opts ...RouteOption) *Route {
	return register(r, http.MethodDelete, path, h, opts)
}

// Head registers a handler for HEAD requests at the given path template.
// Registering one is rarely necessary: a GET route answers HEAD automatically,
// running the handler and discarding the body, and an explicit HEAD route
// takes precedence over that.
func (r *Router) Head[In, Out any](path string, h Handler[In, Out], opts ...RouteOption) *Route {
	return register(r, http.MethodHead, path, h, opts)
}

// Handle registers a handler for an arbitrary HTTP method, for the methods the
// named helpers do not cover. The method is upper-cased before use.
//
// It is also how an OPTIONS route is registered. There is no Router.Options
// method, because [App.Options] applies configuration to an application and
// the two would shadow each other on an App; OPTIONS is answered automatically
// with an Allow header in any case, so registering one is only necessary to
// replace that behaviour:
//
//	r.Handle(http.MethodOptions, "/things", describeThings)
func (r *Router) Handle[In, Out any](method, path string, h Handler[In, Out], opts ...RouteOption) *Route {
	return register(r, strings.ToUpper(method), path, h, opts)
}

// register builds the Route value shared by every registration method. It is a
// free function rather than a method because it needs the same type parameters
// the caller inferred, and keeping it separate lets every method above be a
// one-line delegation.
//
// A route is recorded only once it is fully formed. A registration that fails
// its checks reports the reason and returns without joining the router's route
// list, so nothing downstream has to cope with a half-built Route.
func register[In, Out any](r *Router, method, path string, h Handler[In, Out], opts []RouteOption) *Route {
	rt := &Route{
		Method:   method,
		rawPath:  path,
		inType:   reflect.TypeFor[In](),
		outType:  reflect.TypeFor[Out](),
		registry: r,
	}
	for _, opt := range opts {
		opt.applyRoute(&rt.cfg)
	}
	if h == nil {
		r.errs = append(r.errs, fmt.Errorf("muzak: %s %s: handler is nil", method, path))
		return rt
	}
	if !strings.HasPrefix(path, "/") {
		r.errs = append(r.errs, fmt.Errorf("muzak: %s %s: path must begin with %q", method, path, "/"))
		return rt
	}

	// The plan pointer is read through rt at request time, so the closure
	// works even though the plan is compiled later, once the full path is
	// known.
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
		out, err := h(c, in)
		if err != nil {
			return err
		}
		return c.writeResponse(out)
	}
	r.routes = append(r.routes, rt)
	return rt
}

// inherited is the configuration a router passes down to everything it
// contains.
type inherited struct {
	prefix                string
	tags                  []string
	guards                []Guard
	providers             []*provider
	responses             []responseDoc
	maxBodySize           int64
	maxUploadSize         int64
	maxFileSize           int64
	ws                    WSOptions
	wsMaxConnections      int
	wsMaxConnectionsPerIP int
	sse                   SSEOptions
	sseMaxStreams         int
	sseMaxStreamsPerIP    int
	rateLimit             RateLimitOptions
	allowUnknownFields    bool
	captureBody           bool
	versions              []Version
	deprecated            bool
	hidden                bool
	skipRateLimit         bool
}

// merge layers a router's own configuration on top of what it inherited,
// returning the result without mutating either input.
func (in inherited) merge(cfg routerConfig) inherited {
	out := inherited{
		prefix:                in.prefix + cfg.prefix,
		tags:                  concat(in.tags, cfg.tags),
		guards:                concat(in.guards, cfg.guards),
		providers:             concat(in.providers, cfg.providers),
		responses:             concat(in.responses, cfg.responses),
		maxBodySize:           in.maxBodySize,
		maxUploadSize:         in.maxUploadSize,
		maxFileSize:           in.maxFileSize,
		ws:                    in.ws,
		wsMaxConnections:      in.wsMaxConnections,
		wsMaxConnectionsPerIP: in.wsMaxConnectionsPerIP,
		sse:                   in.sse,
		sseMaxStreams:         in.sseMaxStreams,
		sseMaxStreamsPerIP:    in.sseMaxStreamsPerIP,
		rateLimit:             in.rateLimit,
		allowUnknownFields:    in.allowUnknownFields,
		captureBody:           in.captureBody,
		versions:              in.versions,
		deprecated:            in.deprecated || cfg.deprecated,
		hidden:                in.hidden || cfg.hidden,
		skipRateLimit:         in.skipRateLimit || cfg.skipRateLimit,
	}
	if cfg.maxBodySize > 0 {
		out.maxBodySize = cfg.maxBodySize
	}
	if cfg.maxUploadSize != 0 {
		out.maxUploadSize = cfg.maxUploadSize
	}
	if cfg.maxFileSize != 0 {
		out.maxFileSize = cfg.maxFileSize
	}
	if cfg.ws != nil {
		out.ws = out.ws.overlay(*cfg.ws)
	}
	if cfg.sse != nil {
		out.sse = out.sse.overlay(*cfg.sse)
	}
	if cfg.rateLimit != nil {
		out.rateLimit = out.rateLimit.overlay(*cfg.rateLimit)
	}
	if cfg.captureBody != nil {
		out.captureBody = *cfg.captureBody
	}
	if cfg.allowUnknownFields != nil {
		out.allowUnknownFields = *cfg.allowUnknownFields
	}
	if cfg.versionsSet {
		out.versions = cfg.versions
	}
	return out
}

// concat returns a fresh slice holding a followed by b, so that sibling
// routers can never share and then extend the same backing array.
func concat[T any](a, b []T) []T {
	if len(b) == 0 {
		return a
	}
	out := make([]T, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// finalize resolves this router's subtree against the configuration it
// inherits, filling in each route and handing it to emit. Errors are collected
// rather than returned one at a time so that a misconfigured application
// reports every problem in a single pass.
func (r *Router) finalize(in inherited, emit func(*Route) error, state *buildState) {
	if r.mounted {
		state.errs = append(state.errs, errors.New("muzak: a router was included more than once; build a separate router for each mount point"))
		return
	}
	r.mounted = true
	state.errs = append(state.errs, r.errs...)
	state.lifecycles = append(state.lifecycles, r.cfg.lifecycles...)

	cur := in.merge(r.cfg)
	if err := validatePrefix(cur.prefix); err != nil {
		state.errs = append(state.errs, err)
		return
	}

	for _, rt := range r.routes {
		if err := rt.resolve(cur); err != nil {
			state.errs = append(state.errs, err)
			continue
		}
		if err := emit(rt); err != nil {
			state.errs = append(state.errs, err)
		}
	}
	for _, mount := range r.frontends {
		if err := mount.resolve(cur); err != nil {
			state.errs = append(state.errs, err)
			continue
		}
		state.frontends = append(state.frontends, mount)
	}
	for _, inc := range r.includes {
		state.lifecycles = append(state.lifecycles, inc.cfg.lifecycles...)
		inc.child.finalize(cur.merge(inc.cfg), emit, state)
	}
}

// resolve computes a route's final path, documentation and dependency chain,
// and compiles its binding plan.
func (rt *Route) resolve(in inherited) error {
	cfg := rt.cfg
	rt.Path = in.prefix + rt.rawPath
	rt.Tags = dedupeStrings(concat(in.tags, cfg.tags))
	rt.guards = concat(in.guards, cfg.guards)
	rt.providers = concat(in.providers, cfg.providers)
	rt.responses = concat(in.responses, cfg.responses)
	rt.Summary = cfg.summary
	rt.Description = cfg.description
	rt.Deprecated = in.deprecated || cfg.deprecated
	rt.Hidden = in.hidden || cfg.hidden

	rt.Status = cfg.status
	if rt.Status == 0 {
		rt.Status = http.StatusOK
	}
	if rt.websocket != nil {
		rt.Status = http.StatusSwitchingProtocols
	}
	if rt.Status != clampStatus(rt.Status) {
		return fmt.Errorf("muzak: %s %s: declared status %d is not a valid HTTP status code", rt.Method, rt.Path, cfg.status)
	}
	for _, doc := range rt.responses {
		// A documented outcome becomes a key in the OpenAPI document, so a
		// code outside the range is caught here rather than emitted as a
		// response no client could ever receive.
		if doc.code != clampStatus(doc.code) {
			return fmt.Errorf("muzak: %s %s: documented response status %d is not a valid HTTP status code", rt.Method, rt.Path, doc.code)
		}
	}

	rt.maxBodySize = in.maxBodySize
	if cfg.maxBodySize > 0 {
		rt.maxBodySize = cfg.maxBodySize
	}
	rt.maxUploadSize = in.maxUploadSize
	if cfg.maxUploadSize != 0 {
		rt.maxUploadSize = cfg.maxUploadSize
	}
	rt.maxFileSize = in.maxFileSize
	if cfg.maxFileSize != 0 {
		rt.maxFileSize = cfg.maxFileSize
	}
	rt.captureBody = in.captureBody
	if cfg.captureBody != nil {
		rt.captureBody = *cfg.captureBody
	}
	rt.allowUnknownFields = in.allowUnknownFields
	if cfg.allowUnknownFields != nil {
		rt.allowUnknownFields = *cfg.allowUnknownFields
	}
	rt.skipValidation = cfg.skipValidation

	rt.Versions = in.versions
	if cfg.versionsSet {
		rt.Versions = cfg.versions
	}
	if cfg.versionsSet && len(cfg.versions) == 0 {
		return fmt.Errorf("muzak: %s %s: WithVersion was called with no versions", rt.Method, rt.Path)
	}
	if err := validateVersionList(rt.Versions, fmt.Sprintf("%s %s", rt.Method, rt.Path)); err != nil {
		return err
	}

	// Rate limiting is resolved before the protocol branches below, because a
	// WebSocket route's message limits are built from the same policy.
	if err := rt.resolveRateLimit(in); err != nil {
		return err
	}

	rt.OperationID = cfg.operationID
	if rt.OperationID == "" {
		rt.OperationID = deriveOperationID(rt.Method, rt.Path)
		// A header, media type or custom versioning scheme can register more
		// than one route at this exact method and path, distinguished only
		// by the version each answers rather than by the path itself, which
		// would otherwise derive the identical ID for every one of them.
		// [VersioningURI] does not need this: its own expansion re-derives
		// the ID from each version's own, already-distinct path.
		if len(rt.Versions) > 0 && !rt.isVersionNeutral() {
			rt.OperationID += "_" + versionSuffix(rt.Versions)
		}
	}

	plan, err := newBindPlan(rt.inType, rt.Method, rt.Path)
	if err != nil {
		return err
	}
	rt.plan = plan
	if rt.websocket != nil {
		return rt.resolveWebSocket(in)
	}
	if rt.sse != nil {
		return rt.resolveSSE(in)
	}
	return nil
}

// answersHead reports whether a GET route can also answer a HEAD request,
// which is true of every route whose response is a body rather than a
// conversation. A WebSocket cannot be upgraded from a HEAD, and an event
// stream answering one would run its handler with every write discarded, so
// neither is offered.
func (rt *Route) answersHead() bool {
	return rt.websocket == nil && rt.sse == nil
}

// validatePrefix rejects a prefix that would produce a malformed path.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("muzak: route prefix %q must begin with %q", prefix, "/")
	}
	if strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("muzak: route prefix %q must not end with %q, because route paths already begin with one", prefix, "/")
	}
	return nil
}

// dedupeStrings removes repeated entries while preserving first-seen order,
// which keeps inherited and locally declared tags from doubling up.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// deriveOperationID builds a stable identifier from a route's method and path,
// used when the developer does not supply one with [OperationID].
func deriveOperationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('_')
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			b.WriteString("by_")
			b.WriteString(sanitizeIdent(strings.TrimSuffix(seg[1:len(seg)-1], "...")))
			continue
		}
		b.WriteString(sanitizeIdent(seg))
	}
	return b.String()
}

// sanitizeIdent reduces a path segment to characters safe in a generated
// client's method name.
func sanitizeIdent(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, s)
}
