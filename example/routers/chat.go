package routers

import (
	"net/http"
	"time"

	"badele"
	"badele-example/handlers"
)

// Chat returns the router for the streamed chat completion.
func Chat() *badele.Router {
	r := badele.NewRouter(badele.WithTags("chat"))

	// An event stream is not tied to GET. Router.SSEHandle registers one for
	// any method, and the request body binds and validates as it would
	// anywhere else, before a byte of the stream is written.
	r.SSEHandle(http.MethodPost, "/chat/stream", handlers.StreamChat,
		badele.Summary("Answer a prompt one token at a time"),
		badele.WithSSE(badele.SSEOptions{
			// A completion is never quiet for long, so the keepalive is only
			// there for the pause before the first token.
			KeepAlive: 10 * time.Second,
		}))

	return r
}
