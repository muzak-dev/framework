package muzak

import (
	"net/http"
	"testing"
	"time"
)

// A handler that spends longer on each message than a ping and its timeout
// together is not a peer that has gone quiet, and a peer that answers every
// ping is not one either. The pongs wait in the socket for the next read.
//
// Every message is on its way before the handler reads the first. A read that
// waits for the peer is the one place the keepalive does judge, and there an
// honest peer still has to answer inside the timeout, which a client
// descheduled on a loaded machine can miss: that would be the keepalive doing
// its job, not the failure looked for here. Queued messages leave the handler
// waiting on nothing, so the only silence it shows the keepalive is its own.
//
// Reading a message that is already waiting still takes the handler's
// goroutine some time, and to the keepalive a read that has not yet reached
// the message looks like a read waiting on a silent peer. At a 30ms timeout a
// loaded machine held the goroutine up for that long, so the timeout is long
// beside a scheduling delay. The handler is busy with each message for twice
// an interval and a timeout, so that a ping is sent and judged while it is.
func TestKeepaliveLeavesAHandlerThatReadsSlowlyAlone(t *testing.T) {
	t.Parallel()
	const interval, timeout = 50 * time.Millisecond, 400 * time.Millisecond
	const messages = 3
	queued := make(chan struct{})
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			select {
			case <-queued:
			case <-ctx.Context().Done():
				return nil
			}
			for {
				message, err := conn.ReadText(ctx.Context())
				if err != nil {
					return nil
				}
				time.Sleep(2 * (interval + timeout))
				if err := conn.WriteText(ctx.Context(), message); err != nil {
					return nil
				}
			}
		}, WithWebSocket(WSOptions{PingInterval: interval, PongTimeout: timeout}))
	})
	conn := dialWS(t, server.URL, "/ws")
	for i := range messages {
		conn.text("message " + string(rune('a'+i)))
	}
	close(queued)

	for i := range messages {
		want := "message " + string(rune('a'+i))
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
