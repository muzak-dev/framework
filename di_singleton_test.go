package muzak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSingletonRetriesAfterError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, Singleton(func(ctx *Context) (diValue, error) {
		if calls.Add(1) == 1 {
			return diValue{}, NewHTTPError(http.StatusServiceUnavailable, "not today")
		}
		return diValue{Text: "recovered"}, nil
	}))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/x"), http.StatusServiceUnavailable)
	for range 3 {
		assertJSON(t, do(t, app, "GET", "/x"), `{"text":"recovered"}`)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the singleton provider ran %d times, want 2: one failure, then one success kept", got)
	}
}

func TestSingletonPanicIsNotCached(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, Singleton(func(ctx *Context) (diValue, error) {
		if calls.Add(1) == 1 {
			panic("transient: config server hiccup")
		}
		return diValue{Text: "ok"}, nil
	}))
	mustBuild(t, app)

	// The panic still belongs to the request that triggered it, as a 500 from
	// recovery, but it must not poison the requests after it.
	assertStatus(t, do(t, app, "GET", "/x"), http.StatusInternalServerError)
	for range 2 {
		assertJSON(t, do(t, app, "GET", "/x"), `{"text":"ok"}`)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the singleton provider ran %d times, want 2", got)
	}
}

type singletonKey struct{}

func TestSingletonCancellationIsNotCached(t *testing.T) {
	t.Parallel()
	var sawValue atomic.Bool
	app := New(quietOptions())
	app.Get("/me", func(c *Context, _ Empty) (diOut, error) {
		return diOut{Number: From[diOther](c).Number}, nil
	}, Singleton(func(c *Context) (diOther, error) {
		// A provider doing I/O with the request context, as Context invites.
		if err := c.Context().Err(); err != nil {
			return diOther{}, err
		}
		sawValue.Store(c.Context().Value(singletonKey{}) == "kept")
		return diOther{Number: 3}, nil
	}))
	mustBuild(t, app)

	// The first request arrives already abandoned by its client. The provider
	// must not see that cancellation, because its value outlives the request.
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), singletonKey{}, "kept"))
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/me", strings.NewReader("")).WithContext(ctx)
	doRequest(t, app, req)

	for range 2 {
		assertJSON(t, do(t, app, "GET", "/me"), `{"text":"","number":3}`)
	}
	if !sawValue.Load() {
		t.Error("the singleton provider lost the triggering request's context values")
	}
}

func TestSingletonRestoresTheRequestAfterResolving(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app := New(quietOptions())
	app.Get("/x", func(c *Context, _ Empty) (diOut, error) {
		// Only the provider runs detached; the handler still sees the real
		// request, cancellation included.
		if c.Context().Err() == nil {
			return diOut{Text: "detached"}, nil
		}
		return diOut{Text: "original"}, nil
	}, Singleton(func(*Context) (diValue, error) { return diValue{}, nil }))
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodGet, "/x", strings.NewReader("")).WithContext(ctx)
	assertJSON(t, doRequest(t, app, req), `{"text":"original"}`)
}

func TestSingletonConcurrentFirstRequestsConstructOnce(t *testing.T) {
	t.Parallel()
	var attempts, successes, inFlight, maxInFlight atomic.Int32
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, Singleton(func(*Context) (diValue, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		// Hold the provider long enough for the other requests to pile up
		// behind it, which is the moment a stampede would happen.
		time.Sleep(5 * time.Millisecond)
		if attempts.Add(1) == 1 {
			return diValue{}, NewHTTPError(http.StatusServiceUnavailable, "warming up")
		}
		successes.Add(1)
		return diValue{Text: "built"}, nil
	}))
	mustBuild(t, app)

	const requests = 32
	var wg sync.WaitGroup
	var ok, unavailable atomic.Int32
	for range requests {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
			switch rec.Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusServiceUnavailable:
				unavailable.Add(1)
			default:
				t.Errorf("status = %d, want 200 or 503", rec.Code)
			}
		})
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("the singleton value was constructed %d times, want 1", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("the provider ran %d times, want 2: one failure, one success", got)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("%d provider calls overlapped, want them serialized", got)
	}
	if ok.Load() != requests-1 || unavailable.Load() != 1 {
		t.Errorf("got %d successes and %d failures, want %d and 1", ok.Load(), unavailable.Load(), requests-1)
	}
}
