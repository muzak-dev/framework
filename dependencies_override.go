package muzak

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
)

// Override replaces every provider of type T in the application with provide,
// which is how a test swaps the real database, clock or caller for a fake
// without touching the routes:
//
//	app := buildApp()
//	app.Override(func(*muzak.Context) (CurrentUser, error) {
//		return CurrentUser{Username: "alice"}, nil
//	})
//	client := testclient.New(t, app)
//
// Every provider of exactly type T is replaced, on every route, file mount and
// the documentation of this application: one declared with [Needs], [Acquire],
// [Transaction], [Singleton] or [WithSingleton]. [From], [TryFrom] and [Dep]
// all receive the override's value. Guards are not providers and are left as
// they are, so a test still passes or fails them the way a client would.
//
// An override changes the value, not its lifetime. Where the replaced
// provider was request-scoped, provide runs on every request that reaches it;
// where it was a singleton, provide runs once, lazily, under the same rules as
// [Singleton]. A route's Cache-Control does not change either. A Release the
// replaced provider would have returned is not run, since the value it
// released is never acquired; use [App.OverrideAcquire] when the fake needs
// one of its own.
//
// Overrides belong to the application they are registered on, so two
// applications in one test binary never see each other's, and a provider
// option shared between them is not changed. Registering a second override for
// the same type replaces the first. An override replaces providers but never
// adds one, so a route still has to declare what it depends on, exactly as it
// does in production. One for a type no route, mount or the documentation
// declares a provider of is a build error, which is what catches a test
// overriding CurrentUser where the application provides *CurrentUser.
//
// Override must be called before the application is built, explicitly or by
// [App.ServeHTTP], [App.Document], a run method or testclient.New. A call made
// afterwards panics, because the dependency chains are resolved once and the
// override would silently not apply, and because it would race with the
// requests reading them.
func (a *App) Override[T any](provide func(ctx *Context) (T, error)) {
	a.mustBeOpen("App.Override")
	o := &dependencyOverride{typ: reflect.TypeFor[T](), call: "App.Override"}
	if provide == nil {
		o.invalid = fmt.Errorf("muzak: App.Override was given a nil provider for %s", o.typ)
	}
	o.resolve = func(c *Context) (any, error) {
		v, err := provide(c)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	a.addOverride(o)
}

// OverrideAcquire is [App.Override] for a fake that has to be released, with
// the same contract as [Acquire]: the Release provide returns runs once the
// request is over, with the request's failure.
//
// It cannot replace a singleton, which is a build error: a singleton's value is
// shared by every request and released by none, and a fake released at the
// end of each request would be tested under a lifetime production never has.
// Override a singleton with [App.Override], whose value keeps the singleton's
// lifetime.
func (a *App) OverrideAcquire[T any](provide func(ctx *Context) (T, Release, error)) {
	a.mustBeOpen("App.OverrideAcquire")
	o := &dependencyOverride{typ: reflect.TypeFor[T](), call: "App.OverrideAcquire", acquires: true}
	if provide == nil {
		o.invalid = fmt.Errorf("muzak: App.OverrideAcquire was given a nil provider for %s", o.typ)
	}
	o.resolve = acquiring(o.typ, provide)
	a.addOverride(o)
}

// dependencyOverride is one registered override and the providers built from
// it, one for the request-scoped positions it replaces and one, with a cache
// of its own, for the singleton positions.
type dependencyOverride struct {
	typ      reflect.Type
	call     string
	acquires bool
	invalid  error
	resolve  func(*Context) (any, error)

	request *provider
	single  *provider
	used    bool
	refused bool
}

// addOverride records an override, replacing an earlier one for the same type.
func (a *App) addOverride(o *dependencyOverride) {
	for i, existing := range a.overrides {
		if existing.typ == o.typ {
			a.overrides[i] = o
			return
		}
	}
	a.overrides = append(a.overrides, o)
}

// finishDependencies completes the dependency chains once every route and
// mount is known: it reports each provider declared with an invalid argument,
// once however many routes inherit it, and applies the application's
// overrides.
//
// Overrides are applied by replacing entries of a fresh copy of each chain,
// never the shared provider values, which is what keeps one application's
// overrides out of another that was given the same option. An application
// without overrides keeps its chains as they are, so the feature costs it
// nothing on any request.
func (a *App) finishDependencies(state *buildState) {
	chains := make([]*[]*provider, 0, len(a.routes)+len(state.frontends)+1)
	for _, rt := range a.routes {
		chains = append(chains, &rt.providers)
	}
	for _, f := range state.frontends {
		chains = append(chains, &f.providers)
	}
	// The application's own providers are read again when the documentation
	// handler is assembled, after this; every route copied them long before.
	chains = append(chains, &a.cfg.providers)

	reported := make(map[*provider]bool)
	for _, chain := range chains {
		for _, p := range *chain {
			if p.invalid != nil && !reported[p] {
				reported[p] = true
				state.errs = append(state.errs, p.invalid)
			}
		}
	}

	if len(a.overrides) == 0 {
		return
	}
	byType := make(map[reflect.Type]*dependencyOverride, len(a.overrides))
	for _, o := range a.overrides {
		if o.invalid != nil {
			state.errs = append(state.errs, o.invalid)
			continue
		}
		byType[o.typ] = o
	}
	for _, chain := range chains {
		*chain = overrideChain(*chain, byType, state)
	}
	unused := slices.SortedFunc(slices.Values(a.overrides), func(x, y *dependencyOverride) int {
		return cmp.Compare(x.typ.String(), y.typ.String())
	})
	for _, o := range unused {
		if o.invalid == nil && !o.used {
			state.errs = append(state.errs, fmt.Errorf("muzak: %s was given a provider of %s, but no route, mount or the documentation declares a provider of exactly that type, so it would replace nothing; "+
				"check the type, since a pointer and the value it points to are different types", o.call, o.typ))
		}
	}
}

// overrideChain returns the chain with every overridden provider replaced, or
// the chain itself when nothing in it is overridden.
func overrideChain(chain []*provider, byType map[reflect.Type]*dependencyOverride, state *buildState) []*provider {
	var out []*provider
	for i, p := range chain {
		o := byType[p.typ]
		if o == nil {
			continue
		}
		replacement := o.replace(p, state)
		if replacement == p {
			continue
		}
		if out == nil {
			out = slices.Clone(chain)
		}
		out[i] = replacement
	}
	if out == nil {
		return chain
	}
	return out
}

// replace returns the provider that stands in for p: the request-scoped one
// for a request-scoped p, and the cached one for a singleton. An Acquire
// override cannot stand in for a singleton, which is reported once and leaves
// p in place.
func (o *dependencyOverride) replace(p *provider, state *buildState) *provider {
	o.used = true
	if !p.shared {
		if o.request == nil {
			o.request = &provider{typ: o.typ, resolve: o.resolve}
		}
		return o.request
	}
	if o.acquires {
		if !o.refused {
			o.refused = true
			state.errs = append(state.errs, fmt.Errorf("muzak: %s cannot replace the singleton provider of %s, because a singleton is shared by every request and released by none; "+
				"override it with App.Override, whose value keeps the singleton's lifetime", o.call, o.typ))
		}
		return p
	}
	if o.single == nil {
		o.single = &provider{typ: o.typ, resolve: o.resolve, single: new(singleton), shared: true}
	}
	return o.single
}
