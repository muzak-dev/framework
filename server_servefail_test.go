package muzak

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunStopsLifecycleComponentsWhenServingFails is the regression test for a
// serve failure that skipped the shutdown: a certificate that cannot be loaded
// fails ServeTLS as soon as it is called, after the components have started,
// and Run returned the error with every one of them still running.
func TestRunStopsLifecycleComponentsWhenServingFails(t *testing.T) {
	t.Parallel()
	var started, stopped atomic.Int32
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.CertFile, opts.KeyFile = "/nonexistent/cert.pem", "/nonexistent/key.pem"
	app := New(opts, WithLifecycle(NewLifecycle("db",
		func(context.Context) error { started.Add(1); return nil },
		func(context.Context) error { stopped.Add(1); return nil },
	)))
	app.Get("/x", okHandler)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run = nil, want the error that stopped the server")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the server failed to serve")
	}
	if got := started.Load(); got != 1 {
		t.Errorf("components started %d times, want 1", got)
	}
	if got := stopped.Load(); got != 1 {
		t.Errorf("components stopped %d times, want 1: a failed serve left them running", got)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown after a failed serve = %v, want nil", err)
	}
	if got := stopped.Load(); got != 1 {
		t.Errorf("a later Shutdown stopped the components again: %d stops", got)
	}
}
