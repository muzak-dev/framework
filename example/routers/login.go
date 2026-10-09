package routers

import (
	"net/http"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Auth returns the router for signing in.
func Auth() *muzak.Router {
	r := muzak.NewRouter(muzak.WithTags("auth"), muzak.WithCategory("Accounts"))

	r.Get("/login", handlers.LoginForm,
		muzak.Summary("Serve the sign-in form"))

	r.Post("/login/", handlers.Login,
		muzak.Summary("Exchange a username and password for a session"),
		muzak.WithResponseDoc(http.StatusUnauthorized, "The username or password is incorrect"),
		// Stricter than the rest of the application, because guessing a
		// password is the one request worth making a hundred times a minute.
		// The quotas declared here replace the inherited ones rather than
		// adding to them, and the count runs before the handler, so a wrong
		// password costs the same budget as a right one.
		muzak.RateLimit(muzak.Quota{Name: "login", Window: time.Minute, Limit: 5}))

	r.Post("/logout/", handlers.Logout,
		muzak.Summary("End the session"))

	return r
}
