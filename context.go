package muzak

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"

	"muzak.dev/framework/internal/radix"
)

// Context carries the request-scoped state for a single HTTP request,
// including the captured path parameters, the resolved dependency container,
// and the status code that will be written for the response.
//
// A Context is pooled and reused across requests. It must not be retained or
// used after its handler returns; copy out anything you need instead. In
// particular, do not capture a Context in a goroutine that outlives the
// handler; pass [Context.Context] to that goroutine if it needs cancellation.
type Context struct {
	w      *responseWriter
	r      *http.Request
	params radix.Params
	route  *Route
	logger *slog.Logger

	// app is the application the request is being served by, which is what
	// lets a route reach machinery that outlives one request, such as the
	// register of open WebSocket connections.
	app *App

	// status is the code that will be written when the handler returns
	// successfully. It starts at the route's declared default.
	status int

	// requestID is copied from the request context when the Context is
	// acquired, so that error rendering and logging can reach it without
	// walking the context chain on every use.
	requestID string

	// locale is copied from the request context when the Context is acquired,
	// for the same reason requestID is: translating a message should not walk
	// the context chain on every use. It is empty when the application has no
	// translation store configured.
	locale string

	// i18n is the application's translation store, copied so that a handler
	// can translate without reaching back through the App.
	i18n Translator

	// deps holds the value dependencies resolved for this request. It is a
	// slice rather than a map because routes declare a handful of
	// dependencies at most, and a short linear scan beats hashing a
	// reflect.Type. Entries are zeroed on release so that a pooled Context
	// never carries a previous request's values.
	deps []depValue
}

// depValue is one resolved value dependency, keyed by the concrete type the
// provider produces.
type depValue struct {
	typ reflect.Type
	val any
}

// Request returns the underlying *http.Request. Mutating it is allowed but the
// router has already finished matching, so changes to the URL have no effect
// on which handler runs.
func (c *Context) Request() *http.Request { return c.r }

// ResponseWriter returns the http.ResponseWriter for the response.
//
// Writing to it directly bypasses Muzak's response encoding, which means the
// handler's return value will not be serialized and [Context.SetStatus] stops
// having an effect. Use it for streaming, server-sent events or file
// downloads, and return the zero Out value with a nil error afterwards. The
// returned writer supports [http.ResponseController], so flushing and
// hijacking work as usual.
func (c *Context) ResponseWriter() http.ResponseWriter { return c.w }

// Context returns the request's context.Context, which is cancelled when the
// client disconnects or the server begins shutting down. It is shorthand for
// c.Request().Context() and is the correct value to hand to any operation that
// may outlive the handler.
func (c *Context) Context() context.Context { return c.r.Context() }

// Logger returns the structured logger associated with the application,
// annotated by the logging middleware with per-request attributes such as the
// method, path and request identifier when that middleware is enabled.
func (c *Context) Logger() *slog.Logger { return c.logger }

// RequestID returns the identifier assigned to this request, which appears in
// the X-Request-Id response header, in the "request_id" member of an error
// response, and in every log line the request produces. It is empty only when
// the [RequestID] middleware was removed from the chain.
func (c *Context) RequestID() string { return c.requestID }

// Locale returns the locale resolved for this request, which is what
// [Context.T] and [Context.L] answer in.
//
// It is empty when the application has no translation store configured, and is
// otherwise always one of the locales the application declared: nothing a
// client sends is used unless it matches one, so this value is safe to write
// into a response or a query.
func (c *Context) Locale() string { return c.locale }

// T translates a key in the request's locale.
//
//	func handler(ctx *muzak.Context, in Params) (Out, error) {
//		return Out{Title: ctx.T("store.title")}, nil
//	}
//
// Arguments are alternating names and values. Most are interpolated into the
// result; a few say how the lookup is performed, and the i18n package documents
// which. With no translation store configured the key is returned as it stands,
// so a handler written to translate still says something recognisable in an
// application that has not been localized yet.
func (c *Context) T(key string, args ...any) string {
	if c.i18n == nil {
		return key
	}
	return c.i18n.Translate(c.locale, key, args...)
}

// L renders a value the way the request's locale writes it, such as a time or
// an amount of money.
//
//	ctx.L(time.Now(), "format", "short")
//
// With no translation store configured the value is formatted the way Go
// prints it.
func (c *Context) L(value any, args ...any) string {
	if c.i18n == nil {
		return fmt.Sprint(value)
	}
	return c.i18n.Localize(c.locale, value, args...)
}

// Route returns the route being executed, exposing its method, path template,
// tags and declared documentation. It is never nil inside a handler.
func (c *Context) Route() *Route { return c.route }

// PathValue returns the value captured for the named path parameter, or the
// empty string if the route template declares no such parameter. Values are
// percent-decoded.
func (c *Context) PathValue(name string) string {
	v, _ := c.params.Get(name)
	return v
}

// LookupPath returns the value captured for the named path parameter and
// reports whether the route template declared it. Use it to tell an absent
// parameter from one that matched an empty string.
func (c *Context) LookupPath(name string) (string, bool) {
	return c.params.Get(name)
}

// Query returns the first value of the named query parameter, or the empty
// string when it is absent.
func (c *Context) Query(name string) string {
	return c.r.URL.Query().Get(name)
}

// LookupQuery returns the first value of the named query parameter and reports
// whether it was present at all, distinguishing "?token=" from a missing
// "token".
func (c *Context) LookupQuery(name string) (string, bool) {
	values, ok := c.r.URL.Query()[name]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// QueryValues returns every value supplied for the named query parameter, in
// the order they appeared. It returns nil when the parameter is absent.
func (c *Context) QueryValues(name string) []string {
	return c.r.URL.Query()[name]
}

// Header returns the first value of the named request header, or the empty
// string when it is absent. The name is matched case-insensitively, as HTTP
// requires.
func (c *Context) Header(name string) string {
	return c.r.Header.Get(name)
}

// Cookie returns the named cookie from the request. It returns
// [http.ErrNoCookie] when no such cookie was sent.
func (c *Context) Cookie(name string) (*http.Cookie, error) {
	return c.r.Cookie(name)
}

// SetHeader sets a response header, replacing any previously set value.
// Headers must be set before the handler returns; once the response has begun,
// further changes are ignored by net/http.
func (c *Context) SetHeader(name, value string) {
	c.w.Header().Set(name, value)
}

// AddHeader appends a value to a response header without removing values
// already present, which is what repeated headers such as Set-Cookie or Vary
// require.
func (c *Context) AddHeader(name, value string) {
	c.w.Header().Add(name, value)
}

// SetCookie adds a Set-Cookie header for the given cookie. The caller is
// responsible for setting Secure, HttpOnly and SameSite appropriately; Muzak
// does not modify the cookie.
func (c *Context) SetCookie(cookie *http.Cookie) {
	http.SetCookie(c.w, cookie)
}

// SetStatus sets the HTTP status code that will be written for the current
// response.
//
// It exists for statuses that depend on runtime logic; a status that is fixed
// for a route belongs in the route's declaration instead, via [Status].
// Calling SetStatus after the response body has already started writing has no
// effect and is treated as a programming error logged at warn level, because
// the status line has by then been sent. If SetStatus is never called, the
// route's declared default status is used, or 200 if none was declared. Codes
// outside the 100 to 599 range are clamped to 500.
func (c *Context) SetStatus(code int) {
	if c.w.written {
		c.logger.WarnContext(c.Context(), "muzak: SetStatus called after the response body started; ignoring",
			slog.Int("requested_status", code),
			slog.Int("written_status", c.w.status),
			slog.String("route", c.route.Path))
		return
	}
	c.status = clampStatus(code)
}

// Status returns the status code that will be written when the handler
// returns, which is the route's declared default until [Context.SetStatus]
// changes it.
func (c *Context) Status() int { return c.status }

// reset returns the Context to a pristine state before it goes back to the
// pool. Every reference the request could have introduced is cleared: leaving
// a resolved dependency, a request pointer or a route behind would both retain
// memory and risk exposing one request's values to the next.
func (c *Context) reset() {
	c.w = nil
	c.r = nil
	c.route = nil
	c.logger = nil
	c.app = nil
	c.status = 0
	c.requestID = ""
	c.locale = ""
	c.i18n = nil
	c.params.Reset()
	for i := range c.deps {
		c.deps[i] = depValue{}
	}
	c.deps = c.deps[:0]
}
