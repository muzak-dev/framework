package muzak

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// TestMountAnswersThePrefixAndEverythingBeneathIt covers the shape of a mount:
// the prefix itself, with or without its slash, and every path and method
// beneath it, and nothing that merely begins with the same letters.
func TestMountAnswersThePrefixAndEverythingBeneathIt(t *testing.T) {
	t.Parallel()
	rec := &mountRecorder{}
	app := New(quietOptions())
	app.Mount("/debug", rec)
	mustBuild(t, app)

	for _, tc := range []struct{ method, target string }{
		{"GET", "/debug"},
		{"GET", "/debug/"},
		{"GET", "/debug/pprof/heap"},
		{"POST", "/debug/x"},
		{"DELETE", "/debug"},
		{"PROPFIND", "/debug/a/b/c"},
		{"OPTIONS", "/debug/x"},
		{"HEAD", "/debug/x"},
	} {
		res := do(t, app, tc.method, tc.target)
		if res.Code != http.StatusOK || res.Header().Get("X-Mounted") != "yes" {
			t.Errorf("%s %s = %d, want the mounted handler", tc.method, tc.target, res.Code)
			continue
		}
		if got := rec.last(t); got.method != tc.method || got.path != tc.target {
			t.Errorf("%s %s reached the handler as %s %s", tc.method, tc.target, got.method, got.path)
		}
	}
	for _, target := range []string{"/debugger", "/deb", "/", "/other/debug"} {
		assertStatus(t, do(t, app, "GET", target), http.StatusNotFound)
	}
}

// TestMountTrailingSlashIsTheSameMount shows that "/x/" and "/x" are one mount,
// which is what makes a second registration of the other spelling a conflict.
func TestMountTrailingSlashIsTheSameMount(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Mount("/debug/pprof/", &mountRecorder{})
	mustBuild(t, app)
	assertStatus(t, do(t, app, "GET", "/debug/pprof"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/debug/pprof/"), http.StatusOK)

	twice := New(quietOptions())
	twice.Mount("/debug/pprof/", &mountRecorder{})
	twice.Mount("/debug/pprof", &mountRecorder{})
	if err := twice.Build(); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("Build() = %v, want the second mount at the same prefix reported", err)
	}
}

// TestMountAtTheRootAnswersWhatNoRouteDoes is the legacy application case:
// whatever has been moved to routes is answered by them, and the rest by the
// old handler.
func TestMountAtTheRootAnswersWhatNoRouteDoes(t *testing.T) {
	t.Parallel()
	legacy := &mountRecorder{}
	app := New(quietOptions())
	app.Get("/users", func(*Context, Empty) (encodedUserOut, error) { return encodedUserOut{Route: "new"}, nil })
	app.Mount("/", legacy)
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/users"), `{"route":"new","id":""}`)
	for _, tc := range []struct{ method, target string }{
		{"POST", "/users"},
		{"GET", "/"},
		{"GET", "/orders/7"},
		{"GET", "/users/"},
	} {
		res := do(t, app, tc.method, tc.target)
		if res.Header().Get("X-Mounted") != "yes" {
			t.Errorf("%s %s = %d, want the legacy handler", tc.method, tc.target, res.Code)
		}
	}
	if legacy.last(t).route != "/" {
		t.Errorf("the root mount reports route %q, want %q", legacy.last(t).route, "/")
	}
	// The documentation is answered ahead of routing, as it is beside a
	// frontend at the root.
	if res := do(t, app, "GET", "/openapi.json"); res.Header().Get("X-Mounted") != "" {
		t.Error("the OpenAPI document was handed to the root mount")
	}
}

// TestMountUnderAnIncludedRouter resolves the prefix through every router the
// mount was registered under, exactly as a route's path is.
func TestMountUnderAnIncludedRouter(t *testing.T) {
	t.Parallel()
	rec := &mountRecorder{}
	admin := NewRouter()
	admin.Mount("/pprof", rec)
	ops := NewRouter()
	ops.Include(admin, WithPrefix("/admin"))
	app := New(quietOptions())
	app.Include(ops, WithPrefix("/ops"))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/ops/admin/pprof/heap"), http.StatusOK)
	if got := rec.last(t).route; got != "/ops/admin/pprof" {
		t.Errorf("route = %q, want the full prefix", got)
	}
	assertStatus(t, do(t, app, "GET", "/pprof/heap"), http.StatusNotFound)
}

// TestRoutesBeneathAMountKeepTheirMethods pins down the precedence a migration
// relies on: a route answers the methods it registers, with the HEAD a GET
// route answers, and the mounted handler every other method at that path.
func TestRoutesBeneathAMountKeepTheirMethods(t *testing.T) {
	t.Parallel()
	legacy := &mountRecorder{}
	app := New(quietOptions())
	app.Get("/legacy/users", func(*Context, Empty) (encodedUserOut, error) { return encodedUserOut{Route: "users"}, nil })
	app.Get("/legacy/items/{id}", func(_ *Context, in struct {
		ID string `path:"id"`
	}) (encodedUserOut, error) {
		return encodedUserOut{Route: "item", ID: in.ID}, nil
	})
	app.Get("/legacy", func(*Context, Empty) (encodedUserOut, error) { return encodedUserOut{Route: "index"}, nil })
	app.Mount("/legacy", legacy)
	mustBuild(t, app)

	routed := func(method, target string) {
		t.Helper()
		res := do(t, app, method, target)
		if res.Header().Get("X-Mounted") != "" || res.Code != http.StatusOK {
			t.Errorf("%s %s = %d (mounted %q), want the route", method, target, res.Code, res.Header().Get("X-Mounted"))
		}
	}
	mounted := func(method, target string) {
		t.Helper()
		if res := do(t, app, method, target); res.Header().Get("X-Mounted") != "yes" {
			t.Errorf("%s %s = %d, want the mounted handler", method, target, res.Code)
		}
	}
	routed("GET", "/legacy/users")
	routed("HEAD", "/legacy/users")
	routed("GET", "/legacy/items/7")
	routed("GET", "/legacy")
	mounted("POST", "/legacy/users")
	mounted("PUT", "/legacy/users")
	mounted("OPTIONS", "/legacy/users")
	mounted("DELETE", "/legacy/items/7")
	mounted("POST", "/legacy")
	mounted("GET", "/legacy/users/7")
	mounted("GET", "/legacy/items/7/parts")
	mounted("GET", "/legacy/items/")
	mounted("GET", "/legacy/")
}

// TestMountCoversRoutesByWhatTheTreeMatches checks which route paths hand
// their other methods to a mount: one spelled with escapes beneath the prefix
// does, and one whose parameter merely could hold the prefix's text does not,
// since the tree prefers the mount's static segment to the parameter.
func TestMountCoversRoutesByWhatTheTreeMatches(t *testing.T) {
	t.Parallel()
	rec := &mountRecorder{}
	app := New(quietOptions())
	app.Get("/%6Cegacy/users", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Get("/{section}/reports", func(*Context, struct {
		Section string `path:"section"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	app.Get("/le", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Get("/api", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Mount("/legacy", rec)
	app.Mount("/api/v1", rec)
	mustBuild(t, app)

	// A route above a mount's prefix is not beneath it.
	assertStatus(t, do(t, app, "POST", "/api"), http.StatusMethodNotAllowed)

	assertStatus(t, do(t, app, "GET", "/legacy/users"), http.StatusOK)
	if res := do(t, app, "POST", "/legacy/users"); res.Header().Get("X-Mounted") != "yes" {
		t.Errorf("POST /legacy/users = %d, want the mount", res.Code)
	}
	assertStatus(t, do(t, app, "POST", "/other/reports"), http.StatusMethodNotAllowed)
	if res := do(t, app, "GET", "/legacy/reports"); res.Header().Get("X-Mounted") != "yes" {
		t.Errorf("GET /legacy/reports = %d, want the mount's static prefix to win over the parameter", res.Code)
	}
	assertStatus(t, do(t, app, "POST", "/le"), http.StatusMethodNotAllowed)
}

// TestMountBesideTheDocumentation refuses a mount at a documentation path
// only when that path is served.
func TestMountBesideTheDocumentation(t *testing.T) {
	t.Parallel()
	served := New(quietOptions())
	served.Mount("/docs", &mountRecorder{})
	if err := served.Build(); err == nil || !strings.Contains(err.Error(), `mount at "/docs": the documentation is served`) {
		t.Errorf("Build() = %v, want the mount at the documentation UI reported", err)
	}

	options := quietOptions()
	options.DocsUI = nil
	noUI := New(options)
	noUI.Mount("/docs", &mountRecorder{})
	mustBuild(t, noUI)

	options = quietOptions()
	options.DisableDocs = true
	disabled := New(options)
	disabled.Mount("/openapi.json", &mountRecorder{})
	mustBuild(t, disabled)
	if res := do(t, disabled, "GET", "/openapi.json"); res.Header().Get("X-Mounted") != "yes" {
		t.Error("with the documentation disabled, the mount did not answer its path")
	}
}

// TestMountQuotaIsCheckedAsARoutes is reported against the mount.
func TestMountQuotaIsCheckedAsARoutes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Mount("/metrics", &mountRecorder{}, RateLimit(Quota{Name: "bad name", Limit: 1, Window: time.Second}))
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "muzak: mount /metrics: quota name") {
		t.Errorf("Build() = %v, want the quota reported against the mount", err)
	}
}

// TestTheMostSpecificMountWins nests three mounts and a route, and checks each
// path goes to the deepest one that covers it.
func TestTheMostSpecificMountWins(t *testing.T) {
	t.Parallel()
	root, api, v1 := &mountRecorder{}, &mountRecorder{}, &mountRecorder{}
	app := New(quietOptions())
	app.Mount("/api/v1", v1)
	app.Mount("/", root)
	app.Mount("/api", api)
	app.Get("/api/v1/health", func(*Context, Empty) (encodedUserOut, error) { return encodedUserOut{Route: "health"}, nil })
	mustBuild(t, app)

	for target, want := range map[string]*mountRecorder{
		"/":             root,
		"/apiary":       root,
		"/api":          api,
		"/api/v2/x":     api,
		"/api/v1":       v1,
		"/api/v1/users": v1,
	} {
		before := want.count()
		do(t, app, "GET", target)
		if want.count() != before+1 {
			t.Errorf("GET %s did not reach the expected mount", target)
		}
	}
	before := v1.count()
	do(t, app, "GET", "/api/v1/health")
	if v1.count() != before {
		t.Error("GET /api/v1/health reached the mount instead of its route")
	}
	if do(t, app, "POST", "/api/v1/health"); v1.count() != before+1 {
		t.Error("POST /api/v1/health went to a less specific mount than /api/v1")
	}
}

// TestAMountBeatsARootWildcardRoute shows the tree's static-before-wildcard
// rule applied to a mount: the mount's prefix is static text.
func TestAMountBeatsARootWildcardRoute(t *testing.T) {
	t.Parallel()
	rec := &mountRecorder{}
	app := New(quietOptions())
	app.Get("/{rest...}", func(_ *Context, in struct {
		Rest string `path:"rest"`
	}) (encodedUserOut, error) {
		return encodedUserOut{Route: "wildcard", ID: in.Rest}, nil
	})
	app.Mount("/debug", rec)
	mustBuild(t, app)
	if res := do(t, app, "GET", "/debug/vars"); res.Header().Get("X-Mounted") != "yes" {
		t.Errorf("GET /debug/vars = %s, want the mount", res.Body.String())
	}
	assertJSON(t, do(t, app, "GET", "/other"), `{"route":"wildcard","id":"other"}`)
}

// TestMountAndFileMountPrecedence puts a handler mount and a frontend each
// beneath the other.
func TestMountAndFileMountPrecedence(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"index.html":   &fstest.MapFile{Data: []byte("<p>app</p>")},
		"logo.svg":     &fstest.MapFile{Data: []byte("<svg/>")},
		"api/data.txt": &fstest.MapFile{Data: []byte("frontend data")},
	}

	t.Run("frontend beneath a root mount", func(t *testing.T) {
		t.Parallel()
		legacy := &mountRecorder{}
		app := New(quietOptions())
		app.Mount("/", legacy)
		app.Frontend("/app", FrontendOptions{FS: files, NoFallback: true})
		mustBuild(t, app)

		res := do(t, app, "GET", "/app/logo.svg")
		if res.Code != http.StatusOK || res.Body.String() != "<svg/>" {
			t.Errorf("GET /app/logo.svg = %d %q, want the frontend's file", res.Code, res.Body.String())
		}
		// A spelling the frontend's filesystem may read as its own is refused,
		// not handed to the less specific mount, as frontendFor does.
		for _, target := range []string{"/APP/logo.svg", "/App"} {
			if res := do(t, app, "GET", target); res.Code != http.StatusNotFound || res.Header().Get("X-Mounted") != "" {
				t.Errorf("GET %s = %d mounted=%q, want 404 from neither", target, res.Code, res.Header().Get("X-Mounted"))
			}
		}
		if res := do(t, app, "GET", "/elsewhere"); res.Header().Get("X-Mounted") != "yes" {
			t.Errorf("GET /elsewhere = %d, want the root mount", res.Code)
		}
	})

	t.Run("mount beneath a root frontend", func(t *testing.T) {
		t.Parallel()
		api := &mountRecorder{}
		app := New(quietOptions())
		app.Frontend("/", FrontendOptions{FS: files, NoFallback: true})
		app.Mount("/api", api)
		mustBuild(t, app)

		if res := do(t, app, "GET", "/api/data.txt"); res.Header().Get("X-Mounted") != "yes" {
			t.Errorf("GET /api/data.txt = %q, want the mount over the frontend's file", res.Body.String())
		}
		if res := do(t, app, "GET", "/logo.svg"); res.Body.String() != "<svg/>" {
			t.Errorf("GET /logo.svg = %d, want the frontend", res.Code)
		}
	})
}

// TestMountBuildErrors lists every misconfiguration a mount can have, and
// checks all of them are reported by one build.
func TestMountBuildErrors(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}}
	h := &mountRecorder{}

	app := New(quietOptions())
	app.Mount("/twice", h)
	app.Mount("/twice", h)
	app.Mount("/app", h)
	app.Frontend("/app", FrontendOptions{FS: files})
	app.Mount("/assets", h)
	app.Static("/assets", StaticOptions{FS: files})
	app.Mount("relative", h)
	app.Mount("/a//b", h)
	app.Mount("/a/./b", h)
	app.Mount("/a/../b", h)
	app.Mount("/a/{id}", h)
	app.Mount("/files/{rest...}", h)
	app.Mount("/a%2Fb", h)
	app.Mount("/a b", h)
	app.Mount("/caf\u00e9", h)
	app.Mount("/nil", nil)
	app.Mount("/openapi.json", h)
	app.Get("/legacy/{rest...}", func(*Context, struct {
		Rest string `path:"rest"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	app.Mount("/legacy", h)
	tenant := NewRouter()
	tenant.Mount("/x", h)
	app.Include(tenant, WithPrefix("/{tenant}"))

	err := app.Build()
	if err == nil {
		t.Fatal("Build() succeeded, want every mount mistake reported")
	}
	for _, want := range []string{
		`mount at "/twice": a handler is mounted at this prefix more than once`,
		`mount at "/app": frontend are mounted at the same prefix`,
		`mount at "/assets": static files are mounted at the same prefix`,
		`mount at "relative": the prefix must begin with "/"`,
		`mount at "/a//b": the prefix holds an empty segment`,
		`mount at "/a/./b": the prefix holds a dot segment`,
		`mount at "/a/../b": the prefix holds a dot segment`,
		`mount at "/a/{id}": the prefix holds a parameter`,
		`mount at "/files/{rest...}": the prefix holds a parameter`,
		`mount at "/a%2Fb": the prefix holds "%"`,
		`mount at "/a b": the prefix holds " "`,
		"mount at \"/caf\u00e9\": the prefix holds a character outside ASCII",
		`mount at "/nil": the handler is nil`,
		`mount at "/openapi.json": the documentation is served at this path`,
		`mount at "/legacy": a route whose wildcard sits directly beneath this prefix`,
		`mount at "/{tenant}/x": the prefix holds a parameter`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the build error does not mention %q:\n%v", want, err)
		}
	}
}

// TestStripPrefixOutsideAMountIsABuildError keeps the option from doing
// nothing quietly where it cannot apply.
func TestStripPrefixOutsideAMountIsABuildError(t *testing.T) {
	t.Parallel()
	for name, configure := range map[string]func(*App){
		"NewRouter": func(app *App) {
			r := NewRouter(StripPrefix())
			r.Get("/x", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
			app.Include(r)
		},
		"Include": func(app *App) {
			app.Include(NewRouter(), WithPrefix("/p"), StripPrefix())
		},
		"Options": func(app *App) { app.Options(StripPrefix()) },
	} {
		app := New(quietOptions())
		configure(app)
		if err := app.Build(); err == nil || !strings.Contains(err.Error(), "only Router.Mount understands") {
			t.Errorf("%s: Build() = %v, want StripPrefix reported", name, err)
		}
	}
}

// TestMountReceivesTheOriginalRequest checks what the handler is handed: the
// request the middleware passed on, never a copy or the pooled Context, with
// the framework's context values on it.
func TestMountReceivesTheOriginalRequest(t *testing.T) {
	t.Parallel()
	rec := &mountRecorder{}
	var seen atomic.Pointer[http.Request]
	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.Store(r)
			next.ServeHTTP(w, r)
		})
	})
	app.Mount("/metrics", rec)
	mustBuild(t, app)

	res := do(t, app, "GET", "/metrics?x=1")
	got := rec.last(t)
	if got.request != seen.Load() {
		t.Error("the handler was handed a different request from the one the middleware passed on")
	}
	if got.requestID == "" || got.requestID != res.Header().Get(HeaderRequestID) {
		t.Errorf("request id in the context = %q, header = %q", got.requestID, res.Header().Get(HeaderRequestID))
	}
	if got.route != "/metrics" {
		t.Errorf("RouteFromContext = %q, want the mount prefix", got.route)
	}
	if got.request.URL.RawQuery != "x=1" {
		t.Errorf("the query was %q", got.request.URL.RawQuery)
	}
}

// TestMountRunsInsideTheMiddlewareChain covers every layer the application
// wraps a route in: App.Use, CORS, the security headers and the access log.
func TestMountRunsInsideTheMiddlewareChain(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	options.CORS = CORSOptions{AllowedOrigins: []string{"https://app.example.com"}}
	app := New(options)
	var used atomic.Int32
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			used.Add(1)
			next.ServeHTTP(w, r)
		})
	})
	app.Mount("/metrics", &mountRecorder{})
	mustBuild(t, app)

	req := httptest.NewRequest("GET", "/metrics/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	res := doRequest(t, app, req)
	assertStatus(t, res, http.StatusOK)
	if used.Load() != 1 {
		t.Error("middleware installed with Use did not run")
	}
	if res.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Error("the CORS policy did not apply to the mount")
	}
	if res.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the security headers did not apply to the mount")
	}
	if !strings.Contains(logs.String(), `"route":"/metrics"`) {
		t.Errorf("the access log did not record the mount as the route:\n%s", logs.String())
	}
}

// TestMountRunsGuardsProvidersAndRateLimit puts a mount behind everything a
// route can be behind, inherited and given to Mount, and checks each refusal
// is rendered as a route's is and never reaches the handler.
func TestMountRunsGuardsProvidersAndRateLimit(t *testing.T) {
	t.Parallel()
	type user struct{ name string }
	rec := &mountRecorder{}
	var order []string
	var mu sync.Mutex
	note := func(step string) {
		mu.Lock()
		order = append(order, step)
		mu.Unlock()
	}
	ops := NewRouter(WithDependencies(func(ctx *Context) error {
		note("router guard")
		if ctx.Header("X-Team") != "ops" {
			return Forbidden("ops only")
		}
		return nil
	}))
	ops.Mount("/metrics", rec,
		WithDependencies(func(ctx *Context) error {
			note("mount guard")
			return RequireBearerToken("s3cret")(ctx)
		}),
		Needs(func(ctx *Context) (user, error) {
			note("provider")
			if ctx.Header("X-User") == "" {
				return user{}, Unauthorized("who are you")
			}
			return user{ctx.Header("X-User")}, nil
		}),
		RateLimit(Quota{Name: "metrics", Limit: 3, Window: time.Minute}))
	app := New(quietOptions())
	app.Include(ops)
	mustBuild(t, app)

	send := func(team, token, who string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.Header.Set("X-Team", team)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("X-User", who)
		return doRequest(t, app, req)
	}
	res := send("dev", "s3cret", "ana")
	assertStatus(t, res, http.StatusForbidden)
	if body := decodeError(t, res); body.Error.Message != "ops only" {
		t.Errorf("the router guard's refusal was rendered as %+v", body.Error)
	}
	res = send("ops", "", "ana")
	assertStatus(t, res, http.StatusUnauthorized)
	if res.Header().Get("WWW-Authenticate") == "" {
		t.Error("the guard's WWW-Authenticate was dropped")
	}
	assertStatus(t, send("ops", "s3cret", ""), http.StatusUnauthorized)
	if rec.count() != 0 {
		t.Fatalf("a refused request reached the handler %d times", rec.count())
	}
	want := []string{"router guard", "router guard", "mount guard", "router guard", "mount guard", "provider"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("the steps ran in the order %v, want %v", order, want)
	}

	// The fourth request is over the budget of three, which counted the
	// refusals above too, as a route's does.
	res = send("ops", "s3cret", "ana")
	assertStatus(t, res, http.StatusTooManyRequests)
	if res.Header().Get("Retry-After") == "" {
		t.Error("the rate limit refusal carries no Retry-After")
	}
	if rec.count() != 0 {
		t.Error("a rate-limited request reached the handler")
	}
}

// TestMountShareARateLimitWithRoutes shows a quota named on a mount and on a
// route is one budget, as it is between two routes.
func TestMountSharesARateLimitWithRoutes(t *testing.T) {
	t.Parallel()
	quota := Quota{Name: "shared", Limit: 2, Window: time.Minute}
	app := New(quietOptions(), RateLimit(quota))
	app.Get("/ping", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Mount("/metrics", &mountRecorder{})
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/ping"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/metrics"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/metrics"), http.StatusTooManyRequests)
	assertStatus(t, do(t, app, "GET", "/ping"), http.StatusTooManyRequests)

	// Counted after the guards, a request a guard refuses costs nothing.
	after := New(quietOptions())
	after.Mount("/metrics", &mountRecorder{},
		WithDependencies(RequireBearerToken("s3cret")),
		WithRateLimit(RateLimitOptions{Quotas: []Quota{{Name: "after", Limit: 1, Window: time.Minute}}, AfterDependencies: true}))
	mustBuild(t, after)
	authorized := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		return doRequest(t, after, req)
	}
	for range 3 {
		assertStatus(t, do(t, after, "GET", "/metrics"), http.StatusUnauthorized)
	}
	assertStatus(t, authorized(), http.StatusOK)
	assertStatus(t, authorized(), http.StatusTooManyRequests)

	skipped := New(quietOptions(), RateLimit(quota))
	skipped.Mount("/metrics", &mountRecorder{}, SkipRateLimit())
	mustBuild(t, skipped)
	for range 5 {
		assertStatus(t, do(t, skipped, "GET", "/metrics"), http.StatusOK)
	}

	conflict := New(quietOptions(), RateLimit(quota))
	conflict.Get("/ping", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	conflict.Mount("/metrics", &mountRecorder{}, RateLimit(Quota{Name: "shared", Limit: 9, Window: time.Second}))
	if err := conflict.Build(); err == nil || !strings.Contains(err.Error(), "mount /metrics") {
		t.Errorf("Build() = %v, want the mount's conflicting quota reported against it", err)
	}
}

// TestGuardedMountIsPrivateToCaches marks what a guarded mount sends as
// per-client, unless the handler says otherwise, and leaves an unguarded one
// alone.
func TestGuardedMountIsPrivateToCaches(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Mount("/private", &mountRecorder{}, WithDependencies(func(*Context) error { return nil }))
	app.Mount("/own", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}), WithDependencies(func(*Context) error { return nil }))
	app.Mount("/public", &mountRecorder{})
	mustBuild(t, app)

	if got := do(t, app, "GET", "/private").Header().Get("Cache-Control"); got != privateCacheControl {
		t.Errorf("guarded mount Cache-Control = %q, want %q", got, privateCacheControl)
	}
	if got := do(t, app, "GET", "/own").Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("a handler's own Cache-Control was replaced with %q", got)
	}
	if got := do(t, app, "GET", "/public").Header().Get("Cache-Control"); got != "" {
		t.Errorf("an unguarded mount was given Cache-Control %q", got)
	}
}

// TestStripPrefix covers the paths a handler sees with StripPrefix, including
// a prefix spelled with escapes and a remainder that carries some.
func TestStripPrefix(t *testing.T) {
	t.Parallel()
	rec := &mountRecorder{}
	root := &mountRecorder{}
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.Mount("/debug/pprof", rec, StripPrefix())
	app.Mount("/", root, StripPrefix())
	mustBuild(t, app)

	for _, tc := range []struct{ target, path, escaped string }{
		{"/debug/pprof/heap", "/heap", "/heap"},
		{"/debug/pprof", "/", "/"},
		{"/debug/pprof/", "/", "/"},
		{"/debug/pprof//x", "//x", "//x"},
		{"/debug/pprof/a%2Fb", "/a/b", "/a%2Fb"},
		{"/debug/pprof/!x*(y)", "/!x*(y)", "/!x*(y)"},
		{"/debug/pprof/%21x", "/!x", "/%21x"},
		{"/debug/pprof/caf%C3%A9", "/caf\u00e9", "/caf%C3%A9"},
		{"/%64ebug/pprof/goroutine?debug=2", "/goroutine", "/goroutine"},
	} {
		res := doRequest(t, app, httptest.NewRequest("GET", "http://h"+tc.target, nil))
		assertStatus(t, res, http.StatusOK)
		got := rec.last(t)
		if got.path != tc.path || got.escaped != tc.escaped {
			t.Errorf("%s reached the handler as path %q escaped %q, want %q and %q",
				tc.target, got.path, got.escaped, tc.path, tc.escaped)
		}
		if got.requestURI != "http://h"+tc.target {
			t.Errorf("%s: RequestURI = %q, want it unchanged", tc.target, got.requestURI)
		}
	}
	// The request the middleware above holds keeps its own path.
	if !strings.Contains(logs.String(), `"msg":"GET /debug/pprof/heap"`) {
		t.Errorf("the access log no longer records the path the client sent:\n%s", logs.String())
	}
	do(t, app, "GET", "/other/x")
	if got := root.last(t).path; got != "/other/x" {
		t.Errorf("a root mount stripped its path to %q", got)
	}
}

// TestMountServesAServeMuxWithStripPrefix is the pprof case: a ServeMux written
// to be served at the root, served beneath a prefix.
func TestMountServesAServeMuxWithStripPrefix(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /heap", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "heap") })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "index") })
	app := New(quietOptions())
	app.Mount("/debug/pprof/", mux, StripPrefix())
	mustBuild(t, app)

	if res := do(t, app, "GET", "/debug/pprof/heap"); res.Body.String() != "heap" {
		t.Errorf("GET /debug/pprof/heap = %d %q", res.Code, res.Body.String())
	}
	for _, target := range []string{"/debug/pprof", "/debug/pprof/"} {
		if res := do(t, app, "GET", target); res.Code != http.StatusOK || res.Body.String() != "index" {
			t.Errorf("GET %s = %d %q (Location %q), want the index without a redirect", target, res.Code,
				res.Body.String(), res.Header().Get("Location"))
		}
	}
	assertStatus(t, do(t, app, "GET", "/debug/pprof/missing"), http.StatusNotFound)
}

// TestMountPanicIsRecovered renders a panic before the response started as the
// standard 500, logs it with the stack, and leaves ErrAbortHandler alone.
func TestMountPanicIsRecovered(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.Mount("/boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret connection string")
	}))
	app.Mount("/abort", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	app.Mount("/guard", &mountRecorder{}, WithDependencies(func(*Context) error { panic("guard bug") }))
	mustBuild(t, app)

	res := do(t, app, "GET", "/boom/x")
	assertStatus(t, res, http.StatusInternalServerError)
	body := decodeError(t, res)
	if body.Error.Code != CodeInternalError || strings.Contains(res.Body.String(), "secret") {
		t.Errorf("the panic was rendered as %s", res.Body.String())
	}
	for _, want := range []string{"recovered from a panic in a mounted handler", "secret connection string", `"mount":"/boom"`, "goroutine"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not record %q:\n%s", want, logs.String())
		}
	}
	assertStatus(t, do(t, app, "GET", "/guard"), http.StatusInternalServerError)

	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler { //nolint:errorlint // recover yields any
				t.Errorf("ErrAbortHandler from a mounted handler became %v", recovered)
			}
		}()
		do(t, app, "GET", "/abort")
	}()
}

// TestMountPanicAfterTheResponseStartedAborts shows on the wire that a client
// cannot mistake a truncated response for a complete one.
func TestMountPanicAfterTheResponseStartedAborts(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Mount("/stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Chunked, so a response that ended normally would carry its own
		// terminating chunk and read as complete.
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		panic("halfway")
	}))
	addr := serveOnWire(t, app)

	res, err := http.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Error("the truncated body read as complete")
	}
}

// TestMountBodyLimit bounds what a mounted handler can be made to read, with
// the inherited limit, a narrower one and none at all.
func TestMountBodyLimit(t *testing.T) {
	t.Parallel()
	var readErr atomic.Pointer[error]
	reader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		readErr.Store(&err)
		_, _ = io.WriteString(w, strings.Repeat("x", int(n%10)))
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	})
	options := quietOptions()
	options.MaxBodySize = 64
	app := New(options)
	app.Mount("/inherited", reader)
	app.Mount("/narrow", reader, MaxBodySize(8))
	app.Mount("/unbounded", reader, MaxBodySize(-1))
	mustBuild(t, app)

	declared := func(target string, size int) *httptest.ResponseRecorder {
		return doRequest(t, app, httptest.NewRequest("POST", target, strings.NewReader(strings.Repeat("a", size))))
	}
	chunked := func(target string, size int) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, io.NopCloser(strings.NewReader(strings.Repeat("a", size))))
		req.ContentLength = -1
		return doRequest(t, app, req)
	}

	res := declared("/inherited", 65)
	assertStatus(t, res, http.StatusRequestEntityTooLarge)
	if body := decodeError(t, res); body.Error.Code != CodePayloadTooLarge {
		t.Errorf("a declared body over the limit was refused as %+v", body.Error)
	}
	assertStatus(t, declared("/inherited", 64), http.StatusOK)
	assertStatus(t, declared("/narrow", 9), http.StatusRequestEntityTooLarge)
	assertStatus(t, declared("/unbounded", 1<<16), http.StatusOK)

	chunked("/inherited", 1000)
	if err := readErr.Load(); err == nil || *err == nil {
		t.Error("a body of unknown length was read past the limit")
	}
	chunked("/inherited", 10)
	if err := readErr.Load(); *err != nil {
		t.Errorf("a body within the limit failed to read: %v", *err)
	}
	chunked("/unbounded", 1<<16)
	if err := readErr.Load(); *err != nil {
		t.Errorf("an unbounded mount failed to read: %v", *err)
	}
}

// TestMountsAreNotDocumented keeps a mount out of the OpenAPI document.
func TestMountsAreNotDocumented(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/ping", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Mount("/metrics", &mountRecorder{})
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), "metrics") || strings.Contains(string(raw), mountWildcard) {
		t.Errorf("the document describes the mount:\n%s", raw)
	}
	if _, ok := doc.Paths["/ping"]; !ok {
		t.Error("the route beside the mount is missing from the document")
	}
}

// TestMountedHandlerCanStreamAndHijack checks the writer handed to a mounted
// handler flushes and hijacks, which a legacy WebSocket or streaming endpoint
// needs, and that a hijack is recorded as one.
func TestMountedHandlerCanStreamAndHijack(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.Mount("/hijack", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 6\r\nConnection: close\r\n\r\nraw ok")
		_ = buf.Flush()
	}))
	app.Mount("/stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first ")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		_, _ = io.WriteString(w, "second")
	}))
	addr := serveOnWire(t, app)

	if _, body := rawGet(t, addr, "/hijack", "h", ""); body != "raw ok" {
		t.Errorf("the hijacked connection answered %q", body)
	}
	if _, body := rawGet(t, addr, "/stream", "h", ""); body != "first second" {
		t.Errorf("the stream answered %q", body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), `"status":101`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), `"status":101`) {
		t.Errorf("the hijack was not recorded as one:\n%s", logs.String())
	}
}

// TestMountAfterBuildPanics treats a late Mount as the mistake a late route is.
func TestMountAfterBuildPanics(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, New(quietOptions()))
	defer func() {
		if recover() == nil {
			t.Error("Mount after Build did not panic")
		}
	}()
	app.Mount("/late", &mountRecorder{})
}

// TestMountSingletonLifecycleIsManaged starts a component declared on a mount
// with the application, as one declared on a router is.
func TestMountSingletonLifecycleIsManaged(t *testing.T) {
	t.Parallel()
	var started atomic.Bool
	app := New(quietOptions())
	app.Mount("/metrics", &mountRecorder{}, WithSingleton(struct{}{}, LifecycleFunc("metrics",
		func(context.Context) error { started.Store(true); return nil },
		func(context.Context) error { return nil })))
	serveOnWire(t, app)
	if !started.Load() {
		t.Error("the component declared on the mount was not started")
	}
}

// TestMountConcurrentRequests drives a guarded mount from many goroutines, for
// the race detector: nothing of one request may reach another through the
// pooled Context.
func TestMountConcurrentRequests(t *testing.T) {
	t.Parallel()
	type who struct{ name string }
	names := make([]string, 64)
	for i := range names {
		names[i] = strings.Repeat(string(rune('a'+i%26)), 1+i%7)
	}
	app := New(quietOptions())
	app.Mount("/me", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Answered from the test's own table, so nothing a client sent is
		// echoed into the response.
		for _, name := range names {
			if name == r.Header.Get("X-User") {
				_, _ = io.WriteString(w, name)
				return
			}
		}
	}), Needs(func(ctx *Context) (who, error) { return who{ctx.Header("X-User")}, nil }))
	mustBuild(t, app)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			name := names[i]
			req := httptest.NewRequest("GET", "/me", nil)
			req.Header.Set("X-User", name)
			if got := doRequest(t, app, req).Body.String(); got != name {
				t.Errorf("request for %q was answered %q", name, got)
			}
		})
	}
	wg.Wait()
}

// TestMountCostsNothingWhereItIsNotUsed compares a route request on an
// application with no mount and on one with a mount elsewhere: the mount must
// not add a single allocation to a request it does not answer.
func TestMountCostsNothingWhereItIsNotUsed(t *testing.T) {
	skipAllocationCountsUnderRace(t)
	plain := New(quietOptions())
	mounted := New(quietOptions())
	for _, app := range []*App{plain, mounted} {
		app.Get("/ping", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	}
	mounted.Mount("/metrics", &mountRecorder{})
	mustBuild(t, plain)
	mustBuild(t, mounted)

	measure := func(app *App) float64 {
		req := httptest.NewRequest("GET", "/ping", nil)
		return testing.AllocsPerRun(200, func() {
			app.dispatch(httptest.NewRecorder(), req)
		})
	}
	if without, with := measure(plain), measure(mounted); with > without {
		t.Errorf("a route request allocates %.0f times beside a mount and %.0f times without one", with, without)
	}
}
