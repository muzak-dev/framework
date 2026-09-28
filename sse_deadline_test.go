package muzak

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// startSSEServer serves the application over a real listener, on HTTP/2 when
// asked. The two differ in what a write deadline means: on HTTP/1 it is a
// timestamp that is looked at when a write happens, on HTTP/2 it is a timer
// that resets the stream whenever it fires, written to or not.
func startSSEServer(t *testing.T, app *App, h2 bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(app)
	srv.EnableHTTP2 = h2
	if h2 {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestSSEIdleStreamSurvivesItsWriteTimeoutOverHTTP2(t *testing.T) {
	t.Parallel()
	// The keepalive is longer than the write timeout, which is also how the
	// defaults are: a deadline left armed after the last event would reset the
	// stream before the keepalive could arrive.
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		if err := stream.Send(itemOut{Name: "hello"}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return nil
	}, WithSSE(SSEOptions{KeepAlive: time.Second, WriteTimeout: 250 * time.Millisecond}))
	mustBuild(t, app)
	srv := startSSEServer(t, app, true)

	reader, response, err := SSEDial(t.Context(), srv.URL+"/stream",
		SSEDialOptions{HTTPClient: srv.Client(), KeepComments: true})
	if err != nil {
		t.Fatalf("SSEDial() = %v", err)
	}
	defer reader.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("the stream was served over %s, not HTTP/2", response.Proto)
	}

	// The event, then two keepalives: the second arrives long after the write
	// timeout would have reset an idle stream.
	start := time.Now()
	for range 3 {
		if _, err := reader.Next(t.Context()); err != nil {
			t.Fatalf("the idle stream ended after %v: %v", time.Since(start).Round(time.Millisecond), err)
		}
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond {
		t.Fatalf("two keepalives arrived after %v, which is sooner than the keepalive interval allows", elapsed)
	}
}

func TestSSEEndsCleanlyAfterBeingIdleOverHTTP1(t *testing.T) {
	t.Parallel()
	// The handler is quiet for longer than the write timeout and then returns.
	// The bytes net/http writes to end the response must not be written under
	// the deadline of the last event, which has expired by then.
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		if err := stream.Send(itemOut{Name: "hello"}); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
		return nil
	}, WithSSE(SSEOptions{KeepAlive: time.Minute, WriteTimeout: 200 * time.Millisecond}))
	mustBuild(t, app)
	srv := startSSEServer(t, app, false)

	reader, _, err := SSEDial(t.Context(), srv.URL+"/stream", SSEDialOptions{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("SSEDial() = %v", err)
	}
	defer reader.Close()
	if _, err := reader.Next(t.Context()); err != nil {
		t.Fatalf("Next() = %v", err)
	}
	if _, err := reader.Next(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("a handler that returned was seen as %v, want a clean end", err)
	}
}

func TestSSEShutdownEndsAnIdleStreamCleanly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		h2   bool
	}{{"HTTP/1", false}, {"HTTP/2", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
				if err := stream.Send(itemOut{Name: "hello"}); err != nil {
					return err
				}
				<-stream.Context().Done()
				return nil
			})
			mustBuild(t, app)
			srv := startSSEServer(t, app, tc.h2)

			reader, _, err := SSEDial(t.Context(), srv.URL+"/stream", SSEDialOptions{HTTPClient: srv.Client()})
			if err != nil {
				t.Fatalf("SSEDial() = %v", err)
			}
			defer reader.Close()
			if _, err := reader.Next(t.Context()); err != nil {
				t.Fatalf("Next() = %v", err)
			}

			// Nothing is being written, so there is no write for shutdown to
			// interrupt, and the handler's own return ends the response.
			app.streams.shutdown(2*time.Second, (*sseStream).shuttingDown)
			if _, err := reader.Next(t.Context()); !errors.Is(err, io.EOF) {
				t.Fatalf("a stream ended by shutdown was seen as %v, want a clean end", err)
			}
		})
	}
}

// deadlineLog is a response writer that records the last write deadline it was
// given, so that a test can see what state a stream leaves the response in.
type deadlineLog struct {
	deadlineWriter
	mu   sync.Mutex
	last time.Time
}

func (d *deadlineLog) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.last = t
	d.mu.Unlock()
	return d.deadlineWriter.SetWriteDeadline(t)
}

func (d *deadlineLog) deadline() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// newDeadlineStream builds an opened stream over a writer that carries
// deadlines and records them.
func newDeadlineStream(t *testing.T, opts SSEOptions) (*sseStream, *deadlineLog) {
	t.Helper()
	writer := &deadlineLog{deadlineWriter: deadlineWriter{recorder: httptest.NewRecorder()}}
	c := &Context{
		w: asResponseWriter(writer),
		r: httptest.NewRequest(http.MethodGet, "/stream", nil),
	}
	stream := newSSEStream(c, opts.withDefaults())
	t.Cleanup(stream.cancel)
	if err := stream.open(c); err != nil {
		t.Fatalf("open() = %v", err)
	}
	return stream, writer
}

func TestSSEClearsTheWriteDeadlineOnceAnEventIsWritten(t *testing.T) {
	t.Parallel()
	stream, writer := newDeadlineStream(t, SSEOptions{WriteTimeout: time.Minute})
	if err := stream.send(sseFrame{comment: "one"}); err != nil {
		t.Fatalf("send() = %v", err)
	}
	if got := writer.deadline(); !got.IsZero() {
		t.Fatalf("the deadline left after a write is %v, want none: an idle stream would be reset by it", got)
	}
}

func TestSSEInterruptLeavesAnIdleStreamAlone(t *testing.T) {
	t.Parallel()
	stream, writer := newDeadlineStream(t, SSEOptions{WriteTimeout: time.Minute})
	if err := stream.send(sseFrame{comment: "one"}); err != nil {
		t.Fatalf("send() = %v", err)
	}
	stream.shuttingDown()
	if got := writer.deadline(); !got.IsZero() {
		t.Fatalf("ending an idle stream set the write deadline to %v, which would abort the bytes that end the response", got)
	}
}

func TestSSEInterruptStopsAWriteInProgress(t *testing.T) {
	t.Parallel()
	stream, writer := newDeadlineStream(t, SSEOptions{WriteTimeout: time.Minute})
	if err := stream.acquire(); err != nil {
		t.Fatalf("acquire() = %v", err)
	}
	defer stream.release()
	// A writer that has armed its deadline and not yet finished is what the
	// interrupt exists for.
	if err := stream.arm(); err != nil {
		t.Fatalf("arm() = %v", err)
	}
	stream.shuttingDown()
	if got := writer.deadline(); got.IsZero() || time.Until(got) > 0 {
		t.Fatalf("the deadline of a write in progress is %v, want a moment already past", got)
	}
}
func TestSSEOpenAndShutdownDoNotRace(t *testing.T) {
	t.Parallel()
	// A stream is in the register before its header is written, so a shutdown
	// can end it while it is still being opened. Run under -race this is what
	// shows whether the two agree about the state they share.
	for range 300 {
		writer := &deadlineLog{deadlineWriter: deadlineWriter{recorder: httptest.NewRecorder()}}
		c := &Context{
			w: asResponseWriter(writer),
			r: httptest.NewRequest(http.MethodGet, "/stream", nil),
		}
		stream := newSSEStream(c, SSEOptions{Retry: time.Second}.withDefaults())
		done := make(chan struct{})
		go func() {
			stream.shuttingDown()
			close(done)
		}()
		_ = stream.open(c)
		<-done
		stream.finish()
		stream.cancel()
	}
}

func TestSSEOpenRefusesAStreamThatAlreadyEnded(t *testing.T) {
	t.Parallel()
	// A shutdown that lands before the header is written ends the stream, and
	// a stream that has ended must not go on to write a header of its own.
	writer := &deadlineLog{deadlineWriter: deadlineWriter{recorder: httptest.NewRecorder()}}
	c := &Context{
		w: asResponseWriter(writer),
		r: httptest.NewRequest(http.MethodGet, "/stream", nil),
	}
	stream := newSSEStream(c, SSEOptions{}.withDefaults())
	t.Cleanup(stream.cancel)
	stream.shuttingDown()
	if err := stream.open(c); err == nil {
		t.Fatal("open() = nil for a stream that had already ended")
	}
	if writer.recorder.Flushed {
		t.Error("the header of a stream that had already ended was written")
	}
	stream.finish()
}
