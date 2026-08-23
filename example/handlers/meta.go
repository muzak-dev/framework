package handlers

import (
	"net/http"

	"muzak.dev/framework"
	"muzak.dev/framework/example/core"
	"muzak.dev/framework/example/schemas"
)

// Info reports the loaded settings. The singleton is retrieved by type, with no
// cast, and only the three fields InfoOut declares can reach the client.
func Info(ctx *muzak.Context, _ muzak.Empty) (schemas.InfoOut, error) {
	s := muzak.From[core.Settings](ctx)
	return schemas.InfoOut{
		AppName:      s.AppName,
		AdminEmail:   s.AdminEmail,
		ItemsPerUser: s.ItemsPerUser,
	}, nil
}

// Predict runs the model the lifecycle component loaded.
func Predict(ctx *muzak.Context, in schemas.PredictParams) (schemas.PredictOut, error) {
	models := muzak.From[*core.ModelRegistry](ctx)

	result, ready := models.Predict("answer_to_everything", in.X)
	if !ready {
		return schemas.PredictOut{}, muzak.NewHTTPError(
			http.StatusServiceUnavailable, "the model is not loaded")
	}
	return schemas.PredictOut{Result: result}, nil
}

// Health answers the liveness probe. It is hidden from the documentation but
// still routable.
func Health(ctx *muzak.Context, _ muzak.Empty) (schemas.HealthOut, error) {
	return schemas.HealthOut{Status: "ok"}, nil
}
