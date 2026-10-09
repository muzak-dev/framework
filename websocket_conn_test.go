package muzak

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// newPipeConns builds a pair of connections speaking to each other over an
// in-memory pipe.
//
// The pipe is what makes the parts of a connection that a real conversation
// never reaches testable: it has deadlines, it can be closed from either end,
// and it is unbuffered, so a write blocks until the other end reads and the
// contention a real network only produces under load happens on demand.
func newPipeConns(t *testing.T, opts WSOptions) (server, client *WSConn) {
	t.Helper()
	settings := opts.withDefaults()
	serverSide, clientSide := net.Pipe()
	server = newWSConn(serverSide, bufio.NewReader(serverSide), false, "", settings)
	client = newWSConn(clientSide, bufio.NewReader(clientSide), true, "", settings)
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientSide.Close()
	})
	return server, client
}

func TestWSConnAcquireGivesUp(t *testing.T) {
	t.Parallel()

	t.Run("when the caller's context is cancelled", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		conn.writeSem <- struct{}{}
		defer conn.release(conn.writeSem)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := conn.WriteText(ctx, "never sent")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteText = %v, want the cancellation reported", err)
		}
		if !strings.Contains(err.Error(), "waiting for the websocket connection") {
			t.Errorf("error = %q, want it to say what was being waited for", err)
		}
	})

	t.Run("when the connection fails while it waits", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		conn.writeSem <- struct{}{}
		defer conn.release(conn.writeSem)

		go func() {
			time.Sleep(10 * time.Millisecond)
			conn.fail(&WSCloseError{Status: WSStatusAbnormalClosure, Reason: "gone"})
		}()
		if err := conn.WriteText(t.Context(), "never sent"); !strings.Contains(err.Error(), "gone") {
			t.Fatalf("WriteText = %v, want the failure that ended the connection", err)
		}
	})

	t.Run("when the connection failed before the turn came", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		conn.writeSem <- struct{}{}
		released := make(chan struct{})
		go func() {
			time.Sleep(10 * time.Millisecond)
			conn.fail(&WSCloseError{Status: WSStatusAbnormalClosure, Reason: "gone first"})
			conn.release(conn.writeSem)
			close(released)
		}()
		err := conn.WriteText(t.Context(), "never sent")
		<-released
		if !strings.Contains(err.Error(), "gone first") {
			t.Fatalf("WriteText = %v, want the failure that ended the connection", err)
		}
		// The semaphore has to come back, or nothing could ever close the
		// connection afterwards.
		select {
		case conn.writeSem <- struct{}{}:
			conn.release(conn.writeSem)
		default:
			t.Error("the write semaphore was not released when the write gave up")
		}
	})

	t.Run("when the connection is already finished", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		conn.fail(&WSCloseError{Status: WSStatusNormalClosure, Reason: "over"})
		if err := conn.WriteText(t.Context(), "never sent"); !strings.Contains(err.Error(), "over") {
			t.Fatalf("WriteText = %v, want the recorded failure", err)
		}
		if _, _, err := conn.Read(t.Context()); !strings.Contains(err.Error(), "over") {
			t.Fatalf("Read = %v, want the recorded failure", err)
		}
		if _, err := conn.ReadText(t.Context()); err == nil {
			t.Error("ReadText succeeded on a finished connection")
		}
		if _, err := conn.ReadBinary(t.Context()); err == nil {
			t.Error("ReadBinary succeeded on a finished connection")
		}
		var into map[string]string
		if err := conn.ReadJSON(t.Context(), &into); err == nil {
			t.Error("ReadJSON succeeded on a finished connection")
		}
	})
}

func TestWSConnRefusesToWriteAfterClosing(t *testing.T) {
	t.Parallel()
	conn, _ := newPipeConns(t, WSOptions{})
	// The goodbye has been sent but the connection has not been torn down
	// yet, which is the window a write can still be attempted in.
	conn.mu.Lock()
	conn.sentClose = true
	conn.mu.Unlock()

	if err := conn.writable(); !errors.Is(err, errWSClosing) {
		t.Errorf("writable() = %v, want the connection reported as closing", err)
	}
	if err := conn.WriteText(t.Context(), "too late"); !errors.Is(err, errWSClosing) {
		t.Errorf("WriteText = %v, want the connection reported as closing", err)
	}
	if status, ok := WSCloseStatus(conn.WriteText(t.Context(), "too late")); !ok || status != WSStatusNormalClosure {
		t.Errorf("the refusal does not read as a closure, which is what a handler loop tests for")
	}
	// Sending the goodbye a second time is a no-op rather than a second frame.
	if err := conn.sendClose(WSStatusGoingAway, "again"); err != nil {
		t.Errorf("sendClose after the goodbye = %v, want it skipped", err)
	}
	// A writer that holds the write half cannot wait for a goodbye another
	// goroutine has claimed, since that goroutine is waiting for the very
	// half it holds; it is told nothing went out instead.
	if err := conn.sendCloseHolding(WSStatusGoingAway, "again"); !errors.Is(err, errWSClosing) {
		t.Errorf("sendCloseHolding after the goodbye = %v, want the connection reported as closing", err)
	}
}

func TestWSConnPingAfterCloseIsNotAnswered(t *testing.T) {
	t.Parallel()
	server, client := newPipeConns(t, WSOptions{})
	server.mu.Lock()
	server.sentClose = true
	server.mu.Unlock()

	go func() {
		// Nothing on this side ever reads, so a pong the server should not
		// have sent would block until its write timeout rather than arrive.
		_ = client.Ping(context.Background())
		_ = client.WriteText(context.Background(), "after the ping")
	}()
	typ, payload, err := server.Read(t.Context())
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if typ != WSText || string(payload) != "after the ping" {
		t.Errorf("read %s %q, want the message that followed the unanswered ping", typ, payload)
	}
}

func TestWSConnCloseFrameCannotBeSentToASilentPeer(t *testing.T) {
	t.Parallel()
	conn, _ := newPipeConns(t, WSOptions{WriteTimeout: 20 * time.Millisecond, CloseGracePeriod: -1})
	// Another goroutine holds the write half and never gives it back, which is
	// what a peer that stopped reading looks like from in here.
	conn.writeSem <- struct{}{}
	defer conn.release(conn.writeSem)

	err := conn.Close(WSStatusNormalClosure, "")
	if err == nil || !strings.Contains(err.Error(), "could not be sent within") {
		t.Fatalf("Close = %v, want the close frame reported as undeliverable", err)
	}
	// The connection is finished regardless, or a peer that stopped reading
	// would keep a handler alive forever.
	if conn.failure() == nil {
		t.Error("the connection is still usable after a close that could not be sent")
	}
}

func TestWSConnCloseFrameWaitsForTheWriteHalf(t *testing.T) {
	t.Parallel()
	// The write timeout bounds the wait being tested, so it is far beyond the
	// moment the write half comes free, however late that runs.
	conn, client := newPipeConns(t, WSOptions{WriteTimeout: wsTestTimeout, CloseGracePeriod: -1})
	conn.writeSem <- struct{}{}
	go func() {
		time.Sleep(10 * time.Millisecond)
		conn.release(conn.writeSem)
	}()
	go func() { _ = conn.Close(WSStatusNormalClosure, "after the wait") }()

	if _, _, err := client.Read(t.Context()); err == nil {
		t.Fatal("the client read a message where the close frame should have been")
	}
	status, ok := WSCloseStatus(client.failure())
	if !ok || status != WSStatusNormalClosure {
		t.Errorf("the client saw %v, want a normal closure", client.failure())
	}
}

func TestWSConnCloseGivesUpWhenTheConnectionIsAlreadyGone(t *testing.T) {
	t.Parallel()
	// The write timeout is the other way out of the wait, with an error of its
	// own, so it is set far beyond the failure below. At 50ms a loaded machine
	// that ran the failing goroutine late let the timeout win, which says
	// nothing about whether the failure ends the wait.
	conn, _ := newPipeConns(t, WSOptions{WriteTimeout: wsTestTimeout})
	conn.writeSem <- struct{}{}
	go func() {
		time.Sleep(10 * time.Millisecond)
		conn.fail(&WSCloseError{Status: WSStatusAbnormalClosure, Reason: "vanished"})
	}()
	if err := conn.sendClose(WSStatusNormalClosure, ""); !strings.Contains(err.Error(), "vanished") {
		t.Fatalf("sendClose = %v, want the failure that ended the connection", err)
	}
	conn.release(conn.writeSem)
}

func TestWSConnWriteFailuresEndTheConnection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload []byte
	}{
		{name: "a small message written in one go", payload: []byte("small")},
		{name: "a large message written as header and body", payload: make([]byte, wsScratchSize+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, client := newPipeConns(t, WSOptions{})
			// Nothing will ever read the other end, so the unbuffered pipe
			// leaves the write to the deadline.
			_ = client.rwc.Close()
			err := server.WriteBinary(t.Context(), tc.payload)
			status, ok := WSCloseStatus(err)
			if !ok || status != WSStatusAbnormalClosure {
				t.Fatalf("WriteBinary = %v, want the connection reported as lost", err)
			}
			if server.failure() == nil {
				t.Error("a half written frame left the connection usable")
			}
		})
	}
}

func TestWSConnWriteReportsTheCallersDeadline(t *testing.T) {
	t.Parallel()
	server, _ := newPipeConns(t, WSOptions{WriteTimeout: time.Minute})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	// Nothing reads the other end of the pipe, so the write waits until the
	// caller's own deadline, which is the earlier of the two.
	err := server.WriteText(ctx, "into the void")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WriteText = %v, want the caller's deadline reported", err)
	}
	if !strings.Contains(err.Error(), "writing a websocket message") {
		t.Errorf("error = %q, want it to name the operation", err)
	}
}

func TestWSConnWriteCancelledWithDeadlines(t *testing.T) {
	t.Parallel()
	// Nothing reads the other end of the pipe, so the write waits until the
	// cancellation reaches it through the deadline the connection sets.
	server, _ := newPipeConns(t, WSOptions{WriteTimeout: time.Minute})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := server.WriteText(ctx, "into the void")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteText = %v, want the cancellation reported", err)
	}
	if server.failure() == nil {
		t.Error("a write cut off part way left the connection usable")
	}
}

func TestWSConnClientMasksALargeMessageInChunks(t *testing.T) {
	t.Parallel()
	server, client := newPipeConns(t, WSOptions{ReadLimit: 1 << 20})
	payload := make([]byte, 3*wsScratchSize+7)
	for i := range payload {
		payload[i] = byte(i * 5)
	}
	go func() { _ = client.WriteBinary(context.Background(), payload) }()

	typ, got, err := server.Read(t.Context())
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if typ != WSBinary || string(got) != string(payload) {
		t.Errorf("the message did not survive being masked in chunks")
	}
}

func TestWSConnTruncatedFrames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		frame []byte
	}{
		{
			name:  "a data frame whose payload never arrives",
			frame: frameHeader(true, opText, 10, []byte{1, 2, 3, 4}),
		},
		{
			name:  "a control frame whose payload never arrives",
			frame: frameHeader(true, opPing, 10, []byte{1, 2, 3, 4}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, client := newPipeConns(t, WSOptions{})
			go func() {
				_, _ = client.rwc.Write(tc.frame)
				_ = client.rwc.Close()
			}()
			_, _, err := server.Read(t.Context())
			status, ok := WSCloseStatus(err)
			if !ok || status != WSStatusAbnormalClosure {
				t.Fatalf("Read = %v, want the connection reported as lost", err)
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				t.Errorf("error = %v, want the truncation reachable underneath", err)
			}
		})
	}
}

func TestWSConnDeadlines(t *testing.T) {
	t.Parallel()

	// Each case is judged against a baseline taken inside its own subtest, not
	// against one captured here. The subtests are parallel, so they do not run
	// until this function returns, and any wall-clock time at all may pass in
	// between; a baseline from out here made the comparison a race against the
	// scheduler rather than a test of which deadline was chosen.
	//
	// The boundary is half a minute for the same reason. The question is only
	// ever whether the connection's minute-long timeout won or the caller's
	// one-second deadline did, and separating them by thirty seconds answers it
	// without depending on how promptly the subtest was scheduled.
	const boundary = 30 * time.Second

	cases := []struct {
		name         string
		writeTimeout time.Duration
		ctxDeadline  time.Duration
		want         func(chosen, base time.Time) bool
		describe     string
	}{
		{
			name:         "the connection's own timeout",
			writeTimeout: time.Minute,
			want:         func(chosen, base time.Time) bool { return chosen.After(base.Add(boundary)) },
			describe:     "a deadline about a minute away",
		},
		{
			name:         "the caller's earlier deadline",
			writeTimeout: time.Minute,
			ctxDeadline:  time.Second,
			want:         func(chosen, base time.Time) bool { return chosen.Before(base.Add(boundary)) },
			describe:     "the caller's own deadline",
		},
		{
			name:         "no timeout and no deadline",
			writeTimeout: -1,
			want:         func(chosen, base time.Time) bool { return chosen.IsZero() },
			describe:     "no deadline at all",
		},
		{
			name:         "no timeout but a deadline",
			writeTimeout: -1,
			ctxDeadline:  time.Second,
			want: func(chosen, base time.Time) bool {
				return !chosen.IsZero() && chosen.Before(base.Add(boundary))
			},
			describe: "the caller's own deadline",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, _ := newPipeConns(t, WSOptions{WriteTimeout: tc.writeTimeout})
			ctx := context.Background()
			base := time.Now()
			if tc.ctxDeadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxDeadline)
				defer cancel()
			}
			if chosen := conn.deadline(ctx); !tc.want(chosen, base) {
				t.Errorf("deadline = %v, want %s", chosen, tc.describe)
			}
		})
	}

	bounded, _ := newPipeConns(t, WSOptions{WriteTimeout: 3 * time.Second})
	if got := bounded.closeTimeout(); got != 3*time.Second {
		t.Errorf("closeTimeout = %v, want the write timeout", got)
	}
	unbounded, _ := newPipeConns(t, WSOptions{WriteTimeout: -1})
	if got := unbounded.closeTimeout(); got != DefaultWSWriteTimeout {
		t.Errorf("closeTimeout = %v, want %v, because a teardown still has to finish", got, DefaultWSWriteTimeout)
	}
}

func TestWSContextFailure(t *testing.T) {
	t.Parallel()
	timeout := &net.OpError{Op: "read", Err: errTimeout{}}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := wsContextFailure(cancelled, io.EOF); !errors.Is(got, context.Canceled) {
		t.Errorf("wsContextFailure(cancelled) = %v, want the cancellation", got)
	}

	// The socket enforces the deadline, so it can report the timeout a moment
	// before the context notices it has passed.
	passed, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if got := wsContextFailure(passed, timeout); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("wsContextFailure(expired) = %v, want the deadline", got)
	}

	// A deadline still in the future means the timeout came from somewhere
	// else, such as the connection's own write timeout.
	future, stopFuture := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer stopFuture()
	if got := wsContextFailure(future, timeout); got != nil {
		t.Errorf("wsContextFailure(future) = %v, want the context left out of it", got)
	}
	if got := wsContextFailure(context.Background(), io.EOF); got != nil {
		t.Errorf("wsContextFailure(background) = %v, want nil", got)
	}
}

// errTimeout is an error that reports itself as a timeout, standing in for
// what a socket returns when a deadline passes.
type errTimeout struct{}

func (errTimeout) Error() string { return "i/o timeout" }
func (errTimeout) Timeout() bool { return true }
func (errTimeout) Unwrap() error { return os.ErrDeadlineExceeded }

func TestWSConnKeepaliveStops(t *testing.T) {
	t.Parallel()

	t.Run("when the connection ends", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		done := make(chan struct{})
		go func() {
			conn.keepalive(t.Context(), time.Millisecond, time.Minute)
			close(done)
		}()
		conn.fail(&WSCloseError{Status: WSStatusAbnormalClosure})
		waitForClose(t, done, "the keepalive did not stop when the connection did")
	})

	t.Run("when the request context ends", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			conn.keepalive(ctx, time.Hour, time.Minute)
			close(done)
		}()
		cancel()
		waitForClose(t, done, "the keepalive did not stop when the request did")
	})

	t.Run("when the ping cannot be sent", func(t *testing.T) {
		t.Parallel()
		conn, client := newPipeConns(t, WSOptions{WriteTimeout: 10 * time.Millisecond})
		_ = client.rwc.Close()
		done := make(chan struct{})
		go func() {
			conn.keepalive(t.Context(), time.Millisecond, time.Minute)
			close(done)
		}()
		waitForClose(t, done, "the keepalive did not stop when its ping could not be sent")
	})

	t.Run("when the connection ends while it waits for a pong", func(t *testing.T) {
		t.Parallel()
		conn, client := newPipeConns(t, WSOptions{})
		done := make(chan struct{})
		go func() {
			conn.keepalive(t.Context(), time.Millisecond, time.Hour)
			close(done)
		}()
		if _, _, err := client.Read(t.Context()); err == nil {
			t.Error("the client read a message where the ping should have been")
		}
		conn.fail(&WSCloseError{Status: WSStatusAbnormalClosure})
		waitForClose(t, done, "the keepalive did not stop when the connection ended")
	})

	t.Run("when the request ends while it waits for a pong", func(t *testing.T) {
		t.Parallel()
		conn, client := newPipeConns(t, WSOptions{})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			conn.keepalive(ctx, time.Millisecond, time.Hour)
			close(done)
		}()
		// The ping is read so that the keepalive gets as far as waiting for
		// its answer, and then the request ends underneath it.
		if _, _, err := client.Read(t.Context()); err == nil {
			t.Error("the client read a message where the ping should have been")
		}
		cancel()
		waitForClose(t, done, "the keepalive did not stop while waiting for a pong")
	})
}

// waitForClose fails the test unless the channel closes in time.
func waitForClose(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(wsTestTimeout):
		t.Fatal(message)
	}
}

func TestWSConnWatchesTheRequestContext(t *testing.T) {
	t.Parallel()

	t.Run("a cancelled request ends the connection", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		conn.watch(ctx)
		defer conn.stopWatching()
		if !conn.arranged(ctx) {
			t.Error("the watched context still asks for an arrangement of its own")
		}

		reading := make(chan error, 1)
		go func() {
			_, _, err := conn.Read(ctx)
			reading <- err
		}()
		cancel()
		select {
		case err := <-reading:
			if err == nil {
				t.Fatal("the read finished successfully after the request was cancelled")
			}
		case <-time.After(wsTestTimeout):
			t.Fatal("the read was not interrupted when the request was cancelled")
		}
		if conn.failure() == nil {
			t.Error("the connection outlived the request it belonged to")
		}
	})

	t.Run("a context that cannot be cancelled is not watched", func(t *testing.T) {
		t.Parallel()
		conn, _ := newPipeConns(t, WSOptions{})
		conn.watch(context.Background())
		if conn.watched != nil || conn.unwatch != nil {
			t.Error("an arrangement was made for a context that can never be cancelled")
		}
		// Undoing an arrangement that was never made is a no-op rather than a
		// nil call.
		conn.stopWatching()
		if !conn.arranged(context.Background()) {
			t.Error("a context that can never be cancelled asks for an arrangement")
		}
		derived, cancel := context.WithCancel(context.Background())
		defer cancel()
		if conn.arranged(derived) {
			t.Error("a context nothing is watching was treated as arranged")
		}
	})
}

func TestWSConnDrain(t *testing.T) {
	t.Parallel()

	t.Run("what the peer sent after the goodbye is read and dropped", func(t *testing.T) {
		t.Parallel()
		server, client := newPipeConns(t, WSOptions{CloseGracePeriod: wsTestTimeout})
		go func() {
			_, _ = client.rwc.Write(append(frameHeader(true, opText, 3, []byte{1, 2, 3, 4}), 0, 0, 0))
			_, _ = client.rwc.Write(frameHeader(true, opClose, 0, []byte{1, 2, 3, 4}))
		}()
		server.drain()
	})

	t.Run("a frame that promises more than it delivers ends it", func(t *testing.T) {
		t.Parallel()
		server, client := newPipeConns(t, WSOptions{CloseGracePeriod: wsTestTimeout})
		go func() {
			_, _ = client.rwc.Write(frameHeader(true, opText, 10, []byte{1, 2, 3, 4}))
			_ = client.rwc.Close()
		}()
		server.drain()
	})

	t.Run("another reader keeps it out", func(t *testing.T) {
		t.Parallel()
		server, _ := newPipeConns(t, WSOptions{CloseGracePeriod: wsTestTimeout})
		server.readSem <- struct{}{}
		defer server.release(server.readSem)
		// Nothing is sent, so this would wait out the grace period if it did
		// not give the read half up at once.
		server.drain()
	})
}

func TestWSConnWriteCancelledWithoutDeadlines(t *testing.T) {
	t.Parallel()
	// A connection reached through an HTTP client has no deadlines to set, so
	// a cancelled write can only be interrupted by closing the transport.
	transport := &blockedTransport{released: make(chan struct{})}
	conn := newWSConn(transport, bufio.NewReader(transport), true, "", WSOptions{}.withDefaults())
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := conn.WriteText(ctx, "into a transport that never drains")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteText = %v, want the cancellation reported", err)
	}
	if conn.failure() == nil {
		t.Error("a write cut off part way left the connection usable")
	}
}

// blockedTransport is a transport whose reads and writes never finish until it
// is closed, standing in for a peer that has stopped taking anything.
type blockedTransport struct {
	released chan struct{}
	once     sync.Once
}

func (b *blockedTransport) Read([]byte) (int, error) {
	<-b.released
	return 0, io.EOF
}

func (b *blockedTransport) Write([]byte) (int, error) {
	<-b.released
	return 0, io.ErrClosedPipe
}

func (b *blockedTransport) Close() error {
	b.once.Do(func() { close(b.released) })
	return nil
}

func TestWSConnDrainWithoutDeadlines(t *testing.T) {
	t.Parallel()
	// A connection reached through an HTTP client has no deadlines to bound a
	// drain with, so the grace period is enforced by closing the transport
	// when it runs out instead. A peer whose stream has already ended ends the
	// wait at once, and the transport is closed either way.
	transport := &nopReadWriteCloser{}
	conn := newWSConn(transport, bufio.NewReader(transport), true, "", WSOptions{}.withDefaults())
	conn.drain()
	if err := conn.Close(WSStatusNormalClosure, ""); err != nil {
		t.Errorf("Close = %v", err)
	}
	if !transport.closed {
		t.Error("the transport was left open")
	}
}

// nopReadWriteCloser stands in for a transport that is not a network
// connection, which is what a connection dialled through an HTTP client has.
type nopReadWriteCloser struct {
	mu     sync.Mutex
	closed bool
}

func (n *nopReadWriteCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (n *nopReadWriteCloser) Write(p []byte) (int, error) { return len(p), nil }

func (n *nopReadWriteCloser) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return nil
}

func TestWebSocketAcceptsFramesSentWithTheHandshake(t *testing.T) {
	t.Parallel()
	_, server := echoApp(t)
	parsed := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", parsed)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		t.Fatalf("setting a deadline: %v", err)
	}

	// The handshake and the first frame arrive together, which is what a
	// client is allowed to do and what would be lost if the bytes the upgrade
	// had already buffered were thrown away.
	handshake := "GET /ws HTTP/1.1\r\nHost: " + parsed + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testWSKey + "\r\n\r\n"
	raw := &rawConn{t: t, conn: conn}
	key := [4]byte{0x11, 0x22, 0x33, 0x44}
	message := []byte("sent with the handshake")
	masked := make([]byte, len(message))
	for i := range message {
		masked[i] = message[i] ^ key[i%4]
	}
	frame := append(frameHeader(true, opText, len(message), key[:]), masked...)
	if _, err := conn.Write(append([]byte(handshake), frame...)); err != nil {
		t.Fatalf("sending the handshake and the frame: %v", err)
	}

	raw.br = bufio.NewReader(conn)
	response, err := http.ReadResponse(raw.br, nil)
	if err != nil {
		t.Fatalf("reading the handshake response: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	raw.expectText("sent with the handshake")
}

func TestWebSocketRefusesAHandshakeWhileDraining(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/ws", wsEcho)
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	app.websockets.shutdown(time.Second, wsCloseGoingAway)
	_, response := dialRaw(t, server.URL, "/ws")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestWebSocketShutdownGivesUpOnAStuckHandler(t *testing.T) {
	t.Parallel()
	var registry liveRegistry[*WSConn]
	conn, _ := newPipeConns(t, WSOptions{CloseGracePeriod: -1, WriteTimeout: 10 * time.Millisecond})
	if registry.add(conn, "") != admitted {
		t.Fatal("the connection was not accepted")
	}
	// Nothing ever removes it, which is what a handler that ignores its closed
	// connection looks like from the register's side.
	start := time.Now()
	if closed := registry.shutdown(20*time.Millisecond, wsCloseGoingAway); closed != 1 {
		t.Errorf("shutdown closed %d connections, want 1", closed)
	}
	if elapsed := time.Since(start); elapsed > wsTestTimeout {
		t.Errorf("shutdown waited %v, want it to give up after its timeout", elapsed)
	}
}

func TestLiveRegistryPerKeyLimit(t *testing.T) {
	t.Parallel()
	var registry liveRegistry[*WSConn]
	registry.perKeyLimit = 2

	a, b, c := &WSConn{}, &WSConn{}, &WSConn{}
	if registry.add(a, "alice") != admitted {
		t.Fatal("alice's first entry was refused")
	}
	if registry.add(b, "alice") != admitted {
		t.Fatal("alice's second entry was refused")
	}
	if registry.add(c, "alice") != registryKeyFull {
		t.Fatal("a third entry for alice was admitted past her per-key limit")
	}

	// bob's own budget is untouched by alice holding hers.
	bob := &WSConn{}
	if registry.add(bob, "bob") != admitted {
		t.Fatal("bob's entry was refused by alice's budget")
	}

	// Removing one of alice's frees a slot for her specifically.
	registry.remove(a)
	if registry.add(c, "alice") != admitted {
		t.Fatal("alice was not readmitted after one of her entries closed")
	}

	// The process-wide limit, when also configured, is still checked first
	// and independently of who is asking.
	registry.limit = registry.count()
	if got := registry.admits("carol"); got != registryFull {
		t.Errorf("admits(carol) = %v, want registryFull once the process-wide limit is reached", got)
	}

	// An empty key is never counted against the per-key dimension at all,
	// which is what a caller with nothing to key on (or with the dimension
	// turned off) relies on.
	registry.limit = 0
	for range registry.perKeyLimit + 1 {
		if registry.add(&WSConn{}, "") != admitted {
			t.Fatal("an unkeyed entry was refused by a per-key limit that does not apply to it")
		}
	}
}

func TestWebSocketUpgradeRefusedAfterTheResponseStarted(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Something in the chain answered first, so there is no longer a
			// connection for the upgrade to take over.
			w.WriteHeader(http.StatusAccepted)
			next.ServeHTTP(w, r)
		})
	})
	app.WS("/ws", func(*Context, Empty, *WSConn) error {
		t.Error("the handler ran on a connection that was never upgraded")
		return nil
	})
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	_, response := dialRaw(t, server.URL, "/ws")
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want the answer that was already on the wire", response.StatusCode)
	}
}

func TestWebSocketHandshakeThatCannotBeSent(t *testing.T) {
	t.Parallel()
	// The response is made far larger than any socket will buffer and the
	// client is made to look away until the server has closed the connection,
	// so the handshake genuinely cannot be delivered. What matters is that the
	// server gives up and closes rather than holding open a connection whose
	// client will never hear back.
	const padding = 4 << 20
	const writeTimeout = 20 * time.Millisecond

	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Padding", strings.Repeat("a", padding))
			next.ServeHTTP(w, r)
		})
	})
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{WriteTimeout: writeTimeout}))
	mustBuild(t, app)
	server, sockets := startCountedServer(t, app)

	address := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		t.Fatalf("setting a deadline: %v", err)
	}
	handshake := "GET /ws HTTP/1.1\r\nHost: " + address + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testWSKey + "\r\n\r\n"
	if _, err := conn.Write([]byte(handshake)); err != nil {
		t.Fatalf("sending the handshake: %v", err)
	}

	// Nothing is read until the server has closed its end. Sleeping for a
	// multiple of the write timeout instead was not enough on a loaded machine,
	// where building the padded response alone can outlast the sleep: the
	// client then read before the deadline was even set, and the handshake
	// went out after all.
	waitFor(t, func() bool { return sockets.accepted.Load() == 1 && sockets.open.Load() == 0 },
		"the server to give up on the handshake and close")
	// Reading to the end returns only because the server closed the
	// connection, with whatever part of the response the socket held. Only a
	// timeout means it is still open. A reset is the server's close too, and
	// is not matched by its text, which differs on Windows.
	_, err = io.ReadAll(conn)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("reading what came back: %v", err)
	}
}

// fuzzTransport plays a fixed stream of bytes at a connection and swallows
// everything it writes back, so that a fuzzer can drive the read path without
// a peer to answer it.
type fuzzTransport struct {
	remaining []byte
}

func (t *fuzzTransport) Read(p []byte) (int, error) {
	if len(t.remaining) == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.remaining)
	t.remaining = t.remaining[n:]
	return n, nil
}

func (t *fuzzTransport) Write(p []byte) (int, error) { return len(p), nil }

func (t *fuzzTransport) Close() error { return nil }

// FuzzWebSocketRead plays arbitrary bytes at a connection as though a peer had
// sent them.
//
// Reading is where a WebSocket server meets whatever an unauthenticated client
// chooses to send, so what is asserted here is that nothing on that path can
// panic, allocate without bound or fail to terminate, whatever arrives.
func FuzzWebSocketRead(f *testing.F) {
	for _, seed := range [][]byte{
		{0x81, 0x83, 1, 2, 3, 4, 'a' ^ 1, 'b' ^ 2, 'c' ^ 3},
		{0x88, 0x82, 0, 0, 0, 0, 0x03, 0xE8},
		{0x01, 0x81, 0, 0, 0, 0, 'a', 0x80, 0x81, 0, 0, 0, 0, 'b'},
		{0x89, 0x80, 0, 0, 0, 0},
		{0x81, 0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0},
		{0xFF, 0xFF, 0xFF, 0xFF},
	} {
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, stream []byte, asClient bool) {
		transport := &fuzzTransport{remaining: stream}
		conn := newWSConn(transport, bufio.NewReader(transport), asClient, "",
			WSOptions{ReadLimit: 4 << 10}.withDefaults())

		// The stream is finite, so every message it can hold is read within a
		// frame's worth of iterations. Needing more would mean a frame that
		// consumed nothing, which is the loop that must not exist.
		for range len(stream) + 2 {
			typ, payload, err := conn.Read(context.Background())
			if err != nil {
				if conn.failure() == nil {
					t.Fatalf("Read reported %v but left the connection usable", err)
				}
				return
			}
			if typ != WSText && typ != WSBinary {
				t.Fatalf("Read returned the message type %d, which is not one a caller can act on", typ)
			}
			if int64(len(payload)) > conn.readLimit {
				t.Fatalf("Read returned %d bytes, more than the %d byte limit", len(payload), conn.readLimit)
			}
			if typ == WSText && !utf8.Valid(payload) {
				t.Fatal("Read returned a text message that is not valid UTF-8")
			}
		}
		t.Fatal("the stream was exhausted without the connection ending")
	})
}
