package muzak

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// handshakeStatus performs a handshake by hand with a Host of the caller's
// choosing, which is what dialRaw fixes, and reports the status it was
// answered with.
func handshakeStatus(t *testing.T, serverURL, host, origin string) int {
	t.Helper()
	address := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dialling %s: %v", address, err)
	}
	defer conn.Close()
	request := "GET /ws HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testWSKey + "\r\n"
	if origin != "" {
		request += "Origin: " + origin + "\r\n"
	}
	_ = conn.SetDeadline(time.Now().Add(wsTestTimeout))
	if _, err := conn.Write([]byte(request + "\r\n")); err != nil {
		t.Fatalf("sending the handshake: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the handshake response: %v", err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// The same-origin rule compares the Origin's host with the request's Host, and
// Host is whatever the client wrote. In a DNS-rebinding attack the attacker's
// page is served from a name that has been made to resolve to the victim's
// server, so both say the attacker's name and the handshake reads as
// same-origin. Without a list of the names the server answers to there is
// nothing for the rule to check the Host against, which AllowedHosts supplies.
func TestWebSocketSameOriginNeedsAHostTheServerAnswersTo(t *testing.T) {
	t.Parallel()
	echo := func(app *App) { app.WS("/ws", wsEcho) }

	t.Run("without a list the Host is believed", func(t *testing.T) {
		t.Parallel()
		_, server := newWSTestApp(t, echo)
		if got := handshakeStatus(t, server.URL, "rebound.example:8080", "http://rebound.example:8080"); got != http.StatusSwitchingProtocols {
			t.Errorf("status = %d, want 101: with no AllowedHosts the rule is the one it always was", got)
		}
	})

	t.Run("with a list only its names are the server's own", func(t *testing.T) {
		t.Parallel()
		_, server := newWSTestApp(t, echo, WithWebSocket(WSOptions{AllowedHosts: []string{"app.example", "APP.example:8443"}}))
		for _, tc := range []struct {
			host, origin string
			want         int
		}{
			{"app.example", "http://app.example", http.StatusSwitchingProtocols},
			{"app.example:8443", "https://app.example:8443", http.StatusSwitchingProtocols},
			{"rebound.example:8080", "http://rebound.example:8080", http.StatusForbidden},
			{"app.example.rebound.example", "http://app.example.rebound.example", http.StatusForbidden},
			// A named host with a foreign origin is still cross-origin.
			{"app.example", "http://rebound.example", http.StatusForbidden},
			// No Origin is no browser, so the list has nothing to say.
			{"rebound.example", "", http.StatusSwitchingProtocols},
		} {
			if got := handshakeStatus(t, server.URL, tc.host, tc.origin); got != tc.want {
				t.Errorf("Host %q with Origin %q: status = %d, want %d", tc.host, tc.origin, got, tc.want)
			}
		}
	})

	t.Run("an allowed origin does not need the Host to be listed", func(t *testing.T) {
		t.Parallel()
		_, server := newWSTestApp(t, echo, WithWebSocket(WSOptions{
			AllowedHosts:   []string{"app.example"},
			AllowedOrigins: []string{"https://partner.example"},
		}))
		if got := handshakeStatus(t, server.URL, "rebound.example", "https://partner.example"); got != http.StatusSwitchingProtocols {
			t.Errorf("status = %d, want 101 for an origin the route lists", got)
		}
	})
}
