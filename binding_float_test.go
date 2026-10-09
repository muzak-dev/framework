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

	// An exponent and a leading zero are decimal, so they stay. A hex float
	// does not; see TestBinderNumbersAreDecimal.
	req := httptest.NewRequest("GET", "/pay/08?q=-1.5&many=1e3&many=2E-1", nil)
	req.Header.Set("X-Amount", "1e308")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"header":1e+308,"many":[1000,0.2],"path":8,"query":-1.5}`)
}

type intParamsIn struct {
	Path   int    `path:"n"`
	Query  int64  `query:"q"`
	Header *int   `header:"X-N"`
	Cookie int    `cookie:"n"`
	Many   []int8 `query:"many"`
}

// TestBinderNumbersAreDecimal covers the grammar a number parameter is read
// with. strconv.ParseFloat reads Go's literal syntax, so "0x1p-2" was a
// quarter, "1_000" a thousand and ".5" a half, none of which a JSON body
// accepts and none of which the document's type: number describes; a proxy
// or a firewall reading the same parameter by the JSON rules saw something
// else from what the handler got. ParseInt took a leading "+" the body
// refuses. A parameter now takes the JSON number grammar, with leading zeros
// allowed, since they are decimal and common in what people type.
func TestBinderNumbersAreDecimal(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/pay/{amount}", func(ctx *Context, in floatParamsIn) (floatParamsIn, error) { return in, nil })
	app.Post("/form", func(ctx *Context, in floatFormIn) (floatFormIn, error) { return in, nil })
	app.Get("/n/{n}", func(ctx *Context, in intParamsIn) (intParamsIn, error) { return in, nil })
	mustBuild(t, app)

	locations := func(base, query, header, cookie, many, value string) map[string]*http.Request {
		h := httptest.NewRequest("GET", base+"1", nil)
		h.Header.Set(header, value)
		c := httptest.NewRequest("GET", base+"1", nil)
		c.AddCookie(&http.Cookie{Name: cookie, Value: value})
		return map[string]*http.Request{
			"path":   httptest.NewRequest("GET", base+url.PathEscape(value), nil),
			"query":  httptest.NewRequest("GET", base+"1?"+query+"="+url.QueryEscape(value), nil),
			"many":   httptest.NewRequest("GET", base+"1?"+many+"=1&"+many+"="+url.QueryEscape(value), nil),
			"header": h,
			"cookie": c,
		}
	}
	for _, bad := range []string{"0x1p-2", "0x10", "1_000", ".5", "5.", "+1.5", "1e", "1e+", "-", "1.5.2", "0b1", "1.5f"} {
		requests := locations("/pay/", "q", "X-Amount", "amount", "many", bad)
		form := httptest.NewRequest("POST", "/form", strings.NewReader("amount="+url.QueryEscape(bad)))
		form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		requests["form"] = form
		for name, req := range requests {
			rec := doRequest(t, app, req)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must be a valid number") {
				t.Errorf("float %s %q: status %d body %s, want 422 must be a valid number", name, bad, rec.Code, rec.Body.String())
			}
		}
	}
	for _, bad := range []string{"+5", "0x10", "1_000", "1e3", "1.0", "-", "+0"} {
		for name, req := range locations("/n/", "q", "X-N", "n", "many", bad) {
			rec := doRequest(t, app, req)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "must be a valid integer") {
				t.Errorf("integer %s %q: status %d body %s, want 422 must be a valid integer", name, bad, rec.Code, rec.Body.String())
			}
		}
	}
	for _, good := range []string{"0", "-0", "7", "007", "-12"} {
		for name, req := range locations("/n/", "q", "X-N", "n", "many", good) {
			if rec := doRequest(t, app, req); rec.Code != http.StatusOK {
				t.Errorf("integer %s %q: status %d body %s, want 200", name, good, rec.Code, rec.Body.String())
			}
		}
	}
}
