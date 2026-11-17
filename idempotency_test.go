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

func TestIdempotencyReplayAndBypass(t *testing.T) {
	store := NewMemoryIdempotencyStore(16)
	var handlerCalls atomic.Int32
	var bgCalls atomic.Int32

	app := New(quietOptions())
	app.Post("/orders", func(ctx *Context, in struct {
		Item string `json:"item"`
	}) (map[string]string, error) {
		n := handlerCalls.Add(1)
		_ = ctx.AfterResponse(func(context.Context) {
			bgCalls.Add(1)
		})
		ctx.SetStatus(http.StatusCreated)
		return map[string]string{"item": in.Item, "attempt": strconv.Itoa(int(n))}, nil
	}, WithIdempotency(IdempotencyOptions{Store: store}))
	mustBuild(t, app)

	send := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set(DefaultIdempotencyHeader, key)
		}
		return doRequest(t, app, req)
	}

	r1 := send("k1", `{"item":"book"}`)
	if r1.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201 (%s)", r1.Code, r1.Body.String())
	}
	waitPoolIdle(t, app)

	r2 := send("k1", `{"item":"book"}`)
	if r2.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201", r2.Code)
	}
	if r2.Body.String() != r1.Body.String() {
		t.Fatalf("replay body = %q, want %q", r2.Body.String(), r1.Body.String())
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	if got := bgCalls.Load(); got != 1 {
		t.Fatalf("background calls = %d, want 1 (replay must not enqueue AfterResponse)", got)
	}

	// Same key with different payload -> 422
	r3 := send("k1", `{"item":"pen"}`)
	if r3.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch status = %d, want 422", r3.Code)
	}
}

func TestIdempotencyInFlightConflictAndExpiry(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }

	started := make(chan struct{})
	release := make(chan struct{})

	app := New(quietOptions())
	app.Post("/work", func(_ *Context, _ Empty) (string, error) {
		close(started)
		<-release
		return "done", nil
	}, WithIdempotency(IdempotencyOptions{Store: store, LockTTL: 5 * time.Second}))
	mustBuild(t, app)

	var wg sync.WaitGroup
	var first *httptest.ResponseRecorder
	wg.Go(func() {
		req := httptest.NewRequest(http.MethodPost, "/work", nil)
		req.Header.Set(DefaultIdempotencyHeader, "in-flight-1")
		first = doRequest(t, app, req)
	})

	<-started
	req2 := httptest.NewRequest(http.MethodPost, "/work", nil)
	req2.Header.Set(DefaultIdempotencyHeader, "in-flight-1")
	r2 := doRequest(t, app, req2)
	if r2.Code != http.StatusConflict {
		t.Fatalf("concurrent status = %d, want 409", r2.Code)
	}
	if r2.Header().Get(HeaderRetryAfter) != "1" {
		t.Fatalf("Retry-After = %q, want 1", r2.Header().Get(HeaderRetryAfter))
	}

	close(release)
	wg.Wait()
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}

	// Advance clock past 24h TTL -> key expires and runs again
	now = now.Add(25 * time.Hour)
	started = make(chan struct{})
	release = make(chan struct{})
	go func() {
		<-started
		close(release)
	}()
	req3 := httptest.NewRequest(http.MethodPost, "/work", nil)
	req3.Header.Set(DefaultIdempotencyHeader, "in-flight-1")
	r3 := doRequest(t, app, req3)
	if r3.Code != http.StatusOK {
		t.Fatalf("post-expiry status = %d, want 200", r3.Code)
	}
}

func TestIdempotencyAuthBeforeStoreAndPrincipalIsolation(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	var calls atomic.Int32

	appOpts := quietOptions()
	appOpts.SecuritySchemes = map[string]SecurityScheme{
		"key": APIKeyVerifier(APIKeyOptions{
			Name: "X-API-Key",
			Lookup: func(_ *Context, key string) (*APIKeyPrincipal, error) {
				switch key {
				case "secret-a":
					return &APIKeyPrincipal{ID: "tenant-a"}, nil
				case "secret-b":
					return &APIKeyPrincipal{ID: "tenant-b"}, nil
				default:
					return nil, nil
				}
			},
		}),
	}
	app := New(appOpts)
	app.Post("/items", func(ctx *Context, _ Empty) (map[string]string, error) {
		p := From[*APIKeyPrincipal](ctx)
		calls.Add(1)
		return map[string]string{"tenant": p.ID}, nil
	}, WithSecurity(Require("key")), WithIdempotency(IdempotencyOptions{Store: store, RequireKey: true}))
	mustBuild(t, app)

	// Unauthenticated request missing X-API-Key and missing Idempotency-Key must return 401 (auth runs first).
	unauth := httptest.NewRequest(http.MethodPost, "/items", nil)
	if res := doRequest(t, app, unauth); res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", res.Code)
	}

	// Missing Idempotency-Key when RequireKey=true -> 400
	noKey := httptest.NewRequest(http.MethodPost, "/items", nil)
	noKey.Header.Set("X-API-Key", "secret-a")
	if res := doRequest(t, app, noKey); res.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", res.Code)
	}

	// Same Idempotency-Key across two tenants must be isolated
	reqA := httptest.NewRequest(http.MethodPost, "/items", nil)
	reqA.Header.Set("X-API-Key", "secret-a")
	reqA.Header.Set(DefaultIdempotencyHeader, "shared-key")
	resA := doRequest(t, app, reqA)

	reqB := httptest.NewRequest(http.MethodPost, "/items", nil)
	reqB.Header.Set("X-API-Key", "secret-b")
	reqB.Header.Set(DefaultIdempotencyHeader, "shared-key")
	resB := doRequest(t, app, reqB)

	if !strings.Contains(resA.Body.String(), "tenant-a") || !strings.Contains(resB.Body.String(), "tenant-b") {
		t.Fatalf("tenant isolation failed: A=%s B=%s", resA.Body.String(), resB.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

type failingIdempotencyStore struct {
	err     error
	deletes atomic.Int32
}

func (s *failingIdempotencyStore) Acquire(context.Context, string, [32]byte, time.Duration) ([]byte, IdempotencyState, error) {
	return nil, IdempotencyNew, s.err
}
func (s *failingIdempotencyStore) Complete(context.Context, string, []byte, time.Duration) error {
	return s.err
}
func (s *failingIdempotencyStore) Delete(context.Context, string) error {
	s.deletes.Add(1)
	return nil
}

func TestIdempotencyStoreFailureAndOversizeAnd5xxDelete(t *testing.T) {
	failing := &failingIdempotencyStore{err: errors.New("down")}
	appClosed := New(quietOptions())
	appClosed.Post("/closed", func(_ *Context, _ Empty) (string, error) { return "ok", nil },
		WithIdempotency(IdempotencyOptions{Store: failing}))
	mustBuild(t, appClosed)
	req := httptest.NewRequest(http.MethodPost, "/closed", nil)
	req.Header.Set(DefaultIdempotencyHeader, "k")
	if res := doRequest(t, appClosed, req); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed status = %d, want 503", res.Code)
	}

	appOpen := New(quietOptions())
	appOpen.Post("/open", func(_ *Context, _ Empty) (string, error) { return "ok", nil },
		WithIdempotency(IdempotencyOptions{Store: failing, FailOpen: true}))
	mustBuild(t, appOpen)
	reqOpen := httptest.NewRequest(http.MethodPost, "/open", nil)
	reqOpen.Header.Set(DefaultIdempotencyHeader, "k")
	if res := doRequest(t, appOpen, reqOpen); res.Code != http.StatusOK {
		t.Fatalf("fail-open status = %d, want 200", res.Code)
	}

	// Oversized response deletes key instead of caching; 5xx deletes key instead of caching
	store := NewMemoryIdempotencyStore()
	var n atomic.Int32
	appBounds := New(quietOptions())
	appBounds.Post("/big", func(_ *Context, _ Empty) (string, error) {
		n.Add(1)
		return strings.Repeat("x", 64), nil
	}, WithIdempotency(IdempotencyOptions{Store: store, MaxBodySize: 10}))
	appBounds.Post("/err500", func(_ *Context, _ Empty) (string, error) {
		n.Add(1)
		return "", NewHTTPError(http.StatusInternalServerError, "boom")
	}, WithIdempotency(IdempotencyOptions{Store: store}))
	mustBuild(t, appBounds)

	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/big", nil)
		r.Header.Set(DefaultIdempotencyHeader, "big-key")
		if res := doRequest(t, appBounds, r); res.Code != http.StatusOK {
			t.Fatalf("big status = %d", res.Code)
		}
	}
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/err500", nil)
		r.Header.Set(DefaultIdempotencyHeader, "err-key")
		if res := doRequest(t, appBounds, r); res.Code != http.StatusInternalServerError {
			t.Fatalf("err500 status = %d", res.Code)
		}
	}
	if n.Load() != 4 {
		t.Fatalf("calls = %d, want 4 (neither oversized nor 500 should cache)", n.Load())
	}
}

func TestIdempotencyBuildValidation(t *testing.T) {
	appNilStore := New(quietOptions())
	appNilStore.Post("/x", func(_ *Context, _ Empty) (string, error) { return "ok", nil }, WithIdempotency())
	if err := appNilStore.Build(); err == nil || !strings.Contains(err.Error(), "IdempotencyOptions.Store") {
		t.Fatalf("expected nil store build error, got %v", err)
	}

	appSSE := New(quietOptions())
	appSSE.SSE("/events", func(_ *Context, _ Empty, _ *SSEStream[string]) error { return nil },
		WithIdempotency(IdempotencyOptions{Store: NewMemoryIdempotencyStore()}))
	if err := appSSE.Build(); err == nil || !strings.Contains(err.Error(), "SSE or WebSocket") {
		t.Fatalf("expected SSE build error, got %v", err)
	}
}

type managedIdempotencyStore struct {
	*MemoryIdempotencyStore
	starts atomic.Int32
	stops  atomic.Int32
}

func (m *managedIdempotencyStore) Name() string                { return "idem" }
func (m *managedIdempotencyStore) Start(context.Context) error { m.starts.Add(1); return nil }
func (m *managedIdempotencyStore) Stop(context.Context) error  { m.stops.Add(1); return nil }

func TestIdempotencyLifecycleAutoRegistration(t *testing.T) {
	store := &managedIdempotencyStore{MemoryIdempotencyStore: NewMemoryIdempotencyStore()}
	appOpts := quietOptions()
	appOpts.Idempotency = IdempotencyOptions{Store: store}
	app := New(appOpts)
	app.Post("/p", func(_ *Context, _ Empty) (string, error) { return "ok", nil })
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("start lifecycle: %v", err)
	}
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatalf("stop lifecycle: %v", err)
	}
	if store.starts.Load() != 1 || store.stops.Load() != 1 {
		t.Fatalf("starts=%d stops=%d, want 1/1", store.starts.Load(), store.stops.Load())
	}
}

type corruptIdempotencyStore struct{}

func (corruptIdempotencyStore) Acquire(context.Context, string, [32]byte, time.Duration) ([]byte, IdempotencyState, error) {
	return []byte{99, '{', '}'}, IdempotencyCompleted, nil
}
func (corruptIdempotencyStore) Complete(context.Context, string, []byte, time.Duration) error {
	return nil
}
func (corruptIdempotencyStore) Delete(context.Context, string) error { return nil }

func TestIdempotencyEdgePathsAndMCPToolScope(t *testing.T) {
	// SkipIdempotency + overlay + direct Unwrap + GET opt-in + MCP tools/call scoping
	store := NewMemoryIdempotencyStore(2)
	var calls atomic.Int32
	appOpts := quietOptions()
	appOpts.Idempotency = IdempotencyOptions{
		Store:                 store,
		Header:                "X-Idem",
		TTL:                   time.Minute,
		LockTTL:               5 * time.Second,
		MaxBodySize:           1024,
		StoreTimeout:          -1,
		DisablePrincipalScope: true,
	}
	app := New(appOpts)
	skipped := NewRouter(SkipIdempotency())
	skipped.Post("/skip", func(_ *Context, _ Empty) (string, error) {
		calls.Add(1)
		return "ok", nil
	})
	app.Include(skipped)
	app.Post("/unwrap", func(ctx *Context, _ Empty) (Empty, error) {
		_ = http.NewResponseController(ctx.ResponseWriter()).Flush()
		ctx.ResponseWriter().Header().Set("Set-Cookie", "a=1")
		_, _ = ctx.ResponseWriter().Write([]byte("raw"))
		return Empty{}, nil
	}, WithIdempotency(IdempotencyOptions{FailOpen: true, RequireKey: true, DisablePrincipalScope: true, Header: "X-Idem", TTL: time.Minute, LockTTL: 5 * time.Second, MaxBodySize: 1024, StoreTimeout: time.Second}))
	app.Post("/tool", func(_ *Context, in struct {
		A string `json:"a"`
	}) (map[string]string, error) {
		calls.Add(1)
		return map[string]string{"a": in.A}, nil
	}, MCPTool())
	app.MCP("/mcp", MCPOptions{}, WithIdempotency(IdempotencyOptions{Store: store}))
	mustBuild(t, app)

	// SkipIdempotency executes twice with same key
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/skip", nil)
		r.Header.Set("X-Idem", "k")
		if res := doRequest(t, app, r); res.Code != http.StatusOK {
			t.Fatalf("skip status = %d", res.Code)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("skip calls = %d, want 2", calls.Load())
	}

	// Unwrap + raw Write without WriteHeader + Set-Cookie exclusion on replay
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/unwrap", nil)
		r.Header.Set("X-Idem", "u1")
		res := doRequest(t, app, r)
		if res.Code != http.StatusOK || res.Body.String() != "raw" {
			t.Fatalf("unwrap status=%d body=%q", res.Code, res.Body.String())
		}
	}

	// Duplicate Idempotency-Key header -> 400
	dup := httptest.NewRequest(http.MethodPost, "/unwrap", nil)
	dup.Header.Add("X-Idem", "a")
	dup.Header.Add("X-Idem", "b")
	if res := doRequest(t, app, dup); res.Code != http.StatusBadRequest {
		t.Fatalf("dup header status = %d, want 400", res.Code)
	}

	// MCP tools/call through /mcp extracts tool name in scope
	initReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("Accept", "application/json")
	initRes := doRequest(t, app, initReq)
	if initRes.Code != http.StatusOK {
		t.Fatalf("mcp init status = %d: %s", initRes.Code, initRes.Body.String())
	}
	sid := initRes.Header().Get(HeaderMCPSessionID)
	mcpBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_tool","arguments":{"body":{"a":"1"}}}}`
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(mcpBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json")
		r.Header.Set(HeaderMCPSessionID, sid)
		r.Header.Set("X-Idem", "mcp-k1")
		if res := doRequest(t, app, r); res.Code != http.StatusOK {
			t.Fatalf("mcp idempotent status = %d: %s", res.Code, res.Body.String())
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (MCP tool call should replay)", calls.Load())
	}

	// Direct MemoryIdempotencyStore Complete without Acquire + LRU eviction + ctx cancel + Delete
	mem := NewMemoryIdempotencyStore(2)
	now := time.Unix(1_700_000_000, 0)
	mem.now = func() time.Time { return now }
	recBytes, err := EncodeIdempotencyRecord(IdempotencyRecord{Status: 200, Body: []byte("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Complete(context.Background(), "orphan", recBytes, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mem.Acquire(context.Background(), "k2", [32]byte{1}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mem.Acquire(context.Background(), "k3", [32]byte{2}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := mem.Delete(context.Background(), "k3"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := mem.Acquire(cancelled, "x", [32]byte{}, time.Second); err == nil {
		t.Fatal("expected cancelled error from Acquire")
	}
	if err := mem.Complete(cancelled, "x", recBytes, time.Second); err == nil {
		t.Fatal("expected cancelled error from Complete")
	}
	if err := mem.Delete(cancelled, "x"); err == nil {
		t.Fatal("expected cancelled error from Delete")
	}

	// Corrupt record handling (503 when FailOpen=false)
	appCorrupt := New(quietOptions())
	appCorrupt.Post("/corrupt", func(_ *Context, _ Empty) (string, error) { return "ok", nil },
		WithIdempotency(IdempotencyOptions{Store: corruptIdempotencyStore{}}))
	mustBuild(t, appCorrupt)
	rc := httptest.NewRequest(http.MethodPost, "/corrupt", nil)
	rc.Header.Set(DefaultIdempotencyHeader, "c1")
	if res := doRequest(t, appCorrupt, rc); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt status = %d, want 503", res.Code)
	}
}
