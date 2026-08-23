package routers

import (
	"net/http"

	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Feed returns the router for the reader's feed.
func Feed() *muzak.Router {
	r := muzak.NewRouter(muzak.WithTags("feed"))

	r.Get("/feed", handlers.Feed,
		muzak.Summary("Return the reader's feed"),
		muzak.WithResponseDoc(http.StatusNotModified, "The feed has not changed since If-Modified-Since"))

	return r
}
