package routers

import (
	"net/http"

	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Admin returns the admin router.
//
// It declares no prefix and no guard of its own; main supplies both where the
// router is included.
func Admin() *muzak.Router {
	r := muzak.NewRouter()

	r.Post("/", handlers.AdminAction,
		muzak.Status(http.StatusCreated),
		muzak.Summary("Admin action"),
		muzak.Description("Performs a privileged action. Requires the shared admin token."))

	return r
}
