package muzak

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A urlencoded body carries no file and is held in memory whole, so it is
// bounded by the route's body limit rather than by its upload limit: under
// the upload limit a form-only route buffered 32 MiB, three times what
// net/http allows on its own. A multipart body to a route that declares no
// file is bounded the same way.
func TestURLEncodedFormIsBoundedByMaxBodySize(t *testing.T) {
	t.Parallel()
	type in struct {
		S string `form:"s"`
	}
	app := New(quietOptions())
	app.Post("/default", func(ctx *Context, _ in) (Empty, error) { return Empty{}, nil })
	app.Post("/raised", func(ctx *Context, _ in) (Empty, error) { return Empty{}, nil }, MaxBodySize(4<<20))
	built := mustBuild(t, app)

	post := func(path string, size int) *httptest.ResponseRecorder {
		body := url.Values{"s": {strings.Repeat("a", size)}}.Encode()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return doRequest(t, built, req)
	}

	rec := post("/default", 2<<20)
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
	if message := decodeError(t, rec).Error.Message; !strings.Contains(message, "1048576 byte limit") {
		t.Errorf("message = %q, want it to name the body limit", message)
	}
	assertStatus(t, post("/default", 1<<10), http.StatusOK)
	assertStatus(t, post("/raised", 2<<20), http.StatusOK)

	// A multipart body to the same route carries no file either, so it is
	// bounded the same way.
	req := uploadRequest(t, "/default", []string{"s", strings.Repeat("a", 2<<20)})
	assertStatus(t, doRequest(t, built, req), http.StatusRequestEntityTooLarge)
	req = uploadRequest(t, "/raised", []string{"s", strings.Repeat("a", 2<<20)})
	assertStatus(t, doRequest(t, built, req), http.StatusOK)
}

// With the body limit removed, the urlencoded body is left unwrapped, so
// net/http's own cap on a form still applies rather than nothing at all.
func TestURLEncodedFormWithoutALimitKeepsNetHTTPsCap(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.MaxBodySize = -1
	app := New(opts)
	app.Post("/f", func(ctx *Context, _ struct {
		S string `form:"s"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	built := mustBuild(t, app)

	req := httptest.NewRequest(http.MethodPost, "/f", strings.NewReader("s="+strings.Repeat("a", 11<<20)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	assertStatus(t, doRequest(t, built, req), http.StatusBadRequest)

	req = httptest.NewRequest(http.MethodPost, "/f", strings.NewReader("s=ok"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	assertStatus(t, doRequest(t, built, req), http.StatusOK)
}
