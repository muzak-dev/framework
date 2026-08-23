package routers

import (
	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Meta returns the router for the service's own endpoints: what it is
// configured with, what it can predict, and whether it is alive.
func Meta() *muzak.Router {
	r := muzak.NewRouter(muzak.WithTags("meta"))

	r.Get("/info", handlers.Info,
		muzak.Summary("Report the running configuration"))

	r.Get("/predict", handlers.Predict,
		muzak.Summary("Run the loaded model"))

	r.Get("/healthz", handlers.Health,
		muzak.Summary("Liveness probe"),
		// Routable, but left out of the documentation.
		muzak.Hidden(),
		// A monitor polling every second is the one client that should never
		// be told to slow down.
		muzak.SkipRateLimit())

	return r
}
