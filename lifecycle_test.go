package muzak

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is a Lifecycle that records what happened to it.
type recorder struct {
	name       string
	startErr   error
	stopErr    error
	startDelay time.Duration

	mu      sync.Mutex
	started int
	stopped int
	ctxDone bool
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Start(ctx context.Context) error {
	if r.startDelay > 0 {
		select {
		case <-time.After(r.startDelay):
		case <-ctx.Done():
			r.mu.Lock()
			r.ctxDone = true
			r.mu.Unlock()
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.started++
	r.mu.Unlock()
	return r.startErr
}

func (r *recorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.stopped++
	r.mu.Unlock()
	return r.stopErr
}

func (r *recorder) counts() (started, stopped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, r.stopped
}

func TestLifecycleStartsAndStops(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	redis := &recorder{name: "redis"}
	database := &recorder{name: "database"}

	app := New(opts, WithSingleton(redis), WithSingleton(database))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	for _, component := range []*recorder{redis, database} {
		if started, _ := component.counts(); started != 1 {
			t.Errorf("%s started %d times, want 1", component.name, started)
		}
	}

	recorded := logs.String()
	for _, want := range []string{
		"Starting 2 lifecycle components in parallel: redis, database",
		`Started \"redis\"`,
		`Started \"database\"`,
		"All lifecycle components ready",
	} {
		if !strings.Contains(recorded, want) {
			t.Errorf("the start-up log is missing %q:\n%s", want, recorded)
		}
	}

	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatalf("StopLifecycle = %v", err)
	}
	for _, component := range []*recorder{redis, database} {
		if _, stopped := component.counts(); stopped != 1 {
			t.Errorf("%s stopped %d times, want 1", component.name, stopped)
		}
	}
}

func TestLifecycleStartsInParallel(t *testing.T) {
	t.Parallel()
	const delay = 60 * time.Millisecond
	components := []*recorder{
		{name: "a", startDelay: delay},
		{name: "b", startDelay: delay},
		{name: "c", startDelay: delay},
	}
	app := New(quietOptions(),
		WithLifecycle(components[0], components[1], components[2]))
	mustBuild(t, app)

	began := time.Now()
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	elapsed := time.Since(began)

	// Started sequentially this would take at least three delays. Allow ample
	// slack so the test stays reliable on a loaded machine.
	if elapsed >= 3*delay {
		t.Errorf("start took %v, want roughly one delay of %v (components must start in parallel)", elapsed, delay)
	}
}

// TestLifecycleFailFastReleasesWhatStarted is the property the design exists
// for: a failure must not leave a started component running.
func TestLifecycleFailFastReleasesWhatStarted(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	healthy := &recorder{name: "database"}
	broken := &recorder{name: "redis", startErr: errors.New("connection refused")}
	alsoBroken := &recorder{name: "queue", startErr: errors.New("no broker")}

	app := New(opts, WithLifecycle(healthy, broken, alsoBroken))
	mustBuild(t, app)

	err := app.StartLifecycle(context.Background())
	if err == nil {
		t.Fatal("StartLifecycle succeeded, want an error")
	}

	// Errors are reported together, not one per attempt.
	message := err.Error()
	if !strings.Contains(message, "connection refused") || !strings.Contains(message, "no broker") {
		t.Errorf("the error does not aggregate both failures:\n%s", message)
	}
	if !strings.Contains(message, `"redis"`) || !strings.Contains(message, `"queue"`) {
		t.Errorf("the error does not name the failing components:\n%s", message)
	}

	// The component that did start was released rather than leaked.
	started, stopped := healthy.counts()
	if started != 1 || stopped != 1 {
		t.Errorf("the healthy component started %d and stopped %d times, want 1 and 1", started, stopped)
	}
	if !strings.Contains(logs.String(), "releasing the components that did start") {
		t.Errorf("the unwind was not reported:\n%s", logs.String())
	}
}

// TestLifecycleFailureCancelsSiblings checks that a failure does not leave the
// remaining components dialling until they time out on their own.
func TestLifecycleFailureCancelsSiblings(t *testing.T) {
	t.Parallel()
	slow := &recorder{name: "slow", startDelay: 5 * time.Second}
	broken := &recorder{name: "broken", startErr: errors.New("immediate failure")}

	app := New(quietOptions(), WithLifecycle(slow, broken))
	mustBuild(t, app)

	began := time.Now()
	if err := app.StartLifecycle(context.Background()); err == nil {
		t.Fatal("StartLifecycle succeeded, want an error")
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Errorf("start took %v, want the failure to cancel the slow component promptly", elapsed)
	}

	slow.mu.Lock()
	cancelled := slow.ctxDone
	slow.mu.Unlock()
	if !cancelled {
		t.Error("the slow component's context was not cancelled by its sibling's failure")
	}
}

func TestLifecycleStopErrorsAreReported(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	broken := &recorder{name: "broken", stopErr: errors.New("could not flush")}
	app := New(opts, WithLifecycle(broken))
	mustBuild(t, app)

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	err := app.StopLifecycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not flush") {
		t.Fatalf("StopLifecycle = %v, want the stop failure", err)
	}
	if !strings.Contains(logs.String(), "failed to stop") {
		t.Errorf("the stop failure was not logged:\n%s", logs.String())
	}
}

func TestLifecycleIsIdempotent(t *testing.T) {
	t.Parallel()
	component := &recorder{name: "once"}
	app := New(quietOptions(), WithLifecycle(component))
	mustBuild(t, app)

	for range 3 {
		if err := app.StartLifecycle(context.Background()); err != nil {
			t.Fatalf("StartLifecycle = %v", err)
		}
	}
	if started, _ := component.counts(); started != 1 {
		t.Errorf("started %d times, want 1", started)
	}

	for range 3 {
		if err := app.StopLifecycle(context.Background()); err != nil {
			t.Fatalf("StopLifecycle = %v", err)
		}
	}
	if _, stopped := component.counts(); stopped != 1 {
		t.Errorf("stopped %d times, want 1", stopped)
	}
}

func TestLifecycleWithNoComponents(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler)
	mustBuild(t, app)

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Errorf("StartLifecycle with no components = %v, want nil", err)
	}
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Errorf("StopLifecycle with no components = %v, want nil", err)
	}
}

func TestStartLifecycleReportsBuildFailures(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("bad-path", okHandler)

	if err := app.StartLifecycle(context.Background()); err == nil {
		t.Fatal("StartLifecycle succeeded on an application that cannot build")
	}
}

func TestLifecycleFuncClosures(t *testing.T) {
	t.Parallel()
	models := map[string]func(float64) float64{}

	type predictOut struct {
		Result float64 `json:"result"`
	}

	app := New(quietOptions(), WithSingleton(models, LifecycleFunc("ml-model",
		func(ctx context.Context) error {
			models["answer_to_everything"] = func(x float64) float64 { return x * 42 }
			return nil
		},
		func(ctx context.Context) error {
			clear(models)
			return nil
		},
	)))
	app.Get("/predict", func(ctx *Context, in struct {
		X float64 `query:"x" default:"1"`
	}) (predictOut, error) {
		loaded := From[map[string]func(float64) float64](ctx)
		return predictOut{Result: loaded["answer_to_everything"](in.X)}, nil
	})
	mustBuild(t, app)

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	assertJSON(t, do(t, app, "GET", "/predict?x=2"), `{"result":84}`)

	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatalf("StopLifecycle = %v", err)
	}
	if len(models) != 0 {
		t.Errorf("the model map was not cleared: %v", models)
	}
}

func TestNewLifecycleWithNilFunctions(t *testing.T) {
	t.Parallel()
	component := NewLifecycle("inert", nil, nil)
	if component.Name() != "inert" {
		t.Errorf("Name = %q", component.Name())
	}
	if err := component.Start(context.Background()); err != nil {
		t.Errorf("Start with a nil function = %v, want nil", err)
	}
	if err := component.Stop(context.Background()); err != nil {
		t.Errorf("Stop with a nil function = %v, want nil", err)
	}
}

// TestWithLifecycleAtRouteLevelIsIgnored documents that a lifecycle component
// belongs to the application, not to one route.
func TestWithLifecycleAtRouteLevelIsIgnored(t *testing.T) {
	t.Parallel()
	component := &recorder{name: "route-scoped"}
	app := New(quietOptions())
	app.Get("/x", okHandler, WithLifecycle(component))
	mustBuild(t, app)

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	if started, _ := component.counts(); started != 0 {
		t.Errorf("a route-level lifecycle component was started %d times, want 0", started)
	}
}

func TestRoundDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{1500 * time.Nanosecond, 2 * time.Microsecond},
		{18 * time.Millisecond, 18 * time.Millisecond},
		{1500*time.Millisecond + 3*time.Millisecond, 1500 * time.Millisecond},
		{2 * time.Second, 2 * time.Second},
	}
	for _, tc := range tests {
		if got := roundDuration(tc.in); got != tc.want {
			t.Errorf("roundDuration(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestLifecycleComponentsAreUsableFromHandlers ties the whole thing together:
// a component publishes a value that a request then reads.
func TestLifecycleComponentsAreUsableFromHandlers(t *testing.T) {
	t.Parallel()
	type pool struct{ ready *atomic.Bool }
	type out struct {
		Ready bool `json:"ready"`
	}

	ready := &atomic.Bool{}
	value := pool{ready: ready}

	app := New(quietOptions(), WithSingleton(value, LifecycleFunc("pool",
		func(ctx context.Context) error { ready.Store(true); return nil },
		func(ctx context.Context) error { ready.Store(false); return nil },
	)))
	app.Get("/ready", func(ctx *Context, _ Empty) (out, error) {
		return out{Ready: From[pool](ctx).ready.Load()}, nil
	})
	mustBuild(t, app)

	// Before start-up the component has not run.
	assertJSON(t, do(t, app, "GET", "/ready"), `{"ready":false}`)

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	assertJSON(t, do(t, app, "GET", "/ready"), `{"ready":true}`)

	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatalf("StopLifecycle = %v", err)
	}
	assertJSON(t, do(t, app, "GET", "/ready"), `{"ready":false}`)
}

// TestLifecycleStartIsRaceFree runs concurrent starts and stops, which is worth
// running under -race.
func TestLifecycleStartIsRaceFree(t *testing.T) {
	t.Parallel()
	component := &recorder{name: "contended"}
	app := New(quietOptions(), WithLifecycle(component))
	mustBuild(t, app)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := app.StartLifecycle(context.Background()); err != nil {
				t.Errorf("StartLifecycle = %v", err)
			}
		}()
	}
	wg.Wait()

	if started, _ := component.counts(); started != 1 {
		t.Errorf("concurrent starts ran the component %d times, want 1", started)
	}
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Errorf("StopLifecycle = %v", err)
	}
}

// TestLifecycleStartsBeforeTrafficThroughRun is covered by the server tests;
// this one pins the ordering guarantee that Stop runs only after the HTTP
// server has drained.
func TestShutdownStopsComponentsAfterDraining(t *testing.T) {
	t.Parallel()
	var order []string
	var mu sync.Mutex
	note := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, event)
	}

	release := make(chan struct{})
	component := NewLifecycle("db",
		func(ctx context.Context) error { note("component-start"); return nil },
		func(ctx context.Context) error { note("component-stop"); return nil },
	)

	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(component))
	app.Get("/slow", func(ctx *Context, _ Empty) (rtOut, error) {
		note("request-start")
		<-release
		note("request-end")
		return rtOut{OK: true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()

	addr := waitForAddr(t, app)
	responses := make(chan int, 1)
	go func() {
		res, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			responses <- 0
			return
		}
		defer res.Body.Close()
		responses <- res.StatusCode
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) >= 2 && order[1] == "request-start"
	}, "the request to reach the handler")

	// Ask the server to stop while the request is still in flight.
	cancel()
	close(release)

	if status := <-responses; status != http.StatusOK {
		t.Errorf("the in-flight request finished with %d, want 200", status)
	}
	if err := <-done; err != nil {
		t.Errorf("RunContext = %v", err)
	}

	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	want := "component-start,request-start,request-end,component-stop"
	if got != want {
		t.Errorf("ordering = %q, want %q (components must outlive in-flight requests)", got, want)
	}
}
