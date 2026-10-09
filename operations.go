package muzak

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// This file holds what the run methods do on behalf of the operational
// features, health and background tasks, at the points of a run where those
// features have something to do: when a run begins, and when its shutdown
// begins.

// validateOperations reports every misconfiguration of the operational
// features. It runs once the routes and mounts are known, because a health
// path that one of them answers is one of those misconfigurations.
func (a *App) validateOperations(state *buildState) {
	if err := a.opts.Background.validate(); err != nil {
		state.errs = append(state.errs, err)
	}
	a.validateHealth(state)
}

// validateDrainDelay reports a [ServerOptions.DrainDelay] that cannot be
// honoured: a negative one, and one that would spend the whole shutdown
// deadline before a single request was drained.
func (o ServerOptions) validateDrainDelay() error {
	switch {
	case o.DrainDelay < 0:
		return fmt.Errorf("muzak: ServerOptions.DrainDelay is %s; it must not be negative, and zero closes the listeners at once", o.DrainDelay)
	case o.ShutdownTimeout > 0 && o.DrainDelay >= o.ShutdownTimeout:
		return fmt.Errorf("muzak: ServerOptions.DrainDelay (%s) is not shorter than ServerOptions.ShutdownTimeout (%s), "+
			"which it counts against, so no time would be left to drain in-flight requests; shorten the delay or lengthen the timeout",
			o.DrainDelay, o.ShutdownTimeout)
	}
	return nil
}

// reopenOperations prepares the operational features for a new run: readiness
// stops reporting the shutdown the previous run ended with, background tasks
// are accepted again, and no readiness result found by the previous run is
// reused.
func (a *App) reopenOperations() {
	a.readiness.draining.Store(false)
	a.background.reopen()
	if a.health != nil && a.health.checker != nil {
		a.health.checker.forget()
	}
}

// beginDrain is the first thing a shutdown does, before any connection is
// closed or listener stopped.
//
// Readiness reports unavailable from here on, so that a load balancer probing
// it stops sending traffic, and the run then waits [ServerOptions.DrainDelay]
// for the load balancer to notice, still serving whatever arrives meanwhile,
// before the listeners are closed. The wait counts against the shutdown
// deadline, which ctx carries, and ends early if the deadline passes first.
//
// Background tasks are refused from the moment the listeners close rather
// than when the delay begins: a request served during the delay is ordinary
// traffic, and the tasks it registers are drained with the rest.
func (a *App) beginDrain(ctx context.Context, log *slog.Logger) {
	a.readiness.draining.Store(true)
	if delay := a.opts.DrainDelay; delay > 0 {
		log.Info(fmt.Sprintf("Readiness now reports unavailable; waiting %s before closing the listeners", delay))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
	a.background.close()
}
