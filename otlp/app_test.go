package otlp_test

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	muzak "muzak.dev/framework"
	"muzak.dev/framework/otlp"
)

type itemIn struct {
	ID string `path:"id"`
}

type itemOut struct {
	ID string `json:"id"`
}

// TestAnApplicationExportsItsSpans runs an application with the exporter as
// its Tracer, end to end over a real socket: the application starts the
// exporter, a request is traced, and stopping the application's components
// sends the spans. It uses nothing but the standard library and the root
// package, as the exporter itself does.
func TestAnApplicationExportsItsSpans(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var bodies [][]byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(collector.Close)

	exporter, err := otlp.New(otlp.Options{Endpoint: collector.URL, ServiceName: "shop", BatchInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	app := muzak.New(muzak.AppOptions{
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
		Tracing:       muzak.TracingOptions{Tracer: exporter},
	})
	app.Get("/items/{id}", func(ctx *muzak.Context, in itemIn) (itemOut, error) {
		_, span := muzak.StartSpan(ctx.Context(), "load item", muzak.SpanKindInternal)
		span.End()
		return itemOut{ID: in.ID}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The exporter is not registered as a component: naming it as the
	// Tracer is enough for the application to start and stop it.
	if err := app.StartLifecycle(ctx); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	server := httptest.NewServer(app)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/items/42", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	server.Close()
	if err := app.StopLifecycle(ctx); err != nil {
		t.Fatalf("StopLifecycle = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("the collector received %d requests, want the one flushed on shutdown", len(bodies))
	}
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string `json:"traceId"`
					SpanID       string `json:"spanId"`
					ParentSpanID string `json:"parentSpanId"`
					Name         string `json:"name"`
					Kind         int    `json:"kind"`
					Flags        int    `json:"flags"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(bodies[0], &doc); err != nil {
		t.Fatalf("the body is not the expected JSON: %v\n%s", err, bodies[0])
	}
	spans := doc.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("received %d spans, want the server span and its child", len(spans))
	}
	child, serverSpan := spans[0], spans[1]
	if serverSpan.Name != "GET /items/{id}" || serverSpan.Kind != 2 || serverSpan.ParentSpanID != "b7ad6b7169203331" || serverSpan.Flags != 0x301 {
		t.Errorf("server span = %+v", serverSpan)
	}
	if child.Name != "load item" || child.ParentSpanID != serverSpan.SpanID || child.TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("child span = %+v", child)
	}
	if stats := exporter.Stats(); stats.Exported != 2 {
		t.Errorf("stats = %+v", stats)
	}
}
