package muzak

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"muzak.dev/framework/i18n"
)

// A handler that sets its own Vary, as one doing content negotiation does,
// used to replace the field CORS, Locale and header versioning had added
// before it ran. The response then carried a reflected
// Access-Control-Allow-Origin, a Content-Language chosen from Accept-Language,
// or the answer of one version, with no Vary naming the request header it
// depended on, and a shared cache stored it for every client. These are the
// regression tests: the field is merged in when the response is written, so
// nothing the handler does to the header can remove it.

// varyHandlers are the ways a handler can meet the Vary already in the header.
var varyHandlers = map[string]Handler[Empty, rtOut]{
	"set": func(ctx *Context, _ Empty) (rtOut, error) {
		ctx.SetHeader("Vary", "Accept")
		return rtOut{OK: true}, nil
	},
	"delete": func(ctx *Context, _ Empty) (rtOut, error) {
		ctx.ResponseWriter().Header().Del("Vary")
		return rtOut{OK: true}, nil
	},
	"set another field": func(ctx *Context, _ Empty) (rtOut, error) {
		ctx.SetHeader("Vary", "Accept-Encoding")
		return rtOut{OK: true}, nil
	},
}

func TestCORSVarySurvivesAHandlerThatSetsVary(t *testing.T) {
	t.Parallel()
	for name, handler := range varyHandlers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.CORS = CORSOptions{AllowedOrigins: []string{"https://app.example"}}
			app := New(opts)
			app.Get("/x", handler)
			mustBuild(t, app)
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Origin", "https://app.example")
			rec := doRequest(t, app, req)
			assertStatus(t, rec, http.StatusOK)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
				t.Fatalf("Access-Control-Allow-Origin = %q, want the origin reflected", got)
			}
			if !varyNamesField(rec.Header(), "Origin") {
				t.Errorf("Vary = %q, want it to keep naming Origin", rec.Header().Values("Vary"))
			}
		})
	}
}

// A response nothing is written to is committed by net/http when the handler
// returns, so the field has to be in the header by then as well.
func TestCORSVarySurvivesAResponseWithNoBody(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.CORS = CORSOptions{AllowedOrigins: []string{"https://app.example"}}
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.SetHeader("Vary", "Accept")
		return Empty{}, nil
	})
	mustBuild(t, app)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := doRequest(t, app, req)
	if !varyNamesField(rec.Header(), "Origin") || !varyNamesField(rec.Header(), "Accept") {
		t.Errorf("Vary = %q, want both Origin and the handler's Accept", rec.Header().Values("Vary"))
	}
}

// A refusal is written by the framework after the handler chain has run, and
// depends on the same request headers.
func TestCORSVaryIsOnAnErrorAndAPreflight(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.CORS = CORSOptions{AllowedOrigins: []string{"https://app.example"}}
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		ctx.SetHeader("Vary", "Accept")
		return rtOut{}, Conflict("no")
	})
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusConflict)
	if !varyNamesField(rec.Header(), "Origin") {
		t.Errorf("error response: Vary = %q, want it to name Origin", rec.Header().Values("Vary"))
	}

	req = httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec = doRequest(t, app, req)
	assertStatus(t, rec, http.StatusNoContent)
	for _, field := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		if !varyNamesField(rec.Header(), field) {
			t.Errorf("preflight: Vary = %q, want it to name %s", rec.Header().Values("Vary"), field)
		}
	}
}

func TestLocaleVarySurvivesAHandlerThatSetsVary(t *testing.T) {
	t.Parallel()
	for name, handler := range varyHandlers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, err := i18n.New(i18n.StoreOptions{AvailableLocales: []string{"en", "fr"}})
			if err != nil {
				t.Fatal(err)
			}
			_ = store.StoreTranslations("fr", map[string]any{"hi": "bonjour"})
			_ = store.StoreTranslations("en", map[string]any{"hi": "hello"})
			opts := quietOptions()
			opts.I18n = I18nOptions{Store: store}
			app := New(opts)
			app.Get("/x", handler)
			mustBuild(t, app)
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Accept-Language", "fr")
			rec := doRequest(t, app, req)
			assertStatus(t, rec, http.StatusOK)
			if got := rec.Header().Get("Content-Language"); got != "fr" {
				t.Fatalf("Content-Language = %q, want fr", got)
			}
			if !varyNamesField(rec.Header(), "Accept-Language") {
				t.Errorf("Vary = %q, want it to keep naming Accept-Language", rec.Header().Values("Vary"))
			}
		})
	}
}

func TestVersionVarySurvivesAHandlerThatSetsVary(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningHeader, Header: "X-Api-Version"}
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		ctx.SetHeader("Vary", "Accept")
		return rtOut{OK: true}, nil
	}, WithVersion("1"))
	app.Get("/x", versionHandler("2"), WithVersion("2"))
	mustBuild(t, app)
	rec := doHeader(t, app, http.MethodGet, "/x", "X-Api-Version", "1")
	assertStatus(t, rec, http.StatusOK)
	if !varyNamesField(rec.Header(), "X-Api-Version") || !varyNamesField(rec.Header(), "Accept") {
		t.Errorf("Vary = %q, want both X-Api-Version and the handler's Accept", rec.Header().Values("Vary"))
	}
}
