package core

import (
	"context"
	"sync"
)

// ModelRegistry holds the prediction models the service serves.
//
// It implements muzak.Lifecycle, so publishing it with muzak.WithSingleton is
// enough for Muzak to load it before the server accepts traffic and release it
// after the server has drained. This is the shape a real resource takes: a
// database pool, a cache client and a model registry all need the same
// treatment, and none of them should be constructed inside a handler.
type ModelRegistry struct {
	mu     sync.RWMutex
	models map[string]func(float64) float64
}

// NewModelRegistry returns an empty registry. The models themselves are loaded
// in Start, not here, so that construction stays cheap and failure has a place
// to be reported.
func NewModelRegistry() *ModelRegistry {
	return &ModelRegistry{models: map[string]func(float64) float64{}}
}

// Name identifies the component in start-up and shutdown logs.
func (r *ModelRegistry) Name() string { return "ml-model" }

// Start loads the models. A real implementation would read weights from disk or
// object storage and should honour ctx, which Muzak cancels as soon as a
// sibling component fails.
func (r *ModelRegistry) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models["answer_to_everything"] = func(x float64) float64 { return x * 42 }
	return nil
}

// Stop releases the models. It runs only after the HTTP server has finished
// draining, so a request that is still predicting keeps working right up to the
// end.
func (r *ModelRegistry) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.models)
	return nil
}

// Predict runs the named model and reports whether it was loaded.
func (r *ModelRegistry) Predict(name string, x float64) (float64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	model, ready := r.models[name]
	if !ready {
		return 0, false
	}
	return model(x), true
}
