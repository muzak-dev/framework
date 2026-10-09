package muzak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// benchObservedApp is benchApp with the access log left on, which is the
// configuration whose cost the observability hooks touch: the access log and
// a failure are where trace identifiers are looked for.
func benchObservedApp(b *testing.B, opts AppOptions) *App {
	b.Helper()
	opts.Title = "Benchmark"
	opts.Version = "1.0.0"
	opts.LoggerOptions = LoggerOptions{Format: LogFormatNone}
	opts.DisableDocs = true
	app := New(opts)
	app.Get("/api/v1/plain/{id}", func(ctx *Context, in struct {
		ID string `path:"id"`
	}) (benchOut, error) {
		return benchOut{ID: in.ID}, nil
	})
	if err := app.Build(); err != nil {
		b.Fatalf("Build: %v", err)
	}
	return app
}

// BenchmarkObservabilityOff measures a routed request with the access log on
// and neither tracing nor an observer configured. Its allocations are the
// baseline the feature must not add to.
func BenchmarkObservabilityOff(b *testing.B) {
	app := benchObservedApp(b, AppOptions{})
	req := httptest.NewRequest("GET", "/api/v1/plain/12345", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusOK)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

// noopTracer starts spans that record nothing, so that the benchmark below
// measures what the framework does for tracing rather than what an exporter
// does.
type noopTracer struct{}

func (noopTracer) StartSpan(context.Context, SpanStart) Span { return noopSpan{} }

// BenchmarkObservabilityOn measures the same request with a Tracer and an
// observer configured, which is the cost of what the framework does for
// them: two identifiers drawn, the span's attributes, the access log's trace
// fields and the observation.
func BenchmarkObservabilityOn(b *testing.B) {
	app := benchObservedApp(b, AppOptions{
		Tracing:  TracingOptions{Tracer: noopTracer{}},
		Observer: RequestObserverFunc(func(RequestObservation) {}),
	})
	req := httptest.NewRequest("GET", "/api/v1/plain/12345", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusOK)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}

// BenchmarkObservabilityOffNotFound is the same for a request nothing routes,
// which goes through the failure path.
func BenchmarkObservabilityOffNotFound(b *testing.B) {
	app := benchObservedApp(b, AppOptions{})
	req := httptest.NewRequest("GET", "/nothing/here", nil)
	w := newDiscardWriter()
	mustServe(b, app, req, http.StatusNotFound)
	b.ReportAllocs()
	for b.Loop() {
		serve(app, req, w)
	}
}
