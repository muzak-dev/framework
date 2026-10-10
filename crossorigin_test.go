package muzak

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"muzak.dev/framework/i18n"
)

// crossOriginOut reports that a handler ran.
type crossOriginOut struct {
	OK bool `json:"ok"`
}

// crossOriginApp builds an application on api.example.com with cross-origin
// protection, a trusted frontend origin and a bypassed webhook, and counts
// what reaches the middleware installed with Use and the handlers.
func crossOriginApp(t *testing.T, configure func(*AppOptions)) (*App, *atomic.Int32, *syncBuffer) {
	t.Helper()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	options.CrossOriginProtection = &CrossOriginOptions{
		TrustedOrigins:         []string{"https://app.example.com"},
		InsecureBypassPatterns: []string{"POST /webhooks/{provider}"},
	}
	if configure != nil {
		configure(&options)
	}
	reached := new(atomic.Int32)
	app := New(options)
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached.Add(1)
			next.ServeHTTP(w, r)
		})
	})
	answer := func(*Context, Empty) (crossOriginOut, error) { return crossOriginOut{OK: true}, nil }
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		app.Handle(method, "/items", answer)
	}
	app.Post("/webhooks/{provider}", func(*Context, webhookIn) (crossOriginOut, error) {
		return crossOriginOut{OK: true}, nil
	})
	return mustBuild(t, app), reached, logs
}

// webhookIn names the provider a webhook is from.
type webhookIn struct {
	Provider string `path:"provider"`
}

// crossOriginRequest sends method target to api.example.com with the given
// Sec-Fetch-Site and Origin, each left out when empty.
func crossOriginRequest(t *testing.T, app *App, method, target, site, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "api.example.com"
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

// TestCrossOriginMatrix is the whole decision, one request at a time: which
// methods are checked, which Sec-Fetch-Site values and Origins pass, and
// what trusted origins and bypass patterns let through.
func TestCrossOriginMatrix(t *testing.T) {
	t.Parallel()
	app, _, _ := crossOriginApp(t, nil)
	const (
		self    = "https://api.example.com"
		sibling = "https://evil.example.com"
		evil    = "https://evil.test"
		trusted = "https://app.example.com"
	)
	cases := []struct {
		name, method, target, site, origin string
		allowed                            bool
	}{
		{"same origin", "POST", "/items", "same-origin", self, true},
		{"typed by the user", "POST", "/items", "none", "", true},
		{"sibling subdomain", "POST", "/items", "same-site", sibling, false},
		{"another site", "POST", "/items", "cross-site", evil, false},
		{"another site without an origin", "POST", "/items", "cross-site", "", false},
		{"an unknown fetch site", "POST", "/items", "elsewhere", evil, false},
		{"an old browser from another site", "POST", "/items", "", evil, false},
		{"an old browser from a sibling", "POST", "/items", "", sibling, false},
		{"an old browser from this host", "POST", "/items", "", self, true},
		{"an old browser sending null", "POST", "/items", "", "null", false},
		{"no browser at all", "POST", "/items", "", "", true},
		{"trusted from another site", "POST", "/items", "cross-site", trusted, true},
		{"trusted from a sibling", "POST", "/items", "same-site", trusted, true},
		{"trusted by an old browser", "POST", "/items", "", trusted, true},
		{"trusted in another case", "POST", "/items", "cross-site", "https://APP.example.com", false},
		{"trusted with a port", "POST", "/items", "cross-site", "https://app.example.com:8443", false},
		{"trusted over http", "POST", "/items", "cross-site", "http://app.example.com", false},
		{"PUT from another site", "PUT", "/items", "cross-site", evil, false},
		{"PATCH from another site", "PATCH", "/items", "cross-site", evil, false},
		{"DELETE from another site", "DELETE", "/items", "cross-site", evil, false},
		{"GET from another site", "GET", "/items", "cross-site", evil, true},
		{"HEAD from another site", "HEAD", "/items", "cross-site", evil, true},
		{"OPTIONS from another site", "OPTIONS", "/items", "cross-site", evil, true},
		{"a bypassed webhook", "POST", "/webhooks/stripe", "cross-site", evil, true},
		{"beneath the webhook", "POST", "/webhooks/stripe/more", "cross-site", evil, false},
		{"the webhook through a dot segment", "POST", "/webhooks/../items", "cross-site", evil, false},
		{"the webhook with another method", "PUT", "/webhooks/stripe", "cross-site", evil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := crossOriginRequest(t, app, tc.method, tc.target, tc.site, tc.origin)
			refused := rec.Code == http.StatusForbidden
			if refused == tc.allowed {
				t.Fatalf("%s %s with Sec-Fetch-Site %q and Origin %q answered %d; want allowed=%v\n%s",
					tc.method, tc.target, tc.site, tc.origin, rec.Code, tc.allowed, rec.Body.String())
			}
			if refused {
				if code := decodeError(t, rec).Error.Code; code != CodeCrossOriginRequest {
					t.Errorf("the refusal is classified %q", code)
				}
			}
		})
	}
}

// TestCrossOriginRefusal covers what a refusal is: a 403 through the error
// renderer with its own code and message, uncacheable and varying on what it
// was decided by, written before any of the application ran, and logged with
// the origin in a form that cannot forge a line.
func TestCrossOriginRefusal(t *testing.T) {
	t.Parallel()
	app, reached, logs := crossOriginApp(t, nil)
	rec := crossOriginRequest(t, app, "POST", "/items", "cross-site", "https://evil.test\nforged")
	assertStatus(t, rec, http.StatusForbidden)
	body := decodeError(t, rec)
	if body.Error.Code != CodeCrossOriginRequest || body.Error.Message != crossOriginMessage || body.RequestID == "" {
		t.Errorf("the refusal is %+v", body)
	}
	vary := rec.Header().Values("Vary")
	if !varyNames(vary, "Origin") || !varyNames(vary, "Sec-Fetch-Site") {
		t.Errorf("Vary = %q, want Origin and Sec-Fetch-Site", vary)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("middleware installed with Use ran %d times for a refused request", n)
	}
	log := logs.String()
	if !strings.Contains(log, "refused a state-changing request from another origin") || !strings.Contains(log, `https://evil.test\\nforged`) {
		t.Errorf("the refusal is not logged with the origin escaped:\n%s", log)
	}
	// A request that passes reaches everything as usual.
	assertStatus(t, crossOriginRequest(t, app, "POST", "/items", "same-origin", "https://api.example.com"), http.StatusOK)
	if reached.Load() != 1 {
		t.Error("an allowed request did not reach the middleware")
	}
}

// TestCrossOriginRefusalAsProblemDetails shows the refusal is rendered by
// the application's renderer, problem details included.
func TestCrossOriginRefusalAsProblemDetails(t *testing.T) {
	t.Parallel()
	app, _, _ := crossOriginApp(t, func(o *AppOptions) {
		o.ProblemDetails = &ProblemOptions{TypeBase: "https://errors.example.com/"}
	})
	rec := crossOriginRequest(t, app, "DELETE", "/items", "cross-site", "https://evil.test")
	assertStatus(t, rec, http.StatusForbidden)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, ProblemContentType) {
		t.Fatalf("Content-Type = %q", ct)
	}
	var problem Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Status != http.StatusForbidden || problem.Code != CodeCrossOriginRequest ||
		problem.Type != "https://errors.example.com/"+CodeCrossOriginRequest || problem.Detail != crossOriginMessage {
		t.Errorf("the problem is %+v", problem)
	}
}

// TestCrossOriginRefusalIsTranslatable keeps the shipped English and the
// constant in step, as the drift guard does for every other sentence.
func TestCrossOriginRefusalIsTranslatable(t *testing.T) {
	t.Parallel()
	if got := i18n.Builtin().T("en", "muzak.security.cross_origin"); got != crossOriginMessage {
		t.Errorf("the shipped sentence is %q, want %q", got, crossOriginMessage)
	}
}

// TestCrossOriginWithCORS covers the composition: a preflight passes, since
// it changes nothing; the request it announced is still refused when its
// origin is allowed by CORS but not trusted, with the CORS headers that let
// the page read why; and is served once the origin is trusted too.
func TestCrossOriginWithCORS(t *testing.T) {
	t.Parallel()
	const spa = "https://spa.example.com"
	cors := CORSOptions{AllowedOrigins: []string{spa}, AllowCredentials: true}
	app, _, _ := crossOriginApp(t, func(o *AppOptions) { o.CORS = cors })

	req := httptest.NewRequest("OPTIONS", "/items", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", spa)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != spa {
		t.Fatalf("the preflight answered %d with %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}

	rec = crossOriginRequest(t, app, "PUT", "/items", "same-site", spa)
	assertStatus(t, rec, http.StatusForbidden)
	if rec.Header().Get("Access-Control-Allow-Origin") != spa || rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("the refusal to a CORS-allowed origin cannot be read by it: %v", rec.Header())
	}

	trusted, _, _ := crossOriginApp(t, func(o *AppOptions) {
		o.CORS = cors
		o.CrossOriginProtection.TrustedOrigins = []string{spa}
	})
	assertStatus(t, crossOriginRequest(t, trusted, "PUT", "/items", "same-site", spa), http.StatusOK)
}

// TestCrossOriginLeavesWebSocketsToTheirOwnCheck covers the handshake: a GET
// that cross-origin protection never refuses, so the WebSocket origin check
// decides, refusing a cross-origin handshake and accepting a listed one.
func TestCrossOriginLeavesWebSocketsToTheirOwnCheck(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.CrossOriginProtection = &CrossOriginOptions{}
	app := New(options)
	app.WS("/ws", wsEcho)
	app.WS("/open", wsEcho, WithWebSocket(WSOptions{AllowedOrigins: []string{"https://evil.test"}}))
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	_, response := dialRaw(t, server.URL, "/ws", "Origin", "https://evil.test", "Sec-Fetch-Site", "cross-site")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-origin handshake answered %d", response.StatusCode)
	}
	var body ErrorResponse
	if err := json.UnmarshalRead(response.Body, &body); err != nil || body.Error.Code == CodeCrossOriginRequest {
		t.Errorf("the handshake was refused by cross-origin protection rather than the WebSocket check: %+v, %v", body, err)
	}
	_, response = dialRaw(t, server.URL, "/open", "Origin", "https://evil.test", "Sec-Fetch-Site", "cross-site")
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("a handshake from an allowed origin answered %d", response.StatusCode)
	}
}

// TestCrossOriginCoversMounts shows the check sits in front of
// everything: a mounted handler is refused like a route.
func TestCrossOriginCoversMounts(t *testing.T) {
	t.Parallel()
	var mounted atomic.Int32
	options := quietOptions()
	options.CrossOriginProtection = &CrossOriginOptions{}
	app := New(options)
	app.Mount("/legacy", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { mounted.Add(1) }))
	mustBuild(t, app)
	assertStatus(t, crossOriginRequest(t, app, "POST", "/legacy/form", "cross-site", "https://evil.test"), http.StatusForbidden)
	if mounted.Load() != 0 {
		t.Error("a mounted handler received a forged request")
	}
}

// TestCrossOriginOptionsAreValidated covers every entry refused when the
// application is built.
func TestCrossOriginOptionsAreValidated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		opts  CrossOriginOptions
		wants []string
	}{
		{"trailing slash", CrossOriginOptions{TrustedOrigins: []string{"https://app.example.com/"}}, []string{"ends in a slash"}},
		{"upper case", CrossOriginOptions{TrustedOrigins: []string{"https://App.example.com"}}, []string{`write "https://app.example.com"`}},
		{"default port", CrossOriginOptions{TrustedOrigins: []string{"https://app.example.com:443"}}, []string{`write "https://app.example.com"`}},
		{"pattern", CrossOriginOptions{TrustedOrigins: []string{"https://*.example.com"}}, []string{"is a pattern"}},
		{"wildcard", CrossOriginOptions{TrustedOrigins: []string{"*"}}, []string{"is a pattern"}},
		{"null", CrossOriginOptions{TrustedOrigins: []string{"null"}}, []string{"is refused"}},
		{"no scheme", CrossOriginOptions{TrustedOrigins: []string{"app.example.com"}}, []string{"is not an origin"}},
		{"bad pattern", CrossOriginOptions{InsecureBypassPatterns: []string{"POST /x/{"}}, []string{"is not a pattern net/http accepts"}},
		{"empty pattern", CrossOriginOptions{InsecureBypassPatterns: []string{""}}, []string{"is not a pattern net/http accepts"}},
		{"conflicting patterns", CrossOriginOptions{InsecureBypassPatterns: []string{"POST /x", "POST /x"}}, []string{"is not a pattern net/http accepts"}},
		{"safe method", CrossOriginOptions{InsecureBypassPatterns: []string{"GET /x"}}, []string{"names GET", "bypasses nothing"}},
		{"head", CrossOriginOptions{InsecureBypassPatterns: []string{"HEAD /x"}}, []string{"names HEAD"}},
		{"safe method after a tab", CrossOriginOptions{InsecureBypassPatterns: []string{"GET\t/x"}}, []string{"names GET"}},
		{"the whole application", CrossOriginOptions{InsecureBypassPatterns: []string{"/"}}, []string{`"/" turns cross-origin protection off`}},
		{"the whole application for one method", CrossOriginOptions{InsecureBypassPatterns: []string{"POST /"}}, []string{`"POST /" turns cross-origin protection off`}},
		{"a whole host", CrossOriginOptions{InsecureBypassPatterns: []string{"PUT api.example.com/"}}, []string{`"PUT api.example.com/" turns cross-origin protection off`}},
		{"several", CrossOriginOptions{TrustedOrigins: []string{"null", "x"}, InsecureBypassPatterns: []string{"OPTIONS /x"}}, []string{
			"null", "not an origin", "names OPTIONS",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := quietOptions()
			options.CrossOriginProtection = &tc.opts
			message := buildError(t, New(options))
			for _, want := range tc.wants {
				if !strings.Contains(message, want) {
					t.Errorf("the build error does not mention %q:\n%s", want, message)
				}
			}
		})
	}
	// Every spelling a browser sends is accepted, with a port and over
	// plain HTTP included.
	options := quietOptions()
	options.CrossOriginProtection = &CrossOriginOptions{
		TrustedOrigins:         []string{"https://app.example.com", "http://localhost:5173", "https://[::1]:8443"},
		InsecureBypassPatterns: []string{"POST /hooks/", "/callback", "example.com/form"},
	}
	mustBuild(t, New(options))
}

// TestBypassPatternCoveringASubtreeIsWarnedOf covers a bypass pattern ending
// in a slash, which matches every path beneath it as a ServeMux pattern does.
// Routes here conventionally end in one, so listing a route's path that way
// silently turns the check off below it: the application is built, and a
// warning says so and how to match the one path.
func TestBypassPatternCoveringASubtreeIsWarnedOf(t *testing.T) {
	t.Parallel()
	for pattern, warned := range map[string]bool{
		"POST /hooks/":               true,
		"example.com/hooks/":         true,
		"POST /hooks/{$}":            false,
		"POST /hooks":                false,
		"POST /hooks/{provider}":     false,
		"POST /hooks/{provider...}":  false,
		"POST /hooks/{provider}/{$}": false,
	} {
		logger, logs := captureLogger(t)
		options := quietOptions()
		options.Logger = logger
		options.CrossOriginProtection = &CrossOriginOptions{InsecureBypassPatterns: []string{pattern}}
		mustBuild(t, New(options))
		line := `"level":"WARN","msg":"muzak: CrossOriginOptions.InsecureBypassPatterns entry \"` + pattern + `\"`
		if got := strings.Contains(logs.String(), line) && strings.Contains(logs.String(), "{$}"); got != warned {
			t.Errorf("%q: warned = %t, want %t; the log holds:\n%s", pattern, got, warned, logs.String())
		}
	}
}

// TestTrustedOriginPatternIsExplainedOnItsOwnTerms is the regression test for
// the build error a pattern in TrustedOrigins gave, which was the one CORS
// gives: it spoke of AllowedOrigins and sent the reader to AllowOriginFunc,
// neither of which CrossOriginOptions has.
func TestTrustedOriginPatternIsExplainedOnItsOwnTerms(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"https://*.example.com", "*"} {
		options := quietOptions()
		options.CrossOriginProtection = &CrossOriginOptions{TrustedOrigins: []string{entry}}
		message := buildError(t, New(options))
		if !strings.Contains(message, "TrustedOrigins matches each origin exactly") || containsAny(message, "AllowedOrigins", "AllowOriginFunc") {
			t.Errorf("the build error for %q is not about TrustedOrigins:\n%s", entry, message)
		}
	}
}
