package muzak

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestShutdownDuringStartupStopsRun is the regression test for a Shutdown
// that arrived while Run was still starting its lifecycle components, as a
// SIGTERM during a slow warm-up does: it returned nil and was forgotten, and
// the server went on to serve.
func TestShutdownDuringStartupStopsRun(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var stopped atomic.Bool
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(NewLifecycle("db", func(context.Context) error {
		close(entered)
		<-release // a warm-up that does not watch its context
		return nil
	}, func(context.Context) error {
		stopped.Store(true)
		return nil
	})))
	app.Get("/x", okHandler)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	<-entered

	returned := make(chan error, 1)
	go func() { returned <- app.Shutdown(context.Background()) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("Shutdown during start-up = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown during start-up waited for a start-up that cannot finish until it returns")
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil after a requested shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run went on serving after a Shutdown requested during start-up")
	}
	if addr := app.Addr(); addr != "" {
		t.Errorf("Addr = %q, want no listener left behind", addr)
	}
	if !stopped.Load() {
		t.Error("the component that started was not stopped")
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("a second Shutdown = %v, want nil", err)
	}
}

// TestShutdownDuringStartupCancelsStart covers a component that honours its
// start context: the shutdown cancels it, so start-up gives up at once, and
// Run reports the requested stop rather than the cancellation it caused.
func TestShutdownDuringStartupCancelsStart(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(NewLifecycle("db", func(ctx context.Context) error {
		close(entered)
		<-ctx.Done() // a dial that gives up when asked to
		return ctx.Err()
	}, nil)))

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	<-entered
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil after a requested shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start context was not cancelled by the shutdown")
	}
}
