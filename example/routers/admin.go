package routers

import (
	"net/http"

	"badele"
	"badele-example/handlers"
)

// Admin returns the admin router.
//
// It declares no prefix and no guard of its own; main supplies both where the
// router is included.
func Admin() *badele.Router {
	r := badele.NewRouter()

	r.Post("/", handlers.AdminAction,
		badele.Status(http.StatusCreated),
		badele.Summary("Admin action"),
		badele.Description("Performs a privileged action. Requires the shared admin token."))

	return r
}
