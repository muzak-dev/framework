package muzak

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestAppServesEventStreamsAgainAfterShutdown is the regression test for an
// application that could be run a second time but refused every event stream
// and WebSocket on it: the registries stay in the draining state Shutdown put
// them in, so each admission was answered 503 "shutting down".
func TestAppServesEventStreamsAgainAfterShutdown(t *testing.T) {
	t.Parallel()
	// A fixed port, because Addr reports the previous run's listener until the
	// next one has opened, which would make the wait below race.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	opts := quietOptions()
	opts.Addr = addr
	app := New(opts)
	app.SSE("/stream", streamItems("Plumbus"))

	streamStatus := func() int {
		deadline := time.Now().Add(10 * time.Second)
		for {
			resp, err := http.Get("http://" + addr + "/stream") //nolint:noctx // a test against a local server
			if err == nil {
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			if time.Now().After(deadline) {
				t.Fatalf("the server never came up: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	for run := 1; run <= 2; run++ {
		done := make(chan error, 1)
		go func() { done <- app.Run() }()
		if got := streamStatus(); got != http.StatusOK {
			t.Fatalf("run %d: event stream status = %d, want 200", run, got)
		}
		if err := app.Shutdown(context.Background()); err != nil {
			t.Fatalf("run %d: Shutdown = %v", run, err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run %d: Run = %v", run, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("run %d: Run did not return after Shutdown", run)
		}
	}
}
