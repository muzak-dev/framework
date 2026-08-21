package badele

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"
)

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler)
	mustBuild(t, app)

	first := do(t, app, "GET", "/x").Header().Get(HeaderRequestID)
	second := do(t, app, "GET", "/x").Header().Get(HeaderRequestID)

	if first == "" || second == "" {
		t.Fatal("no request identifier was assigned")
	}
	if first == second {
		t.Error("two requests were given the same identifier")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Errorf("the identifier %q is not a UUID: %v", first, err)
	}
}

func TestRequestIDInboundHeader(t *testing.T) {
	t.Parallel()
	supplied := "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"

	t.Run("untrusted by default", func(t *testing.T) {
		t.Parallel()
		app := New(quietOptions())
		app.Get("/x", okHandler)
		mustBuild(t, app)

		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(HeaderRequestID, supplied)
		if got := doRequest(t, app, req).Header().Get(HeaderRequestID); got == supplied {
			t.Error("a client-supplied identifier was trusted by default")
		}
	})

	t.Run("trusted when configured", func(t *testing.T) {
		t.Parallel()
		opts := quietOptions()
		opts.TrustRequestIDHeader = true
		app := New(opts)
		app.Get("/x", okHandler)
		mustBuild(t, app)

		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(HeaderRequestID, supplied)
		if got := doRequest(t, app, req).Header().Get(HeaderRequestID); got != supplied {
			t.Errorf("identifier = %q, want the supplied %q", got, supplied)
		}
	})

	t.Run("a non-UUID is refused even when trusting", func(t *testing.T) {
		t.Parallel()
		opts := quietOptions()
		opts.TrustRequestIDHeader = true
		app := New(opts)
		app.Get("/x", okHandler)
		mustBuild(t, app)

		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set(HeaderRequestID, "injected\nlog line")
		got := doRequest(t, app, req).Header().Get(HeaderRequestID)
		if strings.Contains(got, "injected") {
			t.Errorf("a forged identifier was accepted: %q", got)
		}
		if _, err := uuid.Parse(got); err != nil {
			t.Errorf("the fallback identifier %q is not a UUID", got)
		}
	})
}

func TestRequestIDFromContext(t *testing.T) {
	t.Parallel()
	if _, ok := RequestIDFromContext(context.Background()); ok {
		t.Error("a bare context reported an identifier")
	}
	ctx := contextWithRequestID(context.Background(), "abc")
	got, ok := RequestIDFromContext(ctx)
	if !ok || got != "abc" {
		t.Errorf("RequestIDFromContext = %q, %v", got, ok)
	}
}

func TestPanicRecovery(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.Get("/boom", func(ctx *Context, _ Empty) (rtOut, error) {
		panic("secret internal detail at 0xdeadbeef")
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/boom")
	assertStatus(t, rec, http.StatusInternalServerError)

	body := rec.Body.String()
	if strings.Contains(body, "0xdeadbeef") || strings.Contains(body, "goroutine") {
		t.Errorf("the response leaked panic detail:\n%s", body)
	}
	if code := decodeError(t, rec).Error.Code; code != CodeInternalError {
		t.Errorf("code = %q, want %q", code, CodeInternalError)
	}

	recorded := logs.String()
	if !strings.Contains(recorded, "0xdeadbeef") {
		t.Errorf("the panic value was not logged:\n%s", recorded)
	}
	if !strings.Contains(recorded, "stack") {
		t.Errorf("the stack trace was not logged:\n%s", recorded)
	}
}

func TestPanicInADependencyIsRecovered(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler, Needs(func(ctx *Context) (diValue, error) {
		panic("dependency exploded")
	}))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/x"), http.StatusInternalServerError)
}

func TestAbortHandlerPanicIsNotSwallowed(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/abort", func(ctx *Context, _ Empty) (rtOut, error) {
		panic(http.ErrAbortHandler)
	})
	mustBuild(t, app)

	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", recovered)
		}
	}()
	do(t, app, "GET", "/abort")
	t.Error("the request completed, want ErrAbortHandler to propagate")
}

// TestRecoveryMiddlewareStandalone covers the outer safety net directly,
// including its own ErrAbortHandler passthrough.
func TestRecoveryMiddlewareStandalone(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)

	t.Run("recovers and reports", func(t *testing.T) {
		handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("outer boom")
		}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
		if !strings.Contains(logs.String(), "outer boom") {
			t.Errorf("the panic was not logged:\n%s", logs.String())
		}
	})

	t.Run("passes through a normal response", func(t *testing.T) {
		handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("status = %d, want 418", rec.Code)
		}
	})

	t.Run("re-panics on ErrAbortHandler", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
				t.Errorf("recovered %v, want ErrAbortHandler", recovered)
			}
		}()
		handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic(http.ErrAbortHandler)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	})

	t.Run("does not rewrite a response already on the wire", func(t *testing.T) {
		handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := asResponseWriter(w)
			rw.WriteHeader(http.StatusOK)
			_, _ = rw.Write([]byte("partial"))
			panic("too late")
		}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		if rec.Body.String() != "partial" {
			t.Errorf("body = %q, want the partial response left alone", rec.Body.String())
		}
	})
}

func TestAccessLog(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.Get("/ok", okHandler)
	app.Get("/missing", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, NewHTTPError(http.StatusNotFound, "gone")
	})
	app.Get("/broken", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, errors.New("internal")
	})
	mustBuild(t, app)

	do(t, app, "GET", "/ok")
	do(t, app, "GET", "/missing")
	do(t, app, "GET", "/broken")

	recorded := logs.String()
	for _, want := range []string{
		`"level":"INFO"`, `"msg":"GET /ok"`,
		`"level":"WARN"`, `"msg":"GET /missing"`,
		`"level":"ERROR"`, `"msg":"GET /broken"`,
		`"status":200`, `"status":404`, `"status":500`,
		`"scope":"Request"`, `"duration"`, `"bytes"`, `"request_id"`,
	} {
		if !strings.Contains(recorded, want) {
			t.Errorf("the access log is missing %s:\n%s", want, recorded)
		}
	}
}

func TestAccessLogSkipPaths(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.AccessLogOptions = AccessLogOptions{SkipPaths: []string{"/healthz"}}

	app := New(opts)
	app.Get("/healthz", okHandler)
	app.Get("/other", okHandler)
	mustBuild(t, app)

	do(t, app, "GET", "/healthz")
	do(t, app, "GET", "/other")

	recorded := logs.String()
	if strings.Contains(recorded, "/healthz") {
		t.Errorf("a skipped path was logged:\n%s", recorded)
	}
	if !strings.Contains(recorded, "/other") {
		t.Errorf("a normal path was not logged:\n%s", recorded)
	}
}

func TestAccessLogCanBeDisabled(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.DisableAccessLog = true

	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)
	do(t, app, "GET", "/x")

	if strings.Contains(logs.String(), `"scope":"Request"`) {
		t.Errorf("the access log ran despite being disabled:\n%s", logs.String())
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler)
	app.Get("/framed", func(ctx *Context, _ Empty) (rtOut, error) {
		ctx.SetHeader("X-Frame-Options", "SAMEORIGIN")
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// A handler may override a header the middleware would otherwise set.
	framed := do(t, app, "GET", "/framed")
	if got := framed.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want the handler's value", got)
	}
}

func TestSecurityHeadersCanBeDisabled(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.DisableSecurityHeaders = true
	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	if got := do(t, app, "GET", "/x").Header().Get("X-Content-Type-Options"); got != "" {
		t.Errorf("X-Content-Type-Options = %q, want it absent", got)
	}
}

func TestCORSDeniesByDefault(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler)
	mustBuild(t, app)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := doRequest(t, app, req)

	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want nothing without configuration", got)
	}
}

func TestCORSConfigured(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.CORS = CORSOptions{
		AllowedOrigins:   []string{"https://app.example"},
		AllowCredentials: true,
		ExposedHeaders:   []string{"X-Total-Count"},
		MaxAge:           5 * time.Minute,
	}
	app := New(opts)
	app.Get("/x", okHandler)
	app.Post("/x", okHandler)
	mustBuild(t, app)

	t.Run("allowed origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Origin", "https://app.example")
		rec := doRequest(t, app, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
			t.Errorf("Allow-Origin = %q", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Errorf("Allow-Credentials = %q", got)
		}
		if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "X-Total-Count" {
			t.Errorf("Expose-Headers = %q", got)
		}
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary = %q, want it to include Origin", got)
		}
	})

	t.Run("denied origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("a denied origin received %q", got)
		}
	})

	t.Run("preflight", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/x", nil)
		req.Header.Set("Origin", "https://app.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusNoContent)
		if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
			t.Errorf("Allow-Methods = %q", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
			t.Errorf("Allow-Headers = %q", got)
		}
		if got := rec.Header().Get("Access-Control-Max-Age"); got != "300" {
			t.Errorf("Max-Age = %q, want 300", got)
		}
	})

	t.Run("denied preflight", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/x", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		assertStatus(t, doRequest(t, app, req), http.StatusForbidden)
	})

	t.Run("same-origin requests are untouched", func(t *testing.T) {
		rec := do(t, app, "GET", "/x")
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("a same-origin request received %q", got)
		}
	})
}

func TestCORSWildcard(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.CORS = CORSOptions{AllowedOrigins: []string{"*"}}
	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Origin", "https://anywhere.example")
	if got := doRequest(t, app, req).Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want %q", got, "*")
	}
}

func TestCORSRefusesWildcardWithCredentials(t *testing.T) {
	t.Parallel()
	if _, err := CORS(CORSOptions{AllowedOrigins: []string{"*"}, AllowCredentials: true}); !errors.Is(err, ErrCORSWildcardCredentials) {
		t.Fatalf("CORS = %v, want ErrCORSWildcardCredentials", err)
	}

	opts := quietOptions()
	opts.CORS = CORSOptions{AllowedOrigins: []string{"*"}, AllowCredentials: true}
	app := New(opts)
	app.Get("/x", okHandler)
	if got := buildError(t, app); !strings.Contains(got, "wildcard CORS origin") {
		t.Errorf("build error = %q, want it to reject the policy", got)
	}
}

func TestCORSOriginFunc(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.CORS = CORSOptions{
		AllowOriginFunc: func(origin string) bool {
			return strings.HasSuffix(origin, ".trusted.example")
		},
	}
	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	for origin, want := range map[string]string{
		"https://a.trusted.example": "https://a.trusted.example",
		"https://evil.example":      "",
	} {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Origin", origin)
		if got := doRequest(t, app, req).Header().Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("origin %q received %q, want %q", origin, got, want)
		}
	}
}

func TestResponseWriter(t *testing.T) {
	t.Parallel()

	t.Run("records the status and byte count", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		w := asResponseWriter(rec)
		w.WriteHeader(http.StatusCreated)
		n, err := w.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if w.status != http.StatusCreated || w.bytes != 5 {
			t.Errorf("status = %d, bytes = %d", w.status, w.bytes)
		}
	})

	t.Run("a repeated WriteHeader is ignored", func(t *testing.T) {
		t.Parallel()
		w := asResponseWriter(httptest.NewRecorder())
		w.WriteHeader(http.StatusCreated)
		w.WriteHeader(http.StatusTeapot)
		if w.status != http.StatusCreated {
			t.Errorf("status = %d, want the first value to stand", w.status)
		}
	})

	t.Run("writing without a status implies 200", func(t *testing.T) {
		t.Parallel()
		w := asResponseWriter(httptest.NewRecorder())
		_, _ = w.Write([]byte("x"))
		if w.statusOrDefault() != http.StatusOK {
			t.Errorf("status = %d, want 200", w.statusOrDefault())
		}
	})

	t.Run("an untouched writer reports 200", func(t *testing.T) {
		t.Parallel()
		w := asResponseWriter(httptest.NewRecorder())
		if w.statusOrDefault() != http.StatusOK {
			t.Errorf("status = %d, want 200", w.statusOrDefault())
		}
	})

	t.Run("wrapping is not nested", func(t *testing.T) {
		t.Parallel()
		inner := asResponseWriter(httptest.NewRecorder())
		if outer := asResponseWriter(inner); outer != inner {
			t.Error("an already wrapped writer was wrapped again")
		}
	})

	t.Run("Unwrap exposes the underlying writer", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		w := asResponseWriter(rec)
		if w.Unwrap() != http.ResponseWriter(rec) {
			t.Error("Unwrap did not return the wrapped writer")
		}
	})
}

func TestWriteMinimalErrorSkipsAStartedResponse(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := asResponseWriter(rec)
	_, _ = w.Write([]byte("already sent"))

	writeMinimalError(w, "req-1")
	if rec.Body.String() != "already sent" {
		t.Errorf("body = %q, want the started response left alone", rec.Body.String())
	}
}

func TestAccessLogLevelOption(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	handler := AccessLog(logger, AccessLogOptions{Level: slog.LevelDebug})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if !strings.Contains(logs.String(), `"level":"DEBUG"`) {
		t.Errorf("the configured level was not used:\n%s", logs.String())
	}
}
