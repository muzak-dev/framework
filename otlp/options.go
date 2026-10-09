package otlp

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Defaults applied to the [Options] fields left at zero.
const (
	// DefaultTimeout bounds one attempt to deliver a batch, from dialling to
	// reading the collector's answer.
	DefaultTimeout = 10 * time.Second
	// DefaultQueueSize is how many ended spans wait to be exported before
	// more are dropped.
	DefaultQueueSize = 2048
	// DefaultBatchSize is how many spans are sent in one request.
	DefaultBatchSize = 512
	// DefaultBatchInterval is how long a span waits for its batch to fill
	// before the batch is sent anyway.
	DefaultBatchInterval = 5 * time.Second
	// DefaultRetryInitialInterval is the wait before the first retry.
	DefaultRetryInitialInterval = time.Second
	// DefaultRetryMaxInterval caps any one wait between attempts, including
	// one a collector asks for with Retry-After.
	DefaultRetryMaxInterval = 30 * time.Second
	// DefaultRetryMaxElapsedTime is how long a batch is retried for in all
	// before it is given up on.
	DefaultRetryMaxElapsedTime = time.Minute
)

// MaxQueueSize is the largest [Options.QueueSize] accepted. The queue is
// allocated up front, so a size typed with too many zeroes would cost its
// memory before a single span had been recorded.
const MaxQueueSize = 1 << 20

// Options configures an [Exporter]. Only Endpoint is required.
type Options struct {
	// Endpoint is the base URL of the collector's OTLP/HTTP receiver, such as
	// "http://localhost:4318" or "https://otlp.example.com/otlp"; spans are
	// posted to Endpoint followed by "/v1/traces". It must be an absolute
	// http or https URL with a host and nothing after its path.
	//
	// A URL carrying credentials, "https://user:secret@collector", is
	// refused: credentials in a URL end up in error messages, proxy logs and
	// process listings. Send them in Headers.
	//
	// Plain http sends the spans, and every header, unencrypted. That is
	// what a collector on localhost or beside the service in a pod is spoken
	// to with; one reached over any other network wants https.
	Endpoint string

	// ServiceName is the service.name resource attribute every span is
	// reported under, which is what a tracing backend lists services by. It
	// defaults to "unknown_service:" followed by the executable's name, as
	// OpenTelemetry's conventions say.
	ServiceName string

	// ServiceVersion is the service.version resource attribute, left off when
	// empty.
	ServiceVersion string

	// ResourceAttributes are further resource attributes, such as
	// deployment.environment.name. ServiceName and ServiceVersion take
	// precedence over an attribute here with the same key.
	ResourceAttributes []slog.Attr

	// Headers are sent with every request, which is where a backend's API key
	// goes. A name must be a valid header name and a value may not hold a
	// line break or any other control character but a tab; one that does is
	// refused by [New]. The headers that describe the request itself,
	// Content-Type, Content-Encoding, Content-Length, Host and the hop-by-hop
	// headers, are the exporter's to set and are refused too. Values are never
	// logged, and [Headers] prints its names alone.
	Headers Headers

	// Gzip compresses every request body, which a collector reached over a
	// network usually wants: span batches are repetitive and shrink several
	// times over.
	Gzip bool

	// Timeout bounds one attempt to deliver a batch. It defaults to
	// [DefaultTimeout].
	Timeout time.Duration

	// QueueSize is how many ended spans may wait to be exported. A span that
	// ends when the queue is full is dropped and counted in [Stats], because
	// the alternative is a request waiting on a collector. It defaults to
	// [DefaultQueueSize] and may not exceed [MaxQueueSize].
	QueueSize int

	// BatchSize is how many spans are sent in one request, at most
	// QueueSize. It defaults to [DefaultBatchSize].
	BatchSize int

	// BatchInterval is how long the spans of a batch that has not filled
	// wait before it is sent anyway. It defaults to [DefaultBatchInterval].
	BatchInterval time.Duration

	// RetryInitialInterval, RetryMaxInterval and RetryMaxElapsedTime shape
	// the retries. A batch the collector answers with 429, 502, 503 or 504,
	// or that could not be delivered at all, is sent again after a wait that
	// starts at RetryInitialInterval and doubles each time up to
	// RetryMaxInterval, with up to half of it drawn at random so that many
	// instances refused at once do not return at once. A Retry-After from the
	// collector replaces the wait, capped at RetryMaxInterval. A batch still
	// refused after RetryMaxElapsedTime is given up on and counted as failed.
	// Any other answer, every other 4xx and 5xx included, is final. They
	// default to [DefaultRetryInitialInterval], [DefaultRetryMaxInterval] and
	// [DefaultRetryMaxElapsedTime].
	RetryInitialInterval time.Duration
	RetryMaxInterval     time.Duration
	RetryMaxElapsedTime  time.Duration

	// Client sends the requests, which is how a proxy, a custom CA or a
	// client certificate is configured. The exporter uses a copy that never
	// follows a redirect, since a redirect would carry Headers to wherever
	// it pointed, and closes the copy's idle connections when it stops. It
	// defaults to a client of its own.
	Client *http.Client

	// Logger receives what the exporter has to report: a batch the
	// collector refused or that could not be delivered, and spans dropped
	// because the queue was full, at most once per BatchInterval. It defaults
	// to [slog.Default].
	Logger *slog.Logger
}

// Headers are the extra headers an [Exporter] sends. Their values are usually
// credentials, so a Headers value prints its names and never its values,
// whether it is formatted with fmt or logged with slog.
type Headers map[string]string

// String lists the header names with their values redacted.
func (h Headers) String() string {
	names := slices.Sorted(maps.Keys(h))
	for i, name := range names {
		names[i] = name + ": [redacted]"
	}
	return "{" + strings.Join(names, ", ") + "}"
}

// GoString is String, so that the %#v verb does not print the values either.
func (h Headers) GoString() string { return h.String() }

// MarshalJSON writes the header names with redacted values, which is what
// slog's JSON handler, and any configuration dump, writes for an Options
// value holding them.
func (h Headers) MarshalJSON() ([]byte, error) {
	redacted := make(map[string]string, len(h))
	for name := range h {
		redacted[name] = "[redacted]"
	}
	return json.Marshal(redacted, json.Deterministic(true))
}

// LogValue renders the header names, each with a redacted value.
func (h Headers) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, len(h))
	for _, name := range slices.Sorted(maps.Keys(h)) {
		attrs = append(attrs, slog.String(name, "[redacted]"))
	}
	return slog.GroupValue(attrs...)
}

// reservedHeaders are the headers an exporter sets itself, or that belong to
// the connection rather than the request, keyed by their canonical spelling.
var reservedHeaders = map[string]bool{
	"Content-Type":        true,
	"Content-Encoding":    true,
	"Content-Length":      true,
	"Host":                true,
	"Transfer-Encoding":   true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Upgrade":             true,
	"Proxy-Authorization": true,
}

// config is Options resolved: defaults applied, the endpoint parsed and the
// headers built once.
type config struct {
	url            string
	header         http.Header
	headerNames    []string
	gzip           bool
	timeout        time.Duration
	queueSize      int
	batchSize      int
	batchInterval  time.Duration
	retryInitial   time.Duration
	retryMax       time.Duration
	retryElapsed   time.Duration
	serviceName    string
	serviceVersion string
	resource       []slog.Attr
	logger         *slog.Logger
}

// resolve validates the options and fills in the defaults, reporting every
// problem at once.
func (o Options) resolve() (config, error) {
	var errs []error
	cfg := config{
		gzip:           o.Gzip,
		timeout:        orDefault(o.Timeout, DefaultTimeout),
		queueSize:      orDefault(o.QueueSize, DefaultQueueSize),
		batchSize:      orDefault(o.BatchSize, DefaultBatchSize),
		batchInterval:  orDefault(o.BatchInterval, DefaultBatchInterval),
		retryInitial:   orDefault(o.RetryInitialInterval, DefaultRetryInitialInterval),
		retryMax:       orDefault(o.RetryMaxInterval, DefaultRetryMaxInterval),
		retryElapsed:   orDefault(o.RetryMaxElapsedTime, DefaultRetryMaxElapsedTime),
		serviceName:    o.ServiceName,
		serviceVersion: o.ServiceVersion,
		resource:       slices.Clone(o.ResourceAttributes),
		logger:         o.Logger,
	}
	if cfg.serviceName == "" {
		cfg.serviceName = "unknown_service:" + filepath.Base(os.Args[0])
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}
	endpoint, err := parseEndpoint(o.Endpoint)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.url = endpoint
	cfg.header, cfg.headerNames, err = buildHeader(o.Headers, o.Gzip)
	if err != nil {
		errs = append(errs, err)
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"Timeout", o.Timeout}, {"BatchInterval", o.BatchInterval},
		{"RetryInitialInterval", o.RetryInitialInterval}, {"RetryMaxInterval", o.RetryMaxInterval},
		{"RetryMaxElapsedTime", o.RetryMaxElapsedTime},
	} {
		if d.value < 0 {
			errs = append(errs, fmt.Errorf("otlp: %s is %v, which is negative; leave it at zero for the default", d.name, d.value))
		}
	}
	switch {
	case o.QueueSize < 0 || o.QueueSize > MaxQueueSize:
		errs = append(errs, fmt.Errorf("otlp: QueueSize is %d, but it must be between 1 and %d, or zero for the default", o.QueueSize, MaxQueueSize))
	case o.BatchSize < 0:
		errs = append(errs, fmt.Errorf("otlp: BatchSize is %d, which is negative; leave it at zero for the default", o.BatchSize))
	case cfg.batchSize > cfg.queueSize:
		errs = append(errs, fmt.Errorf("otlp: BatchSize is %d but QueueSize is %d, so a batch could never fill; make BatchSize at most QueueSize", cfg.batchSize, cfg.queueSize))
	}
	if cfg.retryInitial > cfg.retryMax {
		errs = append(errs, fmt.Errorf("otlp: RetryInitialInterval is %v, longer than RetryMaxInterval of %v", cfg.retryInitial, cfg.retryMax))
	}
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// orDefault returns v, or fallback when v is zero.
func orDefault[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

// parseEndpoint checks the collector's base URL and returns the URL spans are
// posted to.
//
// No error quotes the endpoint as it was given, because one that carries
// credentials is exactly the case to refuse without repeating them.
func parseEndpoint(endpoint string) (string, error) {
	if endpoint == "" {
		return "", errors.New("otlp: Endpoint is empty; set it to the collector's OTLP/HTTP address, such as http://localhost:4318")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("otlp: Endpoint is not a URL; write it as http://host:port or https://host:port")
	}
	if u.User != nil {
		return "", errors.New("otlp: Endpoint carries credentials, which would be repeated in logs and error messages; remove them from the URL and send them in Headers")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("otlp: Endpoint %q is not an http or https URL", u.Redacted())
	}
	if u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("otlp: Endpoint %q has no host", u.Redacted())
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("otlp: Endpoint %q has a query or a fragment, which the path /v1/traces cannot follow", u.Redacted())
	}
	return strings.TrimSuffix(u.String(), "/") + "/v1/traces", nil
}

// buildHeader checks the configured headers and builds the set every request
// carries. An error names the header and never quotes its value.
func buildHeader(headers Headers, gzip bool) (http.Header, []string, error) {
	header := http.Header{"Content-Type": {"application/json"}}
	if gzip {
		header.Set("Content-Encoding", "gzip")
	}
	var errs []error
	names := make([]string, 0, len(headers))
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		canonical := http.CanonicalHeaderKey(name)
		switch {
		case !validHeaderName(name):
			errs = append(errs, fmt.Errorf("otlp: header name %q is not a valid HTTP header name", name))
		case reservedHeaders[canonical]:
			errs = append(errs, fmt.Errorf("otlp: header %s is set by the exporter or belongs to the connection, and cannot be configured", canonical))
		case !validHeaderValue(headers[name]):
			errs = append(errs, fmt.Errorf("otlp: the value of header %s holds a line break or another control character; the value is not shown", canonical))
		case header[canonical] != nil:
			errs = append(errs, fmt.Errorf("otlp: header %s is configured more than once, in different spellings", canonical))
		default:
			header.Set(canonical, headers[name])
			names = append(names, canonical)
		}
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}
	return header, names, nil
}

// validHeaderName reports whether name is an HTTP token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

// validHeaderValue reports whether value can be written as a header value
// without ending the header early: no control character but a tab, and no
// DEL. A CR or LF would let a configured value add headers of its own, or a
// request.
func validHeaderValue(value string) bool {
	for i := range len(value) {
		if c := value[i]; (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
