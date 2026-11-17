package muzak

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClientCarriesTheTraceOfTheRequestBeingServed joins the outbound client
// to tracing: a call made while serving a traced request carries a
// traceparent naming that request's span as its parent and its sampling
// decision, so the service called records its work as part of the same
// trace, while a traceparent the caller set is left alone and a request that
// is not traced sends none.
func TestClientCarriesTheTraceOfTheRequestBeingServed(t *testing.T) {
	t.Parallel()
	upstream := newRecordingServer(t, func(http.ResponseWriter, *http.Request) {})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", upstream.Server)

	call := func(ctx *Context, path string, header http.Header) error {
		req, err := http.NewRequestWithContext(ctx.Context(), http.MethodGet, "http://api.example.com"+path, nil)
		if err != nil {
			return err
		}
		for name, values := range header {
			req.Header[name] = values
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	tracer := &recordingTracer{}
	opts := quietOptions()
	opts.Tracing = TracingOptions{Tracer: tracer}
	app := New(opts)
	app.Get("/traced", func(ctx *Context, _ Empty) (Empty, error) {
		return Empty{}, call(ctx, "/traced", nil)
	})
	app.Get("/own", func(ctx *Context, _ Empty) (Empty, error) {
		return Empty{}, call(ctx, "/own", http.Header{"Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}})
	})
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/traced", ""), http.StatusOK)
	span := tracer.only(t)
	want := span.start.SpanContext.Traceparent()
	if got := upstream.headers("api.example.com/traced").Get(HeaderTraceparent); got != want {
		t.Errorf("traceparent = %q, want the served request's span %q", got, want)
	}

	assertStatus(t, do(t, app, http.MethodGet, "/own", ""), http.StatusOK)
	if got := upstream.headers("api.example.com/own").Get(HeaderTraceparent); got != "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01" {
		t.Errorf("traceparent = %q, want the caller's own left alone", got)
	}

	// Without tracing, nothing is sent.
	untraced := New(quietOptions())
	untraced.Get("/plain", func(ctx *Context, _ Empty) (Empty, error) {
		return Empty{}, call(ctx, "/plain", nil)
	})
	mustBuild(t, untraced)
	assertStatus(t, do(t, untraced, http.MethodGet, "/plain", ""), http.StatusOK)
	if got := upstream.headers("api.example.com/plain").Values(HeaderTraceparent); len(got) != 0 {
		t.Errorf("traceparent = %q, want none from an application that does not trace", got)
	}
}

type recordingClientObserver struct {
	mu              sync.Mutex
	attempts        int
	budgetExhausted int
	breakerChanges  []string
}

func (o *recordingClientObserver) OnAttempt(_ *http.Request, _ int, _ int, _ time.Duration, _ error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts++
}
func (o *recordingClientObserver) OnRetryBudgetExhausted(string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.budgetExhausted++
}
func (o *recordingClientObserver) OnBreakerStateChange(host, fromState, toState string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.breakerChanges = append(o.breakerChanges, host+":"+fromState+"->"+toState)
}

func TestClientSpanKindClientAndObserver(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	tracer := &recordingTracer{}
	obs := &recordingClientObserver{}
	client, network := newTestClient(t, ClientOptions{
		Tracer:         tracer,
		Observer:       obs,
		MaxAttempts:    2,
		RetryBaseDelay: time.Millisecond,
		RetryMaxDelay:  time.Millisecond,
		RetryBudget:    RetryBudget{Ratio: 0.1, Burst: 1},
		CircuitBreaker: CircuitBreakerOptions{Threshold: 2, Cooldown: time.Minute},
	})
	network.serve("api.example.com", publicA, "80", upstream.Server)

	// First call: attempt 1 -> 503, spends the 1 retry in budget, attempt 2 -> 503 (breaker trips closed->open).
	req1, _ := http.NewRequest(http.MethodGet, "http://api.example.com/fail", nil)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp1.Body.Close()

	// Verify SpanKindClient span emitted and traceparent sent.
	tracer.mu.Lock()
	spans := append([]*recordedSpan(nil), tracer.spans...)
	tracer.mu.Unlock()
	if len(spans) != 1 || spans[0].start.Kind != SpanKindClient {
		t.Fatalf("spans = %+v, want 1 SpanKindClient span", spans)
	}
	if got := upstream.headers("api.example.com/fail").Get(HeaderTraceparent); got != spans[0].start.SpanContext.Traceparent() {
		t.Fatalf("traceparent = %q, want %q", got, spans[0].start.SpanContext.Traceparent())
	}

	obs.mu.Lock()
	attempts := obs.attempts
	changes := append([]string(nil), obs.breakerChanges...)
	obs.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(changes) == 0 || changes[0] != "http://api.example.com:80:closed->open" {
		t.Fatalf("breaker changes = %v, want http://api.example.com:80:closed->open", changes)
	}
}
