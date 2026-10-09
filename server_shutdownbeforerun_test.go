package muzak

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestShutdownBeforeRunStopsTheNextRun is the regression test for a Shutdown
// that came before its run. "go app.Run(); app.Shutdown(ctx)" is the natural
// way to start a server and stop it again, and when the goroutine had not
// yet reached Run, Shutdown found nothing to stop, returned nil and was
// forgotten, and the server went on serving. As net/http's ListenAndServe
// does after Shutdown, the next run now returns at once without serving; the
// run after that serves as usual.
func TestShutdownBeforeRunStopsTheNextRun(t *testing.T) {
	t.Parallel()
	var starts atomic.Int32
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(NewLifecycle("db",
		func(context.Context) error { starts.Add(1); return nil }, nil)))
	app.Get("/x", okHandler)

	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown with no run = %v, want nil", err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after Shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run went on serving after a Shutdown that came before it")
	}
	if got := starts.Load(); got != 0 {
		t.Errorf("components started %d times by a run that was already shut down", got)
	}
	if addr := app.Addr(); addr != "" {
		t.Errorf("Addr = %q, want no socket opened", addr)
	}

	// The Shutdown was for that run alone.
	go func() { done <- app.Run() }()
	waitForAddr(t, app)
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("the run after = %v, want nil", err)
	}
	if got := starts.Load(); got != 1 {
		t.Errorf("components started %d times, want once by the run that served", got)
	}
}

// TestShutdownRightAfterStartingARunIsNeverLost drives the race itself,
// on one application run over and over, so that the Shutdown lands before
// the run is recorded, while it starts and while it serves, and after an
// earlier run has left its runner behind. Every run must return.
func TestShutdownRightAfterStartingARunIsNeverLost(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts)
	app.Get("/x", okHandler)

	for i := range 25 {
		done := make(chan error, 1)
		go func() { done <- app.Run() }()
		if i%5 == 4 {
			// Now and then the run gets as far as serving first.
			waitFor(t, func() bool { return app.serving() }, "the run to serve")
		}
		if err := app.Shutdown(context.Background()); err != nil {
			t.Fatalf("run %d: Shutdown = %v", i, err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run %d: Run = %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: the Shutdown was lost and Run went on serving", i)
		}
	}
	// No Shutdown is left over to stop a run nobody asked to stop.
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	waitFor(t, func() bool { return app.serving() }, "a run after the loop to serve")
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
}

// serving reports whether a run is in progress and has opened its socket.
func (a *App) serving() bool {
	a.server.mu.Lock()
	runner := a.server.running
	a.server.mu.Unlock()
	if runner == nil {
		return false
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.http != nil
}
