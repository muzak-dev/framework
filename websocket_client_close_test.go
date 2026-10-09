package muzak

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// RFC 6455 section 7.1.1 asks the server to close the TCP connection first, so
// that the TIME_WAIT state is held by the server rather than by every client
// it has ever had, and asks a client to wait for that. A connection opened
// with WSDial reaches its peer through the body of an HTTP response, which has
// no deadlines to bound a wait with, and the client used to skip the wait
// altogether: it hung up the moment its own close frame was out, whatever
// WSDialOptions.CloseGracePeriod said. The tests in this file play a server
// that takes its time and check that the client lets it.

// wsServerHangUpDelay is how long the servers in this file take to answer, long
// enough that a client which did not wait is caught hanging up before then.
const wsServerHangUpDelay = 100 * time.Millisecond

// stillThere reports whether the client kept its end of the connection open
// for the given time, which is what a client waiting for the server to close
// first looks like: nothing arrives, and neither does the end of the stream.
func stillThere(conn net.Conn, wait time.Duration) bool {
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return false
	}
	var one [1]byte
	_, err := conn.Read(one[:])
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// wsServerClose is a close frame as a server sends it, unmasked.
func wsServerClose(status WSStatus) []byte {
	return binary.BigEndian.AppendUint16([]byte{0x80 | opClose, 2}, uint16(status))
}

func TestWSDialCloseWaitsForTheServerToHangUpFirst(t *testing.T) {
	t.Parallel()
	observed := make(chan error, 1)
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		opcode, _, err := readClientFrame(conn)
		if err != nil || opcode != opClose {
			observed <- fmt.Errorf("the server received opcode %#x (%w), want the client's close frame", opcode, err)
			return
		}
		if !stillThere(conn, wsServerHangUpDelay) {
			observed <- errors.New("the client hung up before the server had answered its close frame")
			return
		}
		// The server answers and then closes, which is the order the
		// specification asks for.
		_, _ = conn.Write(wsServerClose(WSStatusNormalClosure))
		observed <- nil
	})
	conn := dialClient(t, server.URL, WSDialOptions{CloseGracePeriod: wsTestTimeout})

	start := time.Now()
	if err := conn.Close(WSStatusNormalClosure, "done"); err != nil {
		t.Fatalf("Close = %v", err)
	}
	waited := time.Since(start)
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	if waited < wsServerHangUpDelay {
		t.Errorf("Close returned after %v, before the server had answered", waited)
	}
	// The server's answer and its hanging up are what end the wait, not the
	// grace period, which is far longer.
	if waited > wsStallBudget {
		t.Errorf("Close returned after %v, want it to end once the server hung up", waited)
	}
}

func TestWSDialCloseGivesUpAfterTheGracePeriod(t *testing.T) {
	t.Parallel()
	// The server reads the close frame and then neither answers nor hangs up,
	// so only the grace period can end the client's wait.
	release := make(chan struct{})
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _, _ = readClientFrame(conn)
		<-release
	})
	t.Cleanup(func() { close(release) })

	const grace = 100 * time.Millisecond
	conn := dialClient(t, server.URL, WSDialOptions{CloseGracePeriod: grace})
	start := time.Now()
	if err := conn.Close(WSStatusNormalClosure, ""); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if waited := time.Since(start); waited < grace || waited > wsStallBudget {
		t.Errorf("Close returned after %v, want it to wait out the %v grace period and no longer", waited, grace)
	}
}

func TestWSDialAnswersAServerCloseAndLetsTheServerHangUp(t *testing.T) {
	t.Parallel()
	observed := make(chan error, 1)
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write(wsServerClose(WSStatusGoingAway)); err != nil {
			observed <- err
			return
		}
		opcode, payload, err := readClientFrame(conn)
		if err != nil || opcode != opClose || len(payload) < 2 || binary.BigEndian.Uint16(payload) != uint16(WSStatusGoingAway) {
			observed <- fmt.Errorf("the server received opcode %#x with % x (%w), want its status echoed", opcode, payload, err)
			return
		}
		if !stillThere(conn, wsServerHangUpDelay) {
			observed <- errors.New("the client hung up as soon as it had answered, before the server could")
			return
		}
		observed <- nil
	})
	conn := dialClient(t, server.URL, WSDialOptions{CloseGracePeriod: wsTestTimeout})

	start := time.Now()
	_, _, err := conn.Read(t.Context())
	if status, ok := WSCloseStatus(err); !ok || status != WSStatusGoingAway {
		t.Fatalf("Read = %v, want the server's going away", err)
	}
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > wsStallBudget {
		t.Errorf("Read returned after %v, want it to end once the server hung up", waited)
	}
}

func TestWSDialWithoutAGracePeriodHangsUpAtOnce(t *testing.T) {
	t.Parallel()
	// A negative grace period asks for no wait at all, so the client hangs up
	// the moment it has answered rather than leaving that to the server.
	observed := make(chan error, 1)
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write(wsServerClose(WSStatusGoingAway)); err != nil {
			observed <- err
			return
		}
		if _, _, err := readClientFrame(conn); err != nil {
			observed <- fmt.Errorf("the client did not answer the close frame: %w", err)
			return
		}
		if stillThere(conn, wsStallBudget) {
			observed <- errors.New("the client waited for the server to hang up although it was told not to")
			return
		}
		observed <- nil
	})
	conn := dialClient(t, server.URL, WSDialOptions{CloseGracePeriod: -1})
	if _, _, err := conn.Read(t.Context()); err == nil {
		t.Fatal("Read succeeded, want the server's close reported")
	}
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
}
