package muzak

import (
	"net/http"
	"strings"
	"testing"
)

// TestDocsRunApplicationGuards is the regression test for documentation served
// ahead of routing: an application-wide guard refused every route and still
// let anyone read the OpenAPI document describing them, internal paths and
// break-glass headers included.
func TestDocsRunApplicationGuards(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.DocsUI = nil
	app := New(opts, WithDependencies(RequireBearerToken("s3cret")))
	type purgeIn struct {
		Tenant string `path:"tenant"`
		Key    string `header:"X-Internal-Break-Glass"`
	}
	app.Delete("/internal/tenants/{tenant}/purge", func(*Context, purgeIn) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	assertStatus(t, do(t, app, "DELETE", "/internal/tenants/acme/purge"), http.StatusUnauthorized)
	for _, target := range []string{"/openapi.json", "/openapi%2Ejson"} {
		rec := do(t, app, "GET", target)
		assertStatus(t, rec, http.StatusUnauthorized)
		if strings.Contains(rec.Body.String(), "purge") {
			t.Fatalf("GET %s without a token leaked the document: %s", target, rec.Body.String())
		}
		// The refusal is the one a route gives, in the standard envelope.
		if got := decodeError(t, rec); got.Error.Code == "" {
			t.Errorf("GET %s answered without the error envelope", target)
		}
	}

	rec := doRequest(t, app, withToken("/openapi.json"))
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "/internal/tenants/{tenant}/purge") {
		t.Error("the authorized document does not describe the application")
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Errorf("Cache-Control = %q, want a guarded document kept out of shared caches", got)
	}
}

// TestDocsUIRunsApplicationProviders covers the page and its assets, and the
// providers declared on the application as well as its guards.
func TestDocsUIRunsApplicationProviders(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(currentMountUser))
	app.Get("/me", func(ctx *Context, _ Empty) (mountUser, error) { return From[mountUser](ctx), nil })
	mustBuild(t, app)

	for _, target := range []string{"/docs", "/docs/", "/docs/_nuxt/app.js", "/openapi.json", "/me"} {
		assertStatus(t, do(t, app, "GET", target), http.StatusUnauthorized)
		assertStatus(t, doRequest(t, app, withToken(target)), http.StatusOK)
	}

	failing := New(quietOptions(), Singleton(func(*Context) (mountTenant, error) {
		return mountTenant{}, NewHTTPError(http.StatusServiceUnavailable, "directory unavailable")
	}))
	mustBuild(t, failing)
	rec := do(t, failing, "GET", "/docs")
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if got := decodeError(t, rec); got.Error.Message != "directory unavailable" {
		t.Errorf("message = %q, want the provider's error rendered as a route's would be", got.Error.Message)
	}
}

// TestDocsIgnoreRouterGuards pins the other side: a guard declared on an
// included router protects that router, not the document describing every
// router, which is how an application keeps its documentation public while
// its API stays private. Such documentation stays cacheable.
func TestDocsIgnoreRouterGuards(t *testing.T) {
	t.Parallel()
	api := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	api.Get("/things", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app := New(quietOptions())
	app.Include(api, WithPrefix("/api"))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/api/things"), http.StatusUnauthorized)
	rec := do(t, app, "GET", "/openapi.json")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q for public documentation", got, "no-cache")
	}
	assertStatus(t, do(t, app, "GET", "/docs"), http.StatusOK)
}
