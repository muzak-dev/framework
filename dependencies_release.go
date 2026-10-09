package muzak

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"runtime/debug"
)

// Release ends what an [Acquire] provider acquired: it commits or rolls back a
// transaction, returns a connection to its pool, closes a file.
//
// failure is nil when the request succeeded, and otherwise says why it did
// not: the error the handler returned or the one a later provider, the rate
// limit or the binder refused the request with, an error describing a panic,
// or the error that ended an event stream or a WebSocket. A Release decides
// from it whether to keep or undo the work, which is what makes the rule
// "commit only if everything succeeded" a single if statement.
//
// The error a Release returns matters only when failure is nil. A buffered
// response has not been written yet, so the error replaces the success and is
// rendered exactly as a handler's error would be: an [HTTPError] chooses its
// status, and anything else becomes an opaque 500 with the cause logged and
// never sent. When failure is not nil the response is already the failure's,
// so the error is logged. A Release that panics is recovered, logged with its
// stack, and counted as having returned an error.
type Release func(failure error) error

// Acquire declares a request-scoped value dependency that has to be released
// once the request is over, which is what FastAPI writes as a dependency that
// yields:
//
//	func OpenSession(ctx *muzak.Context) (*Session, muzak.Release, error) {
//		s, err := pool.Open(ctx.Context())
//		if err != nil {
//			return nil, nil, err
//		}
//		return s, func(failure error) error { return s.Close() }, nil
//	}
//
//	r.Post("/orders", PlaceOrder, muzak.Acquire(OpenSession))
//
// The value is read like any other, with [From] or a [Dep] field, and the
// provider runs where a [Needs] provider would. What it adds is the Release,
// whose contract is:
//
//   - It runs exactly once for every provider call that returned a nil error,
//     and never for one that returned an error. A provider that fails part way
//     through undoes what it had acquired itself; a Release it returns
//     alongside an error is not called. A nil Release means there is nothing
//     to release.
//   - Releases run in the reverse of the order their values were acquired, so
//     a value acquired from another is released first.
//   - They run on every path: the handler succeeding, returning an error or
//     panicking; a later provider failing; the rate limit counted after the
//     dependencies refusing; binding or validation failing; the client going
//     away; and an event stream or a WebSocket ending.
//   - For a buffered response they run after the handler has returned and its
//     result has been encoded, and before anything is written, so a commit
//     that fails can still turn the success into an error; see [Release].
//     Once the response has started, because the handler wrote to
//     [Context.ResponseWriter], streamed events, hijacked the connection or
//     upgraded it, the status can no longer change. A release error is then
//     treated as the handler returning it at that point would be: logged,
//     with a response the handler started aborted rather than completed and a
//     WebSocket closed with 1011 rather than 1000.
//   - A panic is still the usual 500 and is logged once; the releases receive
//     an error describing it.
//
// A Release receives no Context. It runs on the request's goroutine before the
// Context returns to the pool, so a closure may use what the provider
// captured, but it must not keep the Context. A pooled Context never carries a
// release from one request to the next.
//
// Acquire is request-scoped by construction. There is no singleton that
// releases, because a value shared by every request has no request to be
// released at the end of: a resource that lives as long as the application is
// published with [WithSingleton] and closed through [Lifecycle].
//
// A file mount and the documentation run the providers they inherit too, so an
// Acquire declared on the application acquires and releases for them as well.
// They have no handler to fail, so their releases receive a nil failure once
// the file has been served, and the refusal otherwise. Declare Acquire on the
// routers that need it.
func Acquire[T any](provide func(ctx *Context) (T, Release, error)) SharedOption {
	p := &provider{typ: reflect.TypeFor[T]()}
	if provide == nil {
		p.invalid = fmt.Errorf("muzak: Acquire was given a nil provider for %s", p.typ)
	}
	p.resolve = acquiring(p.typ, provide)
	return providerOption(p)
}

// acquiring adapts an Acquire provider into a provider's resolve function,
// recording the Release on the request so that [Context.settle] runs it.
func acquiring[T any](typ reflect.Type, provide func(ctx *Context) (T, Release, error)) func(*Context) (any, error) {
	return func(c *Context) (any, error) {
		v, release, err := provide(c)
		if err != nil {
			return nil, err
		}
		if release != nil {
			// The slice belongs to the pooled Context, so after the first few
			// requests appending reuses its capacity rather than allocating.
			// It holds one entry per Acquire on the route's chain, a number
			// fixed when the route is declared.
			c.releases = append(c.releases, heldRelease{typ: typ, release: release})
		}
		return v, nil
	}
}

// heldRelease is a Release waiting to run, with the type of the value it
// releases so that a failure can be logged against it.
type heldRelease struct {
	typ     reflect.Type
	release Release
}

// settle runs the request's pending releases, last acquired first, and
// returns the error the request ends with.
//
// failure is what the request is ending with so far: nil for a success. When
// it is nil and a release fails, that release's error becomes the failure
// every remaining release sees and is returned, for the caller to render or,
// once the response has started, to report as it would a handler's error.
// When failure is not nil it is returned unchanged and a release error is
// logged here, since the response is already the failure's.
//
// Each release is removed before it is called, so it runs exactly once even
// if something it calls ends up here again. With nothing pending this is a
// length check, which is all a route without [Acquire] pays.
func (c *Context) settle(failure error) error {
	if len(c.releases) == 0 {
		return failure
	}
	return c.runReleases(failure)
}

// runReleases is the slow path of settle, kept apart so that settle inlines.
func (c *Context) runReleases(failure error) error {
	for n := len(c.releases); n > 0; n = len(c.releases) {
		held := c.releases[n-1]
		c.releases[n-1] = heldRelease{}
		c.releases = c.releases[:n-1]
		err := c.callRelease(held, failure)
		switch {
		case err == nil:
		case failure == nil:
			failure = err
		default:
			c.logger.ErrorContext(c.Context(), "muzak: a dependency's release failed while the request was already failing",
				slog.String("dependency", held.typ.String()),
				slog.String("method", truncateForMessage(c.r.Method)),
				slog.String("path", truncateForMessage(c.r.URL.Path)),
				slog.String(RequestIDKey, c.RequestID()),
				slog.String("error", err.Error()))
		}
	}
	return failure
}

// callRelease runs one release, turning a panic into an error after logging
// it with its stack, so that the remaining releases still run.
func (c *Context) callRelease(held heldRelease, failure error) (err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		c.logger.ErrorContext(c.Context(), "muzak: recovered from a panic in a dependency's release",
			slog.String("dependency", held.typ.String()),
			slog.String("panic", panicValue(recovered)),
			slog.String("method", truncateForMessage(c.r.Method)),
			slog.String("path", truncateForMessage(c.r.URL.Path)),
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("stack", string(debug.Stack())))
		err = fmt.Errorf("muzak: the release of the %s dependency panicked; the panic and its stack are logged", held.typ)
	}()
	return held.release(failure)
}

// panicFailure is what releases receive when the request panicked. The panic
// [http.ErrAbortHandler] is how a response that had started is aborted, and
// is reported as that rather than as a crash; anything else is described by
// its value, which is logged as well and never sent.
func panicFailure(recovered any) error {
	if recovered == http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
		return fmt.Errorf("muzak: the response was aborted after it had started: %w", http.ErrAbortHandler)
	}
	return fmt.Errorf("muzak: the request panicked: %s", panicValue(recovered))
}

// errReleaseAbandoned is what releases receive when the request ended without
// reaching any of the points that settle them, which only a panic outside a
// route can cause: nothing says the request succeeded, so it is released as a
// failure.
var errReleaseAbandoned = errors.New("muzak: the request ended abnormally before its dependencies were released")

// settleServed runs the releases of a file mount or of the documentation once
// it has answered. Neither has a handler that could fail, so the failure is
// nil; a release error is then handled as any failure after the response has
// started.
func (a *App) settleServed(c *Context) {
	if err := c.settle(nil); err != nil {
		a.fail(c, err)
	}
}
