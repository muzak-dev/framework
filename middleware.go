package muzak

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
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
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := ""
			if opts.TrustInboundHeader {
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
// http.ErrAbortHandler is re-panicked rather than swallowed, because net/http
// uses it to abort a response deliberately.
func Recovery(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrapping here is what lets the deferred function tell whether the
			// response has already started. Appending an error envelope to a
			// body that is partly on the wire would corrupt it, so a panic
			// after the first write is logged and nothing more is sent.
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
					slog.Any("panic", recovered),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String(RequestIDKey, id),
					slog.String("stack", string(debug.Stack())))
				writeMinimalError(rw, id)
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// writeMinimalError writes the standard error envelope without needing a
// Muzak Context, for failures that happen outside the routed request path.
func writeMinimalError(w http.ResponseWriter, requestID string) {
	if rw, ok := w.(*responseWriter); ok && rw.written {
		// The response is already on the wire; the best available outcome is
		// a truncated body, which the client will see as a protocol error.
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(body)
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

// AccessLog records one line per request with its method, path, status,
// duration and request identifier.
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
			next.ServeHTTP(rw, r)

			status := rw.statusOrDefault()
			id, _ := RequestIDFromContext(r.Context())
			level := opts.Level
			switch {
			case status >= 500:
				level = slog.LevelError
			case status >= 400:
				level = slog.LevelWarn
			}
			scoped.LogAttrs(r.Context(), level, r.Method+" "+r.URL.Path,
				slog.Int("status", status),
				slog.Duration("duration", time.Since(start)),
				slog.Int64("bytes", rw.bytes),
				slog.String(RequestIDKey, id))
		})
	}
}

// CORSOptions configures [CORS]. The zero value denies every cross-origin
// request, which is the only safe default: a permissive policy set by accident
// hands any web page on the internet the ability to read authenticated
// responses from the browser of anyone visiting it.
type CORSOptions struct {
	// AllowedOrigins lists the exact origins permitted, such as
	// "https://app.example.com". The single entry "*" allows any origin and
	// is rejected outright when AllowCredentials is also set.
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
// The error returned describes a policy that cannot be served safely. A valid
// policy returns a nil error.
func CORS(opts CORSOptions) (Middleware, error) {
	wildcard := slices.Contains(opts.AllowedOrigins, "*")
	if wildcard && opts.AllowCredentials {
		return nil, ErrCORSWildcardCredentials
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
			origin := r.Header.Get("Origin")
			if origin == "" || !allowed(origin) {
				// Same-origin requests and denied origins are served without
				// any CORS header, which is what makes the browser refuse to
				// expose the response.
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			header := w.Header()
			if wildcard && !opts.AllowCredentials {
				header.Set("Access-Control-Allow-Origin", "*")
			} else {
				header.Set("Access-Control-Allow-Origin", origin)
				header.Add("Vary", "Origin")
			}
			if opts.AllowCredentials {
				header.Set("Access-Control-Allow-Credentials", "true")
			}
			if exposeHeaders != "" {
				header.Set("Access-Control-Expose-Headers", exposeHeaders)
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				header.Set("Access-Control-Allow-Methods", allowMethods)
				header.Set("Access-Control-Allow-Headers", allowHeaders)
				header.Set("Access-Control-Max-Age", maxAge)
				header.Add("Vary", "Access-Control-Request-Method")
				header.Add("Vary", "Access-Control-Request-Headers")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// SecurityHeaders sets conservative response headers on every response.
//
// It sets X-Content-Type-Options to stop a browser from guessing a content
// type other than the one declared, X-Frame-Options to prevent framing, and a
// referrer policy that keeps paths and query strings from leaking to third
// parties. Existing values are never overwritten, so a handler or a later
// middleware can opt out per response.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := w.Header()
			setIfAbsent(header, "X-Content-Type-Options", "nosniff")
			setIfAbsent(header, "X-Frame-Options", "DENY")
			setIfAbsent(header, "Referrer-Policy", "strict-origin-when-cross-origin")
			next.ServeHTTP(w, r)
		})
	}
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
type responseWriter struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

// asResponseWriter wraps w unless it is already a *responseWriter, so that
// nesting middleware does not stack wrappers.
func asResponseWriter(w http.ResponseWriter) *responseWriter {
	if rw, ok := w.(*responseWriter); ok {
		return rw
	}
	return &responseWriter{ResponseWriter: w}
}

// WriteHeader records the status and forwards it, ignoring repeated calls the
// way net/http does.
func (w *responseWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	w.status = status
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

// Write records the byte count and marks the response as started.
func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.written {
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
func (w *responseWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
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
