package muzak

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pong is the handler every rate limiting test is registered with, chosen
// because what it returns is of no interest: what matters is whether it ran.
func pong(_ *Context, _ Empty) (string, error) { return "pong", nil }

// requestFrom builds a request that arrived from a particular address, which
// is how a test gives two clients two budgets.
func requestFrom(target, ip string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = ip + ":41234"
	return req
}

// limitedApp builds an application with one rate limited route at /ping.
func limitedApp(t *testing.T, opts RateLimitOptions, routeOpts ...RouteOption) *App {
	t.Helper()
	app := New(quietOptions(), WithRateLimit(opts))
	app.Get("/ping", pong, routeOpts...)
	return mustBuild(t, app)
}

// oneQuota is the policy most of these tests use: three requests a minute, so
// that the fourth is refused without any waiting.
func oneQuota() RateLimitOptions {
	return RateLimitOptions{Quotas: []Quota{{Name: "test", Window: time.Minute, Limit: 3}}}
}

// recordingStorage counts in memory and records what it was asked, and can be
// told to fail. It deliberately does not implement [Lifecycle], so that the
// tests can tell an automatically registered storage from one that is not.
type recordingStorage struct {
	inner *MemoryRateLimitStorage

	mu    sync.Mutex
	quota []string
	keys  []string
	err   error
}

func newRecordingStorage() *recordingStorage {
	return &recordingStorage{inner: NewMemoryRateLimitStorage(MemoryRateLimitOptions{})}
}

func (s *recordingStorage) Increment(ctx context.Context, quota, key string, window time.Duration) (int, time.Duration, error) {
	s.mu.Lock()
	s.quota = append(s.quota, quota)
	s.keys = append(s.keys, key)
	failure := s.err
	s.mu.Unlock()
	if failure != nil {
		return 0, 0, failure
	}
	return s.inner.Increment(ctx, quota, key, window)
}

func (s *recordingStorage) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingStorage) seenKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

func (s *recordingStorage) seenQuotas() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.quota...)
}

func (s *recordingStorage) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// managedStorage is a storage that takes part in the application lifecycle,
// which is how the tests check that one is started without being registered by
// hand.
type managedStorage struct {
	recordingStorage
	mu     sync.Mutex
	starts int
	stops  int
}

func newManagedStorage() *managedStorage {
	return &managedStorage{recordingStorage: *newRecordingStorage()}
}

func (s *managedStorage) Name() string { return "test-ratelimit" }

func (s *managedStorage) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	return nil
}

func (s *managedStorage) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	return nil
}

func (s *managedStorage) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.stops
}

func TestRateLimitIsOffByDefault(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/ping", pong)
	mustBuild(t, app)

	for range 50 {
		rec := do(t, app, http.MethodGet, "/ping")
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get(HeaderRateLimitLimit); got != "" {
			t.Fatalf("%s = %q, want no rate limit headers on an unlimited route", HeaderRateLimitLimit, got)
		}
	}
}

func TestRateLimitRefusesOnceTheQuotaIsSpent(t *testing.T) {
	t.Parallel()
	app := limitedApp(t, oneQuota())

	for i := range 3 {
		rec := do(t, app, http.MethodGet, "/ping")
		assertStatus(t, rec, http.StatusOK)
		if got, want := rec.Header().Get(HeaderRateLimitRemaining), strconv.Itoa(2-i); got != want {
			t.Errorf("request %d: %s = %q, want %q", i+1, HeaderRateLimitRemaining, got, want)
		}
		if got := rec.Header().Get(HeaderRateLimitLimit); got != "3" {
			t.Errorf("request %d: %s = %q, want %q", i+1, HeaderRateLimitLimit, got, "3")
		}
	}

	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusTooManyRequests)
	body := decodeError(t, rec)
	if body.Error.Code != CodeTooManyRequests {
		t.Errorf("error code = %q, want %q", body.Error.Code, CodeTooManyRequests)
	}
	if !strings.Contains(body.Error.Message, `"test"`) {
		t.Errorf("message = %q, want it to name the quota that was exceeded", body.Error.Message)
	}
	if got := rec.Header().Get(HeaderRateLimitRemaining); got != "0" {
		t.Errorf("%s = %q, want %q", HeaderRateLimitRemaining, got, "0")
	}
	retry, err := strconv.Atoi(rec.Header().Get(HeaderRetryAfter))
	if err != nil || retry <= 0 || retry > 60 {
		t.Errorf("%s = %q, want a positive number of seconds no larger than the window",
			HeaderRetryAfter, rec.Header().Get(HeaderRetryAfter))
	}
}

func TestRateLimitDescribesTheWholePolicy(t *testing.T) {
	t.Parallel()
	app := limitedApp(t, RateLimitOptions{Quotas: []Quota{
		{Name: "short", Window: time.Second, Limit: 3},
		{Name: "medium", Window: 10 * time.Second, Limit: 20},
		{Name: "long", Window: time.Minute, Limit: 100},
	}})

	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusOK)
	if got, want := rec.Header().Get(HeaderRateLimitPolicy), "3;w=1, 20;w=10, 100;w=60"; got != want {
		t.Errorf("%s = %q, want %q", HeaderRateLimitPolicy, got, want)
	}
	// The quota closest to being spent is the one worth reporting.
	if got := rec.Header().Get(HeaderRateLimitLimit); got != "3" {
		t.Errorf("%s = %q, want the tightest quota, %q", HeaderRateLimitLimit, got, "3")
	}
	if got := rec.Header().Get(HeaderRateLimitRemaining); got != "2" {
		t.Errorf("%s = %q, want %q", HeaderRateLimitRemaining, got, "2")
	}
}

func TestRateLimitCountsEveryQuotaForAServedRequest(t *testing.T) {
	t.Parallel()
	counters, clock := newTestStorage(t, MemoryRateLimitOptions{})
	storage := newRecordingStorage()
	storage.inner = counters
	app := limitedApp(t, RateLimitOptions{
		Storage: storage,
		Quotas: []Quota{
			{Name: "burst", Window: time.Second, Limit: 2},
			{Name: "sustained", Window: time.Minute, Limit: 3},
		},
	})

	// Two bursts of two requests, a second apart. The short window forgives
	// the pause; the long one does not, which is the whole point of declaring
	// both.
	for range 2 {
		assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	}
	clock.advance(2 * time.Second)
	assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)

	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusTooManyRequests)
	if !strings.Contains(decodeError(t, rec).Error.Message, `"sustained"`) {
		t.Error("the sustained quota should be the one refusing, since the burst quota's window has restarted")
	}
	if got := rec.Header().Get(HeaderRetryAfter); got == "" {
		t.Errorf("%s is missing from a refusal", HeaderRetryAfter)
	}
	// Each served request is counted by both quotas, the longer window first
	// whatever order they were declared in. The refused one stops at the
	// quota that refused it, which is the first one counted.
	want := []string{"sustained", "burst", "sustained", "burst", "sustained", "burst", "sustained"}
	if got := storage.seenQuotas(); !slices.Equal(got, want) {
		t.Errorf("quotas consulted = %q, want %q", got, want)
	}
}

// TestRateLimitStopsCountingAtTheQuotaThatRefuses is the regression test for a
// refused request that was still counted against every quota of its policy.
// Each quota after the one that refused it spent budget the request was never
// going to use, and counted it under a key that need not have been seen
// before, so a client being answered 429 still created counters at full speed.
// Quotas are now counted longest window first and the count stops at the
// first that refuses: a request refused by a short burst limit has already
// been counted against the sustained one, so overrunning the burst still
// costs a client its sustained budget, and a request the sustained limit
// refuses touches nothing shorter.
func TestRateLimitStopsCountingAtTheQuotaThatRefuses(t *testing.T) {
	t.Parallel()
	counters, clock := newTestStorage(t, MemoryRateLimitOptions{})
	storage := newRecordingStorage()
	storage.inner = counters
	app := limitedApp(t, RateLimitOptions{
		Storage: storage,
		Quotas: []Quota{
			{Name: "burst", Window: time.Second, Limit: 1},
			{Name: "sustained", Window: time.Hour, Limit: 3},
		},
	})

	assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusTooManyRequests)
	if !strings.Contains(decodeError(t, rec).Error.Message, `"burst"`) {
		t.Errorf("message = %q, want the burst quota to be the one refusing", decodeError(t, rec).Error.Message)
	}
	clock.advance(2 * time.Second)
	assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	for range 3 {
		rec = do(t, app, http.MethodGet, "/ping")
		assertStatus(t, rec, http.StatusTooManyRequests)
		if !strings.Contains(decodeError(t, rec).Error.Message, `"sustained"`) {
			t.Errorf("message = %q, want the sustained quota to be the one refusing", decodeError(t, rec).Error.Message)
		}
	}

	want := []string{
		"sustained", "burst", // served
		"sustained", "burst", // refused by the burst limit, and still counted against the sustained one
		"sustained", "burst", // served, the third against the sustained limit
		"sustained", "sustained", "sustained", // refused by the sustained limit, and nothing shorter counted
	}
	if got := storage.seenQuotas(); !slices.Equal(got, want) {
		t.Errorf("quotas consulted = %q, want %q", got, want)
	}
	if count, _ := increment(t, counters, "burst", storage.seenKeys()[0], time.Second); count != 2 {
		t.Errorf("the burst count = %d, want 2: requests the sustained limit refused were counted against it", count)
	}
}

// TestRateLimitResolvedQuotasAreCountedLongestFirst covers the counting order
// for quotas a resolver supplies, alone and added to static ones. The order is
// the limiter's, so a plan's own slice, which a resolver typically hands out
// to every request, is never reordered, and the policy header still lists the
// quotas as they were declared and resolved.
func TestRateLimitResolvedQuotasAreCountedLongestFirst(t *testing.T) {
	t.Parallel()
	plan := []Quota{
		{Name: "plan-burst", Window: time.Second, Limit: 5},
		{Name: "plan-daily", Window: 24 * time.Hour, Limit: 50},
	}
	resolver := func(*Context) ([]Quota, error) { return plan, nil }

	t.Run("resolved alone", func(t *testing.T) {
		t.Parallel()
		storage := newRecordingStorage()
		app := limitedApp(t, RateLimitOptions{Storage: storage, Resolver: resolver})
		rec := do(t, app, http.MethodGet, "/ping")
		assertStatus(t, rec, http.StatusOK)
		if got, want := storage.seenQuotas(), []string{"plan-daily", "plan-burst"}; !slices.Equal(got, want) {
			t.Errorf("quotas consulted = %q, want %q", got, want)
		}
		if got, want := rec.Header().Get(HeaderRateLimitPolicy), "5;w=1, 50;w=86400"; got != want {
			t.Errorf("%s = %q, want %q", HeaderRateLimitPolicy, got, want)
		}
		if plan[0].Name != "plan-burst" {
			t.Errorf("the resolver's own slice was reordered to %v", plan)
		}
	})

	t.Run("added to static quotas", func(t *testing.T) {
		t.Parallel()
		storage := newRecordingStorage()
		app := limitedApp(t, RateLimitOptions{
			Storage:  storage,
			Resolver: resolver,
			Quotas:   []Quota{{Name: "floor", Window: time.Minute, Limit: 100}},
		})
		rec := do(t, app, http.MethodGet, "/ping")
		assertStatus(t, rec, http.StatusOK)
		if got, want := storage.seenQuotas(), []string{"plan-daily", "floor", "plan-burst"}; !slices.Equal(got, want) {
			t.Errorf("quotas consulted = %q, want %q", got, want)
		}
		if got, want := rec.Header().Get(HeaderRateLimitPolicy), "100;w=60, 5;w=1, 50;w=86400"; got != want {
			t.Errorf("%s = %q, want %q", HeaderRateLimitPolicy, got, want)
		}
	})
}

func TestRateLimitReportsTheLongestWaitAmongExceededQuotas(t *testing.T) {
	t.Parallel()
	storage, _ := newTestStorage(t, MemoryRateLimitOptions{})
	app := limitedApp(t, RateLimitOptions{
		Storage: storage,
		Quotas: []Quota{
			{Name: "brief", Window: time.Second, Limit: 1},
			{Name: "hour", Window: time.Hour, Limit: 1},
		},
	})

	assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusTooManyRequests)
	retry, err := strconv.Atoi(rec.Header().Get(HeaderRetryAfter))
	if err != nil {
		t.Fatalf("%s = %q", HeaderRetryAfter, rec.Header().Get(HeaderRetryAfter))
	}
	if retry != 3600 {
		t.Errorf("%s = %d, want the longest wait among the quotas that refused, 3600", HeaderRetryAfter, retry)
	}
}

func TestRateLimitGivesEachClientItsOwnBudget(t *testing.T) {
	t.Parallel()
	app := limitedApp(t, oneQuota())

	for range 3 {
		assertStatus(t, doRequest(t, app, requestFrom("/ping", "198.51.100.1")), http.StatusOK)
	}
	assertStatus(t, doRequest(t, app, requestFrom("/ping", "198.51.100.1")), http.StatusTooManyRequests)
	assertStatus(t, doRequest(t, app, requestFrom("/ping", "198.51.100.2")), http.StatusOK)
}

func TestRateLimitCannotBeEvadedByRewritingTheAddress(t *testing.T) {
	t.Parallel()
	app := limitedApp(t, oneQuota())

	for range 3 {
		assertStatus(t, doRequest(t, app, requestFrom("/ping", "198.51.100.1")), http.StatusOK)
	}
	// The same address written as an IPv4-mapped IPv6 address is the same
	// client, and must not buy a second budget.
	assertStatus(t, doRequest(t, app, requestFrom("/ping", "[::ffff:198.51.100.1]")), http.StatusTooManyRequests)
}

func TestRateLimitIgnoresAnUntrustedForwardingHeader(t *testing.T) {
	t.Parallel()
	app := limitedApp(t, oneQuota())

	for i := range 4 {
		req := requestFrom("/ping", "198.51.100.1")
		req.Header.Set(DefaultForwardedHeader, "203.0.113."+strconv.Itoa(i))
		want := http.StatusOK
		if i == 3 {
			want = http.StatusTooManyRequests
		}
		assertStatus(t, doRequest(t, app, req), want)
	}
}

func TestRateLimitFollowsATrustedForwardingHeader(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ClientIP = ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}}
	opts.RateLimit = oneQuota()
	app := New(opts)
	app.Get("/ping", pong)
	mustBuild(t, app)

	send := func(claimed string) int {
		req := requestFrom("/ping", "192.0.2.9")
		req.Header.Set(DefaultForwardedHeader, claimed)
		return doRequest(t, app, req).Code
	}
	for range 3 {
		if got := send("198.51.100.1"); got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
	}
	if got := send("198.51.100.1"); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 for the client that spent its quota", got)
	}
	if got := send("198.51.100.2"); got != http.StatusOK {
		t.Errorf("status = %d, want 200 for a different client behind the same proxy", got)
	}
}

func TestRateLimitPerRouteReplacesTheInheritedQuotas(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{
		Quotas: []Quota{{Name: "global", Window: time.Minute, Limit: 100}},
	}))
	app.Get("/ping", pong)
	app.Post("/login", func(_ *Context, _ Empty) (string, error) { return "ok", nil },
		RateLimit(Quota{Name: "login", Window: time.Minute, Limit: 2}))
	mustBuild(t, app)

	for range 2 {
		assertStatus(t, do(t, app, http.MethodPost, "/login"), http.StatusOK)
	}
	rec := do(t, app, http.MethodPost, "/login")
	assertStatus(t, rec, http.StatusTooManyRequests)
	if got := rec.Header().Get(HeaderRateLimitPolicy); got != "2;w=60" {
		t.Errorf("%s = %q, want only the route's own policy, %q", HeaderRateLimitPolicy, got, "2;w=60")
	}
	// The stricter route did not spend the budget of the rest.
	assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
}

func TestRateLimitAppliesToARouter(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	admin := NewRouter()
	admin.Get("/reports", pong)
	app.Get("/ping", pong)
	app.Include(admin, WithPrefix("/admin"), WithRateLimit(RateLimitOptions{
		Quotas: []Quota{{Name: "admin", Window: time.Minute, Limit: 1}},
	}))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/admin/reports"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, "/admin/reports"), http.StatusTooManyRequests)
	for range 5 {
		assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	}
}

func TestSkipRateLimit(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithRateLimit(oneQuota()))
	app.Get("/ping", pong)
	app.Get("/health", pong, SkipRateLimit())
	internal := NewRouter(SkipRateLimit())
	internal.Get("/metrics", pong)
	app.Include(internal, WithPrefix("/internal"))
	mustBuild(t, app)

	for range 20 {
		assertStatus(t, do(t, app, http.MethodGet, "/health"), http.StatusOK)
		assertStatus(t, do(t, app, http.MethodGet, "/internal/metrics"), http.StatusOK)
	}
	for range 3 {
		assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	}
	assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusTooManyRequests)
}

func TestRateLimitWithNoQuotasIsOff(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithRateLimit(oneQuota()))
	app.Get("/ping", pong, RateLimit())
	mustBuild(t, app)

	for range 20 {
		assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	}
}

func TestRateLimitHeadersCanBeTurnedOff(t *testing.T) {
	t.Parallel()
	opts := oneQuota()
	opts.DisableHeaders = true
	app := limitedApp(t, opts)

	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusOK)
	for _, name := range []string{HeaderRateLimitLimit, HeaderRateLimitRemaining, HeaderRateLimitReset, HeaderRateLimitPolicy} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("%s = %q, want it absent", name, got)
		}
	}
	for range 3 {
		rec = do(t, app, http.MethodGet, "/ping")
	}
	assertStatus(t, rec, http.StatusTooManyRequests)
	if got := rec.Header().Get(HeaderRetryAfter); got == "" {
		t.Error("Retry-After is missing; a refusal must still say how long to wait")
	}
}

func TestRateLimitTrackerErrorBecomesTheResponse(t *testing.T) {
	t.Parallel()
	opts := oneQuota()
	opts.Tracker = func(ctx *Context) (string, error) {
		key := ctx.Header("X-API-Key")
		if key == "" {
			return "", NewHTTPError(http.StatusUnauthorized, "an API key is required")
		}
		return "apikey:" + key, nil
	}
	storage := newRecordingStorage()
	opts.Storage = storage
	app := limitedApp(t, opts)

	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusUnauthorized)
	if storage.calls() != 0 {
		t.Error("a request the tracker refused was counted; nothing should be spent on it")
	}

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("X-API-Key", "abc")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
	if keys := storage.seenKeys(); len(keys) != 1 || keys[0] != "apikey:abc" {
		t.Errorf("keys = %q, want the tracker's key", keys)
	}
}

func TestRateLimitRefusesAnEmptyKey(t *testing.T) {
	t.Parallel()
	opts := oneQuota()
	opts.Tracker = func(*Context) (string, error) { return "", nil }
	app := limitedApp(t, opts)

	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusInternalServerError)
	if got := decodeError(t, rec).Error.Message; got != internalMessage {
		t.Errorf("message = %q, want the opaque internal message", got)
	}
}

func TestIPTrackerWithoutAnAddress(t *testing.T) {
	t.Parallel()
	app := limitedApp(t, oneQuota())

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.RemoteAddr = "/var/run/muzak.sock"
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestRateLimitFailsClosedWhenTheStorageCannotCount(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	storage.fail(errors.New("connection refused"))
	opts := oneQuota()
	opts.Storage = storage

	logger, logs := captureLogger(t)
	appOpts := quietOptions()
	appOpts.Logger = logger
	app := New(appOpts, WithRateLimit(opts))
	app.Get("/ping", pong)
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/ping")
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if got := decodeError(t, rec).Error.Message; strings.Contains(got, "connection refused") {
		t.Errorf("message = %q, want the storage failure kept out of the response", got)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Error("the storage failure was not logged; a limiter that stops counting must say so")
	}
}

// TestRateLimitStorageFailureSaysWhenToRetry is the regression test for the
// 503 a failing storage produces, which carried no Retry-After: every client
// refused because the store had stopped answering was free to retry at once,
// adding its load to the store's recovery. It now says when to come back, as
// the 429 does, and like the 429's the header outlives the reset an error
// response gets and is sent even with the RateLimit headers turned off.
func TestRateLimitStorageFailureSaysWhenToRetry(t *testing.T) {
	t.Parallel()
	for _, disableHeaders := range []bool{false, true} {
		storage := newRecordingStorage()
		storage.fail(errors.New("connection refused"))
		opts := oneQuota()
		opts.Storage = storage
		opts.DisableHeaders = disableHeaders
		app := limitedApp(t, opts)

		rec := do(t, app, http.MethodGet, "/ping")
		assertStatus(t, rec, http.StatusServiceUnavailable)
		retry, err := strconv.Atoi(rec.Header().Get(HeaderRetryAfter))
		if err != nil || retry <= 0 {
			t.Errorf("DisableHeaders %v: %s = %q, want a positive number of seconds",
				disableHeaders, HeaderRetryAfter, rec.Header().Get(HeaderRetryAfter))
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("DisableHeaders %v: Cache-Control = %q, want the error reset to have run", disableHeaders, got)
		}
	}
}

func TestRateLimitCanFailOpen(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	storage.fail(errors.New("connection refused"))
	opts := oneQuota()
	opts.Storage = storage
	opts.FailOpen = true

	logger, logs := captureLogger(t)
	appOpts := quietOptions()
	appOpts.Logger = logger
	app := New(appOpts, WithRateLimit(opts))
	app.Get("/ping", pong)
	mustBuild(t, app)

	for range 10 {
		assertStatus(t, do(t, app, http.MethodGet, "/ping"), http.StatusOK)
	}
	if !strings.Contains(logs.String(), "serving the request unmetered") {
		t.Error("a request served without being counted was not logged")
	}
}

// currentTestUser is the identity the deferred-count tests key on.
type currentTestUser struct{ ID string }

// userOrIPTracker prefers a resolved identity and falls back to the address,
// which is the shape the deferred count exists for.
func userOrIPTracker(ctx *Context) (string, error) {
	if user, ok := TryFrom[currentTestUser](ctx); ok {
		return "user:" + user.ID, nil
	}
	ip := ctx.ClientIP()
	if ip == "" {
		return "", errRateLimitNoAddress
	}
	return "ip:" + ip, nil
}

func TestRateLimitAfterDependenciesSeesTheResolvedIdentity(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{
		Storage:           storage,
		Tracker:           userOrIPTracker,
		AfterDependencies: true,
		Quotas:            []Quota{{Name: "per-user", Window: time.Minute, Limit: 2}},
	}))
	app.Get("/ping", pong, Needs(func(ctx *Context) (currentTestUser, error) {
		return currentTestUser{ID: ctx.Header("X-User")}, nil
	}))
	mustBuild(t, app)

	send := func(user string) int {
		req := requestFrom("/ping", "198.51.100.1")
		req.Header.Set("X-User", user)
		return doRequest(t, app, req).Code
	}
	for range 2 {
		if got := send("alice"); got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
	}
	if got := send("alice"); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 once the user's quota is spent", got)
	}
	// Two users behind one address have two budgets, which is the whole
	// reason to count after the dependencies have run.
	if got := send("bob"); got != http.StatusOK {
		t.Errorf("status = %d, want 200 for a different user at the same address", got)
	}
	for _, key := range storage.seenKeys() {
		if !strings.HasPrefix(key, "user:") {
			t.Errorf("key %q, want every key to come from the resolved identity", key)
		}
	}
}

func TestRateLimitBeforeDependenciesCountsARejectedRequest(t *testing.T) {
	t.Parallel()
	denied := NewHTTPError(http.StatusUnauthorized, "no")
	storage := newRecordingStorage()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{
		Storage: storage,
		Quotas:  []Quota{{Name: "attempts", Window: time.Minute, Limit: 2}},
	}))
	app.Post("/login", func(_ *Context, _ Empty) (string, error) { return "ok", nil },
		WithDependencies(func(*Context) error { return denied }))
	mustBuild(t, app)

	for range 2 {
		assertStatus(t, do(t, app, http.MethodPost, "/login"), http.StatusUnauthorized)
	}
	// The point of counting first: a client cannot make unlimited failed
	// attempts simply because each of them was rejected before the handler.
	assertStatus(t, do(t, app, http.MethodPost, "/login"), http.StatusTooManyRequests)
	if storage.calls() != 3 {
		t.Errorf("storage calls = %d, want every attempt counted", storage.calls())
	}
}

func TestRateLimitAfterDependenciesDoesNotCountARejectedRequest(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{
		Storage:           storage,
		AfterDependencies: true,
		Quotas:            []Quota{{Name: "deferred", Window: time.Minute, Limit: 1}},
	}))
	app.Post("/login", func(_ *Context, _ Empty) (string, error) { return "ok", nil },
		WithDependencies(func(*Context) error { return NewHTTPError(http.StatusUnauthorized, "no") }))
	mustBuild(t, app)

	for range 5 {
		assertStatus(t, do(t, app, http.MethodPost, "/login"), http.StatusUnauthorized)
	}
	if storage.calls() != 0 {
		t.Errorf("storage calls = %d, want none; this is the cost of deferring the count", storage.calls())
	}
}

func TestRateLimitCreatesOneStorageForTheApplication(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithRateLimit(oneQuota()))
	app.Get("/one", pong)
	app.Get("/two", pong, RateLimit(Quota{Name: "other", Window: time.Minute, Limit: 3}))
	mustBuild(t, app)

	first := app.routes[0].rateLimit.storage
	second := app.routes[1].rateLimit.storage
	if first == nil || first != second {
		t.Fatalf("routes were given different storages (%p and %p), want one shared storage", first, second)
	}
	memory, ok := first.(*MemoryRateLimitStorage)
	if !ok {
		t.Fatalf("storage = %T, want the memory storage a policy that names none is given", first)
	}

	ctx := context.Background()
	if err := app.StartLifecycle(ctx); err != nil {
		t.Fatalf("StartLifecycle() = %v", err)
	}
	if memory.stop == nil {
		t.Error("the storage was not started with the application, so nothing sweeps it")
	}
	assertStatus(t, do(t, app, http.MethodGet, "/one"), http.StatusOK)
	if memory.Len() == 0 {
		t.Error("the shared storage counted nothing")
	}
	if err := app.StopLifecycle(ctx); err != nil {
		t.Fatalf("StopLifecycle() = %v", err)
	}
	if memory.Len() != 0 {
		t.Error("the storage kept its counters after the application stopped")
	}
}

func TestRateLimitStartsAManagedStorageOnce(t *testing.T) {
	t.Parallel()
	storage := newManagedStorage()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{Storage: storage, Quotas: oneQuota().Quotas}))
	app.Get("/one", pong)
	app.Get("/two", pong)
	app.Get("/three", pong, RateLimit(Quota{Name: "other", Window: time.Minute, Limit: 3}))
	mustBuild(t, app)

	ctx := context.Background()
	if err := app.StartLifecycle(ctx); err != nil {
		t.Fatalf("StartLifecycle() = %v", err)
	}
	if err := app.StopLifecycle(ctx); err != nil {
		t.Fatalf("StopLifecycle() = %v", err)
	}
	if starts, stops := storage.counts(); starts != 1 || stops != 1 {
		t.Errorf("storage started %d times and stopped %d, want once each however many routes share it", starts, stops)
	}
}

func TestRateLimitDoesNotManageAStorageThatIsNotALifecycle(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{
		Storage: newRecordingStorage(),
		Quotas:  oneQuota().Quotas,
	}))
	app.Get("/ping", pong)
	mustBuild(t, app)

	if got := len(app.lifecycle.components); got != 0 {
		t.Errorf("lifecycle components = %d, want none for a storage that manages nothing", got)
	}
}

func TestRateLimitBuildErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		build func(*App)
		want  string
	}{
		{
			name:  "a quota with no name",
			build: func(a *App) { a.Get("/x", pong, RateLimit(Quota{Window: time.Minute, Limit: 1})) },
			want:  "a quota needs a name",
		},
		{
			name:  "a quota name that is not a token",
			build: func(a *App) { a.Get("/x", pong, RateLimit(Quota{Name: "a b", Window: time.Minute, Limit: 1})) },
			want:  "is not a valid token",
		},
		{
			name: "the same name twice in one policy",
			build: func(a *App) {
				a.Get("/x", pong, RateLimit(
					Quota{Name: "dup", Window: time.Minute, Limit: 1},
					Quota{Name: "dup", Window: time.Minute, Limit: 1}))
			},
			want: "is declared twice in one policy",
		},
		{
			name:  "a window that is not positive",
			build: func(a *App) { a.Get("/x", pong, RateLimit(Quota{Name: "q", Limit: 1})) },
			want:  "needs a positive window",
		},
		{
			name:  "a limit that is not positive",
			build: func(a *App) { a.Get("/x", pong, RateLimit(Quota{Name: "q", Window: time.Minute})) },
			want:  "needs a positive limit",
		},
		{
			name: "one name meaning two policies",
			build: func(a *App) {
				a.Get("/x", pong, RateLimit(Quota{Name: "same", Window: time.Minute, Limit: 1}))
				a.Get("/y", pong, RateLimit(Quota{Name: "same", Window: time.Hour, Limit: 1}))
			},
			want: "one name cannot mean two policies",
		},
		{
			name: "one name meaning two limits",
			build: func(a *App) {
				a.Get("/x", pong, RateLimit(Quota{Name: "same", Window: time.Minute, Limit: 1}))
				a.Get("/y", pong, RateLimit(Quota{Name: "same", Window: time.Minute, Limit: 2}))
			},
			want: "one name cannot mean two policies",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			tc.build(app)
			if message := buildError(t, app); !strings.Contains(message, tc.want) {
				t.Errorf("build error = %q, want it to mention %q", message, tc.want)
			}
		})
	}
}

func TestRateLimitAllowsOneNameAcrossRoutesWhenItMeansOneThing(t *testing.T) {
	t.Parallel()
	quota := Quota{Name: "shared", Window: time.Minute, Limit: 2}
	app := New(quietOptions())
	app.Get("/x", pong, RateLimit(quota))
	app.Get("/y", pong, RateLimit(quota))
	mustBuild(t, app)

	// One name is one budget, so the two routes draw on the same counter.
	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, "/y"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusTooManyRequests)
}

func TestRateLimitDocumentsItsRefusal(t *testing.T) {
	t.Parallel()
	app := limitedApp(t, oneQuota())
	rec := do(t, app, http.MethodGet, "/openapi.json")
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "too many requests") {
		t.Error("the generated document does not describe the 429 a rate limited route can produce")
	}
}

func TestRateLimitOptionsOverlay(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	base := RateLimitOptions{
		Quotas:   []Quota{{Name: "base", Window: time.Minute, Limit: 1}},
		Storage:  storage,
		Tracker:  IPTracker,
		FailOpen: true,
	}
	empty := base.overlay(RateLimitOptions{})
	if len(empty.Quotas) != 1 || empty.Storage != storage || empty.Tracker == nil || !empty.FailOpen {
		t.Errorf("overlay with nothing set changed the options: %+v", empty)
	}

	other := newRecordingStorage()
	over := base.overlay(RateLimitOptions{
		Quotas:            []Quota{},
		Storage:           other,
		Tracker:           func(*Context) (string, error) { return "x", nil },
		DisableHeaders:    true,
		AfterDependencies: true,
	})
	if len(over.Quotas) != 0 {
		t.Error("an explicitly empty quota list must replace the inherited one")
	}
	if over.Storage != other {
		t.Error("the narrower storage did not win")
	}
	if !over.DisableHeaders || !over.AfterDependencies || !over.FailOpen {
		t.Errorf("flags = %+v, want the narrower ones set and the inherited one kept", over)
	}
}

func TestResetSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reset time.Duration
		want  int
	}{
		{reset: -time.Second, want: 0},
		{reset: 0, want: 0},
		{reset: time.Millisecond, want: 1},
		{reset: time.Second, want: 1},
		{reset: 1500 * time.Millisecond, want: 2},
		{reset: time.Minute, want: 60},
	}
	for _, tc := range cases {
		if got := resetSeconds(tc.reset); got != tc.want {
			t.Errorf("resetSeconds(%s) = %d, want %d", tc.reset, got, tc.want)
		}
	}
}

func TestWindowSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		window time.Duration
		want   int
	}{
		{window: time.Millisecond, want: 1},
		{window: time.Second, want: 1},
		{window: 90 * time.Second, want: 90},
		{window: time.Hour, want: 3600},
	}
	for _, tc := range cases {
		if got := windowSeconds(tc.window); got != tc.want {
			t.Errorf("windowSeconds(%s) = %d, want %d", tc.window, got, tc.want)
		}
	}
}

func TestContainsStorage(t *testing.T) {
	t.Parallel()
	first, second := newRecordingStorage(), newRecordingStorage()
	memory := NewMemoryRateLimitStorage(MemoryRateLimitOptions{})
	list := []RateLimitStorage{first, memory}

	if !containsStorage(list, first) {
		t.Error("containsStorage() = false for a storage in the list")
	}
	if containsStorage(list, second) {
		t.Error("containsStorage() = true for a different storage of the same type")
	}
	if containsStorage(nil, first) {
		t.Error("containsStorage(nil, ...) = true")
	}
	if containsStorage(list, nil) {
		t.Error("containsStorage() = true for a nil storage")
	}
}

func TestRateLimitLayersFieldByField(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	opts := quietOptions()
	// The application names the storage and the quotas; the option given to
	// New names the tracker; the router narrows the quotas; the route narrows
	// nothing at all and should still inherit every one of those.
	opts.RateLimit = RateLimitOptions{
		Storage: storage,
		Quotas:  []Quota{{Name: "app-wide", Window: time.Minute, Limit: 100}},
	}
	app := New(opts, WithRateLimit(RateLimitOptions{
		Tracker: func(ctx *Context) (string, error) { return "tenant:" + ctx.Header("X-Tenant"), nil },
	}))
	scoped := NewRouter(WithRateLimit(RateLimitOptions{
		Quotas: []Quota{{Name: "scoped", Window: time.Minute, Limit: 2}},
	}))
	scoped.Get("/scoped", pong)
	app.Get("/plain", pong)
	app.Include(scoped)
	mustBuild(t, app)

	send := func(target, tenant string) int {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("X-Tenant", tenant)
		return doRequest(t, app, req).Code
	}
	for range 2 {
		if got := send("/scoped", "one"); got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
	}
	if got := send("/scoped", "one"); got != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the router's own quota to be the one enforced", got)
	}
	if got := send("/scoped", "two"); got != http.StatusOK {
		t.Errorf("status = %d, want the inherited tracker to give a second tenant its own budget", got)
	}
	if got := send("/plain", "one"); got != http.StatusOK {
		t.Errorf("status = %d, want the route outside the router to keep the application's quota", got)
	}
	for _, key := range storage.seenKeys() {
		if !strings.HasPrefix(key, "tenant:") {
			t.Fatalf("key %q, want the inherited tracker to have produced every key", key)
		}
	}
	if storage.calls() == 0 {
		t.Error("the inherited storage counted nothing")
	}
}
