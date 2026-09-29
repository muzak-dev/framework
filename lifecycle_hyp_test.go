package muzak

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hangingStop is a component whose Stop returns only when its context ends,
// which is how a Stop stuck on an unreachable broker behaves.
type hangingStop struct {
	name    string
	stopped atomic.Int32
}

func (h *hangingStop) Name() string                { return h.name }
func (h *hangingStop) Start(context.Context) error { return nil }
func (h *hangingStop) Stop(ctx context.Context) error {
	h.stopped.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func TestFailedStartStopsComponentsWithinTheShutdownTimeout(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ShutdownTimeout = 200 * time.Millisecond

	hung := &hangingStop{name: "broker"}
	failing := &recorder{name: "database", startErr: errors.New("refused")}
	app := New(opts, WithLifecycle(hung, failing))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	done := make(chan error, 1)
	go func() { done <- app.StartLifecycle(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StartLifecycle = %v, want the start failure joined with the stop deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Stop that never returns held a failed start-up forever")
	}
	if hung.stopped.Load() != 1 {
		t.Errorf("the component that started was stopped %d times, want 1", hung.stopped.Load())
	}
}

func TestListenFailureStopsComponentsWithinTheShutdownTimeout(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	opts := quietOptions()
	opts.Addr = taken.Addr().String()
	opts.ShutdownTimeout = 200 * time.Millisecond
	hung := &hangingStop{name: "broker"}
	app := New(opts, WithLifecycle(hung))
	app.Get("/x", okHandler)

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run on a taken address returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Stop that never returns held Run after its listen failed")
	}
	if hung.stopped.Load() != 1 {
		t.Errorf("stopped %d times, want 1", hung.stopped.Load())
	}
}

// barrierStop returns from Stop only once every component has entered it,
// which no sequential shutdown could satisfy.
type barrierStop struct {
	recorder
	entered *sync.WaitGroup
}

func (b *barrierStop) Stop(ctx context.Context) error {
	b.entered.Done()
	done := make(chan struct{})
	go func() { b.entered.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("stops were not concurrent")
	}
}

func TestLifecycleStopsComponentsConcurrently(t *testing.T) {
	t.Parallel()
	var entered sync.WaitGroup
	entered.Add(2)
	a := &barrierStop{recorder: recorder{name: "consumer"}, entered: &entered}
	b := &barrierStop{recorder: recorder{name: "pool"}, entered: &entered}
	app := New(quietOptions(), WithLifecycle(a, b))
	app.Get("/x", okHandler)
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.StopLifecycle(ctx); err != nil {
		t.Fatalf("StopLifecycle = %v; Stop calls are documented to overlap", err)
	}
}

func TestConcurrentStartLifecycleWaitsForTheFirst(t *testing.T) {
	t.Parallel()
	slow := &recorder{name: "database", startDelay: 300 * time.Millisecond}
	app := New(quietOptions(), WithLifecycle(slow))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	first := make(chan error, 1)
	go func() { first <- app.StartLifecycle(context.Background()) }()
	// Let the first call claim the start before the second arrives.
	deadline := time.Now().Add(2 * time.Second)
	for {
		app.lifecycle.mu.Lock()
		claimed := app.lifecycle.attempt != nil
		app.lifecycle.mu.Unlock()
		if claimed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first StartLifecycle never began")
		}
		time.Sleep(time.Millisecond)
	}
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("second StartLifecycle = %v", err)
	}
	if started, _ := slow.counts(); started != 1 {
		t.Fatalf("the second StartLifecycle returned before the component was ready (started=%d)", started)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if started, _ := slow.counts(); started != 1 {
		t.Errorf("component started %d times, want 1", started)
	}
}

// gatedStart blocks in Start, ignoring its context, until released.
type gatedStart struct {
	recorder
	inStart chan struct{}
	release chan struct{}
}

func (g *gatedStart) Start(ctx context.Context) error {
	close(g.inStart)
	<-g.release
	return g.recorder.Start(ctx)
}

func TestStopDuringStartReleasesWhatStarted(t *testing.T) {
	t.Parallel()
	quick := &recorder{name: "cache"}
	gated := &gatedStart{recorder: recorder{name: "database"}, inStart: make(chan struct{}), release: make(chan struct{})}
	app := New(quietOptions(), WithLifecycle(quick, gated))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	started := make(chan error, 1)
	go func() { started <- app.StartLifecycle(context.Background()) }()
	<-gated.inStart
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(gated.release)

	select {
	case err := <-started:
		if err == nil {
			t.Error("StartLifecycle succeeded although StopLifecycle ran while it started")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartLifecycle never returned")
	}
	// Whatever came up after the Stop must not be left running.
	for _, c := range []interface{ counts() (int, int) }{quick, &gated.recorder} {
		s, st := c.counts()
		if s != st {
			t.Errorf("a component was started %d times and stopped %d", s, st)
		}
	}
}
