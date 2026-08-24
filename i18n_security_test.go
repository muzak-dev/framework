package muzak

import (
	"net/http"
	"strings"
	"testing"
)

// TestLocaleNeverTrustsTheRequest is the whole of this feature's security
// argument, stated once.
//
// A locale arrives from a client, and it is then used to pick a file, to key a
// map and to write a response header. The rule that makes all three safe is
// that nothing a request says is used unless it matches a locale the
// application itself declared: what comes out of resolution is always one of
// the application's own strings, never one of the client's.
func TestLocaleNeverTrustsTheRequest(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t, func(o *I18nOptions) {
		o.Sources = []LocaleSource{
			LocaleFromQuery, LocaleFromHeader, LocaleFromCookie, LocaleFromAcceptLanguage,
		}
	}))

	hostile := []string{
		"../../etc/passwd",
		"../../../../../../etc/shadow",
		"..%2F..%2Fetc%2Fpasswd",
		"/absolute/path",
		"en\r\nX-Injected: yes",
		"en\nSet-Cookie: admin=1",
		"en\x00es",
		"<script>alert(1)</script>",
		strings.Repeat("a", 4096),
		"EN; DROP TABLE locales",
	}

	for _, value := range hostile {
		t.Run(strings.Map(printableOnly, value), func(t *testing.T) {
			t.Parallel()
			// A header cannot legally carry a line break, so the ones that do
			// are sent through the query and the cookie, which can.
			if !strings.ContainsAny(value, "\r\n\x00") {
				assertDefaultLocale(t, app, localeRequest{
					headers: map[string]string{"X-Locale": value},
				}, value)
				assertDefaultLocale(t, app, localeRequest{
					headers: map[string]string{"Accept-Language": value},
				}, value)
			}
			assertDefaultLocale(t, app, localeRequest{
				cookies: []*http.Cookie{{Name: "locale", Value: value}},
			}, value)
		})
	}
}

// assertDefaultLocale sends a request carrying something hostile and checks
// that the application answered in its own default rather than in whatever
// arrived.
func assertDefaultLocale(t *testing.T, app *App, r localeRequest, sent string) {
	t.Helper()
	rec := send(t, app, r)
	assertStatus(t, rec, http.StatusOK)

	if got := decodeGreeting(t, rec).Locale; got != "en" {
		t.Errorf("a request carrying %q resolved to %q, want the default locale", sent, got)
	}
	if got := rec.Header().Get("Content-Language"); got != "en" {
		t.Errorf("a request carrying %q produced Content-Language %q, want en", sent, got)
	}
	for name, values := range rec.Header() {
		if strings.EqualFold(name, "X-Injected") || strings.EqualFold(name, "Set-Cookie") {
			t.Errorf("a request carrying %q produced the header %s: %v", sent, name, values)
		}
	}
}

// printableOnly keeps a subtest name readable when the value under test is not.
func printableOnly(r rune) rune {
	if r < ' ' || r > '~' {
		return '.'
	}
	return r
}

// TestLocaleFromAHostilePath covers the source that reads straight out of the
// URL, where a traversal attempt is most natural to write.
func TestLocaleFromAHostilePath(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t, func(o *I18nOptions) {
		o.Sources = []LocaleSource{LocaleFromPath}
	}))

	rec := send(t, app, localeRequest{target: "/..%2F..%2Fetc/greet"})
	// Whether the router matches such a path or not, no locale may come of it.
	if rec.Code == http.StatusOK {
		if got := decodeGreeting(t, rec).Locale; got != "en" {
			t.Errorf("a traversal in the path resolved to %q, want the default locale", got)
		}
	}
}

// TestHostileAcceptLanguageIsBounded covers a client sending far more than a
// browser ever would. The scan has to stop rather than oblige it.
func TestHostileAcceptLanguageIsBounded(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t))

	cases := map[string]string{
		"a very long header":  strings.Repeat("xx-YY;q=0.5, ", 20000),
		"very many ranges":    strings.Repeat("zz,", 5000) + "es",
		"one enormous tag":    strings.Repeat("z", 100000),
		"nothing but commas":  strings.Repeat(",", 10000),
		"nothing but spaces":  strings.Repeat(" ", 10000),
		"repeated wildcards":  strings.Repeat("*,", 5000),
		"malformed qualities": strings.Repeat("es;q=;", 5000),
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := send(t, app, localeRequest{headers: map[string]string{"Accept-Language": header}})
			assertStatus(t, rec, http.StatusOK)
			if got := decodeGreeting(t, rec).Locale; !isAvailableLocale(got) {
				t.Errorf("a hostile header resolved to %q, want one of the application's own locales", got)
			}
		})
	}
}

// isAvailableLocale reports whether a resolved locale is one the test
// application actually declared.
func isAvailableLocale(locale string) bool {
	switch locale {
	case "en", "es", "fr", "pt":
		return true
	default:
		return false
	}
}

// TestHostileExtractor covers the one source that is application code, and so
// the one that can return anything at all.
func TestHostileExtractor(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t, func(o *I18nOptions) {
		o.Sources = []LocaleSource{LocaleFromCustom}
		o.Extractor = func(*http.Request) []string {
			return []string{"../../etc/passwd", "en\r\nX-Injected: yes", "", "de"}
		}
	}))

	rec := send(t, app, localeRequest{})
	assertStatus(t, rec, http.StatusOK)
	if got := decodeGreeting(t, rec).Locale; got != "en" {
		t.Errorf("an extractor returning nothing usable resolved to %q, want the default", got)
	}
	if got := rec.Header().Get("Content-Language"); got != "en" {
		t.Errorf("Content-Language = %q, want en", got)
	}
}
