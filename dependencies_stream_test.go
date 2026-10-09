package muzak

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitForRelease waits for the named release to have run, for a stream whose
// release happens after the client has already seen it end.
func waitForRelease(t *testing.T, log *releaseLog, name string) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		log.mu.Lock()
		failure, ran := log.failures[name]
		log.mu.Unlock()
		if ran {
			return failure
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("release %s never ran; events: %s", name, log.String())
	return nil
}

// newServerFor serves a built application on a real socket and returns its
// URL.
func newServerFor(t *testing.T, app *App) string {
	t.Helper()
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return server.URL
}

type relEvent struct {
	Value string `json:"value"`
}

func TestAcquireOnAnEventStream(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.SSE("/done", func(ctx *Context, in struct{ Value Dep[relA] }, stream *SSEStream[relEvent]) error {
			return stream.Send(relEvent{Value: string(in.Value.Get())})
		}, acquireAs[relA](log, "done", nil))
		app.SSE("/held", func(_ *Context, _ Empty, stream *SSEStream[relEvent]) error {
			if err := stream.Send(relEvent{Value: "first"}); err != nil {
				return err
			}
			// Held open until the client leaves.
			<-stream.Context().Done()
			return stream.Err()
		}, acquireAs[relA](log, "held", nil))
		app.SSE("/release-fails", func(_ *Context, _ Empty, stream *SSEStream[relEvent]) error {
			return stream.Send(relEvent{Value: "sent"})
		}, acquireAs[relA](log, "fails", errors.New("stream release failed")))
	})

	t.Run("released with nil when the handler finishes", func(t *testing.T) {
		reader := openStream(t, server.URL, "/done")
		if got := nextEvent(t, reader).Data; got != `{"value":"done"}` {
			t.Errorf("event = %s, want the acquired value", got)
		}
		assertStreamEnded(t, reader)
		// Released before the stream was finished, so already done.
		if failure := log.failure(t, "done"); failure != nil {
			t.Errorf("release was handed %v, want nil", failure)
		}
	})

	t.Run("released with the failure when the client goes away", func(t *testing.T) {
		reader := openStream(t, server.URL, "/held")
		nextEvent(t, reader)
		_ = reader.Close()
		if failure := waitForRelease(t, log, "held"); failure == nil {
			t.Error("release was handed nil for a stream the client left")
		}
	})

	t.Run("a release error is reported as the stream's failure", func(t *testing.T) {
		reader := openStream(t, server.URL, "/release-fails")
		nextEvent(t, reader)
		assertStreamEnded(t, reader)
		waitForLog(t, logs, "stream release failed")
		if !strings.Contains(logs.String(), "an event stream handler failed") {
			t.Errorf("the release error was not logged as the stream's failure:\n%s", logs.String())
		}
	})
}

func TestAcquireOnAWebSocket(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	log := newReleaseLog()
	app := New(opts)
	app.WS("/done", func(ctx *Context, in struct{ Value Dep[relA] }, conn *WSConn) error {
		return conn.WriteText(ctx.Context(), string(in.Value.Get()))
	}, acquireAs[relA](log, "done", nil))
	app.WS("/echo", func(ctx *Context, _ Empty, conn *WSConn) error {
		for {
			typ, payload, err := conn.Read(ctx.Context())
			if err != nil {
				// What ended the conversation is the handler's to report, and
				// the release's to see.
				return err
			}
			if err := conn.Write(ctx.Context(), typ, payload); err != nil {
				return err
			}
		}
	}, acquireAs[relA](log, "echo", nil))
	app.WS("/release-fails", func(ctx *Context, _ Empty, conn *WSConn) error {
		return conn.WriteText(ctx.Context(), "sent")
	}, acquireAs[relA](log, "fails", errors.New("socket release failed")))
	mustBuild(t, app)
	server := newServerFor(t, app)

	t.Run("released with nil when the handler finishes", func(t *testing.T) {
		conn := dialWS(t, server, "/done")
		conn.expectText("done")
		conn.expectClose(1000)
		if failure := waitForRelease(t, log, "done"); failure != nil {
			t.Errorf("release was handed %v, want nil", failure)
		}
	})

	t.Run("released with the failure when the peer disconnects", func(t *testing.T) {
		conn := dialWS(t, server, "/echo")
		conn.text("hi")
		conn.expectText("hi")
		_ = conn.conn.Close()
		if failure := waitForRelease(t, log, "echo"); failure == nil {
			t.Error("release was handed nil for a connection the peer dropped")
		}
	})

	t.Run("a release error closes the connection as a failure", func(t *testing.T) {
		conn := dialWS(t, server, "/release-fails")
		conn.expectText("sent")
		if reason := conn.expectClose(1011); strings.Contains(reason, "socket release failed") {
			t.Errorf("the close reason %q carries the release error", reason)
		}
		waitForLog(t, logs, "socket release failed")
	})
}
