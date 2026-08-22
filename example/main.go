// Command example is a runnable Badele service, laid out the way a real one
// would be.
//
// This file does one thing: it composes. Configuration is read, resources are
// constructed and published, routers are mounted, and the server runs. Every
// piece of behaviour lives in a package of its own:
//
//	core/       configuration, guards, value dependencies, managed resources
//	schemas/    the request and response models the API exposes
//	handlers/   the functions that answer requests
//	routers/    which handler answers which path
//
// The dependencies point one way. Routers know handlers, handlers know schemas
// and core, and core knows nothing about any of them, so each package can be
// tested on its own.
//
// Run it and open http://localhost:8080/docs:
//
//	go run .
//
// Every route sits below the application-wide guard, so a working request
// carries a token:
//
//	curl 'http://localhost:8080/users/me?token=jessica'
package main

import (
	"log"
	"net/http"

	"badele"
	"badele-example/core"
	"badele-example/routers"
)

func main() {
	settings := core.LoadSettings()

	// Resources are constructed here and published to every handler. The model
	// registry implements badele.Lifecycle, so Badele discovers it; the store
	// does not, so it supplies the closure form instead. Either way both are
	// started before the socket opens and released after it drains.
	models := core.NewModelRegistry()
	store := core.NewItemStore()

	app := badele.New(badele.AppOptions{
		Title:       settings.AppName,
		Version:     "1.0.0",
		Description: "The Bigger Applications example, rebuilt on Badele.",
		Contact:     &badele.Contact{Email: settings.AdminEmail},
		Addr:        settings.Addr,
	},
		badele.WithDependencies(core.GetQueryToken),
		badele.WithSingleton(settings),
		badele.WithSingleton(models),
		badele.WithSingleton(store, store.Lifecycle()),
	)

	// Middleware installed here runs inside the built-in chain, so it already
	// has a request identifier and is already covered by panic recovery.
	// ProcessTime is outermost of the two, so the duration it reports includes
	// the time spent compressing.
	app.Use(core.ProcessTime())
	app.Use(badele.Compress(badele.CompressionOptions{}))

	app.Include(routers.Users())
	app.Include(routers.Items())
	app.Include(routers.Meta())
	app.Include(routers.Uploads())
	app.Include(routers.Auth())
	app.Include(routers.Feed())

	// The admin router is written without a prefix or a guard. Both are applied
	// here, which is what keeps that router reusable and puts the security
	// decision somewhere a reviewer will find it.
	app.Include(routers.Admin(),
		badele.WithPrefix("/admin"),
		badele.WithTags("admin"),
		badele.WithDependencies(core.GetTokenHeader(settings)),
		badele.WithResponseDoc(http.StatusTeapot, "I'm a teapot"),
	)

	// The built frontend is served last: every route above is matched first,
	// so mounting at the root cannot shadow the API. The directory here is
	// what a frontend build tool would have written.
	app.Frontend("/", badele.FrontendOptions{Dir: "dist"})

	if err := app.RunSignals(); err != nil {
		log.Fatal(err)
	}
}
