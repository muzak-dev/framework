package muzak

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// A window near the longest a Duration holds used to overflow the rounding in
// resetSeconds, which turned a refusal that should say "come back in three
// centuries" into a negative RateLimit-Reset and a Retry-After of one second.
func TestRateLimitSecondsDoNotOverflow(t *testing.T) {
	t.Parallel()
	longest := time.Duration(math.MaxInt64)
	if got := resetSeconds(longest); got < 1<<30 {
		t.Errorf("resetSeconds(MaxInt64) = %d, want a very large positive number", got)
	}
	if got := resetSeconds(longest - time.Second + 1); got < 1<<30 {
		t.Errorf("resetSeconds(just under MaxInt64) = %d, want a very large positive number", got)
	}
	if got := windowSeconds(longest); got < 1<<30 {
		t.Errorf("windowSeconds(MaxInt64) = %d, want a very large positive number", got)
	}
	// Rounding up is kept below the extreme.
	for reset, want := range map[time.Duration]int{
		1: 1, time.Second: 1, time.Second + 1: 2, 1500 * time.Millisecond: 2, 0: 0, -5: 0,
	} {
		if got := resetSeconds(reset); got != want {
			t.Errorf("resetSeconds(%s) = %d, want %d", reset, got, want)
		}
	}
}

func TestRateLimitWithAnEnormousWindowStillRefuses(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{
		Quotas: []Quota{{Name: "once", Window: time.Duration(math.MaxInt64), Limit: 1}},
	}
	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	var rec *httptest.ResponseRecorder
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = "192.0.2.1:1"
		rec = doRequest(t, app, req)
	}
	assertStatus(t, rec, http.StatusTooManyRequests)
	for _, name := range []string{"Retry-After", "RateLimit-Reset"} {
		seconds, err := strconv.Atoi(rec.Header().Get(name))
		if err != nil || seconds < 1<<30 {
			t.Errorf("%s = %q, want a very large positive number of seconds", name, rec.Header().Get(name))
		}
	}
}

// A count that has run as far as an int goes stays there, where wrapping to
// a negative number would put the client under its limit again.
func TestMemoryRateLimitCountSaturates(t *testing.T) {
	t.Parallel()
	s := NewMemoryRateLimitStorage(MemoryRateLimitOptions{})
	if _, _, err := s.Increment(context.Background(), "q", "k", time.Hour); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.entries[memoryKey("q", "k")].count = math.MaxInt
	s.mu.Unlock()
	count, _, err := s.Increment(context.Background(), "q", "k", time.Hour)
	if err != nil || count != math.MaxInt {
		t.Errorf("Increment = %d, %v; want the count to stay at MaxInt", count, err)
	}
}
