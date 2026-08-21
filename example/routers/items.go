package routers

import (
	"net/http"

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

	return r
}
