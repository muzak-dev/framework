package muzak

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// testAPIKey is the key the "key" scheme of the enforcement tests accepts.
const testAPIKey = "k-0123456789abcdef"

// enforcementSchemes are a JWT scheme, an API key scheme and a descriptive one.
func enforcementSchemes() map[string]SecurityScheme {
	return map[string]SecurityScheme{
		"jwt": testScheme(jwtOptions()),
		"key": APIKeyVerifier(APIKeyOptions{Name: "X-API-Key", Keys: []APIKey{
			{ID: "partner", Key: testAPIKey, Scopes: []string{"items:read"}},
		}}),
		"described": BearerAuth("JWT"),
	}
}

// request sends a request carrying the credentials named.
func request(t *testing.T, app *App, method, target string, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	return doRequest(t, app, req)
}

// TestEnforcementMatrix drives every combination of credentials through every
// shape of requirement.
func TestEnforcementMatrix(t *testing.T) {
	good := signJWT(t, "RS256", testRSAKey(), "", map[string]any{"scope": "items:read"})
	bad := signJWT(t, "RS256", testRSAKeyOther(), "", nil)
	noScope := signJWT(t, "RS256", testRSAKey(), "", nil)
	type creds struct{ token, key string }
	cases := []struct {
		name         string
		requirements []SecurityRequirement
		want         map[creds]int
	}{
		{
			name:         "jwt alone",
			requirements: []SecurityRequirement{Require("jwt")},
			want: map[creds]int{
				{}: 401, {good, ""}: 200, {bad, ""}: 401, {"", testAPIKey}: 401, {good, testAPIKey}: 200,
			},
		},
		{
			name:         "jwt or key",
			requirements: []SecurityRequirement{Require("jwt"), Require("key")},
			want: map[creds]int{
				{}: 401, {good, ""}: 200, {"", testAPIKey}: 200, {bad, ""}: 401, {"", "wrong-key-0123456789"}: 401,
				{bad, testAPIKey}: 200, {good, "wrong-key-0123456789"}: 200,
			},
		},
		{
			name:         "jwt and key",
			requirements: []SecurityRequirement{{"jwt": nil, "key": nil}},
			want: map[creds]int{
				{}: 401, {good, ""}: 401, {"", testAPIKey}: 401, {good, testAPIKey}: 200, {bad, testAPIKey}: 401,
			},
		},
		{
			name:         "jwt with a scope",
			requirements: []SecurityRequirement{Require("jwt", "items:read")},
			want: map[creds]int{
				{}: 401, {good, ""}: 200, {noScope, ""}: 403, {bad, ""}: 401,
			},
		},
		{
			name:         "jwt with a scope or key",
			requirements: []SecurityRequirement{Require("jwt", "items:write"), Require("key", "items:read")},
			want: map[creds]int{
				{good, ""}: 403, {"", testAPIKey}: 200, {good, testAPIKey}: 200,
			},
		},
		{
			name:         "jwt or anonymous",
			requirements: []SecurityRequirement{Require("jwt"), {}},
			want: map[creds]int{
				{}: 200, {good, ""}: 200, {bad, ""}: 401, {"", testAPIKey}: 200,
			},
		},
		{
			name:         "jwt and key, or anonymous",
			requirements: []SecurityRequirement{{"jwt": nil, "key": nil}, {}},
			want: map[creds]int{
				{}: 200, {"", testAPIKey}: 401, {good, ""}: 401, {good, testAPIKey}: 200,
			},
		},
		{
			name:         "key with a scope it lacks",
			requirements: []SecurityRequirement{Require("key", "items:write")},
			want: map[creds]int{
				{"", testAPIKey}: 403, {}: 401,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := mustBuild(t, authApp(t, enforcementSchemes(), tc.requirements...))
			for c, want := range tc.want {
				rec := request(t, app, http.MethodGet, "/me", c.token, c.key)
				if rec.Code != want {
					t.Errorf("token=%t key=%q: status = %d, want %d\nbody: %s", c.token != "", c.key, rec.Code, want, rec.Body.String())
				}
				assertNoTokenLeak(t, rec, c.token)
				if strings.Contains(rec.Body.String(), testAPIKey) || strings.Contains(fmt.Sprint(rec.Header()), testAPIKey) {
					t.Errorf("the response carries the API key")
				}
			}
		})
	}
}

// TestEnforcementChallenges checks the WWW-Authenticate a refusal carries in
// each case, following RFC 6750.
func TestEnforcementChallenges(t *testing.T) {
	app := mustBuild(t, authApp(t, enforcementSchemes(), Require("jwt", "items:read")))
	rec := request(t, app, http.MethodGet, "/me", "", "")
	assertStatus(t, rec, 401)
	if got := rec.Header().Values("WWW-Authenticate"); len(got) != 1 || got[0] != `Bearer realm="api"` {
		t.Fatalf("no credential: WWW-Authenticate = %q", got)
	}
	if e := decodeError(t, rec); e.Error.Code != CodeUnauthorized || e.Error.Message != "Authentication is required." {
		t.Fatalf("no credential: body = %+v", e)
	}
	rec = request(t, app, http.MethodGet, "/me", "not-a-token", "")
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api", error="invalid_token"` {
		t.Fatalf("bad token: WWW-Authenticate = %q", got)
	}
	// Two Authorization headers are refused, even when one of them is good.
	good := signJWT(t, "RS256", testRSAKey(), "", map[string]any{"scope": "items:read"})
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Add("Authorization", "Bearer "+good)
	req.Header.Add("Authorization", "Basic dXNlcjpwYXNz")
	rec = doRequest(t, app, req)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer realm="api", error="invalid_token"` {
		t.Fatalf("two Authorization headers: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// Another scheme in Authorization is no bearer credential at all.
	req = httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec = doRequest(t, app, req)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer realm="api"` {
		t.Fatalf("basic credentials: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// The scheme name is matched without regard to case.
	req = httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "bEaReR "+good)
	assertStatus(t, doRequest(t, app, req), 200)
	// The same body whatever was wrong, so a client learns nothing about
	// which check failed.
	bodies := map[string]bool{}
	for _, token := range []string{
		"not-a-token",
		signJWT(t, "RS256", testRSAKeyOther(), "", nil),
		signJWT(t, "RS256", testRSAKey(), "", map[string]any{"exp": testNow.Unix() - 3600}),
		signJWT(t, "RS256", testRSAKey(), "", map[string]any{"iss": "https://evil.example/"}),
		signJWT(t, "RS256", testRSAKey(), "", map[string]any{"aud": "https://other.api"}),
	} {
		rec := request(t, app, http.MethodGet, "/me", token, "")
		e := decodeError(t, rec)
		bodies[e.Error.Code+"|"+e.Error.Message] = true
		assertNoTokenLeak(t, rec, token)
	}
	if len(bodies) != 1 {
		t.Fatalf("refusals differ by cause: %v", bodies)
	}
	rec = request(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", nil), "")
	assertStatus(t, rec, 403)
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api", error="insufficient_scope", scope="items:read"` {
		t.Fatalf("insufficient scope: WWW-Authenticate = %q", got)
	}
	// Two JWT schemes each name their own challenge.
	schemes := enforcementSchemes()
	other := jwtOptions()
	other.Realm = "partners"
	schemes["jwt2"] = testScheme(other)
	app = mustBuild(t, authApp(t, schemes, Require("jwt"), Require("jwt2"), Require("key")))
	rec = request(t, app, http.MethodGet, "/me", "", "")
	got := rec.Header().Values("WWW-Authenticate")
	if len(got) != 2 || got[0] != `Bearer realm="api"` || got[1] != `Bearer realm="partners"` {
		t.Fatalf("two schemes: WWW-Authenticate = %q", got)
	}
	// A key scheme alone has no challenge to make.
	app = mustBuild(t, authApp(t, enforcementSchemes(), Require("key")))
	rec = request(t, app, http.MethodGet, "/me", "", "wrong-key-0123456789")
	assertStatus(t, rec, 401)
	if got := rec.Header().Values("WWW-Authenticate"); len(got) != 0 {
		t.Fatalf("key scheme: WWW-Authenticate = %q", got)
	}
	// A token that verified but whose requirement failed on its partner is
	// not called invalid.
	app = mustBuild(t, authApp(t, enforcementSchemes(), SecurityRequirement{"key": nil, "jwt": nil}))
	rec = request(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", nil), "")
	assertStatus(t, rec, 401)
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api"` {
		t.Fatalf("valid token, missing key: WWW-Authenticate = %q", got)
	}
}

// TestEnforcementRunsBeforeGuardsAndProviders checks the gate runs first, so a
// guard or a provider never sees an unauthenticated request, and that both
// see the verified claims.
func TestEnforcementRunsBeforeGuardsAndProviders(t *testing.T) {
	var guardRan, providerRan atomic.Int32
	opts := quietOptions()
	opts.SecuritySchemes = enforcementSchemes()
	app := New(opts, WithDependencies(func(ctx *Context) error {
		guardRan.Add(1)
		if _, ok := TryFrom[*Claims](ctx); !ok {
			return errors.New("the guard ran without claims")
		}
		return nil
	}))
	type In struct {
		Claims Dep[*Claims]
		User   Dep[string]
	}
	app.Get("/me", func(ctx *Context, in In) (meOut, error) {
		return meOut{Subject: in.User.Get() + ":" + in.Claims.Get().Subject}, nil
	}, WithSecurity(Require("jwt")), Needs(func(ctx *Context) (string, error) {
		providerRan.Add(1)
		return From[*Claims](ctx).Issuer, nil
	}))
	mustBuild(t, app)
	assertStatus(t, request(t, app, http.MethodGet, "/me", "", ""), 401)
	if guardRan.Load() != 0 || providerRan.Load() != 0 {
		t.Fatal("a guard or a provider ran for a refused request")
	}
	rec := request(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", nil), "")
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"subject":"`+testIssuer+`:user-1","scheme":"","scopes":[]}`)
	if got := rec.Header().Get("Cache-Control"); got != privateCacheControl {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Authorization") {
		t.Fatalf("Vary = %q", got)
	}
}

// TestEnforcementPublicExempts checks Public opts a route out of a router's
// verifying requirement, and that the router's other routes stay enforced.
func TestEnforcementPublicExempts(t *testing.T) {
	opts := quietOptions()
	opts.SecuritySchemes = enforcementSchemes()
	app := New(opts)
	r := NewRouter(WithSecurity(Require("jwt")))
	r.Get("/private", me)
	r.Get("/public", me, Public())
	app.Include(r, WithPrefix("/api"))
	mustBuild(t, app)
	assertStatus(t, request(t, app, http.MethodGet, "/api/private", "", ""), 401)
	assertStatus(t, request(t, app, http.MethodGet, "/api/public", "", ""), 200)
	// Public is open to a bad token too, since it reads none.
	assertStatus(t, request(t, app, http.MethodGet, "/api/public", "garbage", ""), 200)
}

// TestEnforcementCoversFallbacks checks a route is not reachable around its
// security through the methods the framework answers for it.
func TestEnforcementCoversFallbacks(t *testing.T) {
	var ran atomic.Int32
	opts := quietOptions()
	opts.SecuritySchemes = enforcementSchemes()
	app := New(opts)
	app.Get("/me", func(ctx *Context, _ struct{}) (meOut, error) {
		ran.Add(1)
		return meOut{}, nil
	}, WithSecurity(Require("jwt")))
	mustBuild(t, app)
	rec := request(t, app, http.MethodHead, "/me", "", "")
	assertStatus(t, rec, 401)
	if rec.Header().Get("WWW-Authenticate") != `Bearer realm="api"` {
		t.Fatalf("HEAD: WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	rec = request(t, app, http.MethodOptions, "/me", "", "")
	assertStatus(t, rec, http.StatusNoContent)
	if rec.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
	assertStatus(t, request(t, app, http.MethodPost, "/me", "", ""), http.StatusMethodNotAllowed)
	if ran.Load() != 0 {
		t.Fatal("the handler ran without a token")
	}
	assertStatus(t, request(t, app, http.MethodHead, "/me", signJWT(t, "RS256", testRSAKey(), "", nil), ""), 200)
	if ran.Load() != 1 {
		t.Fatal("HEAD with a token did not reach the handler")
	}
}

// TestEnforcementCoversMounts checks a handler mount, a static mount and a
// frontend inherit a router's verifying requirement, and Public exempts them.
func TestEnforcementCoversMounts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>secret</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("secret()"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := quietOptions()
	opts.SecuritySchemes = enforcementSchemes()
	app := New(opts)
	secured := NewRouter(WithSecurity(Require("jwt")))
	secured.Mount("/legacy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "legacy secret")
	}))
	secured.Mount("/open", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "open")
	}), Public())
	secured.Static("/assets", StaticOptions{Dir: dir})
	secured.Frontend("/app", FrontendOptions{FS: fstest.MapFS{"index.html": {Data: []byte("<p>app</p>")}}})
	app.Include(secured, WithPrefix("/s"))
	mustBuild(t, app)
	token := signJWT(t, "RS256", testRSAKey(), "", nil)
	// A deep link of the frontend is answered with its fallback only for a
	// navigation, so it is checked for the refusal alone.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if rec := request(t, app, method, "/s/app/deep/link", "", ""); rec.Code != 401 {
			t.Errorf("%s /s/app/deep/link without a token: %d", method, rec.Code)
		}
	}
	for _, path := range []string{"/s/legacy", "/s/legacy/x", "/s/assets/app.js", "/s/app"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodHead} {
			rec := request(t, app, method, path, "", "")
			if rec.Code != 401 {
				t.Errorf("%s %s without a token: %d", method, path, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("%s %s leaked the content", method, path)
			}
		}
		rec := request(t, app, http.MethodGet, path, token, "")
		if rec.Code != 200 {
			t.Errorf("GET %s with a token: %d %s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
			t.Errorf("GET %s: Cache-Control = %q", path, got)
		}
	}
	assertStatus(t, request(t, app, http.MethodGet, "/s/open", "", ""), 200)
}

// TestEnforcementCoversTheDocumentation checks the application's own
// requirement covers the document, as its guards do, and that one declared on
// an included router does not.
func TestEnforcementCoversTheDocumentation(t *testing.T) {
	opts := quietOptions()
	opts.SecuritySchemes = enforcementSchemes()
	app := New(opts, WithSecurity(Require("jwt")))
	app.Get("/me", me)
	mustBuild(t, app)
	for _, path := range []string{"/openapi.json", "/docs", "/docs/_nuxt/app.js"} {
		rec := request(t, app, http.MethodGet, path, "", "")
		if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer realm="api"` {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
		assertStatus(t, request(t, app, http.MethodGet, path, signJWT(t, "RS256", testRSAKey(), "", nil), ""), 200)
	}

	app = New(opts)
	r := NewRouter(WithSecurity(Require("jwt")))
	r.Get("/me", me)
	app.Include(r)
	mustBuild(t, app)
	assertStatus(t, request(t, app, http.MethodGet, "/openapi.json", "", ""), 200)
	assertStatus(t, request(t, app, http.MethodGet, "/me", "", ""), 401)
}

// TestEnforcementBuildErrors covers every declaration that cannot be enforced
// as written.
func TestEnforcementBuildErrors(t *testing.T) {
	t.Run("mixed alternatives", func(t *testing.T) {
		msg := buildError(t, authApp(t, enforcementSchemes(), Require("jwt"), Require("described")))
		if !strings.Contains(msg, `GET /me: WithSecurity offers alternatives that mix schemes Muzak verifies ("jwt") with schemes it only describes ("described")`) {
			t.Fatalf("build error = %s", msg)
		}
	})
	t.Run("one requirement may mix", func(t *testing.T) {
		var guardRan atomic.Int32
		opts := quietOptions()
		opts.SecuritySchemes = enforcementSchemes()
		app := New(opts)
		app.Get("/me", me, WithSecurity(SecurityRequirement{"jwt": nil, "described": nil}),
			WithDependencies(func(*Context) error { guardRan.Add(1); return nil }))
		mustBuild(t, app)
		assertStatus(t, request(t, app, http.MethodGet, "/me", "", ""), 401)
		if guardRan.Load() != 0 {
			t.Fatal("the guard ran before the gate refused")
		}
		assertStatus(t, request(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", nil), ""), 200)
	})
	t.Run("invalid scope", func(t *testing.T) {
		for _, scope := range []string{"", "two words", `quo"te`, "back\\slash", "tab\t", "caf\xc3\xa9"} {
			msg := buildError(t, authApp(t, enforcementSchemes(), Require("jwt", scope)))
			if !strings.Contains(msg, "is not a valid scope") {
				t.Fatalf("scope %q: build error = %s", scope, msg)
			}
		}
	})
	t.Run("undeclared on a mount", func(t *testing.T) {
		opts := quietOptions()
		opts.SecuritySchemes = enforcementSchemes()
		app := New(opts)
		app.Mount("/m", http.NotFoundHandler(), WithSecurity(Require("jtw")))
		msg := buildError(t, app)
		if !strings.Contains(msg, `mount at /m: security scheme "jtw" is not declared`) {
			t.Fatalf("build error = %s", msg)
		}
	})
	t.Run("too many schemes", func(t *testing.T) {
		schemes := map[string]SecurityScheme{}
		var reqs []SecurityRequirement
		for i := range maxGateSchemes + 1 {
			name := fmt.Sprintf("jwt%02d", i)
			schemes[name] = testScheme(jwtOptions())
			reqs = append(reqs, Require(name))
		}
		msg := buildError(t, authApp(t, schemes, reqs...))
		if !strings.Contains(msg, "names 17 verifying schemes, over the limit of 16") {
			t.Fatalf("build error = %s", msg)
		}
	})
	t.Run("edited scheme", func(t *testing.T) {
		edited := testScheme(jwtOptions())
		edited.Type = "apiKey"
		edited.In, edited.Name = "header", "X-Token"
		msg := buildError(t, authApp(t, map[string]SecurityScheme{"jwt": edited}, Require("jwt")))
		if !strings.Contains(msg, "was built by a verifying constructor and then changed") {
			t.Fatalf("build error = %s", msg)
		}
		key := APIKeyVerifier(APIKeyOptions{Name: "X-API-Key", Keys: []APIKey{{ID: "a", Key: testAPIKey}}})
		key.Name = "X-Other"
		msg = buildError(t, authApp(t, map[string]SecurityScheme{"key": key}, Require("key")))
		if !strings.Contains(msg, "was built by a verifying constructor and then changed") {
			t.Fatalf("build error = %s", msg)
		}
	})
	t.Run("invalid options are reported once per scheme", func(t *testing.T) {
		schemes := map[string]SecurityScheme{"jwt": JWTBearer(JWTOptions{})}
		msg := buildError(t, authApp(t, schemes, Require("jwt")))
		for _, want := range []string{
			`muzak: AppOptions.SecuritySchemes["jwt"]: JWTOptions.Issuers names no issuer`,
			`muzak: AppOptions.SecuritySchemes["jwt"]: JWTOptions.Audience is empty`,
			`muzak: AppOptions.SecuritySchemes["jwt"]: JWTOptions.Algorithms names no algorithm`,
			`muzak: AppOptions.SecuritySchemes["jwt"]: JWTOptions names no key`,
		} {
			if strings.Count(msg, want) != 1 {
				t.Errorf("build error does not name %q once:\n%s", want, msg)
			}
		}
	})
}

// TestEnforcementPrincipalDeps checks a Dep of a verified principal is
// accepted only where the route's security always provides it.
func TestEnforcementPrincipalDeps(t *testing.T) {
	type ClaimsIn struct {
		Claims Dep[*Claims]
	}
	type KeyIn struct {
		Key Dep[*APIKeyPrincipal]
	}
	handler := func(ctx *Context, in ClaimsIn) (meOut, error) {
		return meOut{Subject: in.Claims.Get().Subject, Scheme: in.Claims.Get().Scheme}, nil
	}
	build := func(reqs ...SecurityRequirement) *App {
		opts := quietOptions()
		opts.SecuritySchemes = enforcementSchemes()
		app := New(opts)
		app.Get("/me", handler, WithSecurity(reqs...))
		return app
	}
	app := mustBuild(t, build(Require("jwt")))
	assertJSON(t, request(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", nil), ""), `{"subject":"user-1","scheme":"jwt","scopes":[]}`)
	mustBuild(t, build(SecurityRequirement{"jwt": nil, "key": nil}))
	for name, reqs := range map[string][]SecurityRequirement{
		"key only":           {Require("key")},
		"jwt or key":         {Require("jwt"), Require("key")},
		"jwt or anonymous":   {Require("jwt"), {}},
		"public":             {},
		"descriptive scheme": {Require("described")},
	} {
		var app *App
		if len(reqs) == 0 {
			opts := quietOptions()
			opts.SecuritySchemes = enforcementSchemes()
			app = New(opts)
			app.Get("/me", handler, Public())
		} else {
			app = build(reqs...)
		}
		msg := buildError(t, app)
		if !strings.Contains(msg, "field Claims is a muzak.Dep[*muzak.Claims], which only a JWTBearer scheme fills") {
			t.Errorf("%s: build error = %s", name, msg)
		}
	}
	// A route with no security at all.
	opts := quietOptions()
	app = New(opts)
	app.Get("/me", handler)
	if msg := buildError(t, app); !strings.Contains(msg, "which only a JWTBearer scheme fills") {
		t.Fatalf("build error = %s", msg)
	}
	// A provider of the application's own satisfies it, as for any Dep.
	app = New(opts)
	app.Get("/me", handler, Needs(func(*Context) (*Claims, error) { return &Claims{Subject: "fake"}, nil }))
	mustBuild(t, app)
	// And the API key principal.
	opts.SecuritySchemes = enforcementSchemes()
	app = New(opts)
	app.Get("/key", func(ctx *Context, in KeyIn) (meOut, error) {
		return meOut{Subject: in.Key.Get().ID, Scheme: in.Key.Get().Scheme, Scopes: in.Key.Get().Scopes}, nil
	}, WithSecurity(Require("key")))
	app.Get("/either", func(ctx *Context, in KeyIn) (meOut, error) { return meOut{}, nil }, WithSecurity(Require("key"), Require("jwt")))
	msg := buildError(t, app)
	if !strings.Contains(msg, "GET /either: field Key is a muzak.Dep[*muzak.APIKeyPrincipal], which only a APIKeyVerifier scheme fills") ||
		strings.Contains(msg, "GET /key:") {
		t.Fatalf("build error = %s", msg)
	}
}

// TestClaimsAs checks custom claims decode once per request and type, and the
// failures.
func TestClaimsAs(t *testing.T) {
	type Tenant struct {
		Tenant string   `json:"tid"`
		Roles  []string `json:"roles"`
	}
	type Wrong struct {
		Tenant int `json:"tid"`
	}
	var same atomic.Bool
	opts := quietOptions()
	opts.SecuritySchemes = enforcementSchemes()
	app := New(opts)
	app.Get("/tenant", func(ctx *Context, _ struct{}) (Tenant, error) {
		first, err := ClaimsAs[Tenant](ctx)
		if err != nil {
			return Tenant{}, err
		}
		second, _ := ClaimsAs[Tenant](ctx)
		same.Store(&first.Roles[0] == &second.Roles[0])
		return first, nil
	}, WithSecurity(Require("jwt")))
	app.Get("/wrong", func(ctx *Context, _ struct{}) (Wrong, error) {
		w, err := ClaimsAs[Wrong](ctx)
		if _, again := ClaimsAs[Wrong](ctx); again == nil || err == nil {
			return Wrong{}, errors.New("a decoding failure was not remembered")
		}
		return w, err
	}, WithSecurity(Require("jwt")))
	app.Get("/anonymous", func(ctx *Context, _ struct{}) (Tenant, error) {
		return ClaimsAs[Tenant](ctx)
	}, WithSecurity(Require("jwt"), SecurityRequirement{}))
	mustBuild(t, app)
	token := signJWT(t, "RS256", testRSAKey(), "", map[string]any{"tid": "acme", "roles": []string{"admin"}})
	rec := request(t, app, http.MethodGet, "/tenant", token, "")
	assertJSON(t, rec, `{"tid":"acme","roles":["admin"]}`)
	if !same.Load() {
		t.Fatal("the second call decoded again")
	}
	assertStatus(t, request(t, app, http.MethodGet, "/wrong", token, ""), 401)
	assertStatus(t, request(t, app, http.MethodGet, "/anonymous", "", ""), 401)
	if (*Claims)(nil).HasScope("x") {
		t.Fatal("a nil Claims has a scope")
	}
}

// TestEnforcementRendersThroughProblemDetails checks a refusal is rendered by
// the configured error renderer and keeps its challenge.
func TestEnforcementRendersThroughProblemDetails(t *testing.T) {
	opts := quietOptions()
	opts.SecuritySchemes = enforcementSchemes()
	opts.ProblemDetails = &ProblemOptions{}
	app := New(opts)
	app.Get("/me", me, WithSecurity(Require("jwt", "items:read")))
	mustBuild(t, app)
	token := signJWT(t, "RS256", testRSAKeyOther(), "", nil)
	rec := request(t, app, http.MethodGet, "/me", token, "")
	assertStatus(t, rec, 401)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("WWW-Authenticate") != `Bearer realm="api", error="invalid_token"` {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
	assertNoTokenLeak(t, rec, token)
	rec = request(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", nil), "")
	assertStatus(t, rec, 403)
	var problem map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil || problem["status"] != float64(403) {
		t.Fatalf("problem = %v (%v)", problem, err)
	}
}

// TestVerifyingSchemesAreDocumentedAsDescriptiveOnes checks the document
// cannot tell a verifying scheme from the descriptive one of the same shape.
func TestVerifyingSchemesAreDocumentedAsDescriptiveOnes(t *testing.T) {
	encode := func(schemes map[string]SecurityScheme) []byte {
		opts := quietOptions()
		opts.SecuritySchemes = schemes
		app := New(opts)
		app.Get("/me", me, WithSecurity(Require("a", "items:read"), Require("b")))
		doc, err := app.Document()
		if err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(doc, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	jwtDescribed := BearerAuth("JWT")
	jwtDescribed.Description = "Tokens from the login service."
	jwtOpts := jwtOptions()
	jwtOpts.Description = "Tokens from the login service."
	for _, in := range []string{"header", "query", "cookie"} {
		verifying := encode(map[string]SecurityScheme{
			"a": testScheme(jwtOpts),
			"b": APIKeyVerifier(APIKeyOptions{In: in, Name: "api_key", Keys: []APIKey{{ID: "x", Key: testAPIKey}}}),
		})
		described := encode(map[string]SecurityScheme{
			"a": jwtDescribed,
			"b": map[string]SecurityScheme{"header": APIKeyHeader("api_key"), "query": APIKeyQuery("api_key"), "cookie": APIKeyCookie("api_key")}[in],
		})
		if !bytes.Equal(verifying, described) {
			t.Fatalf("%s: the documents differ\nverifying: %s\ndescribed: %s", in, verifying, described)
		}
	}
}

// TestUnsecuredRoutesPayNothing checks a route that names no verifying scheme
// gets no gate and allocates exactly what it did before.
func TestUnsecuredRoutesPayNothing(t *testing.T) {
	build := func(opts ...RouteOption) *App {
		o := quietOptions()
		o.DisableAccessLog = true
		o.SecuritySchemes = map[string]SecurityScheme{"described": BearerAuth("JWT"), "jwt": testScheme(jwtOptions())}
		app := New(o)
		app.Get("/x", func(*Context, struct{}) (struct{}, error) { return struct{}{}, nil }, opts...)
		return mustBuild(t, app)
	}
	plain := build()
	described := build(WithSecurity(Require("described")))
	if len(described.routes[0].guards) != 0 {
		t.Fatal("a descriptive scheme added a guard")
	}
	if raceDetector {
		// sync.Pool drops a share of what it is given under -race, so the
		// counts below are not exact there.
		return
	}
	allocs := func(app *App) float64 {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		return testing.AllocsPerRun(200, func() {
			app.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
	if a, b := allocs(plain), allocs(described); a != b {
		t.Fatalf("allocations: %v without security, %v with a descriptive scheme", a, b)
	}
}
