package routers

import (
	"net/http"

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
		badele.WithResponseDoc(http.StatusUnauthorized, "The username or password is incorrect"))

	return r
}
