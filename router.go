package muzak

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
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
	category           string
	categorySet        bool
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
	security           []SecurityRequirement
	securitySet        bool
	// timeout is what [Timeout] declared; see [Route.resolveTimeout].
	timeout time.Duration
}

// routeConfig accumulates the settings declared on a single route.
type routeConfig struct {
	tags               []string
	category           string
	categorySet        bool
	guards             []Guard
	providers          []*provider
	responses          []responseDoc
	status             int
	summary            string
	title              string
	titleSet           bool
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
	security           []SecurityRequirement
	securitySet        bool
	// timeout is what [Timeout] declared; see [Route.resolveTimeout].
	timeout time.Duration
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

// Title gives the operation a short human name, such as "Fetch User Profile",
// which a documentation tool shows in its navigation in place of the endpoint's
// path. It is a label for the sidebar and nothing else: the one-line
// [Summary] and the long [Description] still say what the operation does, the
// path is still what a client requests, and a route with no title is listed by
// its path as before.
//
// The title is written into the OpenAPI document as the operation's "x-title"
// extension. It is checked when the application is built: after surrounding
// whitespace is trimmed it must not be empty, must be valid UTF-8 of at most
// 120 characters, and must hold no control character, line break or
// bidirectional control, since the name is displayed as a single line. Titles
// are not required to be unique.
func Title(title string) RouteOption {
	return routeOptionFunc(func(c *routeConfig) { c.title, c.titleSet = title, true })
}

// WithCategory files a router, or a single route, under a category: the one
// heading a documentation tool lists it beneath in its navigation, such as
// "Billing". With a hundred routers the list of tags is too long to read, and a
// category is the coarser grouping laid over it.
//
//	billing := muzak.NewRouter(muzak.WithCategory("Billing"))
//	billing.Get("/invoices", listInvoices, muzak.Title("List Invoices"))
//	billing.Get("/invoices/export", export, muzak.WithCategory("Reports"))
//
// A category is not a tag, and the two do not interact: tags keep grouping
// operations exactly as [WithTags] describes, and an operation may carry both.
// Unlike tags, which add up, a category is a single value. Every route beneath
// a router inherits its category, several routers may share one, and a router
// included into another, or a route itself, replaces the category it would
// have inherited. A route no category reaches has none, and is listed as it
// always was.
//
// The category is written into the OpenAPI document as the operation's
// "x-category" extension. The document also lists every category some
// operation carries once, as the top-level "x-categories" array, in the order
// the categories were first registered: a route counts where it was declared,
// a router included into another counts where the Include call was made, and a
// category first met on a route the document leaves out, such as a [Hidden]
// one, is not listed. That order is what a documentation tool should present
// them in, since the document's paths are sorted and say nothing of it. It is
// checked when the application is built: after
// surrounding whitespace is trimmed it must not be empty, must be valid UTF-8
// of at most 64 characters, and must hold no control character, line break or
// bidirectional control. Two spellings that differ in case are two categories.
func WithCategory(name string) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.category, c.categorySet = name, true },
		router: func(c *routerConfig) { c.category, c.categorySet = name, true },
	}
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

// MaxUploadSize overrides the maximum accepted size, in bytes, of a multipart
// body for a route or for every route beneath a router.
//
// It is what bounds a multipart body sent to a route that binds `file`
// fields, in place of [MaxBodySize], because an upload is expected to be
// larger than a JSON document and the two limits should not have to be traded
// off against each other. A route that binds only `form` values receives
// nothing but text, in either encoding, and is held in memory whole, so it
// stays bounded by [MaxBodySize]. A body that exceeds the limit is rejected
// with 413 while it is being read, so the server never buffers more than the
// limit. The application-wide
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
//
// A member naming a path, query, header or cookie field is an unknown member
// too, because a located field is never read from the body; under this option
// it is ignored rather than refused.
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
	// Title is the human name the documentation lists the operation by, as
	// set with [Title]. It is empty when none was set.
	Title string
	// Category is the heading the documentation lists the operation under,
	// resolved from [WithCategory] on the route and the routers above it. It
	// is empty when none applies.
	Category string
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
	// security and securitySet are what the document says the route needs to
	// authenticate, resolved from [WithSecurity] and [Public]. They describe
	// and are never read while a request is served.
	security    []SecurityRequirement
	securitySet bool

	// rateLimit is the route's resolved request policy, and is nil for a route
	// that is not rate limited. rateLimitOpts is the resolved configuration it
	// was built from, kept even when no quota was declared because a
	// WebSocket route's message limits are counted with the same storage and
	// tracker.
	rateLimit     *rateLimitConfig
	rateLimitOpts RateLimitOptions
	skipRateLimit bool

	// timeout is the deadline [Timeout] gives the route, and zero for none.
	timeout time.Duration

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
	seq      uint64
	rawPath  string
	registry *Router
}

// include records a child router and the options supplied at the point of
// inclusion.
type include struct {
	child *Router
	cfg   routerConfig
	// seq places the inclusion among the routes registered on the same router;
	// see [registrations].
	seq uint64
}

// registrations numbers every route registered and every router included, in
// the order it happened. A router keeps its routes and its includes in lists
// of their own, so this is what lets the two be put back in the order they
// were declared in, which decides the order the documentation lists categories
// in. The count is only ever compared, so it does not matter that routers built
// by different goroutines interleave in it.
var registrations atomic.Uint64

// addRoute records a registered route on the router, numbering it.
func (r *Router) addRoute(rt *Route) {
	rt.seq = registrations.Add(1)
	r.routes = append(r.routes, rt)
}

// registrationOrder ranks every route beneath the router by where it was
// registered, keyed by the number [Router.addRoute] gave it. A route registered
// on a router counts where it was declared, and a router included into another
// counts where the Include call was made, so the routes of one included between
// two of its parent's are ranked between them.
func (r *Router) registrationOrder() map[uint64]int {
	ranks := make(map[uint64]int)
	var walk func(*Router)
	walk = func(r *Router) {
		type step struct {
			seq   uint64
			route *Route
			child *Router
		}
		steps := make([]step, 0, len(r.routes)+len(r.includes))
		for _, rt := range r.routes {
			steps = append(steps, step{seq: rt.seq, route: rt})
		}
		for _, inc := range r.includes {
			steps = append(steps, step{seq: inc.seq, child: inc.child})
		}
		slices.SortFunc(steps, func(a, b step) int { return cmp.Compare(a.seq, b.seq) })
		for _, s := range steps {
			if s.route != nil {
				ranks[s.route.seq] = len(ranks)
				continue
			}
			walk(s.child)
		}
	}
	walk(r)
	return ranks
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
	// mounted is set when the router is resolved into an application being
	// built. From then on its configuration has been read for good, which is
	// why a further registration is refused rather than quietly ignored.
	mounted bool
}

// mustBeOpen panics when the router has already been built into an
// application, naming the call that came too late.
//
// Building reads the routing tree, the guards and the middleware once, and a
// call made afterwards used to be accepted and then silently had no effect.
// That fails open: a guard or a deny-all middleware added after something
// built the application implicitly, [App.Document] writing the OpenAPI file
// at start-up for instance, protected nothing while the code read as though
// it did. A panic is the loud failure that mistake needs, since a call like
// this is made once during start-up, where a panic is seen at once.
func (r *Router) mustBeOpen(call string) {
	if r.mounted {
		panic("muzak: " + call + " was called after the application was built, where it would have no effect; " +
			"make every configuration call before the first call to Build, ServeHTTP, Document or a run method")
	}
}

// NewRouter returns a router configured by the given options.
//
// Options that apply to a whole subtree, namely [WithTags], [WithCategory],
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
// has a single resolved path. Include panics once the application the router
// belongs to has been built, as registering a route does.
func (r *Router) Include(child *Router, opts ...RouterOption) {
	r.mustBeOpen("Include")
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
	r.includes = append(r.includes, include{child: child, cfg: cfg, seq: registrations.Add(1)})
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
// wildcard. A parameter is percent-decoded once before it is bound, and a
// segment that arrives as "%2F" stays inside the segment it belongs to when
// the route is matched, so a wildcard's value can contain "/" and "..", and a
// value can contain a NUL byte. Treat one as text a client wrote: check it
// before using it as a file name, as [Router.Frontend] and [Router.Static] do
// for the paths they serve.
//
// A wildcard also receives spellings of the routes beside it, because a path
// is matched as it arrives and nothing normalises it first. With a guarded
// "/admin/panel" and a public "/{rest...}", every one of "//admin/panel",
// "/admin//panel", "/admin/panel/", "/ADMIN/panel", "/./admin/panel",
// "/x/../admin/panel", "/%2e/admin/panel" and "/admin%2Fpanel" reaches the
// public wildcard, the last with rest set to "admin/panel". The guarded route
// itself is never reached without its guard, and net/http.ServeMux behaves
// the same way; the danger is in what the wildcard's handler does with the
// value. One that serves a file, looks up a record or calls another service
// by it must check the caller is allowed what the value refers to, not rely
// on the guards of a route the request did not match, and must not forward
// the value to a backend that cleans, decodes or case-folds paths, which may
// resolve it to the very resource the guard was protecting.
//
// Registration errors (an unbindable input type, a duplicate route,
// or a path parameter no field binds) are collected and reported when the
// application is built. Registering a route on a router whose application has
// already been built panics, because the route would never be served.
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
// named helpers do not cover. The method is upper-cased before use. The path
// template is the same as [Router.Get]'s, and a "{name...}" wildcard in it
// receives the same unnormalised spellings of its neighbours; see there.
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
	r.mustBeOpen("registering " + method + " " + path)
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
			// The body is left for the handler, which may want the raw bytes
			// of a route that binds nothing from it, and what it did not read
			// is dropped once it has returned.
			defer discardBody(c.r)
		}
		out, err := h(c, in)
		if err != nil {
			return err
		}
		return c.writeResponse(out)
	}
	r.addRoute(rt)
	return rt
}

// inherited is the configuration a router passes down to everything it
// contains.
type inherited struct {
	prefix                string
	tags                  []string
	category              string
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
	security              []SecurityRequirement
	securitySet           bool
	timeout               time.Duration
}

// merge layers a router's own configuration on top of what it inherited,
// returning the result without mutating either input.
func (in inherited) merge(cfg routerConfig) inherited {
	out := inherited{
		prefix:                in.prefix + cfg.prefix,
		tags:                  concat(in.tags, cfg.tags),
		category:              in.category,
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
		security:              in.security,
		securitySet:           in.securitySet,
		timeout:               in.timeout,
	}
	if cfg.timeout != 0 {
		out.timeout = cfg.timeout
	}
	if cfg.securitySet {
		out.security, out.securitySet = cfg.security, true
	}
	if cfg.categorySet {
		out.category = strings.TrimSpace(cfg.category)
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
	if r.cfg.categorySet {
		if err := checkLabel("WithCategory", r.cfg.category, maxCategoryLen); err != nil {
			state.errs = append(state.errs, fmt.Errorf("muzak: %s: %w", describeRouter(cur.prefix, r), err))
		}
	}
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
		merged := cur.merge(inc.cfg)
		if inc.cfg.categorySet {
			if err := checkLabel("WithCategory", inc.cfg.category, maxCategoryLen); err != nil {
				state.errs = append(state.errs, fmt.Errorf("muzak: %s: %w", describeRouter(merged.prefix, inc.child), err))
			}
		}
		inc.child.finalize(merged, emit, state)
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
	rt.Category = in.category
	if cfg.categorySet {
		rt.Category = strings.TrimSpace(cfg.category)
		if err := checkLabel("WithCategory", cfg.category, maxCategoryLen); err != nil {
			return fmt.Errorf("muzak: %s %s: %w", rt.Method, rt.Path, err)
		}
	}
	if cfg.titleSet {
		rt.Title = strings.TrimSpace(cfg.title)
		if err := checkLabel("Title", cfg.title, maxTitleLen); err != nil {
			return fmt.Errorf("muzak: %s %s: %w", rt.Method, rt.Path, err)
		}
	}
	rt.Deprecated = in.deprecated || cfg.deprecated
	rt.Hidden = in.hidden || cfg.hidden
	rt.security, rt.securitySet = in.security, in.securitySet
	if cfg.securitySet {
		rt.security, rt.securitySet = cfg.security, true
	}

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
	if err := rt.resolveTimeout(in); err != nil {
		return err
	}
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

// The longest a category and a title may be, in characters. A category is a
// heading in a navigation tree and a title a label in it, so a value past these
// is a sentence that was meant for [Summary] or [Description], and would only
// wrap or be cut off wherever it is shown.
const (
	maxCategoryLen = 64
	maxTitleLen    = 120
)

// checkLabel reports why a category or a title is not one a documentation tool
// can list, naming the option it was given to. The value is judged as it was
// written: whitespace at either end is trimmed before use, but a line break
// there is still a mistake worth reporting rather than absorbing.
//
// A label is a single line of text that is displayed as itself. A control
// character or line break would split it or, in a terminal or a log, forge a
// line, and a bidirectional control reorders what is drawn around it, so none
// is accepted.
func checkLabel(option, value string, limit int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s was given a value that is not valid UTF-8", option)
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Bidi_Control, r) {
			return fmt.Errorf("%s was given %q, which holds a control character or line break (U+%04X); a label is a single line of text",
				option, value, r)
		}
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s was given a name that is empty once whitespace is trimmed; give it a name or leave the option out", option)
	}
	if n := utf8.RuneCountInString(trimmed); n > limit {
		return fmt.Errorf("%s was given a value of %d characters, over the limit of %d", option, n, limit)
	}
	return nil
}

// describeRouter names a router in an error, given the prefix it resolves
// under. A router has no name of its own, so it is identified by where it is
// mounted, or by its first route when it is mounted at the root.
func describeRouter(prefix string, r *Router) string {
	switch {
	case prefix != "":
		return fmt.Sprintf("the router mounted at %q", prefix)
	case len(r.routes) > 0:
		return fmt.Sprintf("the router holding %s %s", r.routes[0].Method, r.routes[0].rawPath)
	}
	return "a router with no prefix and no routes"
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
