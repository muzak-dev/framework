package muzak

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type floatParamsIn struct {
	Path   float64   `path:"amount"`
	Query  float32   `query:"q"`
	Header *float64  `header:"X-Amount"`
	Cookie float64   `cookie:"amount"`
	Many   []float64 `query:"many"`
}

type floatFormIn struct {
	Amount float64 `form:"amount"`
}

// TestBinderRefusesNonFiniteFloats is the review's withdrawal example turned
// round: NaN fails every comparison, so a handler that checks
// `amount > balance` lets it through, and an infinity passes any lower bound.
// Every location that converts text to a float has to refuse both.
func TestBinderRefusesNonFiniteFloats(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/pay/{amount}", func(ctx *Context, in floatParamsIn) (floatParamsIn, error) { return in, nil })
	app.Post("/form", func(ctx *Context, in floatFormIn) (floatFormIn, error) { return in, nil })
	mustBuild(t, app)

	for _, bad := range []string{"NaN", "nan", "-NaN", "Inf", "+Inf", "-inf", "Infinity", "-infinity", "1e400"} {
		for _, target := range []struct {
			name  string
			build func() *http.Request
		}{
			{"path", func() *http.Request { return httptest.NewRequest("GET", "/pay/"+url.PathEscape(bad), nil) }},
			{"query", func() *http.Request { return httptest.NewRequest("GET", "/pay/1?q="+url.QueryEscape(bad), nil) }},
			{"repeated query", func() *http.Request {
				return httptest.NewRequest("GET", "/pay/1?many=2&many="+url.QueryEscape(bad), nil)
			}},
			{"header", func() *http.Request {
				req := httptest.NewRequest("GET", "/pay/1", nil)
				req.Header.Set("X-Amount", bad)
				return req
			}},
			{"cookie", func() *http.Request {
				req := httptest.NewRequest("GET", "/pay/1", nil)
				req.AddCookie(&http.Cookie{Name: "amount", Value: bad})
				return req
			}},
			{"form", func() *http.Request {
				req := httptest.NewRequest("POST", "/form", strings.NewReader("amount="+url.QueryEscape(bad)))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return req
			}},
		} {
			rec := doRequest(t, app, target.build())
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must be a valid number") {
				t.Errorf("%s %q: status %d body %s, want 422 must be a valid number", target.name, bad, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestBinderAcceptsFiniteFloats(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/pay/{amount}", func(ctx *Context, in floatParamsIn) (map[string]any, error) {
		return map[string]any{"path": in.Path, "query": in.Query, "header": *in.Header, "many": in.Many}, nil
	})
	mustBuild(t, app)

	// Hex floats are ordinary ParseFloat syntax and finite, so they stay.
	req := httptest.NewRequest("GET", "/pay/0x1p3?q=-1.5&many=1e3&many=2", nil)
	req.Header.Set("X-Amount", "1e308")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"header":1e+308,"many":[1000,2],"path":8,"query":-1.5}`)
}
