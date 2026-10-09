package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSocketOriginMustBeCanonical(t *testing.T) {
	t.Parallel()
	// The same-origin rule accepts what a browser sends, which is a scheme and
	// a host and nothing else. A value that merely contains the server's host
	// somewhere a lenient parser will find it is not an origin.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho)
	})
	host := strings.TrimPrefix(server.URL, "http://")

	for _, tc := range []struct {
		origin string
		status int
	}{
		{"http://" + host, http.StatusSwitchingProtocols},
		{"HTTP://" + strings.ToUpper(host), http.StatusSwitchingProtocols},
		{"https://" + host, http.StatusSwitchingProtocols},
		{"http://evil.example@" + host, http.StatusForbidden},
		{"http://" + host + "@evil.example", http.StatusForbidden},
		{"http://" + host + "/", http.StatusForbidden},
		{"http://" + host + "/x", http.StatusForbidden},
		{"http://" + host + "?x", http.StatusForbidden},
		{"http://" + host + "#x", http.StatusForbidden},
		{"//" + host, http.StatusForbidden},
		{host, http.StatusForbidden},
		{"null", http.StatusForbidden},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			t.Parallel()
			_, response := dialRaw(t, server.URL, "/ws", "Origin", tc.origin)
			if response.StatusCode != tc.status {
				t.Fatalf("Origin %q got status %d, want %d", tc.origin, response.StatusCode, tc.status)
			}
		})
	}
}

// TestWebSocketNarrowerScopeCanRestoreTheOriginCheck pins that the one origin
// setting which relaxes security can be taken back. Options layer field by
// field and a bool left false reads as "not set", so an application or router
// that set InsecureSkipOriginCheck used to leave every route beneath it open to
// any origin with no way for one of them to close again.
func TestWebSocketNarrowerScopeCanRestoreTheOriginCheck(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.WebSocket = WSOptions{InsecureSkipOriginCheck: true}
	app := New(opts)
	app.WS("/open", wsEcho)
	app.WS("/checked", wsEcho, WithWebSocket(WSOptions{EnforceOriginCheck: true}))
	// Asking for both at once is a contradiction, and the safe reading of it
	// is the one that keeps the check.
	app.WS("/both", wsEcho, WithWebSocket(WSOptions{EnforceOriginCheck: true, InsecureSkipOriginCheck: true}))

	strict := NewRouter()
	strict.WS("/inherits", wsEcho)
	strict.WS("/reopened", wsEcho, WithWebSocket(WSOptions{InsecureSkipOriginCheck: true}))
	app.Include(strict, WithPrefix("/strict"), WithWebSocket(WSOptions{EnforceOriginCheck: true}))
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/open", http.StatusSwitchingProtocols},
		{"/checked", http.StatusForbidden},
		{"/both", http.StatusForbidden},
		{"/strict/inherits", http.StatusForbidden},
		{"/strict/reopened", http.StatusSwitchingProtocols},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			_, response := dialRaw(t, server.URL, tc.path, "Origin", "https://evil.example")
			if response.StatusCode != tc.status {
				t.Fatalf("a foreign origin got status %d at %s, want %d", response.StatusCode, tc.path, tc.status)
			}
			// The check that came back is the ordinary one, so the server's own
			// origin is still welcome wherever it applies.
			own := "http://" + strings.TrimPrefix(server.URL, "http://")
			if _, response := dialRaw(t, server.URL, tc.path, "Origin", own); response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("the server's own origin got status %d at %s, want %d", response.StatusCode, tc.path, http.StatusSwitchingProtocols)
			}
		})
	}
}

func TestWebSocketRefusesMoreThanOneOrigin(t *testing.T) {
	t.Parallel()
	// Two Origin headers are two answers to one question, and a proxy that
	// merges or reorders them would have this end and the browser reading
	// different ones. Neither order is accepted.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho)
		app.WS("/open", wsEcho, WithWebSocket(WSOptions{InsecureSkipOriginCheck: true}))
	})
	host := strings.TrimPrefix(server.URL, "http://")
	own, evil := "http://"+host, "https://evil.example"

	for name, headers := range map[string][]string{
		"own then foreign": {"Origin", own, "Origin", evil},
		"foreign then own": {"Origin", evil, "Origin", own},
		"own twice":        {"Origin", own, "Origin", own},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, response := dialRaw(t, server.URL, "/ws", headers...)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
			}
		})
	}

	// A route that has turned the check off does not read the header at all.
	conn, response := dialRaw(t, server.URL, "/open", "Origin", own, "Origin", evil)
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d for a route that skips the origin check", response.StatusCode, http.StatusSwitchingProtocols)
	}
	_ = conn
}
