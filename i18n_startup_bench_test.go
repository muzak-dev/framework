package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"muzak.dev/framework/i18n"
)

// coldStartLocales stands in for an application's own translations: five
// languages, each with the handful of strings a service adds and a few
// framework rules reworded.
func coldStartLocales() fstest.MapFS {
	files := fstest.MapFS{}
	for _, locale := range []string{"en", "es", "fr", "de", "tr"} {
		files["locales/"+locale+".yml"] = &fstest.MapFile{Data: []byte(
			locale + ":\n" +
				"  greeting:\n    hello: \"Hello, %{name}\"\n" +
				"    items:\n      one: one thing\n      other: \"%{count} things\"\n" +
				"  errors:\n    messages:\n      blank: is required\n" +
				"      email: must be an email\n" +
				"    models:\n      create_item:\n        attributes:\n          name:\n            blank: needs a name\n")}
	}
	return files
}

// BenchmarkColdStart measures everything a process does before it can answer:
// parse the locale files, chain the built-in ones beneath them, register the
// routes and build the application.
//
// It is the number that matters on a platform which starts a fresh instance to
// serve a burst and stops it again afterwards, because it is paid in full by
// the first request each instance sees.
func BenchmarkColdStart(b *testing.B) {
	files := coldStartLocales()
	b.ReportAllocs()
	for b.Loop() {
		store, err := i18n.Load(files, "locales")
		if err != nil {
			b.Fatal(err)
		}
		options := quietOptions()
		options.I18n = I18nOptions{Store: store}
		app := New(options)
		app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
			return greeting{Locale: ctx.Locale(), Message: ctx.T("greeting.hello", "name", "Ada")}, nil
		})
		if err := app.Build(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFirstRequestAfterColdStart measures the whole of what an instance
// does for the very first request it ever sees, build included.
func BenchmarkFirstRequestAfterColdStart(b *testing.B) {
	files := coldStartLocales()
	b.ReportAllocs()
	for b.Loop() {
		store, err := i18n.Load(files, "locales")
		if err != nil {
			b.Fatal(err)
		}
		options := quietOptions()
		options.I18n = I18nOptions{Store: store}
		app := New(options)
		app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
			return greeting{Locale: ctx.Locale(), Message: ctx.T("greeting.hello", "name", "Ada")}, nil
		})
		req := httptest.NewRequest(http.MethodGet, "/greet", strings.NewReader(""))
		req.Header.Set("Accept-Language", "tr")
		app.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkTranslatedRequest is the steady-state number, once an instance is
// warm: one request, resolved, translated and rendered.
func BenchmarkTranslatedRequest(b *testing.B) {
	store, err := i18n.Load(coldStartLocales(), "locales")
	if err != nil {
		b.Fatal(err)
	}
	options := quietOptions()
	options.I18n = I18nOptions{Store: store}
	app := New(options)
	app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
		return greeting{Locale: ctx.Locale(), Message: ctx.T("greeting.hello", "name", "Ada")}, nil
	})
	if err := app.Build(); err != nil {
		b.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/greet", strings.NewReader(""))
	req.Header.Set("Accept-Language", "es-ES,es;q=0.9,en;q=0.1")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		app.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkColdStartWithoutI18n is the baseline the one above is read against.
//
// The same application with no translation store, so the difference between the
// two is what internationalization costs an instance at start-up, rather than
// what starting an instance costs.
func BenchmarkColdStartWithoutI18n(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		app := New(quietOptions())
		app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
			return greeting{Message: "Hello, Ada"}, nil
		})
		if err := app.Build(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreOnly measures the translation store alone, with no application
// around it, which is the part of a cold start this feature is responsible for.
func BenchmarkStoreOnly(b *testing.B) {
	files := coldStartLocales()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := i18n.Load(files, "locales"); err != nil {
			b.Fatal(err)
		}
	}
}
