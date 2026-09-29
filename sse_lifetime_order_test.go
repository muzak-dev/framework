package muzak

import (
	"errors"
	"io"
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
