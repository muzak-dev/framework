package routers

import (
	"net/http"

	"badele"
	"badele-example/handlers"
)

// Feed returns the router for the reader's feed.
func Feed() *badele.Router {
	r := badele.NewRouter(badele.WithTags("feed"))

	r.Get("/feed", handlers.Feed,
		badele.Summary("Return the reader's feed"),
		badele.WithResponseDoc(http.StatusNotModified, "The feed has not changed since If-Modified-Since"))

	return r
}
