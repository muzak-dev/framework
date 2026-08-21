// Package admin exposes privileged routes.
//
// The router declares no prefix and no guard of its own. The application
// supplies both where it includes the router, which is what keeps this package
// reusable and keeps the security decision in one visible place.
package admin

import (
	"net/http"

	"badele"
)

// CreateAdminIn is the JSON body for the administrative action.
type CreateAdminIn struct {
	// Name identifies what is being acted on.
	Name string `json:"name" doc:"The name to act on"`
}

// CreateAdminOut is the response model for the administrative action.
type CreateAdminOut struct {
	// Name echoes what was acted on.
	Name string `json:"name"`
	// Message describes what happened.
	Message string `json:"message"`
}

// NewRouter returns the admin router.
func NewRouter() *badele.Router {
	r := badele.NewRouter()

	r.Post("/", update,
		badele.Status(http.StatusCreated),
		badele.Summary("Admin action"),
		badele.Description("Performs a privileged action. Requires the shared admin token."))

	return r
}

// update performs the privileged action.
func update(ctx *badele.Context, in CreateAdminIn) (CreateAdminOut, error) {
	return CreateAdminOut{Name: in.Name, Message: "Admin getting schwifty"}, nil
}
