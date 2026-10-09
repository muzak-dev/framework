// Package otlp sends the spans a Muzak application records to an
// OpenTelemetry collector, or to any backend that accepts OTLP over HTTP with
// the JSON encoding, without a dependency outside the standard library.
//
//	exporter, err := otlp.New(otlp.Options{
//		Endpoint:    "http://localhost:4318",
//		ServiceName: "shop",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	app := muzak.New(muzak.AppOptions{
//		Tracing: muzak.TracingOptions{Tracer: exporter},
//	})
//
// The [Exporter] is the application's [muzak.Tracer] and one of its lifecycle
// components: the application starts it before serving the first request and
// stops it after the last, and stopping it sends what is still queued.
//
// # What it promises a request
//
// Nothing the exporter does is waited on by a request. A span that ends is put
// in a bounded queue and the request moves on; when the queue is full the span
// is dropped and counted rather than waited for. Batches are sent by one
// goroutine of the exporter's own.
//
// # What it promises the collector
//
// Batches are posted to the endpoint's /v1/traces with the JSON encoding the
// OTLP specification gives, gzip-compressed when [Options.Gzip] is set. A
// batch answered with 429, 502, 503 or 504, or that could not be delivered, is
// retried with exponential backoff and jitter, honouring a Retry-After within
// [Options.RetryMaxInterval]; any other answer is final. An answer is read up
// to 64 KiB and no further, and every attempt is bounded by
// [Options.Timeout].
//
// # What it does not do
//
// It does not read the OTEL_EXPORTER_OTLP_* environment variables, speak
// gRPC or protobuf, or export metrics or logs. Request metrics are a
// [muzak.RequestObserver]'s to record.
package otlp
