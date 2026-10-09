package muzak

import "net/http"

// buildInterop completes, once every route and file mount is known, what lets
// an application stand beside the rest of net/http: the handlers given to
// [Router.Mount], the host allowlist and HTTPS redirect that run ahead of
// routing, and the problem details error format. Each problem found is
// reported with the rest of the build's.
func (a *App) buildInterop(state *buildState) {
	a.installMounts(state)
	if a.opts.Health.Enabled {
		// A platform probes a replica by its own address, with whatever Host
		// that makes and over plain HTTP, so the host allowlist and the HTTPS
		// redirect would refuse or redirect every probe and take every replica
		// out of rotation. The probes say only whether the process is up and
		// ready, which is why they may be answered for any host.
		a.edgeExempt = func(r *http.Request) bool { return a.isHealthPath(r.URL.Path) }
	}
	edge, err := a.edgeMiddleware()
	if err != nil {
		state.errs = append(state.errs, err)
	}
	a.edge = edge
	if err := a.opts.ProblemDetails.validate(); err != nil {
		state.errs = append(state.errs, err)
	}
}
