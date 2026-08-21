package badele

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// and, if the value implements Lifecycle, Badele takes care of the rest:
//
//	app := badele.New(badele.AppOptions{Title: "Shop"},
//		badele.WithSingleton(redisClient), // *Redis implements Lifecycle
//		badele.WithSingleton(db),          // *DB implements Lifecycle
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
	// so a long-running dial should honour it and give up.
	Start(ctx context.Context) error
	// Stop releases the resource. It is called once, after the HTTP server
	// has finished draining in-flight requests, and is called even for a
	// start-up that failed part way, for every component that did start.
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
//	badele.NewLifecycle("ml-model",
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
//	app.Options(badele.WithSingleton(models, badele.LifecycleFunc("ml-model",
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

	mu      sync.Mutex
	started []Lifecycle
	running bool
}

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
	if m.running || len(m.components) == 0 {
		m.mu.Unlock()
		return nil
	}
	// Claiming the running flag up front makes a concurrent Start a no-op
	// rather than a second pass over the same components.
	m.running = true
	m.mu.Unlock()
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
	defer cancel()

	began := time.Now()
	results := make([]componentResult, len(m.components))
	var wg sync.WaitGroup
	for i, component := range m.components {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at := time.Now()
			err := component.Start(startCtx)
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
	m.started = started
	m.mu.Unlock()

	if len(failures) > 0 {
		joined := errors.Join(failures...)
		m.logger.Error("Lifecycle start-up failed, releasing the components that did start",
			slog.String("error", joined.Error()))
		// Every component that came up is released before returning, so a
		// failed start-up never leaves a pool or a consumer running behind a
		// process that is about to exit.
		if stopErr := m.Stop(context.WithoutCancel(ctx)); stopErr != nil {
			joined = errors.Join(joined, stopErr)
		}
		return joined
	}

	m.logger.Info(fmt.Sprintf("All lifecycle components ready (%s total)", roundDuration(time.Since(began))))
	return nil
}

// Stop releases every component that was started, in parallel.
//
// It is called after the HTTP server has drained its in-flight requests, never
// before: pulling a database connection out from under a request that is still
// running would turn an orderly shutdown into a burst of errors. Stop is
// idempotent, so calling it twice releases nothing the second time.
func (m *lifecycleManager) Stop(ctx context.Context) error {
	m.mu.Lock()
	components := m.started
	m.started = nil
	m.running = false
	m.mu.Unlock()

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
			if err := component.Stop(ctx); err != nil {
				errs[i] = fmt.Errorf("lifecycle component %q failed to stop: %w", component.Name(), err)
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
// The run methods call it automatically, between building the application and
// opening the listening socket, so a program that uses [App.Run] never needs
// it. Call it directly when driving an application by hand, as a test does
// when it serves the app through httptest, and pair it with
// [App.StopLifecycle].
//
// Starting twice is a no-op, so it is safe to call defensively.
func (a *App) StartLifecycle(ctx context.Context) error {
	if err := a.Build(); err != nil {
		return err
	}
	return a.lifecycle.Start(ctx)
}

// StopLifecycle releases every lifecycle component that was started.
//
// [App.Shutdown] calls it after the HTTP server has drained, which is the
// order that matters: components stay usable for as long as a request might
// still be using them. Calling it on an application whose components were
// never started returns nil.
func (a *App) StopLifecycle(ctx context.Context) error {
	return a.lifecycle.Stop(ctx)
}
