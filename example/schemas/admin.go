package schemas

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
