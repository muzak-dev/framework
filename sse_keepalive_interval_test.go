package muzak

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// A keepalive asked for every interval arrives every interval. Measured from
// fixed ticks it skipped whenever its own previous write had landed just after
// the tick before, so it arrived at 1, 3 and 4 intervals: twice the silence a
// proxy's idle timeout was being kept off with.
func TestSSEKeepaliveArrivesEveryInterval(t *testing.T) {
	t.Parallel()
	const interval = 100 * time.Millisecond
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		<-stream.Context().Done()
		return nil
	}, WithSSE(SSEOptions{KeepAlive: interval}))
	mustBuild(t, app)
	srv := startSSEServer(t, app, false)

	reader, _, err := SSEDial(t.Context(), srv.URL+"/stream",
		SSEDialOptions{HTTPClient: srv.Client(), KeepComments: true})
	if err != nil {
		t.Fatalf("SSEDial() = %v", err)
	}
	defer reader.Close()

	const window = 1500 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()
	var comments int
	for {
		message, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			break
		}
		if err != nil {
			t.Fatalf("Next() = %v", err)
		}
		if message.Comment == sseKeepAliveComment {
			comments++
		}
	}
	// Fifteen fit in the window. Skipping every other tick gave seven or eight.
	if comments < 11 {
		t.Errorf("%d keepalives in %v at an interval of %v, want about %d", comments, window, interval, window/interval)
	}
}
