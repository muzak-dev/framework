package muzak

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// guardedMountApp serves a mount at /admin behind a guard that admits only a
// request carrying the key, beside a public wildcard at the root that takes
// whatever the mount does not. The guard records every request it admitted
// and the handler every request it served, by request identifier, so a test
// can prove that nothing was served without being admitted.
type guardedMountApp struct {
	app      *App
	mu       sync.Mutex
	admitted []string
	served   []string
}

func newGuardedMountApp(t *testing.T) *guardedMountApp {
	t.Helper()
	g := &guardedMountApp{app: New(quietOptions())}
	g.app.Get("/{rest...}", func(_ *Context, in struct {
		Rest string `path:"rest"`
	}) (encodedUserOut, error) {
		return encodedUserOut{Route: "public", ID: in.Rest}, nil
	})
	g.app.Mount("/admin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := RequestIDFromContext(r.Context())
		g.mu.Lock()
		g.served = append(g.served, id)
		g.mu.Unlock()
		_, _ = w.Write([]byte("admin"))
	}), WithDependencies(func(ctx *Context) error {
		if !SecureCompare(ctx.Header("X-Key"), "open") {
			return Forbidden("")
		}
		g.mu.Lock()
		g.admitted = append(g.admitted, ctx.RequestID())
		g.mu.Unlock()
		return nil
	}))
	return g
}

// mountSpellings lists request targets a server, a proxy or a filesystem might
// read as a path beneath /admin, and whether the routing tree hands each to
// the mount. Nothing is normalised, so each either reaches the mount through
// its exact prefix, and runs its guard, or does not reach it at all.
var mountSpellings = []struct {
	target  string
	reaches bool
}{
	{"/admin", true},
	{"/admin/", true},
	{"/admin/panel", true},
	{"/admin//panel", true},
	{"/admin/./panel", true},
	{"/admin/../panel", true},
	{"/admin/panel%2F..%2F..", true},
	{"/%61dmin/panel", true},
	{"/adm%69n", true},
	{"/%61%64%6D%69%6E/panel", true},
	{"http://example.com/admin/panel", true},
	{"//admin/panel", false},
	{"///admin", false},
	{"/ADMIN/panel", false},
	{"/Admin", false},
	{"/./admin/panel", false},
	{"/x/../admin/panel", false},
	{"/%2e/admin/panel", false},
	{"/admin%2Fpanel", false},
	{"/admin%2fpanel", false},
	{"/admin%2F", false},
	{"/admin;panel", false},
	{"/admin%00", false},
	{"/admin%20", false},
	{"/admin.", false},
	{"/admin\\panel", false},
	{"/%2561dmin/panel", false},
}

// TestMountGuardCoversEverySpellingOnTheWire is the proof the documentation of
// Router.Mount promises: sent as raw request lines through a real server, no
// spelling of a path beneath the prefix reaches the mounted handler without
// its guard, and the ones that reach it are exactly the ones the tree matches.
func TestMountGuardCoversEverySpellingOnTheWire(t *testing.T) {
	t.Parallel()
	g := newGuardedMountApp(t)
	addr := serveOnWire(t, g.app)

	for _, spelling := range mountSpellings {
		res, body := rawGet(t, addr, spelling.target, "example.com", "")
		if spelling.reaches {
			if res.StatusCode != http.StatusForbidden {
				t.Errorf("GET %s without the key = %d %q, want the mount's guard to refuse it", spelling.target, res.StatusCode, body)
			}
			continue
		}
		if res.StatusCode == http.StatusOK && strings.HasPrefix(body, "admin") {
			t.Errorf("GET %s without the key was served by the mounted handler", spelling.target)
		}
	}
	g.mu.Lock()
	served := len(g.served)
	g.mu.Unlock()
	if served != 0 {
		t.Fatalf("the mounted handler served %d requests the guard refused", served)
	}

	for _, spelling := range mountSpellings {
		res, body := rawGet(t, addr, spelling.target, "example.com", "X-Key: open\r\n")
		reached := res.StatusCode == http.StatusOK && strings.HasPrefix(body, "admin")
		if reached != spelling.reaches {
			t.Errorf("GET %s with the key: reached the mount = %v (%d %q), want %v",
				spelling.target, reached, res.StatusCode, body, spelling.reaches)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range g.served {
		if !slices.Contains(g.admitted, id) {
			t.Errorf("request %s was served by the mount without its guard admitting it", id)
		}
	}
}

// TestMountRateLimitCountsEverySpellingThatReachesIt shows an escape in the
// prefix buys no fresh budget: every spelling that reaches the mount is
// counted against the one quota.
func TestMountRateLimitCountsEverySpellingThatReachesIt(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Mount("/login", &mountRecorder{}, RateLimit(Quota{Name: "login", Limit: 3, Window: time.Minute}))
	mustBuild(t, app)
	for _, target := range []string{"/login", "/%6Cogin", "/login//", "/l%6Fgin/x"} {
		res := doRequest(t, app, httptest.NewRequest("POST", "http://h"+target, nil))
		if target == "/l%6Fgin/x" {
			assertStatus(t, res, http.StatusTooManyRequests)
			continue
		}
		assertStatus(t, res, http.StatusOK)
	}
}

// TestMountProvidersSeeNothingOfAnEarlierRequest sends a request that resolves
// a user and then one that resolves none through the same pooled Context, and
// checks the second is refused rather than handed the first's value.
func TestMountProvidersSeeNothingOfAnEarlierRequest(t *testing.T) {
	t.Parallel()
	type user struct{ name string }
	type session struct{ owner string }
	rec := &mountRecorder{}
	app := New(quietOptions())
	app.Mount("/me", rec,
		Needs(func(ctx *Context) (user, error) { return user{ctx.Header("X-User")}, nil }),
		Needs(func(ctx *Context) (session, error) {
			if u := From[user](ctx); u.name != "" {
				return session{u.name}, nil
			}
			return session{}, Unauthorized("no session")
		}))
	mustBuild(t, app)

	for range 50 {
		req := httptest.NewRequest("GET", "/me", nil)
		req.Header.Set("X-User", "ana")
		assertStatus(t, doRequest(t, app, req), http.StatusOK)
		assertStatus(t, do(t, app, "GET", "/me"), http.StatusUnauthorized)
	}
	if rec.count() != 50 {
		t.Errorf("the handler served %d requests, want the 50 that carried a user", rec.count())
	}
}

// TestMountCannotReachAGuardedFrontendByCase puts a public handler at the root
// and a guarded frontend beneath it, and checks a spelling of the frontend's
// path that a case-insensitive filesystem would open is refused rather than
// handed to the public handler, which might serve the same directory.
func TestMountCannotReachAGuardedFrontendByCase(t *testing.T) {
	t.Parallel()
	legacy := &mountRecorder{}
	app := New(quietOptions())
	app.Mount("/", legacy)
	admin := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	admin.Static("/admin", StaticOptions{FS: fstest.MapFS{"secret.txt": &fstest.MapFile{Data: []byte("secret")}}})
	app.Include(admin)
	mustBuild(t, app)

	for _, target := range []string{"/ADMIN/secret.txt", "/Admin", "/admin\\secret.txt"} {
		res := doRequest(t, app, httptest.NewRequest("GET", "http://h"+strings.ReplaceAll(target, "\\", "%5C"), nil))
		if res.Header().Get("X-Mounted") != "" {
			t.Errorf("GET %s was handed to the public root mount", target)
		}
	}
	assertStatus(t, do(t, app, "GET", "/admin/secret.txt"), http.StatusUnauthorized)
}
