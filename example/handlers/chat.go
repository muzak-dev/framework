package handlers

import (
	"strings"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/example/schemas"
)

// tokenDelay stands in for the time a real model would take to produce the
// next token.
const tokenDelay = 80 * time.Millisecond

// StreamChat answers a prompt one token at a time, which is the shape every
// chat completion API streams in.
//
// The events carry text rather than JSON, so the stream is declared with
// muzak.Empty as its model: there is no schema to describe, and the generated
// document says so instead of describing one that does not exist. Sending is
// the same either way.
//
//	curl -N -X POST 'http://localhost:8080/chat/stream?token=jessica' \
//	     -H 'Content-Type: application/json' -d '{"text":"what is a plumbus"}'
func StreamChat(ctx *muzak.Context, in schemas.ChatIn, stream *muzak.SSEStream[muzak.Empty]) error {
	for word := range strings.SplitSeq(in.Text, " ") {
		select {
		case <-stream.Context().Done():
			// The client closed the tab, or the server is shutting down. There
			// is no one left to answer.
			return nil
		case <-time.After(tokenDelay):
		}
		if err := stream.SendEvent(muzak.SSEEvent[muzak.Empty]{Name: "token", Text: word}); err != nil {
			return err
		}
	}
	// The sentinel some clients expect at the end of a completion. It is text
	// rather than a value, which is what Text is for.
	return stream.SendEvent(muzak.SSEEvent[muzak.Empty]{Name: "done", Text: "[DONE]"})
}
