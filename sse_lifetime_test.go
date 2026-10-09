package muzak

import (
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lifetimeStream is a handler that numbers its events and resumes after the
// one the client last saw, the way a stream that survives reconnects does.
func lifetimeStream(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
	next := 1
	if last := stream.LastEventID(); last != "" {
		n, err := strconv.Atoi(last)
		if err != nil {
			return err
		}
		next = n + 1
	}
	if err := stream.SendEvent(SSEEvent[itemOut]{ID: strconv.Itoa(next), Data: &itemOut{Name: "event " + strconv.Itoa(next)}}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func TestSSEMaxLifetimeEndsAStreamCleanly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		h2   bool
	}{{"HTTP/1", false}, {"HTTP/2", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.SSE("/stream", lifetimeStream,
				WithSSE(SSEOptions{MaxLifetime: 400 * time.Millisecond, KeepAlive: 100 * time.Millisecond}))
			mustBuild(t, app)
			srv := startSSEServer(t, app, tc.h2)

			start := time.Now()
			reader, _, err := SSEDial(t.Context(), srv.URL+"/stream",
				SSEDialOptions{HTTPClient: srv.Client(), KeepComments: true})
			if err != nil {
				t.Fatalf("SSEDial() = %v", err)
			}
			defer reader.Close()

			var comments []string
			for {
				message, err := reader.Next(t.Context())
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("the stream ended with %v after %v, want a clean end", err, time.Since(start).Round(time.Millisecond))
				}
				if message.Comment != "" {
					comments = append(comments, message.Comment)
				}
			}
			elapsed := time.Since(start)
			if elapsed < 350*time.Millisecond || elapsed > 3*time.Second {
				t.Errorf("the stream lasted %v, want about its 400ms lifetime", elapsed.Round(time.Millisecond))
			}
			// The last thing on the wire says why, so that a client or a
			// person watching does not take the end for a fault.
			if len(comments) == 0 || comments[len(comments)-1] != sseLifetimeComment {
				t.Errorf("the comments were %q, want the last to be %q", comments, sseLifetimeComment)
			}
			waitFor(t, func() bool { return app.streams.count() == 0 }, "the stream to release its slot")

			// A well-behaved client reconnects with what it last saw and is
			// served from there.
			again, _, err := SSEDial(t.Context(), srv.URL+"/stream",
				SSEDialOptions{HTTPClient: srv.Client(), LastEventID: reader.LastEventID()})
			if err != nil {
				t.Fatalf("reconnecting: SSEDial() = %v", err)
			}
			defer again.Close()
			message, err := again.Next(t.Context())
			if err != nil || message.ID != "2" {
				t.Fatalf("after reconnecting Next() = %+v, %v; want the event after 1", message, err)
			}
		})
	}
}

// A handler that never stops sending is ended by its next send, and the
// keepalive does not outlive the stream.
func TestSSEMaxLifetimeStopsABusyHandlerAndTheKeepalive(t *testing.T) {
	t.Parallel()
	returned := make(chan error, 1)
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		for {
			if err := stream.Send(itemOut{Name: "tick"}); err != nil {
				returned <- err
				return err
			}
			time.Sleep(5 * time.Millisecond)
		}
	}, WithSSE(SSEOptions{MaxLifetime: 200 * time.Millisecond, KeepAlive: 20 * time.Millisecond}))
	mustBuild(t, app)
	srv := startSSEServer(t, app, false)

	reader, _, err := SSEDial(t.Context(), srv.URL+"/stream", SSEDialOptions{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("SSEDial() = %v", err)
	}
	defer reader.Close()
	for {
		if _, err := reader.Next(t.Context()); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("the stream ended with %v, want a clean end", err)
			}
			break
		}
	}
	select {
	case err := <-returned:
		if !errors.Is(err, ErrSSEStreamEnded) || !strings.Contains(err.Error(), "maximum lifetime") {
			t.Errorf("the handler's send failed with %v, want the stream's lifetime named", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the handler was never told the stream had ended")
	}
	waitFor(t, func() bool { return app.streams.count() == 0 }, "the stream to release its slot")
	assertNoGoroutineLeaks(t)
}

// A client that reads nothing while a keepalive trickles in never fills a
// socket buffer, so no write timeout ever fires and the client holds its slot
// for as long as it likes. The lifetime is what takes the slot back.
func TestSSEMaxLifetimeFreesTheSlotOfAClientThatNeverReads(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.SSE = SSEOptions{MaxStreams: 1}
	const lifetime = 300 * time.Millisecond
	app, server := newSSETestAppWith(t, options, func(app *App) {
		app.SSE("/stream", lifetimeStream,
			WithSSE(SSEOptions{MaxLifetime: lifetime, KeepAlive: 20 * time.Millisecond}))
	})

	// The lifetime cannot have started before this, so a slot found free
	// sooner than a lifetime after it was freed early.
	start := time.Now()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /stream HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// It never reads. While it is silent the slot is taken. A loaded machine
	// can stretch the wait and the sleep past the lifetime, so only a slot
	// freed before the lifetime could have run out counts against it.
	waitFor(t, func() bool { return app.streams.count() == 1 }, "the stream to open")
	time.Sleep(100 * time.Millisecond)
	if got := app.streams.count(); got != 1 {
		if elapsed := time.Since(start); elapsed < lifetime {
			t.Fatalf("the stream was gone after %v with %d registered, before its lifetime", elapsed.Round(time.Millisecond), got)
		}
	}
	waitFor(t, func() bool { return app.streams.count() == 0 }, "the lifetime to release the slot of a client that reads nothing")

	// What it did not read is still there, ending in the response's last chunk.
	_ = conn.SetReadDeadline(time.Now().Add(sseTestTimeout))
	var rest strings.Builder
	buf := make([]byte, 4096)
	for !strings.HasSuffix(rest.String(), "0\r\n\r\n") {
		n, err := conn.Read(buf)
		rest.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(rest.String(), sseLifetimeComment) || !strings.HasSuffix(rest.String(), "0\r\n\r\n") {
		t.Errorf("the unread bytes did not end with the lifetime comment and the last chunk: %q", rest.String())
	}
}

func TestSSEMaxLifetimeOptions(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.SSE = SSEOptions{MaxLifetime: time.Hour}
	app := New(opts)
	inherits := app.SSE("/inherits", streamItems("x"))
	shorter := app.SSE("/shorter", streamItems("x"), WithSSE(SSEOptions{MaxLifetime: time.Minute}))
	unbounded := app.SSE("/unbounded", streamItems("x"), WithSSE(SSEOptions{MaxLifetime: -1}))
	mustBuild(t, app)

	if got := inherits.sse.opts.MaxLifetime; got != time.Hour {
		t.Errorf("an inheriting route resolved to %v, want the application's hour", got)
	}
	if got := shorter.sse.opts.MaxLifetime; got != time.Minute {
		t.Errorf("a narrowed route resolved to %v, want a minute", got)
	}
	// A negative value is how a route says it has no ceiling, whatever the
	// application set, and it resolves to none.
	if got := unbounded.sse.opts.MaxLifetime; got != 0 {
		t.Errorf("a route that removed the bound resolved to %v, want none", got)
	}
	if got := (SSEOptions{}).withDefaults().MaxLifetime; got != 0 {
		t.Errorf("the zero value resolved to a lifetime of %v; a stream has none unless asked", got)
	}
}
