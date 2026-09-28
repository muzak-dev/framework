package muzak

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// failingQuotaStorage counts every quota except the named ones, which fail, as
// a sharded store does when one shard is down.
type failingQuotaStorage struct {
	inner RateLimitStorage
	fail  map[string]bool
}

func (s failingQuotaStorage) Increment(ctx context.Context, quota, key string, window time.Duration) (int, time.Duration, error) {
	if s.fail[quota] {
		return 0, 0, errors.New("the shard is down")
	}
	return s.inner.Increment(ctx, quota, key, window)
}

func failOpenApp(t *testing.T, failOpen bool, quotas []Quota, fail ...string) *App {
	t.Helper()
	failing := map[string]bool{}
	for _, name := range fail {
		failing[name] = true
	}
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{
		FailOpen: failOpen,
		Storage:  failingQuotaStorage{inner: NewMemoryRateLimitStorage(MemoryRateLimitOptions{}), fail: failing},
		Quotas:   quotas,
	}
	app := New(opts)
	app.Get("/x", okHandler)
	return mustBuild(t, app)
}

func statusesOf(t *testing.T, app *App, n int) []int {
	t.Helper()
	var codes []int
	for range n {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "192.0.2.1:1"
		codes = append(codes, doRequest(t, app, req).Code)
	}
	return codes
}

// TestFailOpenKeepsAnExceededVerdict is the regression test for FailOpen
// serving a request as soon as one quota's storage call failed, discarding
// what the quotas counted before it had already said: a limit on a healthy
// quota stopped holding whenever another one of the policy was down.
func TestFailOpenKeepsAnExceededVerdict(t *testing.T) {
	t.Parallel()
	a := Quota{Name: "a", Window: time.Minute, Limit: 1}
	b := Quota{Name: "b", Window: time.Minute, Limit: 1}
	for name, tc := range map[string][]Quota{
		"failing quota after the healthy one":  {a, b},
		"failing quota before the healthy one": {b, a},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			codes := statusesOf(t, failOpenApp(t, true, tc, "b"), 4)
			want := []int{http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests}
			for i := range want {
				if codes[i] != want[i] {
					t.Fatalf("statuses = %v, want %v: the healthy quota's limit must still hold", codes, want)
				}
			}
		})
	}
}

// With every quota unable to count, FailOpen still serves the request, and has
// no state of any quota to report in the headers.
func TestFailOpenServesWhenNothingCouldBeCounted(t *testing.T) {
	t.Parallel()
	app := failOpenApp(t, true, []Quota{{Name: "a", Window: time.Minute, Limit: 1}, {Name: "b", Window: time.Minute, Limit: 1}}, "a", "b")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "192.0.2.1:1"
	for range 3 {
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("RateLimit-Limit"); got != "" {
			t.Errorf("RateLimit-Limit = %q, want none when no quota could be counted", got)
		}
	}
}

// The headers describe a quota that was counted, not one that failed.
func TestFailOpenReportsAQuotaThatWasCounted(t *testing.T) {
	t.Parallel()
	app := failOpenApp(t, true, []Quota{{Name: "b", Window: time.Minute, Limit: 7}, {Name: "a", Window: time.Minute, Limit: 3}}, "b")
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "192.0.2.1:1"
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("RateLimit-Limit"); got != "3" {
		t.Errorf("RateLimit-Limit = %q, want 3, the limit of the quota that was counted", got)
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "2" {
		t.Errorf("RateLimit-Remaining = %q, want 2", got)
	}
}

// Failing closed is unchanged: any quota that cannot be counted refuses.
func TestFailClosedRefusesWhenAnyQuotaCannotBeCounted(t *testing.T) {
	t.Parallel()
	app := failOpenApp(t, false, []Quota{{Name: "a", Window: time.Minute, Limit: 5}, {Name: "b", Window: time.Minute, Limit: 5}}, "b")
	if codes := statusesOf(t, app, 1); codes[0] != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", codes[0])
	}
}
