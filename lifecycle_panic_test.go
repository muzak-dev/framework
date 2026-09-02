package muzak_test

import (
	"context"
	"strings"
	"testing"

	muzak "muzak.dev/framework"
)

// A component's Start and Stop are application code running on the framework's
// goroutines. Stop in particular runs after the drain, when every request has
// been answered and there is nothing left to report a crash to, so a panic
// there would take the process down at the one moment where doing so achieves
// nothing.

type panicComponent struct {
	name       string
	onStart    bool
	onStop     bool
	stopCalled *bool
}

func (c *panicComponent) Name() string { return c.name }

func (c *panicComponent) Start(context.Context) error {
	if c.onStart {
		panic("start went wrong")
	}
	return nil
}

func (c *panicComponent) Stop(context.Context) error {
	if c.stopCalled != nil {
		*c.stopCalled = true
	}
	if c.onStop {
		panic("stop went wrong")
	}
	return nil
}

func panicApp(t *testing.T, components ...muzak.Lifecycle) *muzak.App {
	t.Helper()

	app := muzak.New(muzak.AppOptions{
		Title: "Lifecycle", Version: "1.0.0", Addr: ":0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	},
		muzak.WithLifecycle(components...),
	)
	app.Get("/", func(ctx *muzak.Context, _ muzak.Empty) (resolverOut, error) {
		return resolverOut{OK: true}, nil
	})
	if err := app.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return app
}

func TestLifecycleStopPanicBecomesAnError(t *testing.T) {
	app := panicApp(t, &panicComponent{name: "bad", onStop: true})
	ctx := context.Background()

	if err := app.StartLifecycle(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	err := app.StopLifecycle(ctx)
	if err == nil {
		t.Fatal("a panicking Stop returned no error")
	}
	if !strings.Contains(err.Error(), "panicked") || !strings.Contains(err.Error(), "bad") {
		t.Errorf("err = %v, want it to name the component and say it panicked", err)
	}
}

// One component crashing must not stop the others being released: a pool left
// open because a neighbour panicked is a leak with no owner.
func TestLifecycleStopPanicDoesNotStrandOtherComponents(t *testing.T) {
	var goodStopped bool

	app := panicApp(t,
		&panicComponent{name: "bad", onStop: true},
		&panicComponent{name: "good", stopCalled: &goodStopped},
	)
	ctx := context.Background()

	if err := app.StartLifecycle(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := app.StopLifecycle(ctx); err == nil {
		t.Fatal("expected the panicking component to be reported")
	}

	if !goodStopped {
		t.Error("a component was left running because another one panicked")
	}
}

func TestLifecycleStartPanicFailsTheStartUp(t *testing.T) {
	var goodStopped bool

	app := panicApp(t,
		&panicComponent{name: "bad", onStart: true},
		&panicComponent{name: "good", stopCalled: &goodStopped},
	)

	err := app.StartLifecycle(context.Background())
	if err == nil {
		t.Fatal("a panicking Start returned no error")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("err = %v, want it to say the component panicked", err)
	}

	// The existing contract: every component that did come up is released.
	if !goodStopped {
		t.Error("a component that started was not released after the start-up failed")
	}
}
