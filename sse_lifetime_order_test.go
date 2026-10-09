package muzak

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The comment that says a stream reached its lifetime is the last thing on it.
// A keepalive that was already waiting to write when the lifetime passed took
// the write lock after the comment and went out behind it, so a client saw the
// reason and then more of the stream it was told had ended.
func TestSSEMaxLifetimeClosingCommentIsTheLastWrite(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	// A keepalive far shorter than the lifetime keeps one always waiting.
	app.SSE("/stream", lifetimeStream,
		WithSSE(SSEOptions{MaxLifetime: 30 * time.Millisecond, KeepAlive: 2 * time.Millisecond}))
	mustBuild(t, app)
	srv := startSSEServer(t, app, false)

	for round := range 40 {
		reader, _, err := SSEDial(t.Context(), srv.URL+"/stream",
			SSEDialOptions{HTTPClient: srv.Client(), KeepComments: true})
		if err != nil {
			t.Fatalf("round %d: SSEDial() = %v", round, err)
		}
		var comments []string
		for {
			message, err := reader.Next(t.Context())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("round %d: the stream ended with %v, want a clean end", round, err)
			}
			if message.Comment != "" {
				comments = append(comments, message.Comment)
			}
		}
		_ = reader.Close()
		if len(comments) == 0 || comments[len(comments)-1] != sseLifetimeComment {
			t.Fatalf("round %d: the comments were %q, want the last to be %q", round, comments, sseLifetimeComment)
		}
	}
}

// stallingCommentWriter is a response writer that carries deadlines and holds
// the write of the closing comment until it is released or its deadline is
// moved into the past, which is what a client slow to take the comment and a
// stream interrupting that write look like from the stream's side.
type stallingCommentWriter struct {
	recorder *httptest.ResponseRecorder
	started  chan struct{}
	release  chan struct{}
	moved    chan struct{}
	once     sync.Once
	mu       sync.Mutex
}

func newStallingCommentWriter() *stallingCommentWriter {
	return &stallingCommentWriter{
		recorder: httptest.NewRecorder(),
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		moved:    make(chan struct{}, 1),
	}
}

func (w *stallingCommentWriter) Header() http.Header             { return w.recorder.Header() }
func (w *stallingCommentWriter) WriteHeader(status int)          { w.recorder.WriteHeader(status) }
func (w *stallingCommentWriter) FlushError() error               { return nil }
func (w *stallingCommentWriter) SetReadDeadline(time.Time) error { return nil }

func (w *stallingCommentWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		select {
		case w.moved <- struct{}{}:
		default:
		}
	}
	return nil
}

func (w *stallingCommentWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(sseLifetimeComment)) {
		w.once.Do(func() { close(w.started) })
		select {
		case <-w.release:
		case <-w.moved:
			return 0, os.ErrDeadlineExceeded
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recorder.Write(p)
}

func (w *stallingCommentWriter) body() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recorder.Body.String()
}

// A handler whose send is refused because the stream reached its lifetime
// returns, and its stream is finished as soon as it does. If it is told while
// the closing comment is still being written, finishing the stream interrupts
// that write, and the client is left with a response cut off part way instead
// of the comment and a clean end. The refusal has to wait for the comment.
func TestSSELifetimeRefusalWaitsForTheClosingComment(t *testing.T) {
	t.Parallel()
	writer := newStallingCommentWriter()
	c := &Context{
		w: asResponseWriter(writer),
		r: httptest.NewRequest(http.MethodGet, "/stream", nil),
	}
	stream := newSSEStream(c, SSEOptions{WriteTimeout: time.Minute}.withDefaults())
	t.Cleanup(stream.cancel)
	if err := stream.open(c); err != nil {
		t.Fatalf("open() = %v", err)
	}

	go stream.expire()
	select {
	case <-writer.started:
	case <-time.After(sseTestTimeout):
		t.Fatal("the closing comment was never written")
	}
	// What serveSSE does with a handler that sends while the comment is
	// being written: the handler returns what the send said, and the stream
	// is finished.
	handler := make(chan error, 1)
	go func() {
		err := stream.send(sseFrame{comment: "busy"})
		stream.finish()
		handler <- err
	}()
	// Time for a refusal given at once to have finished the stream. On a
	// loaded machine it may not have, and then this passes without proving
	// anything, but it cannot fail a stream that waits.
	time.Sleep(100 * time.Millisecond)
	close(writer.release)
	select {
	case err := <-handler:
		if !errors.Is(err, errSSELifetime) {
			t.Errorf("send() = %v, want the stream's lifetime named", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the refused send never returned")
	}
	if body := writer.body(); !strings.Contains(body, ": "+sseLifetimeComment+"\n\n") {
		t.Errorf("the closing comment was cut off; the stream carried %q", body)
	}
}
