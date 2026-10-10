package muzak

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestShutdownDeadlineCancelsAStuckTask checks the bound on a shutdown that
// finds a task still running at its deadline: the task's context is cancelled
// with a cause that says why, the tasks still queued are dropped rather than
// started, and the components are stopped once the task has returned.
func TestShutdownDeadlineCancelsAStuckTask(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.Addr = "127.0.0.1:0"
	opts.ShutdownTimeout = 300 * time.Millisecond
	opts.Background = BackgroundOptions{Workers: 1}
	var cause atomic.Pointer[error]
	var queuedRan, startedCancelled atomic.Int32
	var stoppedAfterTask atomic.Bool
	var taskReturned atomic.Bool
	component := NewLifecycle("db", nil, func(context.Context) error {
		stoppedAfterTask.Store(taskReturned.Load())
		return nil
	})
	app := New(opts, WithLifecycle(component))
	app.Get("/task", func(ctx *Context, _ Empty) (rtOut, error) {
		err := ctx.AfterResponse(func(bg context.Context) {
			if bg.Err() != nil {
				startedCancelled.Add(1)
			}
			<-bg.Done()
			c := context.Cause(bg)
			cause.Store(&c)
			taskReturned.Store(true)
		})
		for range 2 {
			err = errors.Join(err, ctx.AfterResponse(func(context.Context) { queuedRan.Add(1) }))
		}
		return rtOut{OK: true}, err
	})
	addr, done := startServer(t, app)
	if status, _, err := fetchOverTheWire(t, "http://"+addr+"/task"); err != nil || status != http.StatusOK {
		t.Fatalf("GET /task = %d, %v", status, err)
	}
	waitFor(t, func() bool { return poolAlive(app) == 1 }, "the stuck task to start")
	began := time.Now()
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if took < opts.ShutdownTimeout || took > opts.ShutdownTimeout+2*time.Second {
		t.Errorf("Shutdown took %s with a %s deadline", took, opts.ShutdownTimeout)
	}
	if c := cause.Load(); c == nil || !errors.Is(*c, errBackgroundDeadline) {
		t.Errorf("the stuck task's context ended with %v, want errBackgroundDeadline", cause.Load())
	}
	if n := queuedRan.Load(); n != 0 {
		t.Errorf("%d queued tasks were started after the deadline", n)
	}
	if !stoppedAfterTask.Load() {
		t.Error("the component was stopped before the cancelled task returned")
	}
	if !strings.Contains(logs.String(), "dropped 2 queued background tasks") {
		t.Errorf("the dropped tasks were not reported:\n%s", logs.String())
	}
	waitPoolIdle(t, app)

	// The next run hands its tasks a context that is alive again, rather
	// than the one the first run's deadline cancelled.
	addr, done = startServerAgain(t, app, addr)
	if status, _, err := fetchOverTheWire(t, "http://"+addr+"/task"); err != nil || status != http.StatusOK {
		t.Fatalf("GET /task on the second run = %d, %v", status, err)
	}
	waitFor(t, func() bool { return poolAlive(app) == 1 }, "the second run's task to start")
	if n := startedCancelled.Load(); n != 0 {
		t.Errorf("%d tasks started with their context already cancelled", n)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitPoolIdle(t, app)
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}

// TestShutdownIsBoundedByATaskThatIgnoresItsContext checks the last line of
// defence: a task that never returns delays the shutdown by its grace period,
// is reported, and is then left behind.
func TestShutdownIsBoundedByATaskThatIgnoresItsContext(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.Addr = "127.0.0.1:0"
	opts.ShutdownTimeout = 200 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	app := taskApp(t, opts, func(context.Context) {
		close(started)
		<-release
	})
	addr, done := startServer(t, app)
	if status, _, err := fetchOverTheWire(t, "http://"+addr+"/task"); err != nil || status != http.StatusOK {
		t.Fatalf("GET /task = %d, %v", status, err)
	}
	// The task has to be running when the shutdown starts. One still queued
	// when the deadline passes is dropped instead, which is reported
	// differently, and a loaded machine did not always start it in time.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the task never started")
	}
	began := time.Now()
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > opts.ShutdownTimeout+2*time.Second {
		t.Errorf("Shutdown took %s", took)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "1 background task still running after the shutdown deadline") {
		t.Errorf("the task left running was not reported:\n%s", logs.String())
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}

func TestAfterResponseIsAcceptedDuringTheDrainDelay(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.DrainDelay = 300 * time.Millisecond
	var ran atomic.Int32
	app := taskApp(t, opts, func(context.Context) { ran.Add(1) })
	addr, done := startServer(t, app)
	shutdown := make(chan error, 1)
	go func() { shutdown <- app.Shutdown(context.Background()) }()
	waitFor(t, app.readiness.draining.Load, "the shutdown to begin")
	status, body, err := fetchOverTheWire(t, "http://"+addr+"/task")
	if err != nil || status != http.StatusOK || !strings.Contains(body, `"queued":true`) {
		t.Fatalf("a task registered during the drain delay: %d %s %v", status, body, err)
	}
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Error("the task registered during the drain delay did not run before Shutdown returned")
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}

func TestShutdownWithNoTasksDoesNotWait(t *testing.T) {
	t.Parallel()
	pool := newBackgroundPool(BackgroundOptions{}.withDefaults(), slog.New(discardHandler{}))
	dropped, running := pool.drain(context.Background(), time.Hour)
	if dropped != 0 || running != 0 {
		t.Errorf("drain of an idle pool = %d, %d", dropped, running)
	}
	if err := pool.reserve(); !errors.Is(err, ErrBackgroundShuttingDown) {
		t.Errorf("reserve after a drain = %v", err)
	}
	pool.reopen()
	if err := pool.reserve(); err != nil {
		t.Errorf("reserve after reopening = %v", err)
	}
}

func TestBackgroundTaskHandedOverAfterTheShutdownGaveUpIsDropped(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	pool := newBackgroundPool(BackgroundOptions{}.withDefaults(), logger)
	// A request still running registered a task, and the shutdown's deadline
	// passed while it ran.
	if err := pool.reserve(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if dropped, running := pool.drain(ctx, time.Millisecond); dropped != 0 || running != 0 {
		t.Errorf("drain = %d, %d", dropped, running)
	}
	var ran atomic.Bool
	pool.settle([]backgroundTask{{run: func(context.Context) { ran.Store(true) }, ctx: context.Background()}}, true)
	time.Sleep(20 * time.Millisecond)
	if ran.Load() {
		t.Error("a task handed over after the shutdown gave up was run")
	}
	if !strings.Contains(logs.String(), "dropped 1 background task registered by a request that outlived the shutdown deadline") {
		t.Errorf("the dropped task was not reported:\n%s", logs.String())
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.reserved != 0 {
		t.Errorf("reserved = %d after the late task was dropped", pool.reserved)
	}
}

func TestBackgroundWorkersStartLazilyAndExitWhenIdle(t *testing.T) {
	// The workers are counted across the process, so those of earlier tests
	// that are still finishing are let go first, or they would be counted as
	// this application's.
	waitNoWorkers(t)
	opts := quietOptions()
	opts.Background = BackgroundOptions{Workers: 4}
	release := make(chan struct{})
	app := New(opts)
	app.Get("/x", okHandler)
	app.Get("/task", func(ctx *Context, _ Empty) (rtOut, error) {
		for range 10 {
			if err := ctx.AfterResponse(func(context.Context) { <-release }); err != nil {
				return rtOut{}, err
			}
		}
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)
	for range 20 {
		do(t, app, http.MethodGet, "/x")
	}
	if n := workerFrames(); n != 0 {
		t.Fatalf("%d workers run before any task was registered", n)
	}
	assertStatus(t, do(t, app, http.MethodGet, "/task"), http.StatusOK)
	waitFor(t, func() bool { return workerFrames() == 4 }, "four workers for ten tasks")
	close(release)
	waitNoWorkers(t)
	app.background.mu.Lock()
	defer app.background.mu.Unlock()
	if app.background.alive != 0 || app.background.reserved != 0 || app.background.queue.len() != 0 {
		t.Errorf("an idle pool holds alive=%d reserved=%d queued=%d",
			app.background.alive, app.background.reserved, app.background.queue.len())
	}
}

func TestNoGoroutineLeaksFromBackgroundTasks(t *testing.T) {
	for range 3 {
		opts := quietOptions()
		opts.Addr = "127.0.0.1:0"
		// Long enough for the tasks to finish on a loaded machine. Past the
		// deadline and its grace period a shutdown returns with tasks still
		// running, by design, and that is not the leak looked for here.
		opts.ShutdownTimeout = 5 * time.Second
		app := taskApp(t, opts, func(ctx context.Context) {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
			}
		})
		addr, done := startServer(t, app)
		for range 20 {
			if status, _, err := fetchOverTheWire(t, "http://"+addr+"/task"); err != nil || status != http.StatusOK {
				t.Fatalf("GET /task = %d, %v", status, err)
			}
		}
		if err := app.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		// Shutdown has returned, so nothing of the pool may still be running.
		// A worker leaves the pool's count as its last act under the lock and
		// then returns, and a loaded machine can catch its goroutine between
		// the two, so the count is what is held to zero at once and the
		// goroutines are given the moment it takes them to return.
		if n := poolAlive(app); n != 0 {
			t.Fatalf("%d background workers survived Shutdown", n)
		}
		waitNoWorkers(t)
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	assertNoGoroutineLeaks(t)
}

func TestBackgroundOptions(t *testing.T) {
	t.Parallel()
	if got := (BackgroundOptions{}).withDefaults(); got.Workers != DefaultBackgroundWorkers || got.Queue != DefaultBackgroundQueue {
		t.Errorf("defaults = %+v", got)
	}
	if got := (BackgroundOptions{Workers: 2, Queue: 3}).withDefaults(); got.Workers != 2 || got.Queue != 3 {
		t.Errorf("explicit values were replaced: %+v", got)
	}
	opts := quietOptions()
	opts.Background = BackgroundOptions{Workers: -1, Queue: -2}
	msg := buildError(t, New(opts))
	for _, want := range []string{"BackgroundOptions.Workers is -1", "BackgroundOptions.Queue is -2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("build error %q does not mention %q", msg, want)
		}
	}
}

func TestTaskRing(t *testing.T) {
	t.Parallel()
	var ring taskRing
	if _, ok := ring.pop(); ok {
		t.Fatal("an empty ring popped a task")
	}
	mark := func(i int) backgroundTask {
		return backgroundTask{ctx: context.WithValue(context.Background(), requestIDContextKey{}, string(rune('a'+i%26)))}
	}
	id := func(task backgroundTask) string { v, _ := RequestIDFromContext(task.ctx); return v }
	// Interleave pushes and pops so the head wraps around before the ring
	// grows, which is when a copy that ignored the head would scramble it.
	next, want := 0, 0
	for round := range 200 {
		for range round%5 + 1 {
			ring.push(mark(next))
			next++
		}
		for range round % 4 {
			task, ok := ring.pop()
			if !ok {
				break
			}
			if id(task) != string(rune('a'+want%26)) {
				t.Fatalf("popped %q, want %q", id(task), string(rune('a'+want%26)))
			}
			want++
		}
	}
	if ring.len() != next-want {
		t.Fatalf("len = %d, want %d", ring.len(), next-want)
	}
	for ring.len() > 0 {
		task, _ := ring.pop()
		if id(task) != string(rune('a'+want%26)) {
			t.Fatalf("popped %q, want %q", id(task), string(rune('a'+want%26)))
		}
		want++
	}
	if ring.buf != nil {
		t.Errorf("an emptied ring kept a buffer of %d", len(ring.buf))
	}
	ring.push(mark(0))
	ring.reset()
	if ring.len() != 0 || ring.buf != nil {
		t.Error("reset kept tasks")
	}
}

// TestBackgroundCostsNothingUnused compares the allocations of a request that
// registers no task in an application with a large pool configured and one
// without: the feature must cost a request that does not use it nothing.
func TestBackgroundCostsNothingUnused(t *testing.T) {
	// Workers are counted across the process; see
	// TestBackgroundWorkersStartLazilyAndExitWhenIdle.
	waitNoWorkers(t)
	measure := func(opts AppOptions) float64 {
		app := New(opts)
		app.Get("/x", okHandler)
		mustBuild(t, app)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		return exactAllocs(200, func() { app.ServeHTTP(httptest.NewRecorder(), req) })
	}
	plain := measure(quietOptions())
	opts := quietOptions()
	opts.Background = BackgroundOptions{Workers: 64, Queue: 10_000}
	if configured := measure(opts); configured != plain {
		t.Errorf("a request allocates %v times with a pool configured and %v without", configured, plain)
	}
	if n := workerFrames(); n != 0 {
		t.Errorf("%d workers were started for requests that registered nothing", n)
	}
}
