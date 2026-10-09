package muzak

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jwksServer serves a key set the test changes as it goes, counting every
// request.
type jwksServer struct {
	*httptest.Server
	mu           sync.Mutex
	body         []byte
	status       int
	cacheControl string
	delay        time.Duration
	hits         atomic.Int64
}

func newJWKSServer(t testing.TB, keys ...map[string]any) *jwksServer {
	t.Helper()
	s := &jwksServer{status: http.StatusOK}
	s.setKeys(t, keys...)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		body, status, cc, delay := s.body, s.status, s.cacheControl, s.delay
		s.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if cc != "" {
			w.Header().Set("Cache-Control", cc)
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) setKeys(t testing.TB, keys ...map[string]any) {
	list := make([]any, len(keys))
	for i, k := range keys {
		list[i] = k
	}
	s.setBody(mustJSON(t, map[string]any{"keys": list}))
}

func (s *jwksServer) setBody(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = body
}

func (s *jwksServer) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// loopbackClient is the client a test reaches its own server with, which the
// default client refuses.
func loopbackClient() *Client {
	return NewClient(ClientOptions{AllowPrivateNetworks: true, MaxAttempts: 1})
}

// jwksOptions is a scheme fetching its keys from the test server.
func jwksOptions(url string) JWTOptions {
	return JWTOptions{
		Issuers:    []string{testIssuer},
		Audience:   testAudience,
		Algorithms: []string{"RS256", "ES256"},
		JWKS:       JWKSOptions{URL: url, Client: loopbackClient(), MinRefreshInterval: time.Minute},
	}
}

// jwksApp builds an application whose "jwt" scheme reads the clock.
func jwksApp(t *testing.T, opts JWTOptions, clock *testClock) *App {
	t.Helper()
	scheme := JWTBearer(opts)
	scheme.verifier.jwt.now = clock.now
	return mustBuild(t, authApp(t, map[string]SecurityScheme{"jwt": scheme}, Require("jwt")))
}

// jwksOf returns the application's key set cache for the "jwt" scheme.
func jwksOf(app *App) *jwksCache {
	return app.auth.schemes["jwt"].(*jwtScheme).jwks
}

func k1() map[string]any {
	return rsaJWK(&testRSAKey().PublicKey, map[string]any{"kid": "k1", "use": "sig", "alg": "RS256"})
}

func k2() map[string]any {
	return rsaJWK(&testRSAKeyOther().PublicKey, map[string]any{"kid": "k2", "use": "sig"})
}

// TestJWKSVerifiesWithFetchedKeys checks the key set is fetched for the first
// token and then cached.
func TestJWKSVerifiesWithFetchedKeys(t *testing.T) {
	server := newJWKSServer(t, k1(), ecJWK(&testECKey256().PublicKey, map[string]any{"kid": "e1"}))
	clock := &testClock{at: testNow}
	app := jwksApp(t, jwksOptions(server.URL), clock)
	for range 5 {
		assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil)), http.StatusOK)
		assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "ES256", testECKey256(), "e1", nil)), http.StatusOK)
	}
	if hits := server.hits.Load(); hits != 1 {
		t.Fatalf("the key set was fetched %d times, want 1", hits)
	}
	// The kid selects the key: k1's token naming e1 is refused.
	token := signJWT(t, "RS256", testRSAKey(), "e1", nil)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", token), token)
}

// TestJWKSPicksUpRotatedKeys checks that a token naming a kid the set does not
// hold makes it fetched again, once MinRefreshInterval allows.
func TestJWKSPicksUpRotatedKeys(t *testing.T) {
	server := newJWKSServer(t, k1())
	clock := &testClock{at: testNow}
	app := jwksApp(t, jwksOptions(server.URL), clock)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil)), http.StatusOK)

	server.setKeys(t, k1(), k2())
	rotated := signJWT(t, "RS256", testRSAKeyOther(), "k2", nil)
	// Within the interval of the last fetch, an unknown kid is refused
	// without another fetch.
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", rotated), rotated)
	if hits := server.hits.Load(); hits != 1 {
		t.Fatalf("fetched %d times within the interval, want 1", hits)
	}
	clock.advance(time.Minute)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", rotated), http.StatusOK)
	if hits := server.hits.Load(); hits != 2 {
		t.Fatalf("fetched %d times, want 2", hits)
	}
	// A retired key stops verifying once the set is fetched without it.
	server.setKeys(t, k2())
	clock.advance(time.Minute)
	stale := signJWT(t, "RS256", testRSAKey(), "k3", nil)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", stale), stale)
	old := signJWT(t, "RS256", testRSAKey(), "k1", nil)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", old), old)
}

// TestJWKSUnknownKidFloodIsBounded sends many concurrent tokens naming random
// kids, and checks the issuer is asked at most once per MinRefreshInterval.
func TestJWKSUnknownKidFloodIsBounded(t *testing.T) {
	server := newJWKSServer(t, k1())
	clock := &testClock{at: testNow}
	app := jwksApp(t, jwksOptions(server.URL), clock)
	flood := func() {
		var wg sync.WaitGroup
		for range 200 {
			wg.Go(func() {
				buf := make([]byte, 8)
				_, _ = rand.Read(buf)
				token := signJWT(t, "RS256", testRSAKey(), hex.EncodeToString(buf), nil)
				if rec := withBearer(t, app, http.MethodGet, "/me", token); rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", rec.Code)
				}
			})
		}
		wg.Wait()
	}
	flood()
	if hits := server.hits.Load(); hits != 1 {
		t.Fatalf("200 random kids fetched the key set %d times, want 1", hits)
	}
	clock.advance(time.Minute)
	flood()
	if hits := server.hits.Load(); hits != 2 {
		t.Fatalf("after one interval, %d fetches, want 2", hits)
	}
	if got := jwksOf(app).fetches.Load(); got != 2 {
		t.Fatalf("the cache started %d fetches, want 2", got)
	}
}

// TestJWKSKeepsTheLastKeysWhenAFetchFails checks that an issuer answering with
// an error, garbage or an oversized document leaves the last good set in use.
func TestJWKSKeepsTheLastKeysWhenAFetchFails(t *testing.T) {
	server := newJWKSServer(t, k1())
	clock := &testClock{at: testNow}
	logger, logs := captureLogger(t)
	opts := jwksOptions(server.URL)
	opts.JWKS.MaxBytes = 4 << 10
	scheme := JWTBearer(opts)
	scheme.verifier.jwt.now = clock.now
	appOpts := quietOptions()
	appOpts.Logger = logger
	appOpts.SecuritySchemes = map[string]SecurityScheme{"jwt": scheme}
	app := New(appOpts)
	app.Get("/me", me, WithSecurity(Require("jwt")))
	mustBuild(t, app)

	// Signed afresh each time, since the clock moves past each one's expiry.
	fresh := func(key any, kid string) string {
		return signJWT(t, "RS256", key, kid, map[string]any{"exp": clock.now().Add(time.Hour).Unix(), "iat": clock.now().Unix()})
	}
	token := fresh(testRSAKey(), "k1")
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusOK)
	failures := []func(){
		func() { server.setStatus(http.StatusInternalServerError) },
		func() { server.setStatus(http.StatusOK); server.setBody([]byte("<html>maintenance</html>")) },
		func() { server.setBody([]byte(`{"keys":[{"kty":"oct","k":"c2VjcmV0"}]}`)) },
		func() { server.setBody([]byte(`{"keys":[` + strings.Repeat(`{"kty":"RSA"},`, 300) + `{}]}`)) },
		func() { server.setBody([]byte(`{"nokeys":true}`)) },
	}
	for i, fail := range failures {
		fail()
		// Past the cache's life, so the next token makes it fetch again.
		clock.advance(DefaultJWKSMaxCacheTTL)
		before := server.hits.Load()
		assertStatus(t, withBearer(t, app, http.MethodGet, "/me", fresh(testRSAKey(), "k1")), http.StatusOK)
		if server.hits.Load() != before+1 {
			t.Fatalf("failure %d: the expired set was not fetched again", i)
		}
	}
	if !strings.Contains(logs.String(), "the key set could not be refreshed") {
		t.Fatalf("the failures were not logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "maintenance") || strings.Contains(logs.String(), strings.Split(token, ".")[1]) {
		t.Fatalf("the log carries the document or the token:\n%s", logs.String())
	}
	// Once the issuer recovers, the set is replaced.
	server.setKeys(t, k2())
	clock.advance(DefaultJWKSMaxCacheTTL)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", fresh(testRSAKeyOther(), "k2")), http.StatusOK)
}

// TestJWKSWithNoKeysYetAnswers503 checks that a token that cannot be judged
// because no set was ever fetched is the server's failure, not the client's.
func TestJWKSWithNoKeysYetAnswers503(t *testing.T) {
	cases := map[string]func(*jwksServer){
		"server error": func(s *jwksServer) { s.setStatus(http.StatusBadGateway) },
		"too large":    func(s *jwksServer) { s.setBody([]byte(`{"keys":[],"pad":"` + strings.Repeat("x", 8<<10) + `"}`)) },
		"too many keys": func(s *jwksServer) {
			keys := make([]map[string]any, 0, 3)
			for range 3 {
				keys = append(keys, k1())
			}
			s.setKeys(t, keys...)
		},
		"no usable key": func(s *jwksServer) {
			s.setKeys(t, rsaJWK(&testRSAKey().PublicKey, map[string]any{"kid": "k1", "use": "enc"}))
		},
		"duplicate members": func(s *jwksServer) { s.setBody([]byte(`{"keys":[],"keys":[]}`)) },
		"too deep": func(s *jwksServer) {
			s.setBody([]byte(`{"keys":[],"x":` + strings.Repeat("[", 20) + strings.Repeat("]", 20) + `}`))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			server := newJWKSServer(t, k1())
			setup(server)
			opts := jwksOptions(server.URL)
			opts.JWKS.MaxBytes = 4 << 10
			opts.JWKS.MaxKeys = 2
			app := jwksApp(t, opts, &testClock{at: testNow})
			token := signJWT(t, "RS256", testRSAKey(), "k1", nil)
			rec := withBearer(t, app, http.MethodGet, "/me", token)
			assertStatus(t, rec, http.StatusServiceUnavailable)
			assertNoTokenLeak(t, rec, token)
			// Further tokens within the interval do not fetch again.
			withBearer(t, app, http.MethodGet, "/me", token)
			if hits := server.hits.Load(); hits != 1 {
				t.Fatalf("fetched %d times, want 1", hits)
			}
		})
	}
}

// TestJWKSSkipsKeysItCannotUse checks that one unusable key does not cost the
// rest of the set.
func TestJWKSSkipsKeysItCannotUse(t *testing.T) {
	server := newJWKSServer(t,
		map[string]any{"kty": "oct", "kid": "k1", "k": "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0"},
		rsaJWK(&testRSAKeyOther().PublicKey, map[string]any{"kid": "k1", "use": "enc"}),
		map[string]any{"kty": "RSA", "kid": "k1", "n": 7},
		k1(),
	)
	app := jwksApp(t, jwksOptions(server.URL), &testClock{at: testNow})
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil)), http.StatusOK)
	// A token signed with HMAC using the published "oct" key is refused: a
	// symmetric key in a public set is a secret anyone can read.
	forged := signJWT(t, "HS256", []byte("secretsecretsecretsecretsecretse"), "k1", nil)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", forged), forged)
}

// TestJWKSSlowServerIsBoundedByTimeout checks that an issuer that never
// answers costs a request the fetch timeout, and nothing longer.
func TestJWKSSlowServerIsBoundedByTimeout(t *testing.T) {
	server := newJWKSServer(t, k1())
	server.mu.Lock()
	server.delay = 10 * time.Second
	server.mu.Unlock()
	opts := jwksOptions(server.URL)
	opts.JWKS.Timeout = 100 * time.Millisecond
	app := jwksApp(t, opts, &testClock{at: testNow})
	start := time.Now()
	rec := withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil))
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the request waited %s for a fetch bounded at 100ms", elapsed)
	}
}

// TestJWKSWaitersStopWithTheirRequest checks that a request waiting on a
// fetch another request started stops waiting when its own context ends.
func TestJWKSWaitersStopWithTheirRequest(t *testing.T) {
	server := newJWKSServer(t, k1())
	server.mu.Lock()
	server.delay = 2 * time.Second
	server.mu.Unlock()
	opts := jwksOptions(server.URL)
	opts.JWKS.Timeout = 5 * time.Second
	app := jwksApp(t, opts, &testClock{at: testNow})
	token := signJWT(t, "RS256", testRSAKey(), "k1", nil)
	first := make(chan int)
	go func() { first <- withBearer(t, app, http.MethodGet, "/me", token).Code }()
	for jwksOf(app).fetches.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	rec := doRequest(t, app, req)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the waiter waited %s after its context ended", elapsed)
	}
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("the request that fetched got %d", code)
	}
}

// TestJWKSDefaultClientRefusesPrivateNetworks checks the default client is the
// SSRF-safe one: a key set on a private address is not fetched unless the
// application opts in.
func TestJWKSDefaultClientRefusesPrivateNetworks(t *testing.T) {
	server := newJWKSServer(t, k1())
	logger, logs := captureLogger(t)
	opts := jwksOptions(server.URL)
	opts.JWKS.Client = nil
	appOpts := quietOptions()
	appOpts.Logger = logger
	appOpts.SecuritySchemes = map[string]SecurityScheme{"jwt": testScheme(opts)}
	app := New(appOpts)
	app.Get("/me", me, WithSecurity(Require("jwt")))
	mustBuild(t, app)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil)), http.StatusServiceUnavailable)
	if server.hits.Load() != 0 {
		t.Fatal("the default client reached a loopback address")
	}
	if !strings.Contains(logs.String(), "refused") {
		t.Fatalf("the refusal was not logged:\n%s", logs.String())
	}
	// The client built for the scheme is released with the application.
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jwksOf(app).owned == nil {
		t.Fatal("no client was built for the scheme")
	}
	// An *http.Client is accepted as well, and decides for itself.
	opts.JWKS.Client = server.Client()
	app = mustBuild(t, authApp(t, map[string]SecurityScheme{"jwt": testScheme(opts)}, Require("jwt")))
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil)), http.StatusOK)
}

// redirectedDoer answers a fetch as a client that has followed a redirect
// does: the response's Request is the last request sent, here to final.
type redirectedDoer struct {
	body  []byte
	final string
}

func (d redirectedDoer) Do(req *http.Request) (*http.Response, error) {
	last := req.Clone(req.Context())
	u, err := url.Parse(d.final)
	if err != nil {
		return nil, err
	}
	last.URL = u
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(d.body)),
		Request:    last,
	}, nil
}

// TestJWKSRefusesKeysRedirectedToPlainHTTP checks that keys are used only when
// they arrived over https, wherever the client went to get them. An
// *http.Client follows a redirect from the https URL configured down to plain
// http, where anyone on the path could replace the keys with their own and
// sign any token they liked.
func TestJWKSRefusesKeysRedirectedToPlainHTTP(t *testing.T) {
	body := mustJSON(t, map[string]any{"keys": []any{k1()}})
	cases := []struct {
		final string
		want  int
	}{
		{"http://keys.example.net/jwks?tenant=secret", http.StatusServiceUnavailable},
		{"https://keys.example.net/jwks", http.StatusOK},
		{"http://127.0.0.1:8080/jwks", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.final, func(t *testing.T) {
			logger, logs := captureLogger(t)
			opts := jwksOptions("https://login.example.com/jwks")
			opts.JWKS.Client = redirectedDoer{body: body, final: tc.final}
			appOpts := quietOptions()
			appOpts.Logger = logger
			appOpts.SecuritySchemes = map[string]SecurityScheme{"jwt": testScheme(opts)}
			app := New(appOpts)
			app.Get("/me", me, WithSecurity(Require("jwt")))
			mustBuild(t, app)
			assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil)), tc.want)
			if tc.want == http.StatusOK {
				return
			}
			if !strings.Contains(logs.String(), "plain http") {
				t.Fatalf("the refusal was not logged:\n%s", logs.String())
			}
			if strings.Contains(logs.String(), "secret") {
				t.Fatalf("the log quotes the URL redirected to:\n%s", logs.String())
			}
		})
	}
}

// TestJWKSOptionsAreChecked covers every JWKS option that cannot be used.
func TestJWKSOptionsAreChecked(t *testing.T) {
	cases := map[string]struct {
		jwks JWKSOptions
		want string
	}{
		"plain http":          {JWKSOptions{URL: "http://login.example.com/jwks"}, "must be https"},
		"no host":             {JWKSOptions{URL: "https:///jwks"}, "absolute https URL"},
		"relative":            {JWKSOptions{URL: "/jwks"}, "absolute https URL"},
		"credentials":         {JWKSOptions{URL: "https://user:pass@login.example.com/jwks"}, "no credentials"},
		"fragment":            {JWKSOptions{URL: "https://login.example.com/jwks#x"}, "no fragment"},
		"ftp":                 {JWKSOptions{URL: "ftp://login.example.com/jwks"}, "must be https"},
		"negative timeout":    {JWKSOptions{URL: "https://a.example/j", Timeout: -1}, "JWTOptions.JWKS.Timeout is -1ns"},
		"negative interval":   {JWKSOptions{URL: "https://a.example/j", MinRefreshInterval: -1}, "MinRefreshInterval"},
		"min over max":        {JWKSOptions{URL: "https://a.example/j", MinCacheTTL: time.Hour, MaxCacheTTL: time.Minute}, "is longer than MaxCacheTTL"},
		"negative bytes":      {JWKSOptions{URL: "https://a.example/j", MaxBytes: -1}, "MaxBytes is -1"},
		"too many keys":       {JWKSOptions{URL: "https://a.example/j", MaxKeys: maxJWKSKeysCeiling + 1}, "MaxKeys is 1025"},
		"no URL":              {JWKSOptions{MaxKeys: 3}, "JWTOptions.JWKS is configured but has no URL"},
		"negative keys":       {JWKSOptions{URL: "https://a.example/j", MaxKeys: -1}, "MaxKeys is -1"},
		"negative min ttl":    {JWKSOptions{URL: "https://a.example/j", MinCacheTTL: -1}, "MinCacheTTL"},
		"negative max ttl":    {JWKSOptions{URL: "https://a.example/j", MaxCacheTTL: -1}, "MaxCacheTTL"},
		"http to public name": {JWKSOptions{URL: "http://127.0.0.1.nip.io/jwks"}, "must be https"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := jwtOptions()
			opts.JWKS = tc.jwks
			if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, tc.want) {
				t.Fatalf("build error = %s\nwant %q", msg, tc.want)
			}
		})
	}
	for _, url := range []string{"http://127.0.0.1:9/jwks", "http://[::1]:9/jwks", "http://localhost:9/jwks", "https://login.example.com/jwks"} {
		opts := jwtOptions()
		opts.JWKS = JWKSOptions{URL: url}
		mustBuild(t, jwtApp(t, opts))
	}
}

// TestJWKSClientFailuresAreLoggedWithoutTheURL checks a client failure and a
// read failure are logged with their reason, and without the URL, whose query
// an issuer may consider private.
func TestJWKSClientFailuresAreLoggedWithoutTheURL(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	server := newJWKSServer(t, k1())
	server.setBody([]byte(`{"keys":[],"pad":"` + strings.Repeat("x", 3000) + `"}`))
	cases := map[string]struct {
		url    string
		client HTTPDoer
		want   string
	}{
		"connection refused": {closed.URL + "/keys?secret=s3cr3t", &http.Client{}, "refused"},
		"read past the client's limit": {server.URL + "/keys?secret=s3cr3t",
			NewClient(ClientOptions{AllowPrivateNetworks: true, MaxAttempts: 1, MaxResponseBytes: 1024}), "exceeds the client's limit"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logger, logs := captureLogger(t)
			opts := jwksOptions(tc.url)
			opts.JWKS.Client = tc.client
			appOpts := quietOptions()
			appOpts.Logger = logger
			appOpts.SecuritySchemes = map[string]SecurityScheme{"jwt": testScheme(opts)}
			app := New(appOpts)
			app.Get("/me", me, WithSecurity(Require("jwt")))
			mustBuild(t, app)
			assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "k1", nil)), http.StatusServiceUnavailable)
			if !strings.Contains(logs.String(), tc.want) || strings.Contains(logs.String(), "s3cr3t") {
				t.Fatalf("log:\n%s", logs.String())
			}
		})
	}
}

// panickingDoer is a client that panics, the worst an injected client can do
// short of hanging.
type panickingDoer struct{ calls atomic.Int32 }

func (d *panickingDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	panic("the client exploded")
}

// TestJWKSClientPanics checks a panic in the client is a 500 for the request
// that fetched, leaves no fetch stuck in flight, and on the background
// goroutine is logged rather than ending the process.
func TestJWKSClientPanics(t *testing.T) {
	doer := &panickingDoer{}
	clock := &testClock{at: testNow}
	logger, logs := captureLogger(t)
	opts := jwksOptions("https://login.example.com/jwks")
	opts.JWKS.Client = doer
	scheme := JWTBearer(opts)
	scheme.verifier.jwt.now = clock.now
	appOpts := quietOptions()
	appOpts.Logger = logger
	appOpts.SecuritySchemes = map[string]SecurityScheme{"jwt": scheme}
	app := New(appOpts)
	app.Get("/me", me, WithSecurity(Require("jwt")))
	mustBuild(t, app)
	token := signJWT(t, "RS256", testRSAKey(), "k1", nil)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusInternalServerError)
	clock.advance(time.Minute)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusInternalServerError)
	if doer.calls.Load() != 2 {
		t.Fatalf("the client was called %d times, want 2: a panicking fetch was left in flight", doer.calls.Load())
	}
	clock.advance(time.Minute)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "the key set refresh panicked") {
		if time.Now().After(deadline) {
			t.Fatalf("the background panic was not logged:\n%s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoGoroutineLeaks(t)
}

// TestJWKSConcurrentRequestsDuringRefresh drives requests while the
// background refresh runs, for the race detector.
func TestJWKSConcurrentRequestsDuringRefresh(t *testing.T) {
	server := newJWKSServer(t, k1(), k2())
	server.mu.Lock()
	server.cacheControl = "no-store"
	server.mu.Unlock()
	opts := jwksOptions(server.URL)
	opts.JWKS.MinRefreshInterval = time.Millisecond
	app := mustBuild(t, authApp(t, map[string]SecurityScheme{"jwt": JWTBearer(opts)}, Require("jwt")))
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := map[string]any{"exp": now.Add(time.Hour).Unix(), "iat": now.Unix()}
	tokens := []string{
		signJWT(t, "RS256", testRSAKey(), "k1", claims),
		signJWT(t, "RS256", testRSAKeyOther(), "k2", claims),
		signJWT(t, "RS256", testRSAKey(), "k9", claims),
	}
	var wg sync.WaitGroup
	for i := range 60 {
		wg.Go(func() {
			rec := withBearer(t, app, http.MethodGet, "/me", tokens[i%3])
			if want := map[int]int{0: 200, 1: 200, 2: 401}[i%3]; rec.Code != want {
				t.Errorf("token %d: status = %d, want %d", i%3, rec.Code, want)
			}
		})
	}
	wg.Wait()
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestJWKSCacheTTL checks Cache-Control is honoured within the bounds.
func TestJWKSCacheTTL(t *testing.T) {
	lo, hi := 5*time.Minute, 24*time.Hour
	cases := []struct {
		header []string
		want   time.Duration
	}{
		{nil, time.Hour},
		{[]string{"max-age=600"}, 10 * time.Minute},
		{[]string{"public, max-age=7200, must-revalidate"}, 2 * time.Hour},
		{[]string{"max-age=1"}, lo},
		{[]string{"max-age=0"}, lo},
		{[]string{"max-age=99999999"}, hi},
		{[]string{"max-age=999999999999999999999"}, time.Hour},
		{[]string{"no-store"}, lo},
		{[]string{"max-age=3600", "no-cache"}, lo},
		{[]string{`max-age="900"`}, 15 * time.Minute},
		{[]string{"MAX-AGE=900"}, 15 * time.Minute},
		{[]string{"max-age=1200, max-age=900"}, 15 * time.Minute},
		{[]string{"max-age=-5"}, time.Hour},
		{[]string{"max-age=abc"}, time.Hour},
		{[]string{"s-maxage=60"}, time.Hour},
	}
	for _, tc := range cases {
		h := http.Header{}
		for _, v := range tc.header {
			h.Add("Cache-Control", v)
		}
		if got := cacheTTL(h, lo, hi); got != tc.want {
			t.Errorf("cacheTTL(%q) = %s, want %s", tc.header, got, tc.want)
		}
	}
}

// TestJWKSHonoursCacheControlOnTheWire checks the cached set expires when the
// response said it does.
func TestJWKSHonoursCacheControlOnTheWire(t *testing.T) {
	server := newJWKSServer(t, k1())
	server.mu.Lock()
	server.cacheControl = "max-age=600"
	server.mu.Unlock()
	clock := &testClock{at: testNow}
	app := jwksApp(t, jwksOptions(server.URL), clock)
	token := signJWT(t, "RS256", testRSAKey(), "k1", nil)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusOK)
	clock.advance(9 * time.Minute)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusOK)
	if server.hits.Load() != 1 {
		t.Fatal("fetched again before max-age")
	}
	clock.advance(time.Minute)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusOK)
	if server.hits.Load() != 2 {
		t.Fatal("not fetched again once max-age passed")
	}
}

// TestJWKSBackgroundRefreshRunsWithTheLifecycle checks the set is fetched when
// the application starts, before any token, and that stopping the application
// leaves nothing running.
func TestJWKSBackgroundRefreshRunsWithTheLifecycle(t *testing.T) {
	server := newJWKSServer(t, k1())
	app := mustBuild(t, authApp(t, map[string]SecurityScheme{"jwt": JWTBearer(jwksOptions(server.URL))}, Require("jwt")))
	for round := range 2 {
		if err := app.StartLifecycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for jwksOf(app).current.Load() == nil {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the key set was not fetched in the background", round)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := app.StopLifecycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	assertNoGoroutineLeaks(t)
	// The lifecycle component is named after the scheme.
	if name := jwksOf(app).Name(); name != "jwks jwt" {
		t.Fatalf("Name() = %q", name)
	}
}

// TestJWKSStopCancelsAFetchInFlight checks a stop does not wait out a fetch
// to an issuer that hangs.
func TestJWKSStopCancelsAFetchInFlight(t *testing.T) {
	server := newJWKSServer(t, k1())
	server.mu.Lock()
	server.delay = time.Minute
	server.mu.Unlock()
	opts := jwksOptions(server.URL)
	opts.JWKS.Timeout = time.Minute
	app := mustBuild(t, authApp(t, map[string]SecurityScheme{"jwt": JWTBearer(opts)}, Require("jwt")))
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for server.hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stopping took %s", elapsed)
	}
	assertNoGoroutineLeaks(t)
	// A second stop is harmless.
	if err := jwksOf(app).Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// blockingDoer is a client that ignores its request's context and answers
// only when released, the worst an injected client can do.
type blockingDoer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingDoer) Do(*http.Request) (*http.Response, error) {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return nil, context.Canceled
}

// TestJWKSStopIsBoundedByItsContext checks that a stop gives up when its own
// deadline passes, even under a client that does not honour cancellation, and
// that the refresh still ends once the client returns.
func TestJWKSStopIsBoundedByItsContext(t *testing.T) {
	doer := &blockingDoer{started: make(chan struct{}), release: make(chan struct{})}
	opts := jwksOptions("https://login.example.com/jwks")
	opts.JWKS.Client = doer
	app := mustBuild(t, authApp(t, map[string]SecurityScheme{"jwt": JWTBearer(opts)}, Require("jwt")))
	cache := jwksOf(app)
	if err := cache.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Starting twice starts one refresh.
	if err := cache.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-doer.started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := cache.Stop(ctx); err == nil || !strings.Contains(err.Error(), `the key set refresh of scheme "jwt" did not stop in time`) {
		t.Fatalf("Stop() = %v", err)
	}
	close(doer.release)
	assertNoGoroutineLeaks(t)
}

// TestJWKSBackgroundRetriesWithBackoff checks the delay between background
// attempts while fetches fail, and before the next refresh once one succeeds.
func TestJWKSBackgroundRetriesWithBackoff(t *testing.T) {
	clock := &testClock{at: testNow}
	j := &jwksCache{opts: JWKSOptions{MinRefreshInterval: 30 * time.Second}, now: clock.now}
	if got := j.nextRefresh(); got != 30*time.Second {
		t.Fatalf("with no set, nextRefresh() = %s", got)
	}
	for failures, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute, 5: 5 * time.Minute, 50: 5 * time.Minute} {
		j.failures = failures
		if got := j.nextRefresh(); got != want {
			t.Errorf("after %d failures, nextRefresh() = %s, want %s", failures, got, want)
		}
	}
	j.failures = 0
	j.current.Store(&jwksSnapshot{keys: &keySet{}, fetched: testNow, expires: testNow.Add(time.Hour)})
	if got := j.nextRefresh(); got != 54*time.Minute {
		t.Fatalf("nextRefresh() = %s, want 54m", got)
	}
	if j.due() {
		t.Fatal("a fresh set is due")
	}
	clock.advance(54 * time.Minute)
	if !j.due() {
		t.Fatal("a set in the last tenth of its life is not due")
	}
	if got := j.nextRefresh(); got != 30*time.Second {
		t.Fatalf("a due set waits %s, want the minimum", got)
	}
}
