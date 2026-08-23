package schemas

import (
	"muzak.dev/framework"
)

// AdminActionIn is the JSON body for the administrative action.
type AdminActionIn struct {
	// Name identifies what is being acted on.
	Name string `json:"name" doc:"The name to act on"`
}

// AdminActionOut is the response model for the administrative action.
type AdminActionOut struct {
	// Name echoes what was acted on.
	Name string `json:"name"`
	// Message describes what happened.
	Message string `json:"message"`
}

// Validate constrains the administrative action.
func (in *AdminActionIn) Validate(v *muzak.Validation) {
	v.String(&in.Name).Trim().Required().MinLen(1).MaxLen(80)
}
