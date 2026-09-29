package muzak

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
)

// Guard is a dependency that validates or authorizes a request without
// producing a value.
//
// Every guard on a route runs before any value dependency does, whatever order
// they were declared in, and before the handler. A guard therefore cannot read
// a value a [Needs] produces: [TryFrom] reports false for it and [From] panics,
// which fails the request closed. A check that needs the caller's identity
// belongs in the provider that resolves it, which returns the error itself.
// Returning a non-nil error aborts the request, and the error is
// mapped to a response exactly as one returned from a handler would be, so
// returning [NewHTTPError](401, "unauthorized") is the idiomatic way to reject.
// Guards are attached to an application or a router with [WithDependencies], so
// that one written once covers every route mounted beneath it.
type Guard func(ctx *Context) error

// provider is a type-erased value dependency. The generic constructor [Needs]
// captures the caller's concrete type in typ and hides the type parameter
// inside resolve, which is what lets a non-generic Route hold a
// heterogeneous, ordered list of dependencies.
type provider struct {
	typ     reflect.Type
	resolve func(*Context) (any, error)

	// single is set only for a [Singleton] and is nil for every other
	// provider, so the per-request path pays one nil check for the feature.
	// Every route that inherits the singleton holds the same *provider and so
	// shares one cached value.
	single *singleton

	// shared marks a provider whose value is the same for every request, a
	// [Singleton] or a [WithSingleton]. Such a value cannot depend on who is
	// asking, so it is left out when deciding whether a response is
	// per-client; see [answersPerClient].
	shared bool
}

// answersPerClient reports whether a route or mount that runs these guards and
// providers may answer one client differently from another, which is what
// decides that its responses are marked private to caches.
//
// A guard decides who may see a response, and a request-scoped provider
// typically resolves the client's own session or account, so either makes the
// answer per-client. A singleton does not: it is built once and handed to
// every request alike, and counting it would mark every route of an
// application that shares a configuration or a connection pool as private,
// which is noise that teaches people to override the header.
func answersPerClient(guards []Guard, providers []*provider) bool {
	if len(guards) > 0 {
		return true
	}
	for _, p := range providers {
		if !p.shared {
			return true
		}
	}
	return false
}

// singleton is the shared state behind a lazily resolved [Singleton].
//
// It deliberately does not use sync.Once. Once marks itself done even when the
// function inside it panics or fails, which turned a single transient failure,
// or a first client that simply disconnected, into an error every later
// request received for the life of the process. Here only a successful value
// is kept: done is set after val is written, so a reader that observes done
// through the atomic load is guaranteed to observe val as well, and the fast
// path after the first success is a single atomic load with no lock.
//
// Nor does it hold a lock while the provider runs. It did once, and a provider
// that failed slowly, a dial to a database that is down, then made every
// waiting request take the lock in turn and run the provider again, so the
// k-th request waited k full attempts, long after its client had gone. Now at
// most one attempt is in flight, every request that arrives while it runs
// waits for that attempt and shares its outcome, error included, and a waiter
// whose own request is cancelled stops waiting.
type singleton struct {
	done atomic.Bool
	val  any

	// mu guards inflight, and is never held while a provider runs.
	mu       sync.Mutex
	inflight *singletonCall
}

// singletonCall is one attempt at resolving a singleton, which the requests
// that arrive while it runs wait on rather than starting one of their own.
type singletonCall struct {
	// finished is closed once the attempt is over; val and err are written
	// before it is, and read only after.
	finished chan struct{}
	val      any
	err      error
}

// errSingletonPanicked is what a request that was waiting for a singleton
// receives when the attempt it waited on panicked. The panic itself belongs to
// the request whose goroutine ran the provider, which reports it as a 500 with
// the stack logged; a waiter is failed the same way, but the panic value is
// not handed to it.
var errSingletonPanicked = errors.New("muzak: the singleton provider this request waited on panicked; " +
	"the panic and its stack are logged against the request that ran it")

// get returns the dependency's value for this request. A plain provider runs
// every time. A singleton returns its cached value once one has been computed
// and otherwise joins the attempt in flight or starts one, so concurrent first
// requests do not stampede the provider and a successful value is computed
// exactly once.
func (p *provider) get(c *Context) (any, error) {
	if p.single == nil {
		return p.resolve(c)
	}
	if p.single.done.Load() {
		return p.single.val, nil
	}
	return p.resolveSingleton(c)
}

// resolveSingleton is the slow path of get for a singleton that has not yet
// produced a value. It is kept separate so that get stays small enough to
// inline on the per-request path.
//
// The request that finds no attempt in flight runs the provider itself, on its
// own goroutine, which is what keeps a panic the provider raises that
// request's panic, recovered into a 500 like any other. Every request arriving
// while that runs waits for the attempt's outcome or for its own request to
// end, whichever comes first. A failure is handed to the waiters of the
// attempt that produced it and to nobody else: the next request after the
// attempt is over starts a new one, because a failure may be transient.
func (p *provider) resolveSingleton(c *Context) (any, error) {
	s := p.single
	s.mu.Lock()
	if s.done.Load() {
		// Another request resolved it while this one took the lock.
		s.mu.Unlock()
		return s.val, nil
	}
	if call := s.inflight; call != nil {
		s.mu.Unlock()
		return call.wait(c)
	}
	call := &singletonCall{finished: make(chan struct{})}
	s.inflight = call
	s.mu.Unlock()

	returned := false
	defer func() {
		if !returned {
			// The provider panicked. The panic carries on to this request's
			// recovery; the waiters are failed rather than left waiting.
			call.err = errSingletonPanicked
		}
		s.mu.Lock()
		s.inflight = nil
		s.mu.Unlock()
		close(call.finished)
	}()
	v, err := p.resolveDetached(c)
	returned = true
	if err != nil {
		// Not cached: a failure may be transient, and caching it would let one
		// unlucky or hostile request fail every request after it.
		call.err = err
		return nil, err
	}
	call.val = v
	s.val = v
	s.done.Store(true)
	return v, nil
}

// wait blocks until the attempt is over or the waiting request ends. A
// request whose client has gone, or whose deadline passed, has no use for the
// value, so it stops waiting rather than holding its goroutine and its
// connection for however long the provider takes.
func (call *singletonCall) wait(c *Context) (any, error) {
	ctx := c.Context()
	select {
	case <-call.finished:
		return call.val, call.err
	case <-ctx.Done():
		return nil, fmt.Errorf("muzak: the request ended while it waited for a singleton another request is resolving: %w", ctx.Err())
	}
}

// resolveDetached runs the provider with a request whose context carries the
// current request's values but not its cancellation or deadline. The value it
// produces outlives the request that happened to trigger it, so the client
// that request came from disconnecting must not be able to fail the
// construction for everyone else. The original request is restored before
// returning, including when the provider panics.
func (p *provider) resolveDetached(c *Context) (any, error) {
	original := c.r
	c.r = original.WithContext(context.WithoutCancel(original.Context()))
	defer func() { c.r = original }()
	return p.resolve(c)
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
// values. Use [Singleton] for a value that should be computed once for the
// whole application instead.
//
// Declaring the same type more than once along the chain is allowed, and the
// most recent declaration overrides the value, not the check. Every provider
// on the chain runs, outermost first, and the first error aborts the request;
// [From] then returns the value of the innermost declaration. This matters
// because a provider is often also an authorization step: a router included
// with Needs(RequireAdmin) keeps rejecting non-admins even when the router
// itself, or a route inside it, declares Needs(CurrentUser) of the same type,
// rather than the nearer declaration silently removing the outer one and the
// check with it. A provider that calls From for its own type
// sees the value produced by the nearest enclosing declaration of that type,
// since its own value does not exist yet, which lets an inner provider refine
// an outer one rather than repeat its work.
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
// concurrent use, because every later request shares the same value.
//
// At most one attempt to run the provider is in flight at a time. Requests
// that need the value while it runs wait for that attempt and share its
// outcome, so a value is constructed at most once and a provider that fails
// is not run again by every request that was queued behind it. A waiting
// request stops waiting when its own context ends, and fails with that
// context's error.
//
// Only a successful value is cached. An error fails the request that
// triggered it and every request that waited on the same attempt, and the
// next request after that runs the provider again. A panic reaches the
// recovery middleware of the triggering request as a 500 like any other, and
// the requests that waited on it fail with a 500 of their own. While the
// provider runs, [Context.Context] returns a context that keeps the
// triggering request's values but not its cancellation or deadline: the value
// outlives that request, so its client disconnecting must not fail the
// construction. A provider that does I/O should therefore apply its own
// timeout with [context.WithTimeout], because nothing else will bound it and
// the triggering request waits while it runs.
func Singleton[T any](provide func(ctx *Context) (T, error)) SharedOption {
	p := &provider{
		typ:    reflect.TypeFor[T](),
		single: new(singleton),
		shared: true,
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
		shared:  true,
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
// shared helper declare. When the type is declared more than once along the
// chain it returns the innermost value, as [From] does.
func TryFrom[T any](ctx *Context) (T, bool) {
	target := reflect.TypeFor[T]()
	// Searching from the end is what makes the innermost declaration win:
	// values are appended in declaration order, outermost first, and every
	// declaration of a type is resolved rather than only the last one.
	for i := len(ctx.deps) - 1; i >= 0; i-- {
		if ctx.deps[i].typ == target {
			// The value was stored by Needs or Singleton instantiated at this
			// very type, so the assertion can only fail on a nil interface
			// value, which is stored as a nil any and is a legitimate result:
			// a provider of an interface type may return nil for an absent
			// caller. The comma-ok form turns that into T's zero value, the
			// nil the provider returned, rather than a panic in the one
			// accessor documented not to panic.
			v, _ := ctx.deps[i].val.(T)
			return v, true
		}
	}
	var zero T
	return zero, false
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
	return resolveProviders(c, rt.providers)
}

// resolveProviders runs every provider in order and appends each value to the
// request Context, stopping at the first error. It is shared by routes and by
// anything else that inherits a router's providers, such as a file mount, so
// that all of them apply the same rule: no inherited provider is skipped, and
// [From] sees the innermost value of each type.
func resolveProviders(c *Context, providers []*provider) error {
	for _, p := range providers {
		v, err := p.get(c)
		if err != nil {
			return err
		}
		c.deps = append(c.deps, depValue{typ: p.typ, val: v})
	}
	return nil
}
