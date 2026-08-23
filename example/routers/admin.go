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

	// The tag here is the route's own. It adds to whatever the router is
	// included under, so this operation is documented in both groups.
	r.Post("/", handlers.AdminAction,
		muzak.Status(http.StatusCreated),
		muzak.WithTags("audit"),
		muzak.Summary("Admin action"),
		muzak.Description("Performs a privileged action. Requires the shared admin token."))

	return r
}
