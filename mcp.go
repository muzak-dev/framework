package muzak

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// This file is the Model Context Protocol endpoint's surface: the options, the
// registration and the build step that turns selected routes into tools. The
// transport is in mcp_transport.go, JSON-RPC in mcp_jsonrpc.go, sessions in
// mcp_session.go, schemas in mcp_schema.go and calling a tool in mcp_call.go.

// The protocol revisions the MCP endpoint speaks.
//
// 2026-07-28 is stateless: every request carries its version in its _meta and
// in the MCP-Protocol-Version header, and there is no handshake and no
// session. The three before it open a session with initialize and are
// answered for as long as clients speak them, which is what a dual-era server
// is allowed to do.
const (
	mcpVersion20250326 = "2025-03-26"
	mcpVersion20250618 = "2025-06-18"
	mcpVersion20251125 = "2025-11-25"
	mcpVersion20260728 = "2026-07-28"
)

// mcpVersions lists every revision the endpoint supports, newest first, which
// is the order an UnsupportedProtocolVersionError reports them in.
var mcpVersions = []string{mcpVersion20260728, mcpVersion20251125, mcpVersion20250618, mcpVersion20250326}

// mcpEra groups the revisions by what a tool listing and a tool result may
// carry.
type mcpEra uint8

const (
	// eraBasic is 2025-03-26, which has no structured content, no output
	// schema and no tool title.
	eraBasic mcpEra = iota
	// eraStructured is 2025-06-18 and 2025-11-25, whose structured content
	// and output schema are objects.
	eraStructured
	// eraStateless is 2026-07-28, whose structured content may be any JSON
	// value and whose results name their type and the server.
	eraStateless
	mcpEras
)

// eraOf returns the era of a revision a session negotiated. A stateless
// request is of [eraStateless] whatever it names.
func eraOf(version string) mcpEra {
	if version == mcpVersion20250326 {
		return eraBasic
	}
	return eraStructured
}

// Headers of the Streamable HTTP transport, as an application lists them in
// [CORSOptions] for a browser-based MCP client: AllowHeaders for both, and
// ExposeHeaders for the session identifier, which a browser otherwise hides
// from the page.
const (
	// HeaderMCPSessionID carries the session a legacy client opened with
	// initialize.
	HeaderMCPSessionID = "Mcp-Session-Id"
	// HeaderMCPProtocolVersion carries the protocol revision of a request.
	HeaderMCPProtocolVersion = "MCP-Protocol-Version"
)

// Defaults and bounds of [MCPOptions].
const (
	// DefaultMCPMaxResultSize is the largest response a route may answer a
	// tool call with, at one mebibyte.
	DefaultMCPMaxResultSize int64 = 1 << 20
	// MaxMCPResultSize is the largest MaxResultSize accepted, at 64 MiB. A
	// result is held in memory whole and sent twice in a structured answer.
	MaxMCPResultSize int64 = 64 << 20
	// DefaultMCPPageSize is how many tools one page of tools/list holds.
	DefaultMCPPageSize = 100
	// MaxMCPPageSize is the largest PageSize accepted.
	MaxMCPPageSize = 1000
	// DefaultMCPMaxSessions is how many sessions are held at once.
	DefaultMCPMaxSessions = 10_000
	// MaxMCPSessions is the largest MaxSessions accepted.
	MaxMCPSessions = 1_000_000
	// DefaultMCPSessionIdleTimeout is how long a session that sends nothing
	// is kept.
	DefaultMCPSessionIdleTimeout = 24 * time.Hour
	// DefaultMCPListTTL is how long a 2026-07-28 client may cache the tool
	// list and the discovery result.
	DefaultMCPListTTL = 5 * time.Minute
)

// MCPOptions configures the Model Context Protocol endpoint [App.MCP] serves.
//
// Nothing is a tool unless it is chosen. A route is one when it is declared
// with [MCPTool], when it carries one of Tags, or when Include returns true
// for it; the three add up. Whatever is chosen, a route that cannot be called
// as a tool never becomes one: a WebSocket or event stream route, a [Hidden]
// one, the endpoint itself, and a route whose method OpenAPI has no place for.
// [Router.Mount], [Router.Static], [Router.Frontend], the documentation and
// the health endpoints are not routes, and are never tools either.
type MCPOptions struct {
	// Tags chooses every route carrying one of these tags, the ones
	// [WithTags] gives it or its routers, as a tool.
	Tags []string

	// Include chooses routes by any rule an application has. It is called
	// once for each route that can be a tool, when the application is built,
	// and returning true makes the route one. It cannot unchoose a route that
	// MCPTool or Tags chose, so a rule with exceptions belongs here whole.
	Include func(*Route) bool

	// Instructions is sent to clients in the initialize and server/discover
	// results, as guidance a model reads about how to use the tools.
	Instructions string

	// AllowedOrigins lists the browser origins, such as
	// "https://inspector.example.com", that may call the endpoint besides
	// its own. Only a browser sends an Origin, and a request without one is
	// not affected.
	//
	// The endpoint's own origin is an Origin whose host is the request's Host,
	// and it counts only when that Host is one the application vouches for: a
	// name listed in [AppOptions.AllowedHosts], which refuses every other Host
	// before this runs, or localhost or an address, which a page served from
	// somebody else's domain cannot present. That is the defence the MCP
	// specification requires against DNS rebinding, where a page from the
	// attacker's name, made to resolve to this server, sends that name as
	// both Origin and Host. Any other Origin is refused with 403.
	//
	// An entry is held to the rules [CORSOptions.AllowedOrigins] is. "*" and
	// "null" are build errors: a page anywhere would be let in. A browser
	// client also needs CORS, configured in [AppOptions.CORS].
	AllowedOrigins []string

	// AllowOriginFunc decides an origin AllowedOrigins and the same-origin
	// rule did not allow. It runs on every request carrying one, so it must be
	// cheap.
	AllowOriginFunc func(r *http.Request, origin string) bool

	// ForwardHeaders names request headers, besides Authorization, that carry
	// credentials a tool's route reads, such as "X-API-Key". Each is copied
	// from the MCP request to every route a tool call reaches, unchanged, as
	// Authorization is. Nothing else the MCP request carries is, so a route
	// whose input binds one of these, or Authorization, cannot be a tool: its
	// value would compete with the forwarded one.
	ForwardHeaders []string

	// ForwardCookies names cookies, such as a session cookie, copied from the
	// MCP request to every route a tool call reaches. None is by default.
	ForwardCookies []string

	// MaxResultSize bounds the body a route may answer a tool call with, in
	// bytes. A larger one ends the call with an error result and cancels the
	// route's context, so a stream stops. It defaults to
	// [DefaultMCPMaxResultSize] and may not exceed [MaxMCPResultSize].
	MaxResultSize int64

	// PageSize is how many tools one tools/list page holds, defaulting to
	// [DefaultMCPPageSize] and at most [MaxMCPPageSize].
	PageSize int

	// MaxSessions bounds the sessions legacy clients hold open at once,
	// defaulting to [DefaultMCPMaxSessions]. A session that would go past it
	// evicts the one that has been idle longest, whose client is told 404 and
	// starts a new one, as the protocol has it do.
	MaxSessions int

	// SessionIdleTimeout is how long a session is kept without a request,
	// defaulting to [DefaultMCPSessionIdleTimeout].
	SessionIdleTimeout time.Duration

	// ListTTL is how long a 2026-07-28 client may consider the tool list
	// fresh, defaulting to [DefaultMCPListTTL]. The list is fixed when the
	// application is built, so this is about how soon a client sees a
	// deployment's new tools. Negative means zero.
	ListTTL time.Duration
}

// withDefaults fills in the unset bounds.
func (o MCPOptions) withDefaults() MCPOptions {
	if o.MaxResultSize == 0 {
		o.MaxResultSize = DefaultMCPMaxResultSize
	}
	if o.PageSize == 0 {
		o.PageSize = DefaultMCPPageSize
	}
	if o.MaxSessions == 0 {
		o.MaxSessions = DefaultMCPMaxSessions
	}
	if o.SessionIdleTimeout == 0 {
		o.SessionIdleTimeout = DefaultMCPSessionIdleTimeout
	}
	if o.ListTTL == 0 {
		o.ListTTL = DefaultMCPListTTL
	}
	if o.ListTTL < 0 {
		o.ListTTL = 0
	}
	return o
}

// validate reports every option that cannot be served, joined.
func (o MCPOptions) validate() []error {
	var errs []error
	if o.MaxResultSize < 0 || o.MaxResultSize > MaxMCPResultSize {
		errs = append(errs, fmt.Errorf("muzak: MCPOptions.MaxResultSize is %d, but it must be between 1 and %d bytes; "+
			"leave it at zero for the %d byte default", o.MaxResultSize, MaxMCPResultSize, DefaultMCPMaxResultSize))
	}
	if o.PageSize < 0 || o.PageSize > MaxMCPPageSize {
		errs = append(errs, fmt.Errorf("muzak: MCPOptions.PageSize is %d, but it must be between 1 and %d",
			o.PageSize, MaxMCPPageSize))
	}
	if o.MaxSessions < 0 || o.MaxSessions > MaxMCPSessions {
		errs = append(errs, fmt.Errorf("muzak: MCPOptions.MaxSessions is %d, but it must be between 1 and %d",
			o.MaxSessions, MaxMCPSessions))
	}
	if o.SessionIdleTimeout < 0 {
		errs = append(errs, fmt.Errorf("muzak: MCPOptions.SessionIdleTimeout is %s, but it must be positive; "+
			"leave it at zero for the %s default", o.SessionIdleTimeout, DefaultMCPSessionIdleTimeout))
	}
	for _, entry := range o.AllowedOrigins {
		if entry == "*" {
			errs = append(errs, errors.New("muzak: MCPOptions.AllowedOrigins holds \"*\", which would let a page anywhere "+
				"call the endpoint and is exactly what the MCP specification's Origin check exists to stop; list the origins, "+
				"or decide them in AllowOriginFunc"))
			continue
		}
		if err := checkOriginEntry("MCPOptions.AllowedOrigins entry", entry, true); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range o.ForwardHeaders {
		key := http.CanonicalHeaderKey(name)
		switch {
		case !isHTTPToken(name):
			errs = append(errs, fmt.Errorf("muzak: MCPOptions.ForwardHeaders holds %q, which is not a header name", clientShorten(name)))
		case headersNotBound[key] || key == "Content-Type" || key == "Accept" || strings.HasPrefix(key, "Mcp-"):
			errs = append(errs, fmt.Errorf("muzak: MCPOptions.ForwardHeaders holds %s, which a tool call writes itself or which "+
				"belongs to the MCP request; forward cookies with ForwardCookies", key))
		}
	}
	for _, name := range o.ForwardCookies {
		if !isHTTPToken(name) {
			errs = append(errs, fmt.Errorf("muzak: MCPOptions.ForwardCookies holds %q, which is not a cookie name", clientShorten(name)))
		}
	}
	return errs
}

// MCPTool makes a route a tool of the application's MCP endpoint, whatever
// [MCPOptions] chooses. Declaring it on a route that cannot be a tool, such
// as a WebSocket route or a [Hidden] one, is a build error rather than an
// option quietly ignored.
//
//	r.Get("/orders/{id}", handlers.GetOrder, muzak.Summary("Fetch an order"), muzak.MCPTool())
//
// The tool is named after the route's operation id, cut to the characters
// and length tool names may have; set [OperationID] to choose the name. It is
// described by the route's [Summary] and [Description], titled by its
// [Title], and its input and output schemas are the OpenAPI document's.
func MCPTool() RouteOption {
	return routeOptionFunc(func(c *routeConfig) { c.mcpTool = true })
}

// mcpServer is the MCP endpoint of one application.
type mcpServer struct {
	app  *App
	opts MCPOptions
	// post and del are the routes the endpoint is served by.
	post, del *Route

	// tools are the routes chosen as tools, sorted by name, and byName finds
	// one; both are fixed when the application is built.
	tools  []*mcpTool
	byName map[string]*mcpTool

	sessions *mcpSessions
	// cursorKey authenticates the cursors tools/list hands out; see
	// [mcpServer.cursor].
	cursorKey [sha256.Size]byte

	// forwardHeaders and forwardCookies are the credentials a tool call
	// copies from the MCP request besides Authorization, and forwarding the
	// headers a client's address or scheme is read from, which it copies too;
	// see [mcpServer.innerRequest].
	forwardHeaders []string
	forwardCookies []string
	forwarding     []string
	// credentials are the other places a credential is read from, which a
	// tool's input may not bind; see [mcpServer.credentialSources].
	credentials map[mcpCredential]string

	// cacheScope is what a 2026-07-28 listing says about who may cache it:
	// private when the endpoint is guarded, since a shared cache would hand
	// the list to callers the guard refuses.
	cacheScope string
	ttlMs      int64
}

// MCP serves a Model Context Protocol endpoint at path, through which an AI
// client calls the application's routes as tools:
//
//	app.MCP("/mcp", muzak.MCPOptions{Tags: []string{"agent"}},
//		muzak.WithSecurity(muzak.Require("oidc")))
//
// The endpoint speaks the Streamable HTTP transport of MCP revisions
// 2025-03-26, 2025-06-18 and 2025-11-25, which open a session with
// initialize, and of 2026-07-28, which is stateless. Every request is a POST
// of one JSON-RPC message, answered as application/json; DELETE ends a
// session and GET is refused with 405, as no stream from the server is
// offered. What a client may call is chosen by opts; see [MCPOptions].
//
// A tool call is the route's own request. Its arguments are decoded into the
// route's input type, written as the request the binder reads back, and
// served in-process by the whole application, so the route's security, guards,
// providers, rate limit, validation and releases all apply to it, and its
// error answer is the tool's error result. The request carries the MCP
// request's Authorization, and the headers and cookies opts forwards, its
// client address and request identifier, and its cancellation; nothing else.
// A token therefore reaches the routes of this application unchanged, and no
// other service. A credential is never the model's to choose: a route whose
// input binds the header, query parameter or cookie an API key scheme or the
// session reads one from is a build error once it is chosen. The answer is
// bounded by MaxResultSize.
//
// routeOpts configure the endpoint as they would any route: [WithSecurity]
// makes an unauthenticated client get the 401 whose challenge names the
// [ResourceMetadata] an OAuth client discovers the authorization server by,
// and guards, [WithRateLimit], [MaxBodySize] and [Timeout] apply to every
// message. The endpoint is left out of the OpenAPI document, since the tools
// it serves are described there already. Calling MCP twice is a build error,
// and like registering a route, it panics once the application is built.
func (a *App) MCP(path string, opts MCPOptions, routeOpts ...RouteOption) {
	a.mustBeOpen("App.MCP")
	if a.mcp != nil {
		a.errs = append(a.errs, errors.New("muzak: App.MCP was called more than once; an application serves one MCP endpoint, "+
			"and the tools it serves are chosen by its MCPOptions"))
		return
	}
	s := &mcpServer{app: a, opts: opts}
	a.mcp = s
	options := []RouteOption{Hidden()}
	if a.opts.Versioning.enabled() {
		// The endpoint answers whatever version a request declares; the
		// versions of the routes it calls are chosen per tool.
		options = append(options, WithVersion(VersionNeutral))
	}
	options = append(options, routeOpts...)
	s.post = a.Post(path, s.servePost, options...)
	s.del = a.Delete(path, s.serveDelete, options...)
}

// buildMCP completes the endpoint once every route is resolved: it chooses
// the tools, compiles how each is called and describes them. A problem is
// reported with the build's others.
func (a *App) buildMCP(state *buildState) {
	if a.mcp == nil {
		return
	}
	errs := a.mcp.build(len(state.errs) == 0)
	state.errs = append(state.errs, errs...)
}

// build does the work of [App.buildMCP]. The schemas are described only when
// nothing else is wrong, because they are made from the OpenAPI document,
// which is built from a routing tree that resolved.
func (s *mcpServer) build(healthy bool) []error {
	errs := s.opts.validate()
	s.opts = s.opts.withDefaults()
	a := s.app
	for _, name := range s.opts.ForwardHeaders {
		s.forwardHeaders = append(s.forwardHeaders, http.CanonicalHeaderKey(name))
	}
	s.forwardCookies = s.opts.ForwardCookies
	// The headers the client address and the scheme are read from, copied so
	// that a tool call is attributed and judged as the MCP request was; see
	// [mcpServer.innerRequest].
	s.forwarding = dedupeStrings([]string{"Forwarded", "X-Forwarded-Proto", http.CanonicalHeaderKey(a.clientIP.header)})
	s.credentials = s.credentialSources()
	errs = append(errs, s.chooseTools()...)
	if len(errs) > 0 || !healthy {
		return errs
	}
	doc, operations := a.describeOperations()
	if !a.opts.DisableDocs {
		// The same document, so the application does not describe itself twice.
		a.spec = doc
	}
	components := map[string]*Schema{}
	if doc.Components != nil {
		components = doc.Components.Schemas
	}
	for _, tool := range s.tools {
		if err := tool.describe(operations[tool.route], components); err != nil {
			errs = append(errs, err)
		}
	}
	s.sessions = newMCPSessions(s.opts.MaxSessions, s.opts.SessionIdleTimeout)
	// crypto/rand never fails to fill a buffer; it ends the program first.
	_, _ = rand.Read(s.cursorKey[:])
	s.cacheScope = "public"
	if s.post.isGuarded() {
		s.cacheScope = "private"
	}
	s.ttlMs = s.opts.ListTTL.Milliseconds()
	if a.obs != nil {
		// A tool call's span is a child of the MCP request's; see
		// [observability.startServerSpan].
		a.obs.inProcessParent = a.subrequestParent
	}
	return errs
}

// chooseTools finds every route MCPOptions chooses and compiles each one,
// reporting a route MCPTool chose that cannot be a tool, one whose input
// cannot be written as a request, and two whose tool names collide.
func (s *mcpServer) chooseTools() []error {
	var errs []error
	s.byName = map[string]*mcpTool{}
	for _, rt := range s.app.routes {
		explicit := rt.cfg.mcpTool
		if why := s.ineligible(rt); why != "" {
			if explicit {
				errs = append(errs, fmt.Errorf("muzak: %s %s is declared with MCPTool, but %s, so it cannot be a tool",
					rt.Method, rt.Path, why))
			}
			continue
		}
		if !explicit && !s.chooses(rt) {
			continue
		}
		tool, compileErrs := s.compileTool(rt)
		if len(compileErrs) > 0 {
			errs = append(errs, compileErrs...)
			continue
		}
		if other, taken := s.byName[tool.name]; taken {
			errs = append(errs, fmt.Errorf("muzak: %s %s and %s %s are both MCP tools named %q; tool names must be unique, "+
				"so give one of them its own name with muzak.OperationID", other.route.Method, other.route.Path,
				rt.Method, rt.Path, tool.name))
			continue
		}
		s.byName[tool.name] = tool
		s.tools = append(s.tools, tool)
	}
	slices.SortFunc(s.tools, func(x, y *mcpTool) int { return strings.Compare(x.name, y.name) })
	return errs
}

// ineligible says why a route can never be a tool, or returns the empty
// string when it can.
func (s *mcpServer) ineligible(rt *Route) string {
	switch {
	case rt == s.post || rt == s.del:
		return "it is the MCP endpoint itself"
	case rt.websocket != nil:
		return "it is a WebSocket route, whose conversation a tool call cannot hold"
	case rt.sse != nil:
		return "it is an event stream, which has no end a tool result could wait for"
	case rt.Hidden:
		return "it is hidden, and a tool's schemas are the OpenAPI document's"
	case !new(PathItem).set(rt.Method, nil):
		return "its method has no place in the OpenAPI document a tool's schemas come from"
	}
	return ""
}

// chooses reports whether the options choose a route by its tags or by the
// application's own rule.
func (s *mcpServer) chooses(rt *Route) bool {
	for _, tag := range s.opts.Tags {
		if slices.Contains(rt.Tags, tag) {
			return true
		}
	}
	return s.opts.Include != nil && s.opts.Include(rt)
}

// mcpToolName reduces an operation id to what a tool name may hold: ASCII
// letters, digits, underscore and hyphen, at most 64 of them. That is the
// narrowest rule any revision or common client applies, so a name valid here
// is valid everywhere.
func mcpToolName(operationID string) string {
	const most = 64
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, operationID)
	if len(name) > most {
		name = name[:most]
	}
	return name
}
