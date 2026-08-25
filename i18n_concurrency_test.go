package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"muzak.dev/framework/i18n"
)

// TestConcurrentRequestsInManyLocales is the test that matters for a server
// rather than for a library.
//
// Every instance of a deployed service handles many requests at once, in
// different languages, on goroutines that know nothing about each other. The
// locale is resolved per request, carried on a pooled Context and read while
// other requests are resolving their own, so nothing about it may be shared
// mutable state. Run under the race detector, this is what proves it.
func TestConcurrentRequestsInManyLocales(t *testing.T) {
	t.Parallel()

	store, err := i18n.Load(fstest.MapFS{
		"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  hello: Hello\n  count:\n    one: one thing\n    other: \"%{count} things\"\n")},
		"locales/es.yml": &fstest.MapFile{Data: []byte("es:\n  hello: Hola\n  count:\n    one: una cosa\n    other: \"%{count} cosas\"\n")},
		"locales/fr.yml": &fstest.MapFile{Data: []byte("fr:\n  hello: Bonjour\n  count:\n    one: une chose\n    other: \"%{count} choses\"\n")},
		"locales/ru.yml": &fstest.MapFile{Data: []byte("ru:\n  i18n:\n    plural:\n      rule: slavic\n  hello: Privet\n  count:\n    one: \"%{count} a\"\n    few: \"%{count} b\"\n    many: \"%{count} c\"\n")},
	}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	options := quietOptions()
	options.I18n = I18nOptions{Store: store}
	app := New(options)
	app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
		return greeting{Locale: ctx.Locale(), Message: ctx.T("hello")}, nil
	})
	mustBuild(t, app)

	want := map[string]string{"en": "Hello", "es": "Hola", "fr": "Bonjour", "ru": "Privet"}
	locales := []string{"en", "es", "fr", "ru"}

	var wg sync.WaitGroup
	for worker := range 48 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			locale := locales[worker%len(locales)]
			for range 40 {
				req := httptest.NewRequest(http.MethodGet, "/greet", strings.NewReader(""))
				req.Header.Set("Accept-Language", locale)
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want 200", rec.Code)
					return
				}
				got := decodeGreeting(t, rec)
				// The locale one goroutine resolved must never be the one
				// another resolved, which is what a shared current locale would
				// produce under load.
				if got.Locale != locale || got.Message != want[locale] {
					t.Errorf("a request for %s was answered %q in %q", locale, got.Message, got.Locale)
					return
				}
				if header := rec.Header().Get("Content-Language"); header != locale {
					t.Errorf("Content-Language = %q, want %q", header, locale)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

// TestConcurrentStoreReads exercises the store directly, including the parts
// that cache: the plural rules resolved at load, the buffer pool a rendered
// message is built in, and the time zone names remembered as they are seen.
func TestConcurrentStoreReads(t *testing.T) {
	t.Parallel()
	store := i18n.Builtin()

	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 100 {
				if got := store.T("en", "errors.messages.blank"); got != "is required" {
					t.Errorf("plain lookup gave %q", got)
					return
				}
				if got := store.T("en", "errors.messages.too_short", "count", i+2); !strings.Contains(got, "characters") {
					t.Errorf("interpolated lookup gave %q", got)
					return
				}
				if got := store.L("en", 1234.5); got != "1,234.5" {
					t.Errorf("localized number gave %q", got)
					return
				}
				if got := store.T("en", "muzak.http.404"); got == "" {
					t.Error("status sentence was empty")
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

// TestNoLocaleLeaksThroughThePool is the leak test that matters most.
//
// A Context is pooled and reused across requests, and it carries the resolved
// locale. If it were ever handed to a request without being cleared, a client
// that asked for nothing would be answered in whatever language the previous
// request through that Context happened to want, and only under load.
//
// Half the requests here name a locale and half name none. Every unnamed one
// must come back in the default, whatever ran on that Context a moment earlier.
func TestNoLocaleLeaksThroughThePool(t *testing.T) {
	t.Parallel()

	store, err := i18n.Load(fstest.MapFS{
		"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  hello: Hello\n")},
		"locales/es.yml": &fstest.MapFile{Data: []byte("es:\n  hello: Hola\n")},
		"locales/fr.yml": &fstest.MapFile{Data: []byte("fr:\n  hello: Bonjour\n")},
		"locales/ru.yml": &fstest.MapFile{Data: []byte("ru:\n  hello: Privet\n")},
		"locales/pt.yml": &fstest.MapFile{Data: []byte("pt:\n  hello: Ola\n")},
	}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	options := quietOptions()
	options.I18n = I18nOptions{Store: store}
	app := New(options)
	app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
		return greeting{Locale: ctx.Locale(), Message: ctx.T("hello")}, nil
	})
	mustBuild(t, app)

	said := map[string]string{
		"en": "Hello", "es": "Hola", "fr": "Bonjour", "ru": "Privet", "pt": "Ola",
	}
	// A regional locale is included because it resolves to its language, which
	// is a different value going in from the one coming back.
	asked := []string{"es", "fr", "ru", "pt", "pt-BR", "de", ""}

	var wg sync.WaitGroup
	for worker := range 64 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 60 {
				header := asked[(worker*7+i)%len(asked)]

				// A locale nobody has, and no header at all, must both land on
				// the default. Those are the two cases a leak would show up in,
				// because there is nothing in the request to overwrite a stale
				// value with.
				want := header
				switch header {
				case "", "de":
					want = "en"
				case "pt-BR":
					want = "pt"
				}

				req := httptest.NewRequest(http.MethodGet, "/greet", strings.NewReader(""))
				if header != "" {
					req.Header.Set("Accept-Language", header)
				}
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, req)

				got := decodeGreeting(t, rec)
				if got.Locale != want || got.Message != said[want] {
					t.Errorf("asked %q, answered %q in %q, want %q in %q",
						header, got.Message, got.Locale, said[want], want)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

// TestManyInstancesInParallel stands in for a platform that starts and stops
// instances under load.
//
// Each application below is a separate process as far as the code is concerned:
// its own store, its own Context pool, its own resolved chains. They are built
// and driven at the same time, because that is what a scaling event looks like.
func TestManyInstancesInParallel(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup
	for instance := range 12 {
		wg.Add(1)
		go func(instance int) {
			defer wg.Done()

			store, err := i18n.Load(fstest.MapFS{
				"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  hello: Hello\n")},
				"locales/es.yml": &fstest.MapFile{Data: []byte("es:\n  hello: Hola\n")},
			}, "locales")
			if err != nil {
				t.Errorf("instance %d could not load: %v", instance, err)
				return
			}

			options := quietOptions()
			options.I18n = I18nOptions{Store: store}
			app := New(options)
			app.Get("/greet", func(ctx *Context, _ struct{}) (greeting, error) {
				return greeting{Locale: ctx.Locale(), Message: ctx.T("hello")}, nil
			})
			if err := app.Build(); err != nil {
				t.Errorf("instance %d could not build: %v", instance, err)
				return
			}

			for i := range 50 {
				locale := "en"
				if i%2 == 0 {
					locale = "es"
				}
				req := httptest.NewRequest(http.MethodGet, "/greet", strings.NewReader(""))
				req.Header.Set("Accept-Language", locale)
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, req)

				if got := decodeGreeting(t, rec); got.Locale != locale {
					t.Errorf("instance %d answered %q for %q", instance, got.Locale, locale)
					return
				}
			}
		}(instance)
	}
	wg.Wait()
}
