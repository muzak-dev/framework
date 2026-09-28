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
