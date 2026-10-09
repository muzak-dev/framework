package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// keyApp builds an application with one API key scheme named "key" and GET /me
// answering with the principal.
func keyApp(t *testing.T, opts APIKeyOptions, scopes ...string) *App {
	t.Helper()
	appOpts := quietOptions()
	appOpts.SecuritySchemes = map[string]SecurityScheme{"key": APIKeyVerifier(opts)}
	app := New(appOpts)
	app.Get("/me", func(ctx *Context, _ struct{}) (meOut, error) {
		p := From[*APIKeyPrincipal](ctx)
		return meOut{Subject: p.ID, Scheme: p.Scheme, Scopes: p.Scopes}, nil
	}, WithSecurity(Require("key", scopes...)))
	return app
}

// TestAPIKeyVerifierReadsEachLocation checks a key in a header, a query
// parameter and a cookie.
func TestAPIKeyVerifierReadsEachLocation(t *testing.T) {
	keys := []APIKey{{ID: "acme", Key: testAPIKey, Scopes: []string{"orders:read"}}, {ID: "globex", Key: "g-0123456789abcdef"}}
	cases := []struct {
		in   string
		send func(*http.Request, string)
	}{
		{"header", func(r *http.Request, k string) { r.Header.Set("X-API-Key", k) }},
		{"", func(r *http.Request, k string) { r.Header.Set("x-api-key", k) }},
		{"query", func(r *http.Request, k string) { r.URL.RawQuery = "X-API-Key=" + k }},
		{"cookie", func(r *http.Request, k string) { r.AddCookie(&http.Cookie{Name: "X-API-Key", Value: k}) }},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			app := mustBuild(t, keyApp(t, APIKeyOptions{In: tc.in, Name: "X-API-Key", Keys: keys}))
			send := func(key string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/me", nil)
				if key != "" {
					tc.send(req, key)
				}
				return doRequest(t, app, req)
			}
			rec := send(testAPIKey)
			assertJSON(t, rec, `{"subject":"acme","scheme":"key","scopes":["orders:read"]}`)
			assertJSON(t, send("g-0123456789abcdef"), `{"subject":"globex","scheme":"key","scopes":[]}`)
			for _, key := range []string{"", "wrong-0123456789abc", testAPIKey + "x", testAPIKey[:len(testAPIKey)-1], strings.ToUpper(testAPIKey)} {
				rec := send(key)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("key %q: status = %d", key, rec.Code)
				}
				if got := rec.Header().Values("WWW-Authenticate"); len(got) != 0 {
					t.Errorf("an API key refusal names a challenge: %q", got)
				}
			}
			switch tc.in {
			case "header", "":
				if !strings.Contains(rec.Header().Get("Vary"), "X-Api-Key") {
					t.Errorf("Vary = %q", rec.Header().Get("Vary"))
				}
			case "cookie":
				if !strings.Contains(rec.Header().Get("Vary"), "Cookie") {
					t.Errorf("Vary = %q", rec.Header().Get("Vary"))
				}
			}
		})
	}
}

// TestAPIKeyVerifierRefusesAmbiguousKeys checks a request that sends the key
// twice is refused, whichever copy is the good one.
func TestAPIKeyVerifierRefusesAmbiguousKeys(t *testing.T) {
	keys := []APIKey{{ID: "acme", Key: testAPIKey}}
	header := mustBuild(t, keyApp(t, APIKeyOptions{Name: "X-API-Key", Keys: keys}))
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Add("X-API-Key", testAPIKey)
	req.Header.Add("X-API-Key", "other-0123456789abc")
	assertStatus(t, doRequest(t, header, req), http.StatusUnauthorized)

	query := mustBuild(t, keyApp(t, APIKeyOptions{In: "query", Name: "key", Keys: keys}))
	assertStatus(t, do(t, query, http.MethodGet, "/me?key="+testAPIKey+"&key="+testAPIKey), http.StatusUnauthorized)
	assertStatus(t, do(t, query, http.MethodGet, "/me?key="), http.StatusUnauthorized)
	assertStatus(t, do(t, query, http.MethodGet, "/me?key="+strings.Repeat("k", maxAPIKeyBytes+1)), http.StatusUnauthorized)
	assertStatus(t, do(t, query, http.MethodGet, "/me?key="+testAPIKey), http.StatusOK)

	cookie := mustBuild(t, keyApp(t, APIKeyOptions{In: "cookie", Name: "key", Keys: keys}))
	req = httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Cookie", "key="+testAPIKey+"; key="+testAPIKey)
	assertStatus(t, doRequest(t, cookie, req), http.StatusUnauthorized)
}

// TestAPIKeyVerifierChecksScopes checks a key's scopes against a route's.
func TestAPIKeyVerifierChecksScopes(t *testing.T) {
	keys := []APIKey{{ID: "acme", Key: testAPIKey, Scopes: []string{"orders:read"}}}
	app := mustBuild(t, keyApp(t, APIKeyOptions{Name: "X-API-Key", Keys: keys}, "orders:write"))
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("X-API-Key", testAPIKey)
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusForbidden)
	if got := rec.Header().Values("WWW-Authenticate"); len(got) != 0 {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	app = mustBuild(t, keyApp(t, APIKeyOptions{Name: "X-API-Key", Keys: keys}, "orders:read"))
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
	if !(&APIKeyPrincipal{Scopes: []string{"a"}}).HasScope("a") || (*APIKeyPrincipal)(nil).HasScope("a") {
		t.Fatal("HasScope")
	}
}

// TestAPIKeyVerifierLookup checks a key resolved by the application, and that
// a failing lookup is a 503 logged without the key.
func TestAPIKeyVerifierLookup(t *testing.T) {
	logger, logs := captureLogger(t)
	calls := 0
	opts := APIKeyOptions{
		Name: "X-API-Key",
		Keys: []APIKey{{ID: "static", Key: testAPIKey}},
		Lookup: func(ctx *Context, key string) (*APIKeyPrincipal, error) {
			calls++
			switch key {
			case "db-0123456789abcdef":
				return &APIKeyPrincipal{Scheme: "forged", ID: "from-db", Scopes: []string{"x"}}, nil
			case "fail-0123456789abcd":
				return nil, errors.New("the database is down")
			}
			return nil, nil
		},
	}
	appOpts := quietOptions()
	appOpts.Logger = logger
	appOpts.SecuritySchemes = map[string]SecurityScheme{"key": APIKeyVerifier(opts)}
	app := New(appOpts)
	app.Get("/me", func(ctx *Context, _ struct{}) (meOut, error) {
		p := From[*APIKeyPrincipal](ctx)
		return meOut{Subject: p.ID, Scheme: p.Scheme, Scopes: p.Scopes}, nil
	}, WithSecurity(Require("key")))
	mustBuild(t, app)
	send := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		req.Header.Set("X-API-Key", key)
		return doRequest(t, app, req)
	}
	assertJSON(t, send(testAPIKey), `{"subject":"static","scheme":"key","scopes":[]}`)
	if calls != 0 {
		t.Fatal("a static key was looked up")
	}
	assertJSON(t, send("db-0123456789abcdef"), `{"subject":"from-db","scheme":"key","scopes":["x"]}`)
	assertStatus(t, send("unknown-0123456789"), http.StatusUnauthorized)
	rec := send("fail-0123456789abcd")
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if strings.Contains(rec.Body.String(), "database") {
		t.Fatalf("the lookup's error reached the client: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "the database is down") {
		t.Fatalf("the lookup's error was not logged:\n%s", logs.String())
	}
	for _, key := range []string{testAPIKey, "db-0123456789abcdef", "fail-0123456789abcd", "unknown-0123456789"} {
		if strings.Contains(logs.String(), key) {
			t.Fatalf("the log carries the key %q:\n%s", key, logs.String())
		}
	}
	// A lookup that panics is a 500, recovered as a handler's panic is.
	panicking := mustBuild(t, keyApp(t, APIKeyOptions{Name: "X-API-Key", Lookup: func(*Context, string) (*APIKeyPrincipal, error) {
		panic("the lookup exploded")
	}}))
	req0 := httptest.NewRequest(http.MethodGet, "/me", nil)
	req0.Header.Set("X-API-Key", "db-0123456789abcdef")
	assertStatus(t, doRequest(t, panicking, req0), http.StatusInternalServerError)
	// A lookup alone is enough.
	lookupOnly := mustBuild(t, keyApp(t, APIKeyOptions{Name: "X-API-Key", Lookup: opts.Lookup}))
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("X-API-Key", "db-0123456789abcdef")
	assertStatus(t, doRequest(t, lookupOnly, req), http.StatusOK)
}

// TestAPIKeyVerifierLogsNoKey checks the access log and the error log never
// carry a key, whether it was accepted or refused.
func TestAPIKeyVerifierLogsNoKey(t *testing.T) {
	logger, logs := captureLogger(t)
	appOpts := quietOptions()
	appOpts.Logger = logger
	appOpts.SecuritySchemes = map[string]SecurityScheme{
		"key":   APIKeyVerifier(APIKeyOptions{Name: "X-API-Key", Keys: []APIKey{{ID: "acme", Key: testAPIKey}}}),
		"query": APIKeyVerifier(APIKeyOptions{In: "query", Name: "api_key", Keys: []APIKey{{ID: "acme", Key: testAPIKey}}}),
	}
	app := New(appOpts)
	app.Get("/me", me, WithSecurity(Require("key")))
	app.Get("/q", me, WithSecurity(Require("query")))
	mustBuild(t, app)
	for _, key := range []string{testAPIKey, "wrong-0123456789abc"} {
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		req.Header.Set("X-API-Key", key)
		doRequest(t, app, req)
		do(t, app, http.MethodGet, "/q?api_key="+key)
	}
	if strings.Contains(logs.String(), testAPIKey) || strings.Contains(logs.String(), "wrong-0123456789abc") {
		t.Fatalf("the log carries a key:\n%s", logs.String())
	}
}

// TestAPIKeyVerifierHoldsOnlyDigests checks no static key is kept in memory as
// it was given.
func TestAPIKeyVerifierHoldsOnlyDigests(t *testing.T) {
	secret := "plaintext-key-0123456789"
	scheme := APIKeyVerifier(APIKeyOptions{Name: "X-API-Key", Keys: []APIKey{{ID: "acme", Key: secret}}})
	var walk func(v reflect.Value) bool
	walk = func(v reflect.Value) bool {
		switch v.Kind() {
		case reflect.String:
			return strings.Contains(v.String(), secret)
		case reflect.Pointer, reflect.Interface:
			return !v.IsNil() && walk(v.Elem())
		case reflect.Struct:
			for i := range v.NumField() {
				if walk(v.Field(i)) {
					return true
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() == reflect.Uint8 && v.Kind() == reflect.Slice {
				return strings.Contains(string(v.Bytes()), secret)
			}
			for i := range v.Len() {
				if walk(v.Index(i)) {
					return true
				}
			}
		}
		return false
	}
	if walk(reflect.ValueOf(scheme)) {
		t.Fatal("the scheme holds the key in plain text")
	}
}

// TestAPIKeyVerifierComparesInConstantTime checks the comparison does the
// same work whichever key matches, or none: a verifier that stopped at the
// first match would answer the first key a thousand times faster than the
// last. The bound is loose, since it only has to tell constant from linear.
func TestAPIKeyVerifierComparesInConstantTime(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	keys := make([]APIKey, maxStaticAPIKeys)
	for i := range keys {
		keys[i] = APIKey{ID: fmt.Sprint(i), Key: fmt.Sprintf("key-%016d", i)}
	}
	scheme := APIKeyVerifier(APIKeyOptions{Name: "X-API-Key", Keys: keys})
	s := newAPIKeyScheme("key", scheme.verifier.apiKey)
	measure := func(key string) time.Duration {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", key)
		c := &Context{r: req}
		samples := make([]time.Duration, 41)
		for i := range samples {
			start := time.Now()
			for range 20 {
				s.verify(c)
			}
			samples[i] = time.Since(start)
		}
		sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
		return samples[len(samples)/2]
	}
	first := measure(keys[0].Key)
	last := measure(keys[len(keys)-1].Key)
	none := measure("key-none-of-these-0")
	slowest := max(first, last, none)
	fastest := min(first, last, none)
	if slowest > 4*fastest {
		t.Fatalf("first %s, last %s, none %s: the time depends on which key matched", first, last, none)
	}
}

// TestAPIKeyVerifierOptionsAreChecked covers every option that cannot be used.
func TestAPIKeyVerifierOptionsAreChecked(t *testing.T) {
	good := []APIKey{{ID: "a", Key: testAPIKey}}
	many := make([]APIKey, maxStaticAPIKeys+1)
	for i := range many {
		many[i] = APIKey{ID: fmt.Sprint(i), Key: fmt.Sprintf("key-%016d", i)}
	}
	cases := map[string]struct {
		opts APIKeyOptions
		want string
	}{
		"no name":         {APIKeyOptions{Keys: good}, "APIKeyOptions.Name must be a valid header name"},
		"bad header name": {APIKeyOptions{Name: "X API Key", Keys: good}, "APIKeyOptions.Name must be a valid header name"},
		"no query name":   {APIKeyOptions{In: "query", Keys: good}, "APIKeyOptions.Name must name the query"},
		"no cookie name":  {APIKeyOptions{In: "cookie", Keys: good}, "APIKeyOptions.Name must name the cookie"},
		"bad in":          {APIKeyOptions{In: "body", Name: "k", Keys: good}, `APIKeyOptions.In must be "header", "query" or "cookie", not "body"`},
		"no keys":         {APIKeyOptions{Name: "k"}, "APIKeyOptions names no key"},
		"no ID":           {APIKeyOptions{Name: "k", Keys: []APIKey{{Key: testAPIKey}}}, "APIKeyOptions.Keys[0] has no ID"},
		"short key":       {APIKeyOptions{Name: "k", Keys: []APIKey{{ID: "a", Key: "xq7z!"}}}, `APIKeyOptions.Keys[0] ("a") is 5 bytes, shorter than the minimum of 16`},
		"long key":        {APIKeyOptions{Name: "k", Keys: []APIKey{{ID: "a", Key: strings.Repeat("k", maxAPIKeyBytes+1)}}}, "could never match"},
		"duplicate ID":    {APIKeyOptions{Name: "k", Keys: []APIKey{{ID: "a", Key: testAPIKey}, {ID: "a", Key: testAPIKey + "2"}}}, `has the ID "a", as Keys[0] does`},
		"duplicate key":   {APIKeyOptions{Name: "k", Keys: []APIKey{{ID: "a", Key: testAPIKey}, {ID: "b", Key: testAPIKey}}}, `Keys[1] ("b") has the same key as Keys[0] ("a")`},
		"bad scope":       {APIKeyOptions{Name: "k", Keys: []APIKey{{ID: "a", Key: testAPIKey, Scopes: []string{"two words"}}}}, `grants "two words", which is not a valid scope`},
		"too many":        {APIKeyOptions{Name: "k", Keys: many}, "holds 1025 keys, over the limit of 1024"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			msg := buildError(t, keyApp(t, tc.opts))
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("build error = %s\nwant %q", msg, tc.want)
			}
			for _, k := range tc.opts.Keys {
				if len(k.Key) >= 5 && strings.Contains(msg, k.Key) {
					t.Fatalf("the build error quotes a key: %s", msg)
				}
			}
		})
	}
}
