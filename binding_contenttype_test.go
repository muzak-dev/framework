package muzak

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type transferIn struct {
	To     string `json:"to"`
	Amount int    `json:"amount"`
}

// TestJSONBodyWithoutContentTypeIsRefused is the review's cross-site transfer:
// fetch with a Blob body sends no Content-Type, which is a request a browser
// makes without a CORS preflight. The route must not act on it.
func TestJSONBodyWithoutContentTypeIsRefused(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/transfer", func(ctx *Context, in transferIn) (transferIn, error) { return in, nil })
	mustBuild(t, app)

	req := httptest.NewRequest("POST", "/transfer", strings.NewReader(`{"to":"attacker","amount":1000}`))
	req.Header.Del("Content-Type")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusUnsupportedMediaType)
	if !strings.Contains(rec.Body.String(), "no Content-Type") {
		t.Errorf("body = %s, want the missing Content-Type named", rec.Body.String())
	}

	// A body of unknown length, sent chunked, is refused the same way.
	req = httptest.NewRequest("POST", "/transfer", io.NopCloser(strings.NewReader(`{"to":"attacker","amount":1}`)))
	req.ContentLength = -1
	assertStatus(t, doRequest(t, app, req), http.StatusUnsupportedMediaType)

	req = httptest.NewRequest("POST", "/transfer", strings.NewReader(`{"to":"attacker","amount":1000}`))
	req.Header.Set("Content-Type", "text/plain")
	assertStatus(t, doRequest(t, app, req), http.StatusUnsupportedMediaType)

	req = httptest.NewRequest("POST", "/transfer", strings.NewReader(`{"to":"friend","amount":5}`))
	req.Header.Set("Content-Type", "application/json")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
}

// TestEmptyBodyWithoutContentTypeIsNotAMediaTypeProblem checks that the label
// is demanded only of a body that exists: a bodiless call is answered with the
// same "is required" it always was, not with a 415 about a header nobody
// needed to send.
func TestEmptyBodyWithoutContentTypeIsNotAMediaTypeProblem(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/transfer", func(ctx *Context, in transferIn) (transferIn, error) { return in, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/transfer")
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if got := decodeError(t, rec).Error.Details; len(got) != 1 || got[0].Location != "body" || got[0].Issue != "is required" {
		t.Errorf("details = %+v, want the body reported as required", got)
	}
}

// TestUnlabelledBodyRuleLeavesOtherBodiesAlone checks the routes that do not
// decode JSON: a form route already demanded its own media type, and a route
// that only captures the raw bytes decodes nothing at all.
func TestUnlabelledBodyRuleLeavesOtherBodiesAlone(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/form", func(ctx *Context, in floatFormIn) (floatFormIn, error) { return in, nil })
	app.Post("/raw", func(ctx *Context, _ Empty) (map[string]string, error) {
		raw, _ := ctx.RawBody()
		return map[string]string{"raw": string(raw)}, nil
	}, CaptureBody())
	mustBuild(t, app)

	req := httptest.NewRequest("POST", "/form", strings.NewReader("amount=2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)

	req = httptest.NewRequest("POST", "/form", strings.NewReader("amount=2"))
	assertStatus(t, doRequest(t, app, req), http.StatusUnsupportedMediaType)

	req = httptest.NewRequest("POST", "/raw", strings.NewReader("anything"))
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"raw":"anything"}`)
}
