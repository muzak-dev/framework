package muzak

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// slowEvent blocks inside MarshalJSON until released, which is how a test
// holds a send in the one part of its work that no deadline can interrupt.
type slowEvent struct {
	entered chan struct{}
	unblock chan struct{}
}

func (e slowEvent) MarshalJSON() ([]byte, error) {
	close(e.entered)
	<-e.unblock
	return []byte(`"SECRET-OF-THE-FIRST-CLIENT"`), nil
}

func TestSSELateSendAfterTheHandlerReturnsNeverReachesTheResponse(t *testing.T) {
	t.Parallel()
	// A send that a producer goroutine left encoding when the handler
	// returned used to hold the write semaphore while it encoded. The stream
	// gave up waiting for it after the write timeout and handed the response
	// back anyway, so the send finished by writing into a response net/http
	// had already recycled: a crash, or an event delivered inside the next
	// request on the same connection.
	entered := make(chan struct{})
	unblock := make(chan struct{})
	sendErr := make(chan error, 1)
	nextRunning := make(chan struct{})
	nextRelease := make(chan struct{})

	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[slowEvent]) error {
			go func() { sendErr <- stream.Send(slowEvent{entered: entered, unblock: unblock}) }()
			<-entered
			return nil
		}, WithSSE(SSEOptions{WriteTimeout: 20 * time.Millisecond, KeepAlive: -1}))
		app.Get("/next", func(_ *Context, _ Empty) (rtOut, error) {
			close(nextRunning)
			<-nextRelease
			return rtOut{OK: true}, nil
		})
	})
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	})

	// One keep-alive connection carries both requests, which is what makes a
	// late write land in the second of them.
	transport := &http.Transport{MaxConnsPerHost: 1}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: sseTestTimeout}

	started := time.Now()
	response, err := client.Get(server.URL + "/stream")
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	_ = response.Body.Close()
	if elapsed := time.Since(started); elapsed > sseTestTimeout/2 {
		t.Errorf("the stream took %v to end, want it to end without waiting for the encoding send", elapsed)
	}

	type result struct {
		status int
		body   string
		err    error
	}
	next := make(chan result, 1)
	go func() {
		response, err := client.Get(server.URL + "/next")
		if err != nil {
			next <- result{err: err}
			return
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		next <- result{status: response.StatusCode, body: string(body), err: err}
	}()
	<-nextRunning

	close(unblock)
	select {
	case err := <-sendErr:
		if !errors.Is(err, ErrSSEStreamEnded) {
			t.Errorf("the late send returned %v, want ErrSSEStreamEnded", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the late send never returned")
	}
	close(nextRelease)

	got := <-next
	if got.err != nil {
		t.Fatalf("the next request failed: %v", got.err)
	}
	if got.status != http.StatusOK || strings.Contains(got.body, "SECRET") {
		t.Errorf("the next request got %d %q, want a clean 200 with none of the first client's event", got.status, got.body)
	}
}

func TestWebSocketLateWriteAfterTheHandlerReturnsIsRefused(t *testing.T) {
	t.Parallel()
	// The WebSocket write path already encodes before it takes the write
	// semaphore, and the connection it writes to is hijacked, so it is never
	// handed back to net/http. A late write therefore finds a closed
	// connection and reports it, and this test keeps it that way.
	entered := make(chan struct{})
	unblock := make(chan struct{})
	writeErr := make(chan error, 1)
	returned := make(chan struct{})

	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			go func() {
				writeErr <- conn.WriteJSON(ctx.Context(), slowEvent{entered: entered, unblock: unblock})
			}()
			<-entered
			close(returned)
			return nil
		})
	})
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	})

	client := dialWS(t, server.URL, "/ws")
	<-returned
	// The close frame is the last thing the server sends once the handler has
	// returned, so the connection is known to be over before the write is let
	// go.
	if _, opcode, _ := client.recv(); opcode != opClose {
		t.Fatalf("received opcode %#x, want the close frame", opcode)
	}
	close(unblock)
	select {
	case err := <-writeErr:
		var closed *WSCloseError
		if !errors.As(err, &closed) {
			t.Errorf("the late write returned %v, want the connection reported as ended", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the late write never returned")
	}
}

func TestSSESendQueuedBehindAWriteEndsWithTheStream(t *testing.T) {
	t.Parallel()
	stream := newTestStream(t, SSEOptions{})
	if err := stream.acquire(); err != nil {
		t.Fatalf("acquire() = %v", err)
	}
	// The event is encoded and then waits for the writer ahead of it, which
	// is where a stream ending has to find it and send it away.
	queued := make(chan error, 1)
	go func() { queued <- stream.send(sseFrame{text: "queued", hasText: true}) }()
	time.Sleep(10 * time.Millisecond)
	stream.shuttingDown()
	select {
	case err := <-queued:
		if !errors.Is(err, ErrSSEStreamEnded) {
			t.Errorf("the queued send returned %v, want ErrSSEStreamEnded", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("a send queued behind a write was never released")
	}
	stream.release()
}
