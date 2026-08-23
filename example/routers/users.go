package routers

import (
	"net/http"

	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Users returns the users router, tagged so its operations are grouped together
// in the generated documentation.
func Users() *muzak.Router {
	r := muzak.NewRouter(muzak.WithTags("users"))

	r.Get("/users/", handlers.ListUsers,
		muzak.Summary("List users"))

	r.Get("/users/me", handlers.CurrentUser,
		muzak.Summary("Read the authenticated user"))

	r.Get("/users/{username}", handlers.ReadUser,
		muzak.Summary("Read a user by name"))

	r.Post("/users/", handlers.CreateUser,
		muzak.Summary("Register a user"),
		muzak.Status(http.StatusCreated))

	return r
}
