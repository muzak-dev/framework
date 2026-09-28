package muzak

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// The contract a documentation UI meets, and the two strings the framework
// rewrites in its page. A UI is built without knowing where it will be
// mounted or where the document it reads is served, so it writes both as
// placeholders and they are substituted when the application is built. See
// [AppOptions.DocsUI].
const (
	// docsUIIndex is the page itself within the UI's file system.
	docsUIIndex = "index.html"
	// docsUIBasePlaceholder prefixes every absolute URL the page carries, and
	// becomes the configured documentation path.
	docsUIBasePlaceholder = "/__muzak_docs__/"
	// docsUISpecPlaceholder is where the page fetches the OpenAPI document
	// from, and becomes the configured document path.
	docsUISpecPlaceholder = "/__muzak_spec__"
)

// docsAssets holds everything the documentation routes serve, prepared once
// when the application is built.
//
// page is the dashboard's HTML shell, served at [AppOptions.DocsPath]. static
// holds every file the shell then loads - the scripts, the stylesheet and the
// fonts - keyed by the path it is served at, which is the documentation path
// followed by the file's own name within the build.
type docsAssets struct {
	spec   *asset
	page   *asset
	static map[string]*asset
}

// prepareDocs renders the OpenAPI document and the documentation dashboard. It
// returns nil when the document cannot be rendered, which leaves the routes
// unregistered rather than serving a broken page.
//
// The dashboard is built elsewhere (see [internal/docsui]) and knows neither
// where it will be mounted nor where the document it reads is served, so both
// are written into the shell here: every absolute URL is rebased onto the
// configured documentation path, and the placeholder the page fetches its
// document from becomes the configured OpenAPI path. Only the shell carries
// absolute URLs; the scripts import each other relatively and the stylesheet
// reaches its fonts relatively, so nothing else has to be rewritten.
func (a *App) prepareDocs() *docsAssets {
	spec, err := a.spec.Marshal()
	if err != nil {
		// coverage: Document is built from Muzak's own types, every one of
		// which is JSON-encodable, so marshaling cannot fail. The branch keeps
		// a future schema field from taking down start-up.
		Scoped(a.logger, ScopeDocs).Error("muzak: the OpenAPI document could not be rendered",
			slog.String("error", err.Error()))
		return nil
	}

	assets := &docsAssets{
		spec:   newAsset("application/json; charset=utf-8", spec),
		static: map[string]*asset{},
	}

	files := a.opts.DocsUI
	if files == nil {
		// No UI was configured, which is the default: the application still
		// describes itself at OpenAPIPath, and DocsPath answers as any other
		// unknown path does. See [AppOptions.DocsUI] for adding one.
		return assets
	}

	shell, err := fs.ReadFile(files, docsUIIndex)
	if err != nil {
		Scoped(a.logger, ScopeDocs).Error("muzak: AppOptions.DocsUI carries no "+docsUIIndex+", so no documentation page is served",
			slog.String("error", err.Error()))
		return assets
	}

	base := urlPath(strings.TrimSuffix(a.opts.DocsPath, "/")) + "/"
	page := strings.ReplaceAll(string(shell), docsUIBasePlaceholder, base)
	page = strings.ReplaceAll(page, docsUISpecPlaceholder, urlPath(a.opts.OpenAPIPath))
	assets.page = newAsset("text/html; charset=utf-8", []byte(page))
	assets.page.policy = contentSecurityPolicy(page)

	err = fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || name == docsUIIndex {
			return err
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			// coverage: every name comes from walking the embedded tree.
			return err
		}
		assets.static[base+name] = newAsset(docsContentType(name), body)
		return nil
	})
	if err != nil {
		Scoped(a.logger, ScopeDocs).Error("muzak: AppOptions.DocsUI could not be read, so no documentation page is served",
			slog.String("error", err.Error()))
		assets.page, assets.static = nil, nil
	}
	return assets
}

// docsContentType names the media type of one of the dashboard's files.
//
// The set is closed: these are the extensions its build produces, and anything
// else is served as bytes rather than guessed at, since a wrong type on a
// script is a page that silently does not run.
func docsContentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".txt":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// validateDocsPaths checks that the documentation can be reached where it was
// configured to be.
//
// A path that is not absolute would never match a request, two equal paths
// would leave the page and the document it reads fighting over one address,
// and a path an application route already answers would be shadowed by a page
// nobody asked for at that address. All three are reported while the
// application is built, rather than discovered as a 404 in a browser.
func (a *App) validateDocsPaths(state *buildState) {
	if a.opts.DisableDocs {
		return
	}
	// DocsPath is only checked when a UI is configured to be served there. An
	// application with no UI serves nothing at that path, so a route of its
	// own may use it, and the default "/docs" must not become a reserved word
	// for every service that never wanted a dashboard.
	configured := [...]struct {
		field, path string
		served      bool
	}{
		{"AppOptions.DocsPath", a.opts.DocsPath, a.opts.DocsUI != nil},
		{"AppOptions.OpenAPIPath", a.opts.OpenAPIPath, true},
	}
	for _, configured := range configured {
		if !configured.served {
			continue
		}
		if !strings.HasPrefix(configured.path, "/") {
			state.errs = append(state.errs, fmt.Errorf(
				"muzak: %s is %q, which is not an absolute path and so would answer no request; write it as %q",
				configured.field, configured.path, "/"+configured.path))
			continue
		}
		if _, taken := a.entries[configured.path]; taken {
			state.errs = append(state.errs, fmt.Errorf(
				"muzak: %s is %q, which a route of this application already answers; "+
					"move one of the two, or set AppOptions.DisableDocs to serve no documentation at all",
				configured.field, configured.path))
		}
	}
	if a.opts.DocsUI != nil && a.opts.DocsPath == a.opts.OpenAPIPath {
		state.errs = append(state.errs, fmt.Errorf(
			"muzak: AppOptions.DocsPath and AppOptions.OpenAPIPath are both %q, "+
				"but the page and the document it reads need an address each",
			a.opts.DocsPath))
	}
}

// logDocumentation reports where the documentation can be read the moment the
// socket is open, as URLs that can be opened from the terminal the server was
// started in.
func (a *App) logDocumentation(scheme, addr string) {
	log := Scoped(a.logger, ScopeDocs)
	if a.opts.DisableDocs {
		log.Debug("Documentation is not served, because AppOptions.DisableDocs is set")
		return
	}
	openapi := browsableURL(scheme, addr, a.opts.OpenAPIPath)
	if a.opts.DocsUI == nil {
		// Without a UI there is still a document, and saying where it is beats
		// saying nothing; the hint is how an application finds out a dashboard
		// exists at all.
		log.Info("OpenAPI document at "+openapi,
			slog.String("ui", "set AppOptions.DocsUI to serve a documentation page"))
		return
	}
	log.Info("Documentation at "+browsableURL(scheme, addr, a.opts.DocsPath),
		slog.String("openapi", openapi))
}

// browsableURL renders a bound address and a path as a URL that can be
// followed. A socket bound to every interface is reported as localhost,
// because "[::]" is where the process listens rather than somewhere a browser
// can go.
func browsableURL(scheme, addr, path string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// coverage: the address comes from a listening socket, which always
		// carries a port. Reporting it as it is beats reporting nothing.
		return scheme + "://" + addr + path
	}
	switch host {
	case "", "::", "0.0.0.0":
		host = "localhost"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + path
}

// withDocs intercepts the documentation paths and delegates everything else to
// the application's routes.
//
// The documentation is answered here, ahead of routing, so it runs the
// application's own guards and providers itself: those declared on [New],
// which every route inherits. Serving it without them published the full
// shape of an API, every internal path and header it reads included, to
// clients the same application refused on every route.
func (a *App) withDocs(next http.Handler) http.Handler {
	assets := a.prepareDocs()
	if assets == nil {
		return next
	}
	specPath := a.opts.OpenAPIPath
	docsPath := a.opts.DocsPath

	serve := func(as *asset, w http.ResponseWriter, r *http.Request) { as.serve(w, r) }
	if guards, providers := a.cfg.guards, a.cfg.providers; len(guards) > 0 || len(providers) > 0 {
		// A document only some clients may read must not be kept by a cache
		// shared between them.
		assets.markPrivate()
		serve = func(as *asset, w http.ResponseWriter, r *http.Request) {
			a.serveGuardedAsset(as, w, r, guards, providers)
		}
	}

	// The dashboard is one page at the documentation path, and its scripts,
	// stylesheet and fonts sit beneath it. Serving them from the same subtree
	// is what lets the whole thing move with DocsPath, and it keeps the
	// application's own namespace clear: nothing is mounted at the root.
	docsSlash := strings.TrimSuffix(docsPath, "/") + "/"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == specPath:
			serve(assets.spec, w, r)
		case assets.page != nil && (r.URL.Path == docsPath || r.URL.Path == docsSlash):
			serve(assets.page, w, r)
		default:
			if static, found := assets.static[r.URL.Path]; found {
				serve(static, w, r)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

// serveGuardedAsset runs the application's guards and then its providers, in
// the order a route runs them, and serves the asset only if every one of them
// let the request through. A refusal is rendered by the same error path a
// route's is, so a client sees the same 401 from the documentation as from
// the API it describes.
func (a *App) serveGuardedAsset(as *asset, w http.ResponseWriter, r *http.Request, guards []Guard, providers []*provider) {
	rw := asResponseWriter(w)
	c := a.acquire(rw, r)
	defer a.release(c)
	for _, guard := range guards {
		if err := guard(c); err != nil {
			a.fail(c, err)
			return
		}
	}
	if err := resolveProviders(c, providers); err != nil {
		a.fail(c, err)
		return
	}
	as.serve(rw, r)
}

// markPrivate marks every documentation asset as one a shared cache must not
// store, for an application whose documentation is served only to some
// clients.
func (d *docsAssets) markPrivate() {
	d.spec.private = true
	if d.page != nil {
		d.page.private = true
	}
	for _, static := range d.static {
		static.private = true
	}
}

// asset is one document the documentation routes serve. Everything a response
// needs is computed when the application is built: the body, its compressed
// form, the entity tag of each, and the policy the body is served under. A
// request then costs a few header writes and one copy of a byte slice that
// never changes, with no rendering, no allocation and no compression on the
// request path.
type asset struct {
	contentType string
	// policy is the Content-Security-Policy the asset is served under, and is
	// empty for one that needs none.
	policy string
	// private reports that the asset is served only to clients the
	// application's guards admit, so no cache shared between clients may keep
	// it.
	private  bool
	body     []byte
	etag     string
	gzip     []byte
	gzipETag string
}

// newAsset prepares a document for serving. The compressed form is kept only
// when it is smaller than the original, so a body that does not compress is
// never sent as a larger one, and it carries an entity tag of its own because
// a cache holding both representations must be able to tell them apart.
func newAsset(contentType string, body []byte) *asset {
	as := &asset{
		contentType: contentType,
		body:        body,
		etag:        entityTag(body, ""),
	}
	if packed, err := gzipBytes(body); err == nil && len(packed) < len(body) {
		as.gzip = packed
		as.gzipETag = entityTag(body, "gzip")
	}
	return as
}

// serve writes the asset, honouring conditional requests so that a
// documentation page reloaded repeatedly transfers its body once, and sending
// the compressed form to a client that accepts it.
func (as *asset) serve(w http.ResponseWriter, r *http.Request) {
	if !isReadMethod(r.Method) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, etag := as.body, as.etag
	compressed := as.gzip != nil && negotiateEncoding(r.Header.Get("Accept-Encoding")) == "gzip"
	if compressed {
		body, etag = as.gzip, as.gzipETag
	}

	header := w.Header()
	header.Set("Content-Type", as.contentType)
	header.Set("ETag", etag)
	// The documents change whenever the application does, so a client may hold
	// them but has to ask; the entity tag then makes that question cheap.
	if as.private {
		header.Set("Cache-Control", "private, no-cache")
	} else {
		header.Set("Cache-Control", "no-cache")
	}
	if as.policy != "" {
		header.Set("Content-Security-Policy", as.policy)
	}
	if as.gzip != nil {
		addVaryAcceptEncoding(header)
	}
	if compressed {
		header.Set("Content-Encoding", "gzip")
	}
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// entityTag derives a strong entity tag from a body. The tag of a compressed
// representation is marked as such, because it is a different sequence of
// bytes for the same document and a cache must not serve one where the other
// was asked for.
func entityTag(body []byte, encoding string) string {
	sum := sha256.Sum256(body)
	tag := `"` + hex.EncodeToString(sum[:8])
	if encoding != "" {
		tag += "-" + encoding
	}
	return tag + `"`
}

// matchesETag reports whether an If-None-Match header names the tag the
// response would carry. The comparison is the weak one the specification asks
// for on a conditional read, so a tag some intermediary marked weak still
// matches the strong one it was derived from.
func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	for entry := range strings.SplitSeq(header, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "*" || strings.TrimPrefix(entry, "W/") == etag {
			return true
		}
	}
	return false
}

// gzipBytes compresses a body once, at the level that costs the most time and
// produces the fewest bytes: this runs at start-up, and every request
// afterwards is paid for by it.
func gzipBytes(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		// coverage: the level is a constant the package accepts.
		return nil, err
	}
	if _, err := writer.Write(body); err != nil {
		// coverage: a bytes.Buffer does not fail a write.
		return nil, err
	}
	if err := writer.Close(); err != nil {
		// coverage: closing only flushes into that same buffer.
		return nil, err
	}
	return buf.Bytes(), nil
}

// contentSecurityPolicy builds the policy the documentation page is served
// under. Everything the dashboard loads comes from this application: its
// scripts and stylesheet are served beside the page, its fonts are embedded
// alongside them, and the only network access it is granted is to this
// origin, which is what the request console needs and all it needs.
//
// The shell's own inline scripts - the colour-mode preference and the
// configuration block naming the OpenAPI path - are identified by the hash of
// their contents rather than by a nonce issued per response. That is what
// makes the page a constant: it is compressed once, a client may cache it and
// revalidate with an entity tag, and serving it needs neither randomness nor
// rewriting.
//
// Inline styles are permitted, which the page embedded before this dashboard
// did not need. A Vue application sets style attributes as it renders - the
// width of a resized pane, the offset of a popover, the height of a
// transition - and those are style attributes rather than blocks, so no hash
// can cover them. It is the one relaxation the dashboard costs, and it is
// confined to styling: no script executes that is not hashed here or served
// from this origin.
func contentSecurityPolicy(page string) string {
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self' " + inlineHashes(page, "script"),
		"style-src 'self' 'unsafe-inline'",
		"font-src 'self'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

// inlineHashes returns the policy source list covering every inline block of
// one kind in the page, as the base64 SHA-256 of each block's contents. A page
// with no such block yields 'none', which forbids the kind outright rather
// than leaving the directive empty and unparseable, and a block left unclosed
// by a bad edit ends the walk rather than being covered by a hash of the rest
// of the file.
func inlineHashes(page, element string) string {
	var sources []string
	open, closing := "<"+element, "</"+element+">"
	rest := page
	for {
		start := strings.Index(rest, open)
		if start < 0 {
			break
		}
		rest = rest[start+len(open):]
		attributes := strings.Index(rest, ">")
		if attributes < 0 {
			break
		}
		body := rest[attributes+1:]
		end := strings.Index(body, closing)
		if end < 0 {
			break
		}
		sum := sha256.Sum256([]byte(body[:end]))
		sources = append(sources, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		rest = body[end+len(closing):]
	}
	if len(sources) == 0 {
		return "'none'"
	}
	return strings.Join(sources, " ")
}

// isReadMethod reports whether a method may read a documentation resource.
func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// urlPath renders a configured path so that it is safe everywhere the
// dashboard's shell substitutes it: inside an href attribute, and inside a
// JavaScript string in the configuration block.
//
// Each segment is percent-encoded, which leaves an ordinary path such as
// "/docs" untouched and turns anything that could end an attribute or a string
// literal - a quote, an angle bracket, a backslash - into an escape that
// cannot. The separators are preserved, because the result has to remain the
// path the router matches.
func urlPath(p string) string {
	segments := strings.Split(p, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
