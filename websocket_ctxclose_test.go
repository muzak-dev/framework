package muzak

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A handler that gives its peer a limited time for each message does it with a
// context of its own, and returns whatever error the read reports. That ended
// the connection without a close frame, so the peer saw an abnormal closure
// with no reason, and the handler's own deadline was logged as a failure. The
// tests in this file pin what happens instead: the peer is told, with a status
// that says whose time ran out, and the log says the connection ended.

// wsLoggedApp serves the given handler at /ws with a logger the test can read.
func wsLoggedApp(t *testing.T, handler WSHandler[Empty], opts ...RouteOption) (*httptest.Server, *syncBuffer) {
	t.Helper()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.WS("/ws", handler, opts...)
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return server, logs
}

func TestWebSocketIdleTimeoutTellsThePeerWhy(t *testing.T) {
	t.Parallel()
	reported := make(chan error, 1)
	server, logs := wsLoggedApp(t, func(ctx *Context, _ Empty, conn *WSConn) error {
		for {
			idle, cancel := context.WithTimeout(ctx.Context(), 50*time.Millisecond)
			_, _, err := conn.Read(idle)
			cancel()
			if err != nil {
				reported <- err
				return err
			}
		}
	})
	conn := dialWS(t, server.URL, "/ws")

	// The peer says nothing, so the handler's deadline passes between
	// messages, where the stream is at a frame boundary and a close frame can
	// follow whatever came before it.
	if reason := conn.expectClose(uint16(WSStatusPolicyViolation)); !strings.Contains(reason, "no message arrived") {
		t.Errorf("close reason = %q, want it to say the peer was idle for too long", reason)
	}
	// The peer answers, as a well-behaved one does, which ends the server's
	// wait for it.
	conn.send(true, opClose, closePayload(uint16(WSStatusPolicyViolation), ""))
	conn.expectEOF()

	if err := <-reported; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("read error = %v, want the handler's own deadline reported", err)
	}
	waitForLog(t, logs, "a websocket connection ended")
	if strings.Contains(logs.String(), "handler failed") {
		t.Errorf("the handler's own deadline was logged as a failure:\n%s", logs.String())
	}
}

func TestWebSocketCancelledReadTellsThePeer(t *testing.T) {
	t.Parallel()
	server, logs := wsLoggedApp(t, func(ctx *Context, _ Empty, conn *WSConn) error {
		cancellable, cancel := context.WithCancel(ctx.Context())
		time.AfterFunc(20*time.Millisecond, cancel)
		_, _, err := conn.Read(cancellable)
		return err
	})
	conn := dialWS(t, server.URL, "/ws")

	conn.expectClose(uint16(WSStatusGoingAway))
	conn.expectEOF()
	waitForLog(t, logs, "a websocket connection ended")
	if strings.Contains(logs.String(), "handler failed") {
		t.Errorf("the handler's own cancellation was logged as a failure:\n%s", logs.String())
	}
}

func TestWebSocketWriteWithAnEndedContextTellsThePeer(t *testing.T) {
	t.Parallel()
	reported := make(chan error, 1)
	server, logs := wsLoggedApp(t, func(ctx *Context, _ Empty, conn *WSConn) error {
		cancelled, cancel := context.WithCancel(ctx.Context())
		cancel()
		err := conn.WriteText(cancelled, "never sent")
		reported <- err
		return err
	})
	conn := dialWS(t, server.URL, "/ws")

	// Nothing of the message went out, so the stream is where it was and the
	// close frame is the next thing on it.
	conn.expectClose(uint16(WSStatusGoingAway))
	conn.expectEOF()
	if err := <-reported; !errors.Is(err, context.Canceled) {
		t.Errorf("WriteText = %v, want the cancellation reported", err)
	}
	waitForLog(t, logs, "a websocket connection ended")
	if strings.Contains(logs.String(), "handler failed") {
		t.Errorf("the handler's own cancellation was logged as a failure:\n%s", logs.String())
	}
}

func TestWebSocketHandlerReturningItsContextsErrorIsNotAFailure(t *testing.T) {
	t.Parallel()
	// A handler that waits on its context rather than on a read, and returns
	// the context's error when it ends, has not failed: its connection ended,
	// which is what its context is for.
	server, logs := wsLoggedApp(t, func(ctx *Context, _ Empty, conn *WSConn) error {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = conn.Close(WSStatusNormalClosure, "")
		}()
		<-ctx.Context().Done()
		return ctx.Context().Err()
	})
	conn := dialWS(t, server.URL, "/ws")
	conn.expectClose(uint16(WSStatusNormalClosure))
	conn.send(true, opClose, closePayload(uint16(WSStatusNormalClosure), ""))
	waitForLog(t, logs, "a websocket connection ended")
	if strings.Contains(logs.String(), "handler failed") {
		t.Errorf("a handler returning its ended context's error was logged as a failure:\n%s", logs.String())
	}
}

func TestWebSocketHandlerReturningAnUnrelatedDeadlineIsStillAFailure(t *testing.T) {
	t.Parallel()
	// A deadline that ran out somewhere else, such as on a query the handler
	// made, is a failure like any other: the connection had nothing to do
	// with it.
	server, logs := wsLoggedApp(t, func(*Context, Empty, *WSConn) error {
		return context.DeadlineExceeded
	})
	conn := dialWS(t, server.URL, "/ws")
	conn.expectClose(uint16(WSStatusInternalError))
	waitForLog(t, logs, "handler failed")
}
