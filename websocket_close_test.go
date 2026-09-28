package muzak

import (
	"bufio"
	"net"
	"testing"
	"time"
)

func TestWSConnSecondCloseWaitsForTheFirstCloseFrame(t *testing.T) {
	t.Parallel()
	// Shutdown and a returning handler both close a connection, and the second
	// one to arrive used to find the goodbye already claimed, report success
	// and close the transport under the frame the first was still writing, so
	// the peer saw a lost connection where it was being told the server was
	// going away.
	server, client := newPipeConns(t, WSOptions{CloseGracePeriod: -1})
	first := make(chan error, 1)
	go func() { first <- server.Close(WSStatusGoingAway, "server shutting down") }()

	// The pipe is unbuffered, so the first close blocks writing until the
	// client reads, which holds the window the second one has to fall into.
	deadline := time.Now().Add(wsTestTimeout)
	for len(server.writeSem) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first close never started writing")
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() { second <- server.Close(WSStatusNormalClosure, "") }()
	time.Sleep(20 * time.Millisecond)

	_, _, err := client.Read(t.Context())
	if status, ok := WSCloseStatus(err); !ok || status != WSStatusGoingAway {
		t.Fatalf("the peer read %v, want it told the server was going away", err)
	}
	for _, done := range []chan error{first, second} {
		select {
		case <-done:
		case <-time.After(wsTestTimeout):
			t.Fatal("a close never returned")
		}
	}
}

func TestWSConnAnswersAPeerCloseWhileAWriterIsBusy(t *testing.T) {
	t.Parallel()
	// The peer's close is recorded before it is echoed, so that a failed echo
	// cannot replace the status the peer sent. Recording ends the connection
	// as far as anything waiting on it can tell, and the echo used to treat
	// that as a reason not to wait for a writer that was mid-frame, so a peer
	// that closed while the server was sending got no answer at all.
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = serverSide.Close(); _ = clientSide.Close() })
	server := newWSConn(serverSide, bufio.NewReader(serverSide), false, "", WSOptions{CloseGracePeriod: -1}.withDefaults())
	peer := &rawConn{t: t, conn: clientSide, br: bufio.NewReader(clientSide)}

	// The writer blocks, because nothing reads the other end of the pipe yet.
	go func() { _ = server.WriteBinary(t.Context(), []byte("hello")) }()
	deadline := time.Now().Add(wsTestTimeout)
	for len(server.writeSem) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the writer never started")
		}
		time.Sleep(time.Millisecond)
	}
	read := make(chan error, 1)
	go func() {
		_, _, err := server.Read(t.Context())
		read <- err
	}()
	go peer.send(true, opClose, []byte{0x03, 0xE8}) // 1000

	// Nothing is read until the server has taken the close in, so that the
	// writer is still holding the write half when the echo wants it.
	for server.failure() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the server never took the close in")
		}
		time.Sleep(time.Millisecond)
	}
	// What the writer was sending arrives first, and the echo after it.
	if fin, opcode, payload := peer.recv(); !fin || opcode != opBinary || string(payload) != "hello" {
		t.Fatalf("first frame = (fin %v, opcode %#x, %q), want the message being written", fin, opcode, payload)
	}
	if _, opcode, payload := peer.recv(); opcode != opClose || len(payload) < 2 || payload[0] != 0x03 || payload[1] != 0xE8 {
		t.Fatalf("second frame = (opcode %#x, % x), want the close echoed with status 1000", opcode, payload)
	}
	select {
	case err := <-read:
		if status, ok := WSCloseStatus(err); !ok || status != WSStatusNormalClosure {
			t.Errorf("read error = %v, want the peer's own close status", err)
		}
	case <-time.After(wsTestTimeout):
		t.Fatal("the read never returned")
	}
}
