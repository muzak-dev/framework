package muzak

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// Three dependency types, so that one route can acquire several values and
// the order they are released in can be told apart.
type (
	relA string
	relB string
	relC string
)

// releaseLog records every acquisition and release a test's providers make,
// with the failure each release was handed.
type releaseLog struct {
	mu       sync.Mutex
	events   []string
	failures map[string]error
}

func newReleaseLog() *releaseLog { return &releaseLog{failures: map[string]error{}} }

func (l *releaseLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *releaseLog) released(name string, failure error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, "release "+name)
	l.failures[name] = failure
}

func (l *releaseLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.events, ", ")
}

// failure returns what the named release was handed, failing the test if it
// never ran.
func (l *releaseLog) failure(t *testing.T, name string) error {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	failure, ran := l.failures[name]
	if !ran {
		t.Fatalf("release %s never ran; events: %s", name, strings.Join(l.events, ", "))
	}
	return failure
}

// acquireAs is an Acquire provider that records itself and whose release
// returns releaseErr.
func acquireAs[T ~string](log *releaseLog, name string, releaseErr error) SharedOption {
	return Acquire(func(*Context) (T, Release, error) {
		log.add("acquire " + name)
		return T(name), func(failure error) error {
			log.released(name, failure)
			return releaseErr
		}, nil
	})
}

// assertEvents fails the test unless the log holds exactly want.
func assertEvents(t *testing.T, log *releaseLog, want string) {
	t.Helper()
	if got := log.String(); got != want {
		t.Errorf("events:\n got: %s\nwant: %s", got, want)
	}
}

type relOut struct {
	Value string `json:"value"`
}

func TestAcquireReleasesLastAcquiredFirstAndOnce(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	rec := httptest.NewRecorder()
	var bodyAtRelease atomic.Int64
	bodyAtRelease.Store(-1)
	app := New(quietOptions(), acquireAs[relA](log, "a", nil))
	app.Get("/x", func(ctx *Context, _ Empty) (relOut, error) {
		log.add("handler")
		return relOut{Value: string(From[relA](ctx)) + string(From[relB](ctx)) + string(From[relC](ctx))}, nil
	},
		acquireAs[relB](log, "b", nil),
		Acquire(func(*Context) (relC, Release, error) {
			log.add("acquire c")
			return "c", func(failure error) error {
				// Nothing has been written when a buffered response's releases
				// run, which is what lets a failing one replace it.
				bodyAtRelease.Store(int64(rec.Body.Len()))
				log.released("c", failure)
				return nil
			}, nil
		}))
	mustBuild(t, app)

	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"value":"abc"}`)
	assertEvents(t, log, "acquire a, acquire b, acquire c, handler, release c, release b, release a")
	for _, name := range []string{"a", "b", "c"} {
		if failure := log.failure(t, name); failure != nil {
			t.Errorf("release %s was handed %v, want nil for a success", name, failure)
		}
	}
	if got := bodyAtRelease.Load(); got != 0 {
		t.Errorf("the body held %d bytes when the release ran, want it unwritten", got)
	}
}

// TestAcquireReleasesOnEveryPath drives each way a request can end after a
// value was acquired, and checks the release saw why.
func TestAcquireReleasesOnEveryPath(t *testing.T) {
	t.Parallel()
	handlerErr := NewHTTPError(http.StatusTeapot, "short and stout")
	providerErr := NewHTTPError(http.StatusForbidden, "not yours")

	type quantityIn struct {
		Quantity int `query:"quantity"`
	}
	tests := []struct {
		name       string
		register   func(app *App, log *releaseLog)
		target     string
		status     int
		events     string
		wantFailed func(error) bool
	}{
		{
			name: "the handler returns an error",
			register: func(app *App, log *releaseLog) {
				app.Get("/x", func(*Context, Empty) (relOut, error) { return relOut{}, handlerErr }, acquireAs[relA](log, "a", nil))
			},
			status:     http.StatusTeapot,
			events:     "acquire a, release a",
			wantFailed: func(err error) bool { return errors.Is(err, handlerErr) },
		},
		{
			name: "the handler panics",
			register: func(app *App, log *releaseLog) {
				app.Get("/x", func(*Context, Empty) (relOut, error) { panic("the plumbus broke") }, acquireAs[relA](log, "a", nil))
			},
			status: http.StatusInternalServerError,
			events: "acquire a, release a",
			wantFailed: func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "the request panicked: the plumbus broke")
			},
		},
		{
			name: "a later provider refuses",
			register: func(app *App, log *releaseLog) {
				app.Get("/x", relOK, acquireAs[relA](log, "a", nil),
					Needs(func(*Context) (relB, error) { return "", providerErr }))
			},
			status:     http.StatusForbidden,
			events:     "acquire a, release a",
			wantFailed: func(err error) bool { return errors.Is(err, providerErr) },
		},
		{
			name: "a later Acquire refuses",
			register: func(app *App, log *releaseLog) {
				app.Get("/x", relOK, acquireAs[relA](log, "a", nil),
					Acquire(func(*Context) (relB, Release, error) {
						log.add("acquire b")
						// A release returned with an error is not called.
						return "", func(error) error { log.add("release b"); return nil }, providerErr
					}))
			},
			status:     http.StatusForbidden,
			events:     "acquire a, acquire b, release a",
			wantFailed: func(err error) bool { return errors.Is(err, providerErr) },
		},
		{
			name: "a later provider panics",
			register: func(app *App, log *releaseLog) {
				app.Get("/x", relOK, acquireAs[relA](log, "a", nil),
					Needs(func(*Context) (relB, error) { panic("provider bug") }))
			},
			status:     http.StatusInternalServerError,
			events:     "acquire a, release a",
			wantFailed: func(err error) bool { return err != nil && strings.Contains(err.Error(), "provider bug") },
		},
		{
			name: "binding fails",
			register: func(app *App, log *releaseLog) {
				app.Get("/x", func(*Context, quantityIn) (relOut, error) { return relOut{}, nil }, acquireAs[relA](log, "a", nil))
			},
			target: "/x?quantity=lots",
			status: http.StatusUnprocessableEntity,
			events: "acquire a, release a",
			wantFailed: func(err error) bool {
				var ve *ValidationError
				return errors.As(err, &ve)
			},
		},
		{
			name: "validation fails",
			register: func(app *App, log *releaseLog) {
				app.Post("/x", func(*Context, depValidatedIn) (relOut, error) { return relOut{}, nil },
					acquireAs[relA](log, "a", nil),
					Needs(func(*Context) (depUser, error) { return depUser{Name: "alice"}, nil }))
			},
			status: http.StatusUnprocessableEntity,
			events: "acquire a, release a",
			wantFailed: func(err error) bool {
				var ve *ValidationError
				return errors.As(err, &ve)
			},
		},
		{
			name: "the response does not encode",
			register: func(app *App, log *releaseLog) {
				app.Get("/x", func(*Context, Empty) (struct{ V float64 }, error) {
					return struct{ V float64 }{V: math.NaN()}, nil
				}, acquireAs[relA](log, "a", nil))
			},
			status:     http.StatusInternalServerError,
			events:     "acquire a, release a",
			wantFailed: func(err error) bool { return err != nil && strings.Contains(err.Error(), "encoding the response") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := newReleaseLog()
			app := New(quietOptions())
			tc.register(app, log)
			mustBuild(t, app)

			target := tc.target
			if target == "" {
				target = "/x"
			}
			method := http.MethodGet
			body := []string(nil)
			if tc.name == "validation fails" {
				method, body = http.MethodPost, []string{`{"owner":"mallory"}`}
			}
			assertStatus(t, do(t, app, method, target, body...), tc.status)
			assertEvents(t, log, tc.events)
			if failure := log.failure(t, "a"); !tc.wantFailed(failure) {
				t.Errorf("the release was handed %v", failure)
			}
		})
	}
}

// relOK is a handler that succeeds with nothing to say.
func relOK(*Context, Empty) (relOut, error) { return relOut{Value: "ok"}, nil }

// TestAcquireIsNeverReachedPastARefusingGuard pins the other side of exactly
// once: a guard runs before every provider, so a refusal acquires nothing and
// has nothing to release.
func TestAcquireIsNeverReachedPastARefusingGuard(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions())
	app.Get("/x", relOK, acquireAs[relA](log, "a", nil),
		WithDependencies(func(*Context) error { return Unauthorized("no") }))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusUnauthorized)
	assertEvents(t, log, "")
}

func TestAcquireReleasesWhenTheRateLimitRefusesAfterTheDependencies(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{
		AfterDependencies: true,
		Quotas:            []Quota{{Name: "one", Window: time.Minute, Limit: 1}},
	}))
	app.Get("/x", relOK, acquireAs[relA](log, "a", nil))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusOK)
	if failure := log.failure(t, "a"); failure != nil {
		t.Fatalf("the first release was handed %v, want nil", failure)
	}
	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusTooManyRequests)
	var he *HTTPError
	if failure := log.failure(t, "a"); !errors.As(failure, &he) || he.Status != http.StatusTooManyRequests {
		t.Errorf("the second release was handed %v, want the 429", failure)
	}
	assertEvents(t, log, "acquire a, release a, acquire a, release a")
}

func TestAcquireReleasesWhenTheClientGoesAway(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (relOut, error) {
		<-ctx.Context().Done()
		return relOut{}, ctx.Context().Err()
	}, acquireAs[relA](log, "a", nil))
	mustBuild(t, app)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	doRequest(t, app, httptest.NewRequestWithContext(ctx, http.MethodGet, "/x", nil))
	if failure := log.failure(t, "a"); !errors.Is(failure, context.Canceled) {
		t.Errorf("the release was handed %v, want the cancellation", failure)
	}
}

// TestAReleaseErrorReplacesTheSuccess is the reason releases run before the
// response is written: a commit that fails must not be reported as done.
func TestAReleaseErrorReplacesTheSuccess(t *testing.T) {
	t.Parallel()
	secret := errors.New("commit failed: password=hunter2 at db.internal:5432")
	conflict := Conflict("the order changed underneath")
	type htmlIn struct{}
	tests := []struct {
		name     string
		register func(app *App, opt SharedOption)
		release  error
		status   int
	}{
		{"a JSON body", func(app *App, opt SharedOption) { app.Get("/x", relOK, opt) }, secret, http.StatusInternalServerError},
		{"a JSON body and an HTTP error", func(app *App, opt SharedOption) { app.Get("/x", relOK, opt) }, conflict, http.StatusConflict},
		{"no content", func(app *App, opt SharedOption) {
			app.Delete("/x", func(*Context, Empty) (Empty, error) { return Empty{}, nil }, opt, Status(http.StatusNoContent))
		}, secret, http.StatusInternalServerError},
		{"an HTML page", func(app *App, opt SharedOption) {
			app.Get("/x", func(*Context, htmlIn) (HTML, error) { return "<p>done</p>", nil }, opt)
		}, secret, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := captureLogger(t)
			opts := quietOptions()
			opts.Logger = logger
			log := newReleaseLog()
			app := New(opts)
			method := http.MethodGet
			if tc.name == "no content" {
				method = http.MethodDelete
			}
			tc.register(app, acquireAs[relA](log, "a", tc.release))
			app.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// What the handler declared about the success it never sent
					// must not reach the error.
					w.Header().Set("ETag", `"v1"`)
					next.ServeHTTP(w, r)
				})
			})
			mustBuild(t, app)

			rec := do(t, app, method, "/x")
			assertStatus(t, rec, tc.status)
			if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "db.internal") {
				t.Errorf("the release error's text reached the client: %s", rec.Body.String())
			}
			if rec.Header().Get("ETag") != "" {
				t.Error("the success's ETag was sent with the error")
			}
			if tc.status == http.StatusInternalServerError && !strings.Contains(logs.String(), "hunter2") {
				t.Errorf("the cause was not logged:\n%s", logs.String())
			}
			if tc.status == http.StatusConflict && !strings.Contains(rec.Body.String(), "the order changed underneath") {
				t.Errorf("the HTTP error was not rendered: %s", rec.Body.String())
			}
		})
	}
}

// TestAFailingReleaseFailsTheReleasesBeforeIt checks that the release of an
// earlier acquisition sees a later release's failure, so a second transaction
// is rolled back when the first one's commit failed.
func TestAFailingReleaseFailsTheReleasesBeforeIt(t *testing.T) {
	t.Parallel()
	commitErr := errors.New("commit failed")
	log := newReleaseLog()
	app := New(quietOptions())
	app.Get("/x", relOK, acquireAs[relA](log, "a", nil), acquireAs[relB](log, "b", commitErr))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusInternalServerError)
	if failure := log.failure(t, "b"); failure != nil {
		t.Errorf("b was handed %v, want nil", failure)
	}
	if failure := log.failure(t, "a"); !errors.Is(failure, commitErr) {
		t.Errorf("a was handed %v, want b's failure", failure)
	}
}

func TestAReleaseErrorAfterAFailureIsLoggedAndChangesNothing(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	app := New(opts)
	app.Get("/x", func(*Context, Empty) (relOut, error) { return relOut{}, NotFound("no such order") },
		acquireAs[relA](log, "a", errors.New("rollback failed: broken pipe")))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusNotFound)
	if !strings.Contains(rec.Body.String(), "no such order") {
		t.Errorf("body = %s, want the handler's error", rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, "already failing") || !strings.Contains(out, "broken pipe") || !strings.Contains(out, "muzak.relA") {
		t.Errorf("the release error was not logged against its dependency:\n%s", out)
	}
}

func TestAPanickingReleaseIsRecoveredAndTheRestStillRun(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	app := New(opts)
	app.Get("/x", relOK, acquireAs[relA](log, "a", nil),
		Acquire(func(*Context) (relB, Release, error) {
			return "b", func(error) error { panic("release bug: token=s3cr3t") }, nil
		}))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Errorf("the panic reached the client: %s", rec.Body.String())
	}
	if failure := log.failure(t, "a"); failure == nil || !strings.Contains(failure.Error(), "panicked") {
		t.Errorf("a was handed %v, want the panicking release's failure", failure)
	}
	out := logs.String()
	if n := strings.Count(out, "recovered from a panic in a dependency's release"); n != 1 {
		t.Errorf("the panic was logged %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "dependencies_release_test.go") {
		t.Errorf("the panic's stack was not logged:\n%s", out)
	}
}

func TestAHandlerPanicIsLoggedOnceWithReleases(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	app := New(opts)
	app.Get("/x", func(*Context, Empty) (relOut, error) { panic("handler bug") },
		acquireAs[relA](log, "a", errors.New("rollback failed")))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusInternalServerError)
	if n := strings.Count(logs.String(), "recovered from a panic in a handler"); n != 1 {
		t.Errorf("the panic was logged %d times, want once:\n%s", n, logs.String())
	}
}

// TestAReleaseErrorAfterTheResponseStartedAbortsIt covers a handler that wrote
// its own response: the status is gone, so the failure is signalled the way a
// handler's own error would be, by aborting the transfer.
func TestAReleaseErrorAfterTheResponseStartedAbortsIt(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	app := New(opts)
	app.Get("/export", writeRowsThen(func() error { return nil }), acquireAs[relA](log, "a", errors.New("commit failed late")))
	app.Get("/fine", writeRowsThen(func() error { return nil }), acquireAs[relB](log, "b", nil))
	mustBuild(t, app)
	server, serverLog := abortServer(t, app)

	if status, body, err := fetchOverTheWire(t, server.URL+"/export"); err == nil {
		t.Fatalf("the response ended cleanly: status %d, body %q", status, body)
	}
	if failure := log.failure(t, "a"); failure != nil {
		t.Errorf("the release was handed %v, want nil for a handler that succeeded", failure)
	}
	if out := logs.String(); !strings.Contains(out, "commit failed late") || !strings.Contains(out, "the connection was aborted") {
		t.Errorf("the release error was not logged as an abort:\n%s", out)
	}
	if got := serverLog.String(); got != "" {
		t.Errorf("net/http logged the abort as well:\n%s", got)
	}

	// Without a release error the same response completes.
	if status, body, err := fetchOverTheWire(t, server.URL+"/fine"); err != nil || status != http.StatusOK || !strings.HasPrefix(body, "id,amount") {
		t.Errorf("status %d, body %q, err %v, want the whole export", status, body, err)
	}
}

func TestAReleaseErrorAfterAHijackIsOnlyLogged(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	app := New(opts)
	app.Get("/raw", func(ctx *Context, _ Empty) (Empty, error) {
		conn, rw, err := http.NewResponseController(ctx.ResponseWriter()).Hijack()
		if err != nil {
			return Empty{}, err
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
		return Empty{}, rw.Flush()
	}, acquireAs[relA](log, "a", errors.New("release after hijack")))
	mustBuild(t, app)
	server, _ := abortServer(t, app)

	status, body, err := fetchOverTheWire(t, server.URL+"/raw")
	if err != nil || status != http.StatusOK || body != "ok" {
		t.Errorf("status %d, body %q, err %v, want the hijacker's own response", status, body, err)
	}
	waitForLog(t, logs, "release after hijack")
	if failure := log.failure(t, "a"); failure != nil {
		t.Errorf("the release was handed %v, want nil", failure)
	}
}

// TestReleasesNeverCrossRequests is the concurrency test: every request
// acquires a value of its own, reads it back through From and a Dep, and
// releases it, with many requests in flight at once on pooled Contexts.
func TestReleasesNeverCrossRequests(t *testing.T) {
	t.Parallel()
	type tokenIn struct {
		Token Dep[relA]
	}
	var acquired, releasedTotal atomic.Int64
	var released sync.Map // token -> *atomic.Int64
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, in tokenIn) (relOut, error) {
		if from := From[relA](ctx); from != in.Token.Get() {
			return relOut{}, fmt.Errorf("From = %s, Dep = %s", from, in.Token.Get())
		}
		return relOut{Value: string(in.Token.Get())}, nil
	}, Acquire(func(ctx *Context) (relA, Release, error) {
		acquired.Add(1)
		token := relA(ctx.Header("X-Token"))
		counter, _ := released.LoadOrStore(token, new(atomic.Int64))
		return token, func(failure error) error {
			if failure != nil {
				t.Errorf("token %s released with %v", token, failure)
			}
			counter.(*atomic.Int64).Add(1)
			releasedTotal.Add(1)
			return nil
		}, nil
	}))
	mustBuild(t, app)

	// Enough workers to keep many requests in flight on many pooled Contexts,
	// and few enough to stay well inside the race detector's goroutine limit
	// while other tests run beside this one.
	const requests, workers = 4000, 64
	next := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range next {
				token := "t" + strconv.Itoa(i)
				req := httptest.NewRequest(http.MethodGet, "/x", nil)
				req.Header.Set("X-Token", token)
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || rec.Body.String() != `{"value":"`+token+`"}` {
					t.Errorf("request %s got %d %s", token, rec.Code, rec.Body.String())
				}
			}
		})
	}
	for i := range requests {
		next <- i
	}
	close(next)
	wg.Wait()

	if acquired.Load() != requests || releasedTotal.Load() != requests {
		t.Fatalf("acquired %d and released %d, want %d of each", acquired.Load(), releasedTotal.Load(), requests)
	}
	released.Range(func(token, counter any) bool {
		if n := counter.(*atomic.Int64).Load(); n != 1 {
			t.Errorf("token %v was released %d times, want once", token, n)
		}
		return true
	})
}

// TestAPooledContextNeverRunsAnotherRequestsRelease reuses one Context the way
// the pool does and checks that nothing a request acquired survives it.
func TestAPooledContextNeverRunsAnotherRequestsRelease(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	mustBuild(t, app)
	var runs atomic.Int64
	held := heldRelease{typ: reflect.TypeFor[relA](), release: func(error) error { runs.Add(1); return nil }}

	rw := asResponseWriter(httptest.NewRecorder())
	c := app.acquire(rw, httptest.NewRequest(http.MethodGet, "/x", nil))
	c.releases = append(c.releases, held)
	backing := c.releases[:1]
	c.reset()
	if len(c.releases) != 0 || backing[0].release != nil {
		t.Fatal("reset left a release behind in the Context or its backing array")
	}

	// A request that ends with a release still pending, which only a panic
	// outside a route leaves, is released as failed before the Context goes
	// back to the pool, and only once.
	c = app.acquire(rw, httptest.NewRequest(http.MethodGet, "/x", nil))
	var failure error
	c.releases = append(c.releases, heldRelease{typ: held.typ, release: func(f error) error {
		runs.Add(1)
		failure = f
		return nil
	}})
	app.release(c)
	if runs.Load() != 1 || !errors.Is(failure, errReleaseAbandoned) {
		t.Errorf("runs = %d, failure = %v, want one run with errReleaseAbandoned", runs.Load(), failure)
	}
	c = app.ctxPool.Get().(*Context)
	_ = c.settle(nil)
	if runs.Load() != 1 {
		t.Errorf("a Context from the pool ran %d releases, want none", runs.Load()-1)
	}
}

// TestSequentialRequestsShareNoRelease drives the same check end to end: a
// request on a route with no Acquire, following one with, runs nothing.
func TestSequentialRequestsShareNoRelease(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions())
	app.Get("/acquire", relOK, acquireAs[relA](log, "a", nil))
	app.Get("/plain", relOK)
	app.Get("/failing", func(*Context, Empty) (relOut, error) { return relOut{}, NotFound("no") })
	mustBuild(t, app)

	for range 50 {
		assertStatus(t, do(t, app, http.MethodGet, "/acquire"), http.StatusOK)
		assertStatus(t, do(t, app, http.MethodGet, "/plain"), http.StatusOK)
		assertStatus(t, do(t, app, http.MethodGet, "/failing"), http.StatusNotFound)
	}
	if got, want := strings.Count(log.String(), "release a"), 50; got != want {
		t.Errorf("released %d times, want %d", got, want)
	}
}

// TestMountsAndDocumentationReleaseWhatTheyInherit covers the paths with no
// handler: a file mount and the OpenAPI document run the application's
// providers, so an application-wide Acquire is released after each of them.
func TestMountsAndDocumentationReleaseWhatTheyInherit(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions(), acquireAs[relA](log, "a", nil))
	app.Static("/assets", StaticOptions{FS: fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("x")}}})
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/assets/app.js"), http.StatusOK)
	if failure := log.failure(t, "a"); failure != nil {
		t.Errorf("a served file's release was handed %v, want nil", failure)
	}
	assertStatus(t, do(t, app, http.MethodGet, "/assets/missing.js"), http.StatusNotFound)
	if failure := log.failure(t, "a"); failure == nil {
		t.Error("a missing file's release was handed nil, want the 404")
	}
	assertStatus(t, do(t, app, http.MethodGet, "/openapi.json"), http.StatusOK)
	if failure := log.failure(t, "a"); failure != nil {
		t.Errorf("the document's release was handed %v, want nil", failure)
	}
	assertEvents(t, log, "acquire a, release a, acquire a, release a, acquire a, release a")
}

// TestAReleaseErrorOnAMountIsReported covers a release failing after a file
// was served: the response is complete, so it is recorded as a failure after
// the response started rather than lost.
func TestAReleaseErrorOnAMountIsReported(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	app := New(opts, acquireAs[relA](log, "a", errors.New("mount release failed")))
	app.Static("/assets", StaticOptions{FS: fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("x")}}})
	mustBuild(t, app)
	server, serverLog := abortServer(t, app)

	for i, target := range []string{"/assets/app.js", "/openapi.json"} {
		_, _, _ = fetchOverTheWire(t, server.URL+target)
		deadline := time.Now().Add(5 * time.Second)
		for strings.Count(logs.String(), "mount release failed") <= i && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if n := strings.Count(logs.String(), "mount release failed"); n != i+1 {
			t.Fatalf("after %s the release error was logged %d times, want %d:\n%s", target, n, i+1, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "the connection was aborted") {
		t.Errorf("the failure was not recorded as one after the response started:\n%s", logs.String())
	}
	if got := serverLog.String(); got != "" {
		t.Errorf("net/http logged the abort as well:\n%s", got)
	}
}

// TestAPanicOutsideARouteStillReleases covers a provider panicking while a
// file mount admits a request: there is no route to recover it, so the
// Context's release catches what was acquired before it.
func TestAPanicOutsideARouteStillReleases(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions(), acquireAs[relA](log, "a", nil),
		Needs(func(*Context) (relB, error) { panic("mount provider bug") }))
	app.Static("/assets", StaticOptions{FS: fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("x")}}})
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/assets/app.js"), http.StatusInternalServerError)
	if failure := log.failure(t, "a"); !errors.Is(failure, errReleaseAbandoned) {
		t.Errorf("the release was handed %v, want errReleaseAbandoned", failure)
	}
	assertEvents(t, log, "acquire a, release a")
}

func TestAcquireWithANilProviderIsABuildError(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Acquire[relA](nil))
	app.Get("/a", relOK)
	app.Get("/b", relOK)
	err := app.Build()
	if err == nil || !strings.Contains(err.Error(), "muzak: Acquire was given a nil provider for muzak.relA") {
		t.Fatalf("Build() = %v, want the nil provider reported", err)
	}
	if n := strings.Count(err.Error(), "nil provider"); n != 1 {
		t.Errorf("the nil provider was reported %d times, want once however many routes inherit it:\n%v", n, err)
	}
}

// TestANilReleaseMeansNothingToRelease checks that a provider with nothing to
// release can say so.
func TestANilReleaseMeansNothingToRelease(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", relOK, Acquire(func(*Context) (relA, Release, error) { return "a", nil, nil }))
	mustBuild(t, app)
	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusOK)
}

// TestAcquireLeavesNoGoroutineBehind runs every release path and then asks
// the runtime whether anything is stuck.
func TestAcquireLeavesNoGoroutineBehind(t *testing.T) {
	log := newReleaseLog()
	app := New(quietOptions())
	app.Get("/ok", relOK, acquireAs[relA](log, "a", nil))
	app.Get("/err", func(*Context, Empty) (relOut, error) { return relOut{}, NotFound("no") }, acquireAs[relA](log, "a", nil))
	app.Get("/panic", func(*Context, Empty) (relOut, error) { panic("boom") }, acquireAs[relA](log, "a", nil))
	app.Get("/release-fails", relOK, acquireAs[relA](log, "a", errors.New("no")))
	mustBuild(t, app)
	for range 20 {
		for _, target := range []string{"/ok", "/err", "/panic", "/release-fails"} {
			do(t, app, http.MethodGet, target)
		}
	}
	assertNoGoroutineLeaks(t)
}
