package muzak

import (
	"net/http"
	"testing"
	"time"
)

// Named collection types that contain themselves. Nothing in them is a struct,
// so the guards written for a struct that contains itself never fired, and
// registering a route with one hung or overflowed the stack.
type recursiveMap map[string]recursiveMap

type recursiveList []recursiveList

type recursiveBody struct {
	Tree recursiveMap `json:"tree"`
}

type recursiveListBody struct {
	Nodes recursiveList `json:"nodes"`
}

type recursiveQuery struct {
	Nodes recursiveList `query:"nodes"`
}

// buildWithin builds an application, failing the test rather than hanging it
// when the build does not finish.
func buildWithin(t *testing.T, app *App) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- app.Build() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("building the application did not finish")
		return nil
	}
}

func TestARecursiveBodyTypeBuildsAndIsDescribed(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/map", func(ctx *Context, in recursiveBody) (recursiveBody, error) { return in, nil })
	app.Post("/list", func(ctx *Context, in recursiveListBody) (recursiveListBody, error) { return in, nil })
	if err := buildWithin(t, app); err != nil {
		t.Fatalf("a body holding a recursive collection was refused: %v", err)
	}

	assertStatus(t, do(t, app, "POST", "/map", `{"tree":{"a":{"b":{}}}}`), http.StatusOK)
	assertStatus(t, do(t, app, "POST", "/list", `{"nodes":[[[]],[]]}`), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/openapi.json"), http.StatusOK)
}

func TestARecursiveParameterTypeIsRefusedWhenTheRouteIsRegistered(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/q", func(ctx *Context, in recursiveQuery) (Empty, error) { return Empty{}, nil })
	if err := buildWithin(t, app); err == nil {
		t.Fatal("a query parameter that contains itself was accepted")
	}
}
