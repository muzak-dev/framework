package muzak

// buildInterop completes, once every route and file mount is known, what lets
// an application stand beside the rest of net/http: the handlers given to
// [Router.Mount]. Each problem found is reported with the rest of the build's.
func (a *App) buildInterop(state *buildState) {
	a.installMounts(state)
}
