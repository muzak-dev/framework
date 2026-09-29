package muzak

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// A client that appends a zone to its own IPv6 address ("%a", "%b", ...) is a
// new string each time, so a per-address cap keyed on the text would give it a
// budget for every spelling. The zone means nothing to this host and is dropped
// before an address becomes a key, even when the whole address is the key.
func TestSSEPerClientCapIgnoresAnIPv6Zone(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.SSE = SSEOptions{MaxStreams: 10, MaxStreamsPerIP: 1, KeepAlive: -1}
	opts.ClientIP = ClientIPOptions{
		TrustedProxies:       []string{"127.0.0.1/32"},
		ConnectionIPv6Prefix: 128,
	}
	release := make(chan struct{})
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: "held"}); err != nil {
				return err
			}
			<-release
			return nil
		})
	})
	defer close(release)

	from := func(ip string) func(*SSEDialOptions) {
		return func(o *SSEDialOptions) { o.Header = http.Header{"X-Forwarded-For": []string{ip}} }
	}
	first := openStream(t, server.URL, "/stream", from("2001:db8::1%a"))
	nextEvent(t, first)
	for _, spelling := range []string{"2001:db8::1%b", "2001:db8::1", "2001:db8::1%eth0"} {
		if _, refused := tryStream(t, server.URL, "/stream", from(spelling)); refused.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("the address spelled %q was admitted as a client of its own (status %d)", spelling, refused.StatusCode)
		}
	}
}

// An event that arrives inside the read timeout is never cut off by the clock
// of the one before it, however many there are and however close to the limit
// they run. The timer that bounds an event is stopped when the event completes
// and a reader that is idle between events has none, so a stale one has nothing
// to fire into.
func TestSSEReaderReadTimeoutNeverOutlivesItsEvent(t *testing.T) {
	t.Parallel()
	// Generous enough that a busy scheduler does not stand in for the race
	// this looks for: the pauses below are fractions of it.
	const readTimeout = 600 * time.Millisecond
	pipeReader, pipeWriter := io.Pipe()
	reader := newSSEReader(pipeReader, SSEDialOptions{ReadTimeout: readTimeout})
	t.Cleanup(func() { _ = reader.Close() })

	const events = 10
	go func() {
		defer pipeWriter.Close()
		for range events {
			// Half an event now and the rest a good part of the timeout later,
			// then a pause during which the reader is waiting, not reading.
			_, _ = io.WriteString(pipeWriter, "data: x\n")
			time.Sleep(readTimeout / 4)
			_, _ = io.WriteString(pipeWriter, "\n")
			time.Sleep(readTimeout / 2)
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for i := range events {
		message, err := reader.Next(ctx)
		if err != nil {
			t.Fatalf("event %d: Next() = %v", i, err)
		}
		if message.Data != "x" {
			t.Fatalf("event %d: data = %q", i, message.Data)
		}
	}
	if _, err := reader.Next(ctx); !errors.Is(err, io.EOF) && !errors.Is(err, ErrSSEStreamEnded) {
		t.Fatalf("after the last event Next() = %v, want the end of the stream", err)
	}
}
