package muzak

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stallingServer accepts a request and says nothing, the way a server that has
// hung does. It is released when the test ends so that closing it does not wait
// for a handler that would never return.
func stallingServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

func TestSSEDialGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	t.Parallel()
	server := stallingServer(t, func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []struct {
		name string
		opts SSEDialOptions
	}{
		{"its own timeout", SSEDialOptions{HandshakeTimeout: 150 * time.Millisecond}},
		{"the timeout of the client it was given", SSEDialOptions{HTTPClient: &http.Client{Timeout: 150 * time.Millisecond}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Nothing but the timeout ends this: the context has no deadline.
			start := time.Now()
			reader, _, err := SSEDial(context.Background(), server.URL, tc.opts)
			if err == nil {
				reader.Close()
				t.Fatal("SSEDial() = nil for a server that never answered")
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("SSEDial() gave up after %v, want about 150ms", elapsed)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("SSEDial() = %v, want it to report a deadline", err)
			}
		})
	}
}

func TestSSEDialGivesUpOnARefusalThatNeverFinishes(t *testing.T) {
	t.Parallel()
	// The response is not a stream, so its body is read into memory to be
	// returned with the error, and a server that trickles it holds the reader
	// there for as long as it likes unless the handshake's bound covers it.
	server := stallingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "not a stream")
		w.(http.Flusher).Flush()
	})
	// The headers and the first part of the body have to arrive inside the
	// bound for there to be anything to return, and on a loaded machine they
	// did not always arrive inside 150ms. The bound is what ends the test, so
	// it is no longer than it needs to be for that.
	const bound = time.Second
	start := time.Now()
	_, response, err := SSEDial(context.Background(), server.URL, SSEDialOptions{HandshakeTimeout: bound})
	if err == nil {
		t.Fatal("SSEDial() = nil for a response that is not a stream")
	}
	if elapsed := time.Since(start); elapsed > bound+sseTestTimeout {
		t.Errorf("SSEDial() gave up after %v, want about %v", elapsed, bound)
	}
	if response == nil {
		t.Fatal("the refused response was not returned")
	}
	if body, _ := io.ReadAll(response.Body); string(body) != "not a stream" {
		t.Errorf("the refused body = %q, want what arrived before the bound", body)
	}
}

func TestSSEDialHandshakeTimeoutDoesNotEndTheStream(t *testing.T) {
	t.Parallel()
	// The bound is for getting the stream open. A stream that then says nothing
	// for longer than it is what a stream is for.
	//
	// The event is held back until the bound has certainly run out, counted
	// from before the dial began, rather than sent at a fixed moment: a bound
	// of 100ms and an event at 500ms left a loaded machine 100ms to open the
	// stream, which it did not always manage.
	const bound = 500 * time.Millisecond
	late := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-late:
			_, _ = io.WriteString(w, "data: late\n\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	start := time.Now()
	reader, _, err := SSEDial(context.Background(), server.URL, SSEDialOptions{HandshakeTimeout: bound})
	if err != nil {
		t.Fatalf("SSEDial() = %v", err)
	}
	defer reader.Close()
	time.Sleep(bound - time.Since(start) + 50*time.Millisecond)
	close(late)
	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	message, err := reader.Next(ctx)
	if err != nil {
		t.Fatalf("Next() = %v, want the event that arrived after the handshake bound", err)
	}
	if message.Data != "late" {
		t.Errorf("Data = %q, want %q", message.Data, "late")
	}
}
