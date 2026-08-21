package routers

import (
	"net/http"

	"badele"
	"badele-example/handlers"
)

// Users returns the users router, tagged so its operations are grouped together
// in the generated documentation.
func Users() *badele.Router {
	r := badele.NewRouter(badele.WithTags("users"))

	r.Get("/users/", handlers.ListUsers,
		badele.Summary("List users"))

	r.Get("/users/me", handlers.CurrentUser,
		badele.Summary("Read the authenticated user"))

	r.Get("/users/{username}", handlers.ReadUser,
		badele.Summary("Read a user by name"))

	r.Post("/users/", handlers.CreateUser,
		badele.Summary("Register a user"),
		badele.Status(http.StatusCreated))

	return r
}
