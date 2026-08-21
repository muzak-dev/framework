package handlers

import (
	"net/http"

	"badele"
	"badele-example/core"
	"badele-example/schemas"
)

// Info reports the loaded settings. The singleton is retrieved by type, with no
// cast, and only the three fields InfoOut declares can reach the client.
func Info(ctx *badele.Context, _ badele.Empty) (schemas.InfoOut, error) {
	s := badele.From[core.Settings](ctx)
	return schemas.InfoOut{
		AppName:      s.AppName,
		AdminEmail:   s.AdminEmail,
		ItemsPerUser: s.ItemsPerUser,
	}, nil
}

// Predict runs the model the lifecycle component loaded.
func Predict(ctx *badele.Context, in schemas.PredictParams) (schemas.PredictOut, error) {
	models := badele.From[*core.ModelRegistry](ctx)

	result, ready := models.Predict("answer_to_everything", in.X)
	if !ready {
		return schemas.PredictOut{}, badele.NewHTTPError(
			http.StatusServiceUnavailable, "the model is not loaded")
	}
	return schemas.PredictOut{Result: result}, nil
}

// Health answers the liveness probe. It is hidden from the documentation but
// still routable.
func Health(ctx *badele.Context, _ badele.Empty) (schemas.HealthOut, error) {
	return schemas.HealthOut{Status: "ok"}, nil
}
