package otlp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	muzak "muzak.dev/framework"
)

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-value"
	cases := []struct {
		name string
		opts Options
		want []string
	}{
		{name: "no endpoint", opts: Options{}, want: []string{"Endpoint is empty"}},
		{name: "not a URL", opts: Options{Endpoint: "http://[::1"}, want: []string{"is not a URL"}},
		{name: "another scheme", opts: Options{Endpoint: "ftp://collector:4318"}, want: []string{"not an http or https URL"}},
		{name: "a relative URL", opts: Options{Endpoint: "collector:4318/x"}, want: []string{"not an http or https URL"}},
		{name: "no host", opts: Options{Endpoint: "http:///v1"}, want: []string{"has no host"}},
		{name: "credentials", opts: Options{Endpoint: "https://user:" + secret + "@collector"}, want: []string{"carries credentials"}},
		{name: "a query", opts: Options{Endpoint: "https://collector/?token=x"}, want: []string{"query or a fragment"}},
		{name: "a fragment", opts: Options{Endpoint: "https://collector/#x"}, want: []string{"query or a fragment"}},
		{name: "a header name with a space", opts: Options{Endpoint: "http://c", Headers: Headers{"Bad Name": secret}}, want: []string{`header name "Bad Name"`}},
		{name: "an empty header name", opts: Options{Endpoint: "http://c", Headers: Headers{"": secret}}, want: []string{`header name ""`}},
		{name: "a header value with CRLF", opts: Options{Endpoint: "http://c", Headers: Headers{"X-Key": secret + "\r\nX-Evil: 1"}}, want: []string{"header X-Key holds a line break"}},
		{name: "a header value with NUL", opts: Options{Endpoint: "http://c", Headers: Headers{"X-Key": secret + "\x00"}}, want: []string{"header X-Key holds"}},
		{name: "a header value with DEL", opts: Options{Endpoint: "http://c", Headers: Headers{"X-Key": secret + "\x7f"}}, want: []string{"header X-Key holds"}},
		{name: "a reserved header", opts: Options{Endpoint: "http://c", Headers: Headers{"content-type": "text/plain"}}, want: []string{"Content-Type is set by the exporter"}},
		{name: "a hop-by-hop header", opts: Options{Endpoint: "http://c", Headers: Headers{"Connection": "close"}}, want: []string{"Connection is set by the exporter or belongs to the connection"}},
		{name: "a header twice", opts: Options{Endpoint: "http://c", Headers: Headers{"X-Key": secret, "x-key": secret}}, want: []string{"X-Key is configured more than once"}},
		{name: "negative durations", opts: Options{Endpoint: "http://c", Timeout: -1, BatchInterval: -1, RetryInitialInterval: -1, RetryMaxInterval: -1, RetryMaxElapsedTime: -1},
			want: []string{"Timeout is -1ns", "BatchInterval is", "RetryInitialInterval is -1ns", "RetryMaxInterval is -1ns", "RetryMaxElapsedTime is"}},
		{name: "a negative queue", opts: Options{Endpoint: "http://c", QueueSize: -1}, want: []string{"QueueSize is -1"}},
		{name: "a huge queue", opts: Options{Endpoint: "http://c", QueueSize: MaxQueueSize + 1}, want: []string{"QueueSize is 1048577"}},
		{name: "a negative batch", opts: Options{Endpoint: "http://c", BatchSize: -1}, want: []string{"BatchSize is -1"}},
		{name: "a batch larger than the queue", opts: Options{Endpoint: "http://c", QueueSize: 10, BatchSize: 11}, want: []string{"BatchSize is 11 but QueueSize is 10"}},
		{name: "a first retry longer than the cap", opts: Options{Endpoint: "http://c", RetryInitialInterval: time.Minute, RetryMaxInterval: time.Second}, want: []string{"RetryInitialInterval is 1m0s, longer than"}},
		{name: "every problem at once", opts: Options{Endpoint: "ftp://c", QueueSize: -1, Headers: Headers{"Host": "x"}},
			want: []string{"not an http or https URL", "QueueSize is -1", "Host is set by the exporter"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := New(tc.opts)
			if err == nil || e != nil {
				t.Fatalf("New = %v, %v; want a refusal", e, err)
			}
			message := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(message, want) {
					t.Errorf("the error does not say %q:\n%s", want, message)
				}
			}
			if strings.Contains(message, secret) {
				t.Errorf("the error repeats a secret:\n%s", message)
			}
			for _, line := range strings.Split(message, "\n") {
				if !strings.HasPrefix(line, "otlp: ") {
					t.Errorf("an error line does not start with the package prefix: %q", line)
				}
			}
		})
	}
}

func TestNewAcceptsAndDefaults(t *testing.T) {
	t.Parallel()
	e, err := New(Options{Endpoint: "https://collector.example.com:4318/otlp/", Headers: Headers{"x-api-key": "k"}})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	cfg := e.cfg
	if cfg.url != "https://collector.example.com:4318/otlp/v1/traces" {
		t.Errorf("url = %q", cfg.url)
	}
	if cfg.timeout != DefaultTimeout || cfg.queueSize != DefaultQueueSize || cfg.batchSize != DefaultBatchSize ||
		cfg.batchInterval != DefaultBatchInterval || cfg.retryInitial != DefaultRetryInitialInterval ||
		cfg.retryMax != DefaultRetryMaxInterval || cfg.retryElapsed != DefaultRetryMaxElapsedTime {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if !strings.HasPrefix(cfg.serviceName, "unknown_service:") {
		t.Errorf("service name = %q", cfg.serviceName)
	}
	if cfg.header.Get("X-Api-Key") != "k" || cfg.header.Get("Content-Type") != "application/json" || cfg.header.Get("Content-Encoding") != "" {
		t.Errorf("header = %v", cfg.header)
	}
	if cfg.logger != slog.Default() {
		t.Error("the logger does not default to slog.Default")
	}
	if e.Name() != "otlp" {
		t.Errorf("Name = %q", e.Name())
	}
}

// TestHeaderValuesAreNeverPrinted checks every way a configuration or an
// exporter is commonly printed or logged.
func TestHeaderValuesAreNeverPrinted(t *testing.T) {
	t.Parallel()
	const secret = "Bearer s3cret-token"
	opts := Options{Endpoint: "http://collector:4318", Headers: Headers{"Authorization": secret, "X-Tenant": "acme"}}
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	var logged syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("config", "options", opts, "headers", opts.Headers, "exporter", e)
	printed := []string{
		fmt.Sprintf("%v", opts), fmt.Sprintf("%+v", opts), fmt.Sprintf("%#v", opts),
		fmt.Sprint(opts.Headers), fmt.Sprintf("%#v", opts.Headers), fmt.Sprint(e), e.String(),
		logged.String(),
	}
	for _, out := range printed {
		if strings.Contains(out, "s3cret") {
			t.Errorf("a header value was printed: %s", out)
		}
	}
	if !strings.Contains(logged.String(), "Authorization") || !strings.Contains(fmt.Sprint(opts.Headers), "X-Tenant: [redacted]") {
		t.Errorf("the header names are missing, which a reader needs: %s", logged.String())
	}
}

// TestWireFormat checks a request against the OTLP/HTTP JSON encoding field by
// field.
func TestWireFormat(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	opts.ServiceVersion = "1.2.3"
	opts.ResourceAttributes = []slog.Attr{slog.String("deployment.environment.name", "test"), slog.String("service.name", "ignored")}
	opts.Headers = Headers{"X-Api-Key": "key"}
	e := startExporter(t, opts)

	parent := muzak.SpanContext{TraceID: muzak.TraceID{0xab, 0x01}, SpanID: muzak.SpanID{0xef, 0x02}, TraceFlags: muzak.TraceFlagsSampled, Remote: true}
	start := time.Unix(1_700_000_000, 123_456_789)
	server := e.StartSpan(context.Background(), muzak.SpanStart{
		Name: "GET",
		Kind: muzak.SpanKindServer,
		SpanContext: muzak.SpanContext{
			TraceID: parent.TraceID, SpanID: muzak.SpanID{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0},
			TraceFlags: muzak.TraceFlagsSampled, TraceState: "rojo=1,congo=2",
		},
		Parent:     parent,
		StartTime:  start,
		Attributes: []slog.Attr{slog.String("http.request.method", "GET")},
	})
	server.SetName("GET /items/{id}")
	server.SetAttributes(
		slog.Int("http.response.status_code", 500),
		slog.Uint64("big", math.MaxUint64),
		slog.Uint64("small", 7),
		slog.Float64("ratio", 0.5),
		slog.Float64("nan", math.NaN()),
		slog.Float64("inf", math.Inf(-1)),
		slog.Bool("ok", true),
		slog.Duration("took", 1500*time.Millisecond),
		slog.Time("at", start),
		slog.Group("db", slog.String("system", "postgresql"), slog.Int("rows", 3)),
		slog.Any("tags", []string{"a", "b"}),
		slog.Any("mixed", []any{1, "x", true}),
		slog.Any("raw", []byte("hi")),
		slog.Any("err", errors.New("broken")),
		slog.Any("nothing", nil),
		slog.Any("struct", struct{ A int }{A: 1}),
		slog.Any("invalid", "bad\xffutf8"),
	)
	server.AddEvent("exception", slog.String("exception.type", "*errors.errorString"), slog.String("exception.message", "broken"))
	server.SetStatus(muzak.SpanStatusError, "it failed")
	server.End()

	child := e.StartSpan(context.Background(), spanStart(9, "child"))
	child.SetStatus(muzak.SpanStatusOK, "ignored for OK")
	child.End()
	flush(t, e)

	requests := c.all()
	if len(requests) != 1 {
		t.Fatalf("received %d requests, want one batch", len(requests))
	}
	request := requests[0]
	if request.method != http.MethodPost || request.path != "/v1/traces" {
		t.Errorf("request = %s %s", request.method, request.path)
	}
	if got := request.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := request.header.Get("X-Api-Key"); got != "key" {
		t.Errorf("X-Api-Key = %q", got)
	}

	resourceSpans := list(request.doc["resourceSpans"])
	if len(resourceSpans) != 1 {
		t.Fatalf("resourceSpans = %v", request.doc["resourceSpans"])
	}
	resource := attributes(object(object(resourceSpans[0])["resource"])["attributes"])
	for key, want := range map[string]string{
		"service.name": "test-service", "service.version": "1.2.3", "deployment.environment.name": "test",
		"telemetry.sdk.name": "muzak", "telemetry.sdk.language": "go",
	} {
		if got := resource[key]["stringValue"]; got != want {
			t.Errorf("resource %s = %v, want %q", key, got, want)
		}
	}
	scopeSpans := list(object(resourceSpans[0])["scopeSpans"])
	if got := object(object(scopeSpans[0])["scope"])["name"]; got != "muzak.dev/framework" {
		t.Errorf("scope name = %v", got)
	}

	spans := spansOf(request.doc)
	if len(spans) != 2 {
		t.Fatalf("received %d spans, want 2", len(spans))
	}
	s := spans[0]
	for key, want := range map[string]any{
		"traceId":           "ab010000000000000000000000000000",
		"spanId":            "123456789abcdef0",
		"parentSpanId":      "ef02000000000000",
		"traceState":        "rojo=1,congo=2",
		"name":              "GET /items/{id}",
		"kind":              float64(2),
		"flags":             float64(0x301),
		"startTimeUnixNano": "1700000000123456789",
	} {
		if got := s[key]; got != want {
			t.Errorf("span %s = %#v, want %#v", key, got, want)
		}
	}
	end, err := strconv.ParseInt(s["endTimeUnixNano"].(string), 10, 64)
	if err != nil || end < start.UnixNano() {
		t.Errorf("endTimeUnixNano = %v, want a decimal string after the start", s["endTimeUnixNano"])
	}
	attrs := attributes(s["attributes"])
	checks := map[string]map[string]any{
		"http.request.method":       {"stringValue": "GET"},
		"http.response.status_code": {"intValue": "500"},
		"big":                       {"stringValue": "18446744073709551615"},
		"small":                     {"intValue": "7"},
		"ratio":                     {"doubleValue": 0.5},
		"nan":                       {"doubleValue": "NaN"},
		"inf":                       {"doubleValue": "-Infinity"},
		"ok":                        {"boolValue": true},
		"took":                      {"intValue": "1500000000"},
		"at":                        {"stringValue": start.Format(time.RFC3339Nano)},
		"raw":                       {"bytesValue": base64.StdEncoding.EncodeToString([]byte("hi"))},
		"err":                       {"stringValue": "broken"},
		"struct":                    {"stringValue": "{1}"},
		"invalid":                   {"stringValue": "bad\xef\xbf\xbdutf8"},
		"nothing":                   {},
	}
	for key, want := range checks {
		got := attrs[key]
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("attribute %s = %v, want %v", key, got, want)
		}
	}
	db := attributes(object(attrs["db"]["kvlistValue"])["values"])
	if db["system"]["stringValue"] != "postgresql" || db["rows"]["intValue"] != "3" {
		t.Errorf("group = %v", attrs["db"])
	}
	tags := list(object(attrs["tags"]["arrayValue"])["values"])
	if len(tags) != 2 || object(tags[1])["stringValue"] != "b" {
		t.Errorf("array = %v", attrs["tags"])
	}
	mixed := list(object(attrs["mixed"]["arrayValue"])["values"])
	if len(mixed) != 3 || object(mixed[0])["intValue"] != "1" || object(mixed[2])["boolValue"] != true {
		t.Errorf("mixed array = %v", attrs["mixed"])
	}
	events := list(s["events"])
	if len(events) != 1 {
		t.Fatalf("events = %v", s["events"])
	}
	ev := object(events[0])
	if ev["name"] != "exception" || attributes(ev["attributes"])["exception.message"]["stringValue"] != "broken" {
		t.Errorf("event = %v", ev)
	}
	if _, err := strconv.ParseInt(fmt.Sprint(ev["timeUnixNano"]), 10, 64); err != nil {
		t.Errorf("event time = %#v, want a decimal string", ev["timeUnixNano"])
	}
	if status := object(s["status"]); status["code"] != float64(2) || status["message"] != "it failed" {
		t.Errorf("status = %v", s["status"])
	}

	c2 := spans[1]
	for _, absent := range []string{"parentSpanId", "traceState", "attributes", "events", "droppedAttributesCount"} {
		if _, ok := c2[absent]; ok {
			t.Errorf("the child carries %s, which holds its default and should be left out", absent)
		}
	}
	if c2["flags"] != float64(0x101) || c2["kind"] != float64(1) {
		t.Errorf("child flags = %v kind = %v", c2["flags"], c2["kind"])
	}
	if status := object(c2["status"]); status["code"] != float64(1) || status["message"] != nil {
		t.Errorf("child status = %v, want OK without a message", c2["status"])
	}
}

func TestBatchingBySize(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	opts.BatchSize = 3
	e := startExporter(t, opts)
	endSpans(e, 7)
	waitFor(t, func() bool { return len(c.all()) == 2 }, "two full batches")
	time.Sleep(20 * time.Millisecond)
	if n := len(c.all()); n != 2 {
		t.Fatalf("sent %d requests before the third batch filled, want 2", n)
	}
	for _, request := range c.all() {
		if n := len(spansOf(request.doc)); n != 3 {
			t.Errorf("a batch holds %d spans, want 3", n)
		}
	}
	flush(t, e)
	if requests := c.all(); len(requests) != 3 || len(spansOf(requests[2].doc)) != 1 {
		t.Errorf("after the flush: %d requests", len(requests))
	}
	if stats := e.Stats(); stats.Exported != 7 || stats.Dropped != 0 || stats.Failed != 0 || stats.Queued != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestBatchingByInterval(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	opts.BatchInterval = 30 * time.Millisecond
	e := startExporter(t, opts)
	began := time.Now()
	endSpans(e, 2)
	waitFor(t, func() bool { return len(c.all()) == 1 }, "the interval to send a partial batch")
	if took := time.Since(began); took < 25*time.Millisecond {
		t.Errorf("the partial batch went after %v, before the interval", took)
	}
	if n := len(c.spans()); n != 2 {
		t.Errorf("sent %d spans, want 2", n)
	}
}

// TestQueueOverflow checks that ending a span never waits, and that what does
// not fit is dropped, counted and reported.
func TestQueueOverflow(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, logs := testOptions(c.server.URL)
	opts.QueueSize = 4
	opts.BatchSize = 2
	opts.BatchInterval = 10 * time.Millisecond
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	endSpans(e, 10)
	if took := time.Since(began); took > time.Second {
		t.Errorf("ending spans into a full queue took %v", took)
	}
	if stats := e.Stats(); stats.Dropped != 6 || stats.Queued != 4 {
		t.Fatalf("stats = %+v, want 6 dropped and 4 queued", stats)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	flush(t, e)
	if n := len(c.spans()); n != 4 {
		t.Errorf("sent %d spans, want the 4 that fit", n)
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), `"dropped":6`) }, "the drops to be reported")
	time.Sleep(30 * time.Millisecond)
	if n := strings.Count(logs.String(), "spans were dropped"); n != 1 {
		t.Errorf("the drops were reported %d times, want once", n)
	}
}

// statusResponder answers each request with the next status in the list, and
// with 200 once the list runs out.
func statusResponder(statuses []int, header http.Header) func(int, http.ResponseWriter, *http.Request) {
	return func(n int, w http.ResponseWriter, _ *http.Request) {
		for name, values := range header {
			w.Header()[name] = values
		}
		if n < len(statuses) {
			w.WriteHeader(statuses[n])
			_, _ = io.WriteString(w, "refused by the test")
			return
		}
		_, _ = io.WriteString(w, "{}")
	}
}

func TestRetryWithBackoff(t *testing.T) {
	t.Parallel()
	for _, status := range []int{429, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			c := newCollector(t, statusResponder([]int{status, status}, nil))
			opts, _ := testOptions(c.server.URL)
			opts.RetryInitialInterval = 20 * time.Millisecond
			opts.RetryMaxInterval = 100 * time.Millisecond
			e := startExporter(t, opts)
			endSpans(e, 3)
			flush(t, e)
			requests := c.all()
			if len(requests) != 3 {
				t.Fatalf("sent %d requests, want two refusals and a success", len(requests))
			}
			first, second := requests[1].arrived.Sub(requests[0].arrived), requests[2].arrived.Sub(requests[1].arrived)
			if first < 10*time.Millisecond || second < 20*time.Millisecond {
				t.Errorf("waited %v then %v, want at least half of 20ms and of 40ms", first, second)
			}
			if stats := e.Stats(); stats.Exported != 3 || stats.Failed != 0 {
				t.Errorf("stats = %+v", stats)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	// The cap is 50ms where the header asks for longer, so a wait near it is
	// the cap applied. Where the header asks for none, the backoff of 50ms,
	// at least half of it fixed, is waited all the same.
	cases := []struct {
		name, header string
		limit        time.Duration
		atLeast      time.Duration
		atMost       time.Duration
	}{
		{name: "seconds past the cap are capped", header: "3600", limit: 50 * time.Millisecond, atLeast: 40 * time.Millisecond, atMost: time.Second},
		{name: "zero keeps the backoff", header: "0", limit: 50 * time.Millisecond, atLeast: 20 * time.Millisecond, atMost: time.Second},
		{name: "a date in the past keeps the backoff", header: "Wed, 21 Oct 2015 07:28:00 GMT", limit: 50 * time.Millisecond, atLeast: 20 * time.Millisecond, atMost: time.Second},
		{name: "a date far ahead is capped", header: time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), limit: 50 * time.Millisecond, atLeast: 40 * time.Millisecond, atMost: time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCollector(t, statusResponder([]int{429}, http.Header{"Retry-After": {tc.header}}))
			opts, _ := testOptions(c.server.URL)
			opts.RetryInitialInterval = tc.limit
			opts.RetryMaxInterval = tc.limit
			e := startExporter(t, opts)
			endSpans(e, 1)
			flush(t, e)
			requests := c.all()
			if len(requests) != 2 {
				t.Fatalf("sent %d requests", len(requests))
			}
			if gap := requests[1].arrived.Sub(requests[0].arrived); gap < tc.atLeast || gap > tc.atMost {
				t.Errorf("waited %v, want between %v and %v", gap, tc.atLeast, tc.atMost)
			}
		})
	}
}

// TestRetryAfterCannotShortenTheBackoff checks that a collector, or anything in
// front of it, answering every export with a Retry-After of zero or of a date
// gone by cannot turn the retries into a loop without pause: the batch would
// be sent again as fast as the answer came back, for all of
// RetryMaxElapsedTime, against a collector that just said it was overloaded.
func TestRetryAfterCannotShortenTheBackoff(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"0", "Wed, 21 Oct 2015 07:28:00 GMT"} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			c := newCollector(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", header)
				w.WriteHeader(http.StatusTooManyRequests)
			})
			opts, _ := testOptions(c.server.URL)
			opts.RetryInitialInterval = 20 * time.Millisecond
			opts.RetryMaxInterval = 20 * time.Millisecond
			opts.RetryMaxElapsedTime = 200 * time.Millisecond
			e := startExporter(t, opts)
			endSpans(e, 1)
			flush(t, e)
			// Every wait is at least half of 20ms, so 200ms leaves room for
			// at most 20 retries after the first attempt.
			if sent := len(c.all()); sent > 21 {
				t.Errorf("sent %d requests in 200ms, want the backoff to space them out", sent)
			}
			if stats := e.Stats(); stats.Failed != 1 {
				t.Errorf("stats = %+v, want the batch given up on", stats)
			}
		})
	}
}

// TestNoRetryOnOtherRefusals checks that every answer but the four retried
// ones is final, and that the refusal is logged without the headers.
func TestNoRetryOnOtherRefusals(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 401, 403, 404, 413, 500, 501} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			c := newCollector(t, statusResponder([]int{status, status, status}, nil))
			opts, logs := testOptions(c.server.URL)
			opts.Headers = Headers{"Authorization": "Bearer s3cret"}
			e := startExporter(t, opts)
			endSpans(e, 2)
			flush(t, e)
			if n := len(c.all()); n != 1 {
				t.Errorf("sent %d requests, want one", n)
			}
			if stats := e.Stats(); stats.Failed != 2 || stats.Exported != 0 {
				t.Errorf("stats = %+v", stats)
			}
			out := logs.String()
			if !strings.Contains(out, fmt.Sprintf(`"status":%d`, status)) || !strings.Contains(out, "refused by the test") {
				t.Errorf("the refusal was not logged with its status and body: %s", out)
			}
			if strings.Contains(out, "s3cret") {
				t.Errorf("a header value reached the log: %s", out)
			}
		})
	}
}

// TestRedirectIsNotFollowed checks that a collector cannot send the headers,
// credentials included, somewhere else.
func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	var hits sync.Map
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Store(r.Header.Get("Authorization"), true)
	}))
	t.Cleanup(elsewhere.Close)
	c := newCollector(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/traces", http.StatusTemporaryRedirect)
	})
	opts, _ := testOptions(c.server.URL)
	opts.Headers = Headers{"Authorization": "Bearer s3cret"}
	e := startExporter(t, opts)
	endSpans(e, 1)
	flush(t, e)
	hits.Range(func(key, _ any) bool {
		t.Errorf("the redirect was followed, carrying %q", key)
		return true
	})
	if stats := e.Stats(); stats.Failed != 1 {
		t.Errorf("stats = %+v, want the redirect counted as a refusal", stats)
	}
}

func TestRetryGivesUp(t *testing.T) {
	t.Parallel()
	c := newCollector(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	opts, logs := testOptions(c.server.URL)
	opts.RetryInitialInterval = 5 * time.Millisecond
	opts.RetryMaxInterval = 10 * time.Millisecond
	opts.RetryMaxElapsedTime = 300 * time.Millisecond
	e := startExporter(t, opts)
	endSpans(e, 2)
	began := time.Now()
	flush(t, e)
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("gave up after %v, want about 300ms", took)
	}
	// At least one wait of 5 to 10ms fits in 300ms, and at most 300 / 2.5
	// of them do.
	if n := len(c.all()); n < 2 || n > 121 {
		t.Errorf("sent %d requests in 300ms of retrying", n)
	}
	if stats := e.Stats(); stats.Failed != 2 {
		t.Errorf("stats = %+v", stats)
	}
	if !strings.Contains(logs.String(), "time allowed for retrying") {
		t.Errorf("logs = %s", logs.String())
	}
}

// TestUnreachableCollector checks that a refused connection is retried and
// then given up on, never blocking a span.
func TestUnreachableCollector(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	opts, logs := testOptions(closed.URL)
	opts.RetryMaxElapsedTime = 50 * time.Millisecond
	e := startExporter(t, opts)
	endSpans(e, 1)
	flush(t, e)
	if stats := e.Stats(); stats.Failed != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if !strings.Contains(logs.String(), "connection refused") && !strings.Contains(logs.String(), "connect") {
		t.Errorf("the failure was not logged: %s", logs.String())
	}
}

// TestHangingCollector checks that an attempt the collector never answers is
// abandoned at Timeout and retried.
func TestHangingCollector(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := newCollector(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	opts, _ := testOptions(c.server.URL)
	opts.Timeout = 50 * time.Millisecond
	e := startExporter(t, opts)
	endSpans(e, 1)
	began := time.Now()
	flush(t, e)
	if took := time.Since(began); took > time.Second {
		t.Errorf("the hung attempt held the exporter for %v", took)
	}
	// At least two requests: on a machine loaded enough, as under -race with
	// -count, the retry can outlast the 50ms Timeout as well and need a third.
	if stats := e.Stats(); stats.Exported != 1 || len(c.all()) < 2 {
		t.Errorf("stats = %+v after %d requests", stats, len(c.all()))
	}
}

// TestHugeResponseBody checks that a collector answering with far more than
// an answer needs is read no further than the bound.
func TestHugeResponseBody(t *testing.T) {
	t.Parallel()
	for _, status := range []int{200, 400} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			var written int64
			var mu sync.Mutex
			c := newCollector(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				chunk := []byte(strings.Repeat("x", 32<<10))
				for range 1 << 10 {
					n, err := w.Write(chunk)
					mu.Lock()
					written += int64(n)
					mu.Unlock()
					if err != nil {
						return
					}
				}
			})
			opts, logs := testOptions(c.server.URL)
			e := startExporter(t, opts)
			endSpans(e, 1)
			began := time.Now()
			flush(t, e)
			if took := time.Since(began); took > 2*time.Second {
				t.Errorf("the answer held the exporter for %v", took)
			}
			stats := e.Stats()
			if status == 200 && stats.Exported != 1 || status == 400 && stats.Failed != 1 {
				t.Errorf("stats = %+v", stats)
			}
			for _, line := range strings.Split(logs.String(), "\n") {
				if len(line) > 2048 {
					t.Errorf("a log line of %d bytes quotes the answer", len(line))
				}
			}
		})
	}
}

func TestPartialSuccess(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body               string
		exported, rejected uint64
	}{
		{body: `{"partialSuccess":{"rejectedSpans":"2","errorMessage":"two spans had no name"}}`, exported: 3, rejected: 2},
		{body: `{"partialSuccess":{"rejectedSpans":2}}`, exported: 3, rejected: 2},
		{body: `{"partialSuccess":{"rejectedSpans":"999"}}`, exported: 0, rejected: 5},
		{body: `{"partialSuccess":{"rejectedSpans":"-3"}}`, exported: 5},
		{body: `{"partialSuccess":{}}`, exported: 5},
		{body: `not json`, exported: 5},
		{body: ``, exported: 5},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			t.Parallel()
			c := newCollector(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			opts, logs := testOptions(c.server.URL)
			e := startExporter(t, opts)
			endSpans(e, 5)
			flush(t, e)
			if stats := e.Stats(); stats.Exported != tc.exported || stats.Failed != tc.rejected {
				t.Errorf("stats = %+v, want %d exported and %d rejected", stats, tc.exported, tc.rejected)
			}
			if strings.Contains(tc.body, "no name") && !strings.Contains(logs.String(), "two spans had no name") {
				t.Errorf("the collector's message was not logged: %s", logs.String())
			}
		})
	}
}

func TestGzip(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	opts.Gzip = true
	e := startExporter(t, opts)
	endSpans(e, 4)
	flush(t, e)
	requests := c.all()
	if len(requests) != 1 || requests[0].header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("requests = %d, Content-Encoding = %q", len(requests), requests[0].header.Get("Content-Encoding"))
	}
	if n := len(spansOf(requests[0].doc)); n != 4 {
		t.Errorf("the decompressed body holds %d spans", n)
	}
}

// TestSpanLimits checks every bound a span enforces.
func TestSpanLimits(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	e := startExporter(t, opts)

	many := e.StartSpan(context.Background(), spanStart(0, "many"))
	for i := range 200 {
		many.SetAttributes(slog.Int(fmt.Sprintf("k%03d", i), i))
	}
	many.SetAttributes(slog.Int("k000", -1), slog.String("", "no key"))
	for i := range 150 {
		many.AddEvent(fmt.Sprintf("e%d", i))
	}
	many.End()

	big := e.StartSpan(context.Background(), spanStart(1, strings.Repeat("n", 5000)))
	big.SetAttributes(slog.String("long", strings.Repeat("v", 10_000)+"\xe2\x82\xac"))
	for i := range 40 {
		big.SetAttributes(slog.String(fmt.Sprintf("blob%d", i), strings.Repeat("b", 4000)))
	}
	big.AddEvent("too much", slog.String("x", strings.Repeat("y", 4000)))
	deep := slog.Group("l1", slog.Group("l2", slog.Group("l3", slog.Group("l4", slog.Group("l5", slog.Int("x", 1))))))
	big.SetAttributes(deep)
	big.End()

	wide := e.StartSpan(context.Background(), spanStart(2, "wide"))
	wide.SetAttributes(slog.Any("list", make([]int, 500)))
	wide.End()
	wide.End()
	wide.SetName("after the end")
	wide.SetAttributes(slog.Int("after", 1))
	wide.AddEvent("after")
	wide.SetStatus(muzak.SpanStatusError, "after")
	flush(t, e)

	spans := c.spans()
	if len(spans) != 3 {
		t.Fatalf("received %d spans, want 3 (a span ended twice is sent once)", len(spans))
	}
	m := spans[0]
	attrs := attributes(m["attributes"])
	if len(attrs) != maxAttributes || m["droppedAttributesCount"] != float64(200-maxAttributes) {
		t.Errorf("attributes = %d dropped = %v", len(attrs), m["droppedAttributesCount"])
	}
	if attrs["k000"]["intValue"] != "-1" {
		t.Errorf("a repeated key was not replaced: %v", attrs["k000"])
	}
	if len(list(m["events"])) != maxEvents || m["droppedEventsCount"] != float64(150-maxEvents) {
		t.Errorf("events = %d dropped = %v", len(list(m["events"])), m["droppedEventsCount"])
	}

	b := spans[1]
	if name, _ := b["name"].(string); len(name) != maxStringLength {
		t.Errorf("a 5000 byte name was sent as %d bytes", len(name))
	}
	battrs := attributes(b["attributes"])
	if long, _ := battrs["long"]["stringValue"].(string); len(long) != maxStringLength {
		t.Errorf("a long value was sent as %d bytes", len(long))
	}
	total := 0
	for key, v := range battrs {
		s, _ := v["stringValue"].(string)
		total += len(key) + len(s)
	}
	if total > maxSpanBytes || b["droppedAttributesCount"] == nil {
		t.Errorf("the span holds %d bytes of attributes with %v dropped, want it held to %d", total, b["droppedAttributesCount"], maxSpanBytes)
	}

	w := spans[2]
	if w["name"] != "wide" || len(list(object(attributes(w["attributes"])["list"]["arrayValue"])["values"])) != maxListLength {
		t.Errorf("wide span = %v", w)
	}
	if _, ok := attributes(w["attributes"])["after"]; ok || w["status"] != nil {
		t.Error("a change made after End reached the collector")
	}
}

// TestDeepValues checks that nesting is cut off where the bound says.
func TestDeepValues(t *testing.T) {
	t.Parallel()
	deep := slog.Group("l1", slog.Group("l2", slog.Group("l3", slog.Group("l4", slog.Group("l5", slog.Int("x", 1))))))
	kv, _ := convertAttr(deep, 0)
	v := kv.value
	for range maxValueDepth {
		if v.kind != kindKVList || len(v.kvs) != 1 {
			t.Fatalf("value = %+v", v)
		}
		v = v.kvs[0].value
	}
	if v.kind != kindString || v.str != "[nested too deeply]" {
		t.Errorf("the value past the bound is %+v", v)
	}
	nested := []any{[]any{[]any{[]any{[]any{1}}}}}
	kv, _ = convertAttr(slog.Any("a", nested), 0)
	v = kv.value
	for v.kind == kindArray {
		v = v.list[0]
	}
	if v.kind != kindString {
		t.Errorf("a deep array ended in %+v", v)
	}
}

func TestSetStatusSemantics(t *testing.T) {
	t.Parallel()
	e, err := New(Options{Endpoint: "http://c"})
	if err != nil {
		t.Fatal(err)
	}
	s := newSpan(e, spanStart(0, "x"))
	s.SetStatus(muzak.SpanStatusUnset, "ignored")
	s.SetStatus(muzak.SpanStatusCode(9), "ignored")
	if s.status != muzak.SpanStatusUnset {
		t.Errorf("status = %d", s.status)
	}
	s.SetStatus(muzak.SpanStatusError, "first")
	s.SetStatus(muzak.SpanStatusError, "second")
	if s.status != muzak.SpanStatusError || s.statusMessage != "second" {
		t.Errorf("status = %d %q", s.status, s.statusMessage)
	}
	s.SetStatus(muzak.SpanStatusOK, "dropped")
	s.SetStatus(muzak.SpanStatusError, "too late")
	if s.status != muzak.SpanStatusOK || s.statusMessage != "" {
		t.Errorf("status = %d %q, want OK to be final and carry no message", s.status, s.statusMessage)
	}
}

func TestSpanKindOutOfRange(t *testing.T) {
	t.Parallel()
	for kind, want := range map[muzak.SpanKind]muzak.SpanKind{0: 0, 1: 1, 5: 5, 6: 0, -1: 0} {
		if got := spanKind(kind); got != want {
			t.Errorf("spanKind(%d) = %d, want %d", kind, got, want)
		}
	}
}

func TestLifecycle(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Flush before Start = %v", err)
	}
	ctx := context.Background()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := e.current
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if e.current != first {
		t.Error("starting a running exporter started a second worker")
	}
	endSpans(e, 2)
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("a second Stop = %v", err)
	}
	if n := len(c.spans()); n != 2 {
		t.Errorf("Stop flushed %d spans, want 2", n)
	}
	endSpans(e, 3)
	if stats := e.Stats(); stats.Dropped != 3 {
		t.Errorf("spans ended after Stop: stats = %+v, want them dropped", stats)
	}
	if err := e.Flush(ctx); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Flush after Stop = %v", err)
	}
	// Started again, it exports again.
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	endSpans(e, 1)
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(c.spans()); n != 3 {
		t.Errorf("after a restart the collector has %d spans, want 3", n)
	}
	// Stopping an exporter that was never started drops what it queued.
	idle, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	endSpans(idle, 4)
	if err := idle.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := idle.Stats(); stats.Dropped != 4 || stats.Queued != 0 {
		t.Errorf("stats = %+v", stats)
	}
}

// TestFlushHonoursItsContext checks that a flush stuck behind a collector
// returns when its caller gives up.
func TestFlushHonoursItsContext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	c := newCollector(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	opts, _ := testOptions(c.server.URL)
	e := startExporter(t, opts)
	endSpans(e, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := e.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Flush = %v, want the deadline", err)
	}
}

func TestConcurrentSpans(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	opts.QueueSize = 10_000
	opts.BatchSize = 100
	e := startExporter(t, opts)
	var wg sync.WaitGroup
	for g := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				s := e.StartSpan(context.Background(), spanStart(g*100+i, "work"))
				s.SetAttributes(slog.Int("i", i))
				s.AddEvent("e")
				s.End()
			}
		}()
	}
	wg.Wait()
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := e.Stats()
	if stats.Exported+stats.Dropped != 2000 || stats.Exported != uint64(len(c.spans())) {
		t.Errorf("stats = %+v with %d received, want every span accounted for", stats, len(c.spans()))
	}
}

func TestRetryWaitBounds(t *testing.T) {
	t.Parallel()
	e, err := New(Options{Endpoint: "http://c", RetryInitialInterval: 100 * time.Millisecond, RetryMaxInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for tries := range 70 {
		full := min(100*time.Millisecond<<min(tries, 20), time.Second)
		for range 20 {
			wait := e.retryWait(tries, attempt{retry: true})
			if wait < full/2 || wait > full {
				t.Fatalf("try %d waited %v, want between %v and %v", tries, wait, full/2, full)
			}
		}
	}
	if wait := e.retryWait(0, attempt{hasRetryAfter: true, retryAfter: time.Hour}); wait != time.Second {
		t.Errorf("a Retry-After of an hour gave %v, want the cap", wait)
	}
	for range 20 {
		if wait := e.retryWait(5, attempt{hasRetryAfter: true}); wait < 500*time.Millisecond || wait > time.Second {
			t.Fatalf("a Retry-After of zero gave %v, want the backoff of the sixth try", wait)
		}
		if wait := e.retryWait(0, attempt{hasRetryAfter: true, retryAfter: 700 * time.Millisecond}); wait < 700*time.Millisecond || wait > time.Second {
			t.Fatalf("a Retry-After of 700ms on the first try gave %v, want it obeyed", wait)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, true},
		{" 5 ", 5 * time.Second, true},
		{"0", 0, true},
		{"99999999999999999999999", math.MaxInt64, true},
		{"9223372037", math.MaxInt64, true},
		{"1.5", 0, false},
		{"-1", 0, false},
		{"soon", 0, false},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.header, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

// TestStopFlushesAndLeavesNothingRunning is not parallel, so that the
// goroutines it counts are its own.
func TestStopFlushesAndLeavesNothingRunning(t *testing.T) {
	c := newCollector(t, nil)
	baseline := runtime.NumGoroutine()
	opts, _ := testOptions(c.server.URL)
	opts.BatchSize = 2
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	endSpans(e, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if n := len(c.spans()); n != 5 {
		t.Errorf("Stop flushed %d spans, want 5", n)
	}
	assertNoExporterGoroutines(t)
	waitFor(t, func() bool { return runtime.NumGoroutine() <= baseline }, "the connections' goroutines to exit")
}

// TestStopWithAnExpiredContext checks that Stop keeps its deadline against a
// collector that never answers, and still leaves nothing running.
func TestStopWithAnExpiredContext(t *testing.T) {
	release := make(chan struct{})
	c := newCollector(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	opts, _ := testOptions(c.server.URL)
	opts.BatchSize = 2
	opts.Timeout = time.Hour
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	endSpans(e, 6)
	waitFor(t, func() bool { return len(c.all()) == 1 }, "the first batch to reach the hanging collector")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = e.Stop(ctx)
	if took := time.Since(began); took > time.Second {
		t.Errorf("Stop took %v with a 50ms deadline", took)
	}
	if err == nil || !strings.Contains(err.Error(), "could not be sent") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Stop = %v, want the spans lost to the deadline reported", err)
	}
	stats := e.Stats()
	if stats.Exported != 0 || stats.Dropped+stats.Failed != 6 {
		t.Errorf("stats = %+v, want all six accounted for as not sent", stats)
	}
	assertNoExporterGoroutines(t)

	// A context that has already ended sends nothing at all.
	quick, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := quick.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	endSpans(quick, 1)
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if err := quick.Stop(done); !errors.Is(err, context.Canceled) {
		t.Errorf("Stop = %v", err)
	}
	if stats := quick.Stats(); stats.Dropped != 1 {
		t.Errorf("stats = %+v", stats)
	}
	assertNoExporterGoroutines(t)
}

// TestStopDuringARetryWait checks that a retry the collector asked to be made
// later than Stop's deadline is abandoned at once rather than waited for.
func TestStopDuringARetryWait(t *testing.T) {
	c := newCollector(t, statusResponder([]int{503, 503, 503}, http.Header{"Retry-After": {"30"}}))
	opts, _ := testOptions(c.server.URL)
	opts.RetryMaxInterval = 10 * time.Second
	opts.RetryInitialInterval = time.Second
	opts.RetryMaxElapsedTime = time.Minute
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	endSpans(e, 1)
	go func() { _ = e.Flush(context.Background()) }()
	waitFor(t, func() bool { return len(c.all()) == 1 }, "the first refusal")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	_ = e.Stop(ctx)
	if took := time.Since(began); took > 150*time.Millisecond {
		t.Errorf("Stop waited %v for a retry its deadline could not reach", took)
	}
	if stats := e.Stats(); stats.Failed != 1 {
		t.Errorf("stats = %+v", stats)
	}
	assertNoExporterGoroutines(t)

	// Without a deadline, Stop lets the wait run and the retry go out.
	c2 := newCollector(t, statusResponder([]int{503}, http.Header{"Retry-After": {"0"}}))
	opts2, _ := testOptions(c2.server.URL)
	e2, err := New(opts2)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	endSpans(e2, 1)
	if err := e2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := e2.Stats(); stats.Exported != 1 || len(c2.all()) != 2 {
		t.Errorf("stats = %+v after %d requests", stats, len(c2.all()))
	}
}
