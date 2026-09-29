package muzak

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A handler that declares its success response's headers and then fails used
// to hand every one of them to the error envelope, so a 404 quoting client
// input went out as text/html, as an attachment, and publicly cacheable for a
// day. The entity and caching headers are dropped, and everything describing
// the exchange rather than the body survives.
func TestErrorResponseDropsSuccessEntityHeaders(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/profile/{name}", func(ctx *Context, in struct {
		Name string `path:"name"`
	}) (HTML, error) {
		ctx.SetHeader("Content-Type", "text/html; charset=utf-8")
		ctx.SetHeader("Cache-Control", "public, max-age=86400")
		ctx.SetHeader("Expires", "Thu, 01 Jan 2099 00:00:00 GMT")
		ctx.SetHeader("Content-Disposition", `attachment; filename="profile.html"`)
		ctx.SetHeader("Content-Encoding", "gzip")
		ctx.SetHeader("Content-Language", "fr")
		ctx.SetHeader("Content-Length", "12345")
		ctx.SetHeader("ETag", `"v1"`)
		ctx.SetHeader("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
		// What the response depends on and how to try again stay true of
		// the error, so they are kept.
		ctx.AddHeader("Vary", "Cookie")
		ctx.SetHeader("Retry-After", "30")
		ctx.SetHeader("WWW-Authenticate", `Bearer realm="api"`)
		ctx.SetHeader("X-Trace", "kept")
		ctx.SetCookie(&http.Cookie{Name: "seen", Value: "1"})
		return "", NotFound(fmt.Sprintf("no profile named %s", in.Name))
	})
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/profile/%3Cimg%20src=x%20onerror=alert(1)%3E")
	assertStatus(t, rec, http.StatusNotFound)
	header := rec.Header()
	if got := header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the envelope's own", got)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	for _, name := range []string{"Content-Disposition", "Content-Encoding", "Content-Language", "Expires", "ETag", "Last-Modified"} {
		if got := header.Get(name); got != "" {
			t.Errorf("%s = %q survived onto the error response", name, got)
		}
	}
	if got, want := header.Get("Content-Length"), fmt.Sprint(rec.Body.Len()); got != want {
		t.Errorf("Content-Length = %q, want the envelope's %s", got, want)
	}
	for name, want := range map[string]string{
		"Vary":             "Cookie",
		"Retry-After":      "30",
		"WWW-Authenticate": `Bearer realm="api"`,
		"X-Trace":          "kept",
		"Set-Cookie":       "seen=1",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q kept", name, got, want)
		}
	}
	if header.Get(HeaderRequestID) == "" {
		t.Error("the request identifier was dropped from the error response")
	}
	if decodeError(t, rec).Error.Status != http.StatusNotFound {
		t.Errorf("body = %s, want the JSON envelope", rec.Body)
	}
}

// Content-Language is put back to the locale the envelope is written in, since
// a handler may have changed it for content it never sent.
func TestErrorResponseContentLanguageIsTheRequestLocale(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.I18n = i18nOptions(t)
	app := New(options)
	app.Get("/greet", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.SetHeader("Content-Language", "fr")
		return Empty{}, NotFound("")
	})
	mustBuild(t, app)

	rec := send(t, app, localeRequest{headers: map[string]string{"Accept-Language": "es"}})
	assertStatus(t, rec, http.StatusNotFound)
	if got := rec.Header().Get("Content-Language"); got != "es" {
		t.Errorf("Content-Language = %q, want es, the locale the error is written in", got)
	}
}

// The headers are cleared before the renderer runs, so a renderer that
// declares its own Content-Type or caching keeps them.
func TestErrorRendererHeadersSurviveTheReset(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.ErrorRenderer = func(ctx *Context, err error) (int, any) {
		ctx.SetHeader("Content-Type", "application/problem+json")
		ctx.SetHeader("Cache-Control", "max-age=5")
		return http.StatusTeapot, map[string]string{"title": "teapot"}
	}
	app := New(options)
	app.Get("/x", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.SetHeader("Content-Type", "text/plain")
		ctx.SetHeader("ETag", `"x"`)
		return Empty{}, BadRequest("")
	})
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusTeapot)
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want the renderer's", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "max-age=5" {
		t.Errorf("Cache-Control = %q, want the renderer's", got)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("ETag = %q survived onto the error response", got)
	}
}

// A renderer that writes no body still does not inherit a length or a type
// describing a body that is not there.
func TestErrorWithoutBodyDropsSuccessEntityHeaders(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.ErrorRenderer = func(*Context, error) (int, any) { return http.StatusGone, nil }
	app := New(options)
	app.Get("/x", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.SetHeader("Content-Type", "text/html")
		ctx.SetHeader("Cache-Control", "public, max-age=60")
		return Empty{}, BadRequest("")
	})
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusGone)
	if got := rec.Header().Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q on a response with no body", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// Recovery's own 500 applies the same rule as the routed error path.
func TestRecoveryErrorDropsSuccessEntityHeaders(t *testing.T) {
	t.Parallel()
	logger, _ := captureLogger(t)
	handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("Content-Language", "fr")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Vary", "Cookie")
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assertStatus(t, rec, http.StatusInternalServerError)
	header := rec.Header()
	if got := header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	for _, name := range []string{"Content-Disposition", "Content-Language", "ETag"} {
		if got := header.Get(name); got != "" {
			t.Errorf("%s = %q survived onto the 500", name, got)
		}
	}
	if got := header.Get("Vary"); got != "Cookie" {
		t.Errorf("Vary = %q, want it kept", got)
	}
}

// An integrity or range header describes the body a handler meant to send.
// Left on the error envelope, a Content-Digest is a checksum of bytes that
// never went out and Accept-Ranges invites a Range request the envelope cannot
// honour, so they go with the other entity headers. A Location says where a
// created or moved resource is, which no failure response can say.
func TestErrorResponseDropsDigestRangeAndLocationHeaders(t *testing.T) {
	t.Parallel()
	dropped := map[string]string{
		"Content-Digest":   "sha-256=:AAAA:",
		"Repr-Digest":      "sha-256=:AAAA:",
		"Digest":           "SHA-256=AAAA",
		"Content-MD5":      "AAAA",
		"Accept-Ranges":    "bytes",
		"Trailer":          "Expires",
		"Content-Location": "/items/42",
		"Location":         "/items/42",
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Empty, error) {
		for name, value := range dropped {
			ctx.SetHeader(name, value)
		}
		ctx.SetHeader("X-Trace", "kept")
		return Empty{}, NotFound("")
	})
	app.Get("/panic", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.SetHeader("Location", "/items/42")
		ctx.SetHeader("Content-Digest", "sha-256=:AAAA:")
		panic("boom")
	})
	mustBuild(t, app)

	for _, path := range []string{"/x", "/panic"} {
		rec := do(t, app, http.MethodGet, path)
		for name := range dropped {
			if got := rec.Header().Get(name); got != "" {
				t.Errorf("%s: %s = %q survived onto the error response", path, name, got)
			}
		}
	}
	if got := do(t, app, http.MethodGet, "/x").Header().Get("X-Trace"); got != "kept" {
		t.Errorf("X-Trace = %q, want it kept", got)
	}
}

// redirectError is a deliberate 3xx an application may return, which is the
// one failure response a Location belongs on.
type redirectError struct{}

func (redirectError) Error() string   { return "moved" }
func (redirectError) HTTPStatus() int { return http.StatusFound }

func TestErrorResponseKeepsLocationOnARedirectStatus(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/old", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.SetHeader("Location", "/new")
		return Empty{}, redirectError{}
	})
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/old")
	assertStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); got != "/new" {
		t.Errorf("Location = %q, want /new kept on a 3xx", got)
	}
}
