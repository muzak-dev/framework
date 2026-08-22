package badele

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitBoundsTheKeyItStores(t *testing.T) {
	t.Parallel()
	short := strings.Repeat("a", maxRateLimitKey)
	if got := boundRateLimitKey(short); got != short {
		t.Errorf("boundRateLimitKey() rewrote a key that was already short enough")
	}

	// Two long keys that share everything but their last byte, which is what a
	// truncating bound would merge into one budget.
	first := strings.Repeat("a", maxRateLimitKey*4) + "1"
	second := strings.Repeat("a", maxRateLimitKey*4) + "2"
	boundedFirst, boundedSecond := boundRateLimitKey(first), boundRateLimitKey(second)
	if len(boundedFirst) > maxRateLimitKey {
		t.Errorf("bounded key is %d bytes, want no more than %d", len(boundedFirst), maxRateLimitKey)
	}
	if boundedFirst == boundedSecond {
		t.Error("two different keys were bounded to the same value; that is one shared budget for two clients")
	}
	if boundedFirst != boundRateLimitKey(first) {
		t.Error("bounding is not stable, so a client's counter would move between requests")
	}
}

func TestRateLimitStoresABoundedKeyForAnUnboundedTracker(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	opts := oneQuota()
	opts.Storage = storage
	// A tracker reading a header is a tracker whose key the client chooses.
	opts.Tracker = func(ctx *Context) (string, error) { return "tenant:" + ctx.Header("X-Tenant"), nil }
	app := limitedApp(t, opts)

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("X-Tenant", strings.Repeat("z", 8<<10))
	assertStatus(t, doRequest(t, app, req), http.StatusOK)

	keys := storage.seenKeys()
	if len(keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(keys))
	}
	if len(keys[0]) > maxRateLimitKey {
		t.Errorf("the storage was handed a key of %d bytes; a client must not choose how much the storage holds", len(keys[0]))
	}
}

func TestRateLimitNeverLogsTheKey(t *testing.T) {
	t.Parallel()
	const secret = "sk-live-do-not-log-this"
	storage := newRecordingStorage()
	storage.fail(errors.New("connection refused"))
	opts := oneQuota()
	opts.Storage = storage
	opts.Tracker = func(ctx *Context) (string, error) { return "apikey:" + ctx.Header("X-API-Key"), nil }

	logger, logs := captureLogger(t)
	appOpts := quietOptions()
	appOpts.Logger = logger
	app := New(appOpts, WithRateLimit(opts))
	app.Get("/ping", pong)
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("X-API-Key", secret)
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusServiceUnavailable)

	if strings.Contains(logs.String(), secret) {
		t.Error("the rate limit key reached the log; it carries whatever credential the tracker read")
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Error("the rate limit key reached the response")
	}
}

func TestRateLimitIsExactUnderConcurrency(t *testing.T) {
	t.Parallel()
	const limit = 25
	app := limitedApp(t, RateLimitOptions{
		Quotas: []Quota{{Name: "concurrent", Window: time.Hour, Limit: limit}},
	})

	var allowed, refused atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, requestFrom("/ping", "198.51.100.7"))
			switch rec.Code {
			case http.StatusOK:
				allowed.Add(1)
			case http.StatusTooManyRequests:
				refused.Add(1)
			default:
				t.Errorf("status = %d, want 200 or 429", rec.Code)
			}
		}()
	}
	wg.Wait()

	if allowed.Load() != limit {
		t.Errorf("allowed = %d, want exactly %d; a limit that can be raced past is not a limit", allowed.Load(), limit)
	}
	if refused.Load() != 200-limit {
		t.Errorf("refused = %d, want %d", refused.Load(), 200-limit)
	}
}

func TestRateLimitStateStaysBounded(t *testing.T) {
	t.Parallel()
	// A client that varies its address on every request is the attack the
	// table has to survive: the accounting must not become the exhaustion it
	// was added to prevent.
	storage := NewMemoryRateLimitStorage(MemoryRateLimitOptions{MaxEntries: 64})
	opts := oneQuota()
	opts.Storage = storage
	app := limitedApp(t, opts)

	for i := range 5000 {
		req := requestFrom("/ping", "198.51."+strconv.Itoa(i/256%256)+"."+strconv.Itoa(i%256))
		assertStatus(t, doRequest(t, app, req), http.StatusOK)
	}
	if got := storage.Len(); got > 64 {
		t.Errorf("Len() = %d after 5000 distinct clients, want no more than the bound of 64", got)
	}
}

func TestRateLimitLeavesNothingRunning(t *testing.T) {
	storage := NewMemoryRateLimitStorage(MemoryRateLimitOptions{SweepInterval: time.Millisecond})
	opts := oneQuota()
	opts.Storage = storage
	app := New(quietOptions(), WithRateLimit(opts))
	app.Get("/ping", pong)
	mustBuild(t, app)

	ctx := context.Background()
	if err := app.StartLifecycle(ctx); err != nil {
		t.Fatalf("StartLifecycle() = %v", err)
	}
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, requestFrom("/ping", "198.51.100."+strconv.Itoa(i%64)))
		}()
	}
	wg.Wait()
	if err := app.StopLifecycle(ctx); err != nil {
		t.Fatalf("StopLifecycle() = %v", err)
	}
	assertNoGoroutineLeaks(t)
}
