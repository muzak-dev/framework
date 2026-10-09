package muzak

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// TestWebSocketKeepaliveSparesAPeerStillSendingAMessage pins that a peer in the
// middle of sending a large message is not taken for a dead one. Its pong
// cannot overtake the frame it is part way through, so a keepalive that waited
// only for the pong closed a healthy peer whose message was simply taking
// longer than the pong timeout to arrive, while the bytes of it were still
// coming in, and long before the read timeout that bounds a message had run
// out. With the defaults that was any message of a mebibyte sent at under a
// hundred kilobytes a second.
func TestWebSocketKeepaliveSparesAPeerStillSendingAMessage(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
			PingInterval: 50 * time.Millisecond,
			PongTimeout:  200 * time.Millisecond,
			ReadTimeout:  wsTestTimeout,
		}))
	})
	conn := dialWS(t, server.URL, "/ws")

	payload := bytes.Repeat([]byte("a"), 64<<10)
	key := [4]byte{0x11, 0x22, 0x33, 0x44}
	frame := frameHeader(true, opBinary, len(payload), key[:])
	for i := range payload {
		frame = append(frame, payload[i]^key[i%4])
	}
	// Sixty pieces a few milliseconds apart take several pong timeouts to
	// send, and no gap between two of them comes near one.
	piece := len(frame)/60 + 1
	for start := 0; start < len(frame); start += piece {
		conn.sendRaw(frame[start:min(start+piece, len(frame))])
		time.Sleep(10 * time.Millisecond)
	}

	for {
		_, opcode, received := conn.recv()
		switch opcode {
		case opPing:
			conn.send(true, opPong, received)
		case opBinary:
			if !bytes.Equal(received, payload) {
				t.Fatalf("the echo of %d bytes does not match the %d sent", len(received), len(payload))
			}
			return
		case opClose:
			t.Fatalf("the connection was closed with %d %q while its peer was still sending a message",
				binary.BigEndian.Uint16(received), received[2:])
		}
	}
}

// TestWebSocketKeepaliveClosesAPeerThatStallsMidMessage pins the other half:
// what arrived of a message proves the peer was there when it arrived, not
// that it still is, so a peer that stops part way through a frame and answers
// nothing is closed by the keepalive as one that stops between messages is,
// without waiting for the read timeout.
func TestWebSocketKeepaliveClosesAPeerThatStallsMidMessage(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
			PingInterval: 20 * time.Millisecond,
			PongTimeout:  40 * time.Millisecond,
			ReadTimeout:  time.Hour,
		}))
	})
	conn := dialWS(t, server.URL, "/ws")
	conn.sendRaw(append(frameHeader(true, opText, 64, []byte{1, 2, 3, 4}), 'a'^1, 'b'^2))

	start := time.Now()
	for {
		_, opcode, received := conn.recv()
		if opcode != opClose {
			continue
		}
		if status := binary.BigEndian.Uint16(received); status != uint16(WSStatusPolicyViolation) ||
			!strings.Contains(string(received[2:]), "did not answer a ping") {
			t.Fatalf("close = %d %q, want the keepalive's policy violation", status, received[2:])
		}
		if waited := time.Since(start); waited > wsTestTimeout/2 {
			t.Errorf("the stalled peer was closed after %v, want it caught by the keepalive", waited)
		}
		return
	}
}

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
