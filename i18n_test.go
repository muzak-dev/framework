package muzak

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"muzak.dev/framework/i18n"
)

// localeFiles are the translations every test here is served from. They are
// deliberately small: what is being tested is which locale a request lands in,
// not what the words are.
func localeFiles() fstest.MapFS {
	return fstest.MapFS{
		"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  greeting: Hello\n")},
		"locales/es.yml": &fstest.MapFile{Data: []byte("es:\n  greeting: Hola\n")},
		"locales/fr.yml": &fstest.MapFile{Data: []byte("fr:\n  greeting: Bonjour\n")},
		"locales/pt.yml": &fstest.MapFile{Data: []byte("pt:\n  greeting: Ola\n")},
	}
}

// i18nOptions builds the options a localized test application is given.
func i18nOptions(t *testing.T, apply ...func(*I18nOptions)) I18nOptions {
	t.Helper()
	store, err := i18n.Load(localeFiles(), "locales")
	if err != nil {
		t.Fatalf("loading the test locales: %v", err)
	}
	opts := I18nOptions{Store: store}
	for _, change := range apply {
		change(&opts)
	}
	return opts
}

// greeting is what a localized handler answers with: the locale that was
// resolved, and the message translated into it.
type greeting struct {
	Locale  string `json:"locale"`
	Message string `json:"message"`
}

// greetingApp serves the greeting at two paths, one of which carries the locale
// as a segment so that source can be exercised.
func greetingApp(t *testing.T, opts I18nOptions) *App {
	t.Helper()
	options := quietOptions()
	options.I18n = opts
	app := New(options)
	app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
		return greeting{Locale: ctx.Locale(), Message: ctx.T("greeting")}, nil
	})
	app.Get("/{locale}/greet", func(ctx *Context, _ struct{}) (greeting, error) {
		return greeting{Locale: ctx.Locale(), Message: ctx.T("greeting")}, nil
	})
	return mustBuild(t, app)
}

// localeRequest describes one request a locale test sends.
type localeRequest struct {
	target  string
	headers map[string]string
	cookies []*http.Cookie
}

// send runs a request through an application. The in-process client cannot be
// used from inside the package it is built on, so these tests drive the handler
// the way the rest of this package's tests do.
func send(t *testing.T, app *App, r localeRequest) *httptest.ResponseRecorder {
	t.Helper()
	target := r.target
	if target == "" {
		target = "/greet"
	}
	req := httptest.NewRequest(http.MethodGet, target, strings.NewReader(""))
	for name, value := range r.headers {
		req.Header.Set(name, value)
	}
	for _, cookie := range r.cookies {
		req.AddCookie(cookie)
	}
	return doRequest(t, app, req)
}

// decodeGreeting reads the body a localized handler answered with.
func decodeGreeting(t *testing.T, rec *httptest.ResponseRecorder) greeting {
	t.Helper()
	var out greeting
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body is not a greeting: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

// resolved sends a request and reports the locale it landed in.
func resolved(t *testing.T, app *App, r localeRequest) string {
	t.Helper()
	rec := send(t, app, r)
	assertStatus(t, rec, http.StatusOK)
	return decodeGreeting(t, rec).Locale
}

func TestLocaleFromAcceptLanguage(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t))

	cases := []struct {
		name   string
		header string
		want   string
	}{
		{name: "no header at all", header: "", want: "en"},
		{name: "one language", header: "es", want: "es"},
		{name: "case is ignored", header: "ES", want: "es"},
		{name: "quality orders the choices", header: "es;q=0.5, fr;q=0.9", want: "fr"},
		{name: "equal quality keeps the client's order", header: "fr, es", want: "fr"},
		{name: "an implicit quality of one beats a stated lower one", header: "es, fr;q=0.9", want: "es"},
		{name: "a region falls back to its language", header: "pt-BR", want: "pt"},
		{name: "a language nobody has is skipped", header: "de, es", want: "es"},
		{name: "nothing available falls back to the default", header: "de, ja", want: "en"},
		{name: "a wildcard takes whatever there is", header: "*", want: "en"},
		{name: "a refused language is not served", header: "en;q=0, es", want: "es"},
		{name: "refusing everything unlisted", header: "es, *;q=0", want: "es"},
		{name: "a wildcard skips a language refused by name", header: "*, en;q=0", want: "es"},
		{name: "whitespace is tolerated", header: "  es ;  q=0.9  ,  fr ; q=0.1 ", want: "es"},
		{name: "a malformed quality counts as one", header: "es;q=high", want: "es"},
		{name: "an empty entry is skipped", header: ",,es", want: "es"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			headers := map[string]string{}
			if tc.header != "" {
				headers["Accept-Language"] = tc.header
			}
			if got := resolved(t, app, localeRequest{headers: headers}); got != tc.want {
				t.Errorf("Accept-Language %q resolved to %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// TestLocaleRefusedLanguageStillAnswers covers refusing the only language there
// is: the response still comes back, in the default locale.
func TestLocaleRefusedLanguageStillAnswers(t *testing.T) {
	t.Parallel()
	store, err := i18n.Load(fstest.MapFS{
		"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  greeting: Hello\n")},
	}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	app := greetingApp(t, I18nOptions{Store: store})
	got := resolved(t, app, localeRequest{headers: map[string]string{"Accept-Language": "en;q=0"}})
	if got != "en" {
		t.Errorf("refusing the only language resolved to %q, want the default", got)
	}
}

func TestLocaleSources(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		options func(*I18nOptions)
		request localeRequest
		want    string
	}{
		{
			name:    "a query parameter",
			options: func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromQuery} },
			request: localeRequest{target: "/greet?locale=es"},
			want:    "es",
		},
		{
			name: "a query parameter under another name",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromQuery}
				o.Query = "lang"
			},
			request: localeRequest{target: "/greet?lang=fr"},
			want:    "fr",
		},
		{
			name:    "a header",
			options: func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromHeader} },
			request: localeRequest{headers: map[string]string{"X-Locale": "es"}},
			want:    "es",
		},
		{
			name: "a header under another name",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromHeader}
				o.Header = "X-Language"
			},
			request: localeRequest{headers: map[string]string{"X-Language": "fr"}},
			want:    "fr",
		},
		{
			name:    "a cookie",
			options: func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromCookie} },
			request: localeRequest{cookies: []*http.Cookie{{Name: "locale", Value: "fr"}}},
			want:    "fr",
		},
		{
			name: "a cookie under another name",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromCookie}
				o.Cookie = "lang"
			},
			request: localeRequest{cookies: []*http.Cookie{{Name: "lang", Value: "es"}}},
			want:    "es",
		},
		{
			name:    "no cookie at all falls back",
			options: func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromCookie} },
			request: localeRequest{},
			want:    "en",
		},
		{
			name:    "a path segment",
			options: func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromPath} },
			request: localeRequest{target: "/es/greet"},
			want:    "es",
		},
		{
			name: "a custom extractor",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromCustom}
				o.Extractor = func(r *http.Request) []string {
					return []string{r.Header.Get("X-Preferred"), "fr"}
				}
			},
			request: localeRequest{headers: map[string]string{"X-Preferred": "de"}},
			want:    "fr",
		},
		{
			name:    "a region named in a query falls back to its language",
			options: func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromQuery} },
			request: localeRequest{target: "/greet?locale=pt-BR"},
			want:    "pt",
		},
		{
			name:    "a region nobody has any form of falls back to the default",
			options: func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromQuery} },
			request: localeRequest{target: "/greet?locale=de-AT"},
			want:    "en",
		},
		{
			name: "the first source that yields one wins",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromQuery, LocaleFromAcceptLanguage}
			},
			request: localeRequest{
				target:  "/greet?locale=es",
				headers: map[string]string{"Accept-Language": "fr"},
			},
			want: "es",
		},
		{
			name: "a source that yields nothing moves on to the next",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromQuery, LocaleFromAcceptLanguage}
			},
			request: localeRequest{headers: map[string]string{"Accept-Language": "fr"}},
			want:    "fr",
		},
		{
			name: "a source naming a locale nobody has moves on",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromQuery, LocaleFromAcceptLanguage}
			},
			request: localeRequest{
				target:  "/greet?locale=de",
				headers: map[string]string{"Accept-Language": "fr"},
			},
			want: "fr",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := greetingApp(t, i18nOptions(t, tc.options))
			if got := resolved(t, app, tc.request); got != tc.want {
				t.Errorf("resolved to %q, want %q", got, tc.want)
			}
		})
	}
}

func TestContextTranslates(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t))

	cases := map[string]string{"en": "Hello", "es": "Hola", "fr": "Bonjour"}
	for locale, want := range cases {
		rec := send(t, app, localeRequest{headers: map[string]string{"Accept-Language": locale}})
		assertStatus(t, rec, http.StatusOK)
		if got := decodeGreeting(t, rec).Message; got != want {
			t.Errorf("in %s the greeting was %q, want %q", locale, got, want)
		}
	}
}

// TestContextWithoutAStore covers a handler written to translate running in an
// application that has not been localized, which must still say something
// recognisable rather than nothing.
func TestContextWithoutAStore(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	var key, rendered string
	app.Get("/plain", func(ctx *Context, _ struct{}) (greeting, error) {
		key = ctx.T("store.title")
		rendered = ctx.L(42)
		return greeting{Locale: ctx.Locale()}, nil
	})
	mustBuild(t, app)

	rec := send(t, app, localeRequest{target: "/plain"})
	assertStatus(t, rec, http.StatusOK)
	if key != "store.title" {
		t.Errorf("T with no store = %q, want the key itself", key)
	}
	if rendered != "42" {
		t.Errorf("L with no store = %q, want the value as Go prints it", rendered)
	}
	if locale := decodeGreeting(t, rec).Locale; locale != "" {
		t.Errorf("Locale with no store = %q, want the empty string", locale)
	}
}

func TestContentLanguageAndVary(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t))
	rec := send(t, app, localeRequest{headers: map[string]string{"Accept-Language": "es"}})
	if got := rec.Header().Get("Content-Language"); got != "es" {
		t.Errorf("Content-Language = %q, want es", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Language") {
		t.Errorf("Vary = %q, want it to name Accept-Language", got)
	}

	// A configuration that never reads the header does not vary on it.
	quiet := greetingApp(t, i18nOptions(t, func(o *I18nOptions) {
		o.Sources = []LocaleSource{LocaleFromQuery}
	}))
	rec = send(t, quiet, localeRequest{target: "/greet?locale=es"})
	if got := rec.Header().Get("Vary"); strings.Contains(got, "Accept-Language") {
		t.Errorf("Vary = %q, want nothing about Accept-Language", got)
	}
	if got := rec.Header().Get("Content-Language"); got != "es" {
		t.Errorf("Content-Language = %q, want es", got)
	}

	// The header can be turned off for a service whose cache key is already
	// settled by something else.
	off := greetingApp(t, i18nOptions(t, func(o *I18nOptions) { o.DisableVary = true }))
	rec = send(t, off, localeRequest{headers: map[string]string{"Accept-Language": "es"}})
	if got := rec.Header().Get("Vary"); strings.Contains(got, "Accept-Language") {
		t.Errorf("Vary = %q with DisableVary set, want nothing", got)
	}
}

// TestVaryComposesWithCompression is the reason the header is added rather than
// set: a response that varies on two headers has to name both.
func TestVaryComposesWithCompression(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.I18n = i18nOptions(t)
	app := New(options)
	app.Use(Compress(CompressionOptions{}))
	app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
		return greeting{Locale: ctx.Locale(), Message: strings.Repeat("Hello ", 400)}, nil
	})
	mustBuild(t, app)

	rec := send(t, app, localeRequest{headers: map[string]string{
		"Accept-Language": "es",
		"Accept-Encoding": "gzip",
	}})
	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	for _, want := range []string{"Accept-Language", "Accept-Encoding"} {
		if !strings.Contains(vary, want) {
			t.Errorf("Vary = %q, want it to name %s", vary, want)
		}
	}
}

func TestLocaleFromContext(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.I18n = i18nOptions(t)
	app := New(options)

	var fromContext string
	var found bool
	app.Get("/deep", func(ctx *Context, _ struct{}) (greeting, error) {
		// This is what a repository or a client wrapper sees: a plain context,
		// with no Muzak Context anywhere near it.
		fromContext, found = LocaleFromContext(ctx.Context())
		return greeting{Locale: ctx.Locale()}, nil
	})
	mustBuild(t, app)

	rec := send(t, app, localeRequest{
		target:  "/deep",
		headers: map[string]string{"Accept-Language": "fr"},
	})
	assertStatus(t, rec, http.StatusOK)
	if !found || fromContext != "fr" {
		t.Errorf("LocaleFromContext = %q, %v, want fr, true", fromContext, found)
	}

	if _, ok := LocaleFromContext(context.Background()); ok {
		t.Error("LocaleFromContext reported a locale on a context that carries none")
	}
}

func TestAccessLogRecordsTheLocale(t *testing.T) {
	t.Parallel()
	logger, buffer := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	options.DisableAccessLog = false
	options.I18n = i18nOptions(t)
	app := New(options)
	app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
		return greeting{Locale: ctx.Locale()}, nil
	})
	mustBuild(t, app)

	send(t, app, localeRequest{headers: map[string]string{"Accept-Language": "es"}})
	if logged := buffer.String(); !strings.Contains(logged, `"locale":"es"`) {
		t.Errorf("the access log does not record the locale: %s", logged)
	}
}

func TestI18nBuildErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		options  func(*I18nOptions)
		mentions string
	}{
		{
			name:     "a custom source with no extractor",
			options:  func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleFromCustom} },
			mentions: "Extractor",
		},
		{
			name: "a path index below zero",
			options: func(o *I18nOptions) {
				o.Sources = []LocaleSource{LocaleFromPath}
				o.PathIndex = -1
			},
			mentions: "PathIndex",
		},
		{
			name:     "a source that is not one",
			options:  func(o *I18nOptions) { o.Sources = []LocaleSource{LocaleSource(200)} },
			mentions: "not a locale source",
		},
		{
			name: "a default outside the available locales",
			options: func(o *I18nOptions) {
				o.DefaultLocale = "de"
				o.AvailableLocales = []string{"en", "es"}
			},
			mentions: "DefaultLocale",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := quietOptions()
			options.I18n = i18nOptions(t, tc.options)
			app := New(options)
			app.Get("/greet", func(*Context, struct{}) (greeting, error) { return greeting{}, nil })
			if got := buildError(t, app); !strings.Contains(got, tc.mentions) {
				t.Errorf("the build error is %q, want it to mention %q", got, tc.mentions)
			}
		})
	}
}

// TestI18nBuildErrorsAreReportedTogether covers the rule the framework applies
// to every misconfiguration: a first run lists everything wrong, not the first
// thing wrong.
func TestI18nBuildErrorsAreReportedTogether(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.I18n = i18nOptions(t, func(o *I18nOptions) {
		o.Sources = []LocaleSource{LocaleFromCustom, LocaleSource(200)}
		o.DefaultLocale = "de"
		o.AvailableLocales = []string{"en"}
	})
	app := New(options)
	app.Get("/greet", func(*Context, struct{}) (greeting, error) { return greeting{}, nil })

	got := buildError(t, app)
	for _, mention := range []string{"Extractor", "not a locale source", "DefaultLocale"} {
		if !strings.Contains(got, mention) {
			t.Errorf("the build error does not mention %q: %s", mention, got)
		}
	}
}

func TestLocaleSourceString(t *testing.T) {
	t.Parallel()
	cases := map[LocaleSource]string{
		LocaleFromPath:           "the request path",
		LocaleFromQuery:          "a query parameter",
		LocaleFromHeader:         "a request header",
		LocaleFromCookie:         "a cookie",
		LocaleFromAcceptLanguage: "the Accept-Language header",
		LocaleFromCustom:         "a custom extractor",
		LocaleSource(200):        "an unknown source",
	}
	for source, want := range cases {
		if got := source.String(); got != want {
			t.Errorf("LocaleSource(%d).String() = %q, want %q", source, got, want)
		}
	}
}

// TestQualityParsing covers the header parser directly, including the values
// the compression middleware relies on it reading the same way it always has.
func TestQualityParsing(t *testing.T) {
	t.Parallel()
	cases := map[string]float64{
		"":              1,
		"q=1":           1,
		"q=0":           0,
		"q=0.5":         0.5,
		" q = 0.25 ":    0.25,
		"Q=0":           0,
		"charset=utf-8": 1,
		"q=nonsense":    1,
		"level=1;q=0":   0,
		// ParseFloat reads these, and none is a quality: NaN cannot be
		// ordered, and an infinity or an over-large value would outrank every
		// honest one. Beyond one is one; unreadable is one, as before.
		"q=NaN":  1,
		"q=Inf":  1,
		"q=+Inf": 1,
		"q=1e9":  1,
		"q=7":    1,
		"q=-Inf": 0,
		"q=-1":   0,
	}
	for parameters, want := range cases {
		if got := quality(parameters); got != want {
			t.Errorf("quality(%q) = %v, want %v", parameters, got, want)
		}
	}

	for parameters, want := range map[string]bool{"q=0": true, "q=1": false, "": false, "q=bad": false} {
		if got := refused(parameters); got != want {
			t.Errorf("refused(%q) = %v, want %v", parameters, got, want)
		}
	}
}

// TestNegotiateLanguageDirectly covers the shapes a request cannot easily
// produce through a handler.
func TestNegotiateLanguageDirectly(t *testing.T) {
	t.Parallel()
	available := []string{"en", "pt"}
	cases := []struct {
		header    string
		available []string
		want      string
	}{
		{header: "", available: available, want: ""},
		{header: "es", available: nil, want: ""},
		{header: "pt-PT", available: available, want: "pt"},
		{header: "en-GB", available: []string{"en-GB", "en"}, want: "en-GB"},
		{header: "en", available: []string{"en-GB"}, want: "en-GB"},
		{header: ";q=0.5", available: available, want: ""},
		{header: "de", available: available, want: ""},

		// Refusing a language refuses the regions beneath it, so a client that
		// says it will not read English is not handed British English.
		{header: "en;q=0", available: []string{"en-GB"}, want: ""},
		{header: "en;q=0, pt", available: []string{"en-GB", "pt"}, want: "pt"},
	}
	for _, tc := range cases {
		if got := negotiateLanguage(tc.header, tc.available); got != tc.want {
			t.Errorf("negotiateLanguage(%q, %v) = %q, want %q", tc.header, tc.available, got, tc.want)
		}
	}
}

// TestPathLocale covers the segment reader on its own, since a path with too
// few segments is easier to state here than to route.
func TestPathLocale(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path  string
		index int
		want  []string
	}{
		{path: "/es/books", index: 0, want: []string{"es"}},
		{path: "/api/es/books", index: 1, want: []string{"es"}},
		{path: "/books", index: 1, want: nil},
		{path: "/", index: 0, want: nil},
		{path: "/es", index: 0, want: []string{"es"}},
	}
	for _, tc := range cases {
		got := pathLocale(tc.path, tc.index)
		if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("pathLocale(%q, %d) = %v, want %v", tc.path, tc.index, got, tc.want)
		}
	}
}

// TestLocaleMiddlewareOnItsOwn covers the middleware used directly, which is
// what an application does when it wants the locale resolved somewhere other
// than where the framework installs it.
func TestLocaleMiddlewareOnItsOwn(t *testing.T) {
	t.Parallel()
	store, err := i18n.Load(localeFiles(), "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	var seen string
	handler := Locale(I18nOptions{Store: store})(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			seen, _ = LocaleFromContext(r.Context())
		}))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Language", "fr")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if seen != "fr" {
		t.Errorf("the middleware resolved %q, want fr", seen)
	}
}

// TestBindingKey covers the mapping from a binder failure to the translation of
// it, including the failures this package has no translation for.
func TestBindingKey(t *testing.T) {
	t.Parallel()
	cases := map[error]string{
		errNotDuration: "muzak.binding.duration",
		errNotBool:     "muzak.binding.boolean",
		errNotInt:      "muzak.binding.integer",
		errNotUint:     "muzak.binding.unsigned",
		errNotNumber:   "muzak.binding.number",
		// A failure from a setter of the caller's own has no message the
		// framework could translate, so it keeps the words the setter chose.
		errors.New("the transponder is misaligned"): "",
	}
	for err, want := range cases {
		if got := bindingKey(err); got != want {
			t.Errorf("bindingKey(%v) = %q, want %q", err, got, want)
		}
	}

	// A failure inside a repeated parameter arrives wrapped in the entry it
	// came from, which is why the sentinels are matched rather than compared.
	wrapped := fmt.Errorf("entry 2 %q %w", "abc", errNotInt)
	if got := bindingKey(wrapped); got != "muzak.binding.integer" {
		t.Errorf("bindingKey of a wrapped failure = %q, want it seen through", got)
	}
}

// TestNegotiateLanguageCostDoesNotGrowWithLocales is the regression test for
// negotiation lower-casing and splitting every available locale for every
// range of the header: sixty-four ranges against three hundred locales took
// nineteen thousand allocations, on every request, ahead of routing and the
// rate limit. Only the header itself is prepared per request now.
func TestNegotiateLanguageCostDoesNotGrowWithLocales(t *testing.T) {
	var available []string
	for i := range 300 {
		available = append(available, fmt.Sprintf("Ab-%02dX", i))
	}
	var ranges []string
	for i := range 64 {
		ranges = append(ranges, fmt.Sprintf("zz-%d;q=0.%d", i, 9-i%9))
	}
	header := strings.Join(ranges, ",")
	index := indexLocales(available)
	if got := negotiateIndexed(header, index); got != "" {
		t.Fatalf("negotiateIndexed = %q, want no match", got)
	}
	allocs := testing.AllocsPerRun(20, func() { negotiateIndexed(header, index) })
	if allocs > 20 {
		t.Errorf("negotiation of 64 ranges against 300 locales made %.0f allocations, want it independent of the locale count", allocs)
	}
}
