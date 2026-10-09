package muzak

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// workerFrames counts the goroutines running a background worker, which is
// how a test tells that the pool started none, or that none survived.
func workerFrames() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "(*backgroundPool).work(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitNoWorkers waits until no background worker goroutine is left in the
// process, which only a test that does not run in parallel can ask.
func waitNoWorkers(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool { return workerFrames() == 0 }, "every background worker to exit")
}

// poolAlive reports how many workers an application's pool is running.
func poolAlive(app *App) int {
	app.background.mu.Lock()
	defer app.background.mu.Unlock()
	return app.background.alive
}

// waitPoolIdle waits until an application's pool has no worker and nothing
// queued, which a test running in parallel with others can ask.
func waitPoolIdle(t *testing.T, app *App) {
	t.Helper()
	waitFor(t, func() bool {
		app.background.mu.Lock()
		defer app.background.mu.Unlock()
		return app.background.alive == 0 && app.background.reserved == 0
	}, "the application's background workers to exit")
}

// startServerAgain runs app once more and waits until it listens somewhere
// other than previous, which is where Addr reports until the new socket opens.
func startServerAgain(t *testing.T, app *App, previous string) (string, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	var addr string
	waitFor(t, func() bool {
		addr = app.Addr()
		return addr != "" && addr != previous
	}, "the second run to listen")
	return addr, done
}

// taskApp builds an application whose GET /task registers the task run, and
// reports the error AfterResponse returned in the response.
func taskApp(t *testing.T, opts AppOptions, run func(ctx context.Context), routeOpts ...RouteOption) *App {
	t.Helper()
	app := New(opts)
	app.Get("/task", func(ctx *Context, _ Empty) (taskOut, error) {
		if err := ctx.AfterResponse(run); err != nil {
			return taskOut{Refused: err.Error()}, nil
		}
		return taskOut{Queued: true}, nil
	}, routeOpts...)
	return mustBuild(t, app)
}

type taskOut struct {
	Queued  bool   `json:"queued"`
	Refused string `json:"refused,omitzero"`
}

func TestAfterResponseRunsOnceTheResponseIsWritten(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ran := make(chan struct{})
	app := taskApp(t, quietOptions(), func(context.Context) {
		<-release
		close(ran)
	})
	// The response is complete while the task is still blocked, which is the
	// whole point: the client never waits for it.
	rec := do(t, app, http.MethodGet, "/task")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"queued":true}`)
	close(release)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the task never ran")
	}
	waitPoolIdle(t, app)
}

func TestAfterResponseKeepsTheRequestsValuesButNotItsCancellation(t *testing.T) {
	t.Parallel()
	type seen struct {
		id, locale  string
		err         error
		hasDeadline bool
		custom      any
	}
	results := make(chan seen, 1)
	type key struct{}
	opts := quietOptions()
	opts.I18n = I18nOptions{Store: spanishStore(t)}
	app := New(opts)
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), key{}, "from middleware")))
		})
	})
	app.Get("/task", func(ctx *Context, _ Empty) (taskOut, error) {
		return taskOut{Queued: true}, ctx.AfterResponse(func(bg context.Context) {
			id, _ := RequestIDFromContext(bg)
			locale, _ := LocaleFromContext(bg)
			_, hasDeadline := bg.Deadline()
			results <- seen{id: id, locale: locale, err: bg.Err(), hasDeadline: hasDeadline, custom: bg.Value(key{})}
		})
	}, Timeout(time.Minute))
	mustBuild(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/task", nil).WithContext(ctx)
	rec := doRequest(t, app, req)
	cancel()
	assertStatus(t, rec, http.StatusOK)
	got := <-results
	if got.id == "" || got.id != rec.Header().Get(HeaderRequestID) {
		t.Errorf("the task saw request id %q, the response carried %q", got.id, rec.Header().Get(HeaderRequestID))
	}
	if got.locale == "" || got.custom != "from middleware" {
		t.Errorf("the task lost the request's values: %+v", got)
	}
	if got.err != nil || got.hasDeadline {
		t.Errorf("the task inherited the request's cancellation or its route deadline: %+v", got)
	}
}

func TestAfterResponseRunsOnlyWhenTheHandlerSucceeded(t *testing.T) {
	t.Parallel()
	var ran atomic.Int32
	task := func(context.Context) { ran.Add(1) }
	app := New(quietOptions())
	register := func(ctx *Context) {
		if err := ctx.AfterResponse(task); err != nil {
			t.Errorf("AfterResponse = %v", err)
		}
	}
	app.Get("/error", func(ctx *Context, _ Empty) (rtOut, error) {
		register(ctx)
		return rtOut{}, Conflict("no")
	})
	app.Get("/panic", func(ctx *Context, _ Empty) (rtOut, error) {
		register(ctx)
		panic("handler")
	})
	app.Get("/guard", okHandler, WithDependencies(func(ctx *Context) error {
		register(ctx)
		return Forbidden("")
	}))
	type In struct {
		N int `query:"n"`
	}
	app.Get("/bind", func(ctx *Context, in In) (rtOut, error) { return rtOut{OK: true}, nil },
		WithDependencies(func(ctx *Context) error {
			register(ctx)
			return nil
		}))
	app.Get("/encode", func(ctx *Context, _ Empty) (panickyOut, error) {
		register(ctx)
		return panickyOut{}, nil
	})
	app.Get("/ok", func(ctx *Context, _ Empty) (rtOut, error) {
		register(ctx)
		register(ctx)
		return rtOut{OK: true}, nil
	}, WithDependencies(func(ctx *Context) error {
		register(ctx)
		return nil
	}))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/error"), http.StatusConflict)
	assertStatus(t, do(t, app, http.MethodGet, "/panic"), http.StatusInternalServerError)
	assertStatus(t, do(t, app, http.MethodGet, "/guard"), http.StatusForbidden)
	assertStatus(t, do(t, app, http.MethodGet, "/bind?n=notanumber"), http.StatusUnprocessableEntity)
	// A panic while the response of a successful handler is written is still
	// something that panicked.
	assertStatus(t, do(t, app, http.MethodGet, "/encode"), http.StatusInternalServerError)
	waitPoolIdle(t, app)
	if n := ran.Load(); n != 0 {
		t.Fatalf("%d tasks of failed requests ran", n)
	}
	app.background.mu.Lock()
	reserved := app.background.reserved
	app.background.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("failed requests left %d places in the queue taken", reserved)
	}

	assertStatus(t, do(t, app, http.MethodGet, "/ok"), http.StatusOK)
	waitFor(t, func() bool { return ran.Load() == 3 }, "the guard's and the handler's three tasks to run")
}

// panickyOut panics while it is encoded, after its handler returned.
type panickyOut struct{}

func (panickyOut) MarshalJSON() ([]byte, error) { panic("encoding") }

// goneClientWriter is a response writer whose client has gone: every write
// fails.
type goneClientWriter struct{ header http.Header }

func (w *goneClientWriter) Header() http.Header         { return w.header }
func (w *goneClientWriter) WriteHeader(int)             {}
func (w *goneClientWriter) Write([]byte) (int, error)   { return 0, io.ErrClosedPipe }
func (w *goneClientWriter) Unwrap() http.ResponseWriter { return nil }

func TestAfterResponseRunsWhenTheClientLeftAfterTheHandlerSucceeded(t *testing.T) {
	t.Parallel()
	ran := make(chan struct{})
	app := taskApp(t, quietOptions(), func(context.Context) { close(ran) })
	recovered := catchPanic(func() {
		app.ServeHTTP(&goneClientWriter{header: http.Header{}}, httptest.NewRequest(http.MethodGet, "/task", nil))
	})
	if recovered != http.ErrAbortHandler { //nolint:errorlint // comparing the recovered sentinel
		t.Fatalf("a failed write was not aborted: %v", recovered)
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the task of a handler that succeeded did not run after its response could not be written")
	}
}

// unencodableOut fails to encode with an error, after its handler returned.
type unencodableOut struct{}

func (unencodableOut) MarshalJSON() ([]byte, error) { return nil, errors.New("cannot encode") }

func TestAfterResponseRunsWhenTheResponseCannotBeEncoded(t *testing.T) {
	t.Parallel()
	ran := make(chan struct{})
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (unencodableOut, error) {
		return unencodableOut{}, ctx.AfterResponse(func(context.Context) { close(ran) })
	})
	mustBuild(t, app)
	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusInternalServerError)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the task of a handler that returned nil did not run after its response failed to encode")
	}
}

func TestAfterResponseQueueFull(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Background = BackgroundOptions{Workers: 1, Queue: 2}
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var ran atomic.Int32
	app := New(opts)
	app.Get("/task", func(ctx *Context, _ Empty) (map[string][]string, error) {
		var outcomes []string
		for range 3 {
			err := ctx.AfterResponse(func(context.Context) {
				started <- struct{}{}
				<-release
				ran.Add(1)
			})
			if err != nil {
				outcomes = append(outcomes, err.Error())
			} else {
				outcomes = append(outcomes, "queued")
			}
		}
		return map[string][]string{"outcomes": outcomes}, nil
	})
	mustBuild(t, app)

	// Two fit, and the third is refused at once with the exported error, with
	// the handler neither blocked nor given a goroutine of its own.
	rec := do(t, app, http.MethodGet, "/task")
	if !strings.Contains(rec.Body.String(), `"queued","queued","`+ErrBackgroundQueueFull.Error()) {
		t.Fatalf("outcomes = %s", rec.Body.String())
	}
	<-started
	// One is running on the single worker, so one place is free again.
	rec = do(t, app, http.MethodGet, "/task")
	if !strings.Contains(rec.Body.String(), `"queued","`+ErrBackgroundQueueFull.Error()+`","`+ErrBackgroundQueueFull.Error()) {
		t.Fatalf("outcomes = %s", rec.Body.String())
	}
	if n := poolAlive(app); n > 1 {
		t.Errorf("%d workers are running, more than the one allowed", n)
	}
	close(release)
	waitFor(t, func() bool { return ran.Load() == 3 }, "the three accepted tasks to run")
	waitPoolIdle(t, app)
}

// TestAfterResponseBoundsTheQueueUnderLoad drives far more registrations than
// the queue holds from many requests at once: every one is either accepted and
// run or refused, and the pool never holds more than its bounds.
func TestAfterResponseBoundsTheQueueUnderLoad(t *testing.T) {
	// Not in parallel, for the load it puts on the machine.
	opts := quietOptions()
	opts.Background = BackgroundOptions{Workers: 3, Queue: 50}
	var ran, refused, accepted atomic.Int32
	var maxReserved atomic.Int32
	app := New(opts)
	app.Get("/task", func(ctx *Context, _ Empty) (rtOut, error) {
		for range 10 {
			if err := ctx.AfterResponse(func(context.Context) {
				time.Sleep(time.Millisecond)
				ran.Add(1)
			}); err != nil {
				if !errors.Is(err, ErrBackgroundQueueFull) {
					t.Errorf("AfterResponse = %v", err)
				}
				refused.Add(1)
			} else {
				accepted.Add(1)
			}
		}
		ctx.app.background.mu.Lock()
		if r := int32(ctx.app.background.reserved); r > maxReserved.Load() {
			maxReserved.Store(r)
		}
		ctx.app.background.mu.Unlock()
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)
	var wg sync.WaitGroup
	for range 1000 {
		wg.Go(func() { do(t, app, http.MethodGet, "/task") })
	}
	wg.Wait()
	waitFor(t, func() bool { return ran.Load() == accepted.Load() }, "every accepted task to run")
	waitPoolIdle(t, app)
	if accepted.Load()+refused.Load() != 10_000 {
		t.Errorf("accepted %d and refused %d of 10000", accepted.Load(), refused.Load())
	}
	if refused.Load() == 0 {
		t.Error("nothing was refused, so the bound was never driven")
	}
	if maxReserved.Load() > 50 {
		t.Errorf("the queue held %d tasks, over its bound of 50", maxReserved.Load())
	}
}

func TestAfterResponseRefusals(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	var docsGuardErr atomic.Pointer[error]
	app := New(opts, WithDependencies(func(ctx *Context) error {
		if ctx.Route() == nil {
			err := ctx.AfterResponse(func(context.Context) {})
			docsGuardErr.Store(&err)
		}
		return nil
	}))
	app.Get("/nil", func(ctx *Context, _ Empty) (taskOut, error) {
		return taskOut{Refused: ctx.AfterResponse(nil).Error()}, nil
	})
	sseErr := make(chan error, 1)
	app.SSE("/events", func(ctx *Context, _ Empty, stream *SSEStream[string]) error {
		sseErr <- ctx.AfterResponse(func(context.Context) {})
		return nil
	})
	wsErr := make(chan error, 1)
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		wsErr <- ctx.AfterResponse(func(context.Context) {})
		return nil
	})
	mustBuild(t, app)

	if got := do(t, app, http.MethodGet, "/nil").Body.String(); !strings.Contains(got, "nil task") {
		t.Errorf("a nil task was not refused: %s", got)
	}
	assertStatus(t, do(t, app, http.MethodGet, "/openapi.json"), http.StatusOK)
	if p := docsGuardErr.Load(); p == nil || !errors.Is(*p, errAfterResponseNoRoute) {
		t.Errorf("a guard of the documentation registered a task: %v", p)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	res, err := http.Get(server.URL + "/events") //nolint:noctx // a test against a local server
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if err := <-sseErr; !errors.Is(err, errAfterResponseStream) {
		t.Errorf("an event stream handler registered a task: %v", err)
	}
	conn := dialWS(t, server.URL, "/ws")
	if err := <-wsErr; !errors.Is(err, errAfterResponseStream) {
		t.Errorf("a WebSocket handler registered a task: %v", err)
	}
	conn.conn.Close()

	// A Context whose request has ended belongs to nobody.
	var released Context
	if err := released.AfterResponse(func(context.Context) {}); !errors.Is(err, errAfterResponseNoRoute) {
		t.Errorf("a released Context registered a task: %v", err)
	}
}

func TestBackgroundTaskPanicIsRecovered(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.Background = BackgroundOptions{Workers: 1}
	var after atomic.Bool
	app := New(opts)
	app.Get("/task", func(ctx *Context, _ Empty) (rtOut, error) {
		_ = ctx.AfterResponse(func(context.Context) { panic("task " + strings.Repeat("x", 10_000)) })
		_ = ctx.AfterResponse(func(context.Context) { after.Store(true) })
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)
	rec := do(t, app, http.MethodGet, "/task")
	assertStatus(t, rec, http.StatusOK)
	waitFor(t, after.Load, "the worker to carry on after a panic")
	waitPoolIdle(t, app)
	text := logs.String()
	if !strings.Contains(text, "recovered from a panic in a background task") ||
		!strings.Contains(text, rec.Header().Get(HeaderRequestID)) || !strings.Contains(text, "stack") {
		t.Errorf("the panic was not logged with the request id and stack:\n%s", text)
	}
	if strings.Contains(text, strings.Repeat("x", maxLoggedPanicLength+1)) {
		t.Error("the panic value was logged at full length")
	}
}

// TestAfterResponseNeverTouchesThePooledContext runs tasks that read their
// context long after their request's Context went back to the pool and was
// reused by others. Under the race detector, a worker touching a Context
// would be reported; without it, a value read through one would be another
// request's.
func TestAfterResponseNeverTouchesThePooledContext(t *testing.T) {
	// Not in parallel, for the load it puts on the machine.
	opts := quietOptions()
	opts.Background = BackgroundOptions{Workers: 8, Queue: 1000}
	var mismatches, ran atomic.Int32
	app := New(opts)
	app.Get("/task/{n}", func(ctx *Context, _ Empty) (rtOut, error) {
		id, path := ctx.RequestID(), ctx.PathValue("n")
		return rtOut{OK: true}, ctx.AfterResponse(func(bg context.Context) {
			time.Sleep(2 * time.Millisecond)
			got, _ := RequestIDFromContext(bg)
			if got != id || path == "" {
				mismatches.Add(1)
			}
			ran.Add(1)
		})
	})
	mustBuild(t, app)
	var wg sync.WaitGroup
	for i := range 500 {
		wg.Go(func() { do(t, app, http.MethodGet, "/task/"+strings.Repeat("n", i%7+1)) })
	}
	wg.Wait()
	waitFor(t, func() bool { return ran.Load() == 500 }, "every task to run")
	if n := mismatches.Load(); n > 0 {
		t.Errorf("%d tasks saw another request's values", n)
	}
	waitPoolIdle(t, app)
}

// TestBackgroundTasksDrainBeforeLifecycleStop proves the order of a shutdown:
// the requests in flight, then the tasks they registered, then the components
// those tasks use.
func TestBackgroundTasksDrainBeforeLifecycleStop(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ShutdownTimeout = 10 * time.Second
	var finished, stopBeforeTasks atomic.Int32
	var componentOpen atomic.Bool
	component := NewLifecycle("db",
		func(context.Context) error { componentOpen.Store(true); return nil },
		func(context.Context) error {
			if finished.Load() != 4 {
				stopBeforeTasks.Store(finished.Load() + 1)
			}
			componentOpen.Store(false)
			return nil
		})
	release := make(chan struct{})
	entered := make(chan struct{})
	app := New(opts, WithLifecycle(component))
	var usedClosed atomic.Bool
	task := func(context.Context) {
		time.Sleep(100 * time.Millisecond)
		if !componentOpen.Load() {
			usedClosed.Store(true)
		}
		finished.Add(1)
	}
	app.Get("/task", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{OK: true}, errors.Join(ctx.AfterResponse(task), ctx.AfterResponse(task))
	})
	app.Get("/slow", func(ctx *Context, _ Empty) (rtOut, error) {
		// Registered before the shutdown, handed over during it.
		err := errors.Join(ctx.AfterResponse(task), ctx.AfterResponse(task))
		close(entered)
		<-release
		return rtOut{OK: true}, err
	})
	addr, done := startServer(t, app)
	if status, _, err := fetchOverTheWire(t, "http://"+addr+"/task"); err != nil || status != http.StatusOK {
		t.Fatalf("GET /task = %d, %v", status, err)
	}
	go func() { _, _, _ = fetchOverTheWire(t, "http://"+addr+"/slow") }()
	<-entered
	shutdown := make(chan error, 1)
	go func() { shutdown <- app.Shutdown(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := finished.Load(); n != 4 {
		t.Errorf("%d of 4 tasks finished by the time Shutdown returned", n)
	}
	if n := stopBeforeTasks.Load(); n != 0 {
		t.Errorf("the component was stopped with only %d tasks finished", n-1)
	}
	if usedClosed.Load() {
		t.Error("a task found its component already stopped")
	}
	waitPoolIdle(t, app)
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}

func TestAfterResponseRefusedOnceTheListenersClose(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	release := make(chan struct{})
	entered := make(chan struct{})
	refused := make(chan error, 1)
	app := New(opts)
	app.Get("/slow", func(ctx *Context, _ Empty) (rtOut, error) {
		close(entered)
		<-release
		refused <- ctx.AfterResponse(func(context.Context) {})
		return rtOut{OK: true}, nil
	})
	addr, done := startServer(t, app)
	go func() { _, _, _ = fetchOverTheWire(t, "http://"+addr+"/slow") }()
	<-entered
	shutdown := make(chan error, 1)
	go func() { shutdown <- app.Shutdown(context.Background()) }()
	waitFor(t, func() bool {
		app.background.mu.Lock()
		defer app.background.mu.Unlock()
		return app.background.closed
	}, "the shutdown to close the pool")
	close(release)
	if err := <-refused; !errors.Is(err, ErrBackgroundShuttingDown) {
		t.Errorf("a task registered during the drain = %v, want ErrBackgroundShuttingDown", err)
	}
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}
