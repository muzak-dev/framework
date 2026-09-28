package muzak

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// dotfileTree writes a build output with the files that end up beside one by
// accident, and returns its directory.
func dotfileTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"index.html":                  "<p>app</p>",
		".env":                        "DB_PASSWORD=hunter2",
		".git/config":                 "[remote \"origin\"]\nurl=git@example.com:corp/secret.git",
		"assets/.htpasswd":            "admin:$apr1$hash",
		"assets/app.js":               "export {}",
		".well-known/security.txt":    "Contact: mailto:security@example.com",
		".well-known/.secret/key.pem": "PRIVATE",
	} {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestMountRefusesDotfiles is the regression test for a mount that served
// whatever the directory held, /.env and /.git/config included. A dotfile
// answers 404, without the single page application fallback standing in for
// it, while /.well-known/ keeps working.
func TestMountRefusesDotfiles(t *testing.T) {
	t.Parallel()
	dir := dotfileTree(t)
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: dir})
	app.Static("/static", StaticOptions{Dir: dir})
	mustBuild(t, app)

	for _, target := range []string{
		"/.env", "/%2Eenv", "/.git/config", "/.git/", "/.git", "/assets/.htpasswd",
		"/.well-known/.secret/key.pem", "/assets/.missing",
		"/static/.env", "/static/assets/.htpasswd", "/static/.git/config",
	} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			req := httptest.NewRequest(method, target, nil)
			req.Header.Set("Accept", "text/html")
			rec := doRequest(t, app, req)
			assertStatus(t, rec, http.StatusNotFound)
			if body := rec.Body.String(); strings.Contains(body, "hunter2") || strings.Contains(body, "secret.git") || strings.Contains(body, "$apr1") ||
				strings.Contains(body, "<p>app</p>") || strings.Contains(body, "PRIVATE") {
				t.Fatalf("%s %s answered with %q", method, target, body)
			}
		}
	}
	for _, target := range []string{"/.well-known/security.txt", "/static/.well-known/security.txt", "/assets/app.js"} {
		assertStatus(t, do(t, app, "GET", target), http.StatusOK)
	}
	// An ordinary miss still gets the fallback it always did.
	req := httptest.NewRequest("GET", "/some/page", nil)
	req.Header.Set("Accept", "text/html")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
}

// TestMountAllowDotfiles checks the opt-in on both kinds of mount.
func TestMountAllowDotfiles(t *testing.T) {
	t.Parallel()
	dir := dotfileTree(t)
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: dir, AllowDotfiles: true})
	app.Static("/static", StaticOptions{Dir: dir, AllowDotfiles: true})
	mustBuild(t, app)
	for _, target := range []string{"/.env", "/.git/config", "/static/assets/.htpasswd"} {
		assertStatus(t, do(t, app, "GET", target), http.StatusOK)
	}
	// Allowing dotfiles is not allowing traversal: ".." is still no file.
	assertStatus(t, do(t, app, "GET", "/static/%2E%2E/index.html"), http.StatusNotFound)
}

func TestNamesDotfile(t *testing.T) {
	t.Parallel()
	for relative, want := range map[string]bool{
		"":                          false,
		"index.html":                false,
		"a/b.c/d.txt":               false,
		".env":                      true,
		"a/.env":                    true,
		".git/config":               true,
		"a\\.env":                   true,
		"..":                        true,
		".well-known":               false,
		".well-known/x.txt":         false,
		".well-known/.x":            true,
		"a/.well-known/x":           true,
		".well-knownx/y":            true,
		"assets/":                   false,
		"assets/v1.2/app.js":        false,
		".well-known\\security.txt": false,
	} {
		if got := namesDotfile(relative); got != want {
			t.Errorf("namesDotfile(%q) = %v, want %v", relative, got, want)
		}
	}
}

// TestMountCaseVariantIsNotServedByParent is the regression test for a
// guarded mount reached through a public parent mount on a case-insensitive
// filesystem. The filesystem is modelled with a map holding the upper-case
// spelling too, so the check holds on every platform: whatever the parent's
// filesystem would open, a path that falls under the guarded mount once case
// is ignored is not the parent's to answer.
func TestMountCaseVariantIsNotServedByParent(t *testing.T) {
	t.Parallel()
	secret := &fstest.MapFile{Data: []byte("admin-only")}
	public := fstest.MapFS{
		"index.html":             {Data: []byte("public")},
		"admin/secret.txt":       secret,
		"ADMIN/secret.txt":       secret,
		"Admin/secret.txt":       secret,
		"admin\\secret.txt":      secret,
		"\u212Aeys/secret.txt":   secret,
		"administrator/note.txt": {Data: []byte("public note")},
	}
	admin := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	admin.Static("/", StaticOptions{FS: fstest.MapFS{"secret.txt": secret}})
	keys := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	keys.Static("/", StaticOptions{FS: fstest.MapFS{"secret.txt": secret}})

	app := New(quietOptions())
	app.Static("/", StaticOptions{FS: public, Index: true})
	app.Include(admin, WithPrefix("/admin"))
	app.Include(keys, WithPrefix("/keys"))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/admin/secret.txt"), http.StatusUnauthorized)
	assertStatus(t, doRequest(t, app, withToken("/admin/secret.txt")), http.StatusOK)
	for _, target := range []string{
		"/ADMIN/secret.txt", "/Admin/secret.txt", "/aDmIn", "/admin%5Csecret.txt", "/%E2%84%AAeys/secret.txt",
	} {
		rec := do(t, app, "GET", target)
		assertStatus(t, rec, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "admin-only") {
			t.Fatalf("GET %s served the guarded file: %s", target, rec.Body.String())
		}
	}
	// A path that only shares the first letters is not under the mount.
	assertStatus(t, do(t, app, "GET", "/administrator/note.txt"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/"), http.StatusOK)
}

// TestMountCaseVariantOnThisFilesystem replays the report against a real
// directory, which is only meaningful where the filesystem ignores case.
func TestMountCaseVariantOnThisFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "admin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "public", "admin/secret.txt": "admin-only"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ADMIN", "secret.txt")); err != nil {
		t.Skip("this filesystem is case-sensitive; TestMountCaseVariantIsNotServedByParent covers the logic")
	}
	admin := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	admin.Static("/", StaticOptions{Dir: filepath.Join(dir, "admin")})
	app := New(quietOptions())
	app.Static("/", StaticOptions{Dir: dir})
	app.Include(admin, WithPrefix("/admin"))
	mustBuild(t, app)
	assertStatus(t, do(t, app, "GET", "/admin/secret.txt"), http.StatusUnauthorized)
	for _, target := range []string{"/ADMIN/secret.txt", "/Admin/secret.txt"} {
		assertStatus(t, do(t, app, "GET", target), http.StatusNotFound)
	}
}
