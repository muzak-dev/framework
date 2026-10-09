package muzak

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"muzak.dev/framework/internal/radix"
)

// Endpoint is one operation declared once, as a value, in a package that the
// server implementing it and the Go programs calling it both import:
//
//	// package userapi, imported by the users service and by its callers
//	var GetUser = muzak.NewEndpoint[GetUserIn, User](http.MethodGet, "/users/{id}",
//		muzak.Summary("Fetch a user"))
//
//	// the users service
//	r.Implement(userapi.GetUser, handlers.GetUser)
//
//	// a service that calls it
//	user, err := userapi.GetUser.Call(ctx.Context(), users, userapi.GetUserIn{ID: "42"})
//
// The method, the path and the two types travel together, so the handler
// registered with [Router.Implement] and every call made with
// [Endpoint.Call] agree on them by construction: a handler whose input or
// output type differs from the endpoint's does not compile, and neither does
// a call passing the wrong input or expecting the wrong output. Changing the
// declaration changes both sides in the same commit.
//
// An Endpoint is a small value, safe to copy and to use from any number of
// goroutines. Its zero value was declared by nothing, and both registering it
// and calling it report so.
type Endpoint[In, Out any] struct {
	// The two zero-length fields give Endpoint[A, B] and Endpoint[C, D]
	// different underlying types. Without them every instantiation would be
	// struct{ def *endpointDef }, and a conversion between two of them would
	// compile, handing an endpoint compiled for one input type to a call
	// passing another. They come first, because a zero-size field at the end
	// of a struct is given padding.
	_   [0]*In
	_   [0]*Out
	def *endpointDef
}

// endpointDef is what [NewEndpoint] records, shared by every copy of the
// Endpoint it returned.
type endpointDef struct {
	method string
	path   string
	opts   []RouteOption
	// err is what is wrong with the method or the path, found when the
	// endpoint was declared and reported when it is implemented or called;
	// see [NewEndpoint].
	err error

	// call is how a value of the input is written as a request, compiled on
	// the first call rather than at declaration, so a program that only
	// serves the endpoint never pays for it; see endpoint_encode.go.
	once    sync.Once
	call    *callPlan
	callErr error
}

// NewEndpoint declares an endpoint: the method and path template it answers,
// as [Router.Handle] takes them, and the route options that describe it, such
// as [Summary], [WithTags] or [Status], which apply wherever it is
// implemented. In and Out are written at the call, since nothing else names
// them:
//
//	var CreateItem = muzak.NewEndpoint[CreateItemIn, Item](http.MethodPost, "/items",
//		muzak.Status(http.StatusCreated))
//
// The path is relative to wherever the endpoint is implemented. A router
// included under WithPrefix("/v1") serves it at "/v1/items", and a client
// calls it with "/v1" at the end of its [ClientOptions.BaseURL].
//
// The method and the path are checked here, against the rules the router
// holds a route to, but an endpoint is usually a package-level variable, and
// a panic while packages are initialised stops the program before it can say
// which declaration was wrong. So a mistake is recorded instead: it is a build
// error of the application that implements the endpoint, joined with the
// others, and the error every call returns. The input type is checked when
// the endpoint is implemented, as any route's is, and again on the first
// call, for what writing it as a request needs; see [Endpoint.Call].
func NewEndpoint[In, Out any](method, path string, opts ...RouteOption) Endpoint[In, Out] {
	def := &endpointDef{method: strings.ToUpper(method), path: path, opts: slices.Clone(opts)}
	def.err = checkEndpoint(def.method, path)
	return Endpoint[In, Out]{def: def}
}

// Method returns the endpoint's HTTP method, upper-cased, or the empty string
// for the zero Endpoint.
func (ep Endpoint[In, Out]) Method() string {
	if ep.def == nil {
		return ""
	}
	return ep.def.method
}

// Path returns the endpoint's path template as declared, relative to wherever
// it is implemented, or the empty string for the zero Endpoint.
func (ep Endpoint[In, Out]) Path() string {
	if ep.def == nil {
		return ""
	}
	return ep.def.path
}

// errNoEndpoint is what registering or calling the zero Endpoint reports.
var errNoEndpoint = errors.New("muzak: the Endpoint was not declared with muzak.NewEndpoint, so it has no method or path; " +
	"declare it with NewEndpoint and use the value it returns")

// checkEndpoint reports what is wrong with an endpoint's method or path, by
// the rules a route is held to: the method must be a token, as [Router.Handle]
// sends it, and the path a template the router's tree accepts.
//
// The template is checked by inserting it into a tree of its own, which is
// the same parser the application's routes go through, so an endpoint is
// refused here exactly when its route would be. A template parameter that no
// field of the input binds is checked on the first call instead, since only
// the input type knows it.
func checkEndpoint(method, path string) error {
	if !isHTTPToken(method) {
		return fmt.Errorf("muzak: NewEndpoint was given the method %q, which is not an HTTP method; use one of the http.Method constants", clientShorten(method))
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("muzak: %s %s: path must begin with %q", method, clientShorten(path), "/")
	}
	if err := radix.New[struct{}]().Insert(path, struct{}{}); err != nil {
		return fmt.Errorf("muzak: %s %s: %w", method, clientShorten(path), err)
	}
	return nil
}

// Implement registers the handler of an endpoint, exactly as
// [Router.Handle] with the endpoint's method and path would, under the
// endpoint's route options followed by opts. Options given here describe this
// implementation only, such as a guard or a rate limit, and an option given in
// both places is resolved as two options given to Handle in that order are.
//
// The handler's types are the endpoint's, so a handler that takes or returns
// anything else is a compile error rather than a route that disagrees with its
// callers:
//
//	r.Implement(userapi.GetUser, func(ctx *muzak.Context, in userapi.GetUserIn) (userapi.User, error) {
//		return store.User(ctx.Context(), in.ID)
//	})
//
// A mistake in the endpoint's declaration is reported when the application is
// built, with the router's other registration errors, and the route is not
// registered. Like every registration, Implement panics once the application
// the router belongs to has been built.
func (r *Router) Implement[In, Out any](ep Endpoint[In, Out], h Handler[In, Out], opts ...RouteOption) *Route {
	r.mustBeOpen("Implement")
	def := ep.def
	if def == nil || def.err != nil {
		err := errNoEndpoint
		rt := &Route{inType: reflect.TypeFor[In](), outType: reflect.TypeFor[Out](), registry: r}
		if def != nil {
			err = def.err
			rt.Method, rt.rawPath = def.method, def.path
		}
		r.errs = append(r.errs, err)
		return rt
	}
	return register(r, def.method, def.path, h, concat(def.opts, opts))
}
