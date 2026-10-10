package routers

import (
	"net/http"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/example/core"
	"muzak.dev/framework/example/handlers"
)

// Items returns the items router, filed under the "Catalog" category.
func Items() *muzak.Router {
	r := muzak.NewRouter(muzak.WithTags("items"), muzak.WithCategory("Catalog"),
		// This is the one router whose routes resolve a caller, so it is the
		// one router where the budget is worth spending per caller rather than
		// per address. Deferring the count is what lets the tracker see the
		// resolved user; the quotas themselves are inherited unchanged.
		muzak.WithRateLimit(muzak.RateLimitOptions{AfterDependencies: true}))

	r.Get("/items/", handlers.ListItems,
		muzak.Title("List Items"),
		muzak.Summary("List items"))

	r.Get("/items/{item_id}", handlers.ReadItem,
		muzak.Title("Fetch An Item"),
		muzak.Summary("Read an item"),
		muzak.WithResponseDoc(http.StatusNotFound, "The item does not exist"),
		// Only this route resolves the caller, so only this route pays for it.
		muzak.Needs(core.GetCurrentUser))

	r.Post("/items/", handlers.CreateItem,
		muzak.Summary("Create an item"),
		muzak.Status(http.StatusCreated),
		muzak.WithResponseDoc(http.StatusConflict, "An item with that identifier already exists"))

	r.Put("/items/{item_id}", handlers.RenameItem,
		muzak.Summary("Rename an item"),
		muzak.WithResponseDoc(http.StatusNotFound, "The item does not exist"))

	// An event stream route is declared like any other too, and answers 200
	// rather than upgrading: the guards run and the input binds before a byte
	// of the stream is written, and the handler owns the stream until it
	// returns.
	r.SSE("/items/stream", handlers.StreamItems,
		muzak.Title("Follow Item Changes"),
		muzak.Summary("Follow every change to the items"),
		muzak.WithSSE(muzak.SSEOptions{
			// A stream that says nothing for long enough is closed by proxies
			// that believe it to be idle, so a comment goes out instead.
			KeepAlive: 15 * time.Second,
			// A browser reconnects on its own when a stream ends, and this is
			// how soon.
			Retry: 2 * time.Second,
		}))

	// A WebSocket route is declared like any other: the input is bound from
	// the handshake, the dependency resolves before the upgrade, and the
	// handler owns the connection until it returns.
	r.WS("/items/{item_id}/ws", handlers.ItemSocket,
		muzak.Title("Talk To An Item"),
		muzak.Summary("Talk to an item over a WebSocket"),
		muzak.Needs(core.GetCaller),
		muzak.WithWebSocket(muzak.WSOptions{
			ReadLimit:    64 << 10,
			PingInterval: 30 * time.Second,
			// ReadLimit bounds what one message costs and MaxConnections
			// bounds how many peers there are; this is what bounds a peer that
			// stays inside both and never pauses.
			MessageLimits: []muzak.Quota{
				{Name: "ws-messages", Window: time.Second, Limit: 10},
			},
		}))

	return r
}
