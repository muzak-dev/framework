package muzak

import (
	"fmt"
	"reflect"
	"sync"
)

// Guard is a dependency that validates or authorizes a request without
// producing a value.
//
// A guard runs before the handler and before any value dependency declared
// after it. Returning a non-nil error aborts the request, and the error is
// mapped to a response exactly as one returned from a handler would be, so
// returning [NewHTTPError](401, "unauthorized") is the idiomatic way to reject.
// Guards are attached to an application or a router with [WithDependencies],
// mirroring router-level dependencies in FastAPI.
type Guard func(ctx *Context) error

// provider is a type-erased value dependency. The generic constructor [Needs]
// captures the caller's concrete type in typ and hides the type parameter
// inside resolve, which is what lets a non-generic Route hold a
// heterogeneous, ordered list of dependencies.
type provider struct {
	typ     reflect.Type
	resolve func(*Context) (any, error)

	// once, val and err are used only by singletons. A singleton's value is
	// computed on the first request that needs it and shared from then on;
	// sync.Once supplies the happens-before edge that makes val and err safe
	// to read from other goroutines afterwards.
	once *sync.Once
	val  any
	err  error
}

// get returns the dependency's value for this request, resolving a singleton
// at most once for the lifetime of the application.
func (p *provider) get(c *Context) (any, error) {
	if p.once == nil {
		return p.resolve(c)
	}
	p.once.Do(func() { p.val, p.err = p.resolve(c) })
	return p.val, p.err
}

// WithDependencies attaches guard dependencies to an application, a router or
// a single route.
//
// Guards run in declaration order, outermost first: those declared on the
// application run before those declared when including a router, which in turn
// run before those declared on the route itself. The first guard to return an
// error stops the chain and produces the response, so a guard that authorizes
// a whole subtree can be declared once at the point of inclusion:
//
//	app.Include(admin.NewRouter(), muzak.WithDependencies(GetTokenHeader))
//
// Use [Needs] instead when the dependency must hand a value to the handler.
func WithDependencies(guards ...Guard) SharedOption {
	return sharedOption{
		route: func(c *routeConfig) {
			c.guards = append(c.guards, guards...)
		},
		router: func(c *routerConfig) {
			c.guards = append(c.guards, guards...)
		},
	}
}

// Needs declares a request-scoped value dependency produced by the given
// provider function.
//
// The provider runs once per request, after every guard, and its result is
// retrieved inside the handler with [From]:
//
//	r.Get("/items/{id}", func(ctx *muzak.Context, in Params) (ItemOut, error) {
//		user := muzak.From[CurrentUser](ctx)
//		return ItemOut{ID: in.ID, Owner: user.Username}, nil
//	}, muzak.Needs(GetCurrentUser))
//
// The type parameter is inferred from the provider, so callers never write it
// out. A provider that returns an error aborts the request, and that error is
// mapped to a response the same way a handler error is.
//
// Resolved values are stored on the request's [Context] and discarded when it
// returns to the pool, so two concurrent requests never observe each other's
// values. Declaring the same type more than once along the chain is allowed;
// the most recent declaration wins, which lets a route override a dependency
// its router declared. Use [Singleton] for a value that should be computed
// once for the whole application instead.
func Needs[T any](provide func(ctx *Context) (T, error)) SharedOption {
	p := &provider{
		typ: reflect.TypeFor[T](),
		resolve: func(c *Context) (any, error) {
			v, err := provide(c)
			if err != nil {
				return nil, err
			}
			return v, nil
		},
	}
	return providerOption(p)
}

// Singleton declares a value dependency that is resolved once for the lifetime
// of the application and shared by every request thereafter.
//
// It is the deliberate exception to Muzak's request scoping, meant for
// expensive, immutable values such as a parsed configuration or a compiled
// template set. The provider runs on the first request that needs the value,
// receiving that request's [Context]; it must not retain that Context, read
// request-specific state from it, or return a value that is unsafe for
// concurrent use, because every later request shares the same value. An error
// from the provider is cached too, so a failing singleton fails every request
// rather than being retried.
func Singleton[T any](provide func(ctx *Context) (T, error)) SharedOption {
	p := &provider{
		typ:  reflect.TypeFor[T](),
		once: new(sync.Once),
		resolve: func(c *Context) (any, error) {
			v, err := provide(c)
			if err != nil {
				return nil, err
			}
			return v, nil
		},
	}
	return providerOption(p)
}

// WithSingleton publishes an already constructed value to every handler.
//
// It is the eager counterpart to [Singleton]: the value exists before the
// application starts, so there is nothing to resolve and every request simply
// receives it.
//
//	settings := muzak.MustLoadConfig[Settings](muzak.EnvFile(".env"))
//	app := muzak.New(muzak.AppOptions{Title: "Awesome API"},
//		muzak.WithSingleton(settings),
//	)
//
// Handlers retrieve it by type, with no cast:
//
//	s := muzak.From[Settings](ctx)
//
// If the value implements [Lifecycle], or a [LifecycleFunc] option is
// supplied, the value is also registered as a lifecycle component: it is
// started before the server accepts traffic and stopped after the server has
// drained. Lifecycle registration happens only when the option is applied to
// an application or a router, since a component's life is not tied to a single
// route.
//
// The value is shared by every request, so it must be safe for concurrent use.
func WithSingleton[T any](value T, opts ...SingletonOption) SharedOption {
	cfg := singletonConfig{}
	for _, opt := range opts {
		opt.applySingleton(&cfg)
	}
	if cfg.lifecycle == nil {
		if managed, ok := any(value).(Lifecycle); ok {
			cfg.lifecycle = managed
		}
	}
	p := &provider{
		typ:     reflect.TypeFor[T](),
		resolve: func(*Context) (any, error) { return value, nil },
	}
	return sharedOption{
		route: func(c *routeConfig) {
			c.providers = append(c.providers, p)
		},
		router: func(c *routerConfig) {
			c.providers = append(c.providers, p)
			if cfg.lifecycle != nil {
				c.lifecycles = append(c.lifecycles, cfg.lifecycle)
			}
		},
	}
}

// providerOption adapts a prepared provider into an option usable at every
// level of the routing tree.
func providerOption(p *provider) SharedOption {
	return sharedOption{
		route: func(c *routeConfig) {
			c.providers = append(c.providers, p)
		},
		router: func(c *routerConfig) {
			c.providers = append(c.providers, p)
		},
	}
}

// From returns the value dependency of type T resolved for the current
// request.
//
// The type argument is checked at compile time and no assertion appears in
// calling code. T must have been declared for the route being executed, via
// [Needs] or [Singleton] on the route itself or on any router that encloses
// it; asking for a type the route never declared is a programming error rather
// than a runtime condition to handle, so From panics. The panic is caught by
// the recovery middleware and reported as a 500 with the details logged, but
// it signals a bug to fix rather than an error to recover from. Use [TryFrom]
// when the absence of a dependency is a legitimate state.
func From[T any](ctx *Context) T {
	v, ok := TryFrom[T](ctx)
	if !ok {
		panic(fmt.Sprintf("muzak: no dependency of type %s is declared for route %s %s; "+
			"add muzak.Needs(provider) to the route or an enclosing router",
			reflect.TypeFor[T](), ctx.route.Method, ctx.route.Path))
	}
	return v
}

// TryFrom returns the value dependency of type T resolved for the current
// request and reports whether the route declared one. It is the non-panicking
// form of [From], useful for an optional dependency that only some routes in a
// shared helper declare.
func TryFrom[T any](ctx *Context) (T, bool) {
	target := reflect.TypeFor[T]()
	for i := range ctx.deps {
		if ctx.deps[i].typ == target {
			// The assertion cannot fail: the value was stored by Needs or
			// Singleton instantiated at this very type.
			return ctx.deps[i].val.(T), true
		}
	}
	var zero T
	return zero, false
}

// dedupeProviders returns providers with earlier declarations of a type
// removed, so that the most specific declaration for each type wins while the
// relative order of the survivors is preserved. Ordering matters because a
// provider may itself call [From] for a type declared before it.
func dedupeProviders(providers []*provider) []*provider {
	if len(providers) < 2 {
		return providers
	}
	last := make(map[reflect.Type]int, len(providers))
	for i, p := range providers {
		last[p.typ] = i
	}
	out := make([]*provider, 0, len(providers))
	for i, p := range providers {
		if last[p.typ] == i {
			out = append(out, p)
		}
	}
	return out
}

// resolveDependencies runs the route's guards and then its value providers,
// appending each resolved value to the request Context. It returns the first
// error produced, which the caller maps to a response.
func (rt *Route) resolveDependencies(c *Context) error {
	for _, guard := range rt.guards {
		if err := guard(c); err != nil {
			return err
		}
	}
	for _, p := range rt.providers {
		v, err := p.get(c)
		if err != nil {
			return err
		}
		c.deps = append(c.deps, depValue{typ: p.typ, val: v})
	}
	return nil
}
