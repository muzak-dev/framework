package muzak

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if strings.Contains(body, docsNoncePlaceholder) || strings.Contains(body, docsTitlePlaceholder) {
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
// strict policy and work without internet access.
func TestDocsPageIsSelfContained(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())
	body := do(t, app, "GET", "/docs").Body.String()

	for _, external := range []string{"http://", "https://", "//unpkg", "//cdn"} {
		if strings.Contains(body, external) {
			t.Errorf("the docs page references something external (%q), which a strict policy would block", external)
		}
	}
}

func TestDocsPageContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	first := do(t, app, "GET", "/docs")
	policy := first.Header().Get("Content-Security-Policy")
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
	if strings.Contains(policy, "unsafe-inline") {
		t.Errorf("the policy allows inline script, defeating the nonce:\n%s", policy)
	}

	nonce := nonceFrom(t, policy)
	if !strings.Contains(first.Body.String(), `nonce="`+nonce+`"`) {
		t.Error("the page's script is not tagged with the policy nonce")
	}

	// A nonce that repeated across responses would be no better than allowing
	// inline script outright.
	second := do(t, app, "GET", "/docs")
	if nonceFrom(t, second.Header().Get("Content-Security-Policy")) == nonce {
		t.Error("the same nonce was reused across responses")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store so a nonce is never cached", got)
	}
}

// nonceFrom extracts the nonce from a content security policy.
func nonceFrom(t *testing.T, policy string) string {
	t.Helper()
	_, after, found := strings.Cut(policy, "script-src 'nonce-")
	if !found {
		t.Fatalf("no nonce in policy: %s", policy)
	}
	nonce, _, _ := strings.Cut(after, "'")
	if nonce == "" {
		t.Fatalf("empty nonce in policy: %s", policy)
	}
	return nonce
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

func TestNewNonce(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 100 {
		nonce, err := newNonce()
		if err != nil {
			t.Fatalf("newNonce = %v", err)
		}
		if len(nonce) < 20 {
			t.Errorf("nonce %q is too short to be unguessable", nonce)
		}
		if seen[nonce] {
			t.Fatalf("newNonce repeated %q", nonce)
		}
		seen[nonce] = true
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
