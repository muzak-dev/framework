package muzak

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A connection opened with WSDial reaches its peer through an HTTP client, and
// what that client hands back for a switched connection is the body of the
// response rather than a network connection, so there are no deadlines to set
// on it. The tests in this file play a server that stops cooperating and
// assert that the read and write timeouts hold regardless.

// wsStallBudget is how long an operation bounded by a timeout of a tenth of a
// second may take before the timeout is judged not to have applied. It is
// generous so that a loaded machine does not fail the test, and far short of
// forever, which is what the operation took before.
const wsStallBudget = wsTestTimeout / 2

// wsHugePayload is larger than any socket buffer between two ends of a
// loopback connection, so a write of it cannot finish unless the peer reads.
var wsHugePayload = make([]byte, 64<<20)

// readClientFrame reads one masked frame the client sent, returning its opcode
// and unmasked payload, or an error when none arrives in time.
func readClientFrame(conn net.Conn) (byte, []byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		return 0, nil, err
	}
	var head [6]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return 0, nil, err
	}
	// Only a control frame is expected here, so the length always fits in
	// the first byte and the masking key follows it directly.
	payload := make([]byte, head[1]&0x7F)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= head[2+i%4]
	}
	return head[0] & 0x0F, payload, nil
}

func TestWSDialHonoursTheWriteTimeout(t *testing.T) {
	t.Parallel()
	// The server never reads, so a large write fills every buffer between the
	// two ends and then waits. Without a deadline it waits forever.
	release := make(chan struct{})
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		<-release
	})
	t.Cleanup(func() { close(release) })

	t.Run("the connection's own timeout", func(t *testing.T) {
		t.Parallel()
		conn := dialClient(t, server.URL, WSDialOptions{WriteTimeout: 100 * time.Millisecond})
		start := time.Now()
		err := conn.WriteBinary(t.Context(), wsHugePayload)
		if waited := time.Since(start); waited > wsStallBudget {
			t.Errorf("the write took %v, want it bounded by the write timeout", waited)
		}
		// The error is the one a network connection's deadline produces: the
		// connection is lost, and the cause is a timeout a caller can test for
		// either way the standard library offers.
		if status, ok := WSCloseStatus(err); !ok || status != WSStatusAbnormalClosure {
			t.Fatalf("WriteBinary = %v, want the connection reported as lost", err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("WriteBinary = %v, want it to wrap os.ErrDeadlineExceeded", err)
		}
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Errorf("WriteBinary = %v, want a net.Error reporting a timeout", err)
		}
		if err := conn.WriteText(t.Context(), "after"); err == nil {
			t.Error("a write after the timeout succeeded, want the connection finished")
		}
	})

	t.Run("the caller's own deadline", func(t *testing.T) {
		t.Parallel()
		conn := dialClient(t, server.URL, WSDialOptions{WriteTimeout: time.Hour})
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := conn.WriteBinary(ctx, wsHugePayload)
		if waited := time.Since(start); waited > wsStallBudget {
			t.Errorf("the write took %v, want it bounded by the caller's deadline", waited)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WriteBinary = %v, want the caller's deadline reported", err)
		}
		if !strings.Contains(err.Error(), "writing a websocket message") {
			t.Errorf("error = %q, want it to name the operation", err)
		}
	})
}

func TestWSDialHonoursTheReadTimeout(t *testing.T) {
	t.Parallel()
	// The server begins a message and stops part way through it, which is
	// the dribble a read timeout exists to cut off.
	type frame struct {
		opcode  byte
		payload []byte
		err     error
	}
	received := make(chan frame, 1)
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte{0x80 | opBinary, 10, 'x'}); err != nil {
			received <- frame{err: err}
			return
		}
		opcode, payload, err := readClientFrame(conn)
		received <- frame{opcode, payload, err}
	})
	conn := dialClient(t, server.URL, WSDialOptions{ReadTimeout: 100 * time.Millisecond})

	start := time.Now()
	_, _, err := conn.Read(t.Context())
	if waited := time.Since(start); waited > wsStallBudget {
		t.Errorf("the read took %v, want it bounded by the read timeout", waited)
	}
	// The outcome is the one a served connection reports for the same abuse,
	// and the peer is told so before the transport goes.
	if status, ok := WSCloseStatus(err); !ok || status != WSStatusPolicyViolation {
		t.Fatalf("Read = %v, want the message refused for taking too long", err)
	}
	if !strings.Contains(err.Error(), "did not arrive") {
		t.Errorf("Read = %v, want it to say the message never arrived", err)
	}
	got := <-received
	if got.err != nil {
		t.Fatalf("the server did not receive a close frame: %v", got.err)
	}
	if got.opcode != opClose || len(got.payload) < 2 ||
		binary.BigEndian.Uint16(got.payload) != uint16(WSStatusPolicyViolation) {
		t.Errorf("the server received opcode %#x with % x, want a 1008 close", got.opcode, got.payload)
	}

	server.Close()
	assertNoGoroutineLeaks(t)
}

func TestWSDialTimeoutsDoNotOutliveTheirOperation(t *testing.T) {
	t.Parallel()
	// A timeout enforced by closing the transport must be disarmed the moment
	// its operation finishes. One left running would close a connection that
	// is merely quiet, which is what most connections are most of the time.
	_, server := echoApp(t)
	conn := dialClient(t, server.URL+"/ws", WSDialOptions{
		ReadTimeout:  30 * time.Millisecond,
		WriteTimeout: 30 * time.Millisecond,
	})
	for _, message := range []string{"first", "after a quiet spell"} {
		if err := conn.WriteText(t.Context(), message); err != nil {
			t.Fatalf("WriteText(%q) = %v", message, err)
		}
		got, err := conn.ReadText(t.Context())
		if err != nil || got != message {
			t.Fatalf("ReadText = %q, %v, want %q", got, err, message)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func TestWSDialTimeoutsCanBeDisabled(t *testing.T) {
	t.Parallel()
	// A negative timeout disables it, and a connection with none arms nothing
	// at all: the conversation is bounded only by the contexts it is given.
	_, server := echoApp(t)
	conn := dialClient(t, server.URL+"/ws", WSDialOptions{ReadTimeout: -1, WriteTimeout: -1})
	if err := conn.WriteText(t.Context(), "unbounded"); err != nil {
		t.Fatalf("WriteText = %v", err)
	}
	if got, err := conn.ReadText(t.Context()); err != nil || got != "unbounded" {
		t.Fatalf("ReadText = %q, %v, want the message echoed", got, err)
	}
	if conn.readExpiry.disarm() || conn.writeExpiry.disarm() {
		t.Error("a connection with its timeouts disabled armed an expiry")
	}
}

func TestWSExpiry(t *testing.T) {
	t.Parallel()

	t.Run("a deadline in the past fires at once", func(t *testing.T) {
		t.Parallel()
		var expiry wsExpiry
		fired := make(chan struct{})
		expiry.arm(time.Now().Add(-time.Second), func() { close(fired) })
		select {
		case <-fired:
		case <-time.After(wsTestTimeout):
			t.Fatal("an expired deadline did not fire")
		}
		if expiry.disarm() {
			t.Error("disarm reported a timer that had already fired")
		}
	})

	t.Run("disarming stops it", func(t *testing.T) {
		t.Parallel()
		var expiry wsExpiry
		var fired atomic.Bool
		expiry.arm(time.Now().Add(20*time.Millisecond), func() { fired.Store(true) })
		if !expiry.disarm() {
			t.Error("disarm did not report the armed timer")
		}
		if expiry.disarm() {
			t.Error("a second disarm reported a timer, want none left")
		}
		time.Sleep(60 * time.Millisecond)
		if fired.Load() {
			t.Error("a disarmed deadline fired")
		}
	})

	t.Run("arming again replaces it", func(t *testing.T) {
		t.Parallel()
		var expiry wsExpiry
		var first atomic.Bool
		second := make(chan struct{})
		expiry.arm(time.Now().Add(20*time.Millisecond), func() { first.Store(true) })
		expiry.arm(time.Now().Add(40*time.Millisecond), func() { close(second) })
		select {
		case <-second:
		case <-time.After(wsTestTimeout):
			t.Fatal("the replacing deadline did not fire")
		}
		if first.Load() {
			t.Error("a replaced deadline fired")
		}
	})
}
