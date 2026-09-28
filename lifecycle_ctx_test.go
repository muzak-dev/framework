package muzak

import (
	"context"
	"testing"
)

// TestStartContextOutlivesStart is the regression test for a context that was
// cancelled the moment Start returned: a component that keeps it for a
// background worker, which is what WithLifecycle offers components for, had
// the worker cancelled during boot. The context now lives until the
// components are stopped.
func TestStartContextOutlivesStart(t *testing.T) {
	t.Parallel()
	var saved context.Context
	app := New(quietOptions(), WithLifecycle(NewLifecycle("worker",
		func(ctx context.Context) error { saved = ctx; return nil },
		nil,
	)))

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	if err := saved.Err(); err != nil {
		t.Fatalf("the context handed to Start is already done after a successful start: %v", err)
	}
	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatalf("StopLifecycle = %v", err)
	}
	if saved.Err() == nil {
		t.Error("the context handed to Start outlived the components' Stop")
	}
}

// TestStartContextIsCancelledWithItsParent keeps the other end of the contract:
// a caller that cancels the context it started the components with still ends
// the workers that kept it.
func TestStartContextIsCancelledWithItsParent(t *testing.T) {
	t.Parallel()
	var saved context.Context
	app := New(quietOptions(), WithLifecycle(NewLifecycle("worker",
		func(ctx context.Context) error { saved = ctx; return nil },
		nil,
	)))
	parent, cancel := context.WithCancel(context.Background())
	if err := app.StartLifecycle(parent); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	t.Cleanup(func() { _ = app.StopLifecycle(context.Background()) })
	cancel()
	if saved.Err() == nil {
		t.Error("cancelling the parent did not reach the component's context")
	}
}
