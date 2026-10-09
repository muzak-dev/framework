package muzak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Timeout gives a route, or every route beneath a router, a deadline for
// answering a request.
//
//	r.Get("/reports/{id}", buildReport, muzak.Timeout(2*time.Second))
//
// The deadline is cooperative. It is set on the request's context, which
// [Context.Context] returns, before the route's dependencies are resolved and
// its input is bound, so guards, providers, the handler and everything they
// pass the context to see it; nothing is interrupted that does not watch the
// context, and no second goroutine ever runs on the request's behalf. A
// database query or an outbound call given the context gives up when the
// deadline passes, and its error comes back to the handler.
//
// When the route fails because of the deadline, with an error that
// errors.Is reports as context.DeadlineExceeded and while the context's
// deadline is indeed the one that passed, and the response has not started,
// the client is answered 503 with "Retry-After: 1", rendered by the
// application's error renderer like any other error, and the cause is logged.
// An error that already carries a status of its own, such as one built with
// [GatewayTimeout], is sent as it is. A response that has started is aborted,
// as it is for any failure, so that the client cannot take what it received
// for a complete response. A handler that ignores
// the deadline and returns successfully has its success sent, however late:
// the deadline is a budget the handler is asked to respect, not a promise the
// framework can keep for it. The 503 rather than a 504 is deliberate: 504
// says a gateway's upstream did not answer, which would send whoever reads it
// to look at a proxy, while 503 with a Retry-After says this server could not
// answer in time and when to try again.
//
// The narrower declaration wins, so a route can lengthen or shorten its
// router's deadline, and a negative value removes an inherited one. Zero
// leaves the inherited deadline as it is. A route without a deadline pays
// nothing for this option existing.
//
// A deadline does not apply to an event stream or a WebSocket route, which
// lasts as long as its connection: one declared directly on such a route is a
// build error, and one inherited from a router is not applied to it. Bound
// those with [SSEOptions.MaxLifetime] and [WSOptions.MaxLifetime]. The same
// holds for a handler served with [Router.Mount], which writes its own
// response and may be streaming one.
//
// The deadline is not [ServerOptions.WriteTimeout], which still bounds how
// long the response may take to write and closes the connection when it
// passes; a Timeout longer than WriteTimeout is cut short by it.
func Timeout(d time.Duration) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.timeout = d },
		router: func(c *routerConfig) { c.timeout = d },
	}
}

// retryAfterTimeout is the Retry-After sent with the 503 a route's deadline
// produces. A deadline that passed says the request was slow, not that the
// server is refusing work, so the pause asked for is only long enough to
// keep a client from retrying in a tight loop.
const retryAfterTimeout = "1"

// errRouteTimeout is the cause a route's context is cancelled with when its
// [Timeout] passes, which is how a failure caused by that deadline is told
// apart from one caused by a shorter deadline the handler set itself, or by
// the client leaving.
var errRouteTimeout = errors.New("muzak: the route's deadline passed")

// resolveTimeout settles a route's deadline from what it declared and what
// it inherited; see [Timeout].
func (rt *Route) resolveTimeout(in inherited) error {
	if rt.websocket != nil || rt.sse != nil {
		if rt.cfg.timeout > 0 {
			kind, option := "SSE "+rt.Method, "SSEOptions.MaxLifetime"
			if rt.websocket != nil {
				kind, option = "WS", "WSOptions.MaxLifetime"
			}
			return fmt.Errorf("muzak: %s %s: Timeout cannot be declared on a route whose response lasts as long as its connection; bound it with %s instead",
				kind, rt.Path, option)
		}
		return nil
	}
	rt.timeout = in.timeout
	if rt.cfg.timeout != 0 {
		rt.timeout = rt.cfg.timeout
	}
	if rt.timeout <= 0 {
		rt.timeout = 0
		return nil
	}
	rt.documentRefusals(responseDoc{
		code:        http.StatusServiceUnavailable,
		description: "The request took longer than this operation allows. The Retry-After header says when to try again.",
	})
	return nil
}

// startDeadline puts the route's deadline on the request's context and
// returns the function that releases it. It replaces the request the Context
// carries rather than the Context itself, so everything that reads the
// context from either sees the deadline.
func (c *Context) startDeadline(d time.Duration) context.CancelFunc {
	ctx, cancel := context.WithTimeoutCause(c.r.Context(), d, errRouteTimeout)
	c.r = c.r.WithContext(ctx)
	return cancel
}

// timedOut turns a failure the route's deadline caused into the 503 that
// [Timeout] promises, and returns every other error as it is. It costs a
// comparison on a route without a deadline.
func (rt *Route) timedOut(c *Context, err error) error {
	if rt.timeout == 0 || c.w.written {
		return err
	}
	if !errors.Is(context.Cause(c.r.Context()), errRouteTimeout) {
		return err
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errRouteTimeout) {
		return err
	}
	var coder StatusCoder
	if errors.As(err, &coder) {
		return err
	}
	c.w.Header().Set(HeaderRetryAfter, retryAfterTimeout)
	return NewHTTPError(http.StatusServiceUnavailable,
		"the request took longer than this operation allows; try again shortly").Wrap(err)
}
