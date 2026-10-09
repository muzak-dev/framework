package muzak

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// observability is what an application keeps for tracing and the request
// observer, built once by [New] from [AppOptions.Tracing] and
// [AppOptions.Observer].
//
// It is nil when neither is configured, which is what makes both free when
// off: no middleware is installed, and every hook the rest of the package
// calls returns at its first comparison.
type observability struct {
	tracer          Tracer
	parent          TraceParentPolicy
	sampler         Sampler
	recordUserAgent bool
	observer        RequestObserver

	// clientIP is the application's resolver, consulted for whether a
	// request's peer is a trusted proxy under
	// [TraceParentFromTrustedProxies].
	clientIP *clientIPResolver
	logger   *slog.Logger
}

// newObservability returns the state for opts, or nil when neither tracing nor
// an observer is configured.
func newObservability(opts AppOptions, clientIP *clientIPResolver, logger *slog.Logger) *observability {
	if !opts.Tracing.enabled() && opts.Observer == nil {
		return nil
	}
	return &observability{
		tracer:          opts.Tracing.Tracer,
		parent:          opts.Tracing.Parent,
		sampler:         opts.Tracing.Sampler,
		recordUserAgent: opts.Tracing.RecordUserAgent,
		observer:        opts.Observer,
		clientIP:        clientIP,
		logger:          Scoped(logger, ScopeServer),
	}
}

// tracing reports whether a server span is started for every request.
func (o *observability) tracing() bool { return o != nil && o.tracer != nil }

// middleware starts the server span, counts the request body for the
// observer, and finishes both once the rest of the chain has returned.
//
// It is installed directly below [RequestID], above recovery and the access
// log: above the access log so that the line it writes can carry the trace,
// and above recovery so that the status it reads is the one recovery wrote.
// What it finishes is finished from a deferred function, so the abort of a
// response that had already started, which reaches it as a panic, is still
// recorded and the panic is resumed unchanged.
func (o *observability) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := asResponseWriter(w)
		var active *activeSpan
		if o.tracer != nil {
			r, active = o.startServerSpan(r, start)
		}
		var body *countingBody
		if o.observer != nil && r.Body != nil && r.Body != http.NoBody {
			if active == nil {
				// The request is about to be changed, and it is the caller's,
				// so the change is made to a copy.
				r = r.WithContext(r.Context())
			}
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}
		defer func() {
			recovered := recover()
			o.finish(r, rw, active, body, start, recovered != nil)
			if recovered != nil {
				panic(recovered)
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

// startServerSpan gives a request its span context, continuing the trace the
// request carries when the policy allows and starting a new one otherwise, and
// starts the server span when the trace is sampled.
//
// A traceparent the policy does not accept is not even parsed. The span's
// identity is decided here whether or not it is sampled, because an unsampled
// trace is still propagated and still logged.
func (o *observability) startServerSpan(r *http.Request, start time.Time) (*http.Request, *activeSpan) {
	var parent SpanContext
	continued := false
	if o.acceptsParent(r) {
		parent, continued = extractTraceContext(r.Header)
	}
	active := &activeSpan{tracer: o.tracer}
	active.sc.SpanID = newSpanID()
	if continued {
		active.sc.TraceID = parent.TraceID
		active.sc.TraceFlags = parent.TraceFlags & TraceFlagsSampled
		active.sc.TraceState = parent.TraceState
	} else {
		active.sc.TraceID = newTraceID()
		if o.sample(r, active.sc.TraceID) {
			active.sc.TraceFlags = TraceFlagsSampled
		}
	}
	if active.sc.IsSampled() {
		active.span = o.startSpan(r.Context(), SpanStart{
			Name:        spanMethod(observedMethod(r.Method, false)),
			Kind:        SpanKindServer,
			SpanContext: active.sc,
			Parent:      parent,
			StartTime:   start,
			Attributes:  o.serverAttributes(r),
		})
	}
	return r.WithContext(context.WithValue(r.Context(), spanContextKey{}, active)), active
}

// acceptsParent reports whether the policy lets this request continue the
// trace it names.
func (o *observability) acceptsParent(r *http.Request) bool {
	switch o.parent {
	case TraceParentAccept:
		return true
	case TraceParentFromTrustedProxies:
		peer, ok := peerAddr(r.RemoteAddr)
		return ok && o.clientIP.trusts(peer)
	default:
		return false
	}
}

// sample asks the configured sampler whether a new trace is recorded. A
// sampler that panics is logged and its trace left unrecorded, which is the
// cheaper of the two ways to be wrong.
func (o *observability) sample(r *http.Request, trace TraceID) (sampled bool) {
	if o.sampler == nil {
		return true
	}
	defer o.recoverHook("the tracing sampler", func() { sampled = false })
	return o.sampler(r, trace)
}

// startSpan calls the Tracer, recovering a panic so that a broken Tracer costs
// the request its span rather than its connection: this runs above recovery,
// where nothing else would catch it.
func (o *observability) startSpan(ctx context.Context, start SpanStart) (span Span) {
	defer o.recoverHook("the tracer", func() { span = nil })
	return o.tracer.StartSpan(ctx, start)
}

// serverAttributes returns the attributes a server span starts with. Every one
// of them is bounded: the method is one of a fixed set, the scheme and
// protocol version are constants, the host is a validated name of at most 253
// bytes, and the user agent, which is only recorded when asked for, is cut to
// [maxQuotedLength].
func (o *observability) serverAttributes(r *http.Request) []slog.Attr {
	attrs := make([]slog.Attr, 0, 7)
	attrs = append(attrs,
		slog.String("http.request.method", observedMethod(r.Method, false)),
		slog.String("url.scheme", requestScheme(r)))
	if version := protocolVersion(r); version != "" {
		attrs = append(attrs, slog.String("network.protocol.version", version))
	}
	if host, port, ok := serverAddress(r.Host); ok {
		attrs = append(attrs, slog.String("server.address", host))
		if port != 0 {
			attrs = append(attrs, slog.Int("server.port", port))
		}
	}
	if o.recordUserAgent {
		if agent := r.Header.Get("User-Agent"); agent != "" {
			attrs = append(attrs, slog.String("user_agent.original", strings.ToValidUTF8(truncateForMessage(agent), string(utf8.RuneError))))
		}
	}
	return attrs
}

// finish ends the server span and tells the observer, once the response is
// finished. panicking reports the abort of a response, which is the only
// panic that passes recovery on its way up to here.
func (o *observability) finish(r *http.Request, rw *responseWriter, active *activeSpan, body *countingBody, start time.Time, panicking bool) {
	status, aborted := requestOutcome(rw, panicking)
	route, routed := RouteFromContext(r.Context())
	method := observedMethod(r.Method, registeredMethod(r))
	if active != nil && active.span != nil {
		o.endServerSpan(active, method, route, routed, status, aborted)
	}
	if o.observer == nil {
		return
	}
	observation := RequestObservation{
		Method:       method,
		Route:        route,
		Status:       status,
		Duration:     time.Since(start),
		ResponseSize: rw.bytes,
		Aborted:      aborted,
	}
	if body != nil {
		observation.RequestSize = body.n.Load()
	}
	if active != nil {
		observation.SpanContext = active.sc
	}
	o.observe(observation)
}

// requestOutcome returns the status a request ended with and whether its
// connection was aborted, judged the way the access log judges them so that
// the log, the span and the observer agree: a panic passing through after the
// response started is an abort with the status already sent, and one before
// it is the 500 recovery writes.
func requestOutcome(rw *responseWriter, panicking bool) (status int, aborted bool) {
	status = rw.statusOrDefault()
	if panicking {
		if rw.written {
			aborted = !rw.hijacked
		} else {
			status = http.StatusInternalServerError
		}
	}
	return status, aborted
}

// Descriptions given to a server span that failed in a way its status code
// does not show. A 5xx gets none, because the code already says it.
const (
	abortedSpanDescription = "the response was aborted after it had started"
	failedSpanDescription  = "the handler failed after the response had started"
)

// endServerSpan names the span after its route, records how the request
// ended, and ends it.
func (o *observability) endServerSpan(active *activeSpan, method, route string, routed bool, status int, aborted bool) {
	defer o.recoverHook("the tracer", nil)
	span := active.span
	attrs := make([]slog.Attr, 0, 4)
	attrs = append(attrs, slog.Int("http.response.status_code", status))
	if routed {
		span.SetName(spanMethod(method) + " " + route)
		attrs = append(attrs, slog.String("http.route", route))
		if method != observedMethod(method, false) {
			// A method no standard defines, which the span started with as
			// _OTHER, and which a route was registered for.
			attrs = append(attrs, slog.String("http.request.method", method))
		}
	}
	failed := status >= http.StatusInternalServerError || aborted || active.failed
	if failed {
		errorType := active.errorType
		if errorType == "" {
			errorType = "_OTHER"
			if status >= http.StatusInternalServerError {
				errorType = strconv.Itoa(status)
			}
		}
		attrs = append(attrs, slog.String("error.type", errorType))
	}
	span.SetAttributes(attrs...)
	switch {
	case aborted:
		span.SetStatus(SpanStatusError, abortedSpanDescription)
	case active.failed:
		span.SetStatus(SpanStatusError, failedSpanDescription)
	case failed:
		span.SetStatus(SpanStatusError, "")
	}
	span.End()
}

// observe calls the observer, recovering a panic for the same reason
// [observability.startSpan] does.
func (o *observability) observe(observation RequestObservation) {
	defer o.recoverHook("the request observer", nil)
	o.observer.ObserveRequest(observation)
}

// recoverHook recovers a panic in a callback the application supplied, logs
// it with its stack, and runs fallback, if any, to set what the caller
// returns. It must be called directly by a deferred statement, which is where
// recover has an effect.
func (o *observability) recoverHook(what string, fallback func()) {
	recovered := recover()
	if recovered == nil {
		return
	}
	o.logger.Error("muzak: "+what+" panicked; the request was served without it",
		slog.String("panic", panicValue(recovered)),
		slog.String("stack", string(debug.Stack())))
	if fallback != nil {
		fallback()
	}
}

// standardMethods are the methods HTTP defines, which are the only values of a
// method that is recorded as it is without a route vouching for it.
var standardMethods = [...]string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
	http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch,
}

// observedMethod returns the method as a span or a metric records it: as it is
// when it is a standard one or one a route registered for it answered, and
// "_OTHER" otherwise.
//
// net/http admits any token as a method, as long as the request line, so a
// method recorded as sent would let one client mint a span name and a metric
// series per request.
func observedMethod(method string, registered bool) string {
	if registered || slices.Contains(standardMethods[:], method) {
		return method
	}
	return "_OTHER"
}

// registeredMethod reports whether the request was answered by a route
// registered for its own method, which the routing table bounds. A handler
// mount answers every method a client sends, so the template it publishes
// vouches for the path alone, and its method is recorded as
// [observedMethod] records one no route vouches for.
func registeredMethod(r *http.Request) bool {
	holder, ok := r.Context().Value(routeContextKey{}).(*routeHolder)
	return ok && holder.method != "" && holder.method == r.Method
}

// spanMethod returns the method as it appears in a span name, where
// OpenTelemetry's conventions write "HTTP" for a method they record as
// "_OTHER".
func spanMethod(method string) string {
	if method == "_OTHER" {
		return "HTTP"
	}
	return method
}

// requestScheme returns the scheme the request reached this server with. A
// proxy that terminates TLS in front of it makes this "http" whatever the
// client used, since that is what this server was spoken to with.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// protocolVersion returns the HTTP version as OpenTelemetry's conventions
// write it, or nothing for one they do not name.
func protocolVersion(r *http.Request) string {
	switch {
	case r.ProtoMajor == 1 && r.ProtoMinor == 0:
		return "1.0"
	case r.ProtoMajor == 1 && r.ProtoMinor == 1:
		return "1.1"
	case r.ProtoMajor == 2:
		return "2"
	case r.ProtoMajor == 3:
		return "3"
	}
	return ""
}

// maxServerAddressLength is the longest host name DNS allows.
const maxServerAddressLength = 253

// serverAddress splits a Host header into the name and port it names, and
// reports false for one that is neither an IP address nor a name made of the
// characters DNS allows, at most [maxServerAddressLength] long. The header is
// chosen by the client, so anything else is left off the span rather than
// recorded. The port is zero when the header carries none.
func serverAddress(host string) (string, int, bool) {
	name, port := host, 0
	if split, portText, err := net.SplitHostPort(host); err == nil {
		number, err := strconv.Atoi(portText)
		if err != nil || number < 1 || number > 65535 {
			return "", 0, false
		}
		name, port = split, number
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	if addr, err := netip.ParseAddr(name); err == nil {
		return addr.WithZone("").String(), port, true
	}
	if name == "" || len(name) > maxServerAddressLength {
		return "", 0, false
	}
	for i := range len(name) {
		if !isHostNameByte(name[i]) {
			return "", 0, false
		}
	}
	return strings.ToLower(name), port, true
}

// isHostNameByte reports whether c may appear in a DNS host name.
func isHostNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.'
}

// countingBody counts the bytes read through a request body, for
// [RequestObservation.RequestSize]. The count is atomic because a handler may
// read its body from a goroutine of its own.
type countingBody struct {
	io.ReadCloser
	n atomic.Int64
}

// Read reads from the body and counts what was read.
func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

// observeFailure records on the request's span a failure [App.fail] is about
// to render or abort with. Only a failure the log records is recorded, with
// the same text the log line carries; a failure after the response started is
// always logged, so it is always recorded, and marks the span failed, since
// its status code no longer can.
func (a *App) observeFailure(c *Context, err error) {
	if !a.obs.tracing() {
		return
	}
	cause := logCause(err)
	if cause == nil && c.w.written && !c.w.hijacked && !c.endsItsOwnResponse() {
		// The one failure fail logs although logCause leaves it out: one
		// that aborts a response already on the wire.
		cause = err
	}
	if cause != nil {
		a.recordSpanFailure(c, cause)
	}
}

// observeStreamFailure records a handler failure an event stream or a
// WebSocket route logged, which ends a response that started long before.
func (a *App) observeStreamFailure(c *Context, err error) {
	if !a.obs.tracing() {
		return
	}
	a.recordSpanFailure(c, err)
}

// recordSpanFailure adds an "exception" event to the request's span, and marks
// it failed when the response had already started. The first failure recorded
// names the span's error.type, since a failure handled by another one, a
// stream's handler failing and its connection being aborted for it, is
// usually reported second.
func (a *App) recordSpanFailure(c *Context, cause error) {
	active := activeSpanFrom(c.Context())
	if active == nil || active.span == nil {
		return
	}
	defer a.obs.recoverHook("the tracer", nil)
	errorType := "panic"
	if !errors.Is(cause, errPanic) {
		errorType = fmt.Sprintf("%T", cause)
	}
	if active.errorType == "" {
		active.errorType = errorType
	}
	if c.w.written {
		active.failed = true
	}
	active.span.AddEvent("exception",
		slog.String("exception.type", errorType),
		slog.String("exception.message", strings.ToValidUTF8(truncateTo(cause.Error(), maxLoggedPanicLength), string(utf8.RuneError))))
}

// observabilityComponents adds a Tracer and an observer that implement
// [Lifecycle] to the components the application starts and stops, so that an
// exporter's background work begins before the first request and its last
// spans are flushed after the last one, without the application having to
// register the same value twice.
//
// A value registered with [WithLifecycle] as well is not added again. That is
// told by identity, which only a pointer has in a form that can be compared
// without risk of a panic; any other kind of value is added as it is.
func observabilityComponents(components []Lifecycle, candidates ...any) []Lifecycle {
	for _, candidate := range candidates {
		component, ok := candidate.(Lifecycle)
		if !ok {
			continue
		}
		if slices.ContainsFunc(components, func(existing Lifecycle) bool { return sameComponent(existing, component) }) {
			continue
		}
		components = append(components, component)
	}
	return components
}

// sameComponent reports whether two components are the same pointer.
func sameComponent(a, b Lifecycle) bool {
	ta := reflect.TypeOf(a)
	return ta == reflect.TypeOf(b) && ta.Kind() == reflect.Pointer && a == b
}
