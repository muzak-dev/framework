package muzak

import (
	"testing"
	"time"
)

// A pong is consumed by a read, so a handler that only writes never sees the
// answer to a ping. Closing its connection for that would close every healthy
// peer of a server that only pushes, now that keepalive is on by default.
func TestKeepaliveLeavesAHandlerThatIsNotReadingAlone(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			// Long enough for several unanswered pings and their timeouts.
			time.Sleep(300 * time.Millisecond)
			return conn.WriteText(ctx.Context(), "late")
		}, WithWebSocket(WSOptions{
			PingInterval: 20 * time.Millisecond,
			PongTimeout:  40 * time.Millisecond,
		}))
	})
	conn := dialWS(t, server.URL, "/ws")

	// The peer never answers a ping. It must still receive the message.
	for {
		_, opcode, payload := conn.recv()
		switch opcode {
		case opPing:
			continue
		case opText:
			if string(payload) != "late" {
				t.Fatalf("message = %q, want late", payload)
			}
			return
		case opClose:
			t.Fatal("the connection was closed for an unanswered ping while its handler was not reading")
		}
	}
}
