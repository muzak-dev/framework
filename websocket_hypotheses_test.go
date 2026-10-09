package muzak

import (
	"net/http"
	"testing"
	"time"
)

// A handler that spends longer on each message than a ping and its timeout
// together is not a peer that has gone quiet, and a peer that answers every
// ping is not one either. The pongs wait in the socket for the next read.
func TestKeepaliveLeavesAHandlerThatReadsSlowlyAlone(t *testing.T) {
	t.Parallel()
	const interval, timeout = 20 * time.Millisecond, 30 * time.Millisecond
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			for {
				message, err := conn.ReadText(ctx.Context())
				if err != nil {
					return nil
				}
				time.Sleep(3 * (interval + timeout))
				if err := conn.WriteText(ctx.Context(), message); err != nil {
					return nil
				}
			}
		}, WithWebSocket(WSOptions{PingInterval: interval, PongTimeout: timeout}))
	})
	conn := dialWS(t, server.URL, "/ws")

	for i := range 5 {
		want := "message " + string(rune('a'+i))
		conn.text(want)
		for {
			_, opcode, payload := conn.recv()
			switch opcode {
			case opPing:
				// An honest peer answers every ping, however late its own
				// handler gets round to reading the pong.
				conn.send(true, opPong, payload)
			case opText:
				if string(payload) != want {
					t.Fatalf("message = %q, want %q", payload, want)
				}
			case opClose:
				t.Fatalf("the connection was closed (%s) while its handler was reading slowly", closeText(payload))
			}
			if opcode == opText {
				break
			}
		}
	}
}

func closeText(payload []byte) string {
	if len(payload) < 2 {
		return "no status"
	}
	return string(payload[2:])
}

// A route's options layer over the application's field by field, which is what
// makes the origin opt-outs sticky: a narrower scope that leaves a field at its
// zero value inherits it, and for a bool the zero value is what "not set"
// looks like, so a route that says nothing about the origin keeps an
// application's opt-out. Turning it back off takes EnforceOriginCheck, which
// TestWebSocketNarrowerScopeCanRestoreTheOriginCheck covers. The same layering
// leaves an application's AllowOriginFunc in force for a route that only lists
// origins. Both are what the documentation says layering does; these pin it.
func TestWebSocketOriginOptionsLayerFieldByField(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.WebSocket = WSOptions{InsecureSkipOriginCheck: true}
	app := New(opts)
	inherits := app.WS("/inherits", wsEcho)
	listed := app.WS("/listed", wsEcho, WithWebSocket(WSOptions{AllowedOrigins: []string{"https://app.example"}}))
	mustBuild(t, app)
	for name, route := range map[string]*Route{"inherits": inherits, "listed": listed} {
		if !route.websocket.opts.InsecureSkipOriginCheck {
			t.Errorf("%s: the application's opt-out was lost", name)
		}
	}

	opts = quietOptions()
	opts.WebSocket = WSOptions{AllowOriginFunc: func(*http.Request, string) bool { return true }}
	app = New(opts)
	only := app.WS("/only", wsEcho, WithWebSocket(WSOptions{AllowedOrigins: []string{"https://app.example"}}))
	mustBuild(t, app)
	if only.websocket.opts.AllowOriginFunc == nil {
		t.Error("a route that lists origins dropped the application's AllowOriginFunc")
	}
}
