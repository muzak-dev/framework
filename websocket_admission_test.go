package muzak

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWebSocketOriginIsCheckedBeforeGuardsAndDependencies(t *testing.T) {
	t.Parallel()
	// A dependency that resolves a session may touch a store, slide an expiry
	// or consume a one-shot token, and a cross-site page makes the visitor's
	// browser send the cookie it resolves from. The handshake is going to be
	// refused for its origin, so nothing should run on its behalf first.
	var guards, deps atomic.Int64
	guard := func(*Context) error {
		guards.Add(1)
		return nil
	}
	session := func(*Context) (string, error) {
		deps.Add(1)
		return "session", nil
	}
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithDependencies(guard), Needs(session))
	})

	_, response := dialRaw(t, server.URL, "/ws", "Origin", "https://evil.example")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if guards.Load() != 0 || deps.Load() != 0 {
		t.Errorf("the refused handshake ran %d guards and %d dependencies, want none", guards.Load(), deps.Load())
	}

	// A handshake that is allowed still resolves both, in the usual order.
	conn := dialWS(t, server.URL, "/ws")
	conn.text("hello")
	conn.expectText("hello")
	if guards.Load() != 1 || deps.Load() != 1 {
		t.Errorf("the accepted handshake ran %d guards and %d dependencies, want one of each", guards.Load(), deps.Load())
	}
}

func TestWebSocketConnectionCapIsCheckedBeforeDependencies(t *testing.T) {
	t.Parallel()
	// Once the application is full every further handshake is going to be
	// answered 503, so it should not cost a dependency round trip to hear it.
	var deps atomic.Int64
	session := func(*Context) (string, error) {
		deps.Add(1)
		return "session", nil
	}
	opts := quietOptions()
	opts.WebSocket = WSOptions{MaxConnections: 1}
	app := New(opts)
	app.WS("/ws", wsEcho, Needs(session))
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	first := dialWS(t, server.URL, "/ws")
	first.text("hold")
	first.expectText("hold")
	if deps.Load() != 1 {
		t.Fatalf("dependencies run for the first connection = %d, want 1", deps.Load())
	}

	_, response := dialRaw(t, server.URL, "/ws")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Error("the refusal carries no Retry-After")
	}
	if deps.Load() != 1 {
		t.Errorf("the refused handshake resolved a dependency: %d calls in all, want 1", deps.Load())
	}
}
