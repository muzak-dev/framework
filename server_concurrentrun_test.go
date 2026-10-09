package muzak

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestASecondConcurrentRunIsRefused is the regression test for one App run
// twice at once. The second run failed to bind the address the first was
// serving on and, as any run whose socket could not be opened does, stopped
// the lifecycle components: the ones the first run was still serving with. It
// had also replaced the runner the App kept, so cancelling the first run's
// context shut down a runner that never served and the first run never
// returned. A run is now refused outright while another is in progress, and
// touches nothing the first one uses.
func TestASecondConcurrentRunIsRefused(t *testing.T) {
	t.Parallel()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	var starts, stops atomic.Int32
	opts := quietOptions()
	opts.Addr = addr
	app := New(opts, WithLifecycle(NewLifecycle("db",
		func(context.Context) error { starts.Add(1); return nil },
		func(context.Context) error { stops.Add(1); return nil },
	)))
	app.Get("/x", okHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- app.RunContext(ctx) }()
	waitForAddr(t, app)

	for _, run := range []func() error{app.Run, func() error { return app.RunContext(context.Background()) }} {
		err := run()
		if err == nil || !strings.HasPrefix(err.Error(), "muzak: ") || !strings.Contains(err.Error(), "already running") {
			t.Errorf("a second run = %v, want it refused because the first is running", err)
		}
	}
	if got := stops.Load(); got != 0 {
		t.Errorf("the refused runs stopped the components the first run is serving with (%d stops)", got)
	}
	if got := starts.Load(); got != 1 {
		t.Errorf("components started %d times, want once", got)
	}
	if got := app.Addr(); got != addr {
		t.Errorf("Addr = %q, want the first run's %q", got, addr)
	}

	cancel()
	select {
	case err := <-first:
		if err != nil {
			t.Errorf("the first run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first run did not return once its context was cancelled")
	}
	if got := stops.Load(); got != 1 {
		t.Errorf("components stopped %d times, want once, by the first run", got)
	}
}

// TestARunIsRefusedWhileAnotherIsStarting covers the same refusal before the
// first run has opened its socket, while its components are still starting,
// which is when a second run used to get furthest before failing.
func TestARunIsRefusedWhileAnotherIsStarting(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(NewLifecycle("db", func(context.Context) error {
		close(entered)
		<-release
		return nil
	}, nil)))

	first := make(chan error, 1)
	go func() { first <- app.Run() }()
	<-entered
	if err := app.Run(); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a run during another's start-up = %v, want it refused", err)
	}
	close(release)
	waitForAddr(t, app)
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	select {
	case err := <-first:
		if err != nil {
			t.Errorf("the first run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first run did not return after Shutdown")
	}
}
