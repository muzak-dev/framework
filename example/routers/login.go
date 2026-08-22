package routers

import (
	"net/http"
	"time"

	"badele"
	"badele-example/handlers"
)

// Auth returns the router for signing in.
func Auth() *badele.Router {
	r := badele.NewRouter(badele.WithTags("auth"))

	r.Get("/login", handlers.LoginForm,
		badele.Summary("Serve the sign-in form"))

	r.Post("/login/", handlers.Login,
		badele.Summary("Exchange a username and password for a session"),
		badele.WithResponseDoc(http.StatusUnauthorized, "The username or password is incorrect"),
		// Stricter than the rest of the application, because guessing a
		// password is the one request worth making a hundred times a minute.
		// The quotas declared here replace the inherited ones rather than
		// adding to them, and the count runs before the handler, so a wrong
		// password costs the same budget as a right one.
		badele.RateLimit(badele.Quota{Name: "login", Window: time.Minute, Limit: 5}))

	return r
}
