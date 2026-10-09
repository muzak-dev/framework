package muzak

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// TestWebSocketHandlerContextEndsWithTheConnection pins that a handler waiting
// on its context, rather than on a read, is released when its connection ends.
// A hijacked connection's request context is otherwise cancelled only once the
// handler returns, so such a handler outlived its peer, the server's own close
// at shutdown, and the components stopped after it.
func TestWebSocketHandlerContextEndsWithTheConnection(t *testing.T) {
	t.Parallel()
	conns := make(chan *WSConn, 1)
	causes := make(chan error, 1)

	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			conns <- conn
			<-ctx.Context().Done()
			causes <- context.Cause(ctx.Context())
			return nil
		})
	})
	_ = dialWS(t, server.URL, "/ws")

	var conn *WSConn
	select {
	case conn = <-conns:
	case <-time.After(sseTestTimeout):
		t.Fatal("the handler never started")
	}
	// What shutdown does to a connection whose handler has not returned.
	ended := &WSCloseError{Status: WSStatusGoingAway, Reason: "the server is shutting down"}
	_ = conn.fail(ended)

	select {
	case cause := <-causes:
		if !errors.Is(cause, ended) {
			t.Errorf("the handler's context ended with %v, want the connection's own error", cause)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the handler's context outlived its connection")
	}
}

// TestWebSocketHandlerContextNeedsNoArrangement pins that the context a handler
// is given is the one its connection already watches, so that passing it to a
// read or a write costs nothing beyond the operation itself. The connection
// used to watch the request's context while the handler was handed a child of
// it, which no operation ever matched, so every read and write a handler made
// registered an arrangement of its own.
//
// It is not parallel: allocations are counted for the whole process, so
// anything else running at the same time would be counted here.
func TestWebSocketHandlerContextNeedsNoArrangement(t *testing.T) {
	type result struct {
		arranged          bool
		handler, baseline float64
	}
	results := make(chan result, 1)
	payload := []byte("x")

	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			// The frames are a few bytes each and nobody reads them while they
			// are counted, which keeps the peer's own reading out of the
			// measurement; the socket buffers them all.
			handler := testing.AllocsPerRun(50, func() { _ = conn.WriteBinary(ctx.Context(), payload) })
			baseline := testing.AllocsPerRun(50, func() { _ = conn.WriteBinary(context.Background(), payload) })
			results <- result{arranged: conn.arranged(ctx.Context()), handler: handler, baseline: baseline}
			return nil
		}, WithWebSocket(WSOptions{PingInterval: -1}))
	})
	_ = dialWS(t, server.URL, "/ws")

	select {
	case got := <-results:
		if !got.arranged {
			t.Error("the handler's context is not the one its connection watches")
		}
		if got.handler > got.baseline {
			t.Errorf("a write with the handler's context made %v allocations, want no more than the %v of one that can never be cancelled",
				got.handler, got.baseline)
		}
	case <-time.After(wsTestTimeout):
		t.Fatal("the handler never reported")
	}
}

// TestWebSocketHandlerContextEndsTheConnectionWithTheRequest pins that the
// request's own cancellation still ends the connection now that the context
// watched is the handler's, which is a child of the request's.
func TestWebSocketHandlerContextEndsTheConnectionWithTheRequest(t *testing.T) {
	t.Parallel()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	conn := newWSConn(server, bufio.NewReader(server), false, "", WSOptions{}.withDefaults())

	request, endRequest := context.WithCancel(context.Background())
	handler, cancel := context.WithCancelCause(request)
	defer cancel(nil)
	conn.cancelOnEnd(cancel)
	conn.watch(handler)
	defer conn.stopWatching()

	endRequest()
	select {
	case <-conn.done:
	case <-time.After(wsTestTimeout):
		t.Fatal("the connection outlived the request it belonged to")
	}
	if status, ok := WSCloseStatus(conn.failure()); !ok || status != WSStatusAbnormalClosure {
		t.Errorf("the connection ended with %v, want the request's end reported as an abnormal closure", conn.failure())
	}
}

// TestWebSocketWatchLeavesAnEndedConnectionToWhateverEndedIt pins that the
// watch does not close the transport when the connection's own end is what
// cancelled the context it watches. Whatever ended the connection closes the
// transport once it has finished with it, and closing it any sooner would cut
// short a close frame still on its way out.
func TestWebSocketWatchLeavesAnEndedConnectionToWhateverEndedIt(t *testing.T) {
	t.Parallel()
	transport := &nopReadWriteCloser{}
	conn := newWSConn(transport, bufio.NewReader(transport), false, "", WSOptions{}.withDefaults())
	handler, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	conn.cancelOnEnd(cancel)
	conn.watch(handler)
	defer conn.stopWatching()

	// What a close in progress does: the outcome is recorded, which cancels
	// the handler's context, and the transport is closed afterwards.
	ended := &WSCloseError{Status: WSStatusGoingAway, Reason: "on its way out"}
	_ = conn.record(ended)
	<-handler.Done()
	// The watch runs on a goroutine of its own once the context ends, so it
	// is given a moment to do the wrong thing.
	time.Sleep(20 * time.Millisecond)
	transport.mu.Lock()
	closed := transport.closed
	transport.mu.Unlock()
	if closed {
		t.Error("the watch closed the transport under a connection that was still closing")
	}
	if !errors.Is(conn.failure(), ended) {
		t.Errorf("the connection reports %v, want the outcome recorded first", conn.failure())
	}
}

// TestCancelOnEndAfterTheConnectionEnded pins that a connection which ended
// before the handler's context was tied to it cancels that context at once,
// rather than never.
func TestCancelOnEndAfterTheConnectionEnded(t *testing.T) {
	t.Parallel()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	conn := newWSConn(server, bufio.NewReader(server), false, "", WSOptions{})

	ended := errors.New("the peer went away")
	_ = conn.fail(ended)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	conn.cancelOnEnd(cancel)
	if !errors.Is(context.Cause(ctx), ended) {
		t.Fatalf("the context's cause is %v, want the error the connection ended with", context.Cause(ctx))
	}
}
