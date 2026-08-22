package routers

import (
	"badele"
	"badele-example/handlers"
)

// Meta returns the router for the service's own endpoints: what it is
// configured with, what it can predict, and whether it is alive.
func Meta() *badele.Router {
	r := badele.NewRouter(badele.WithTags("meta"))

	r.Get("/info", handlers.Info,
		badele.Summary("Report the running configuration"))

	r.Get("/predict", handlers.Predict,
		badele.Summary("Run the loaded model"))

	r.Get("/healthz", handlers.Health,
		badele.Summary("Liveness probe"),
		// Routable, but left out of the documentation.
		badele.Hidden(),
		// A monitor polling every second is the one client that should never
		// be told to slow down.
		badele.SkipRateLimit())

	return r
}
