package muzak

import (
	"testing"
	"time"
)

// A peer that answers every ping and never says anything is alive as far as a
// keepalive can tell, and holds its slot for as long as it likes. The lifetime
// is what ends it: the connection is closed with 1001, going away, which is
// the status for a server that is done with a connection rather than one that
// failed, and a client reconnects.
func TestWebSocketMaxLifetimeClosesAConnectionThatAnswersPings(t *testing.T) {
	t.Parallel()
	app, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
			MaxLifetime:  300 * time.Millisecond,
			PingInterval: 40 * time.Millisecond,
			PongTimeout:  time.Second,
		}))
	})
	// The clock starts before the dial, because the lifetime starts during
	// it: started once the handshake had been read, it missed however long a
	// loaded machine took to hand the response over, and the connection
	// looked to have been closed early.
	start := time.Now()
	conn := dialWS(t, server.URL, "/ws")

	// The peer is a model citizen: it answers every ping, so the keepalive never
	// closes it.
	for {
		_, opcode, payload := conn.recv()
		switch opcode {
		case opPing:
			conn.send(true, opPong, payload)
			continue
		case opClose:
		default:
			t.Fatalf("frame with opcode %#x, want pings and then a close", opcode)
		}
		if status := closeStatus(payload); status != WSStatusGoingAway {
			t.Fatalf("close status = %d, want %d", status, WSStatusGoingAway)
		}
		conn.send(true, opClose, closePayload(uint16(WSStatusGoingAway), ""))
		break
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("the connection lasted %v, want about its 300ms lifetime", elapsed.Round(time.Millisecond))
	}
	conn.expectEOF()
	waitFor(t, func() bool { return app.websockets.count() == 0 }, "the connection to release its slot")
}

func closeStatus(payload []byte) WSStatus {
	if len(payload) < 2 {
		return 0
	}
	return WSStatus(uint16(payload[0])<<8 | uint16(payload[1]))
}

func TestWebSocketMaxLifetimeOptions(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.WebSocket = WSOptions{MaxLifetime: time.Hour}
	app := New(opts)
	inherits := app.WS("/inherits", wsEcho)
	shorter := app.WS("/shorter", wsEcho, WithWebSocket(WSOptions{MaxLifetime: time.Minute}))
	unbounded := app.WS("/unbounded", wsEcho, WithWebSocket(WSOptions{MaxLifetime: -1}))
	mustBuild(t, app)

	if got := inherits.websocket.opts.MaxLifetime; got != time.Hour {
		t.Errorf("an inheriting route resolved to %v, want the application's hour", got)
	}
	if got := shorter.websocket.opts.MaxLifetime; got != time.Minute {
		t.Errorf("a narrowed route resolved to %v, want a minute", got)
	}
	if got := unbounded.websocket.opts.MaxLifetime; got != 0 {
		t.Errorf("a route that removed the bound resolved to %v, want none", got)
	}
	if got := (WSOptions{}).withDefaults().MaxLifetime; got != 0 {
		t.Errorf("the zero value resolved to a lifetime of %v; a connection has none unless asked", got)
	}
}
