package muzak

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The reader is exercised here against streams written out by hand rather than
// by the engine, so that a mistake in the format cannot hide by being made
// identically at both ends.

// serveRaw serves one stream body verbatim and reports what the request
// carried, so a test can assert on both halves of the exchange.
func serveRaw(t *testing.T, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, seen
}

// readAll reads every message a stream carries until it ends.
func readAll(t *testing.T, reader *SSEReader) []SSEMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	var messages []SSEMessage
	for {
		message, err := reader.Next(ctx)
		if err != nil {
			if !errors.Is(err, ErrSSEStreamEnded) {
				t.Fatalf("Next() = %v", err)
			}
			return messages
		}
		messages = append(messages, message)
	}
}

func TestSSEReaderParsesTheFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []SSEMessage
	}{
		{
			name: "the simplest event there is",
			body: "data: hello\n\n",
			want: []SSEMessage{{Data: "hello"}},
		},
		{
			name: "carriage returns and line feeds in every combination",
			body: "data: one\r\n\r\ndata: two\r\rdata: three\n\n",
			want: []SSEMessage{{Data: "one"}, {Data: "two"}, {Data: "three"}},
		},
		{
			name: "data lines are joined with newlines",
			body: "data: one\ndata: two\n\n",
			want: []SSEMessage{{Data: "one\ntwo"}},
		},
		{
			name: "only the first space after the colon belongs to the format",
			body: "data:  spaced\n\n",
			want: []SSEMessage{{Data: " spaced"}},
		},
		{
			name: "a field with no value at all",
			body: "data\n\n",
			want: []SSEMessage{{Data: ""}},
		},
		{
			name: "comments are not events",
			body: ": keepalive\ndata: after\n\n",
			want: []SSEMessage{{Data: "after"}},
		},
		{
			name: "a field this reader does not know is ignored",
			body: "invented: 1\ndata: still here\n\n",
			want: []SSEMessage{{Data: "still here"}},
		},
		{
			name: "the identifier persists until another arrives",
			body: "id: 1\ndata: first\n\ndata: second\n\nid: 2\ndata: third\n\n",
			want: []SSEMessage{
				{ID: "1", Data: "first"},
				{ID: "1", Data: "second"},
				{ID: "2", Data: "third"},
			},
		},
		{
			name: "an identifier with a null byte is dropped rather than kept",
			body: "id: 1\ndata: first\n\nid: 2\x00\ndata: second\n\n",
			want: []SSEMessage{{ID: "1", Data: "first"}, {ID: "1", Data: "second"}},
		},
		{
			name: "an event with no data is not dispatched",
			body: "event: ping\nid: 9\n\ndata: real\n\n",
			want: []SSEMessage{{ID: "9", Data: "real"}},
		},
		{
			name: "an event carries its name",
			body: "event: item_update\ndata: {}\n\n",
			want: []SSEMessage{{Name: "item_update", Data: "{}"}},
		},
		{
			name: "a byte order mark is not part of the first field",
			body: "\ufeffdata: hello\n\n",
			want: []SSEMessage{{Data: "hello"}},
		},
		{
			name: "an unterminated event at the end of a stream is dropped",
			body: "data: complete\n\ndata: cut off",
			want: []SSEMessage{{Data: "complete"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _ := serveRaw(t, tc.body)
			reader := openStream(t, server.URL, "/")
			got := readAll(t, reader)
			if len(got) != len(tc.want) {
				t.Fatalf("read %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if got[i] != want {
					t.Errorf("event %d = %+v, want %+v", i, got[i], want)
				}
			}
		})
	}
}

func TestSSEReaderDeliversCommentsWhenAsked(t *testing.T) {
	t.Parallel()
	server, _ := serveRaw(t, ": keepalive\ndata: after\n\n")
	reader := openStream(t, server.URL, "/", func(o *SSEDialOptions) { o.KeepComments = true })

	got := readAll(t, reader)
	want := []SSEMessage{{Comment: "keepalive"}, {Data: "after"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("messages = %+v, want %+v", got, want)
	}
}

// TestSSEReaderDeliversNothingAfterClose is the regression test for a reader
// that went on handing out events already in its buffer after it was closed,
// which made a test asserting that a closed stream ends fail whenever a
// keepalive had arrived in the same read as the event before it.
func TestSSEReaderDeliversNothingAfterClose(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader("data: first\n\n: keepalive\ndata: second\n\n"))
	reader := newSSEReader(body, SSEDialOptions{KeepComments: true})

	message, err := reader.Next(t.Context())
	if err != nil || message.Data != "first" {
		t.Fatalf("Next() = %+v, %v, want the first event", message, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	for range 3 {
		if message, err := reader.Next(t.Context()); !errors.Is(err, ErrSSEStreamEnded) {
			t.Fatalf("Next() after Close = %+v, %v, want ErrSSEStreamEnded", message, err)
		}
	}
}

func TestSSEReaderReportsWhatTheStreamAsksFor(t *testing.T) {
	t.Parallel()
	server, _ := serveRaw(t, "retry: 2500\nid: 7\ndata: hello\n\nretry: nonsense\ndata: again\n\n")
	reader := openStream(t, server.URL, "/")

	readAll(t, reader)
	if got := reader.Retry(); got != 2500*time.Millisecond {
		t.Errorf("Retry() = %v, want %v", got, 2500*time.Millisecond)
	}
	if got := reader.LastEventID(); got != "7" {
		t.Errorf("LastEventID() = %q, want %q", got, "7")
	}
}

func TestSSEReaderResumesFromTheLastIdentifier(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			// The identifier a client sends back is where the stream picks up.
			start := 0
			if resumed := stream.LastEventID(); resumed != "" {
				parsed, err := strconv.Atoi(resumed)
				if err != nil {
					return NewHTTPError(http.StatusBadRequest, "bad identifier")
				}
				start = parsed + 1
			}
			for i := start; i < 3; i++ {
				item := itemOut{Name: string(rune('a' + i))}
				if err := stream.SendEvent(SSEEvent[itemOut]{ID: strconv.Itoa(i), Data: &item}); err != nil {
					return err
				}
			}
			return nil
		})
	})

	first := openStream(t, server.URL, "/stream")
	if message := nextEvent(t, first); message.ID != "0" {
		t.Fatalf("first event = %+v, want the one with identifier 0", message)
	}
	_ = first.Close()

	resumed := openStream(t, server.URL, "/stream", func(o *SSEDialOptions) { o.LastEventID = "0" })
	message := nextEvent(t, resumed)
	if message.ID != "1" || message.Data != `{"name":"b"}` {
		t.Errorf("resumed at %+v, want the event after the one the client saw", message)
	}
}

func TestSSEDialSendsWhatAStreamRequestShould(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	reader, _, err := SSEDial(ctx, server.URL+"/stream", SSEDialOptions{
		Method:      "post",
		Body:        strings.NewReader(`{"text":"hi"}`),
		Header:      http.Header{"Authorization": []string{"Bearer jessica"}},
		LastEventID: "42",
	})
	if err != nil {
		t.Fatalf("SSEDial = %v", err)
	}
	defer func() { _ = reader.Close() }()
	nextEvent(t, reader)

	if seen.Method != http.MethodPost {
		t.Errorf("method = %q, want it upper-cased to POST", seen.Method)
	}
	for name, want := range map[string]string{
		"Accept":        "text/event-stream",
		"Cache-Control": "no-cache",
		"Last-Event-ID": "42",
		"Authorization": "Bearer jessica",
	} {
		if got := seen.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

func TestSSEDialRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wants   string
	}{
		{
			name: "a status that is not 200",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
				_, _ = io.WriteString(w, `{"error":"no"}`)
			},
			wants: "refused with status 418",
		},
		{
			name: "a body that is not an event stream",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<html>nope</html>")
			},
			wants: "not an event stream",
		},
		{
			name: "a response with no content type at all",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header()["Content-Type"] = nil
				w.WriteHeader(http.StatusOK)
			},
			wants: "not an event stream",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(tc.handler)
			t.Cleanup(server.Close)

			ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
			defer cancel()
			reader, response, err := SSEDial(ctx, server.URL, SSEDialOptions{})
			if reader != nil {
				t.Fatal("a response that is not a stream was accepted as one")
			}
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("SSEDial = %v, want it to mention %q", err, tc.wants)
			}
			// The refused response is returned with its body still readable,
			// so a caller can say what came back instead.
			if body, readErr := io.ReadAll(response.Body); readErr != nil {
				t.Errorf("the refused body could not be read again: %v", readErr)
			} else if response.StatusCode == http.StatusTeapot && string(body) != `{"error":"no"}` {
				t.Errorf("the refused body = %q", body)
			}
		})
	}
}

func TestSSEDialReportsAnUnreachableServer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()

	if _, _, err := SSEDial(ctx, "http://127.0.0.1:0/stream", SSEDialOptions{}); err == nil {
		t.Error("dialling a port nothing listens on succeeded")
	}
	if _, _, err := SSEDial(ctx, "://not a url", SSEDialOptions{}); err == nil {
		t.Error("dialling a malformed address succeeded")
	}
}

func TestSSEDialLeavesTheCallersClientAlone(t *testing.T) {
	t.Parallel()
	// A client timeout would bound the whole life of the stream rather than
	// the request that opens it, so it is removed on a copy.
	server, _ := serveRaw(t, "data: hello\n\n")
	caller := &http.Client{Timeout: 50 * time.Millisecond}

	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	// The handshake has its own bound, which by default is the client's
	// timeout. Left at that, a slow scheduler could fail the dial in 50
	// milliseconds, and the test is about the stream outliving the timeout.
	reader, _, err := SSEDial(ctx, server.URL, SSEDialOptions{HTTPClient: caller, HandshakeTimeout: sseTestTimeout})
	if err != nil {
		t.Fatalf("SSEDial = %v", err)
	}
	defer func() { _ = reader.Close() }()

	time.Sleep(100 * time.Millisecond)
	if message := nextEvent(t, reader); message.Data != "hello" {
		t.Errorf("event = %+v, want the stream to have outlived the client's timeout", message)
	}
	if caller.Timeout != 50*time.Millisecond {
		t.Errorf("the caller's client was modified: Timeout = %v", caller.Timeout)
	}
}

func TestSSEReaderEndsWhenItsContextIsCancelled(t *testing.T) {
	t.Parallel()
	held := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-held
	}))
	t.Cleanup(func() { close(held); server.Close() })

	reader := openStream(t, server.URL, "/")
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := reader.Next(ctx); !errors.Is(err, ErrSSEStreamEnded) {
		t.Errorf("Next() = %v, want the stream ended", err)
	}
}

func TestSSEReaderCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	server, _ := serveRaw(t, "data: hello\n\n")
	reader := openStream(t, server.URL, "/")

	if err := reader.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Errorf("the second Close() = %v", err)
	}
}

func TestSSEMessageDecodeReportsWhatDoesNotFit(t *testing.T) {
	t.Parallel()
	item, err := SSEMessage{Data: `{"name":"Plumbus"}`}.Decode[itemOut]()
	if err != nil || item.Name != "Plumbus" {
		t.Fatalf("Decode() = %+v, %v", item, err)
	}
	if _, err := (SSEMessage{Data: "[DONE]"}).Decode[itemOut](); err == nil {
		t.Error("a sentinel decoded as an item")
	}
}

// serveChunks serves a stream written in pieces, flushing between each, which
// is how a test puts a line ending on one side of a read and its partner on
// the other.
func serveChunks(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSSEReaderJoinsALineEndingSplitAcrossTwoReads(t *testing.T) {
	t.Parallel()
	// The carriage return arrives with one read and its line feed with the
	// next, which is one line ending rather than two.
	server := serveChunks(t, "data: one\r", "\ndata: two\n\n")
	reader := openStream(t, server.URL, "/")

	got := readAll(t, reader)
	if len(got) != 1 || got[0].Data != "one\ntwo" {
		t.Errorf("messages = %+v, want one event carrying both lines", got)
	}
}

func TestSSEReaderBoundsWhatOneEventCanCost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{
			name: "one line longer than the limit, complete when it is read",
			body: "data: " + strings.Repeat("x", 2<<10) + "\n\n",
		},
		{
			name: "one line longer than the limit, still arriving",
			body: "data: " + strings.Repeat("x", 32<<10) + "\n\n",
		},
		{
			name: "many short lines adding up to more than the limit",
			body: strings.Repeat("data: "+strings.Repeat("x", 100)+"\n", 40) + "\n",
		},
		{
			name: "a line that never ends",
			body: "data: " + strings.Repeat("x", 32<<10),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _ := serveRaw(t, tc.body)
			reader := openStream(t, server.URL, "/", func(o *SSEDialOptions) { o.ReadLimit = 1 << 10 })

			ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
			defer cancel()
			if _, err := reader.Next(ctx); !errors.Is(err, errSSEEventTooLarge) {
				t.Errorf("Next() = %v, want the event refused for its size", err)
			}
		})
	}
}
