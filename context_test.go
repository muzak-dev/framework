package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextAccessors(t *testing.T) {
	t.Parallel()
	type out struct {
		Path        string `json:"path"`
		PathMissing bool   `json:"path_missing"`
		Query       string `json:"query"`
		QueryEmpty  bool   `json:"query_empty"`
		QueryAbsent bool   `json:"query_absent"`
		Values      int    `json:"values"`
		Header      string `json:"header"`
		Cookie      string `json:"cookie"`
		CookieErr   bool   `json:"cookie_err"`
		Method      string `json:"method"`
		Route       string `json:"route"`
		RequestID   bool   `json:"request_id"`
		Status      int    `json:"status"`
	}

	app := New(quietOptions())
	app.Get("/things/{id}", func(ctx *Context, _ Empty) (out, error) {
		_, pathMissing := ctx.LookupPath("nope")
		_, queryEmptyOK := ctx.LookupQuery("empty")
		_, queryAbsentOK := ctx.LookupQuery("absent")
		cookie, cookieErr := ctx.Cookie("session")
		cookieValue := ""
		if cookieErr == nil {
			cookieValue = cookie.Value
		}
		_, missingCookieErr := ctx.Cookie("absent")

		ctx.SetHeader("X-Set", "set")
		ctx.AddHeader("X-Added", "one")
		ctx.AddHeader("X-Added", "two")
		ctx.SetCookie(&http.Cookie{Name: "issued", Value: "yes", HttpOnly: true})

		if ctx.Logger() == nil {
			return out{}, NewHTTPError(500, "no logger")
		}
		if ctx.Context() == nil {
			return out{}, NewHTTPError(500, "no request context")
		}
		if ctx.ResponseWriter() == nil {
			return out{}, NewHTTPError(500, "no response writer")
		}

		return out{
			Path:        ctx.PathValue("id"),
			PathMissing: !pathMissing,
			Query:       ctx.Query("q"),
			QueryEmpty:  queryEmptyOK,
			QueryAbsent: !queryAbsentOK,
			Values:      len(ctx.QueryValues("multi")),
			Header:      ctx.Header("X-Thing"),
			Cookie:      cookieValue,
			CookieErr:   missingCookieErr != nil,
			Method:      ctx.Request().Method,
			Route:       ctx.Route().Path,
			RequestID:   ctx.RequestID() != "",
			Status:      ctx.Status(),
		}, nil
	})
	mustBuild(t, app)

	req := httptest.NewRequest("GET", "/things/abc?q=search&empty=&multi=1&multi=2", nil)
	req.Header.Set("X-Thing", "value")
	req.AddCookie(&http.Cookie{Name: "session", Value: "sess"})

	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{
		"path":"abc","path_missing":true,
		"query":"search","query_empty":true,"query_absent":true,"values":2,
		"header":"value","cookie":"sess","cookie_err":true,
		"method":"GET","route":"/things/{id}","request_id":true,"status":200
	}`)

	if got := rec.Header().Get("X-Set"); got != "set" {
		t.Errorf("X-Set = %q", got)
	}
	if got := rec.Header().Values("X-Added"); len(got) != 2 {
		t.Errorf("X-Added = %v, want two values", got)
	}
	if got := rec.Header().Get("Set-Cookie"); !strings.Contains(got, "issued=yes") {
		t.Errorf("Set-Cookie = %q", got)
	}
}

func TestSetStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		declared []RouteOption
		set      int
		want     int
	}{
		{name: "default is 200", want: http.StatusOK},
		{name: "declared status", declared: []RouteOption{Status(201)}, want: 201},
		{name: "imperative status wins", declared: []RouteOption{Status(201)}, set: 202, want: 202},
		{name: "imperative status alone", set: 418, want: 418},
		{name: "an impossible code is clamped", set: 9999, want: 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
				if tc.set != 0 {
					ctx.SetStatus(tc.set)
				}
				return rtOut{OK: true}, nil
			}, tc.declared...)
			mustBuild(t, app)

			assertStatus(t, do(t, app, "GET", "/x"), tc.want)
		})
	}
}

// TestSetStatusAfterWriteIsIgnored covers the programming error the
// documentation describes: once the body has started, the status is settled.
func TestSetStatusAfterWriteIsIgnored(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (Empty, error) {
		w := ctx.ResponseWriter()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("streamed"))
		ctx.SetStatus(http.StatusTeapot)
		return Empty{}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	assertStatus(t, rec, http.StatusAccepted)
	if rec.Body.String() != "streamed" {
		t.Errorf("body = %q, want the streamed content", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "SetStatus called after the response body started") {
		t.Errorf("the misuse was not logged:\n%s", logs.String())
	}
}

// TestHandlerMayStreamItsOwnResponse covers the documented escape hatch, where
// a handler writes directly and the return value is not serialized.
func TestHandlerMayStreamItsOwnResponse(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/stream", func(ctx *Context, _ Empty) (rtOut, error) {
		w := ctx.ResponseWriter()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("chunk one\n"))
		http.NewResponseController(w).Flush()
		_, _ = w.Write([]byte("chunk two\n"))
		return rtOut{}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/stream")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != "chunk one\nchunk two\n" {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the one the handler set", got)
	}
}

func TestStatusesWithoutABody(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
				return rtOut{OK: true}, nil
			}, Status(status))
			mustBuild(t, app)

			rec := do(t, app, "GET", "/x")
			assertStatus(t, rec, status)
			if rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty for %d", rec.Body.String(), status)
			}
		})
	}
}

func TestContextResetClearsEverything(t *testing.T) {
	t.Parallel()
	c := &Context{
		w:         &responseWriter{},
		r:         httptest.NewRequest("GET", "/x", nil),
		route:     &Route{Path: "/x"},
		status:    201,
		requestID: "abc",
		deps:      []depValue{{typ: emptyType, val: Empty{}}},
	}
	c.reset()

	if c.w != nil || c.r != nil || c.route != nil || c.logger != nil {
		t.Error("reset left a reference behind")
	}
	if c.status != 0 || c.requestID != "" {
		t.Errorf("reset left status %d and request id %q", c.status, c.requestID)
	}
	if len(c.deps) != 0 {
		t.Errorf("reset left %d dependencies", len(c.deps))
	}
	if cap(c.deps) == 0 {
		t.Error("reset discarded the dependency slice's capacity, defeating pooling")
	}
	// The cleared entry must not still point at the old value.
	restored := c.deps[:1]
	if restored[0].val != nil || restored[0].typ != nil {
		t.Error("reset left a dependency value reachable in the backing array")
	}
}

func TestContentTypeIsNotOverwritten(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		ctx.SetHeader("Content-Type", "application/vnd.custom+json")
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	if got := rec.Header().Get("Content-Type"); got != "application/vnd.custom+json" {
		t.Errorf("Content-Type = %q, want the one the handler set", got)
	}
}

func TestResponseSetsContentLength(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler)
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	want := len(rec.Body.Bytes())
	if got := rec.Header().Get("Content-Length"); got != itoa(want) {
		t.Errorf("Content-Length = %q, want %d", got, want)
	}
}

// itoa keeps the assertion above readable without pulling strconv into the file
// for a single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
