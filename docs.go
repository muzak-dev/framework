package muzak

import (
	"bytes"
	"compress/gzip"
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
// from a third party, and the only requests it makes are for this
// application's own OpenAPI document and, from the request console, this
// application's own routes. That is what lets the page be served under a
// strict content security policy, and it means an air-gapped deployment gets
// working documentation with no further setup.
//
//go:embed docs.html
var docsPage string

// Placeholders substituted into the embedded page when the application is
// built.
const (
	docsTitlePlaceholder = "__TITLE__"
	docsSpecPlaceholder  = "__SPEC__"
)

// docsAssets holds everything the documentation routes serve, prepared once
// when the application is built.
type docsAssets struct {
	spec *asset
	page *asset
}

// prepareDocs renders the OpenAPI document and the documentation page. It
// returns nil when the document cannot be rendered, which leaves the routes
// unregistered rather than serving a broken page.
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
	page := strings.ReplaceAll(docsPage, docsTitlePlaceholder, escapeHTML(a.spec.Info.Title))
	page = strings.ReplaceAll(page, docsSpecPlaceholder, escapeHTML(a.opts.OpenAPIPath))

	assets := &docsAssets{
		spec: newAsset("application/json; charset=utf-8", spec),
		page: newAsset("text/html; charset=utf-8", []byte(page)),
	}
	assets.page.policy = contentSecurityPolicy(page)
	return assets
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
			assets.spec.serve(w, r)
		case docsPath:
			assets.page.serve(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
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
	policy   string
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
	header.Set("Cache-Control", "no-cache")
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
// under. It permits this page's own inline script and stylesheet, each
// identified by the hash of its contents, and only same-origin network access.
// Nothing else the page could be made to load will execute, and the request
// console can reach this application and nothing else.
//
// The hashes are computed here, when the application is built, rather than
// stood in for by a nonce issued per response. That is what makes the page a
// constant: it is compressed once, a client may cache it and revalidate with
// an entity tag, and serving it needs neither randomness nor rewriting.
func contentSecurityPolicy(page string) string {
	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + inlineHashes(page, "script"),
		"style-src " + inlineHashes(page, "style"),
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
