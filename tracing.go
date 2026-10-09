package muzak

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"
)

// SpanKind says what part a span plays in a trace. The values are the ones
// OpenTelemetry's protocol uses, so an exporter can write them as they are.
type SpanKind int

const (
	// SpanKindInternal is work inside one process, which is what a span a
	// handler starts for a step of its own is.
	SpanKindInternal SpanKind = 1
	// SpanKindServer is the handling of a request a client sent, which is
	// what the span Muzak starts for each request is.
	SpanKindServer SpanKind = 2
	// SpanKindClient is a request this process sends and waits on.
	SpanKindClient SpanKind = 3
	// SpanKindProducer is a message this process hands off without waiting.
	SpanKindProducer SpanKind = 4
	// SpanKindConsumer is the handling of a message a producer handed off.
	SpanKindConsumer SpanKind = 5
)

// SpanStatusCode is the outcome a span records. The values are the ones
// OpenTelemetry's protocol uses.
type SpanStatusCode int

const (
	// SpanStatusUnset is a span nothing has judged, which is how a request
	// answered with anything but a 5xx ends.
	SpanStatusUnset SpanStatusCode = 0
	// SpanStatusOK is a span the application has explicitly marked as a
	// success, overriding anything a backend would otherwise infer.
	SpanStatusOK SpanStatusCode = 1
	// SpanStatusError is a span that failed.
	SpanStatusError SpanStatusCode = 2
)

// SpanStart describes a span being started, as it is handed to a [Tracer].
//
// Muzak decides the span's identity before the Tracer sees it: SpanContext is
// already filled in, with identifiers drawn from crypto/rand, and that is the
// identity propagated downstream and written to the logs. A Tracer records it
// rather than choosing one of its own.
type SpanStart struct {
	// Name is the span's name. For a server span it is the request method
	// until routing is done, and is changed with [Span.SetName] once the route
	// is known.
	Name string
	// Kind is the part the span plays.
	Kind SpanKind
	// SpanContext is the new span's own identity.
	SpanContext SpanContext
	// Parent is the span this one is a child of, which is invalid for a span
	// that starts a trace. A parent read from a request has Remote set.
	Parent SpanContext
	// StartTime is when the span began.
	StartTime time.Time
	// Attributes are the span's initial attributes. The slice belongs to the
	// caller: a Tracer that keeps it beyond the call copies it first.
	Attributes []slog.Attr
}

// Tracer records spans. It is the seam between Muzak and whatever receives
// the traces: [muzak.dev/framework/otlp] implements it for any
// OpenTelemetry collector, and an adapter to another SDK is a type with one
// method.
//
// StartSpan is called for every sampled span, on the request's goroutine, and
// must be safe for concurrent use; it should return quickly and never block on
// the network, since a request waits on it. It is never called for a trace
// that is not sampled: such a request still has identifiers, which are
// propagated and logged, but nothing is recorded.
//
// A Tracer that also implements [Lifecycle] is started and stopped with the
// application, so an exporter's background work begins before the first
// request and its last spans are flushed after the last one.
type Tracer interface {
	StartSpan(ctx context.Context, start SpanStart) Span
}

// Span is a span a [Tracer] is recording.
//
// Its methods may be called from several goroutines at once, since a handler
// can hand its context to work it runs in parallel, and every one of them
// must be safe to call after End, which should ignore them. Attributes take
// the form of [slog.Attr], so the values a handler logs and the values it
// records on a span are built the same way.
type Span interface {
	// SetName replaces the span's name.
	SetName(name string)
	// SetAttributes adds attributes, replacing any already recorded under
	// the same key.
	SetAttributes(attrs ...slog.Attr)
	// AddEvent records something that happened at a point in the span.
	AddEvent(name string, attrs ...slog.Attr)
	// SetStatus records the span's outcome.
	SetStatus(code SpanStatusCode, description string)
	// End completes the span. Only the first call counts. Muzak ends the
	// server span itself once the response is written; a span a handler
	// started with [StartSpan] is the handler's to end.
	End()
}

// TraceParentPolicy decides whose traceparent header a request may continue.
type TraceParentPolicy int

const (
	// TraceParentAccept continues any valid traceparent a request carries,
	// which is what a service inside a system that traces end to end wants,
	// and is the default.
	TraceParentAccept TraceParentPolicy = iota
	// TraceParentFromTrustedProxies continues a traceparent only when the
	// connection came from an address listed in
	// [ClientIPOptions.TrustedProxies], and starts a new trace for any other
	// request. That is what a service reached directly from the internet
	// wants: a client choosing its own trace identifier can attach its
	// requests to someone else's trace, or decide for the service that every
	// one of its requests is sampled.
	TraceParentFromTrustedProxies
	// TraceParentIgnore always starts a new trace.
	TraceParentIgnore
)

// Sampler decides whether a trace this server starts is recorded. A trace a
// request continues keeps the decision its caller made, so a sampler is only
// consulted for a request that arrived with no trace, or with one the
// [TraceParentPolicy] did not accept.
type Sampler func(r *http.Request, trace TraceID) bool

// SampleRatio returns a [Sampler] that records the given fraction of new
// traces, from 0 for none to 1 for all.
//
// The decision is taken from the trace identifier rather than drawn afresh, so
// every service sampling at the same ratio makes the same decision for the
// same trace, and the identifier is random enough to make the fraction hold.
func SampleRatio(fraction float64) Sampler {
	if !(fraction > 0) {
		return func(*http.Request, TraceID) bool { return false }
	}
	if fraction >= 1 {
		return func(*http.Request, TraceID) bool { return true }
	}
	bound := uint64(fraction * math.MaxInt64)
	return func(_ *http.Request, trace TraceID) bool {
		return binary.BigEndian.Uint64(trace[8:])>>1 < bound
	}
}

// TracingOptions configures tracing. The zero value leaves it off: no header
// is parsed, no identifier is drawn and nothing is allocated for it on any
// request.
//
//	exporter, err := otlp.New(otlp.Options{Endpoint: "http://localhost:4318", ServiceName: "shop"})
//	if err != nil {
//		log.Fatal(err)
//	}
//	app := muzak.New(muzak.AppOptions{
//		Tracing: muzak.TracingOptions{Tracer: exporter},
//	})
//
// With a Tracer, every request gets a server span named after its method and
// route template, such as "GET /items/{id}", never after its path: a path
// carries identifiers, which makes one name per request, and often personal
// data. A request no route answers is named after its method alone. The span
// carries the OpenTelemetry HTTP attributes http.request.method, http.route,
// http.response.status_code, url.scheme, server.address, server.port and
// network.protocol.version; a 5xx, a response aborted after it started and a
// stream or WebSocket handler that failed mark it as an error. A failure the
// log records is added to the span as an "exception" event carrying the same
// text the log line does, and nothing more. The span ends once the response
// has been written, which for an event stream or a WebSocket is when the
// stream or the connection ends.
type TracingOptions struct {
	// Tracer receives the spans. Tracing is on when it is set.
	Tracer Tracer

	// Parent decides whose traceparent a request may continue, defaulting
	// to [TraceParentAccept]. See [TraceParentPolicy].
	Parent TraceParentPolicy

	// Sampler decides whether a trace this server starts is recorded. When
	// nil, every one is. See [SampleRatio].
	Sampler Sampler

	// RecordUserAgent adds the request's User-Agent to the server span as
	// user_agent.original, cut to 1 KiB. It is off by default because the
	// header is whatever the client chose to send, and in a browser it
	// identifies the visitor's device more closely than most deployments
	// need to record.
	RecordUserAgent bool
}

// enabled reports whether tracing is on.
func (o TracingOptions) enabled() bool { return o.Tracer != nil }

// validate reports options that cannot take effect, so that a sampler or a
// policy set without a Tracer is not silently ignored.
func (o TracingOptions) validate() error {
	var problems []string
	if o.Parent < TraceParentAccept || o.Parent > TraceParentIgnore {
		problems = append(problems, fmt.Sprintf("Tracing.Parent is %d, which is not a TraceParentPolicy", int(o.Parent)))
	}
	if o.Tracer == nil {
		if o.Sampler != nil {
			problems = append(problems, "Tracing.Sampler is set but Tracing.Tracer is not, so nothing would be sampled")
		}
		if o.Parent != TraceParentAccept {
			problems = append(problems, "Tracing.Parent is set but Tracing.Tracer is not, so no traceparent is ever read")
		}
		if o.RecordUserAgent {
			problems = append(problems, "Tracing.RecordUserAgent is set but Tracing.Tracer is not, so there is no span to record it on")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	errs := make([]error, len(problems))
	for i, problem := range problems {
		errs[i] = fmt.Errorf("muzak: %s; set a Tracer or remove the option", problem)
	}
	return errors.Join(errs...)
}

// activeSpan is what a request's context carries for its current span: the
// identity that is propagated and logged, the span being recorded, and the
// Tracer that children are started through.
//
// failed and errorType are written by the goroutine serving the request, when
// a failure is recorded, and read by the same goroutine when the server span
// ends, so they need no lock. A child started with [StartSpan] never has them
// set.
type activeSpan struct {
	sc     SpanContext
	span   Span
	tracer Tracer

	failed    bool
	errorType string
}

// StartSpan starts a span as a child of the one ctx carries, through the
// application's [Tracer], and returns a context carrying the new span along
// with the span itself:
//
//	ctx, span := muzak.StartSpan(c.Context(), "charge card", muzak.SpanKindClient,
//		slog.String("payment.provider", "stripe"))
//	defer span.End()
//
// The child shares the trace and its sampling decision, and has a new
// identifier of its own. A context carrying no span, which is every context
// when tracing is not configured, returns ctx unchanged and a span that
// records nothing, so code written to trace costs nothing where tracing is
// off. A trace that is not sampled gets a child that is propagated but not
// recorded.
func StartSpan(ctx context.Context, name string, kind SpanKind, attrs ...slog.Attr) (context.Context, Span) {
	parent := activeSpanFrom(ctx)
	if parent == nil {
		return ctx, noopSpan{}
	}
	child := &activeSpan{
		sc: SpanContext{
			TraceID:    parent.sc.TraceID,
			SpanID:     newSpanID(),
			TraceFlags: parent.sc.TraceFlags & TraceFlagsSampled,
			TraceState: parent.sc.TraceState,
		},
		tracer: parent.tracer,
	}
	if kind == 0 {
		kind = SpanKindInternal
	}
	if child.sc.IsSampled() && child.tracer != nil {
		child.span = child.tracer.StartSpan(ctx, SpanStart{
			Name:        name,
			Kind:        kind,
			SpanContext: child.sc,
			Parent:      parent.sc,
			StartTime:   time.Now(),
			Attributes:  attrs,
		})
	}
	ctx = context.WithValue(ctx, spanContextKey{}, child)
	if child.span == nil {
		return ctx, noopSpan{}
	}
	return ctx, child.span
}

// SpanFromContext returns the span ctx carries, which inside a handler is the
// request's server span, so that a handler can add what it knows to it:
//
//	muzak.SpanFromContext(c.Context()).SetAttributes(slog.String("tenant.id", tenant))
//
// It returns a span that records nothing when there is none, so the call is
// always safe. The server span is Muzak's to end.
func SpanFromContext(ctx context.Context) Span {
	if active := activeSpanFrom(ctx); active != nil && active.span != nil {
		return active.span
	}
	return noopSpan{}
}

// noopSpan is the span returned where nothing is recorded. It is a zero-size
// value, so returning it as a Span allocates nothing.
type noopSpan struct{}

func (noopSpan) SetName(string)                   {}
func (noopSpan) SetAttributes(...slog.Attr)       {}
func (noopSpan) AddEvent(string, ...slog.Attr)    {}
func (noopSpan) SetStatus(SpanStatusCode, string) {}
func (noopSpan) End()                             {}
