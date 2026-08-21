package schemas

import (
	"badele"
)

// InfoOut is the response model for the settings endpoint.
//
// It is a separate type from the configuration on purpose: the response exposes
// exactly these three fields, so adding a secret to the settings later cannot
// leak it here.
type InfoOut struct {
	AppName      string `json:"app_name"`
	AdminEmail   string `json:"admin_email"`
	ItemsPerUser int    `json:"items_per_user"`
}

// PredictParams binds the input of the prediction route.
type PredictParams struct {
	// X is the value to run through the model.
	X float64 `query:"x" doc:"The value to predict from" default:"1"`
}

// PredictOut is the response model for the prediction route.
type PredictOut struct {
	Result float64 `json:"result"`
}

// HealthOut is the response model for the liveness probe.
type HealthOut struct {
	Status string `json:"status"`
}

// Validate keeps the prediction input inside the range the model was fitted on.
func (in *PredictParams) Validate(v *badele.Validation) {
	v.Number(&in.X).Between(-1000, 1000)
}
