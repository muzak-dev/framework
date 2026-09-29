package muzak

import (
	"context"
	"net/http"
	"net/http/httptest"
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
