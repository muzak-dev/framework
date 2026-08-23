package muzak

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

// wsMessageLimited builds an application whose WebSocket route bounds how fast
// a peer may send.
func wsMessageLimited(t *testing.T, limits []Quota, opts ...RouterOption) (*App, *httptest.Server) {
	t.Helper()
	return newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{MessageLimits: limits}))
	}, opts...)
}

func TestWSMessageLimitClosesAFloodingPeer(t *testing.T) {
	t.Parallel()
	_, server := wsMessageLimited(t, []Quota{{Name: "ws-messages", Window: time.Hour, Limit: 2}})
	conn := dialWS(t, server.URL, "/ws")

	for i := range 2 {
		conn.text("hello " + strconv.Itoa(i))
		conn.expectText("hello " + strconv.Itoa(i))
	}
	conn.text("one too many")
	reason := conn.expectClose(uint16(WSStatusPolicyViolation))
	if !strings.Contains(reason, "faster than this endpoint allows") {
		t.Errorf("close reason = %q, want it to say the peer was sending too fast", reason)
	}
	conn.expectEOF()
}

func TestWSMessageLimitLeavesAnUnlimitedRouteAlone(t *testing.T) {
	t.Parallel()
	_, server := wsMessageLimited(t, nil)
	conn := dialWS(t, server.URL, "/ws")
	for i := range 20 {
		conn.text(strconv.Itoa(i))
		conn.expectText(strconv.Itoa(i))
	}
}

func TestWSMessageLimitIsPerClient(t *testing.T) {
	t.Parallel()
	// Every connection from these tests arrives from the loopback address, so
	// a tracker keyed on a header is what gives two peers two budgets.
	_, server := wsMessageLimited(t,
		[]Quota{{Name: "ws-per-tenant", Window: time.Hour, Limit: 1}},
		WithRateLimit(RateLimitOptions{
			Tracker: func(ctx *Context) (string, error) { return "tenant:" + ctx.Header("X-Tenant"), nil },
		}))

	first := dialWS(t, server.URL, "/ws", "X-Tenant", "one")
	first.text("hello")
	first.expectText("hello")

	second := dialWS(t, server.URL, "/ws", "X-Tenant", "two")
	second.text("hello")
	second.expectText("hello")

	// The budget belongs to the client, not the connection, so opening a
	// second connection does not buy a second budget.
	third := dialWS(t, server.URL, "/ws", "X-Tenant", "one")
	third.text("hello")
	third.expectClose(uint16(WSStatusPolicyViolation))
}

func TestWSMessageLimitRefusesTheHandshakeWhenTheTrackerDoes(t *testing.T) {
	t.Parallel()
	_, server := wsMessageLimited(t,
		[]Quota{{Name: "ws-key", Window: time.Hour, Limit: 5}},
		WithRateLimit(RateLimitOptions{
			Tracker: func(ctx *Context) (string, error) {
				if key := ctx.Header("X-API-Key"); key != "" {
					return "apikey:" + key, nil
				}
				return "", NewHTTPError(http.StatusUnauthorized, "an API key is required")
			},
		}))

	_, response := dialRaw(t, server.URL, "/ws")
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("handshake status = %d, want 401; a tracker has no way to refuse once the connection is upgraded", response.StatusCode)
	}
}

func TestWSMessageLimitClosesWhenTheStorageCannotCount(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	logger, logs := captureLogger(t)
	app := New(AppOptions{Title: "Test API", Version: "1.0.0", Logger: logger},
		WithRateLimit(RateLimitOptions{Storage: storage}))
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
		MessageLimits: []Quota{{Name: "ws-storage", Window: time.Hour, Limit: 10}},
	}))
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	conn := dialWS(t, server.URL, "/ws")
	conn.text("fine")
	conn.expectText("fine")

	storage.fail(errors.New("connection refused"))
	conn.text("counted against a storage that is gone")
	conn.expectClose(uint16(WSStatusTryAgainLater))
	waitForLog(t, logs, "closing the websocket connection")
}

func TestWSMessageLimitCanFailOpen(t *testing.T) {
	t.Parallel()
	storage := newRecordingStorage()
	storage.fail(errors.New("connection refused"))
	logger, logs := captureLogger(t)
	app := New(AppOptions{Title: "Test API", Version: "1.0.0", Logger: logger},
		WithRateLimit(RateLimitOptions{Storage: storage, FailOpen: true}))
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
		MessageLimits: []Quota{{Name: "ws-open", Window: time.Hour, Limit: 1}},
	}))
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	conn := dialWS(t, server.URL, "/ws")
	for i := range 5 {
		conn.text(strconv.Itoa(i))
		conn.expectText(strconv.Itoa(i))
	}
	waitForLog(t, logs, "the websocket message was not counted")
}

func TestWSMessageLimitIsSkippedWithTheRest(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho,
			WithWebSocket(WSOptions{MessageLimits: []Quota{{Name: "ws-skipped", Window: time.Hour, Limit: 1}}}),
			SkipRateLimit())
	})
	conn := dialWS(t, server.URL, "/ws")
	for i := range 5 {
		conn.text(strconv.Itoa(i))
		conn.expectText(strconv.Itoa(i))
	}
}

func TestWSMessageLimitSharesTheApplicationStorage(t *testing.T) {
	t.Parallel()
	storage := newManagedStorage()
	app := New(quietOptions(), WithRateLimit(RateLimitOptions{
		Storage: storage,
		Quotas:  []Quota{{Name: "ws-handshakes", Window: time.Hour, Limit: 10}},
	}))
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
		MessageLimits: []Quota{{Name: "ws-messages", Window: time.Hour, Limit: 10}},
	}))
	mustBuild(t, app)

	ctx := context.Background()
	if err := app.StartLifecycle(ctx); err != nil {
		t.Fatalf("StartLifecycle() = %v", err)
	}
	if err := app.StopLifecycle(ctx); err != nil {
		t.Fatalf("StopLifecycle() = %v", err)
	}
	if starts, _ := storage.counts(); starts != 1 {
		t.Errorf("storage started %d times, want once for the request and message policies together", starts)
	}
}

func TestWSMessageLimitBuildErrors(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
		MessageLimits: []Quota{{Name: "bad name", Window: time.Hour, Limit: 1}},
	}))
	if message := buildError(t, app); !strings.Contains(message, "MessageLimits") {
		t.Errorf("build error = %q, want it to name the option at fault", message)
	}
}

func TestWSMessageLimitLeavesNothingRunning(t *testing.T) {
	// Each connection is a different client, so every one of them is closed
	// by the limiter rather than the first one spending the budget for all.
	_, server := wsMessageLimited(t,
		[]Quota{{Name: "ws-leak", Window: time.Hour, Limit: 2}},
		WithRateLimit(RateLimitOptions{
			Tracker: func(ctx *Context) (string, error) { return "tenant:" + ctx.Header("X-Tenant"), nil },
		}))
	for i := range 10 {
		conn := dialWS(t, server.URL, "/ws", "X-Tenant", strconv.Itoa(i))
		conn.text("one")
		conn.expectText("one")
		conn.text("two")
		conn.expectText("two")
		conn.text("three")
		conn.expectClose(uint16(WSStatusPolicyViolation))
		conn.expectEOF()
	}
	server.Close()
	assertNoGoroutineLeaks(t)
}
