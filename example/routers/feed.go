package routers

import (
	"net/http"

	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Feed returns the router for the reader's feed.
func Feed() *muzak.Router {
	r := muzak.NewRouter(muzak.WithTags("feed"))

	// The conditional answer carries no body at all, so it is documented with
	// the model that says so rather than with the error envelope a returned
	// error would have produced.
	r.Get("/feed", handlers.Feed,
		muzak.Summary("Return the reader's feed"),
		muzak.WithResponseModel[muzak.Empty](http.StatusNotModified, "The feed has not changed since If-Modified-Since"))

	return r
}
