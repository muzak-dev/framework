package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSBuildFailureIsLoggedLikeAnyOther is the regression test for a build
// failure with no reason anywhere: a CORS policy that cannot be served safely
// was recorded only after the start-up log had already said the application
// was built, so ServeHTTP answered every request with a bare 500 while the log
// held nothing but "Registered 1 route".
func TestCORSBuildFailureIsLoggedLikeAnyOther(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.CORS = CORSOptions{AllowedOrigins: []string{"*"}, AllowCredentials: true}
	app := New(opts)
	app.Get("/x", okHandler)

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an application that did not build", rec.Code)
	}
	out := logs.String()
	if !strings.Contains(out, "could not be built") || !strings.Contains(out, "wildcard CORS origin") {
		t.Errorf("the build failure and its reason were not logged:\n%s", out)
	}
	if strings.Contains(out, "Registered") {
		t.Errorf("the routes were reported registered for an application that did not build:\n%s", out)
	}
}

// TestCORSRefusesAnOriginThatCanNeverMatch is the regression test for
// AllowedOrigins entries that were accepted and then never matched anything,
// because an origin is compared exactly with the Origin header and a browser
// sends a scheme, a host and any non-default port, lowercased, with nothing
// after them. A trailing slash, a path or a "*.example.com" pattern quietly
// allowed nobody, and "null", which a sandboxed iframe or a file: page sends
// and any page can arrange to, would have allowed everybody who tried. Each is
// now a build error saying what to write instead, reported together.
func TestCORSRefusesAnOriginThatCanNeverMatch(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		"https://app.example.com/":           `write "https://app.example.com"`,
		"https://app.example.com/callback":   `write "https://app.example.com"`,
		"https://app.example.com?x=1":        `write "https://app.example.com"`,
		"https://app.example.com#top":        `write "https://app.example.com"`,
		"https://user@app.example.com":       `write "https://app.example.com"`,
		"HTTPS://App.Example.COM":            `write "https://app.example.com"`,
		"https://app.example.com:443":        `write "https://app.example.com"`,
		"http://localhost:80/":               `write "http://localhost"`,
		"http://[::1]:80":                    `write "http://[::1]"`,
		"https://b\u00fccher.example":        "ASCII",
		"*.example.com":                      "AllowOriginFunc",
		"https://*.example.com":              "AllowOriginFunc",
		"null":                               "sandboxed",
		"Null":                               "sandboxed",
		"app.example.com":                    "scheme://host",
		"//app.example.com":                  "scheme://host",
		"":                                   "scheme://host",
		" https://app.example.com":           "scheme://host",
		"mailto:security@app.example.com":    "scheme://host",
		"https://app.example.com:not-a-port": "scheme://host",
	}
	for entry, want := range refused {
		_, err := CORS(CORSOptions{AllowedOrigins: []string{"https://ok.example", entry}})
		if err == nil {
			t.Errorf("CORS accepted the AllowedOrigins entry %q", entry)
			continue
		}
		if msg := err.Error(); !strings.HasPrefix(msg, "muzak: ") || !strings.Contains(msg, want) || !strings.Contains(msg, strconvQuote(entry)) {
			t.Errorf("CORS(%q) = %q, want a muzak error naming the entry and saying %q", entry, msg, want)
		}
	}

	for _, entry := range []string{
		"https://app.example.com", "http://localhost:3000", "http://[::1]:8080",
		"https://app.example.com:8443", "chrome-extension://abcdefghijklmnop", "*",
	} {
		if _, err := CORS(CORSOptions{AllowedOrigins: []string{entry}}); err != nil {
			t.Errorf("CORS refused the origin %q: %v", entry, err)
		}
	}

	// Every mistake is reported at once, alongside any other build error.
	opts := quietOptions()
	opts.CORS = CORSOptions{
		AllowedOrigins:   []string{"*", "https://app.example.com/", "null"},
		AllowCredentials: true,
	}
	app := New(opts)
	app.Get("/x", okHandler)
	got := buildError(t, app)
	for _, want := range []string{"wildcard CORS origin", `"https://app.example.com/"`, `"null"`} {
		if !strings.Contains(got, want) {
			t.Errorf("build error = %q, want it to mention %s", got, want)
		}
	}
	_, err := CORS(opts.CORS)
	if !errors.Is(err, ErrCORSWildcardCredentials) {
		t.Errorf("CORS = %v, want it to still match ErrCORSWildcardCredentials", err)
	}
}

// strconvQuote renders an entry the way the error messages quote it.
func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }
