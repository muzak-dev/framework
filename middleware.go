package muzak

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

// Middleware wraps an http.Handler to run logic around every request.
//
// Middleware operates below Muzak's typed layer, on the raw net/http types,
// which is what lets any middleware written for the standard library be used
// unchanged. Install it with [App.Use]; the first one installed is the
// outermost.
type Middleware func(next http.Handler) http.Handler

// HeaderRequestID is the response header carrying the identifier assigned to
// each request.
const HeaderRequestID = "X-Request-Id"

// RequestIDKey is the log attribute key under which the request identifier is
// recorded. The console handler shortens values under this key to their first
// eight characters, keeping development output narrow while JSON output keeps
// the identifier in full.
const RequestIDKey = "request_id"

// requestIDContextKey is the unexported type used to store the request
// identifier in a context, so that no other package can collide with it.
type requestIDContextKey struct{}

// RequestIDFromContext returns the identifier assigned to the request carried
// by ctx, and reports whether one was assigned. Use it in code that has a
// context.Context but no Muzak [Context], such as a repository or a client
// wrapper that wants to propagate the identifier downstream.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDContextKey{}).(string)
	return id, ok
}

// contextWithRequestID returns a context carrying the given identifier.
func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// routeContextKey carries the holder the matched route's template is written
// into.
type routeContextKey struct{}

// routeHolder is filled in by dispatch and read by middleware.
//
// A holder rather than the string itself, because of where the two ends are.
// Middleware runs above routing and keeps the request it was handed; dispatch
// runs below and can only produce a *new* request with a new context. A value
// written down there is invisible up here. So the holder is installed on the
// way in, when middleware can still see it, and filled on the way through.
//
// No lock: one request is handled by one goroutine from the chain root to the
// handler and back, and the read happens after next.ServeHTTP has returned.
type routeHolder struct {
	template string
	// method is the method the matched route was registered for. A handler
	// mount answers every method and leaves it empty; see [registeredMethod].
	method string
}

// RouteFromContext returns the template of the route this request matched,
// such as "/v1/orgs/{org_id}/apps/{app_id}", and whether one matched at all.
//
// It exists for instrumentation. Middleware runs below the typed layer and
// before routing, so it never learns which route matched, and the concrete
// path is the wrong thing to name a span or label a metric with: one time
// series per identifier in the path is unbounded cardinality, and it makes the
// only question worth asking -- how slow is this endpoint -- unanswerable,
// because every request is its own endpoint.
//
// Read it after next.ServeHTTP has returned, which is where an access log
// already reads the status. Before that, routing has not happened and there is
// nothing to report.
//
//	func Tracing(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        start := time.Now()
//	        next.ServeHTTP(w, r)
//	        name, ok := muzak.RouteFromContext(r.Context())
//	        ...
//	    })
//	}
//
// A request that matched nothing reports false rather than an empty string, so
// a 404 is counted as a 404 rather than as traffic to a route named "".
func RouteFromContext(ctx context.Context) (string, bool) {
	holder, ok := ctx.Value(routeContextKey{}).(*routeHolder)
	if !ok || holder.template == "" {
		return "", false
	}
	return holder.template, true
}

// withRouteHolder installs the holder RouteFromContext reads.
//
// Always installed, outermost of the framework's own chain, so that anything
// added with App.Use sits inside it and can read what dispatch wrote.
func withRouteHolder(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), routeContextKey{}, &routeHolder{})))
	})
}

// StatusRecorder is implemented by the response writer Muzak installs, so
// middleware can read the status of a response without wrapping the writer a
// second time.
//
// The assertion can fail: the writer is installed by the access log, and an
// application that sets DisableAccessLog gets its own writer through
// unchanged. Middleware that needs the status either way should fall back to
// wrapping.
type StatusRecorder interface {
	// Status is the code that was written, or 0 if nothing has been written
	// yet.
	Status() int
}

// newRequestID returns a fresh request identifier. Version 7 UUIDs are used
// because their leading timestamp makes identifiers sort chronologically,
// which turns a log store's index into a time index for free.
func newRequestID() string {
	return uuid.NewV7().String()
}

// RequestIDOptions configures [RequestID].
type RequestIDOptions struct {
	// TrustInboundHeader accepts a client-supplied X-Request-Id instead of
	// generating one.
	//
	// It is off by default because an attacker-controlled identifier is an
	// attacker-controlled log field, which invites log forging and, in a log
	// store that indexes it, cross-tenant correlation. When enabled, an
	// inbound value is honoured only if it parses as a UUID, so it can never
	// carry newlines or control characters into a log line.
	TrustInboundHeader bool
}

// RequestID assigns every request an identifier, records it in the request
// context, and echoes it in the [HeaderRequestID] response header.
//
// The identifier is what ties a client's error response to the server-side log
// entry for the same request: [DefaultErrorRenderer] copies it into the
// "request_id" member of the error envelope, and the access log records it
// under [RequestIDKey].
func RequestID(opts RequestIDOptions) Middleware {
	return requestIDMiddleware(opts, nil)
}

// requestIDMiddleware is [RequestID] with a way for the application to hand a
// request the identifier of the request it is part of: a tool call of the MCP
// endpoint shares the identifier of the MCP request; see mcp_call.go.
func requestIDMiddleware(opts RequestIDOptions, inherited func(*http.Request) string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := ""
			if inherited != nil {
				id = inherited(r)
			}
			if id == "" && opts.TrustInboundHeader {
				if inbound := r.Header.Get(HeaderRequestID); inbound != "" {
					if parsed, err := uuid.Parse(inbound); err == nil {
						id = parsed.String()
					}
				}
			}
			if id == "" {
				id = newRequestID()
			}
			w.Header().Set(HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(contextWithRequestID(r.Context(), id)))
		})
	}
}

// Recovery catches a panic escaping any later handler, logs it with its stack
// trace, and returns a generic 500.
//
// Nothing derived from the panic value reaches the client: a panic often
// carries a pointer address, a SQL fragment or a file path, and a stack trace
// maps out the server's internals. The full detail is written to the logger
// instead, correlated with the request identifier so it can be matched to the
// response the client saw.
//
// A panic after the response has started cannot become a 500, because the
// status is already on the wire. It is logged the same way and then turned
// into a panic with http.ErrAbortHandler, which net/http answers by closing
// the connection without logging again, so the client sees a truncated
// response as the protocol error it is rather than as a short body that ended
// cleanly. A handler serving the application outside net/http must be ready
// for that panic, exactly as it would be for one from any http.Handler.
//
// http.ErrAbortHandler is re-panicked rather than swallowed, because net/http
// uses it to abort a response deliberately.
func Recovery(logger *slog.Logger) Middleware {
	return recovery(logger, writeMinimalError)
}

// recovery is [Recovery] writing its 500 with write, which is how an
// application rendering problem details answers a panic outside a route in the
// same format as every other error; see problem.go.
func recovery(logger *slog.Logger, write func(w http.ResponseWriter, requestID string)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrapping here is what lets the deferred function tell whether the
			// response has already started. Appending an error envelope to a
			// body that is partly on the wire would corrupt it, so a panic
			// after the first write is logged and the connection aborted.
			rw := asResponseWriter(w)
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				// recover returns any, not error, so errors.Is does not apply
				// here; net/http compares the sentinel the same way.
				if recovered == http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
					panic(recovered)
				}
				id, _ := RequestIDFromContext(r.Context())
				logger.ErrorContext(r.Context(), "muzak: recovered from a panic",
					slog.String("panic", panicValue(recovered)),
					slog.String("method", r.Method),
					slog.String("path", truncateForMessage(r.URL.Path)),
					slog.String(RequestIDKey, id),
					slog.String("stack", string(debug.Stack())))
				write(rw, id)
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// writeMinimalError writes the standard error envelope without needing a
// Muzak Context, for failures that happen outside the routed request path.
//
// When the response has already started it aborts instead; see
// [abortStartedResponse].
func writeMinimalError(w http.ResponseWriter, requestID string) {
	if rw, ok := w.(*responseWriter); ok && rw.written {
		abortStartedResponse(rw)
		return
	}
	body, err := json.Marshal(ErrorResponse{
		Error: ErrorBody{
			Code:    CodeInternalError,
			Message: internalMessage,
			Status:  http.StatusInternalServerError,
		},
		RequestID: requestID,
	})
	if err != nil {
		// coverage: ErrorResponse is a fixed struct of strings and an int, so
		// marshaling it cannot fail; the branch exists so a future field
		// cannot silently produce a body-less 500.
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// The body is fixed English, not the locale a handler or the Locale
	// middleware declared, so Content-Language is dropped rather than kept.
	resetForError(w.Header(), "")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(body)
}

// abortStartedResponse ends a request whose response is already on the wire
// and has failed, by panicking with http.ErrAbortHandler.
//
// Returning quietly would let net/http finish the response normally: the
// chunked encoding gets its terminating chunk, or the connection is reused as
// though a Content-Length had been honoured, and a client reading a CSV export
// or an NDJSON stream has no way to tell rows 3 to N never came. The sentinel
// is the one net/http documents for exactly this; it closes the connection
// without the stack trace it logs for any other panic, so the failure is
// recorded once, by whoever called this.
//
// A hijacked connection is left alone: it stopped speaking HTTP when it was
// taken over, and whoever took it closes it.
func abortStartedResponse(w *responseWriter) {
	if w.hijacked {
		return
	}
	panic(http.ErrAbortHandler)
}

// AccessLogOptions configures [AccessLog].
type AccessLogOptions struct {
	// Level is the level used for successful responses. Server errors are
	// always logged at error level and client errors at warn level, so that a
	// quiet production level still surfaces failures. It defaults to
	// slog.LevelInfo.
	Level slog.Level
	// SkipPaths lists exact paths that produce no log line, which keeps a
	// health check polled every second from drowning out real traffic.
	SkipPaths []string
}

// AccessLog records one line per request with its method, path, matched route,
// status, duration and request identifier, and with the request's trace and
// server span when tracing is configured; see [TracingOptions].
//
// A request whose connection was aborted after its response had started (see
// [Recovery]) is still recorded, with the status that was sent and
// "aborted=true", and so is a request whose panic is on its way to [Recovery],
// with the 500 that Recovery is about to write.
//
// Only fixed, non-sensitive fields are recorded. Query strings, request bodies
// and headers are deliberately omitted, because each of them routinely carries
// credentials or personal data that should not be duplicated into a log store.
func AccessLog(logger *slog.Logger, opts AccessLogOptions) Middleware {
	skip := make(map[string]struct{}, len(opts.SkipPaths))
	for _, p := range opts.SkipPaths {
		skip[p] = struct{}{}
	}
	scoped := Scoped(logger, ScopeRequest)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, skipped := skip[r.URL.Path]; skipped {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rw := asResponseWriter(w)
			// The line is written from a deferred function so that a panic on
			// its way through, whether one Recovery will turn into a 500 or
			// the abort of a response that already started, still leaves a
			// record of the request it ended. The panic is resumed unchanged.
			defer func() {
				recovered := recover()
				logAccess(scoped, opts.Level, r, rw, start, recovered != nil)
				if recovered != nil {
					panic(recovered)
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// logAccess writes the access log line for one request. panicking reports a
// panic passing through [AccessLog]: before the response has started, that is
// the 500 [Recovery] will write, and after it, the connection is aborted.
func logAccess(scoped *slog.Logger, base slog.Level, r *http.Request, rw *responseWriter, start time.Time, panicking bool) {
	status := rw.statusOrDefault()
	aborted := false
	if panicking {
		if rw.written {
			aborted = !rw.hijacked
		} else {
			status = http.StatusInternalServerError
		}
	}
	id, _ := RequestIDFromContext(r.Context())
	level := base
	switch {
	case status >= 500:
		level = slog.LevelError
	case status >= 400:
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
		slog.Int64("bytes", rw.bytes),
		slog.String(RequestIDKey, id),
	}
	// The template beside the concrete path, which is what joins a log
	// line to the trace and the metric for the same endpoint. The path
	// stays in the message because that is what somebody reading a log
	// wants; the template is what a query groups by.
	if route, ok := RouteFromContext(r.Context()); ok {
		attrs = append(attrs, slog.String("route", route))
	}
	// The trace, when tracing is configured, which joins the line to the
	// request's server span; see [TracingOptions].
	if sc, ok := SpanContextFromContext(r.Context()); ok {
		attrs = append(attrs, slog.String(TraceIDKey, sc.TraceID.String()), slog.String(SpanIDKey, sc.SpanID.String()))
	}
	// The locale is recorded only when one was resolved, so a service
	// that does not translate logs exactly what it logged before.
	if locale, ok := LocaleFromContext(r.Context()); ok && locale != "" {
		attrs = append(attrs, slog.String(LocaleKey, locale))
	}
	if aborted {
		// The status on its own reads as a success, which is exactly
		// what the abort exists to prevent a client from concluding.
		level = slog.LevelError
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	// Both are cut to a bounded length, because a client chooses them and
	// net/http admits either up to the size of the request line; see
	// truncateForMessage.
	scoped.LogAttrs(r.Context(), level, truncateForMessage(r.Method)+" "+truncateForMessage(r.URL.Path), attrs...)
}

// CORSOptions configures [CORS]. The zero value denies every cross-origin
// request, which is the only safe default: a permissive policy set by accident
// hands any web page on the internet the ability to read authenticated
// responses from the browser of anyone visiting it.
type CORSOptions struct {
	// AllowedOrigins lists the exact origins permitted, such as
	// "https://app.example.com". The single entry "*" allows any origin and
	// is rejected outright when AllowCredentials is also set.
	//
	// Each entry is compared exactly with the Origin header, which a browser
	// writes as scheme://host[:port] in lower case, without the scheme's
	// default port and with nothing after it. An entry that cannot match one,
	// such as "https://app.example.com/", "https://App.example.com:443" or
	// "*.example.com", is reported when the application is built, with the
	// spelling to use instead; subdomains are matched in AllowOriginFunc. The
	// entry "null" is refused outright: it is the origin of a sandboxed iframe
	// or a file: page, so any page can send it.
	AllowedOrigins []string
	// AllowOriginFunc decides dynamically whether an origin is permitted. It
	// is consulted only when AllowedOrigins does not already allow the
	// origin, and must not have side effects; it runs on every cross-origin
	// request.
	AllowOriginFunc func(origin string) bool
	// AllowedMethods lists the methods a cross-origin request may use. It
	// defaults to GET, HEAD, POST, PUT, PATCH, DELETE and OPTIONS.
	AllowedMethods []string
	// AllowedHeaders lists the request headers a client may send. It defaults
	// to Content-Type, Authorization and X-Request-Id.
	AllowedHeaders []string
	// ExposedHeaders lists the response headers a client may read. Browsers
	// expose only a small safelist unless a header appears here.
	ExposedHeaders []string
	// AllowCredentials permits cookies and Authorization headers on
	// cross-origin requests. It cannot be combined with a wildcard origin.
	AllowCredentials bool
	// MaxAge is how long a browser may cache the preflight result. It
	// defaults to ten minutes; browsers cap it regardless.
	MaxAge time.Duration
}

// CORS applies a cross-origin resource sharing policy.
//
// The policy denies everything unless explicitly configured: with no allowed
// origins and no [CORSOptions.AllowOriginFunc], no CORS headers are ever
// emitted and browsers refuse every cross-origin read. Combining a wildcard
// origin with credentials is refused as a configuration error, because
// browsers reject that pairing anyway and accepting it here would suggest it
// works.
//
// Every response a policy that names its origins produces carries
// "Vary: Origin", including the ones to a request with no Origin or a denied
// one, because those are exactly the responses that lack the
// Access-Control-Allow-Origin an allowed origin would get: a shared cache
// that stored one of them without the Vary would hand it to the allowed
// origin's browser, which would then refuse to read it. Only a wildcard
// policy leaves it out, and it does so by answering every request the same
// way: "Access-Control-Allow-Origin: *" and the exposed headers go on every
// response, one to a request with no Origin included, so that a copy a cache
// kept from a plain navigation still serves a cross-origin fetch. Every
// OPTIONS response varies on Access-Control-Request-Method, which decides
// whether it is answered as a preflight, and a preflight answer on
// Access-Control-Request-Headers as well.
//
// The error returned describes a policy that cannot be served safely or as
// written: a wildcard with credentials, which is [ErrCORSWildcardCredentials],
// and every AllowedOrigins entry that can never match, joined into one. A
// valid policy returns a nil error.
func CORS(opts CORSOptions) (Middleware, error) {
	wildcard := slices.Contains(opts.AllowedOrigins, "*")
	var errs []error
	if wildcard && opts.AllowCredentials {
		errs = append(errs, ErrCORSWildcardCredentials)
	}
	for _, entry := range opts.AllowedOrigins {
		if entry == "*" {
			continue
		}
		if err := checkCORSOrigin(entry); err != nil {
			errs = append(errs, err)
		}
	}
	switch len(errs) {
	case 0:
	case 1:
		// Returned as itself rather than joined, so that a caller comparing
		// with ErrCORSWildcardCredentials directly still recognises it.
		return nil, errs[0]
	default:
		return nil, errors.Join(errs...)
	}
	if len(opts.AllowedMethods) == 0 {
		opts.AllowedMethods = []string{
			http.MethodGet, http.MethodHead, http.MethodPost,
			http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
		}
	}
	if len(opts.AllowedHeaders) == 0 {
		opts.AllowedHeaders = []string{"Content-Type", "Authorization", HeaderRequestID}
	}
	if opts.MaxAge == 0 {
		opts.MaxAge = 10 * time.Minute
	}
	allowMethods := strings.Join(opts.AllowedMethods, ", ")
	allowHeaders := strings.Join(opts.AllowedHeaders, ", ")
	exposeHeaders := strings.Join(opts.ExposedHeaders, ", ")
	maxAge := strconv.Itoa(int(opts.MaxAge.Seconds()))

	allowed := func(origin string) bool {
		if wildcard {
			return true
		}
		if slices.Contains(opts.AllowedOrigins, origin) {
			return true
		}
		return opts.AllowOriginFunc != nil && opts.AllowOriginFunc(origin)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Declared to the writer instead of added to the header, so a
			// handler that sets a Vary of its own cannot take Origin out of
			// it; see [responseWriter].
			rw := asResponseWriter(w)
			defer rw.commitVary()
			w = rw
			header := w.Header()
			if !wildcard {
				rw.varyOn("Origin")
			}
			preflight := false
			if r.Method == http.MethodOptions {
				rw.varyOn("Access-Control-Request-Method")
				if r.Header.Get("Access-Control-Request-Method") != "" {
					preflight = true
					rw.varyOn("Access-Control-Request-Headers")
				}
			}

			// A wildcard policy answers every request the same way, with or
			// without an Origin, because that is the only way its response
			// does not depend on Origin and so the only way leaving Origin out
			// of Vary is true. A response kept by a browser or a shared cache
			// from a request with no Origin, a navigation or a script tag, is
			// otherwise handed to a later cross-origin fetch without the
			// header that fetch needs. A wildcard can only be served without
			// credentials, which CORS refused to be built with above.
			if wildcard {
				header.Set("Access-Control-Allow-Origin", "*")
				if exposeHeaders != "" {
					header.Set("Access-Control-Expose-Headers", exposeHeaders)
				}
			}

			origin := r.Header.Get("Origin")
			if origin == "" || !allowed(origin) {
				// Same-origin requests and denied origins are served without
				// any CORS header of a policy that names its origins, which is
				// what makes the browser refuse to expose the response. A
				// request with no Origin is not a preflight, whatever else it
				// carries, so it is refused as one under any policy.
				if preflight {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			if !wildcard {
				header.Set("Access-Control-Allow-Origin", origin)
				if exposeHeaders != "" {
					header.Set("Access-Control-Expose-Headers", exposeHeaders)
				}
			}
			if opts.AllowCredentials {
				header.Set("Access-Control-Allow-Credentials", "true")
			}
			if preflight {
				header.Set("Access-Control-Allow-Methods", allowMethods)
				header.Set("Access-Control-Allow-Headers", allowHeaders)
				header.Set("Access-Control-Max-Age", maxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// checkCORSOrigin reports an [CORSOptions.AllowedOrigins] entry that can never
// do what it appears to, with what to write instead.
func checkCORSOrigin(entry string) error {
	return checkOriginEntry("CORS origin", entry, false)
}

// checkOriginEntry reports an allowed-origin entry that can never do what it
// appears to, with what to write instead. It serves [CORSOptions] and
// [WSOptions] alike, because both compare their lists with the Origin header a
// browser sends; subject names the entry in the message, and foldCase says
// whether the list is compared without regard to case, as a WebSocket route's
// is, so that a difference of case alone is no mistake there.
//
// A browser serializes an origin as a scheme and a host with any port that is
// not the scheme's default, lowercased, with nothing after them. An entry
// written any other way, with a trailing slash, a path or ":443", never
// matches and allows nobody, which is a mistake that only shows itself as a
// browser refused. A pattern is the same mistake made deliberately. Case and
// default ports are only folded for http and https, the schemes whose origins
// a browser normalizes; another scheme, a browser extension's say, is compared
// as written.
//
// "null" is the opposite mistake: it matches far more than it appears to. It
// is the origin a sandboxed iframe, a page loaded from a file: URL and a
// request redirected across origins all present, so any page on the internet
// can send it, and listing it lets every one of them in.
func checkOriginEntry(subject, entry string, foldCase bool) error {
	const example = "https://app.example.com"
	if strings.EqualFold(entry, "null") {
		return fmt.Errorf("muzak: %s %q is refused, because it is what a sandboxed iframe, a file: page "+
			"or a request redirected across origins sends, so any page can present it and listing it allows them all; "+
			"remove it", subject, entry)
	}
	if strings.Contains(entry, "*") {
		return fmt.Errorf("muzak: %s %q is a pattern, and AllowedOrigins matches whole origins only, so it "+
			"never matches; list each origin, or allow subdomains in AllowOriginFunc by checking the scheme and that "+
			"the host ends in a dot followed by the domain", subject, entry)
	}
	u, err := url.Parse(entry)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("muzak: %s %q is not an origin, which is written scheme://host[:port], such as %q",
			subject, entry, example)
	}
	for i := range len(u.Host) {
		if u.Host[i] >= utf8.RuneSelf {
			return fmt.Errorf("muzak: %s %q never matches, because a browser sends a host in its ASCII "+
				"form; write the host as its punycode (xn--) spelling", subject, entry)
		}
	}

	scheme, host, port := strings.ToLower(u.Scheme), u.Hostname(), u.Port()
	if scheme == "http" || scheme == "https" {
		host = strings.ToLower(host)
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	origin := scheme + "://" + host
	if port != "" {
		origin += ":" + port
	}
	if entry == origin || (foldCase && strings.EqualFold(entry, origin)) {
		return nil
	}
	var why string
	switch {
	case u.User != nil:
		why = "carries credentials"
	case u.Path == "/":
		why = "ends in a slash"
	case u.Path != "":
		why = "has a path"
	case u.RawQuery != "" || u.ForceQuery:
		why = "has a query"
	case u.Fragment != "":
		why = "has a fragment"
	default:
		why = "is not spelled the way a browser sends it"
	}
	return fmt.Errorf("muzak: %s %q %s, so it never matches the Origin header; an origin is "+
		"scheme://host[:port] in lower case, without the scheme's default port and with nothing after it, so write %q",
		subject, entry, why, origin)
}

// SecurityHeaders sets conservative response headers on every response.
//
// It sets X-Content-Type-Options to stop a browser from guessing a content
// type other than the one declared, X-Frame-Options to prevent framing, and a
// referrer policy that keeps paths and query strings from leaking to third
// parties. Existing values are never overwritten, so a handler or a later
// middleware can opt out per response.
//
// A response to a request that arrived over TLS also gets
// "Strict-Transport-Security: max-age=31536000", which tells the browser to
// use HTTPS for this host for a year, so that a visitor's next request cannot
// be downgraded to plain HTTP on the way. It is set on nothing else: a browser
// ignores it over plain HTTP, and a server behind a proxy that terminates TLS
// cannot tell from the request whether the client used it, so there the proxy
// is the place to send it. It commits only the host that has just answered
// over TLS, since it leaves out includeSubDomains and preload, which commit
// hosts this application does not serve and are hard to take back; set the
// header in a handler, or in middleware installed with [App.Use], to add them
// once every subdomain is on HTTPS, or to remove it from a host that also
// serves plain HTTP on another port, which a browser pins along with it.
// Requests for localhost, a name under .localhost or an address are left
// alone, so that a development server with a local certificate does not pin
// every other server on the developer's machine to HTTPS.
//
// It sets no Content-Security-Policy. A policy that restricts content cannot be
// chosen without knowing the pages it governs, and a default one would break
// the HTML an application serves. One limited to frame-ancestors 'none' would
// add nothing to X-Frame-Options DENY and take something away: a browser
// prefers frame-ancestors, so a handler that relaxed X-Frame-Options to
// SAMEORIGIN for one page would find itself overruled. A page that wants a
// policy sets one of its own, as the documentation's pages do.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := w.Header()
			setIfAbsent(header, "X-Content-Type-Options", "nosniff")
			setIfAbsent(header, "X-Frame-Options", "DENY")
			setIfAbsent(header, "Referrer-Policy", "strict-origin-when-cross-origin")
			if r.TLS != nil && !localHost(r.Host) {
				setIfAbsent(header, "Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// localHost reports whether a request's Host names this machine or an address
// rather than a deployment: localhost, a name under .localhost, or an IP
// literal. A browser applies Strict-Transport-Security to every port of a
// host, so pinning localhost would turn every other development server on it
// into an HTTPS one, and it ignores the header from an address in any case.
func localHost(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	_, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil
}

// setIfAbsent sets a header only when it has no value yet.
func setIfAbsent(h http.Header, key, value string) {
	if h.Get(key) == "" {
		h.Set(key, value)
	}
}

// responseWriter wraps an http.ResponseWriter to record the status code and
// the number of bytes written, which the access log reports and which lets
// [Context.SetStatus] tell whether the response has already started.
//
// hijacked is kept apart from written because the two call for opposite
// handling of a later failure: a started response is aborted so the client
// cannot mistake it for a complete one, while a hijacked connection no longer
// belongs to net/http and must not be touched.
//
// vary holds the request headers the response depends on that middleware
// declared before the handler ran. They are merged into Vary when the response
// is written, and not when they are declared, because Vary is an ordinary
// header a handler may set or replace before it writes: one that does its own
// content negotiation would otherwise erase the Origin, Accept-Language or
// version header the response was chosen by, and a shared cache would store
// it for every client. Compression makes the same promise the same way; see
// [compressWriter].
type responseWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	written  bool
	hijacked bool
	vary     []string
	// commitHook is told the status just before the response starts, the
	// last moment a header can still be added: it is how the session of a
	// handler that writes its own response reaches it. Nil unless a request
	// read its session; see sessions.go.
	commitHook commitHook
}

// asResponseWriter wraps w unless it is already a *responseWriter, so that
// nesting middleware does not stack wrappers.
func asResponseWriter(w http.ResponseWriter) *responseWriter {
	if rw, ok := w.(*responseWriter); ok {
		return rw
	}
	return &responseWriter{ResponseWriter: w}
}

// Status returns the code written so far, satisfying [StatusRecorder].
func (w *responseWriter) Status() int { return w.status }

// WriteHeader records the status and forwards it, ignoring repeated calls the
// way net/http does.
//
// An informational status is forwarded without being recorded; see
// [isInformational].
func (w *responseWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	if isInformational(status) {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.beforeCommit(status)
	w.status = status
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

// varyOn records that the response depends on the named request headers, to be
// merged into Vary by [responseWriter.commitVary] rather than written now.
func (w *responseWriter) varyOn(fields ...string) {
	w.vary = append(w.vary, fields...)
}

// commitVary merges what [responseWriter.varyOn] recorded into Vary, adding
// only the fields the header does not already name. It is called when the
// response is written, the last moment a handler could have replaced the
// header, and by whoever ends the request, for a response with nothing to
// write, which net/http commits from the header as it stands when the handler
// returns. It does nothing once it has run, and nothing after the header is on
// the wire, when a change to the header no longer reaches the client.
func (w *responseWriter) commitVary() {
	if len(w.vary) == 0 {
		return
	}
	addVaryFields(w.Header(), w.vary...)
	w.vary = nil
}

// beforeCommit runs what has to reach the header before the response
// starts with status: the commit hook, once, and then the Vary fields, which
// the hook may have added to.
func (w *responseWriter) beforeCommit(status int) {
	if hook := w.commitHook; hook != nil {
		w.commitHook = nil
		hook.beforeCommit(status)
	}
	w.commitVary()
}

// addVaryFields adds every named field the Vary header does not already
// cover, as one value, and nothing at all when it covers them all.
func addVaryFields(header http.Header, fields ...string) {
	existing := header.Values("Vary")
	var missing []string
	for _, field := range fields {
		if !varyNames(existing, field) && !slices.ContainsFunc(missing, func(m string) bool { return strings.EqualFold(m, field) }) {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		header.Add("Vary", strings.Join(missing, ", "))
	}
}

// isInformational reports whether status is an interim 1xx response, such as
// 103 Early Hints, that precedes the real one rather than being it.
//
// net/http sends such a status on its own and still expects the final one, so
// a wrapper that recorded it as the response would drop what the handler
// returned afterwards, and abort the connection if the handler then failed,
// as though a real response were already on the wire. 101 is excluded, as
// net/http excludes it: Switching Protocols is the final response of the
// exchange.
func isInformational(status int) bool {
	return status >= 100 && status < 200 && status != http.StatusSwitchingProtocols
}

// Write records the byte count and marks the response as started.
func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.beforeCommit(http.StatusOK)
		w.status = http.StatusOK
		w.written = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer to [http.ResponseController], which is
// how flushing, hijacking and deadline control keep working through the
// wrapper.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FlushError pushes what has been written so far to the client.
//
// A wrapper that does not forward a flush is a wrapper that turns a stream
// into a response the client receives all at once at the end, which is the one
// thing a stream cannot survive. The controller is used rather than a type
// assertion so that a writer underneath which spells flushing either way is
// reached.
//
// A flush before the first Write still commits the response: net/http sends
// 200 and the headers on its own. That is recorded here, because everything
// that decides whether a later failure may still be rendered as an error
// response reads it, and one that believed nothing had been sent would append
// the JSON envelope to a stream the client has already been told is a
// success. A writer that cannot flush sent nothing, so nothing is recorded.
func (w *responseWriter) FlushError() error {
	// A flush sends the header as it stands, whether or not anything was
	// written before it.
	if !w.written {
		w.beforeCommit(http.StatusOK)
	}
	w.commitVary()
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !w.written && !errors.Is(err, http.ErrNotSupported) {
		w.status = http.StatusOK
		w.written = true
	}
	return err
}

// Flush is the older spelling of [responseWriter.FlushError], kept because a
// wrapper written before the newer one existed looks for it by name.
func (w *responseWriter) Flush() { _ = w.FlushError() }

// markHijacked records that the connection was taken over by a handler, so
// that nothing later in the chain writes an HTTP response onto a socket that
// has stopped speaking HTTP. The status is the one the handshake wrote.
func (w *responseWriter) markHijacked() {
	w.status = http.StatusSwitchingProtocols
	w.written = true
	w.hijacked = true
}

// hijackAware is implemented by every response writer this package wraps a
// request in, so that a handler which took the connection over can tell the
// whole chain at once.
type hijackAware interface {
	markHijacked()
}

// maxWriterChain bounds how far markHijacked walks, so that a wrapper whose
// Unwrap eventually returns itself cannot spin forever.
const maxWriterChain = 32

// markHijacked walks the chain of response writers wrapping one request and
// tells each of them that the connection is gone.
//
// Walking is necessary because middleware wraps the writer: the wrapper a
// handler sees is not the one the access log measures, and a wrapper left
// believing it still owes a response will try to finish writing one.
func markHijacked(w http.ResponseWriter) {
	for range maxWriterChain {
		if aware, ok := w.(hijackAware); ok {
			aware.markHijacked()
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

// statusOrDefault returns the status written, or 200 for a handler that
// returned without writing anything, matching what net/http puts on the wire.
func (w *responseWriter) statusOrDefault() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
