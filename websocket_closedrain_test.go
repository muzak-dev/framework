package muzak

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A server that refuses a message closes the connection with a status that says
// why, and the peer has to be able to read it. A peer that is still sending
// when the refusal goes out is the case that makes this hard: closing a socket
// with unread data in its receive buffer makes the kernel answer the peer's
// next segment with a reset, and a reset can destroy the close frame before the
// peer has read it. The tests here use a raw TCP client that sends first and
// reads afterwards, which is how a client with a message still streaming
// behaves, and check the close frame arrives whole and before the end of the
// stream.

const closeDrainReadLimit = 4096

// closeDrainApp serves an echo and a text-only route with a small read limit,
// so that a message over it is a few bytes of header away.
func closeDrainApp(t *testing.T, grace time.Duration) string {
	t.Helper()
	options := WithWebSocket(WSOptions{ReadLimit: closeDrainReadLimit, CloseGracePeriod: grace})
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, options)
		app.WS("/text", func(ctx *Context, _ Empty, conn *WSConn) error {
			for {
				if _, err := conn.ReadText(ctx.Context()); err != nil {
					return nil
				}
			}
		}, options)
	})
	return server.URL
}

// sendAll writes the prefix and then filler bytes, in chunks, until either the
// filler has been sent or the connection refuses more. A refusal is not an
// error here: it is what the old behaviour looked like from this side.
func sendAll(c *rawConn, prefix []byte, filler int) (written int) {
	_ = c.conn.SetWriteDeadline(time.Now().Add(wsTestTimeout))
	if _, err := c.conn.Write(prefix); err != nil {
		return 0
	}
	chunk := make([]byte, 16<<10)
	for written < filler {
		n, err := c.conn.Write(chunk[:min(len(chunk), filler-written)])
		written += n
		if err != nil {
			return written
		}
	}
	return written
}

// readClose reads the next frame and returns the status of the close frame it
// must be.
func readClose(c *rawConn) (uint16, error) {
	_, opcode, payload, err := c.tryRecv()
	if err != nil {
		return 0, err
	}
	if opcode != opClose || len(payload) < 2 {
		return 0, errors.New("the frame that arrived was not a close frame with a status")
	}
	return uint16(payload[0])<<8 | uint16(payload[1]), nil
}

// finishClose completes the closing handshake from the client's side, which is
// to stop sending and let the server see the end of the stream, and checks the
// server then closes without a reset.
func finishClose(c *rawConn) error {
	if tcp, ok := c.conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	_, _, _, err := c.tryRecv()
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func TestWebSocketOversizedMessageDeliversItsCloseFrameToAPeerStillSending(t *testing.T) {
	url := closeDrainApp(t, DefaultWSCloseGracePeriod)
	key := []byte{1, 2, 3, 4}

	const runs = 50
	delivered := 0
	for run := range runs {
		c := dialWS(t, url, "/ws")
		// A header declaring a payload one byte over the limit, then far more
		// bytes than the connection is ever going to read.
		sendAll(c, frameHeader(true, opText, closeDrainReadLimit+1, key), 256<<10)
		status, err := readClose(c)
		if err != nil || status != uint16(WSStatusMessageTooBig) {
			t.Logf("run %d: status %d, err %v", run, status, err)
			continue
		}
		if err := finishClose(c); err != nil {
			t.Logf("run %d: the close frame arrived but the connection then failed: %v", run, err)
			continue
		}
		delivered++
	}
	t.Logf("the 1009 was delivered in %d of %d runs", delivered, runs)
	if delivered != runs {
		t.Errorf("the close frame reached the peer in %d of %d runs, want all of them", delivered, runs)
	}
}

func TestWebSocketEveryRefusalDeliversItsCloseFrameToAPeerStillSending(t *testing.T) {
	url := closeDrainApp(t, DefaultWSCloseGracePeriod)
	key := []byte{1, 2, 3, 4}

	// An invalid text message is complete, so what follows it is a stream
	// position the server knows; the others break off part way through a frame.
	badUTF8 := append(frameHeader(true, opText, 2, key), 0xff^key[0], 0xfe^key[1])
	tests := []struct {
		name   string
		path   string
		prefix []byte
		want   WSStatus
	}{
		{"an unmasked frame", "/ws", frameHeader(true, opText, 5, nil), WSStatusProtocolError},
		{"a reserved bit", "/ws", append([]byte{0xC1, 0x85}, key...), WSStatusProtocolError},
		{"a reserved opcode", "/ws", append([]byte{0x83, 0x85}, key...), WSStatusProtocolError},
		{"a continuation with nothing to continue", "/ws", append(frameHeader(true, opContinuation, 5, key), 1, 2, 3, 4, 5), WSStatusProtocolError},
		{"text that is not UTF-8", "/ws", badUTF8, WSStatusInvalidFramePayload},
		{"binary sent to a text route", "/text", append(frameHeader(true, opBinary, 3, key), 1, 2, 3), WSStatusUnsupportedData},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for run := range 20 {
				c := dialWS(t, url, tc.path)
				sendAll(c, tc.prefix, 256<<10)
				status, err := readClose(c)
				if err != nil {
					t.Fatalf("run %d: the close frame never arrived: %v", run, err)
				}
				if status != uint16(tc.want) {
					t.Fatalf("run %d: status = %d, want %d", run, status, tc.want)
				}
				if err := finishClose(c); err != nil {
					t.Fatalf("run %d: the connection failed after the close frame: %v", run, err)
				}
			}
		})
	}
}

func TestWebSocketRefusedPeerCannotHoldTheConnectionPastTheGracePeriod(t *testing.T) {
	t.Parallel()
	const grace = 300 * time.Millisecond
	url := closeDrainApp(t, grace)
	key := []byte{1, 2, 3, 4}

	t.Run("a peer that trickles", func(t *testing.T) {
		t.Parallel()
		c := dialWS(t, url, "/ws")
		start := time.Now()
		sendAll(c, frameHeader(true, opText, closeDrainReadLimit+1, key), 0)
		cutOff := make(chan time.Duration, 1)
		go func() {
			// One byte at a time is far below any byte bound, so only the
			// clock can end this. The server closing is seen as a write that
			// fails.
			for {
				if _, err := c.conn.Write([]byte{0}); err != nil {
					cutOff <- time.Since(start)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		status, err := readClose(c)
		if err != nil || status != uint16(WSStatusMessageTooBig) {
			t.Fatalf("close frame: status %d, err %v", status, err)
		}
		select {
		case elapsed := <-cutOff:
			if elapsed < grace/2 || elapsed > grace+time.Second {
				t.Errorf("the peer was cut off after %s, want it near the %s grace period", elapsed, grace)
			}
		case <-time.After(grace + 3*time.Second):
			t.Fatal("the peer was never cut off")
		}
	})
}

// countingListener counts the bytes every connection it accepted has read, which
// is the one direct measure of how much a server reads from a peer.
type countingListener struct {
	net.Listener
	read atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, read: &l.read}, nil
}

type countingConn struct {
	net.Conn
	read *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// CloseWrite passes the half-close through, which embedding the interface would
// hide: the connection the server holds has to offer it, as a real one does.
func (c *countingConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return errors.ErrUnsupported
}

// Not parallel, for the reason given on the other tests that weigh the heap:
// the reading is process-wide.
func TestWebSocketPeerThatNeverStopsIsCutOffAndCostsNothing(t *testing.T) {
	const grace = 300 * time.Millisecond
	app := New(quietOptions())
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadLimit: closeDrainReadLimit, CloseGracePeriod: grace}))
	mustBuild(t, app)
	listener := &countingListener{}
	server := httptest.NewUnstartedServer(app)
	listener.Listener = server.Listener
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	c := dialWS(t, server.URL, "/ws")
	before := liveHeap()
	handshake := listener.read.Load()
	_ = c.conn.SetDeadline(time.Now().Add(wsTestTimeout))
	if _, err := c.conn.Write(frameHeader(true, opText, closeDrainReadLimit+1, []byte{1, 2, 3, 4})); err != nil {
		t.Fatalf("writing the header: %v", err)
	}
	start := time.Now()
	chunk := make([]byte, 64<<10)
	for {
		if _, err := c.conn.Write(chunk); err != nil {
			break
		}
		if time.Since(start) > grace+2*time.Second {
			t.Fatalf("still sending after %s", time.Since(start))
		}
	}
	if elapsed := time.Since(start); elapsed > grace+time.Second {
		t.Errorf("the server took %s to stop a peer that never does", elapsed)
	}
	// The bound, with room for what the buffered reader held before the wait
	// began and for the header itself.
	if read := listener.read.Load() - handshake; read > wsCloseDrainLimit+(64<<10) {
		t.Errorf("the server read %d bytes from a peer that would not stop, want at most %d", read, wsCloseDrainLimit)
	}
	if grown := liveHeap() - before; grown > 16<<20 {
		t.Errorf("the heap grew by %d bytes while a peer flooded a closing connection", grown)
	}
}

func TestWebSocketPeerThatAnswersTheCloseFrameEndsItAtOnce(t *testing.T) {
	t.Parallel()
	// A grace period this long would make any wait for the peer to go away
	// obvious, so a closing handshake that finishes quickly is one that ended
	// on the peer's own close frame.
	url := closeDrainApp(t, 10*time.Second)
	key := []byte{1, 2, 3, 4}
	badUTF8 := append(frameHeader(true, opText, 2, key), 0xff^key[0], 0xfe^key[1])
	oversized := append(frameHeader(true, opText, closeDrainReadLimit+1, key), make([]byte, closeDrainReadLimit+1)...)

	tests := []struct {
		name string
		send []byte
		want WSStatus
	}{
		{"after a message that was not UTF-8", badUTF8, WSStatusInvalidFramePayload},
		{"after a message that was too big", oversized, WSStatusMessageTooBig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := dialWS(t, url, "/ws")
			start := time.Now()
			c.sendRaw(tc.send)
			status, err := readClose(c)
			if err != nil || status != uint16(tc.want) {
				t.Fatalf("close frame: status %d, err %v", status, err)
			}
			c.send(true, opClose, closePayload(status, ""))
			if _, _, _, err := c.tryRecv(); err == nil {
				t.Fatal("the connection stayed open after the peer answered the close frame")
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("the closing handshake took %s, want about a round trip", elapsed)
			}
		})
	}

	t.Run("when the handler closes", func(t *testing.T) {
		t.Parallel()
		_, server := newWSTestApp(t, func(app *App) {
			app.WS("/bye", func(ctx *Context, _ Empty, conn *WSConn) error {
				return conn.Close(WSStatusNormalClosure, "bye")
			}, WithWebSocket(WSOptions{CloseGracePeriod: 10 * time.Second}))
		})
		c := dialWS(t, server.URL, "/bye")
		start := time.Now()
		status, err := readClose(c)
		if err != nil || status != uint16(WSStatusNormalClosure) {
			t.Fatalf("close frame: status %d, err %v", status, err)
		}
		c.send(true, opClose, closePayload(status, ""))
		if _, _, _, err := c.tryRecv(); err == nil {
			t.Fatal("the connection stayed open after the peer answered the close frame")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("the closing handshake took %s, want about a round trip", elapsed)
		}
	})
}

// The server stops sending as soon as its close frame is out, which the peer
// sees as the end of the stream while the transport is still open for it to
// answer on. Without it the peer would have no sign the server had finished
// until the grace period ran out.
func TestWebSocketHalfClosesAfterItsCloseFrame(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/wait", func(ctx *Context, _ Empty, conn *WSConn) error {
			_, _, _ = conn.Read(context.Background())
			return nil
		}, WithWebSocket(WSOptions{CloseGracePeriod: 10 * time.Second}))
	})
	c := dialWS(t, server.URL, "/wait")
	c.send(true, opText, []byte("go"))
	status, err := readClose(c)
	if err != nil || status != uint16(WSStatusNormalClosure) {
		t.Fatalf("close frame: status %d, err %v", status, err)
	}
	// The peer sends nothing, and the server is waiting for it to, so the end
	// of the stream can only be the server having closed its own side.
	start := time.Now()
	if _, _, _, err := c.tryRecv(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the close frame: %v, want the end of the stream", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the end of the stream took %s to arrive", elapsed)
	}
}
