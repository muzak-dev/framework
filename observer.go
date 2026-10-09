package muzak

import "time"

// RequestObserver is told about every request once its response is written,
// which is the hook request metrics are built on: a Prometheus histogram, an
// OpenTelemetry meter or a counter of one's own is a type with one method,
// and Muzak needs no dependency on any of them.
//
//	type metrics struct{ duration *prometheus.HistogramVec }
//
//	func (m metrics) ObserveRequest(o muzak.RequestObservation) {
//		m.duration.WithLabelValues(o.Method, o.Route, strconv.Itoa(o.Status)).Observe(o.Duration.Seconds())
//	}
//
//	app := muzak.New(muzak.AppOptions{Observer: metrics{duration}})
//
// ObserveRequest is called exactly once for every request the application
// serves, on the goroutine that served it, after the response has been
// written: a routed request, one nothing routes, one that failed, panicked or
// was aborted after its response started, and an event stream or a WebSocket
// once it has ended. It should be quick, since the connection is not handed
// back to net/http until it returns, and must be safe for concurrent use. A
// panic inside it is recovered and logged rather than allowed to drop the
// connection.
//
// An observer that also implements [Lifecycle] is started and stopped with the
// application.
type RequestObserver interface {
	ObserveRequest(RequestObservation)
}

// RequestObserverFunc adapts a function to the [RequestObserver] interface.
type RequestObserverFunc func(RequestObservation)

// ObserveRequest calls f.
func (f RequestObserverFunc) ObserveRequest(o RequestObservation) { f(o) }

// RequestObservation describes one request after its response was written.
//
// Every field that could become a metric label has a bounded set of values,
// so labelling by all of them cannot grow a time series per request: Method is
// one of the standard methods or one a route was registered for, Route is a
// registered template, and Status is a status code. The path, which carries
// identifiers and is chosen by the client, is deliberately absent.
type RequestObservation struct {
	// Method is the request method when it is one of the nine HTTP defines,
	// or one a route was registered for and this request matched, and
	// "_OTHER" for anything else a client sent, as OpenTelemetry's
	// conventions spell it.
	Method string
	// Route is the template of the route that answered, such as
	// "/items/{id}", and empty when no route did: a 404, a 405, an OPTIONS
	// answered from the route table, the documentation, or a frontend mount.
	Route string
	// Status is the status the response was sent with: 101 for a WebSocket
	// or a hijacked connection, and the status already sent for a response
	// that was aborted after it started.
	Status int
	// Duration is the time from the request reaching the application to the
	// response being finished, which for an event stream or a WebSocket is
	// how long it stayed open.
	Duration time.Duration
	// RequestSize is the number of bytes of the request body the server
	// read. That is the whole body of a request that was served, since what a
	// handler leaves unread is drained after it, up to 4 KiB, and less than
	// the client sent for one refused part way, such as a body over its size
	// limit, which is not read past the limit.
	RequestSize int64
	// ResponseSize is the number of bytes of the response body written,
	// before any compression applied by middleware installed outside the
	// application.
	ResponseSize int64
	// Aborted reports that the connection was cut after the response had
	// started, because the handler failed or panicked while writing it. The
	// status is then the one already sent, which reads as a success.
	Aborted bool
	// SpanContext is the request's server span when tracing is configured,
	// which is what an exemplar attached to a metric needs, and the zero
	// value otherwise.
	SpanContext SpanContext
}
