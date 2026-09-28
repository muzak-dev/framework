package muzak

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// docsGuess sends one request for target from a fixed client, carrying token
// as a bearer credential when it is not empty.
func docsGuess(t *testing.T, app *App, target, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "192.0.2.51:1"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doRequest(t, app, req).Code
}

// TestDocsCountAgainstApplicationRateLimit is the regression test for
// documentation served outside the application-wide rate limit that covered
// every route and file mount beside it. The page, its assets and the document
// all count, and they count against the same budget as the routes.
func TestDocsCountAgainstApplicationRateLimit(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{Quotas: []Quota{{Name: "global", Window: time.Hour, Limit: 2}}}
	app := New(opts, WithDependencies(func(*Context) error { return nil }))
	app.Get("/x", okHandler)
	app.Frontend("/files", FrontendOptions{FS: fstest.MapFS{"a.txt": {Data: []byte("a")}}, NoFallback: true})
	mustBuild(t, app)

	for _, target := range []string{"/x", "/files/a.txt", "/openapi.json", "/docs", "/docs/_nuxt/app.js"} {
		var codes []int
		for range 4 {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.RemoteAddr = "192.0.2.60:1"
			codes = append(codes, doRequest(t, app, req).Code)
		}
		// The first path spends the budget; everything after it is refused.
		if codes[3] != http.StatusTooManyRequests {
			t.Errorf("GET %s from a client past its budget: %v, want the last refused with 429", target, codes)
		}
	}

	// The limit is announced exactly as a route's is.
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	req.RemoteAddr = "192.0.2.61:1"
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	if rec.Header().Get("RateLimit-Policy") == "" {
		t.Error("the documentation carries no RateLimit-Policy header")
	}
}

// TestDocsAreNotAnUnthrottledGuardOracle pins the consequence the review
// demonstrated: once a client has spent its budget guessing a token through
// the routes, the documentation does not answer its guesses either.
func TestDocsAreNotAnUnthrottledGuardOracle(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{Quotas: []Quota{{Name: "global", Window: time.Hour, Limit: 5}}}
	app := New(opts, WithDependencies(RequireBearerToken("s3")))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	for i := range 5 {
		if code := docsGuess(t, app, "/x", fmt.Sprintf("wrong%d", i)); code != http.StatusUnauthorized {
			t.Fatalf("guess %d through the route = %d, want 401", i, code)
		}
	}
	for i := range 10 {
		if code := docsGuess(t, app, "/openapi.json", fmt.Sprintf("s%d", i)); code != http.StatusTooManyRequests {
			t.Fatalf("guess s%d through the documentation = %d, want 429 once the budget is spent", i, code)
		}
	}
}

// TestDocsRateLimitAfterDependencies covers a limit declared to count only
// the requests the guards let through, which the documentation honours as a
// route does.
func TestDocsRateLimitAfterDependencies(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{
		Quotas:            []Quota{{Name: "global", Window: time.Hour, Limit: 1}},
		AfterDependencies: true,
	}
	app := New(opts, WithDependencies(RequireBearerToken("s3")))
	mustBuild(t, app)

	for range 3 {
		if code := docsGuess(t, app, "/openapi.json", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("a refused request = %d, want 401 and not counted", code)
		}
	}
	if code := docsGuess(t, app, "/openapi.json", "s3"); code != http.StatusOK {
		t.Fatalf("the first admitted request = %d, want 200", code)
	}
	if code := docsGuess(t, app, "/openapi.json", "s3"); code != http.StatusTooManyRequests {
		t.Fatalf("the second admitted request = %d, want 429", code)
	}
}

// TestDocsRateLimitWithoutGuards covers an application with a rate limit and
// no guards: the documentation is counted but stays cacheable, since every
// client that is answered is answered the same document, and an application
// exempted with SkipRateLimit leaves it uncounted.
func TestDocsRateLimitWithoutGuards(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{Quotas: []Quota{{Name: "global", Window: time.Hour, Limit: 1}}}
	app := New(opts)
	mustBuild(t, app)
	rec := do(t, app, "GET", "/openapi.json")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q for unguarded documentation", got, "no-cache")
	}
	assertStatus(t, do(t, app, "GET", "/openapi.json"), http.StatusTooManyRequests)

	exempt := New(opts, SkipRateLimit())
	mustBuild(t, exempt)
	for range 3 {
		assertStatus(t, do(t, exempt, "GET", "/openapi.json"), http.StatusOK)
	}
}

// TestDocsRateLimitMisconfigured covers a limit the documentation cannot
// enforce, reported against the documentation when no route carries it.
func TestDocsRateLimitMisconfigured(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{Quotas: []Quota{{Name: "global", Window: time.Hour}}}
	msg := buildError(t, New(opts))
	if !strings.Contains(msg, "documentation /openapi.json") || !strings.Contains(msg, "positive limit") {
		t.Errorf("build error = %q, want the documentation named with the quota problem", msg)
	}
}
