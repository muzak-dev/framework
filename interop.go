package muzak

// buildInterop completes, once every route and file mount is known, what lets
// an application stand beside the rest of net/http: the handlers given to
// [Router.Mount], the host allowlist and HTTPS redirect that run ahead of
// routing, and the problem details error format. Each problem found is
// reported with the rest of the build's.
func (a *App) buildInterop(state *buildState) {
	a.installMounts(state)
	edge, err := a.edgeMiddleware()
	if err != nil {
		state.errs = append(state.errs, err)
	}
	a.edge = edge
	if err := a.opts.ProblemDetails.validate(); err != nil {
		state.errs = append(state.errs, err)
	}
}
