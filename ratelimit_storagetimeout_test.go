package muzak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hungStorage never answers until its context ends, as a shared store does
// when the network to it has stopped.
type hungStorage struct{}

func (hungStorage) Increment(ctx context.Context, _, _ string, _ time.Duration) (int, time.Duration, error) {
	<-ctx.Done()
	return 0, 0, ctx.Err()
}

func hungStorageApp(t *testing.T, failOpen bool, timeout time.Duration) *App {
	t.Helper()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{
		Storage:        hungStorage{},
		FailOpen:       failOpen,
		StorageTimeout: timeout,
		Quotas:         []Quota{{Name: "burst", Window: time.Minute, Limit: 5}},
	}
	app := New(opts)
	app.Get("/x", okHandler)
	return mustBuild(t, app)
}

// A storage that stops answering used to hold every request that reached it
// until its client gave up: the request context has no deadline, and a
// failure the limiter never hears about is one FailOpen cannot act on.
func TestHungRateLimitStorageIsBounded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		failOpen bool
		want     int
	}{
		"fail closed": {false, http.StatusServiceUnavailable},
		"fail open":   {true, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := hungStorageApp(t, tc.failOpen, 100*time.Millisecond)
			done := make(chan int, 1)
			go func() {
				req := httptest.NewRequest(http.MethodGet, "/x", nil)
				req.RemoteAddr = "192.0.2.1:1"
				done <- doRequest(t, app, req).Code
			}()
			select {
			case code := <-done:
				if code != tc.want {
					t.Errorf("status = %d, want %d", code, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a storage that never answered held the request")
			}
		})
	}
}

// TestRateLimitStorageCancelledByClient is the regression test for a client
// that hangs up while its request is counted being logged as a failure of the
// storage, which any client could produce at will. A count its own request's
// cancellation ended is logged at debug level and the request is answered as
// before; one StorageTimeout ended is the storage failing, and is logged as
// one. Failing closed, the 503 is still sent, and the failure it carries is
// logged at the same level.
func TestRateLimitStorageCancelledByClient(t *testing.T) {
	t.Parallel()
	const (
		unmetered = `","msg":"muzak: the rate limit storage failed; serving the request unmetered"`
		refused   = `","msg":"muzak: request failed"`
	)
	for name, tc := range map[string]struct {
		failOpen bool
		cancel   bool
		status   int
		line     string
	}{
		"fail open, the client went away":    {true, true, http.StatusOK, `"level":"DEBUG` + unmetered},
		"fail open, the storage timed out":   {true, false, http.StatusOK, `"level":"WARN` + unmetered},
		"fail closed, the client went away":  {false, true, http.StatusServiceUnavailable, `"level":"DEBUG` + refused},
		"fail closed, the storage timed out": {false, false, http.StatusServiceUnavailable, `"level":"ERROR` + refused},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logger, logs := captureLogger(t)
			opts := quietOptions()
			opts.Logger = logger
			opts.RateLimit = RateLimitOptions{
				Storage:        hungStorage{},
				FailOpen:       tc.failOpen,
				StorageTimeout: time.Millisecond,
				Quotas:         []Quota{{Name: "burst", Window: time.Minute, Limit: 5}},
			}
			app := New(opts)
			app.Get("/x", okHandler)
			mustBuild(t, app)
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.RemoteAddr = "192.0.2.1:1"
			if tc.cancel {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			rec := doRequest(t, app, req)
			assertStatus(t, rec, tc.status)
			if tc.status == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") != rateLimitStorageRetryAfter {
				t.Errorf("Retry-After = %q, want %s", rec.Header().Get("Retry-After"), rateLimitStorageRetryAfter)
			}
			if !strings.Contains(logs.String(), tc.line) {
				t.Errorf("want %s; the log holds:\n%s", tc.line, logs.String())
			}
		})
	}
}

// TestWebSocketRateLimitStorageCancelledByClient is the same for a message
// counted on a WebSocket connection: a count the handler's own context ended,
// as it ends when the peer goes away, is logged at debug level, and the
// connection is treated as before.
func TestWebSocketRateLimitStorageCancelledByClient(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		failOpen bool
		cancel   bool
		status   WSStatus
		line     string
	}{
		"fail open, the peer went away":      {true, true, 0, `"level":"DEBUG","msg":"muzak: the rate limit storage failed; the websocket message was not counted"`},
		"fail open, the storage timed out":   {true, false, 0, `"level":"WARN","msg":"muzak: the rate limit storage failed; the websocket message was not counted"`},
		"fail closed, the peer went away":    {false, true, WSStatusTryAgainLater, `"level":"DEBUG","msg":"muzak: the rate limit storage failed; closing the websocket connection"`},
		"fail closed, the storage timed out": {false, false, WSStatusTryAgainLater, `"level":"ERROR","msg":"muzak: the rate limit storage failed; closing the websocket connection"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := newRateLimitConfig(RateLimitOptions{Storage: hungStorage{}, FailOpen: tc.failOpen, StorageTimeout: time.Millisecond},
				[]Quota{{Name: "messages", Window: time.Minute, Limit: 5}})
			if err != nil {
				t.Fatal(err)
			}
			logger, logs := captureLogger(t)
			limiter := &wsMessageLimiter{cfg: cfg, key: "192.0.2.1", logger: logger, requestID: "r"}
			ctx := context.Background()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if status, _ := limiter.allow(ctx); status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if !strings.Contains(logs.String(), tc.line) {
				t.Errorf("want %s; the log holds:\n%s", tc.line, logs.String())
			}
		})
	}
}

func TestRateLimitStorageTimeoutDefaultsAndDisables(t *testing.T) {
	t.Parallel()
	cfg, err := newRateLimitConfig(RateLimitOptions{}, []Quota{{Name: "a", Window: time.Minute, Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.storageTimeout != DefaultRateLimitStorageTimeout {
		t.Errorf("default = %s, want %s", cfg.storageTimeout, DefaultRateLimitStorageTimeout)
	}
	cfg, _ = newRateLimitConfig(RateLimitOptions{StorageTimeout: -1}, []Quota{{Name: "a", Window: time.Minute, Limit: 1}})
	if cfg.storageTimeout != 0 {
		t.Errorf("negative = %s, want no bound", cfg.storageTimeout)
	}
}
