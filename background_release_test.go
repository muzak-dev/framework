package muzak

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

// TestAfterResponseDoesNotFollowWorkThatWasRolledBack is the regression test
// for a background task that outlived the work it followed. A handler that
// succeeded registered a task, a dependency's release then failed, the commit
// of a transaction say, and the client was told the request failed, but the
// task still ran: a welcome email went out for a sign-up that was never
// committed. A task now runs only when everything the request acquired was
// released as a success.
func TestAfterResponseDoesNotFollowWorkThatWasRolledBack(t *testing.T) {
	t.Parallel()
	var ran atomic.Int32
	task := func(context.Context) { ran.Add(1) }
	register := func(ctx *Context) error { return ctx.AfterResponse(task) }

	commit := func(releaseErr error) SharedOption {
		return Acquire(func(*Context) (relA, Release, error) {
			return "tx", func(error) error { return releaseErr }, nil
		})
	}
	app := New(quietOptions())
	app.Get("/rolled-back", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{OK: true}, register(ctx)
	}, commit(errors.New("the commit failed")))
	app.Get("/committed", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{OK: true}, register(ctx)
	}, commit(nil))
	app.Get("/bytes-rolled-back", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{ContentType: "text/plain", Data: []byte("x")}, register(ctx)
	}, commit(errors.New("the commit failed")))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/rolled-back"), http.StatusInternalServerError)
	assertStatus(t, do(t, app, http.MethodGet, "/bytes-rolled-back"), http.StatusInternalServerError)
	waitPoolIdle(t, app)
	if n := ran.Load(); n != 0 {
		t.Fatalf("%d tasks ran after their request's work was rolled back", n)
	}
	app.background.mu.Lock()
	reserved := app.background.reserved
	app.background.mu.Unlock()
	if reserved != 0 {
		t.Fatalf("rolled-back requests left %d places in the queue taken", reserved)
	}

	assertStatus(t, do(t, app, http.MethodGet, "/committed"), http.StatusOK)
	waitFor(t, func() bool { return ran.Load() == 1 }, "the committed request's task to run")
}
