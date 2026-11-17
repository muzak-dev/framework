package muzak

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sharedMCPSessionStore struct {
	mu      sync.Mutex
	records map[string][]byte
	lookups atomic.Int32
	starts  atomic.Int32
	stops   atomic.Int32
}

func newSharedMCPSessionStore() *sharedMCPSessionStore {
	return &sharedMCPSessionStore{records: make(map[string][]byte)}
}

func (s *sharedMCPSessionStore) Name() string                { return "mcp-sessions" }
func (s *sharedMCPSessionStore) Start(context.Context) error { s.starts.Add(1); return nil }
func (s *sharedMCPSessionStore) Stop(context.Context) error  { s.stops.Add(1); return nil }

func (s *sharedMCPSessionStore) Create(ctx context.Context, version string, owner [sha256.Size]byte, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id := rand.Text()
	data, err := EncodeMCPSessionRecord(MCPSessionRecord{Version: version, Owner: owner})
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.records[id] = data
	s.mu.Unlock()
	return id, nil
}

func (s *sharedMCPSessionStore) Lookup(ctx context.Context, id string, _ time.Duration) (MCPSessionRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return MCPSessionRecord{}, false, err
	}
	s.lookups.Add(1)
	s.mu.Lock()
	data, ok := s.records[id]
	s.mu.Unlock()
	if !ok {
		return MCPSessionRecord{}, false, nil
	}
	rec, err := DecodeMCPSessionRecord(data)
	if err != nil {
		return MCPSessionRecord{}, false, err
	}
	return rec, true, nil
}

func (s *sharedMCPSessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.records, id)
	s.mu.Unlock()
	return nil
}

func TestMCPSessionStoreMultiReplicaHandoffAndLifecycle(t *testing.T) {
	shared := newSharedMCPSessionStore()

	makeReplica := func() *App {
		app := New(quietOptions())
		app.Get("/ping", func(_ *Context, _ Empty) (string, error) { return "pong", nil }, MCPTool())
		app.MCP("/mcp", MCPOptions{SessionStore: shared})
		return mustBuild(t, app)
	}

	replicaA := makeReplica()
	replicaB := makeReplica()

	if err := replicaA.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("start lifecycle: %v", err)
	}
	if err := replicaA.StopLifecycle(context.Background()); err != nil {
		t.Fatalf("stop lifecycle: %v", err)
	}
	if shared.starts.Load() != 1 || shared.stops.Load() != 1 {
		t.Fatalf("lifecycle starts=%d stops=%d, want 1/1", shared.starts.Load(), shared.stops.Load())
	}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	initReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initBody))
	initReq.Header.Set("Content-Type", "application/json")
	initReq.Header.Set("Accept", "application/json")
	initRes := doRequest(t, replicaA, initReq)
	if initRes.Code != http.StatusOK {
		t.Fatalf("initialize status = %d: %s", initRes.Code, initRes.Body.String())
	}
	sid := initRes.Header().Get(HeaderMCPSessionID)
	if sid == "" {
		t.Fatal("missing Mcp-Session-Id header")
	}

	// Send tools/call to replica B with the session opened on replica A.
	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_ping","arguments":{}}}`
	callReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(callBody))
	callReq.Header.Set("Content-Type", "application/json")
	callReq.Header.Set("Accept", "application/json")
	callReq.Header.Set(HeaderMCPSessionID, sid)
	callRes := doRequest(t, replicaB, callReq)
	if callRes.Code != http.StatusOK || !strings.Contains(callRes.Body.String(), "pong") {
		t.Fatalf("replicaB call failed: status=%d body=%s", callRes.Code, callRes.Body.String())
	}

	// Delete session on replica A, verify replica B sees 404.
	delReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	delReq.Header.Set(HeaderMCPSessionID, sid)
	if delRes := doRequest(t, replicaA, delReq); delRes.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delRes.Code)
	}
	callReq2 := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(callBody))
	callReq2.Header.Set("Content-Type", "application/json")
	callReq2.Header.Set("Accept", "application/json")
	callReq2.Header.Set(HeaderMCPSessionID, sid)
	if gone := doRequest(t, replicaB, callReq2); gone.Code != http.StatusNotFound {
		t.Fatalf("after delete status = %d, want 404", gone.Code)
	}
}
