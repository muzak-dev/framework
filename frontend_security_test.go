package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type mountUser struct{ Name string }

type mountTenant struct{ Owner string }

// currentMountUser is the per-user authentication the framework recommends: a
// provider that hands the handler the user, or refuses the request.
func currentMountUser(ctx *Context) (mountUser, error) {
	if ctx.Header("Authorization") != "Bearer s3cret" {
		return mountUser{}, NewHTTPError(http.StatusUnauthorized, "login required")
	}
	return mountUser{Name: "alice"}, nil
}

// withToken returns a GET request carrying the credential currentMountUser
// accepts.
func withToken(target string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	return req
}

// TestMountResolvesInheritedProviders is the regression test for a mount that
// ran its routers' guards and nothing else: a router authenticated with Needs
// served its files to anyone, while the routes beside them answered 401.
func TestMountResolvesInheritedProviders(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"index.html":           {Data: []byte("<h1>admin console</h1>")},
		"reports/q3-2026.json": {Data: []byte(`{"revenue":"confidential"}`)},
	}
	admin := NewRouter(Needs(currentMountUser))
	admin.Get("/api/me", func(ctx *Context, _ Empty) (mountUser, error) {
		return From[mountUser](ctx), nil
	})
	admin.Frontend("/", FrontendOptions{FS: files})
	admin.Static("/raw", StaticOptions{FS: files})

	app := New(quietOptions())
	app.Include(admin, WithPrefix("/admin"))
	mustBuild(t, app)

	for _, target := range []string{"/admin/api/me", "/admin/", "/admin/reports/q3-2026.json", "/admin/raw/reports/q3-2026.json", "/admin/missing"} {
		rec := do(t, app, "GET", target)
		assertStatus(t, rec, http.StatusUnauthorized)
		if strings.Contains(rec.Body.String(), "confidential") || strings.Contains(rec.Body.String(), "console") {
			t.Fatalf("GET %s leaked the file: %s", target, rec.Body.String())
		}
	}
	for _, target := range []string{"/admin/", "/admin/reports/q3-2026.json", "/admin/raw/reports/q3-2026.json"} {
		assertStatus(t, doRequest(t, app, withToken(target)), http.StatusOK)
	}
}

// TestMountProvidersSeeEachOther checks that a mount records what it resolves,
// so a provider can build on an earlier one exactly as it would for a route,
// and that a singleton's error refuses the request too.
func TestMountProvidersSeeEachOther(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"a.txt": {Data: []byte("tenant file")}}

	owned := NewRouter(
		Needs(currentMountUser),
		Needs(func(ctx *Context) (mountTenant, error) {
			user := From[mountUser](ctx)
			if user.Name != "alice" {
				return mountTenant{}, NewHTTPError(http.StatusForbidden, "not your tenant")
			}
			return mountTenant{Owner: user.Name}, nil
		}),
	)
	owned.Static("/", StaticOptions{FS: files})

	broken := NewRouter(Singleton(func(*Context) (mountTenant, error) {
		return mountTenant{}, NewHTTPError(http.StatusServiceUnavailable, "tenant directory unavailable")
	}))
	broken.Static("/", StaticOptions{FS: files})

	app := New(quietOptions())
	app.Include(owned, WithPrefix("/owned"))
	app.Include(broken, WithPrefix("/broken"))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/owned/a.txt"), http.StatusUnauthorized)
	assertStatus(t, doRequest(t, app, withToken("/owned/a.txt")), http.StatusOK)
	rec := do(t, app, "GET", "/broken/a.txt")
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if got := decodeError(t, rec); got.Error.Message != "tenant directory unavailable" {
		t.Errorf("error detail = %q, want the provider's error rendered as usual", got.Error.Message)
	}
}

// staticRequest sends a GET from a fixed client address, so that every
// request in a test is counted against one budget.
func staticRequest(t *testing.T, app *App, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = "203.0.113.9:1234"
	return doRequest(t, app, req)
}

// TestMountAppliesInheritedRateLimit is the regression test for a mount that
// dropped the rate limit: an application-wide quota counted every API call
// and none of the file requests beside them. A quota a mount shares with a
// route is also one budget, not two.
func TestMountAppliesInheritedRateLimit(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"a.txt": {Data: []byte("x")}}
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{Quotas: []Quota{{Name: "global", Window: time.Minute, Limit: 2}}}
	app := New(opts)
	app.Get("/ping", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Static("/static", StaticOptions{FS: files})
	mustBuild(t, app)

	assertStatus(t, staticRequest(t, app, "/ping"), http.StatusOK)
	rec := staticRequest(t, app, "/static/a.txt")
	assertStatus(t, rec, http.StatusOK)
	if rec.Header().Get("RateLimit-Policy") == "" {
		t.Error("a file served under a quota carries no RateLimit-Policy header")
	}
	rec = staticRequest(t, app, "/static/a.txt")
	assertStatus(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a refused file request carries no Retry-After")
	}
	assertStatus(t, staticRequest(t, app, "/ping"), http.StatusTooManyRequests)
}

// TestMountRateLimitFollowsRouterOptions covers a limit declared on an
// included router, counted after the dependencies when the policy says so,
// and SkipRateLimit exempting a router's mounts the way it exempts its routes.
func TestMountRateLimitFollowsRouterOptions(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"index.html": {Data: []byte("app")}}

	after := NewRouter(
		Needs(currentMountUser),
		WithRateLimit(RateLimitOptions{
			Quotas:            []Quota{{Name: "ui", Window: time.Minute, Limit: 1}},
			AfterDependencies: true,
		}),
	)
	after.Frontend("/", FrontendOptions{FS: files})

	exempt := NewRouter(SkipRateLimit())
	exempt.Frontend("/", FrontendOptions{FS: files})

	app := New(quietOptions())
	app.Include(after, WithPrefix("/ui"))
	app.Include(exempt, WithPrefix("/public"), RateLimit(Quota{Name: "public", Window: time.Minute, Limit: 1}))
	mustBuild(t, app)

	// Counted after the dependencies, so a request the provider refuses
	// spends nothing and the first authenticated one still gets through.
	for range 3 {
		assertStatus(t, staticRequest(t, app, "/ui/"), http.StatusUnauthorized)
	}
	authed := func() int {
		req := withToken("/ui/")
		req.RemoteAddr = "203.0.113.9:1234"
		return doRequest(t, app, req).Code
	}
	if first, second := authed(), authed(); first != http.StatusOK || second != http.StatusTooManyRequests {
		t.Fatalf("authenticated requests = %d, %d; want 200 then 429", first, second)
	}
	for range 3 {
		assertStatus(t, staticRequest(t, app, "/public/"), http.StatusOK)
	}
}

// TestMountRateLimitMisconfiguration checks that a policy a mount cannot use
// fails the build, named after the mount, together with everything else.
func TestMountRateLimitMisconfiguration(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"a.txt": {Data: []byte("x")}}

	invalid := New(quietOptions(), RateLimit(Quota{Name: "zero", Window: time.Minute}))
	invalid.Static("/assets", StaticOptions{FS: files})
	msg := buildError(t, invalid)
	if !strings.Contains(msg, "static files /assets") || !strings.Contains(msg, "positive limit") {
		t.Errorf("build error = %q, want the mount named with the reason", msg)
	}

	conflict := New(quietOptions())
	conflict.Get("/ping", func(*Context, Empty) (Empty, error) { return Empty{}, nil },
		RateLimit(Quota{Name: "shared", Window: time.Minute, Limit: 5}))
	ui := NewRouter(RateLimit(Quota{Name: "shared", Window: time.Hour, Limit: 5}))
	ui.Frontend("/", FrontendOptions{FS: files, NoFallback: true})
	conflict.Include(ui, WithPrefix("/ui"))
	err := conflict.Build()
	if err == nil || !strings.Contains(err.Error(), "frontend /ui") || !strings.Contains(err.Error(), `quota "shared"`) {
		t.Errorf("build error = %v, want the conflicting quota reported against the mount", err)
	}
}
