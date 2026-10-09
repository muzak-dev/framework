package muzak

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Defaults applied when [BackgroundOptions] leaves a field unset. Both are
// small on purpose: a task is work a response did not wait for, such as an
// email or an audit row, and a pool sized for a burst of those is one a
// shutdown can still drain within [DefaultShutdownTimeout].
const (
	// DefaultBackgroundWorkers is how many background tasks run at once.
	DefaultBackgroundWorkers = 4
	// DefaultBackgroundQueue is how many background tasks may wait for a
	// worker, counting those whose request has not finished yet.
	DefaultBackgroundQueue = 64
)

// BackgroundOptions sizes the pool that runs the tasks handlers register with
// [Context.AfterResponse].
//
// The zero value is usable and starts nothing: workers are started only when
// a task is queued, and each one exits as soon as the queue is empty, so an
// application that never registers a task never runs a goroutine for it.
//
// The two bounds are what keep a burst of requests from turning into an
// unbounded backlog. A registration past Queue is refused with
// [ErrBackgroundQueueFull] rather than blocking the handler or starting a
// goroutine of its own; the handler then decides whether to do the work
// before it returns, to drop it, or to fail the request. Size the queue so
// that Workers can work through it within [ServerOptions.ShutdownTimeout],
// because a shutdown waits for the queue and then cancels what is left.
type BackgroundOptions struct {
	// Workers is how many tasks run at once, defaulting to
	// [DefaultBackgroundWorkers]. Raise it when tasks spend their time
	// waiting on the network rather than on the CPU. A negative value is a
	// build error.
	Workers int
	// Queue is how many registered tasks may wait for a worker, defaulting to
	// [DefaultBackgroundQueue]. A task counts against it from the moment it
	// is registered, while its request is still being served, so that a
	// request that succeeds never finds the queue full at the moment its
	// tasks are handed over. A negative value is a build error.
	Queue int
}

// withDefaults fills in the unset bounds.
func (o BackgroundOptions) withDefaults() BackgroundOptions {
	if o.Workers == 0 {
		o.Workers = DefaultBackgroundWorkers
	}
	if o.Queue == 0 {
		o.Queue = DefaultBackgroundQueue
	}
	return o
}

// validate reports a bound that cannot be honoured. There is deliberately no
// value meaning "unbounded": a queue without a cap is a backlog an attacker
// can grow with requests.
func (o BackgroundOptions) validate() error {
	var errs []error
	if o.Workers < 0 {
		errs = append(errs, fmt.Errorf("muzak: BackgroundOptions.Workers is %d; it must be positive, or zero for the default of %d",
			o.Workers, DefaultBackgroundWorkers))
	}
	if o.Queue < 0 {
		errs = append(errs, fmt.Errorf("muzak: BackgroundOptions.Queue is %d; it must be positive, or zero for the default of %d",
			o.Queue, DefaultBackgroundQueue))
	}
	return errors.Join(errs...)
}

// Errors [Context.AfterResponse] returns for a task it refuses. Either way the
// task will not run, and the handler decides what to do instead.
var (
	// ErrBackgroundQueueFull reports a task refused because as many tasks as
	// [BackgroundOptions.Queue] allows are already waiting. It is a sign the
	// workers are not keeping up, and is returned at once rather than making
	// the request wait for room.
	ErrBackgroundQueueFull = errors.New("muzak: the background task queue is full; " +
		"do the work before the handler returns, or raise BackgroundOptions.Queue or BackgroundOptions.Workers")

	// ErrBackgroundShuttingDown reports a task refused because the server has
	// stopped accepting requests and is draining. The tasks queued before
	// then still run; one registered now could not be promised the time to.
	ErrBackgroundShuttingDown = errors.New("muzak: the server is shutting down and accepts no new background tasks")
)

// Mistakes [Context.AfterResponse] reports about where it was called from.
var (
	errAfterResponseNilTask = errors.New("muzak: AfterResponse was given a nil task")
	errAfterResponseNoRoute = errors.New("muzak: AfterResponse can only be called while a route is being served, " +
		"from its handler, a guard or a provider; this Context is serving no route, or its request has already ended")
	errAfterResponseStream = errors.New("muzak: AfterResponse is not available on an event stream or WebSocket route, " +
		"whose response ends only when the handler returns; do the work before returning instead")
)

// errBackgroundDeadline is the cause a task's context is cancelled with when
// the shutdown deadline passes before the task has returned.
var errBackgroundDeadline = errors.New("muzak: the shutdown deadline passed before this background task finished")

// backgroundTask is one registered task and the request context it keeps the
// values of. It holds nothing of the pooled [Context].
type backgroundTask struct {
	run func(ctx context.Context)
	ctx context.Context
}

// AfterResponse registers a task to run once the response has been written,
// on the application's bounded pool of background workers.
//
// It is for work the client should not wait for: a welcome email, an audit
// record, a cache to warm. The task runs only if the handler returns a nil
// error without panicking; a request that fails, whether its handler returned
// an error, a guard or the binder refused it, or something panicked, drops
// every task it registered, and so does a value that panics while its
// response is encoded. Otherwise, once the handler has returned nil, the
// tasks run whatever happens to the response, a client that left before
// reading it and a value that failed to encode with an error included,
// because the handler's own work is done by then. They are handed to the
// workers after the response has been passed to net/http, before middleware
// installed with [App.Use] has unwound, so a task may begin before the last
// byte has reached the client; the response never waits for one. Tasks from
// one request run concurrently, in no particular order.
//
// The task receives a context that keeps the request's values, so
// [RequestIDFromContext], [LocaleFromContext] and anything middleware stored
// still answer, but not its cancellation or its deadline: the request ending
// does not end the task. It is cancelled when the shutdown deadline is
// reached, and a task should honour it. The task never receives the
// [Context], which is pooled and belongs to another request by the time the
// task runs; copy out what it needs before registering it. [Context.Logger]
// is one such value, safe to keep:
//
//	log := ctx.Logger()
//	user := in.Email
//	if err := ctx.AfterResponse(func(bg context.Context) {
//		if err := mailer.SendWelcome(bg, user); err != nil {
//			log.ErrorContext(bg, "welcome email failed", "error", err)
//		}
//	}); err != nil {
//		log.Warn("welcome email not queued", "error", err)
//	}
//
// A panic in a task is recovered and logged with the request identifier, and
// the worker carries on.
//
// AfterResponse returns [ErrBackgroundQueueFull] when the queue is full and
// [ErrBackgroundShuttingDown] once the server has stopped accepting requests,
// and never blocks or starts a goroutine of its own. A shutdown runs the tasks
// already registered: it stops accepting requests, waits for those in flight,
// then waits for the queued and running tasks, and stops the lifecycle
// components only afterwards, so a task may still use them. Its wait is bounded
// by [ServerOptions.ShutdownTimeout]; at the deadline the tasks still waiting
// are dropped and the running ones have their context cancelled. An
// application served by hand through [App.ServeHTTP] runs its tasks the same
// way, but nothing drains them, since no shutdown is involved.
//
// It is refused on an event stream or WebSocket route, whose response ends
// only when its handler returns, so the handler can do the work itself at
// that point, and outside a route, such as in a guard of the documentation.
// See [BackgroundOptions] for sizing the pool.
func (c *Context) AfterResponse(task func(ctx context.Context)) error {
	switch {
	case task == nil:
		return errAfterResponseNilTask
	case c.app == nil || c.route == nil:
		return errAfterResponseNoRoute
	case c.endsItsOwnResponse():
		return errAfterResponseStream
	}
	if err := c.app.background.reserve(); err != nil {
		return err
	}
	c.tasks = append(c.tasks, backgroundTask{run: task, ctx: c.r.Context()})
	return nil
}

// settleTasks hands a finished request's tasks to the workers when its handler
// succeeded, and gives their places in the queue back when it did not. It is
// called as the Context is released, which is after the response was written
// on every way out, a panic included.
func (a *App) settleTasks(c *Context) {
	if len(c.tasks) == 0 {
		return
	}
	a.background.settle(c.tasks, c.handled)
	// Dropped rather than kept for reuse, so that a pooled Context never holds
	// a closure, or the request context it captured, past its request.
	clear(c.tasks)
	c.tasks = nil
}

// backgroundPool runs registered tasks on at most workers goroutines.
//
// Every bound is enforced under one mutex, which a request takes once to
// register a task and once to hand its tasks over, and which a worker takes
// once per task: O(1) each time, whatever the load. The queue holds at most
// capacity tasks, so memory is bounded by capacity plus workers tasks.
//
// No goroutine exists while the queue is empty. A worker is started when a
// task is handed over and fewer than workers are running, and exits when it
// finds the queue empty, which is what lets a shutdown know it is finished
// once the last worker has gone, and what keeps an idle application from
// holding goroutines it does not need.
type backgroundPool struct {
	logger   *slog.Logger
	workers  int
	capacity int

	mu sync.Mutex
	// reserved counts the tasks registered and not yet started: those whose
	// request is still being served, and those queued for a worker.
	reserved int
	queue    taskRing
	// alive counts the worker goroutines, each running a task or about to
	// look for one.
	alive int
	// closed refuses new registrations once a shutdown has begun.
	closed bool
	// stopped records a shutdown that gave up waiting: nothing more is run,
	// and a task handed over late is dropped rather than started against
	// lifecycle components that are being stopped.
	stopped bool
	// quiet is closed when the pool has nothing left to wait for, while a
	// shutdown is waiting on it.
	quiet chan struct{}
	// lifetime is what every task's context is cancelled through, with
	// errBackgroundDeadline, when a shutdown's deadline passes.
	lifetime context.Context
	cancel   context.CancelCauseFunc
}

// newBackgroundPool prepares a pool for opts, which have had their defaults
// applied. It starts nothing.
func newBackgroundPool(opts BackgroundOptions, logger *slog.Logger) *backgroundPool {
	p := &backgroundPool{logger: logger, workers: opts.Workers, capacity: opts.Queue}
	p.lifetime, p.cancel = context.WithCancelCause(context.Background())
	return p
}

// reserve claims a place in the queue for a task being registered.
func (p *backgroundPool) reserve() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrBackgroundShuttingDown
	}
	if p.reserved >= p.capacity {
		return ErrBackgroundQueueFull
	}
	p.reserved++
	return nil
}

// settle queues one request's tasks when run is true and releases their
// places otherwise, starting as many workers as the new tasks can use.
func (p *backgroundPool) settle(tasks []backgroundTask, run bool) {
	p.mu.Lock()
	if !run || p.stopped {
		p.reserved -= len(tasks)
		p.signalIfQuiet()
		stopped := p.stopped
		p.mu.Unlock()
		if run && stopped {
			// Only a handler still running past the shutdown deadline and its
			// grace period gets here, and its tasks were accepted, so losing
			// them is worth a line.
			p.logger.Warn(fmt.Sprintf("muzak: dropped %d background %s registered by a request that outlived the shutdown deadline",
				len(tasks), plural(len(tasks), "task")))
		}
		return
	}
	for _, task := range tasks {
		p.queue.push(task)
	}
	start := min(p.workers-p.alive, p.queue.len())
	p.alive += start
	p.mu.Unlock()
	for range start {
		go p.work()
	}
}

// work runs queued tasks until the queue is empty, and then exits.
func (p *backgroundPool) work() {
	for {
		p.mu.Lock()
		task, ok := p.queue.pop()
		if !ok {
			p.alive--
			p.signalIfQuiet()
			p.mu.Unlock()
			return
		}
		p.reserved--
		lifetime := p.lifetime
		p.mu.Unlock()
		p.run(lifetime, task)
	}
}

// run calls one task with a context that keeps its request's values and is
// cancelled with lifetime, and recovers a panic so that one task cannot take
// the worker, or the process, with it.
func (p *backgroundPool) run(lifetime context.Context, task backgroundTask) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(task.ctx))
	stop := context.AfterFunc(lifetime, func() { cancel(context.Cause(lifetime)) })
	defer func() {
		stop()
		cancel(nil)
		if recovered := recover(); recovered != nil {
			id, _ := RequestIDFromContext(task.ctx)
			p.logger.ErrorContext(ctx, "muzak: recovered from a panic in a background task",
				slog.String("panic", panicValue(recovered)),
				slog.String(RequestIDKey, id),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	task.run(ctx)
}

// isQuiet reports whether a shutdown has nothing left to wait for: no worker
// is running and, unless the shutdown has already given up on them, no task is
// waiting to be handed over or started. It is called with mu held.
func (p *backgroundPool) isQuiet() bool {
	return p.alive == 0 && (p.stopped || p.reserved == 0)
}

// signalIfQuiet wakes a waiting shutdown once the pool is quiet. It is called
// with mu held.
func (p *backgroundPool) signalIfQuiet() {
	if p.quiet != nil && p.isQuiet() {
		close(p.quiet)
		p.quiet = nil
	}
}

// waitQuiet returns a channel closed once the pool is quiet, or nil when it
// already is. It is called with mu held.
func (p *backgroundPool) waitQuiet() chan struct{} {
	if p.isQuiet() {
		return nil
	}
	if p.quiet == nil {
		p.quiet = make(chan struct{})
	}
	return p.quiet
}

// close refuses every registration from now on.
func (p *backgroundPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

// reopen lets a new run register tasks again, with a fresh lifetime if the
// previous shutdown had to cancel the old one.
func (p *backgroundPool) reopen() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = false
	if p.stopped {
		p.stopped = false
		p.lifetime, p.cancel = context.WithCancelCause(context.Background())
	}
}

// drain waits until every task registered before the pool was closed has run,
// or ctx is done. At that point the tasks still queued are dropped, the
// running ones have their context cancelled, and they are given grace to
// return. It reports how many were dropped and how many were still running
// when it stopped waiting.
func (p *backgroundPool) drain(ctx context.Context, grace time.Duration) (dropped, running int) {
	p.mu.Lock()
	p.closed = true
	quiet := p.waitQuiet()
	p.mu.Unlock()
	if quiet == nil {
		return 0, 0
	}
	select {
	case <-quiet:
		return 0, 0
	case <-ctx.Done():
	}

	p.mu.Lock()
	p.stopped = true
	dropped = p.queue.len()
	p.queue.reset()
	p.reserved -= dropped
	cancel := p.cancel
	quiet = p.waitQuiet()
	p.mu.Unlock()
	cancel(errBackgroundDeadline)
	if quiet != nil {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-quiet:
		case <-timer.C:
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return dropped, p.alive
}

// drainBackground is the part of a shutdown that waits for background tasks,
// between the drain of in-flight requests and the stop of the lifecycle
// components, and reports what it had to give up on.
func (a *App) drainBackground(ctx context.Context, log *slog.Logger) {
	dropped, running := a.background.drain(ctx, shutdownHandlerGrace)
	if dropped > 0 {
		log.Warn(fmt.Sprintf("Shutdown deadline reached; dropped %d queued background %s that had not started",
			dropped, plural(dropped, "task")))
	}
	if running > 0 {
		log.Warn(fmt.Sprintf("%d background %s still running after the shutdown deadline and its grace period; stopping lifecycle components anyway",
			running, plural(running, "task")))
	}
}

// taskRing is a first-in, first-out queue of tasks in a ring buffer. It grows
// by doubling, so it never holds more than twice the tasks the pool's capacity
// lets it queue, and a large buffer is let go once it empties after a burst,
// so an idle pool keeps none.
type taskRing struct {
	buf  []backgroundTask
	head int
	n    int
}

// ringShrinkAbove is the buffer length past which an emptied ring lets its
// buffer go rather than keeping it for the next burst.
const ringShrinkAbove = 64

func (r *taskRing) len() int { return r.n }

func (r *taskRing) push(task backgroundTask) {
	if r.n == len(r.buf) {
		grown := make([]backgroundTask, max(8, 2*len(r.buf)))
		for i := range r.n {
			grown[i] = r.buf[(r.head+i)%len(r.buf)]
		}
		r.buf, r.head = grown, 0
	}
	r.buf[(r.head+r.n)%len(r.buf)] = task
	r.n++
}

func (r *taskRing) pop() (backgroundTask, bool) {
	if r.n == 0 {
		return backgroundTask{}, false
	}
	task := r.buf[r.head]
	r.buf[r.head] = backgroundTask{}
	r.head = (r.head + 1) % len(r.buf)
	r.n--
	if r.n == 0 && len(r.buf) > ringShrinkAbove {
		r.buf, r.head = nil, 0
	}
	return task, true
}

// reset drops every queued task.
func (r *taskRing) reset() {
	r.buf, r.head, r.n = nil, 0, 0
}
