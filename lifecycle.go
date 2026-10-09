package muzak

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Lifecycle is implemented by a resource that must be opened before the server
// accepts traffic and closed after it stops.
//
// A database pool, a cache client, a message consumer and a loaded model all
// fit the same shape: something expensive to create, shared by every request,
// and needing an orderly release. Publish the resource with [WithSingleton]
// and, if the value implements Lifecycle, Muzak takes care of the rest:
//
//	app := muzak.New(muzak.AppOptions{Title: "Shop"},
//		muzak.WithSingleton(redisClient), // *Redis implements Lifecycle
//		muzak.WithSingleton(db),          // *DB implements Lifecycle
//	)
//
// Use [NewLifecycle] when a resource does not warrant its own type.
type Lifecycle interface {
	// Name identifies the component in start-up and shutdown logs. Keep it
	// short and lowercase, such as "redis" or "database".
	Name() string
	// Start acquires the resource. It is called once, before the server
	// begins accepting requests, and must return only when the resource is
	// ready to use. The context is cancelled when a sibling component fails,
	// so a long-running dial should honour it and give up. On a successful
	// start it stays live until the components are stopped, so a component
	// may keep it for a background worker; it is cancelled after Stop has run.
	//
	// Under a run method it is also cancelled when the run is told to stop
	// while the components are still starting, by [App.Shutdown] or by the
	// context given to [App.RunContext], and not once they are up: from then
	// on that context ending, or SIGTERM under [App.RunSignals], begins a
	// drain, and the components are in use until it has finished.
	Start(ctx context.Context) error
	// Stop releases the resource. It is called once, after the HTTP server
	// has finished draining in-flight requests and the background tasks of
	// [Context.AfterResponse], and is called even for a start-up that failed
	// part way, for every component that did start.
	//
	// During [App.Shutdown] the context expires with what is left of
	// [ServerOptions.ShutdownTimeout], or one second after Stop is called if
	// that is later, and a Stop that honours it keeps the shutdown within
	// its bound. A start-up that failed, or a run whose socket could not be
	// opened, stops the components that did start with a context that
	// expires after the whole of [ServerOptions.ShutdownTimeout], and never
	// sooner than one second. See App.Shutdown for the one case in which a
	// handler may still be running when Stop is called.
	//
	// Every component is stopped at once, in no particular order, so Stop
	// must not rely on another component still being open: a consumer that
	// flushes into a database pool should own the pool, or be one component
	// with it, rather than expect it to outlive its own Stop.
	Stop(ctx context.Context) error
}

// closureLifecycle adapts a pair of functions to the [Lifecycle] interface.
type closureLifecycle struct {
	name  string
	start func(ctx context.Context) error
	stop  func(ctx context.Context) error
}

func (c closureLifecycle) Name() string { return c.name }

func (c closureLifecycle) Start(ctx context.Context) error {
	if c.start == nil {
		return nil
	}
	return c.start(ctx)
}

func (c closureLifecycle) Stop(ctx context.Context) error {
	if c.stop == nil {
		return nil
	}
	return c.stop(ctx)
}

// NewLifecycle builds a [Lifecycle] from a name and a pair of functions, for
// resources that do not need a type of their own.
//
// Either function may be nil, which makes that half a no-op. This is the
// lightweight counterpart to implementing the interface:
//
//	models := map[string]func(float64) float64{}
//	muzak.NewLifecycle("ml-model",
//		func(ctx context.Context) error {
//			models["answer"] = func(x float64) float64 { return x * 42 }
//			return nil
//		},
//		func(ctx context.Context) error {
//			clear(models)
//			return nil
//		},
//	)
func NewLifecycle(name string, start, stop func(ctx context.Context) error) Lifecycle {
	return closureLifecycle{name: name, start: start, stop: stop}
}

// SingletonOption adjusts how a value published with [WithSingleton] is
// managed.
type SingletonOption interface {
	applySingleton(*singletonConfig)
}

// singletonConfig accumulates the adjustments made to one published value.
type singletonConfig struct {
	lifecycle Lifecycle
}

// singletonOptionFunc adapts a function into a [SingletonOption].
type singletonOptionFunc func(*singletonConfig)

func (f singletonOptionFunc) applySingleton(c *singletonConfig) { f(c) }

// LifecycleFunc attaches start and stop functions to a value published with
// [WithSingleton], so that a resource can take part in the application
// lifecycle without implementing [Lifecycle] itself.
//
// It is the closure form of a lifecycle component, and is what makes a plain
// map or slice usable as managed state:
//
//	models := map[string]func(float64) float64{}
//
//	app.Options(muzak.WithSingleton(models, muzak.LifecycleFunc("ml-model",
//		func(ctx context.Context) error {
//			models["answer_to_everything"] = func(x float64) float64 { return x * 42 }
//			return nil
//		},
//		func(ctx context.Context) error {
//			clear(models)
//			return nil
//		},
//	)))
//
// Because the value itself is published unchanged, the handler still retrieves
// it by type with [From]. Note that the closures must capture a value whose
// contents can be mutated in place, such as a map, a slice header held behind
// a pointer, or a struct pointer; reassigning a captured variable inside Start
// will not change what handlers see.
func LifecycleFunc(name string, start, stop func(ctx context.Context) error) SingletonOption {
	return singletonOptionFunc(func(c *singletonConfig) {
		c.lifecycle = NewLifecycle(name, start, stop)
	})
}

// WithLifecycle registers lifecycle components that are not tied to a
// published value, such as a background worker or a metrics exporter.
//
// Components registered this way are started and stopped exactly like those
// discovered through [WithSingleton].
func WithLifecycle(components ...Lifecycle) SharedOption {
	return sharedOption{
		route: func(c *routeConfig) {
			// A lifecycle component is application-wide, so attaching one to a
			// single route would be misleading. It is accepted and ignored
			// here only so that the option satisfies SharedOption.
			_ = c
		},
		router: func(c *routerConfig) {
			c.lifecycles = append(c.lifecycles, components...)
		},
	}
}

// lifecycleManager starts and stops the registered components and records
// which of them are running, so that a partial start-up can be unwound
// exactly.
type lifecycleManager struct {
	components []Lifecycle
	logger     *slog.Logger

	// stopTimeout bounds the release of the components after a start-up that
	// failed, which no caller's deadline covers: it is [ServerOptions.ShutdownTimeout]
	// when that is set, and zero, no bound, when it was disabled on purpose.
	stopTimeout time.Duration

	mu      sync.Mutex
	started []Lifecycle
	running bool
	// attempt is the start in progress, so that a second Start waits for it
	// rather than reporting success while the components are still coming up.
	attempt *startAttempt
	// generation counts the Stop calls, which is how a Start that a Stop
	// overtook finds out that nothing is left to own what it started.
	generation uint64
	// cancel ends the context the components were started with. It is kept
	// until Stop rather than released when Start returns, because a component
	// that hands its context to a background worker would otherwise have that
	// worker cancelled during boot.
	cancel context.CancelFunc
}

// startAttempt is one pass of Start, shared with any Start that arrives
// while it runs.
type startAttempt struct {
	done chan struct{}
	err  error
}

// errStoppedWhileStarting is what a Start returns when a Stop overtook it.
var errStoppedWhileStarting = errors.New("lifecycle was stopped while its components were starting")

// componentResult carries the outcome of one component's start.
type componentResult struct {
	component Lifecycle
	err       error
	took      time.Duration
}

// Start brings every registered component up in parallel.
//
// Components are independent by construction, so they are started
// concurrently and the total start-up cost is the slowest one rather than the
// sum. If any component fails, the contexts of the others are cancelled
// immediately, every component that did start is stopped, and the failures are
// reported together rather than one per attempt.
func (m *lifecycleManager) Start(ctx context.Context) error {
	m.mu.Lock()
	// A Start that arrives while another is still running waits for it and
	// reports its outcome. Returning at once would tell a test harness that
	// the components are ready while a database pool is still dialling.
	if attempt := m.attempt; attempt != nil {
		m.mu.Unlock()
		select {
		case <-attempt.done:
			return attempt.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.running || len(m.components) == 0 {
		m.mu.Unlock()
		return nil
	}
	// Claiming the running flag up front makes a concurrent Start a no-op
	// rather than a second pass over the same components.
	m.running = true
	attempt := &startAttempt{done: make(chan struct{})}
	m.attempt = attempt
	generation := m.generation
	m.mu.Unlock()

	err := m.start(ctx, generation)

	m.mu.Lock()
	m.attempt = nil
	attempt.err = err
	m.mu.Unlock()
	close(attempt.done)
	return err
}

// start is the body of Start, run by the one caller that claimed it.
// generation is the Stop count when it was claimed.
func (m *lifecycleManager) start(ctx context.Context, generation uint64) error {
	names := make([]string, len(m.components))
	for i, c := range m.components {
		names[i] = c.Name()
	}
	m.logger.Info(fmt.Sprintf("Starting %d lifecycle %s in parallel: %s",
		len(m.components), plural(len(m.components), "component"), strings.Join(names, ", ")))

	// A failing component cancels the others rather than leaving them to run
	// to completion, which keeps a failed start-up as short as the first
	// failure plus the time the rest take to notice.
	startCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()

	began := time.Now()
	results := make([]componentResult, len(m.components))
	var wg sync.WaitGroup
	for i, component := range m.components {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at := time.Now()
			err := startComponent(startCtx, component)
			results[i] = componentResult{component: component, err: err, took: time.Since(at)}
			if err != nil {
				cancel()
			}
		}()
	}
	wg.Wait()

	var failures []error
	var started []Lifecycle
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, fmt.Errorf("lifecycle component %q failed to start: %w", result.component.Name(), result.err))
			continue
		}
		started = append(started, result.component)
		m.logger.Info(fmt.Sprintf("Started %q (%s)", result.component.Name(), roundDuration(result.took)))
	}
	m.mu.Lock()
	if m.generation != generation {
		// A Stop ran while the components were starting. It found nothing
		// recorded to release, so what came up since belongs to nobody but
		// this call, which releases it rather than leaving it running behind
		// a lifecycle that reports itself stopped.
		m.mu.Unlock()
		stopCtx, cancelStop := m.failedStartContext(ctx)
		defer cancelStop()
		joined := errors.Join(append(failures, errStoppedWhileStarting)...)
		return errors.Join(joined, m.stopComponents(stopCtx, started))
	}
	m.started = started
	m.mu.Unlock()

	if len(failures) > 0 {
		joined := errors.Join(failures...)
		m.logger.Error("Lifecycle start-up failed, releasing the components that did start",
			slog.String("error", joined.Error()))
		// Every component that came up is released before returning, so a
		// failed start-up never leaves a pool or a consumer running behind a
		// process that is about to exit.
		stopCtx, cancelStop := m.failedStartContext(ctx)
		defer cancelStop()
		if stopErr := m.Stop(stopCtx); stopErr != nil {
			joined = errors.Join(joined, stopErr)
		}
		return joined
	}

	m.logger.Info(fmt.Sprintf("All lifecycle components ready (%s total)", roundDuration(time.Since(began))))
	return nil
}

// failedStartContext derives the context the components are released with
// after a start-up that failed. It keeps ctx's values but not its
// cancellation, because the components must be released even when the caller
// gave up, and it ends after the configured shutdown timeout so that a Stop
// that never returns cannot hold a failed start-up, and the process that would
// otherwise exit, forever. As during a shutdown, it is never shorter than
// [lifecycleStopFloor], and a shutdown timeout disabled on purpose leaves it
// without a deadline.
func (m *lifecycleManager) failedStartContext(ctx context.Context) (context.Context, context.CancelFunc) {
	stop := context.WithoutCancel(ctx)
	if m.stopTimeout <= 0 {
		return stop, func() {}
	}
	return context.WithTimeout(stop, max(m.stopTimeout, lifecycleStopFloor))
}

// Stop releases every component that was started, in parallel.
//
// The components are stopped concurrently, with no order between them, so a
// component must not need another one to still be open in its Stop: a consumer
// that flushes into a database pool, for one, should be a single component
// that owns both, or should not share the pool with the other.
//
// It is called after the HTTP server has drained its in-flight requests, not
// before: pulling a database connection out from under a request that is still
// running would turn an orderly shutdown into a burst of errors. The one
// exception, a handler that outlives the shutdown deadline by ignoring both
// its context and its closed connection, is described on [App.Shutdown]. Stop
// is idempotent, so calling it twice releases nothing the second time.
func (m *lifecycleManager) Stop(ctx context.Context) error {
	m.mu.Lock()
	components := m.started
	m.started = nil
	m.running = false
	m.generation++
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()
	// The start context ends with the components it was given to, and is
	// released on every path out, including the one with nothing to stop.
	if cancel != nil {
		defer cancel()
	}

	return m.stopComponents(ctx, components)
}

// stopComponents stops components in parallel and reports what failed.
func (m *lifecycleManager) stopComponents(ctx context.Context, components []Lifecycle) error {
	if len(components) == 0 {
		return nil
	}
	m.logger.Info(fmt.Sprintf("Stopping %d lifecycle %s", len(components), plural(len(components), "component")))

	errs := make([]error, len(components))
	var wg sync.WaitGroup
	for i, component := range components {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at := time.Now()

			err := stopComponent(ctx, component)
			if err != nil {
				errs[i] = err
				return
			}
			m.logger.Info(fmt.Sprintf("Stopped %q (%s)", component.Name(), roundDuration(time.Since(at))))
		}()
	}
	wg.Wait()

	joined := errors.Join(errs...)
	if joined != nil {
		m.logger.Error("Some lifecycle components failed to stop", slog.String("error", joined.Error()))
	}
	return joined
}

// recoverComponent turns a panic in a lifecycle hook into an error.
//
// A component's Start and Stop are application code running on the framework's
// goroutines, and Stop runs after the drain, when every request has already
// been answered and there is nothing left to report a crash to. A panic there
// would take the process down at the one moment where doing so achieves
// nothing and looks like a fault in the framework.
//
// A handler that panics already becomes a 500 rather than a dead process; this
// is the same bargain for the same reason.
func recoverComponent(name, phase string, err *error) {
	recovered := recover()
	if recovered == nil {
		return
	}
	*err = fmt.Errorf("lifecycle component %q panicked while it %s: %v\n%s",
		name, phase, recovered, debug.Stack())
}

// startComponent starts one component, converting a panic into an error so
// that a component built wrong fails the start-up rather than the process.
// Every component that did come up is released either way.
func startComponent(ctx context.Context, component Lifecycle) (err error) {
	defer recoverComponent(component.Name(), "started", &err)
	return component.Start(ctx)
}

// stopComponent stops one component, converting a panic into an error so that
// one misbehaving component cannot take the whole shutdown with it.
func stopComponent(ctx context.Context, component Lifecycle) (err error) {
	defer recoverComponent(component.Name(), "stopped", &err)

	if stopErr := component.Stop(ctx); stopErr != nil {
		return fmt.Errorf("lifecycle component %q failed to stop: %w", component.Name(), stopErr)
	}
	return nil
}

// roundDuration trims a duration to a precision worth reading in a log line.
func roundDuration(d time.Duration) time.Duration {
	switch {
	case d < time.Millisecond:
		return d.Round(time.Microsecond)
	case d < time.Second:
		return d.Round(time.Millisecond)
	default:
		return d.Round(10 * time.Millisecond)
	}
}

// StartLifecycle brings up every registered lifecycle component.
//
// The readiness endpoint of [HealthOptions] answers 503 until it has
// succeeded, and again once [App.StopLifecycle] is called, so an application
// served by hand that wants to report itself ready calls it too.
//
// The run methods call it automatically, between building the application and
// opening the listening socket, so a program that uses [App.Run] never needs
// it. Call it directly when driving an application by hand, as a test does
// when it serves the app through httptest, and pair it with
// [App.StopLifecycle].
//
// Starting twice is a no-op, so it is safe to call defensively. A call made
// while another is still starting the components waits for it and reports the
// same outcome, so a return always means the components are ready. A
// [App.StopLifecycle] that overtakes a start makes it release what it had
// started and return an error.
func (a *App) StartLifecycle(ctx context.Context) error {
	if err := a.Build(); err != nil {
		return err
	}
	if err := a.lifecycle.Start(ctx); err != nil {
		return err
	}
	// Readiness waits for this, whether or not there was a component to
	// start; see HealthOptions.
	a.readiness.started.Store(true)
	return nil
}

// StopLifecycle releases every lifecycle component that was started.
//
// [App.Shutdown] calls it after the HTTP server has drained, which is the
// order that matters: components stay usable for as long as a request might
// still be using them. Calling it on an application whose components were
// never started returns nil.
func (a *App) StopLifecycle(ctx context.Context) error {
	a.readiness.started.Store(false)
	return a.lifecycle.Stop(ctx)
}
