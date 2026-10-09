package muzak

import (
	"net/http"
	"testing"
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
