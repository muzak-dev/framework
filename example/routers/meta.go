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

	r.Get("/greeting", handlers.Greeting,
		muzak.Summary("Greet the caller in their own language"),
		muzak.Description(
			"Every string in the answer is translated, including the ones the framework "+
				"produces when the request is rejected. Add ?locale=es, or send an "+
				"Accept-Language header, to see it in Spanish."))

	r.Post("/items/translated", handlers.AddCatalogueItem,
		muzak.Summary("Add an item, refusing in the caller's language"),
		muzak.Status(201))

	r.Get("/healthz", handlers.Health,
		muzak.Summary("Liveness probe"),
		// Routable, but left out of the documentation.
		muzak.Hidden(),
		// A monitor polling every second is the one client that should never
		// be told to slow down.
		muzak.SkipRateLimit())

	return r
}
