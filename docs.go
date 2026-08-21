package badele

import (
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// docsPage is the documentation UI, embedded so that the binary serves it
// without reading anything from disk at run time.
//
// It is a self-contained page: no script, stylesheet, font or image is fetched
// from a third party, and the only request it makes is for this application's
// own OpenAPI document. That is what lets the page be served under a strict
// content security policy, and it means an air-gapped deployment gets working
// documentation with no further setup.
//
//go:embed docs.html
var docsPage string

// Placeholders substituted into the embedded page when it is served.
const (
	docsTitlePlaceholder = "__TITLE__"
	docsNoncePlaceholder = "__NONCE__"
	docsSpecPlaceholder  = "__SPEC__"
)

// docsAssets holds everything the documentation routes serve, prepared once
// when the application is built.
type docsAssets struct {
	spec     []byte
	specETag string
	page     string
}

// prepareDocs renders the OpenAPI document and the documentation page. It
// returns nil when the document cannot be rendered, which leaves the routes
// unregistered rather than serving a broken page.
func (a *App) prepareDocs() *docsAssets {
	spec, err := a.spec.Marshal()
	if err != nil {
		// coverage: Document is built from Badele's own types, every one of
		// which is JSON-encodable, so marshaling cannot fail. The branch keeps
		// a future schema field from taking down start-up.
		Scoped(a.logger, ScopeDocs).Error("badele: the OpenAPI document could not be rendered",
			slog.String("error", err.Error()))
		return nil
	}
	sum := sha256.Sum256(spec)
	page := strings.ReplaceAll(docsPage, docsTitlePlaceholder, escapeHTML(a.spec.Info.Title))
	page = strings.ReplaceAll(page, docsSpecPlaceholder, escapeHTML(a.opts.OpenAPIPath))
	return &docsAssets{
		spec:     spec,
		specETag: `"` + hex.EncodeToString(sum[:8]) + `"`,
		page:     page,
	}
}

// withDocs intercepts the documentation paths and delegates everything else to
// the application's routes.
func (a *App) withDocs(next http.Handler) http.Handler {
	assets := a.prepareDocs()
	if assets == nil {
		return next
	}
	specPath := a.opts.OpenAPIPath
	docsPath := a.opts.DocsPath

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case specPath:
			assets.serveSpec(w, r)
		case docsPath:
			assets.servePage(w, r)
		default:
			next.ServeHTTP(w, r)
			return
		}
	})
}

// serveSpec writes the OpenAPI document, honouring conditional requests so
// that a documentation page reloaded repeatedly transfers the body once.
func (d *docsAssets) serveSpec(w http.ResponseWriter, r *http.Request) {
	if !isReadMethod(r.Method) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("ETag", d.specETag)
	header.Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, d.specETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Length", strconv.Itoa(len(d.spec)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(d.spec)
}

// servePage writes the documentation UI under a content security policy that
// permits only this page's own inline script and stylesheet, identified by a
// per-response nonce, and only same-origin network access. Nothing else the
// page could be made to load will execute.
func (d *docsAssets) servePage(w http.ResponseWriter, r *http.Request) {
	if !isReadMethod(r.Method) {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nonce, err := newNonce()
	if err != nil {
		// coverage: crypto/rand.Read does not fail on any supported platform;
		// serving the page without a nonce would defeat the policy, so the
		// request fails instead.
		http.Error(w, "documentation is unavailable", http.StatusInternalServerError)
		return
	}
	body := strings.ReplaceAll(d.page, docsNoncePlaceholder, nonce)

	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src 'nonce-" + nonce + "'",
		"style-src 'nonce-" + nonce + "'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; "))
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(body))
}

// isReadMethod reports whether a method may read a documentation resource.
func isReadMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// newNonce returns a fresh base64 nonce for the content security policy. A new
// one is generated per response, because a reused nonce is no better than
// allowing inline script outright.
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// coverage: crypto/rand.Read does not fail on any supported platform,
		// and the caller turns a failure into a 500 rather than serving the
		// page without a usable policy.
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// escapeHTML escapes the characters that would let a configured title or path
// break out of the markup it is substituted into.
func escapeHTML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	).Replace(s)
}
