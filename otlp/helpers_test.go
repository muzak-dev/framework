package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	muzak "muzak.dev/framework"
)

// collected is one export request as the fake collector received it.
type collected struct {
	method  string
	path    string
	header  http.Header
	body    []byte
	doc     map[string]any
	arrived time.Time
}

// collector is a fake OTLP receiver. respond decides the answer to the nth
// request, counting from zero; when nil, every request is answered 200 with
// an empty JSON object, as a real collector does.
type collector struct {
	t       testing.TB
	server  *httptest.Server
	respond func(n int, w http.ResponseWriter, r *http.Request)

	mu       sync.Mutex
	requests []collected
}

func newCollector(t testing.TB, respond func(n int, w http.ResponseWriter, r *http.Request)) *collector {
	t.Helper()
	c := &collector{t: t, respond: respond}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)
	return c
}

func (c *collector) serve(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	body := raw
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			c.t.Errorf("the body is not gzip: %v", err)
			return
		}
		if body, err = io.ReadAll(zr); err != nil {
			c.t.Errorf("the gzip body is truncated: %v", err)
			return
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		c.t.Errorf("the body is not JSON: %v\n%s", err, body)
	}
	c.mu.Lock()
	n := len(c.requests)
	c.requests = append(c.requests, collected{
		method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body, doc: doc, arrived: time.Now(),
	})
	c.mu.Unlock()
	if c.respond != nil {
		c.respond(n, w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

// all returns a copy of the requests received so far.
func (c *collector) all() []collected {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collected(nil), c.requests...)
}

// spans returns every span received, across every request, in order.
func (c *collector) spans() []map[string]any {
	var out []map[string]any
	for _, request := range c.all() {
		out = append(out, spansOf(request.doc)...)
	}
	return out
}

// spansOf digs the spans out of one decoded export request.
func spansOf(doc map[string]any) []map[string]any {
	var out []map[string]any
	for _, rs := range list(doc["resourceSpans"]) {
		for _, ss := range list(object(rs)["scopeSpans"]) {
			for _, s := range list(object(ss)["spans"]) {
				out = append(out, object(s))
			}
		}
	}
	return out
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// attributes turns an OTLP attribute list into a map from key to AnyValue.
func attributes(v any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, kv := range list(v) {
		entry := object(kv)
		key, _ := entry["key"].(string)
		out[key] = object(entry["value"])
	}
	return out
}

// syncBuffer is a buffer safe for the concurrent writes of a logger.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testOptions returns options aimed at a collector, with timings short enough
// for a test and a logger the test can read.
func testOptions(endpoint string) (Options, *syncBuffer) {
	logs := &syncBuffer{}
	return Options{
		Endpoint:             endpoint,
		ServiceName:          "test-service",
		BatchInterval:        time.Hour,
		RetryInitialInterval: 5 * time.Millisecond,
		RetryMaxInterval:     20 * time.Millisecond,
		RetryMaxElapsedTime:  2 * time.Second,
		Timeout:              2 * time.Second,
		Logger:               slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, logs
}

// startExporter builds and starts an exporter, stopping it when the test
// ends.
func startExporter(t testing.TB, opts Options) *Exporter {
	t.Helper()
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	})
	return e
}

// spanStart returns the start of a sampled root span whose identifiers are
// derived from i, so that a test can find it again.
func spanStart(i int, name string, attrs ...slog.Attr) muzak.SpanStart {
	var trace muzak.TraceID
	var id muzak.SpanID
	trace[0], trace[15] = 0xab, byte(i+1)
	id[0], id[7] = 0xcd, byte(i+1)
	return muzak.SpanStart{
		Name:        name,
		Kind:        muzak.SpanKindInternal,
		SpanContext: muzak.SpanContext{TraceID: trace, SpanID: id, TraceFlags: muzak.TraceFlagsSampled},
		StartTime:   time.Now(),
		Attributes:  attrs,
	}
}

// endSpans starts and ends n spans through e.
func endSpans(e *Exporter, n int) {
	for i := range n {
		e.StartSpan(context.Background(), spanStart(i, "work")).End()
	}
}

// flush flushes e, failing the test if it cannot.
func flush(t testing.TB, e *Exporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Flush(ctx); err != nil {
		t.Fatalf("Flush = %v", err)
	}
}

// waitFor polls until condition holds, failing the test after five seconds.
func waitFor(t testing.TB, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// exporterGoroutines counts the goroutines running exporter code.
func exporterGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "muzak.dev/framework/otlp.(*Exporter).work")
}

// assertNoExporterGoroutines fails the test if an exporter's goroutine is
// still running. Stop promises it has exited by the time it returns, so this
// does not wait.
func assertNoExporterGoroutines(t testing.TB) {
	t.Helper()
	if n := exporterGoroutines(); n != 0 {
		buf := make([]byte, 1<<20)
		t.Fatalf("%d exporter goroutines are still running:\n%s", n, buf[:runtime.Stack(buf, true)])
	}
}
