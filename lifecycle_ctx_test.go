package muzak

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// TestRunKeepsTheStartContextThroughTheDrain is the regression test for the
// run path breaking the promise TestStartContextOutlivesStart keeps for
// StartLifecycle. RunContext started the components with a child of its own
// context, and cancelling that context is how RunContext, and RunSignals on
// SIGTERM, are told to shut down: a worker kept on the start context was
// cancelled the moment the drain began, while requests that still used it
// were being served, and Stop was handed components whose context had already
// ended. The context now lives until the components have been stopped.
func TestRunKeepsTheStartContextThroughTheDrain(t *testing.T) {
	t.Parallel()
	var saved atomic.Pointer[context.Context]
	var doneAtStop atomic.Bool
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(NewLifecycle("worker",
		func(ctx context.Context) error { saved.Store(&ctx); return nil },
		func(context.Context) error {
			doneAtStop.Store((*saved.Load()).Err() != nil)
			return nil
		},
	)))
	entered := make(chan struct{})
	release := make(chan struct{})
	var doneDuringDrain atomic.Bool
	app.Get("/slow", func(*Context, Empty) (rtOut, error) {
		close(entered)
		<-release
		doneDuringDrain.Store((*saved.Load()).Err() != nil)
		return rtOut{OK: true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()
	addr := waitForAddr(t, app)
	go func() {
		res, err := http.Get("http://" + addr + "/slow") //nolint:noctx // a test against a local server
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	<-entered

	cancel() // what SIGTERM does under RunSignals
	waitFor(t, func() bool { return strings.Contains(logs.String(), "Shutting down") }, "the drain to begin")
	if err := (*saved.Load()).Err(); err != nil {
		t.Errorf("the start context ended when the drain began: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RunContext = %v", err)
	}
	if doneDuringDrain.Load() {
		t.Error("the start context ended while a request was still being drained")
	}
	if doneAtStop.Load() {
		t.Error("the start context had ended before Stop was called")
	}
	if (*saved.Load()).Err() == nil {
		t.Error("the start context outlived the run")
	}
}

// TestRunContextCancelledDuringStartupCancelsStart keeps what the run context
// is still for while the components start: cancelling it reaches a component
// still dialling, so start-up gives up rather than finishing a warm-up for a
// server that is not going to serve.
func TestRunContextCancelledDuringStartupCancelsStart(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(NewLifecycle("db", func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RunContext = %v, want the cancelled start-up", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the run context did not reach a component that was starting")
	}
}
