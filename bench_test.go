package muzak

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// benchOut is the response model used across the benchmarks.
type benchOut struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Count int    `json:"count"`
}

// benchIn mixes every binding source, so that a single benchmark measures the
// full per-request binding cost rather than one source at a time.
type benchIn struct {
	ID     string `path:"id"`
	Limit  int    `query:"limit" default:"20"`
	Cursor string `query:"cursor"`
	Token  string `header:"X-Token"`
}

type benchBody struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Tags  []string `json:"tags,omitzero"`
}

// benchApp builds an application of a realistic size for the benchmarks.
func benchApp(b *testing.B) *App {
	b.Helper()
	app := New(AppOptions{
		Title:            "Benchmark",
		Version:          "1.0.0",
		LoggerOptions:    LoggerOptions{Format: LogFormatNone},
		DisableAccessLog: true,
		DisableDocs:      true,
	})
	// A spread of routes so that matching is measured against a populated
	// tree rather than a single entry.
	for _, path := range []string{
		"/api/v1/users", "/api/v1/users/me", "/api/v1/users/me/settings",
		"/api/v1/orders", "/api/v1/orders/{id}", "/api/v1/orders/{id}/lines",
		"/health", "/metrics",
	} {
		app.Get(path, func(ctx *Context, _ Empty) (benchOut, error) {
			return benchOut{ID: "static"}, nil
		})
	}

	app.Get("/api/v1/items/{id}", func(ctx *Context, in benchIn) (benchOut, error) {
		return benchOut{ID: in.ID, Name: in.Cursor, Owner: in.Token, Count: in.Limit}, nil
	})
	app.Get("/api/v1/plain/{id}", func(ctx *Context, in struct {
		ID string `path:"id"`
	}) (benchOut, error) {
		return benchOut{ID: in.ID}, nil
	})
	app.Get("/api/v1/empty", func(ctx *Context, _ Empty) (benchOut, error) {
		return benchOut{ID: "empty"}, nil
	})
	app.Post("/api/v1/items", func(ctx *Context, in benchBody) (benchOut, error) {
		return benchOut{Name: in.Name, Count: in.Count}, nil
	}, Status(http.StatusCreated))

	if err := app.Build(); err != nil {
		b.Fatalf("Build: %v", err)
	}
	return app
}

// discardWriter is a ResponseWriter that throws the response away, so that the
// benchmarks measure the framework rather than the recorder. It keeps the
// status so that a benchmark can prove it is measuring what it claims.
type discardWriter struct {
	header http.Header
	status int
}

func newDiscardWriter() *discardWriter {
	return &discardWriter{header: make(http.Header, 8)}
}

func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(status int)      { w.status = status }

// serve runs one request through the application.
func serve(app *App, req *http.Request, w *discardWriter) {
	clear(w.header)
	app.ServeHTTP(w, req)
}

// mustServe checks that a benchmark is measuring the response it means to,
// before the timed loop begins.
//
// It exists because a benchmark that quietly measures an error path looks
// exactly like a fast one: the work it skips is the work being measured. That
// mistake produced a published claim here once already.
func mustServe(b *testing.B, app *App, req *http.Request, want int) {
	b.Helper()
	w := newDiscardWriter()
	serve(app, req, w)
	if w.status != want {
		b.Fatalf("the benchmark serves %d, not %d; it would be measuring the wrong path", w.status, want)
	}
}

func BenchmarkRouteStatic(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/users/me/settings", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusOK)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkRouteParam(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/plain/12345", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusOK)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkRouteNotFound(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/nothing/here", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusNotFound)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkBindEmpty(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/empty", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusOK)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkBindPathQueryHeader(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/items/12345?limit=50&cursor=abcdef", nil)
	req.Header.Set("X-Token", "a-token-value")
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusOK)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkBindJSONBody(b *testing.B) {
	app := benchApp(b)
	body := `{"name":"Portal Gun","count":42,"tags":["a","b","c"]}`
	w := newDiscardWriter()
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest("POST", "/api/v1/items", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		serve(app, req, w)
	}
}

// BenchmarkBindPlanCompilation measures the start-up cost that the per-request
// path avoids by precompiling.
func BenchmarkBindPlanCompilation(b *testing.B) {
	typ := reflect.TypeFor[benchIn]()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := newBindPlan(typ, "GET", "/api/v1/items/{id}"); err != nil {
			b.Fatalf("newBindPlan: %v", err)
		}
	}
}

func BenchmarkJSONMarshal(b *testing.B) {
	value := benchOut{ID: "12345", Name: "Portal Gun", Owner: "fakecurrentuser", Count: 42}
	buf := &bytes.Buffer{}
	b.ReportAllocs()
	for b.Loop() {
		buf.Reset()
		if err := json.MarshalWrite(buf, value); err != nil {
			b.Fatalf("MarshalWrite: %v", err)
		}
	}
}

func BenchmarkJSONUnmarshal(b *testing.B) {
	body := []byte(`{"name":"Portal Gun","count":42,"tags":["a","b","c"]}`)
	b.ReportAllocs()
	for b.Loop() {
		var out benchBody
		if err := json.Unmarshal(body, &out, json.RejectUnknownMembers(true)); err != nil {
			b.Fatalf("Unmarshal: %v", err)
		}
	}
}

func BenchmarkErrorResponse(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/items/12345?limit=not-a-number", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusUnprocessableEntity)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkDependencyResolution(b *testing.B) {
	type user struct{ Name string }
	app := New(AppOptions{
		LoggerOptions:    LoggerOptions{Format: LogFormatNone},
		DisableAccessLog: true,
		DisableDocs:      true,
	})
	app.Get("/x", func(ctx *Context, _ Empty) (benchOut, error) {
		return benchOut{Owner: From[user](ctx).Name}, nil
	}, Needs(func(ctx *Context) (user, error) {
		return user{Name: "fakecurrentuser"}, nil
	}))
	if err := app.Build(); err != nil {
		b.Fatalf("Build: %v", err)
	}

	req := httptest.NewRequest("GET", "/x", nil)
	w := newDiscardWriter()
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkLoggerConsole(b *testing.B) {
	noColor := false
	logger := NewLogger(LoggerOptions{
		Format: LogFormatConsole, Output: nopWriter{}, Color: &noColor,
	})
	scoped := Scoped(logger, "UsersService")
	b.ReportAllocs()
	for b.Loop() {
		scoped.Info("user created", "user_id", 42, RequestIDKey, "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31")
	}
}

func BenchmarkLoggerJSON(b *testing.B) {
	logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: nopWriter{}})
	scoped := Scoped(logger, "UsersService")
	b.ReportAllocs()
	for b.Loop() {
		scoped.Info("user created", "user_id", 42, RequestIDKey, "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31")
	}
}

// nopWriter discards everything written to it.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// BenchmarkBaselineServeMux measures the same work through net/http's own
// router and encoder, so the framework's overhead can be read as a difference
// rather than as an absolute number.
func BenchmarkBaselineServeMux(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/plain/{id}", func(w http.ResponseWriter, r *http.Request) {
		out := benchOut{ID: r.PathValue("id")}
		buf := &bytes.Buffer{}
		if err := json.MarshalWrite(buf, out); err != nil {
			b.Fatalf("MarshalWrite: %v", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	})

	req := httptest.NewRequest("GET", "/api/v1/plain/12345", nil)
	w := newDiscardWriter()

	// The baseline proves itself too, so that a comparison is never drawn
	// against a handler that was quietly doing nothing.
	mux.ServeHTTP(w, req)
	if w.status != http.StatusOK {
		b.Fatalf("the baseline serves %d, not %d", w.status, http.StatusOK)
	}

	b.ReportAllocs()
	for b.Loop() {
		clear(w.header)
		mux.ServeHTTP(w, req)
	}
}

// BenchmarkRouteParamBare measures a Muzak route with the optional middleware
// removed, which isolates routing, binding and encoding from the request
// identifier and the security headers.
func BenchmarkRouteParamBare(b *testing.B) {
	app := New(AppOptions{
		LoggerOptions:          LoggerOptions{Format: LogFormatNone},
		DisableAccessLog:       true,
		DisableDocs:            true,
		DisableSecurityHeaders: true,
	})
	app.Get("/api/v1/plain/{id}", func(ctx *Context, in struct {
		ID string `path:"id"`
	}) (benchOut, error) {
		return benchOut{ID: in.ID}, nil
	})
	// The middleware is dropped before the application is built, so that the
	// handler chain is assembled without it. Rebuilding afterwards would fail,
	// because a router cannot be mounted twice.
	app.middleware = nil
	if err := app.Build(); err != nil {
		b.Fatalf("Build: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/plain/12345", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusOK)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

// BenchmarkValidation measures a request through a model that declares rules,
// against the same model with validation skipped.
//
// The pair is what makes the cost readable: the difference is what the rules
// themselves add, separate from routing, binding and encoding.
func BenchmarkValidation(b *testing.B) {
	body := `{"email":"rick@example.test","password":"a-long-enough-password",` +
		`"confirm_password":"a-long-enough-password","age":70,"role":"editor"}`

	run := func(b *testing.B, opts ...RouteOption) {
		app := New(AppOptions{
			LoggerOptions:    LoggerOptions{Format: LogFormatNone},
			DisableAccessLog: true,
			DisableDocs:      true,
		})
		app.Post("/signup", func(ctx *Context, in signup) (rtOut, error) {
			return rtOut{OK: true}, nil
		}, opts...)
		if err := app.Build(); err != nil {
			b.Fatalf("Build: %v", err)
		}
		w := newDiscardWriter()
		b.ReportAllocs()
		for b.Loop() {
			req := httptest.NewRequest("POST", "/signup", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			serve(app, req, w)
		}
	}

	b.Run("validated", func(b *testing.B) { run(b) })
	b.Run("skipped", func(b *testing.B) { run(b, SkipValidation()) })
}

// BenchmarkRateLimit measures a request through a rate limited route against
// the same route unlimited.
//
// The pair is what makes the cost readable: the difference is what resolving
// the client, building the key and counting the quotas add, separate from
// routing, binding and encoding. The one-quota and three-quota cases are both
// measured because the storage is consulted once per quota, which is the part
// of the cost that grows with the policy.
func BenchmarkRateLimit(b *testing.B) {
	run := func(b *testing.B, opts ...RouteOption) {
		app := New(AppOptions{
			LoggerOptions:    LoggerOptions{Format: LogFormatNone},
			DisableAccessLog: true,
			DisableDocs:      true,
		})
		app.Get("/ping", func(ctx *Context, _ Empty) (rtOut, error) {
			return rtOut{OK: true}, nil
		}, opts...)
		if err := app.Build(); err != nil {
			b.Fatalf("Build: %v", err)
		}
		// A window long enough that nothing is refused, and a limit high
		// enough that the benchmark measures counting rather than rejecting.
		req := httptest.NewRequest("GET", "/ping", nil)
		w := newDiscardWriter()
		b.ReportAllocs()
		for b.Loop() {
			serve(app, req, w)
		}
	}

	unlimited := int(^uint(0) >> 1)
	b.Run("unlimited", func(b *testing.B) { run(b) })
	b.Run("one quota", func(b *testing.B) {
		run(b, RateLimit(Quota{Name: "bench-one", Window: time.Hour, Limit: unlimited}))
	})
	b.Run("three quotas", func(b *testing.B) {
		run(b, RateLimit(
			Quota{Name: "bench-short", Window: time.Hour, Limit: unlimited},
			Quota{Name: "bench-medium", Window: time.Hour, Limit: unlimited},
			Quota{Name: "bench-long", Window: time.Hour, Limit: unlimited}))
	})
}

// BenchmarkClientIP measures resolving the client address, which every
// IP-keyed rate limit pays for on every request.
//
// The forwarded case walks a chain of trusted hops, which is what a service
// behind a load balancer and a CDN actually receives.
func BenchmarkClientIP(b *testing.B) {
	run := func(b *testing.B, opts ClientIPOptions, prepare func(*http.Request)) {
		resolver, err := newClientIPResolver(opts)
		if err != nil {
			b.Fatalf("newClientIPResolver: %v", err)
		}
		req := httptest.NewRequest("GET", "/ping", nil)
		req.RemoteAddr = "10.1.2.3:41234"
		prepare(req)
		b.ReportAllocs()
		for b.Loop() {
			resolver.resolve(req)
		}
	}

	b.Run("peer", func(b *testing.B) {
		run(b, ClientIPOptions{}, func(*http.Request) {})
	})
	b.Run("forwarded", func(b *testing.B) {
		run(b, ClientIPOptions{TrustedProxies: []string{"10.0.0.0/8"}}, func(req *http.Request) {
			req.Header.Set(DefaultForwardedHeader, "198.51.100.9, 10.4.4.4, 10.5.5.5")
		})
	})
}
