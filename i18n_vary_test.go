package muzak

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// varyOf returns the fields a response's Vary header names, in order.
func varyOf(h http.Header) []string {
	var out []string
	for _, value := range h.Values("Vary") {
		for name := range strings.SplitSeq(value, ",") {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

// TestVaryNamesEveryInputTheResolverRead is the cache poisoning case: with a
// header or cookie source, a response that only varied on Accept-Language let
// the first client to reach a shared cache pick the language everyone after it
// was served.
func TestVaryNamesEveryInputTheResolverRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sources []LocaleSource
		header  string
		request localeRequest
		want    []string
	}{
		{
			name:    "a header source names its header",
			sources: []LocaleSource{LocaleFromHeader, LocaleFromAcceptLanguage},
			request: localeRequest{headers: map[string]string{"X-Locale": "es"}},
			want:    []string{"X-Locale"},
		},
		{
			name:    "a custom header name is canonicalized",
			sources: []LocaleSource{LocaleFromHeader},
			header:  "x-app-lang",
			request: localeRequest{headers: map[string]string{"X-App-Lang": "es"}},
			want:    []string{"X-App-Lang"},
		},
		{
			name:    "a cookie source names Cookie",
			sources: []LocaleSource{LocaleFromCookie, LocaleFromAcceptLanguage},
			request: localeRequest{cookies: []*http.Cookie{{Name: "locale", Value: "es"}}},
			want:    []string{"Cookie"},
		},
		{
			name:    "every source read before the default is named once",
			sources: []LocaleSource{LocaleFromHeader, LocaleFromCookie, LocaleFromAcceptLanguage},
			request: localeRequest{},
			want:    []string{"X-Locale", "Cookie", "Accept-Language"},
		},
		{
			name:    "a source that did not match was still read",
			sources: []LocaleSource{LocaleFromCookie, LocaleFromAcceptLanguage},
			request: localeRequest{headers: map[string]string{"Accept-Language": "es"}},
			want:    []string{"Cookie", "Accept-Language"},
		},
		{
			name:    "a header named Accept-Language is not repeated",
			sources: []LocaleSource{LocaleFromHeader, LocaleFromAcceptLanguage},
			header:  "accept-language",
			request: localeRequest{},
			want:    []string{"Accept-Language"},
		},
		{
			name:    "the address alone needs no Vary",
			sources: []LocaleSource{LocaleFromPath, LocaleFromAcceptLanguage},
			request: localeRequest{target: "/es/greet"},
			want:    nil,
		},
		{
			name:    "a path that does not decide falls through to a header",
			sources: []LocaleSource{LocaleFromPath, LocaleFromAcceptLanguage},
			request: localeRequest{target: "/xx/greet", headers: map[string]string{"Accept-Language": "es"}},
			want:    []string{"Accept-Language"},
		},
		{
			name:    "the query needs no Vary",
			sources: []LocaleSource{LocaleFromQuery},
			request: localeRequest{target: "/greet?locale=es"},
			want:    nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := greetingApp(t, i18nOptions(t, func(o *I18nOptions) {
				o.Sources = tc.sources
				o.Header = tc.header
			}))
			rec := send(t, app, tc.request)
			if got := varyOf(rec.Header()); !slices.Equal(got, tc.want) {
				t.Errorf("Vary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisableVaryCoversEverySource(t *testing.T) {
	t.Parallel()
	app := greetingApp(t, i18nOptions(t, func(o *I18nOptions) {
		o.Sources = []LocaleSource{LocaleFromHeader, LocaleFromCookie, LocaleFromAcceptLanguage}
		o.DisableVary = true
	}))
	rec := send(t, app, localeRequest{headers: map[string]string{"X-Locale": "es"}})
	if got := rec.Header().Values("Vary"); len(got) != 0 {
		t.Errorf("Vary = %q with DisableVary set, want nothing", got)
	}
}

func TestLocaleVaryKeepsWhatIsAlreadyThere(t *testing.T) {
	t.Parallel()
	vary := localeVaryPrefixes(I18nOptions{
		Sources: []LocaleSource{LocaleFromHeader, LocaleFromCookie, LocaleFromAcceptLanguage},
		Header:  "X-Locale",
	})

	h := http.Header{}
	h.Add("Vary", "Origin, cookie")
	addVaryFields(h, vary[2].fields...)
	if got, want := varyOf(h), []string{"Origin", "cookie", "X-Locale", "Accept-Language"}; !slices.Equal(got, want) {
		t.Errorf("Vary = %q, want %q", got, want)
	}

	// A response that already varies on everything needs nothing added.
	star := http.Header{}
	star.Set("Vary", "*")
	addVaryFields(star, vary[2].fields...)
	if got := varyOf(star); !slices.Equal(got, []string{"*"}) {
		t.Errorf("Vary = %q, want only *", got)
	}

	// An earlier prefix is not disturbed by the entries computed after it.
	if got := vary[0].fields; !slices.Equal(got, []string{"X-Locale"}) {
		t.Errorf("first prefix = %q, want only X-Locale", got)
	}
}
