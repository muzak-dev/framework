// Command example is a runnable Muzak service, laid out the way a real one
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
//	go run ./cmd
//
// Every route and file sits below the token guard, so a working request
// carries a token (the documentation itself is open, so it can be read without
// one):
//
//	curl 'http://localhost:8080/users/me?token=jessica'
package main

import (
	"log"
	"net/http"

	"muzak.dev/framework"
	"muzak.dev/framework/example/core"
	"muzak.dev/framework/example/routers"
	"muzak.dev/openapi/ui"
)

func main() {
	settings := core.LoadSettings()

	// Resources are constructed here and published to every handler. The model
	// registry implements muzak.Lifecycle, so Muzak discovers it; the store
	// does not, so it supplies the closure form instead. Either way both are
	// started before the socket opens and released after it drains.
	models := core.NewModelRegistry()
	store := core.NewItemStore()

	app := muzak.New(muzak.AppOptions{
		Title:       settings.AppName,
		Version:     "1.0.0",
		Description: "The Bigger Applications example, rebuilt on Muzak.",
		Contact:     &muzak.Contact{Email: settings.AdminEmail},
		// The documentation dashboard, which is a module of its own: a service
		// that wants no UI imports nothing and carries nothing.
		DocsUI: ui.Files(),
		// The groups the reference is presented in. A router or a single route
		// joins one by naming it with WithTags; describing it here is what
		// gives the group a sentence and decides the order the documentation
		// leads with.
		Tags: []muzak.Tag{
			{Name: "users", Description: "Reading and creating the people the service knows about."},
			{Name: "items", Description: "The catalogue, including the stream and the socket that follow it."},
			{Name: "uploads", Description: "Multipart forms, read as bytes or as files."},
			{Name: "auth", Description: "Exchanging a username and password for a session."},
			{Name: "feed", Description: "What a reader sees, assembled per request."},
			{Name: "chat", Description: "A long-running answer, streamed a token at a time."},
			{Name: "admin", Description: "Privileged operations. Every one needs the staff token."},
			{Name: "audit", Description: "Anything that leaves a trace, tagged on the route itself."},
			{Name: "meta", Description: "Health and the settings the process was started with."},
		},
		Addr: settings.Addr,
		// Which address a request is attributed to. Nothing is believed from a
		// header until the proxy that wrote it is named here.
		ClientIP: muzak.ClientIPOptions{TrustedProxies: settings.TrustedProxies},
		// The language every response is written in, resolved once per request.
		// Leaving this out is what a service that answers only in English does,
		// and it then carries none of the translation machinery at all.
		I18n: core.LocaleOptions(),
	},
		// The application-wide budget, counted before any guard runs so that a
		// request a guard rejects still costs the client something. Routers
		// narrow it below where they have a reason to.
		muzak.WithRateLimit(core.RateLimitPolicy()),
		muzak.WithSingleton(settings),
		muzak.WithSingleton(models),
		muzak.WithSingleton(store, store.Lifecycle()),
	)

	// Middleware installed here runs inside the built-in chain, so it already
	// has a request identifier and is already covered by panic recovery.
	// ProcessTime is outermost of the two, so the duration it reports includes
	// the time spent compressing.
	app.Use(core.ProcessTime())
	app.Use(muzak.Compress(muzak.CompressionOptions{}))

	// The token guard is applied where each router is included rather than on
	// muzak.New. A guard given to New covers the documentation and its
	// OpenAPI document too, and the dashboard's own requests carry no token,
	// so the docs would answer 400 to everyone. Declared here it covers every
	// route and file below and leaves /docs readable.
	guarded := muzak.WithDependencies(core.GetQueryToken)

	app.Include(routers.Users(), guarded)
	app.Include(routers.Items(), guarded)
	app.Include(routers.Meta(), guarded)
	app.Include(routers.Uploads(), guarded)
	app.Include(routers.Auth(), guarded)
	app.Include(routers.Feed(), guarded)
	app.Include(routers.Chat(), guarded)

	// The admin router is written without a prefix or a guard. Both are applied
	// here, which is what keeps that router reusable and puts the security
	// decision somewhere a reviewer will find it.
	app.Include(routers.Admin(),
		muzak.WithPrefix("/admin"),
		muzak.WithTags("admin"),
		guarded,
		muzak.WithDependencies(core.GetTokenHeader(settings)),
		muzak.WithResponseDoc(http.StatusTeapot, "I'm a teapot"),
	)

	// Assets that belong to no particular route. A static mount serves what it
	// finds and nothing else, so a miss here stays a miss rather than being
	// answered with the application document by the frontend below.
	files := muzak.NewRouter()
	files.Static("/static", muzak.StaticOptions{Dir: "static"})

	// The built frontend is served last: every route above is matched first,
	// so mounting at the root cannot shadow the API. The directory here is
	// what a frontend build tool would have written.
	files.Frontend("/", muzak.FrontendOptions{Dir: "dist"})
	app.Include(files, guarded)

	if err := app.RunSignals(); err != nil {
		log.Fatal(err)
	}
}
