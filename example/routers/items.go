package routers

import (
	"net/http"
	"time"

	"badele"
	"badele-example/core"
	"badele-example/handlers"
)

// Items returns the items router.
func Items() *badele.Router {
	r := badele.NewRouter(badele.WithTags("items"))

	r.Get("/items/", handlers.ListItems,
		badele.Summary("List items"))

	r.Get("/items/{item_id}", handlers.ReadItem,
		badele.Summary("Read an item"),
		badele.WithResponseDoc(http.StatusNotFound, "The item does not exist"),
		// Only this route resolves the caller, so only this route pays for it.
		badele.Needs(core.GetCurrentUser))

	r.Post("/items/", handlers.CreateItem,
		badele.Summary("Create an item"),
		badele.Status(http.StatusCreated),
		badele.WithResponseDoc(http.StatusConflict, "An item with that identifier already exists"))

	r.Put("/items/{item_id}", handlers.RenameItem,
		badele.Summary("Rename an item"),
		badele.WithResponseDoc(http.StatusNotFound, "The item does not exist"))

	// A WebSocket route is declared like any other: the input is bound from
	// the handshake, the dependency resolves before the upgrade, and the
	// handler owns the connection until it returns.
	r.WS("/items/{item_id}/ws", handlers.ItemSocket,
		badele.Summary("Talk to an item over a WebSocket"),
		badele.Needs(core.GetSessionOrToken),
		badele.WithWebSocket(badele.WSOptions{
			ReadLimit:    64 << 10,
			PingInterval: 30 * time.Second,
		}))

	return r
}
