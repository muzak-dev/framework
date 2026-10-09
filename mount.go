package muzak

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"muzak.dev/framework/internal/radix"
)

// mountWildcard names the wildcard a mount is inserted into the routing tree
// under, beneath its prefix. Nothing binds it: the mounted handler reads the
// request's own path. It is a name no route could bind by accident, so a route
// that puts a wildcard of its own directly beneath a mount is reported by the
// tree as the conflict it is rather than quietly sharing the slot.
const mountWildcard = "muzak.mount"

// mountKind is what a mount is called in a message and in the stand-in route
// its rate limit is counted through.
const mountKind = "mount"

// StripPrefix makes a handler given to [Router.Mount] see the request's path
// relative to the mount, the way [net/http.StripPrefix] does, so a handler
// written to be served at the root can be served beneath a prefix unchanged:
//
//	app.Mount("/debug/pprof", pprofMux, muzak.StripPrefix())
//
// A request for "/debug/pprof/heap" then reaches the handler with the path
// "/heap", and one for the prefix itself, with or without its trailing slash,
// with "/" rather than the empty path [net/http.StripPrefix] leaves, which a
// [net/http.ServeMux] would answer with a redirect to the root of the site.
// The prefix is removed from the escaped path segment by segment, so a request
// that spelled the prefix with percent-escapes is stripped as well as one that
// did not, and what remains keeps its own escapes: "/a%2Fb" beneath the prefix
// arrives as the path "/a/b" with "/a%2Fb" as its escaped form. The request is
// a shallow copy carrying the new URL; RequestURI, the context and everything
// else are those of the original.
//
// A handler that redirects to an absolute path of its own, as ServeMux does to
// add a trailing slash, writes a Location relative to the root of the site
// rather than to the mount, which is true of [net/http.StripPrefix] too; serve
// such a handler without StripPrefix, at the path it expects.
//
// StripPrefix means something only to Router.Mount. Given to [NewRouter], to
// [Router.Include] or to [App.Options] it is a build error rather than an
// option that silently does nothing.
func StripPrefix() RouterOption {
	return routerOptionFunc(func(c *routerConfig) { c.stripPrefix = true })
}

// mountPoint is one handler served with [Router.Mount]: the prefix it answers,
// and what the routers it was registered under and the options it was given
// put in front of it.
type mountPoint struct {
	// raw is the prefix as it was given, and cfg the options; both are kept
	// until the application is built and the inherited configuration is known.
	raw     string
	handler http.Handler
	cfg     routerConfig

	// prefix is the full resolved prefix with no trailing slash, and is empty
	// for a mount at the root. segments counts its segments, which is how many
	// StripPrefix removes. template is what [RouteFromContext] reports for a
	// request the mount answers: the prefix, or "/" for the root.
	prefix   string
	segments int
	template string

	strip     bool
	guards    []Guard
	providers []*provider
	// private reports that the mount answers some clients and not others, so
	// its responses are marked as such unless the handler says otherwise.
	private bool
	// maxBodySize bounds a request body the handler reads, and is not
	// positive when the body is unbounded.
	maxBodySize int64

	// limits is a stand-in route carrying the mount's rate limit, resolved and
	// completed exactly as a frontend mount's is; see [frontend.limits].
	limits *Route
}

// Mount serves any [net/http.Handler] at prefix: the prefix itself and every
// path beneath it, for every method. It is how a handler that was not written
// for Muzak joins an application, such as net/http/pprof, a metrics handler,
// a connect-go or gRPC-gateway service, or a legacy application being
// replaced route by route:
//
//	app.Mount("/metrics", promhttp.Handler(), muzak.Needs(auth.RequireOperator))
//	app.Mount("/debug/pprof", pprofMux, muzak.StripPrefix())
//	app.Mount("/", legacy) // whatever no route answers yet
//
// The prefix is an absolute path relative to the router it is registered on,
// under whatever prefixes that router is included with, as a route's path is.
// It is fixed text: a parameter, a wildcard, an empty or dot segment, or a
// character a request carries only percent-encoded is a build error. A single
// trailing slash is ignored, so "/debug/pprof/" and "/debug/pprof" are one
// mount, and "/" mounts at the root of the router.
//
// The most specific answer wins, as it does everywhere in the routing tree. A
// route registered at a path beneath the prefix answers the methods it
// registers there, together with the HEAD a GET route answers, and the
// mounted handler answers every other method at that path, including OPTIONS:
// a legacy application keeps POST /users while GET /users is moved to a route.
// A mount at a longer prefix beneath this one, and a [Router.Frontend] or
// [Router.Static] mounted beneath it, answer what is beneath their own
// prefix. A route whose wildcard sits directly beneath the prefix would answer
// everything the mount does, and is a build error, as are two mounts at one
// prefix and a mount at the prefix of a Frontend or Static mount.
//
// The handler runs inside the application: the middleware installed with
// [App.Use], the CORS policy, the security headers, the access log, request
// identification and panic recovery all apply, and before it runs, so does
// what a route of the same routers would run before its handler, in the same
// order: the rate limit, the guards and the providers of every router the
// mount is registered under and of opts. A refusal is rendered by the error
// renderer exactly as a route's is, and a mount behind any guard or
// request-scoped provider answers with "Cache-Control: private, no-cache"
// unless the handler sets a Cache-Control of its own. A provider's value is
// not handed on, since the handler has no [Context] to read it from; the
// provider runs for its verdict.
//
// The handler receives the original *http.Request, carrying the framework's
// context values: the request identifier, the locale, and [RouteFromContext]
// reporting the mount's prefix. It is never handed the pooled Context. A
// panic in it is recovered and rendered as a route's is, with
// [net/http.ErrAbortHandler] still aborting the response. A request body is
// bounded by the [MaxBodySize] the mount inherits: one that declares a length
// over it is refused with 413 before the handler runs, and one of unknown
// length is read through [net/http.MaxBytesReader], which fails the read that
// goes over. On a mount, unlike a route, MaxBodySize given a negative value
// removes the limit, which a streaming protocol such as a client-streaming RPC
// needs; such a handler must then bound its own messages. Options that only
// describe or bind a route, such as [WithTags] or [CaptureBody], have no
// effect here. A [Timeout] is not applied to the handler: one inherited from a
// router is left out, as it is for an event stream, and one given to Mount is
// a build error.
//
// # What the guards cover
//
// Nothing normalises a path before it is matched: no case folding, no "//"
// collapsing and no resolving of "." or "..". A request reaches the mount only
// through the tree's exact match of the prefix, segment by segment, with a
// percent-escape decoded only where it does not encode a "/". So
// "/debug/pprof/x", "/debug/pprof//x" and "/%64ebug/pprof/x" reach a mount at
// "/debug/pprof" and run its guards, while "//debug/pprof/x", "/DEBUG/pprof/x",
// "/./debug/pprof/x", "/x/../debug/pprof/x" and "/debug%2Fpprof/x" do not reach
// it at all, and go wherever else the tree sends them. Whatever the handler
// serves has therefore passed its guards; what it serves for the path it is
// given is its own affair, and a handler that cleans, decodes or case-folds
// the path may serve, for a spelling that reached it, what a guarded route
// beside it was meant to protect. Give such a handler guards of its own.
//
// Mounts are not described in the OpenAPI document, which describes the
// routes Muzak can see into. Mount panics once the application has been built,
// as registering a route does.
func (r *Router) Mount(prefix string, h http.Handler, opts ...RouterOption) {
	r.mustBeOpen("mounting a handler at " + prefix)
	if h == nil {
		r.errs = append(r.errs, fmt.Errorf("muzak: mount at %q: the handler is nil", prefix))
		return
	}
	m := &mountPoint{raw: prefix, handler: h}
	for _, opt := range opts {
		opt.applyRouter(&m.cfg)
	}
	r.mounts = append(r.mounts, m)
}

// finalizeMounts resolves the mounts registered on a router against what the
// router inherits, and reports [StripPrefix] given anywhere but to a mount.
func (r *Router) finalizeMounts(in inherited, state *buildState) {
	if r.cfg.stripPrefix {
		state.errs = append(state.errs, fmt.Errorf("muzak: %s was given StripPrefix, which only Router.Mount "+
			"understands; pass it to the Mount call", describeRouter(in.prefix, r)))
	}
	for _, inc := range r.includes {
		if inc.cfg.stripPrefix {
			state.errs = append(state.errs, fmt.Errorf("muzak: Include of %s was given StripPrefix, which only "+
				"Router.Mount understands; pass it to the Mount call", describeRouter(in.prefix+inc.cfg.prefix, inc.child)))
		}
	}
	for _, m := range r.mounts {
		state.lifecycles = append(state.lifecycles, m.cfg.lifecycles...)
		if err := m.resolve(in); err != nil {
			state.errs = append(state.errs, err)
			continue
		}
		state.mounts = append(state.mounts, m)
	}
}

// resolve computes a mount's full prefix, its dependency chain, its body limit
// and its rate limit.
func (m *mountPoint) resolve(in inherited) error {
	if !strings.HasPrefix(m.raw, "/") {
		return fmt.Errorf("muzak: mount at %q: the prefix must begin with %q", m.raw, "/")
	}
	merged := in.merge(m.cfg)
	full := merged.prefix + strings.TrimSuffix(m.raw, "/")
	if err := checkMountPrefix(full); err != nil {
		return fmt.Errorf("muzak: mount at %q: %w", orDefault(full, "/"), err)
	}
	if m.cfg.timeout > 0 {
		// Accepted, it would read as a bound on the handler while nothing
		// applied it; see [Timeout].
		return fmt.Errorf("muzak: mount at %q: Timeout cannot be declared on a mount, whose handler writes its own response; "+
			"give the handler a deadline of its own, with http.TimeoutHandler for one", orDefault(full, "/"))
	}
	m.prefix = full
	m.segments = strings.Count(full, "/")
	m.template = orDefault(full, "/")
	m.strip = m.cfg.stripPrefix
	m.guards = merged.guards
	m.providers = merged.providers
	m.private = answersPerClient(m.guards, m.providers)
	m.maxBodySize = merged.maxBodySize
	if m.cfg.maxBodySize < 0 {
		m.maxBodySize = -1
	}
	m.limits = &Route{Method: mountKind, Path: m.template}
	return m.limits.resolveRateLimit(merged)
}

// checkMountPrefix reports why a resolved prefix cannot be a mount's. The
// empty prefix is the root and is always valid.
//
// A prefix is matched by the routing tree segment by segment, so it has to be
// something a segment of a request can equal: plain text, with no parameter,
// no empty segment and no "." or "..", which a client resolves before it
// sends the request and which therefore never arrive, and no character a
// request carries only percent-encoded, whose bare spelling never arrives
// either.
func checkMountPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	for segment := range strings.SplitSeq(prefix[1:], "/") {
		switch {
		case segment == "":
			return errors.New("the prefix holds an empty segment, which a request reaches only by a path no client sends; write it without the extra slash")
		case segment == "." || segment == "..":
			return errors.New("the prefix holds a dot segment, which a client resolves before it sends a request, so no request would ever carry it")
		case strings.ContainsAny(segment, "{}"):
			return errors.New("the prefix holds a parameter, but a mount answers one fixed prefix; mount it on a router whose prefix is fixed text")
		}
		for i := 0; i < len(segment); i++ {
			if segment[i] >= utf8.RuneSelf {
				return errors.New("the prefix holds a character outside ASCII, which a request path carries only percent-encoded; " +
					"write the prefix in ASCII")
			}
			if !isMountPathByte(segment[i]) {
				return fmt.Errorf("the prefix holds %q, which a request path carries only percent-encoded; write the prefix as plain text", segment[i:i+1])
			}
		}
	}
	return nil
}

// isMountPathByte reports whether a byte may appear unescaped in a segment of
// a mount's prefix: the characters RFC 3986 lets a path segment carry as they
// are, other than '%', so that a prefix is spelled one way only.
func isMountPathByte(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@", b) >= 0
}

// covers reports whether a route template lies at or beneath the mount's
// prefix, so that a request the routing tree resolves to the template is one
// the mount would otherwise have answered. Each segment of the prefix must be
// the template's static segment at the same position, compared decoded as the
// tree compares it; a parameter there does not count, because the tree
// prefers the mount's static segment to it, so no request matching the
// parameter carries the prefix.
func (m *mountPoint) covers(template string) bool {
	if m.prefix == "" {
		return true
	}
	rest := template
	for want := range strings.SplitSeq(m.prefix[1:], "/") {
		if rest == "" || rest[0] != '/' {
			return false
		}
		rest = rest[1:]
		got := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			got, rest = rest[:i], rest[i:]
		} else {
			rest = ""
		}
		if strings.IndexByte(got, '%') >= 0 {
			// A template the tree accepted is validly encoded, so this
			// decodes; one that did not would equal no prefix segment, which
			// cannot contain a '%'.
			if decoded, err := url.PathUnescape(got); err == nil {
				got = decoded
			}
		}
		if got != want {
			return false
		}
	}
	return true
}

// installMounts checks the mounts against each other, the file mounts and the
// documentation, inserts them into the routing tree, and records on every path
// a route answers beneath a mount which mount answers its other methods. It
// runs once every route and file mount is known.
func (a *App) installMounts(state *buildState) {
	if len(state.mounts) == 0 {
		return
	}
	installed := make(map[string]*mountPoint, len(state.mounts))
	for _, m := range state.mounts {
		if _, taken := installed[m.prefix]; taken {
			state.errs = append(state.errs, fmt.Errorf("muzak: mount at %q: a handler is mounted at this prefix more than once, "+
				"and only one of them could ever answer", m.template))
			continue
		}
		if err := a.checkMountNeighbours(m); err != nil {
			state.errs = append(state.errs, err)
			continue
		}
		if err := a.insertMount(m); err != nil {
			state.errs = append(state.errs, err)
			continue
		}
		installed[m.prefix] = m
	}
	for template, entry := range a.entries {
		if entry.mount != nil {
			continue
		}
		for _, m := range installed {
			if m.covers(template) && (entry.mount == nil || m.segments > entry.mount.segments) {
				entry.mount = m
			}
		}
	}
}

// checkMountNeighbours reports a mount at the prefix of a frontend or static
// mount, or at a documentation path, either of which would leave one of the
// two answering nothing there.
func (a *App) checkMountNeighbours(m *mountPoint) error {
	for _, f := range a.frontends {
		if f.path == m.prefix {
			return fmt.Errorf("muzak: mount at %q: %s are mounted at the same prefix, and only one of the two could answer; "+
				"move one of them", m.template, f.kind)
		}
	}
	if a.opts.DisableDocs {
		return nil
	}
	if m.template == a.opts.OpenAPIPath || (a.opts.DocsUI != nil && m.template == a.opts.DocsPath) {
		return fmt.Errorf("muzak: mount at %q: the documentation is served at this path and is answered ahead of routing; "+
			"move the mount, move the documentation, or set AppOptions.DisableDocs", m.template)
	}
	return nil
}

// insertMount places a mount in the routing tree: at its prefix, which a route
// registered at the same path shares, and at a wildcard beneath it.
func (a *App) insertMount(m *mountPoint) error {
	patterns := []string{m.prefix + "/{" + mountWildcard + "...}"}
	if m.prefix != "" {
		patterns = append(patterns, m.prefix)
	}
	for _, pattern := range patterns {
		entry, err := a.entryFor(pattern)
		if errors.Is(err, radix.ErrParamConflict) {
			return fmt.Errorf("muzak: mount at %q: a route whose wildcard sits directly beneath this prefix already "+
				"answers every path the mount would; remove one of the two", m.template)
		}
		if err != nil {
			// coverage: a prefix checkMountPrefix accepted is always a valid
			// pattern, so only the conflict above can be reported here.
			return fmt.Errorf("muzak: mount at %q: %w", m.template, err)
		}
		entry.mount = m
	}
	return nil
}

// dispatchMount answers a request the routing tree resolved to a path a mount
// answers, for a method no route registers there.
//
// A GET route answers HEAD at its own path as it would without the mount,
// since the two are one resource. A path only the mount answers is checked
// against the file mounts first, because one mounted beneath the prefix is
// more specific; see [App.frontendBeneath].
func (a *App) dispatchMount(c *Context, entry *pathEntry) {
	if c.r.Method == http.MethodHead {
		if candidates, ok := entry.methods[http.MethodGet]; ok && anyAnswersHead(candidates) {
			a.dispatchFallback(c, entry)
			return
		}
	}
	m := entry.mount
	if len(entry.methods) == 0 && len(a.frontends) > 0 {
		if f, relative, refused, found := a.frontendBeneath(m, c.r.URL.Path); found {
			if refused {
				a.fail(c, frontendNotFound(c.r))
				return
			}
			a.serveFrontend(c, f, relative)
			// As dispatch settles a file mount it reaches directly: what the
			// file mount's providers acquired is released as served, not left
			// to the end of the request, which would release it as a failure.
			a.settleServed(c)
			return
		}
	}
	a.serveMount(c, m)
}

// frontendBeneath finds a file mount more specific than a handler mount that
// covers a request path, as [App.frontendFor] does for a path no route or
// mount answers. A file mount the path falls under only once case is ignored
// or a backslash is read as a separator refuses it, rather than leave it to
// the less specific handler, for the reason frontendFor gives.
func (a *App) frontendBeneath(m *mountPoint, requestPath string) (f *frontend, relative string, refused, found bool) {
	for _, mount := range a.frontends {
		if len(mount.path) <= len(m.prefix) {
			// Ordered longest first, so nothing after this is more specific.
			return nil, "", false, false
		}
		if relative, ok := mount.matches(requestPath); ok {
			return mount, relative, false, true
		}
		if mount.coversLoosely(requestPath) {
			return nil, "", true, true
		}
	}
	return nil, "", false, false
}

// serveMount runs a mount's rate limit, guards and providers and hands the
// request to its handler.
func (a *App) serveMount(c *Context, m *mountPoint) {
	// Published for instrumentation, as dispatch does for a route.
	if holder, ok := c.r.Context().Value(routeContextKey{}).(*routeHolder); ok {
		holder.template = m.template
	}
	defer a.recoverMount(c, m)
	if err := m.admit(c); err != nil {
		a.fail(c, err)
		return
	}
	r, err := m.request(c)
	if err != nil {
		a.fail(c, err)
		return
	}
	if r != c.r {
		// The handler is given a copy, so a multipart form it parses is set on
		// the copy alone, where neither net/http's cleanup nor the one the
		// request's Context runs would ever see its temporary files. Deferred,
		// so a panic removes them too.
		defer releaseUpload(r)
	}
	if m.private {
		// As for a guarded route: set before the handler runs, so it covers a
		// handler that writes its own body, and only if absent, so the
		// handler's own policy stands.
		setIfAbsent(c.w.Header(), "Cache-Control", privateCacheControl)
	}
	m.handler.ServeHTTP(c.w, r)
	// What the mount's providers acquired is released once the handler has
	// returned without panicking, as a file mount's is once the file is served:
	// the handler decided what to answer, so the request did not fail on the
	// framework's account. A release that fails is reported as a handler's
	// failure would be, which once the handler has written aborts the response.
	// A panic is released with its failure by the recovery deferred above.
	if err := c.settle(nil); err != nil {
		a.fail(c, err)
	}
}

// admit runs what a route of the same routers runs before its handler, in the
// order [App.run] runs it.
func (m *mountPoint) admit(c *Context) error {
	limits := m.limits.rateLimit
	if limits != nil && !limits.afterDependencies {
		if err := limits.check(c); err != nil {
			return err
		}
	}
	for _, guard := range m.guards {
		if err := guard(c); err != nil {
			return err
		}
	}
	if err := resolveProviders(c, m.providers); err != nil {
		return err
	}
	if limits != nil && limits.afterDependencies {
		return limits.check(c)
	}
	return nil
}

// request returns the request the handler is given: the original, with its
// body bounded, or a shallow copy of it with the prefix stripped.
//
// A body that declares a length over the limit is refused before the handler
// runs, as a route refuses one, so a client that asked for 100 Continue is not
// told to send it. A body of unknown length is bounded as it is read. A body
// that declares a length within the limit is already bounded by net/http,
// which never reads past the length declared, so it is left as it is.
func (m *mountPoint) request(c *Context) (*http.Request, error) {
	r := c.r
	if m.maxBodySize > 0 && r.Body != nil && r.Body != http.NoBody {
		if declaredOverLimit(r, m.maxBodySize) {
			return nil, NewHTTPErrorf(http.StatusRequestEntityTooLarge,
				"request body exceeds the %d byte limit for this mount", m.maxBodySize)
		}
		if r.ContentLength < 0 {
			r.Body = http.MaxBytesReader(c.w, r.Body, m.maxBodySize)
		}
	}
	if m.strip {
		r = m.stripped(r)
	}
	return r, nil
}

// stripped returns a shallow copy of r whose path is relative to the mount.
//
// The prefix is removed from the escaped path one segment at a time, because
// that is what the routing tree matched: a segment of the prefix may have
// arrived percent-encoded, so the decoded path cannot simply be cut at the
// prefix's length. What remains is decoded to give the new path, and kept as
// the escaped form when decoding changed it.
func (m *mountPoint) stripped(r *http.Request) *http.Request {
	// Each step drops one segment and the separator in front of it. The tree
	// only routes a path with at least as many segments as the prefix here,
	// and the walk stops at the end of the path whatever it is given.
	rest := r.URL.EscapedPath()
	for n := 0; n < m.segments && rest != ""; n++ {
		if i := strings.IndexByte(rest[1:], '/'); i >= 0 {
			rest = rest[i+1:]
		} else {
			rest = ""
		}
	}
	if rest == "" {
		rest = "/"
	}
	path, err := url.PathUnescape(rest)
	if err != nil {
		// coverage: the escaped path is the one net/http parsed, or that
		// EscapedPath produced from a decoded one, and both decode; this
		// keeps a request built by hand from being handed a path it did not
		// send.
		path = rest
	}
	stripped := new(http.Request)
	*stripped = *r
	u := *r.URL
	// The escaped form is kept wherever it is not the one net/url would write
	// for the path, which is the rule url.URL keeps RawPath by, so that a
	// remainder the client spelled "/a%2Fb" or "/!" keeps that spelling rather
	// than becoming "/a/b" or "/%21".
	u.Path, u.RawPath = path, ""
	if u.EscapedPath() != rest {
		u.RawPath = rest
	}
	stripped.URL = &u
	return stripped
}

// recoverMount turns a panic in a mounted handler, or in what runs before it,
// into the response a panic in a route produces; see [App.recoverRoute].
func (a *App) recoverMount(c *Context, m *mountPoint) {
	recovered := recover()
	if recovered == nil {
		return
	}
	// recover returns any, not error, so errors.Is does not apply here. This is
	// the same identity comparison net/http performs on the sentinel.
	if recovered == http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
		panic(recovered)
	}
	a.logger.ErrorContext(c.Context(), "muzak: recovered from a panic in a mounted handler",
		slog.String("panic", panicValue(recovered)),
		slog.String("method", truncateForMessage(c.r.Method)),
		slog.String("mount", m.template),
		slog.String(RequestIDKey, c.RequestID()),
		slog.String("stack", string(debug.Stack())))
	a.fail(c, errPanic)
}

// mountRateLimitOwners returns the stand-in route each mount counts its
// requests through, for [App.resolveRateLimiting] to complete with every
// route's.
func mountRateLimitOwners(mounts []*mountPoint) []*Route {
	owners := make([]*Route, 0, len(mounts))
	for _, m := range mounts {
		owners = append(owners, m.limits)
	}
	return owners
}
