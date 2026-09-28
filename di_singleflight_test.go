package muzak

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedSingleton builds an application whose /x route needs a singleton that
// blocks until release is closed and then returns what outcome says. arrived
// counts the requests that reached the route's dependencies, and entered is
// closed when the first attempt starts.
type gatedSingleton struct {
	app     *App
	calls   atomic.Int32
	arrived atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func newGatedSingleton(t *testing.T, outcome func(call int32) (diValue, error)) *gatedSingleton {
	t.Helper()
	g := &gatedSingleton{entered: make(chan struct{}), release: make(chan struct{})}
	g.app = New(quietOptions())
	g.app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, WithDependencies(func(*Context) error {
		g.arrived.Add(1)
		return nil
	}), Singleton(func(*Context) (diValue, error) {
		n := g.calls.Add(1)
		if n == 1 {
			close(g.entered)
			<-g.release
		}
		return outcome(n)
	}))
	mustBuild(t, g.app)
	return g
}

// serve sends GET /x under ctx in the background and delivers the status.
func (g *gatedSingleton) serve(ctx context.Context) <-chan int {
	status := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		g.app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx))
		status <- rec.Code
	}()
	return status
}

// waitUntilWaiting blocks until n requests have reached the singleton, and
// then a moment longer so that they are parked on the attempt in flight.
func (g *gatedSingleton) waitUntilWaiting(t *testing.T, n int32) {
	t.Helper()
	waitFor(t, func() bool { return g.arrived.Load() >= n }, "the requests to reach the singleton")
	time.Sleep(20 * time.Millisecond)
}

// TestSingletonSlowFailureDoesNotSerialiseRequests is the regression test for
// a singleton whose provider fails slowly: every waiting request used to take
// the lock in turn and run the provider again, so the tenth request waited
// ten attempts, long after its client had given up.
func TestSingletonSlowFailureDoesNotSerialiseRequests(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, Singleton(func(*Context) (diValue, error) {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond) // a dial to a database that is down
		return diValue{}, errors.New("database unreachable")
	}))
	mustBuild(t, app)

	const n = 10
	var wg sync.WaitGroup
	durations := make([]time.Duration, n)
	for i := range n {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
			began := time.Now()
			app.ServeHTTP(httptest.NewRecorder(), req)
			durations[i] = time.Since(began)
		})
	}
	wg.Wait()
	var longest time.Duration
	for _, d := range durations {
		longest = max(longest, d)
	}
	if got := calls.Load(); got > 2 {
		t.Errorf("the provider ran %d times for %d concurrent requests, want one attempt they share", got, n)
	}
	if longest > 500*time.Millisecond {
		t.Errorf("the slowest request took %v, want no request queued behind repeated attempts", longest)
	}
}

// TestSingletonWaitersStopOnTheirOwnCancellation covers a request whose
// client leaves while another request is still running the provider: it
// stops waiting at once instead of holding its goroutine until the attempt
// is over.
func TestSingletonWaitersStopOnTheirOwnCancellation(t *testing.T) {
	t.Parallel()
	g := newGatedSingleton(t, func(int32) (diValue, error) { return diValue{Text: "built"}, nil })
	trigger := g.serve(context.Background())
	<-g.entered

	ctx, cancel := context.WithCancel(context.Background())
	waiter := g.serve(ctx)
	g.waitUntilWaiting(t, 2)
	cancel()
	select {
	case code := <-waiter:
		if code != http.StatusInternalServerError {
			t.Errorf("an abandoned waiter answered %d, want 500", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter whose request was cancelled kept waiting for the provider")
	}

	close(g.release)
	if code := <-trigger; code != http.StatusOK {
		t.Errorf("the triggering request answered %d, want 200", code)
	}
	if got := g.calls.Load(); got != 1 {
		t.Errorf("the provider ran %d times, want 1", got)
	}
}

// TestSingletonWaitersShareTheOutcome covers both outcomes of the attempt a
// request waited on: a value is handed to the waiters, and so is an error,
// which is then forgotten so that the next request tries again.
func TestSingletonWaitersShareTheOutcome(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		g := newGatedSingleton(t, func(int32) (diValue, error) { return diValue{Text: "built"}, nil })
		trigger := g.serve(context.Background())
		<-g.entered
		waiter := g.serve(context.Background())
		g.waitUntilWaiting(t, 2)
		close(g.release)
		if a, b := <-trigger, <-waiter; a != http.StatusOK || b != http.StatusOK {
			t.Errorf("trigger %d, waiter %d, want both 200", a, b)
		}
		assertJSON(t, do(t, g.app, "GET", "/x"), `{"text":"built"}`)
		if got := g.calls.Load(); got != 1 {
			t.Errorf("the provider ran %d times, want 1", got)
		}
	})
	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		g := newGatedSingleton(t, func(call int32) (diValue, error) {
			if call == 1 {
				return diValue{}, NewHTTPError(http.StatusServiceUnavailable, "database unreachable")
			}
			return diValue{Text: "recovered"}, nil
		})
		trigger := g.serve(context.Background())
		<-g.entered
		waiter := g.serve(context.Background())
		g.waitUntilWaiting(t, 2)
		close(g.release)
		if a, b := <-trigger, <-waiter; a != http.StatusServiceUnavailable || b != http.StatusServiceUnavailable {
			t.Errorf("trigger %d, waiter %d, want both to share the 503", a, b)
		}
		assertJSON(t, do(t, g.app, "GET", "/x"), `{"text":"recovered"}`)
		if got := g.calls.Load(); got != 2 {
			t.Errorf("the provider ran %d times, want one shared failure and one retry", got)
		}
	})
}

// TestSingletonPanicFailsItsWaiters covers a provider that panics while other
// requests wait on it: the panic stays with the request that ran it, as a 500
// from recovery, and the waiters fail with a 500 of their own rather than
// waiting forever or re-raising a panic that is not theirs.
func TestSingletonPanicFailsItsWaiters(t *testing.T) {
	t.Parallel()
	g := newGatedSingleton(t, func(call int32) (diValue, error) {
		if call == 1 {
			panic("config server hiccup")
		}
		return diValue{Text: "ok"}, nil
	})
	trigger := g.serve(context.Background())
	<-g.entered
	waiter := g.serve(context.Background())
	g.waitUntilWaiting(t, 2)
	close(g.release)
	if a, b := <-trigger, <-waiter; a != http.StatusInternalServerError || b != http.StatusInternalServerError {
		t.Errorf("trigger %d, waiter %d, want both 500", a, b)
	}
	assertJSON(t, do(t, g.app, "GET", "/x"), `{"text":"ok"}`)
}

// TestSingletonResolvedWhileTakingTheLock covers the request that missed the
// cached value on the fast path and finds it once it holds the lock, because
// the attempt in flight finished in between; it must take that value rather
// than start another attempt.
func TestSingletonResolvedWhileTakingTheLock(t *testing.T) {
	t.Parallel()
	p := &provider{single: new(singleton), resolve: func(*Context) (any, error) {
		t.Error("the provider ran again for a singleton that already had a value")
		return nil, nil
	}}
	p.single.val = "cached"
	p.single.done.Store(true)
	if v, err := p.resolveSingleton(nil); err != nil || v != "cached" {
		t.Errorf("resolveSingleton = %v, %v; want the cached value", v, err)
	}
}
