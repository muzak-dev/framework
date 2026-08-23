package badele

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests here play the hostile client. Every one of them was checked by
// removing the defence it names and watching it fail, so each is a statement
// about what stops the attack rather than about what happens to work.

func TestSSERefusesFieldsThatWouldForgeEvents(t *testing.T) {
	t.Parallel()
	// A stream that carries one client's input to another is where this
	// matters: a line break in an identifier or an event name would end its
	// own field and let whatever followed be read as fields of its own.
	tests := []struct {
		name  string
		event SSEEvent[itemOut]
		wants string
	}{
		{
			name:  "a newline in the event name",
			event: SSEEvent[itemOut]{Name: "chat\ndata: you have been logged out"},
			wants: "line break",
		},
		{
			name:  "a carriage return in the event name",
			event: SSEEvent[itemOut]{Name: "chat\rdata: nonsense"},
			wants: "line break",
		},
		{
			name:  "a newline in the id",
			event: SSEEvent[itemOut]{ID: "1\nevent: logout"},
			wants: "line break",
		},
		{
			name:  "a null byte in the id",
			event: SSEEvent[itemOut]{ID: "1\x00"},
			wants: "null byte",
		},
		{
			name:  "an event name that is not UTF-8",
			event: SSEEvent[itemOut]{Name: "\xff\xfe"},
			wants: "not valid UTF-8",
		},
		{
			name:  "text that is not UTF-8",
			event: SSEEvent[itemOut]{Text: "\xff\xfe"},
			wants: "must be valid UTF-8",
		},
		{
			name:  "a comment that is not UTF-8",
			event: SSEEvent[itemOut]{Comment: "\xff\xfe"},
			wants: "must be valid UTF-8",
		},
		{
			name:  "a negative retry",
			event: SSEEvent[itemOut]{Retry: -time.Second},
			wants: "cannot be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var refused error
			body := sseWire(t, func(app *App) {
				app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
					refused = stream.SendEvent(tc.event)
					// The stream survives the refusal, because the mistake is
					// the caller's to fix rather than the stream's to die of.
					return stream.Send(itemOut{Name: "still here"})
				})
			})
			if refused == nil {
				t.Fatalf("SendEvent(%+v) was accepted", tc.event)
			}
			if !strings.Contains(refused.Error(), tc.wants) {
				t.Errorf("SendEvent = %q, want it to mention %q", refused, tc.wants)
			}
			if body != "data: {\"name\":\"still here\"}\n\n" {
				t.Errorf("the stream wrote %q, want only the event that followed", body)
			}
		})
	}
}

func TestSSERefusesAnEventCarryingTwoPayloads(t *testing.T) {
	t.Parallel()
	// Data and Text are both the data field, and choosing one of them
	// silently would send something other than what was asked for.
	item := itemOut{Name: "Plumbus"}
	var refused error
	body := sseWire(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			refused = stream.SendEvent(SSEEvent[itemOut]{Data: &item, Text: "[DONE]"})
			return nil
		})
	})
	if !errors.Is(refused, errSSEBothPayloads) {
		t.Errorf("SendEvent = %v, want it refused for carrying two payloads", refused)
	}
	if body != "" {
		t.Errorf("the stream wrote %q, want nothing", body)
	}
}

func TestSSEEncodedDataCannotCarryALineBreak(t *testing.T) {
	t.Parallel()
	// A value's own content reaches the data field, so a newline inside it
	// must not be able to end the field. JSON escapes one, and the line
	// splitting behind every payload is what would catch it if it did not.
	body := sseWire(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			return stream.Send(itemOut{Name: "innocent\n\ndata: forged"})
		})
	})
	// The break is escaped by the encoder, so the whole value stays on the one
	// data line it was meant to be on, and there is exactly one event.
	want := "data: {\"name\":\"innocent\\n\\ndata: forged\"}\n\n"
	if body != want {
		t.Errorf("stream body = %q, want %q", body, want)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(body, "\n\n"), "\n") {
		if line == "data: forged" {
			t.Errorf("a value forged an event of its own: %q", body)
		}
	}
}

func TestSSEBoundsAWriteToAClientThatStoppedReading(t *testing.T) {
	t.Parallel()
	// A client that opens a stream and never reads it costs a goroutine, a
	// connection and a growing socket buffer. The write timeout is what gives
	// up on it.
	ended := make(chan error, 1)
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		var err error
		for err == nil {
			// The payload is large so that the socket buffers fill quickly,
			// which is when a write to a silent client starts blocking.
			err = stream.Send(itemOut{Name: strings.Repeat("x", 32<<10)})
		}
		ended <- err
		return err
	}, WithSSE(SSEOptions{KeepAlive: -1, WriteTimeout: 100 * time.Millisecond}))
	mustBuild(t, app)

	server := &http.Server{Handler: app, ReadHeaderTimeout: time.Second}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := fmt.Fprintf(conn, "GET /stream HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("sending the request: %v", err)
	}
	// Nothing is ever read from conn, which is the whole attack.

	select {
	case err := <-ended:
		if !errors.Is(err, ErrSSEStreamEnded) {
			t.Errorf("the stream ended with %v, want ErrSSEStreamEnded", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("a client that never read held the handler open past the write timeout")
	}
}

func TestSSEStreamLimitBelongsToTheApplication(t *testing.T) {
	t.Parallel()
	// The resource the limit protects is the process, so a route that could
	// raise it would be raising everybody's.
	opts := quietOptions()
	opts.SSE = SSEOptions{MaxStreams: 4}
	app := New(opts)
	app.SSE("/stream", streamItems("x"), WithSSE(SSEOptions{MaxStreams: 4000}))
	if got := buildError(t, app); !strings.Contains(got, "MaxStreams may only be set on the application") {
		t.Errorf("Build() = %q, want it to refuse a route-level stream limit", got)
	}

	unlimited := New(func() AppOptions {
		o := quietOptions()
		o.SSE = SSEOptions{MaxStreams: -1}
		return o
	}())
	unlimited.SSE("/stream", streamItems("x"))
	mustBuild(t, unlimited)
	if unlimited.streams.limit != 0 {
		t.Errorf("limit = %d, want a negative setting to remove it", unlimited.streams.limit)
	}
}

func TestSSEStreamsPerIPLimitBelongsToTheApplication(t *testing.T) {
	t.Parallel()
	// Like MaxStreams, the dimension this protects is a client's share of the
	// process, which a route cannot narrow or widen for itself.
	opts := quietOptions()
	opts.SSE = SSEOptions{MaxStreamsPerIP: 4}
	app := New(opts)
	app.SSE("/stream", streamItems("x"), WithSSE(SSEOptions{MaxStreamsPerIP: 4000}))
	if got := buildError(t, app); !strings.Contains(got, "MaxStreamsPerIP may only be set on the application") {
		t.Errorf("Build() = %q, want it to refuse a route-level per-IP stream limit", got)
	}

	unlimited := New(func() AppOptions {
		o := quietOptions()
		o.SSE = SSEOptions{MaxStreamsPerIP: -1}
		return o
	}())
	unlimited.SSE("/stream", streamItems("x"))
	mustBuild(t, unlimited)
	if unlimited.streams.perKeyLimit != 0 {
		t.Errorf("perKeyLimit = %d, want a negative setting to remove it", unlimited.streams.perKeyLimit)
	}
}

func TestSSESurvivesAStormOfOpenedAndAbandonedStreams(t *testing.T) {
	t.Parallel()
	var running atomic.Int64
	served, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			running.Add(1)
			defer running.Add(-1)
			for {
				if err := stream.Send(itemOut{Name: "tick"}); err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
			}
		}, WithSSE(SSEOptions{KeepAlive: 5 * time.Millisecond}))
	})

	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
			defer cancel()
			reader, _, err := SSEDial(ctx, server.URL+"/stream", SSEDialOptions{})
			if err != nil {
				return
			}
			// One event, then the client vanishes without a word.
			_, _ = reader.Next(ctx)
			_ = reader.Close()
		}()
	}
	wg.Wait()

	waitFor(t, func() bool { return running.Load() == 0 }, "every abandoned stream's handler to return")
	waitFor(t, func() bool { return served.streams.count() == 0 }, "the register to forget every abandoned stream")
}

func TestSSEDoesNotDiscloseAHandlerFailure(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		if err := stream.Send(itemOut{Name: "first"}); err != nil {
			return err
		}
		return fmt.Errorf("select * from accounts where token = %q failed on db-3.internal", "hunter2")
	}, WithSSE(SSEOptions{KeepAlive: -1}))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/stream")
	for _, secret := range []string{"accounts", "hunter2", "db-3.internal"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the stream disclosed %q: %s", secret, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "db-3.internal") {
		t.Errorf("the failure never reached the log:\n%s", logs.String())
	}
}

func TestSSEBoundsAValueOnItsWayIntoAnErrorMessage(t *testing.T) {
	t.Parallel()
	// A field built from something the size of the header limit should not
	// become a log line that size.
	huge := strings.Repeat("a", 4096) + "\n"
	err := sseCheckLine("event name", huge)
	if err == nil {
		t.Fatal("a name with a line break was accepted")
	}
	if len(err.Error()) > 256 {
		t.Errorf("the error is %d bytes long, want the value cut down first", len(err.Error()))
	}
}

func TestSSEKeepsStreamsApart(t *testing.T) {
	t.Parallel()
	// Two clients on one application must never read each other's events,
	// which is what a shared buffer or a shared register entry would cause.
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(ctx *Context, _ Empty, stream *SSEStream[itemOut]) error {
			mine := ctx.Query("who")
			for range 20 {
				if err := stream.Send(itemOut{Name: mine}); err != nil {
					return err
				}
			}
			return nil
		})
	})

	var wg sync.WaitGroup
	for _, who := range []string{"rick", "morty", "summer", "beth"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
			defer cancel()
			reader, _, err := SSEDial(ctx, server.URL+"/stream?who="+who, SSEDialOptions{})
			if err != nil {
				t.Errorf("SSEDial(%s) = %v", who, err)
				return
			}
			defer func() { _ = reader.Close() }()
			for range 20 {
				message, err := reader.Next(ctx)
				if err != nil {
					t.Errorf("Next(%s) = %v", who, err)
					return
				}
				item, err := message.Decode[itemOut]()
				if err != nil || item.Name != who {
					t.Errorf("%s read %q", who, message.Data)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSSEReaderRefusesAnEventLargerThanItsLimit(t *testing.T) {
	t.Parallel()
	// A client cannot choose what it is sent, so the reader bounds what a
	// server can make it hold.
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			return stream.Send(itemOut{Name: strings.Repeat("x", 64<<10)})
		})
	})

	reader := openStream(t, server.URL, "/stream", func(o *SSEDialOptions) { o.ReadLimit = 1 << 10 })
	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	if _, err := reader.Next(ctx); !errors.Is(err, errSSEEventTooLarge) {
		t.Errorf("Next() = %v, want the event refused for its size", err)
	}
}

func TestSSEReaderRefusesAResponseThatIsNotAStream(t *testing.T) {
	t.Parallel()
	// A reader that quietly accepted an HTML error page would report "no
	// events" for what is actually a failure.
	_, server := newSSETestApp(t, func(app *App) {
		app.Get("/json", func(_ *Context, _ Empty) (itemOut, error) {
			return itemOut{Name: "not a stream"}, nil
		})
	})

	reader, response := tryStream(t, server.URL, "/json")
	if reader != nil {
		t.Fatal("a JSON response was accepted as an event stream")
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the response itself to be returned", response.StatusCode)
	}
}

func TestSSEReaderBoundsAnEventThatNeverFinishes(t *testing.T) {
	t.Parallel()
	// A server that begins an event and then dribbles it must not hold a
	// reader open for as long as it likes.
	slow, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = slow.Close() })
	finished := make(chan struct{})
	t.Cleanup(func() { close(finished) })
	go func() {
		conn, err := slow.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// The request is read before a byte of the response is written, which
		// is not politeness but correctness: a response that arrives before
		// the transport has finished registering the call it answers is one it
		// cannot attribute to anything, and it discards the connection as
		// unsolicited. Writing on accept made that a race this test lost about
		// half the time, for a reason it is not about.
		_ = conn.SetReadDeadline(time.Now().Add(sseTestTimeout))
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n")
		_, _ = fmt.Fprint(conn, "data: the beginning of")
		// The rest never arrives. The connection is held until the test is
		// over rather than for a fixed time, so nothing of it outlives the
		// test that started it.
		<-finished
	}()

	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	reader, _, err := SSEDial(ctx, "http://"+slow.Addr().String()+"/stream", SSEDialOptions{
		ReadTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("SSEDial = %v", err)
	}
	defer func() { _ = reader.Close() }()

	start := time.Now()
	if _, err := reader.Next(ctx); !errors.Is(err, ErrSSEStreamEnded) {
		t.Errorf("Next() = %v, want the stream ended", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the reader waited %v for an event that never finished", elapsed)
	}
}
