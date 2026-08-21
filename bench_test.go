package badele

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
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
// benchmarks measure the framework rather than the recorder.
type discardWriter struct {
	header http.Header
}

func newDiscardWriter() *discardWriter {
	return &discardWriter{header: make(http.Header, 8)}
}

func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(int)             {}

// serve runs one request through the application.
func serve(app *App, req *http.Request, w *discardWriter) {
	clear(w.header)
	app.ServeHTTP(w, req)
}

func BenchmarkRouteStatic(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/users/me/settings", nil)
	w := newDiscardWriter()
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkRouteParam(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/plain/12345", nil)
	w := newDiscardWriter()
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkRouteNotFound(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/nothing/here", nil)
	w := newDiscardWriter()
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

func BenchmarkBindEmpty(b *testing.B) {
	app := benchApp(b)
	req := httptest.NewRequest("GET", "/api/v1/empty", nil)
	w := newDiscardWriter()
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
