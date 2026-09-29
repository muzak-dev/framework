package routers

import (
	"net/http"

	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Users returns the users router, tagged so its operations are grouped together
// in the generated documentation.
//
// It is also filed under the "Accounts" category, which the sign-in router
// shares: a tag names what an operation is about, and a category is the one
// heading a documentation sidebar lists a whole router beneath.
func Users() *muzak.Router {
	r := muzak.NewRouter(muzak.WithTags("users"), muzak.WithCategory("Accounts"))

	r.Get("/users/", handlers.ListUsers,
		muzak.Title("List Users"),
		muzak.Summary("List users"))

	r.Get("/users/me", handlers.CurrentUser,
		muzak.Title("Fetch My Profile"),
		muzak.Summary("Read the authenticated user"))

	r.Get("/users/{username}", handlers.ReadUser,
		muzak.Title("Fetch User Profile"),
		muzak.Summary("Read a user by name"))

	r.Post("/users/", handlers.CreateUser,
		muzak.Title("Register A User"),
		muzak.Summary("Register a user"),
		muzak.Status(http.StatusCreated))

	return r
}
