package muzak

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAPIEndpoint(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	rec := do(t, app, "GET", "/openapi.json")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the served document is not valid JSON: %v", err)
	}
	if doc["openapi"] != OpenAPIVersion {
		t.Errorf("openapi = %v, want %q", doc["openapi"], OpenAPIVersion)
	}
	if _, described := doc["paths"]; !described {
		t.Error("the served document has no paths")
	}
}

func TestOpenAPIEndpointConditionalRequests(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	first := do(t, app, "GET", "/openapi.json")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag was issued")
	}

	req := httptest.NewRequest("GET", "/openapi.json", nil)
	req.Header.Set("If-None-Match", etag)
	second := doRequest(t, app, req)
	assertStatus(t, second, http.StatusNotModified)
	if second.Body.Len() != 0 {
		t.Errorf("a 304 carried a body: %q", second.Body.String())
	}

	stale := httptest.NewRequest("GET", "/openapi.json", nil)
	stale.Header.Set("If-None-Match", `"0000000000000000"`)
	assertStatus(t, doRequest(t, app, stale), http.StatusOK)
}

func TestOpenAPIEndpointMethods(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	head := do(t, app, "HEAD", "/openapi.json")
	assertStatus(t, head, http.StatusOK)
	if head.Body.Len() != 0 {
		t.Errorf("HEAD returned a body: %q", head.Body.String())
	}
	if head.Header().Get("Content-Length") == "" {
		t.Error("HEAD did not report a content length")
	}

	post := do(t, app, "POST", "/openapi.json")
	assertStatus(t, post, http.StatusMethodNotAllowed)
	if got := post.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

func TestDocsPage(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	rec := do(t, app, "GET", "/docs")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "<!doctype html>") {
		t.Error("the docs page is not HTML")
	}
	if strings.Contains(body, docsTitlePlaceholder) || strings.Contains(body, docsSpecPlaceholder) {
		t.Error("a placeholder survived into the served page")
	}
	if !strings.Contains(body, "Test API") {
		t.Error("the page does not carry the API title")
	}
	if !strings.Contains(body, `href="/openapi.json"`) {
		t.Error("the page does not point at the OpenAPI document")
	}
}

// TestDocsPageIsSelfContained is the property that lets the page run under a
// strict policy and work without internet access: it loads nothing. Every
// construct a browser would fetch something for is absent, so the only
// requests the page makes are the ones its own script makes to this origin.
func TestDocsPageIsSelfContained(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())
	body := do(t, app, "GET", "/docs").Body.String()

	for _, loader := range []string{"src=", "<link", "@import", "url(", "//unpkg", "//cdn", "//fonts."} {
		if strings.Contains(body, loader) {
			t.Errorf("the docs page loads something (%q), which a strict policy would block", loader)
		}
	}
}

func TestDocsPageContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	rec := do(t, app, "GET", "/docs")
	policy := rec.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("the docs page carries no content security policy")
	}
	for _, directive := range []string{
		"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'",
		"base-uri 'none'", "form-action 'none'",
	} {
		if !strings.Contains(policy, directive) {
			t.Errorf("the policy is missing %q:\n%s", directive, policy)
		}
	}
	for _, escape := range []string{"unsafe-inline", "unsafe-eval", "unsafe-hashes"} {
		if strings.Contains(policy, escape) {
			t.Errorf("the policy allows %s, which would defeat the hashes:\n%s", escape, policy)
		}
	}

	// The page is a constant, so its policy is one too: a client may cache the
	// page and revalidate it, which a per-response nonce would forbid.
	if second := do(t, app, "GET", "/docs"); second.Header().Get("Content-Security-Policy") != policy {
		t.Error("the policy changed between responses, so the page cannot be cached")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache so the page is revalidated rather than refetched", got)
	}
}

// TestDocsPagePolicyCoversItsOwnScript checks the hashes name what the page
// actually carries. A policy whose hash did not match would leave the page
// inert in a browser while every test that only reads headers still passed.
func TestDocsPagePolicyCoversItsOwnScript(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	rec := do(t, app, "GET", "/docs")
	policy := rec.Header().Get("Content-Security-Policy")
	body := rec.Body.String()

	for _, element := range []string{"script", "style"} {
		block := inlineBlock(t, body, element)
		sum := sha256.Sum256([]byte(block))
		want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(policy, want) {
			t.Errorf("the policy does not cover the page's own %s block:\n%s", element, policy)
		}
	}
}

// inlineBlock returns the contents of the first inline block of one kind,
// which is what the policy hashes.
func inlineBlock(t *testing.T, page, element string) string {
	t.Helper()
	_, after, found := strings.Cut(page, "<"+element)
	if !found {
		t.Fatalf("the page carries no <%s> block", element)
	}
	_, body, found := strings.Cut(after, ">")
	if !found {
		t.Fatalf("the <%s> tag is not closed", element)
	}
	block, _, found := strings.Cut(body, "</"+element+">")
	if !found {
		t.Fatalf("the <%s> block is not closed", element)
	}
	return block
}

// TestDocsAreServedCompressed covers the compressed representation prepared at
// start-up: a client that accepts gzip gets fewer bytes, an entity tag of its
// own, and a Vary header so that a cache never hands one representation to a
// client that asked for the other.
func TestDocsAreServedCompressed(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	for _, path := range []string{"/docs", "/openapi.json"} {
		plain := do(t, app, "GET", path)
		assertStatus(t, plain, http.StatusOK)

		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		packed := doRequest(t, app, req)
		assertStatus(t, packed, http.StatusOK)

		if got := packed.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("%s: Content-Encoding = %q, want gzip", path, got)
		}
		if packed.Body.Len() >= plain.Body.Len() {
			t.Errorf("%s: the compressed body is not smaller: %d >= %d",
				path, packed.Body.Len(), plain.Body.Len())
		}
		if got := packed.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
			t.Errorf("%s: Vary = %q, want it to name Accept-Encoding", path, got)
		}
		if packed.Header().Get("ETag") == plain.Header().Get("ETag") {
			t.Errorf("%s: both representations carry the same entity tag", path)
		}

		reader, err := gzip.NewReader(bytes.NewReader(packed.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: the compressed body is not gzip: %v", path, err)
		}
		decoded, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("%s: the compressed body does not decode: %v", path, err)
		}
		if !bytes.Equal(decoded, plain.Body.Bytes()) {
			t.Errorf("%s: the compressed body decodes to something else", path)
		}
	}
}

// TestDocsCompressedRepresentationRevalidates checks the conditional request a
// browser makes on its second visit, which is the one that has to match the
// representation it holds rather than the other one.
func TestDocsCompressedRepresentationRevalidates(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	first := httptest.NewRequest("GET", "/docs", nil)
	first.Header.Set("Accept-Encoding", "gzip")
	rec := doRequest(t, app, first)
	etag := rec.Header().Get("ETag")

	again := httptest.NewRequest("GET", "/docs", nil)
	again.Header.Set("Accept-Encoding", "gzip")
	again.Header.Set("If-None-Match", etag)
	assertStatus(t, doRequest(t, app, again), http.StatusNotModified)

	// The same tag against the uncompressed representation is a different
	// document, and has to be answered with one.
	plain := httptest.NewRequest("GET", "/docs", nil)
	plain.Header.Set("If-None-Match", etag)
	assertStatus(t, doRequest(t, app, plain), http.StatusOK)
}

// TestDocsRefuseGzipWhenTheClientDoes covers a client that names gzip only to
// reject it.
func TestDocsRefuseGzipWhenTheClientDoes(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	req := httptest.NewRequest("GET", "/docs", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0")
	rec := doRequest(t, app, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want the body sent as it is", got)
	}
}

func TestMatchesETag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		header, etag string
		want         bool
	}{
		{"", `"abc"`, false},
		{`"abc"`, `"abc"`, true},
		{`W/"abc"`, `"abc"`, true},
		{`"other", "abc"`, `"abc"`, true},
		{"*", `"abc"`, true},
		{`"abc"`, `"abc-gzip"`, false},
		{`"nope"`, `"abc"`, false},
	}
	for _, tc := range tests {
		if got := matchesETag(tc.header, tc.etag); got != tc.want {
			t.Errorf("matchesETag(%q, %q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}

func TestInlineHashes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, page string
		want       int
	}{
		{"every block, attributes and all", "<style media=all>a{}</style><p>x</p><style>b{}</style>", 2},
		{"no block of that kind", "<p>nothing here</p>", 0},
		{"an opening tag left unclosed", "<style media=all", 0},
		{"a block left unclosed", "<style>a{}", 0},
		{"one block closed, the next not", "<style>a{}</style><style>b{}", 1},
	}
	for _, tc := range tests {
		got := inlineHashes(tc.page, "style")
		if count := strings.Count(got, "'sha256-"); count != tc.want {
			t.Errorf("%s: inlineHashes(%q) = %q, want %d hashes", tc.name, tc.page, got, tc.want)
		}
		if tc.want == 0 && got != "'none'" {
			t.Errorf("%s: inlineHashes(%q) = %q, want 'none'", tc.name, tc.page, got)
		}
	}
}

// TestAssetSkipsCompressionThatWouldNotHelp covers the body small or dense
// enough that gzip makes it longer, which is sent as it is.
func TestAssetSkipsCompressionThatWouldNotHelp(t *testing.T) {
	t.Parallel()
	as := newAsset("text/plain", []byte("no"))
	if as.gzip != nil {
		t.Errorf("a two-byte body was kept compressed at %d bytes", len(as.gzip))
	}

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	as.serve(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none", got)
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want none for a body with one representation", got)
	}
}

func TestDocsPageMethods(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	head := do(t, app, "HEAD", "/docs")
	assertStatus(t, head, http.StatusOK)
	if head.Body.Len() != 0 {
		t.Errorf("HEAD returned a body: %q", head.Body.String())
	}

	post := do(t, app, "POST", "/docs")
	assertStatus(t, post, http.StatusMethodNotAllowed)
}

func TestDocsCanBeDisabled(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.DisableDocs = true
	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/docs"), http.StatusNotFound)
	assertStatus(t, do(t, app, "GET", "/openapi.json"), http.StatusNotFound)
	assertStatus(t, do(t, app, "GET", "/x"), http.StatusOK)
}

func TestDocsPathsAreConfigurable(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.DocsPath = "/reference"
	opts.OpenAPIPath = "/schema.json"

	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/reference"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/schema.json"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/docs"), http.StatusNotFound)
	assertStatus(t, do(t, app, "GET", "/openapi.json"), http.StatusNotFound)
}

// TestDocsRoutesDoNotShadowApplicationRoutes checks that a route registered at
// the documentation path still wins, since it was declared deliberately.
func TestDocsPathsPassThroughToOtherRoutes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/other", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/other"), http.StatusOK)
}

func TestEscapeHTML(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"<script>", "&lt;script&gt;"},
		{`a"b`, "a&quot;b"},
		{"a'b", "a&#39;b"},
		{"a&b", "a&amp;b"},
	}
	for _, tc := range tests {
		if got := escapeHTML(tc.in); got != tc.want {
			t.Errorf("escapeHTML(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDocsTitleIsEscaped checks that a configured title cannot break out of the
// markup it is substituted into.
func TestDocsTitleIsEscaped(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Title = `</title><script>alert(1)</script>`
	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	body := do(t, app, "GET", "/docs").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("the title escaped its context:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the title was not escaped")
	}
}

func TestIsReadMethod(t *testing.T) {
	t.Parallel()
	for method, want := range map[string]bool{
		"GET": true, "HEAD": true, "POST": false, "PUT": false, "DELETE": false,
	} {
		if got := isReadMethod(method); got != want {
			t.Errorf("isReadMethod(%q) = %v, want %v", method, got, want)
		}
	}
}

// TestDocsAreOmittedWhenTheDocumentCannotBeRendered covers the guard that
// leaves the documentation routes unregistered rather than serving a broken
// page.
//
// The document is built from Muzak's own types and cannot normally fail to
// render, so the test poisons the generated schema with a value JSON has no
// representation for.
func TestDocsAreOmittedWhenTheDocumentCannotBeRendered(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	app.spec.Components = &Components{Schemas: map[string]*Schema{
		"poisoned": {Default: make(chan int)},
	}}

	if assets := app.prepareDocs(); assets != nil {
		t.Fatal("prepareDocs returned assets for a document that cannot be rendered")
	}
	if !strings.Contains(logs.String(), "could not be rendered") {
		t.Errorf("the failure was not reported:\n%s", logs.String())
	}

	// With no assets, the documentation paths fall through to the routes.
	handler := app.withDocs(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/openapi.json", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want the request to pass through to the next handler", rec.Code)
	}
}

// TestDocumentationIsAnnouncedAtStartUp covers the line a reader looks for in
// the terminal: where the documentation is, as a URL that can be opened.
func TestDocumentationIsAnnouncedAtStartUp(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.Addr = "127.0.0.1:0"
	opts.DocsPath = "/reference"
	opts.OpenAPIPath = "/reference/openapi.json"

	app := New(opts)
	app.Get("/x", okHandler)
	runAndStop(t, app)

	written := logs.String()
	if !strings.Contains(written, "Documentation at http://127.0.0.1:") ||
		!strings.Contains(written, "/reference") {
		t.Errorf("the configured documentation path was not announced:\n%s", written)
	}
	if !strings.Contains(written, "/reference/openapi.json") {
		t.Errorf("the OpenAPI document was not announced:\n%s", written)
	}
}

// TestDisabledDocumentationIsNotAnnounced checks that an application which
// describes nothing says nothing about it either.
func TestDisabledDocumentationIsNotAnnounced(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.Addr = "127.0.0.1:0"
	opts.DisableDocs = true

	app := New(opts)
	app.Get("/x", okHandler)
	runAndStop(t, app)

	if strings.Contains(logs.String(), "Documentation at") {
		t.Errorf("documentation was announced although it is disabled:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "AppOptions.DisableDocs") {
		t.Errorf("nothing recorded why the documentation is missing:\n%s", logs.String())
	}
}

// runAndStop starts an application on an ephemeral port and shuts it down as
// soon as it is listening, which is enough to exercise what start-up reports.
func runAndStop(t *testing.T, app *App) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()

	deadline := time.After(5 * time.Second)
	for app.Addr() == "" {
		select {
		case err := <-done:
			t.Fatalf("the application stopped before it listened: %v", err)
		case <-deadline:
			t.Fatal("the application never started listening")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunContext = %v", err)
	}
}

func TestBrowsableURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		scheme, addr, path, want string
	}{
		{"http", "[::]:8080", "/docs", "http://localhost:8080/docs"},
		{"http", "0.0.0.0:8080", "/docs", "http://localhost:8080/docs"},
		{"https", "127.0.0.1:443", "/docs", "https://127.0.0.1:443/docs"},
		{"http", "[::1]:8080", "/docs", "http://[::1]:8080/docs"},
		{"http", "example.internal:80", "/openapi.json", "http://example.internal:80/openapi.json"},
	}
	for _, tc := range tests {
		if got := browsableURL(tc.scheme, tc.addr, tc.path); got != tc.want {
			t.Errorf("browsableURL(%q, %q, %q) = %q, want %q",
				tc.scheme, tc.addr, tc.path, got, tc.want)
		}
	}
}

// TestDocsPathsAreValidated covers each way a configured path cannot work,
// every one of which is a build error rather than a page nobody can reach.
func TestDocsPathsAreValidated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configure  func(*AppOptions)
		route      string
		wantErrors []string
	}{
		{
			name:       "a documentation path that is not absolute",
			configure:  func(o *AppOptions) { o.DocsPath = "docs" },
			wantErrors: []string{"AppOptions.DocsPath is \"docs\"", "not an absolute path", `"/docs"`},
		},
		{
			name:       "an OpenAPI path that is not absolute",
			configure:  func(o *AppOptions) { o.OpenAPIPath = "openapi.json" },
			wantErrors: []string{"AppOptions.OpenAPIPath", "not an absolute path"},
		},
		{
			name:       "both documents at one path",
			configure:  func(o *AppOptions) { o.OpenAPIPath = "/docs" },
			wantErrors: []string{"are both \"/docs\"", "an address each"},
		},
		{
			name:       "a route already answering the documentation path",
			route:      "/docs",
			wantErrors: []string{"AppOptions.DocsPath is \"/docs\"", "a route of this application already answers"},
		},
		{
			name:       "a route already answering the OpenAPI path",
			route:      "/openapi.json",
			wantErrors: []string{"AppOptions.OpenAPIPath", "a route of this application already answers"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			if tc.configure != nil {
				tc.configure(&opts)
			}
			app := New(opts)
			app.Get("/x", okHandler)
			if tc.route != "" {
				app.Get(tc.route, okHandler)
			}
			message := buildError(t, app)
			for _, want := range tc.wantErrors {
				if !strings.Contains(message, want) {
					t.Errorf("the build error does not mention %q:\n%s", want, message)
				}
			}
		})
	}
}

// TestDocsPathsMayCollideWhenDocumentationIsDisabled checks that an
// application serving no documentation is free to use those paths itself.
func TestDocsPathsMayCollideWhenDocumentationIsDisabled(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.DisableDocs = true

	app := New(opts)
	app.Get("/docs", okHandler)
	app.Get("/openapi.json", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/docs"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/openapi.json"), http.StatusOK)
}
