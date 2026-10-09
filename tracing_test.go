package muzak

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingTracer keeps every span it is asked to start, so that a test can
// read what the framework told it.
type recordingTracer struct {
	mu    sync.Mutex
	spans []*recordedSpan
	// panicOnStart makes StartSpan panic, and panicOnEnd makes End panic,
	// for the tests of a Tracer that misbehaves. nilSpan makes StartSpan
	// return nil.
	panicOnStart, panicOnEnd, nilSpan bool
}

// recordedSpan is one span as the recording tracer saw it.
type recordedSpan struct {
	tracer *recordingTracer

	mu          sync.Mutex
	start       SpanStart
	name        string
	attrs       map[string]slog.Value
	events      []recordedEvent
	status      SpanStatusCode
	description string
	ends        int
}

// recordedEvent is one event added to a recorded span.
type recordedEvent struct {
	name  string
	attrs map[string]slog.Value
}

func (t *recordingTracer) StartSpan(_ context.Context, start SpanStart) Span {
	if t.panicOnStart {
		panic("the tracer is broken")
	}
	if t.nilSpan {
		return nil
	}
	span := &recordedSpan{tracer: t, start: start, name: start.Name, attrs: map[string]slog.Value{}}
	for _, a := range start.Attributes {
		span.attrs[a.Key] = a.Value
	}
	t.mu.Lock()
	t.spans = append(t.spans, span)
	t.mu.Unlock()
	return span
}

func (s *recordedSpan) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

func (s *recordedSpan) SetAttributes(attrs ...slog.Attr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range attrs {
		s.attrs[a.Key] = a.Value
	}
}

func (s *recordedSpan) AddEvent(name string, attrs ...slog.Attr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	event := recordedEvent{name: name, attrs: map[string]slog.Value{}}
	for _, a := range attrs {
		event.attrs[a.Key] = a.Value
	}
	s.events = append(s.events, event)
}

func (s *recordedSpan) SetStatus(code SpanStatusCode, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.description = code, description
}

func (s *recordedSpan) End() {
	if s.tracer.panicOnEnd {
		panic("the span is broken")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ends++
}

// ended reports how many times End was called.
func (s *recordedSpan) ended() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ends
}

// attr returns an attribute's value as a string, and whether it was set.
func (s *recordedSpan) attr(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.attrs[key]
	if !ok {
		return "", false
	}
	return v.String(), true
}

// all returns a copy of the spans recorded so far.
func (t *recordingTracer) all() []*recordedSpan {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*recordedSpan(nil), t.spans...)
}

// only returns the single span recorded, failing the test if there is not
// exactly one.
func (t *recordingTracer) only(tb testing.TB) *recordedSpan {
	tb.Helper()
	spans := t.all()
	if len(spans) != 1 {
		tb.Fatalf("recorded %d spans, want 1", len(spans))
	}
	return spans[0]
}

// tracedOptions returns quiet options with a recording tracer.
func tracedOptions() (AppOptions, *recordingTracer) {
	tracer := &recordingTracer{}
	opts := quietOptions()
	opts.Tracing = TracingOptions{Tracer: tracer}
	return opts, tracer
}

// itemIn is the input of the routed test route.
type itemIn struct {
	ID string `path:"id"`
}

func TestServerSpanForARoutedRequest(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	app := New(opts)
	app.Get("/items/{id}", func(ctx *Context, in itemIn) (itemOut, error) {
		if span := SpanFromContext(ctx.Context()); span != tracer.all()[0] {
			t.Errorf("SpanFromContext returned %v, not the server span", span)
		}
		return itemOut{Name: in.ID}, nil
	})
	rec := do(t, mustBuild(t, app), "GET", "/items/42")
	assertStatus(t, rec, http.StatusOK)

	span := tracer.only(t)
	if span.name != "GET /items/{id}" {
		t.Errorf("name = %q, want the method and the template, never the path", span.name)
	}
	if span.start.Kind != SpanKindServer || span.start.Parent.IsValid() {
		t.Errorf("kind = %d parent = %+v, want a server span starting a trace", span.start.Kind, span.start.Parent)
	}
	if !span.start.SpanContext.IsValid() || !span.start.SpanContext.IsSampled() || span.start.SpanContext.Remote {
		t.Errorf("span context = %+v, want a valid, sampled, local one", span.start.SpanContext)
	}
	if span.start.StartTime.IsZero() {
		t.Error("the span has no start time")
	}
	want := map[string]string{
		"http.request.method":       "GET",
		"http.route":                "/items/{id}",
		"http.response.status_code": "200",
		"url.scheme":                "http",
		"server.address":            "example.com",
		"network.protocol.version":  "1.1",
	}
	for key, value := range want {
		if got, ok := span.attr(key); !ok || got != value {
			t.Errorf("%s = %q (set %v), want %q", key, got, ok, value)
		}
	}
	for _, absent := range []string{"user_agent.original", "error.type", "url.path", "url.full"} {
		if got, ok := span.attr(absent); ok {
			t.Errorf("%s = %q, want it left off", absent, got)
		}
	}
	if span.status != SpanStatusUnset || span.ended() != 1 || len(span.events) != 0 {
		t.Errorf("status = %d ends = %d events = %d, want unset, once, none", span.status, span.ended(), len(span.events))
	}
	if rec.Header().Get("traceparent") != "" {
		t.Error("the response carries a traceparent, which would tell any client the server's trace identifiers")
	}
}

func TestServerSpanForUnroutedRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, method, target string
		status               int
		spanName, method2    string
	}{
		{name: "no route", method: "GET", target: "/nowhere", status: 404, spanName: "GET", method2: "GET"},
		{name: "a method the path has no route for", method: "DELETE", target: "/items/1", status: 405, spanName: "DELETE", method2: "DELETE"},
		{name: "an OPTIONS answered from the route table", method: "OPTIONS", target: "/items/1", status: 204, spanName: "OPTIONS", method2: "OPTIONS"},
		{name: "a method nobody defines", method: "BREW", target: "/items/1", status: 405, spanName: "HTTP", method2: "_OTHER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, tracer := tracedOptions()
			app := New(opts)
			app.Get("/items/{id}", func(ctx *Context, in itemIn) (itemOut, error) { return itemOut{}, nil })
			rec := do(t, mustBuild(t, app), tc.method, tc.target)
			assertStatus(t, rec, tc.status)
			span := tracer.only(t)
			if span.name != tc.spanName {
				t.Errorf("name = %q, want %q", span.name, tc.spanName)
			}
			if got, _ := span.attr("http.request.method"); got != tc.method2 {
				t.Errorf("http.request.method = %q, want %q", got, tc.method2)
			}
			if got, ok := span.attr("http.route"); ok {
				t.Errorf("http.route = %q on a request no route answered", got)
			}
			if got, _ := span.attr("http.response.status_code"); got != fmt.Sprint(tc.status) {
				t.Errorf("status code = %s, want %d", got, tc.status)
			}
			if span.status != SpanStatusUnset {
				t.Errorf("status = %d, want a 4xx left unset as the conventions ask", span.status)
			}
		})
	}
}

// TestServerSpanForACustomMethod checks that a method a route was registered
// for is recorded as itself, since the routing table bounds it.
func TestServerSpanForACustomMethod(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	app := New(opts)
	app.Handle("PURGE", "/cache", func(ctx *Context, _ Empty) (itemOut, error) { return itemOut{}, nil })
	assertStatus(t, do(t, mustBuild(t, app), "PURGE", "/cache"), http.StatusOK)
	span := tracer.only(t)
	if got, _ := span.attr("http.request.method"); span.name != "PURGE /cache" || got != "PURGE" {
		t.Errorf("name = %q method = %q, want the registered method", span.name, got)
	}
}

func TestServerSpanRecordsFailures(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	logger, logs := captureLogger(t)
	opts.Logger = logger
	app := New(opts)
	app.Get("/opaque", func(*Context, Empty) (itemOut, error) {
		return itemOut{}, errors.New("database exploded at 10.0.0.7")
	})
	app.Get("/wrapped", func(*Context, Empty) (itemOut, error) {
		return itemOut{}, ServiceUnavailable("try later").Wrap(errors.New("pool exhausted"))
	})
	app.Get("/client", func(*Context, Empty) (itemOut, error) {
		return itemOut{}, NotFound("no such item")
	})
	app.Get("/panic", func(*Context, Empty) (itemOut, error) {
		panic("boom with a secret in it")
	})
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/opaque"), 500)
	assertStatus(t, do(t, app, "GET", "/wrapped"), 503)
	assertStatus(t, do(t, app, "GET", "/client"), 404)
	assertStatus(t, do(t, app, "GET", "/panic"), 500)
	spans := tracer.all()
	if len(spans) != 4 {
		t.Fatalf("recorded %d spans, want 4", len(spans))
	}
	opaque, wrapped, client, panicked := spans[0], spans[1], spans[2], spans[3]

	assertException := func(span *recordedSpan, wantType, wantMessage string) {
		t.Helper()
		if len(span.events) != 1 || span.events[0].name != "exception" {
			t.Fatalf("%s: events = %+v, want one exception", span.name, span.events)
		}
		event := span.events[0]
		if got := event.attrs["exception.type"].String(); got != wantType {
			t.Errorf("%s: exception.type = %q, want %q", span.name, got, wantType)
		}
		if got := event.attrs["exception.message"].String(); got != wantMessage {
			t.Errorf("%s: exception.message = %q, want %q", span.name, got, wantMessage)
		}
		if len(event.attrs) != 2 {
			t.Errorf("%s: the exception carries %d attributes, want only its type and message", span.name, len(event.attrs))
		}
		// The span holds nothing the log does not already hold.
		if !strings.Contains(logs.String(), wantMessage) {
			t.Errorf("%s: the span's message %q is not in the log", span.name, wantMessage)
		}
	}
	assertException(opaque, "*errors.errorString", "database exploded at 10.0.0.7")
	assertException(wrapped, "*errors.errorString", "pool exhausted")
	assertException(panicked, "panic", errPanic.Error())

	for _, span := range []*recordedSpan{opaque, wrapped, panicked} {
		if span.status != SpanStatusError || span.description != "" {
			t.Errorf("%s: status = %d %q, want an error described by its status code alone", span.name, span.status, span.description)
		}
	}
	if got, _ := opaque.attr("error.type"); got != "*errors.errorString" {
		t.Errorf("error.type = %q, want the cause's type", got)
	}
	if got, _ := panicked.attr("error.type"); got != "panic" {
		t.Errorf("error.type = %q for a panic", got)
	}
	if client.status != SpanStatusUnset || len(client.events) != 0 {
		t.Errorf("a 404 the handler chose has status %d and %d events, want neither", client.status, len(client.events))
	}
	if _, ok := client.attr("error.type"); ok {
		t.Error("a 404 the handler chose carries an error.type")
	}
	for _, span := range spans {
		for key, value := range span.attrs {
			if strings.Contains(value.String(), "secret") {
				t.Errorf("%s: attribute %s carries the panic value", span.name, key)
			}
		}
		for _, event := range span.events {
			for key, value := range event.attrs {
				if strings.Contains(value.String(), "secret") {
					t.Errorf("%s: event attribute %s carries the panic value", span.name, key)
				}
			}
		}
	}
}

// TestServerSpanForAnAbortedResponse checks that a response which started and
// then failed is an error on its span, whatever status it was sent with.
func TestServerSpanForAnAbortedResponse(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	app := New(opts)
	app.Get("/export", func(ctx *Context, _ Empty) (itemOut, error) {
		_, _ = ctx.ResponseWriter().Write([]byte("row 1\n"))
		return itemOut{}, errors.New("the cursor broke")
	})
	mustBuild(t, app)
	if recovered := catchPanic(func() { do(t, app, "GET", "/export") }); recovered != http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared by identity
		t.Fatalf("recovered %v, want the abort sentinel", recovered)
	}
	span := tracer.only(t)
	if span.status != SpanStatusError || span.description != abortedSpanDescription {
		t.Errorf("status = %d %q, want an error saying it was aborted", span.status, span.description)
	}
	if got, _ := span.attr("http.response.status_code"); got != "200" {
		t.Errorf("status code = %s, want the 200 already sent", got)
	}
	if got, _ := span.attr("error.type"); got != "*errors.errorString" {
		t.Errorf("error.type = %q", got)
	}
	if span.ended() != 1 {
		t.Errorf("ended %d times", span.ended())
	}
}

// TestServerSpanErrorTypes checks the error.type of a failure that left no
// cause to name: the status code for a 5xx, and _OTHER for an abort that
// nothing logged. It also checks that a deliberate 4xx returned after the
// response started, which aborts the connection and is logged for it, is
// recorded too.
func TestServerSpanErrorTypes(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	app := New(opts)
	app.Get("/busy", func(*Context, Empty) (itemOut, error) { return itemOut{}, ServiceUnavailable("busy") })
	app.Get("/abort-early", func(*Context, Empty) (itemOut, error) { panic(http.ErrAbortHandler) })
	app.Get("/abort-late", func(ctx *Context, _ Empty) (itemOut, error) {
		_, _ = ctx.ResponseWriter().Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	})
	app.Get("/late-404", func(ctx *Context, _ Empty) (itemOut, error) {
		_, _ = ctx.ResponseWriter().Write([]byte("partial"))
		return itemOut{}, NotFound("it went away")
	})
	mustBuild(t, app)
	assertStatus(t, do(t, app, "GET", "/busy"), http.StatusServiceUnavailable)
	for _, path := range []string{"/abort-early", "/abort-late", "/late-404"} {
		if recovered := catchPanic(func() { do(t, app, "GET", path) }); recovered != http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared by identity
			t.Fatalf("%s recovered %v", path, recovered)
		}
	}
	spans := tracer.all()
	if len(spans) != 4 {
		t.Fatalf("recorded %d spans", len(spans))
	}
	want := []struct{ errorType, status, description string }{
		{"503", "503", ""},
		{"500", "500", ""},
		{"_OTHER", "200", abortedSpanDescription},
		{"*muzak.HTTPError", "200", abortedSpanDescription},
	}
	for i, w := range want {
		span := spans[i]
		errorType, _ := span.attr("error.type")
		status, _ := span.attr("http.response.status_code")
		if errorType != w.errorType || status != w.status || span.status != SpanStatusError || span.description != w.description {
			t.Errorf("%s: error.type = %q status = %s span status = %d %q, want %q %s %q",
				span.name, errorType, status, span.status, span.description, w.errorType, w.status, w.description)
		}
	}
	if late := spans[3]; len(late.events) != 1 || late.events[0].attrs["exception.message"].String() != NotFound("it went away").Error() {
		t.Errorf("the late 404 was not recorded as the log records it: %+v", late.events)
	}
}

func TestServerSpanForAnEventStream(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	handlerSawOpenSpan := make(chan bool, 2)
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.SSE("/stream", func(ctx *Context, _ Empty, stream *SSEStream[itemOut]) error {
			for _, name := range []string{"a", "b"} {
				if err := stream.Send(itemOut{Name: name}); err != nil {
					return err
				}
			}
			spans := tracer.all()
			handlerSawOpenSpan <- len(spans) == 1 && spans[0].ended() == 0
			return nil
		})
		app.SSE("/broken", func(ctx *Context, _ Empty, stream *SSEStream[itemOut]) error {
			_ = stream.Send(itemOut{Name: "a"})
			return errors.New("the feed went away")
		})
	})
	reader := openStream(t, server.URL, "/stream")
	nextEvent(t, reader)
	nextEvent(t, reader)
	assertStreamEnded(t, reader)
	if !<-handlerSawOpenSpan {
		t.Fatal("the span ended while the stream was still open")
	}
	waitFor(t, func() bool { return tracer.only(t).ended() == 1 }, "the stream's span to end")
	span := tracer.only(t)
	if span.name != "GET /stream" || span.status != SpanStatusUnset {
		t.Errorf("name = %q status = %d", span.name, span.status)
	}

	broken := openStream(t, server.URL, "/broken")
	nextEvent(t, broken)
	assertStreamEnded(t, broken)
	waitFor(t, func() bool { return len(tracer.all()) == 2 && tracer.all()[1].ended() == 1 }, "the broken stream's span to end")
	failed := tracer.all()[1]
	if failed.status != SpanStatusError || failed.description != failedSpanDescription {
		t.Errorf("status = %d %q, want the handler's failure", failed.status, failed.description)
	}
	if len(failed.events) != 1 || failed.events[0].attrs["exception.message"].String() != "the feed went away" {
		t.Errorf("events = %+v", failed.events)
	}
}

func TestServerSpanForAWebSocket(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	handlerSawOpenSpan := make(chan bool, 1)
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.WS("/ws", func(ctx *Context, in Empty, conn *WSConn) error {
			message, err := conn.ReadText(ctx.Context())
			if err != nil {
				return nil
			}
			spans := tracer.all()
			handlerSawOpenSpan <- spans[len(spans)-1].ended() == 0
			if message == "fail" {
				return errors.New("the handler broke")
			}
			return conn.WriteText(ctx.Context(), message)
		})
	})
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	conn := dialClient(t, url, WSDialOptions{})
	if err := conn.WriteText(t.Context(), "hello"); err != nil {
		t.Fatalf("WriteText = %v", err)
	}
	if got, err := conn.ReadText(t.Context()); err != nil || got != "hello" {
		t.Fatalf("ReadText = %q, %v", got, err)
	}
	if !<-handlerSawOpenSpan {
		t.Fatal("the span ended while the connection was still open")
	}
	waitFor(t, func() bool { return tracer.only(t).ended() == 1 }, "the connection's span to end")
	span := tracer.only(t)
	if got, _ := span.attr("http.response.status_code"); span.name != "GET /ws" || got != "101" || span.status != SpanStatusUnset {
		t.Errorf("name = %q status code = %s status = %d", span.name, got, span.status)
	}

	failing := dialClient(t, url, WSDialOptions{})
	if err := failing.WriteText(t.Context(), "fail"); err != nil {
		t.Fatalf("WriteText = %v", err)
	}
	<-handlerSawOpenSpan
	waitFor(t, func() bool { return len(tracer.all()) == 2 && tracer.all()[1].ended() == 1 }, "the failed connection's span to end")
	failed := tracer.all()[1]
	if failed.status != SpanStatusError || failed.description != failedSpanDescription {
		t.Errorf("status = %d %q, want the handler's failure", failed.status, failed.description)
	}
	if got, _ := failed.attr("error.type"); got != "*errors.errorString" {
		t.Errorf("error.type = %q", got)
	}
}

// TestServerSpanForAHijackedConnection checks that a handler which takes the
// connection over gets a span that ends when it returns, with the status the
// framework records for a hijack.
func TestServerSpanForAHijackedConnection(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.Get("/raw", func(ctx *Context, _ Empty) (itemOut, error) {
			conn, brw, err := http.NewResponseController(ctx.ResponseWriter()).Hijack()
			if err != nil {
				return itemOut{}, err
			}
			defer conn.Close()
			_, _ = brw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			_ = brw.Flush()
			return itemOut{}, nil
		})
	})
	status, body, err := fetchOverTheWire(t, server.URL+"/raw")
	if err != nil || status != 200 || body != "ok" {
		t.Fatalf("GET /raw = %d %q %v", status, body, err)
	}
	waitFor(t, func() bool { return len(tracer.all()) == 1 && tracer.all()[0].ended() == 1 }, "the span to end")
	if got, _ := tracer.only(t).attr("http.response.status_code"); got != "101" {
		t.Errorf("status code = %s, want the 101 a hijack is recorded as", got)
	}
}

func TestIncomingTraceContext(t *testing.T) {
	t.Parallel()
	request := func(t *testing.T, opts AppOptions, remote string, headers ...string) (SpanContext, http.Header) {
		t.Helper()
		var seen SpanContext
		outbound := http.Header{}
		app := New(opts)
		app.Get("/x", func(ctx *Context, _ Empty) (itemOut, error) {
			seen, _ = SpanContextFromContext(ctx.Context())
			InjectTraceContext(ctx.Context(), outbound)
			return itemOut{}, nil
		})
		req := httptest.NewRequest("GET", "/x", nil)
		req.RemoteAddr = remote
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Add(headers[i], headers[i+1])
		}
		assertStatus(t, doRequest(t, mustBuild(t, app), req), http.StatusOK)
		return seen, outbound
	}
	inbound, _ := parseTraceparent(testParent)

	t.Run("a valid traceparent is continued", func(t *testing.T) {
		t.Parallel()
		opts, tracer := tracedOptions()
		seen, outbound := request(t, opts, "203.0.113.9:4000", "traceparent", testParent, "tracestate", "rojo=1, congo=2")
		span := tracer.only(t)
		if seen.TraceID != inbound.TraceID || seen.SpanID == inbound.SpanID || !seen.IsSampled() {
			t.Errorf("span context = %+v, want the inbound trace with a span of its own", seen)
		}
		wantParent := inbound
		wantParent.TraceState = "rojo=1,congo=2"
		if span.start.Parent != wantParent || !span.start.Parent.Remote {
			t.Errorf("parent = %+v, want the inbound span marked remote, with its state", span.start.Parent)
		}
		if seen.TraceState != "rojo=1,congo=2" {
			t.Errorf("tracestate = %q, want it carried on by the span, normalized", seen.TraceState)
		}
		want := "00-" + testTraceIDHex + "-" + seen.SpanID.String() + "-01"
		if got := outbound.Get("traceparent"); got != want {
			t.Errorf("outbound traceparent = %q, want %q", got, want)
		}
		if got := outbound.Get("tracestate"); got != "rojo=1,congo=2" {
			t.Errorf("outbound tracestate = %q", got)
		}
	})
	t.Run("an unsampled trace is propagated but not recorded", func(t *testing.T) {
		t.Parallel()
		opts, tracer := tracedOptions()
		seen, outbound := request(t, opts, "203.0.113.9:4000", "traceparent", "00-"+testTraceIDHex+"-"+testSpanIDHex+"-00")
		if len(tracer.all()) != 0 {
			t.Errorf("recorded %d spans of a trace the caller is not sampling", len(tracer.all()))
		}
		if seen.TraceID != inbound.TraceID || seen.IsSampled() || !seen.SpanID.IsValid() {
			t.Errorf("span context = %+v, want the inbound trace, unsampled, with a span id", seen)
		}
		if got := outbound.Get("traceparent"); !strings.HasSuffix(got, "-00") || !strings.Contains(got, testTraceIDHex) {
			t.Errorf("outbound traceparent = %q, want the decision passed on", got)
		}
	})
	t.Run("unknown flags are not passed on", func(t *testing.T) {
		t.Parallel()
		opts, _ := tracedOptions()
		seen, _ := request(t, opts, "203.0.113.9:4000", "traceparent", "00-"+testTraceIDHex+"-"+testSpanIDHex+"-ff")
		if seen.TraceFlags != TraceFlagsSampled {
			t.Errorf("flags = %02x, want only the sampled flag", byte(seen.TraceFlags))
		}
	})
	t.Run("an invalid traceparent starts a new trace", func(t *testing.T) {
		t.Parallel()
		opts, tracer := tracedOptions()
		seen, _ := request(t, opts, "203.0.113.9:4000", "traceparent", strings.ToUpper(testParent), "tracestate", "rojo=1")
		if seen.TraceID == inbound.TraceID || tracer.only(t).start.Parent.IsValid() || seen.TraceState != "" {
			t.Errorf("span context = %+v, want a new trace with no state", seen)
		}
	})
	policies := []struct {
		name      string
		policy    TraceParentPolicy
		remote    string
		continued bool
	}{
		{name: "trusted proxies, from a trusted proxy", policy: TraceParentFromTrustedProxies, remote: "192.0.2.10:4000", continued: true},
		{name: "trusted proxies, from anyone else", policy: TraceParentFromTrustedProxies, remote: "203.0.113.9:4000"},
		{name: "trusted proxies, from an unparseable peer", policy: TraceParentFromTrustedProxies, remote: "@unix"},
		{name: "ignore, even from a trusted proxy", policy: TraceParentIgnore, remote: "192.0.2.10:4000"},
	}
	for _, tc := range policies {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, tracer := tracedOptions()
			opts.Tracing.Parent = tc.policy
			opts.ClientIP = ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}}
			seen, _ := request(t, opts, tc.remote, "traceparent", testParent, "tracestate", "rojo=1")
			if continued := seen.TraceID == inbound.TraceID; continued != tc.continued {
				t.Fatalf("continued = %v, want %v", continued, tc.continued)
			}
			if !tc.continued && (tracer.only(t).start.Parent.IsValid() || seen.TraceState != "") {
				t.Errorf("a refused parent left %+v and state %q behind", tracer.only(t).start.Parent, seen.TraceState)
			}
		})
	}
}

func TestSampler(t *testing.T) {
	t.Parallel()
	t.Run("an unsampled new trace", func(t *testing.T) {
		t.Parallel()
		opts, tracer := tracedOptions()
		var asked []TraceID
		var mu sync.Mutex
		opts.Tracing.Sampler = func(r *http.Request, trace TraceID) bool {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, trace)
			return r.URL.Path == "/sampled"
		}
		var seen SpanContext
		app := New(opts)
		handler := func(ctx *Context, _ Empty) (itemOut, error) {
			seen, _ = SpanContextFromContext(ctx.Context())
			return itemOut{}, nil
		}
		app.Get("/sampled", handler)
		app.Get("/unsampled", handler)
		mustBuild(t, app)
		do(t, app, "GET", "/unsampled")
		if len(tracer.all()) != 0 || seen.IsSampled() || !seen.IsValid() {
			t.Errorf("spans = %d, span context = %+v, want nothing recorded and ids still drawn", len(tracer.all()), seen)
		}
		do(t, app, "GET", "/sampled")
		if len(tracer.all()) != 1 || !seen.IsSampled() {
			t.Errorf("spans = %d, span context = %+v", len(tracer.all()), seen)
		}
		if len(asked) != 2 || asked[1] != seen.TraceID {
			t.Errorf("the sampler was asked about %v, want the two new traces", asked)
		}
		// A continued trace keeps its caller's decision, and the sampler is
		// not asked.
		req := httptest.NewRequest("GET", "/unsampled", nil)
		req.Header.Set("traceparent", testParent)
		doRequest(t, app, req)
		if len(tracer.all()) != 2 || len(asked) != 2 {
			t.Errorf("spans = %d, sampler asked %d times, want 2 and 2", len(tracer.all()), len(asked))
		}
	})
	t.Run("a panicking sampler", func(t *testing.T) {
		t.Parallel()
		opts, tracer := tracedOptions()
		logger, logs := captureLogger(t)
		opts.Logger = logger
		opts.Tracing.Sampler = func(*http.Request, TraceID) bool { panic("sampler bug") }
		app := New(opts)
		app.Get("/x", okHandler)
		assertStatus(t, do(t, mustBuild(t, app), "GET", "/x"), http.StatusOK)
		if len(tracer.all()) != 0 || !strings.Contains(logs.String(), "the tracing sampler panicked") {
			t.Errorf("spans = %d, logs = %s", len(tracer.all()), logs.String())
		}
	})
}

func TestSampleRatio(t *testing.T) {
	t.Parallel()
	const draws = 20_000
	count := func(sampler Sampler) int {
		n := 0
		for i := range draws {
			var id TraceID
			binary.BigEndian.PutUint64(id[8:], splitmix64(uint64(i)))
			if sampler(nil, id) {
				n++
			}
		}
		return n
	}
	if got := count(SampleRatio(0)); got != 0 {
		t.Errorf("ratio 0 sampled %d", got)
	}
	if got := count(SampleRatio(-1)); got != 0 {
		t.Errorf("ratio -1 sampled %d", got)
	}
	if got := count(SampleRatio(float64Nan())); got != 0 {
		t.Errorf("ratio NaN sampled %d", got)
	}
	if got := count(SampleRatio(1)); got != draws {
		t.Errorf("ratio 1 sampled %d of %d", got, draws)
	}
	if got := count(SampleRatio(0.25)); got < draws/5 || got > draws*3/10 {
		t.Errorf("ratio 0.25 sampled %d of %d", got, draws)
	}
	// The decision depends on the trace alone, so every service agrees.
	sampler, id := SampleRatio(0.5), newTraceID()
	first := sampler(nil, id)
	for range 100 {
		if sampler(nil, id) != first {
			t.Fatal("the same trace was decided two ways")
		}
	}
}

// splitmix64 spreads a counter over all 64 bits, so that the ratio test
// draws identifiers whose bits vary the way random ones do, deterministically.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

func float64Nan() float64 {
	zero := 0.0
	return zero / zero
}

func TestStartSpanStartsChildren(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	app := New(opts)
	var server, child SpanContext
	outbound := http.Header{}
	app.Get("/x", func(ctx *Context, _ Empty) (itemOut, error) {
		server, _ = SpanContextFromContext(ctx.Context())
		childCtx, span := StartSpan(ctx.Context(), "charge card", SpanKindClient, slog.String("payment.provider", "stripe"))
		defer span.End()
		child, _ = SpanContextFromContext(childCtx)
		InjectTraceContext(childCtx, outbound)
		if SpanFromContext(childCtx) != span {
			t.Error("SpanFromContext does not return the child")
		}
		_, internal := StartSpan(childCtx, "step", 0)
		internal.End()
		return itemOut{}, nil
	})
	do(t, mustBuild(t, app), "GET", "/x")
	spans := tracer.all()
	if len(spans) != 3 {
		t.Fatalf("recorded %d spans, want the server span and two children", len(spans))
	}
	charge, step := spans[1], spans[2]
	if charge.start.Name != "charge card" || charge.start.Kind != SpanKindClient || charge.start.Parent != server {
		t.Errorf("child = %+v, want a client span whose parent is the server span", charge.start)
	}
	if child.TraceID != server.TraceID || child.SpanID == server.SpanID || !child.IsSampled() {
		t.Errorf("child = %+v, server = %+v", child, server)
	}
	if got, _ := charge.attr("payment.provider"); got != "stripe" {
		t.Errorf("payment.provider = %q", got)
	}
	if step.start.Kind != SpanKindInternal || step.start.Parent != child {
		t.Errorf("grandchild = %+v, want an internal span under the child", step.start)
	}
	if got := outbound.Get("traceparent"); !strings.Contains(got, child.SpanID.String()) {
		t.Errorf("outbound traceparent = %q, want the child as the parent", got)
	}
	if charge.ended() != 1 || step.ended() != 1 {
		t.Error("a child span was not ended exactly once")
	}
}

// TestStartSpanWithoutTracing checks that code written to trace costs nothing
// where tracing is off: no allocation, no identifier, the same context back.
func TestStartSpanWithoutTracing(t *testing.T) {
	ctx := context.Background()
	got, span := StartSpan(ctx, "work", SpanKindInternal)
	if got != ctx {
		t.Error("StartSpan returned a new context with no span to parent it")
	}
	span.SetName("x")
	span.SetAttributes(slog.Int("n", 1))
	span.AddEvent("e")
	span.SetStatus(SpanStatusError, "x")
	span.End()
	if SpanFromContext(ctx) != (noopSpan{}) {
		t.Error("SpanFromContext returned something other than the no-op span")
	}
	allocs := testing.AllocsPerRun(100, func() {
		_, s := StartSpan(ctx, "work", SpanKindInternal)
		s.End()
	})
	if allocs != 0 {
		t.Errorf("StartSpan with tracing off allocates %v times", allocs)
	}
}

// TestUnsampledChildIsNotRecorded checks that a child inherits its trace's
// decision.
func TestUnsampledChildIsNotRecorded(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	opts.Tracing.Sampler = func(*http.Request, TraceID) bool { return false }
	app := New(opts)
	var child SpanContext
	app.Get("/x", func(ctx *Context, _ Empty) (itemOut, error) {
		childCtx, span := StartSpan(ctx.Context(), "work", SpanKindInternal)
		span.End()
		child, _ = SpanContextFromContext(childCtx)
		return itemOut{}, nil
	})
	do(t, mustBuild(t, app), "GET", "/x")
	if len(tracer.all()) != 0 || !child.IsValid() || child.IsSampled() {
		t.Errorf("spans = %d child = %+v, want an unrecorded child with ids", len(tracer.all()), child)
	}
}

func TestUserAgentIsRecordedOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	send := func(t *testing.T, record bool, agent string) (string, bool) {
		t.Helper()
		opts, tracer := tracedOptions()
		opts.Tracing.RecordUserAgent = record
		app := New(opts)
		app.Get("/x", okHandler)
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("User-Agent", agent)
		doRequest(t, mustBuild(t, app), req)
		return tracer.only(t).attr("user_agent.original")
	}
	if got, ok := send(t, false, "curl/8.0"); ok {
		t.Errorf("user_agent.original = %q with the option off", got)
	}
	if got, _ := send(t, true, "curl/8.0"); got != "curl/8.0" {
		t.Errorf("user_agent.original = %q", got)
	}
	long, _ := send(t, true, strings.Repeat("A", 10_000))
	if len(long) > maxQuotedLength+len(truncatedMarker) || !strings.HasSuffix(long, truncatedMarker) {
		t.Errorf("a 10000 byte user agent was recorded as %d bytes", len(long))
	}
	if got, _ := send(t, true, "bad\xffagent"); got != "bad\uFFFDagent" {
		t.Errorf("an agent that is not UTF-8 was recorded as %q", got)
	}
}

func TestServerAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		host string
		name string
		port int
		ok   bool
	}{
		{host: "example.com", name: "example.com", ok: true},
		{host: "API.Example.com:8443", name: "api.example.com", port: 8443, ok: true},
		{host: "127.0.0.1:80", name: "127.0.0.1", port: 80, ok: true},
		{host: "[::1]:8080", name: "::1", port: 8080, ok: true},
		{host: "[::1]", name: "::1", ok: true},
		{host: "[fe80::1%25eth0]:80", name: "fe80::1", port: 80, ok: true},
		{host: ""},
		{host: "example.com:0"},
		{host: "example.com:65536"},
		{host: "example.com:http"},
		{host: "evil<script>.com"},
		{host: "a b"},
		{host: "caf\u00e9.com"},
		{host: strings.Repeat("a", 254)},
		{host: strings.Repeat("a", 253), name: strings.Repeat("a", 253), ok: true},
	}
	for _, tc := range cases {
		name, port, ok := serverAddress(tc.host)
		if name != tc.name || port != tc.port || ok != tc.ok {
			t.Errorf("serverAddress(%q) = %q, %d, %v; want %q, %d, %v", tc.host, name, port, ok, tc.name, tc.port, tc.ok)
		}
	}
}

func TestProtocolAndScheme(t *testing.T) {
	t.Parallel()
	cases := []struct {
		major, minor int
		want         string
	}{{1, 0, "1.0"}, {1, 1, "1.1"}, {2, 0, "2"}, {3, 0, "3"}, {0, 9, ""}}
	for _, tc := range cases {
		if got := protocolVersion(&http.Request{ProtoMajor: tc.major, ProtoMinor: tc.minor}); got != tc.want {
			t.Errorf("protocolVersion(%d.%d) = %q, want %q", tc.major, tc.minor, got, tc.want)
		}
	}
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	if got := requestScheme(req); got != "https" {
		t.Errorf("scheme over TLS = %q", got)
	}
}

// TestMisbehavingTracer checks that a Tracer which panics or returns nothing
// costs the request its span and nothing more.
func TestMisbehavingTracer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*recordingTracer)
		log   bool
	}{
		{name: "panics when starting", setup: func(r *recordingTracer) { r.panicOnStart = true }, log: true},
		{name: "panics when ending", setup: func(r *recordingTracer) { r.panicOnEnd = true }, log: true},
		{name: "returns no span", setup: func(r *recordingTracer) { r.nilSpan = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, tracer := tracedOptions()
			tc.setup(tracer)
			logger, logs := captureLogger(t)
			opts.Logger = logger
			var seen SpanContext
			app := New(opts)
			app.Get("/x", func(ctx *Context, _ Empty) (itemOut, error) {
				seen, _ = SpanContextFromContext(ctx.Context())
				SpanFromContext(ctx.Context()).SetAttributes(slog.Int("n", 1))
				return itemOut{}, errors.New("and the handler failed too")
			})
			rec := do(t, mustBuild(t, app), "GET", "/x")
			assertStatus(t, rec, http.StatusInternalServerError)
			if !seen.IsValid() {
				t.Error("the request lost its trace identifiers along with its span")
			}
			if logged := strings.Contains(logs.String(), "the tracer panicked"); logged != tc.log {
				t.Errorf("logged a panic = %v, want %v\n%s", logged, tc.log, logs.String())
			}
		})
	}
}

func TestTracingBuildErrors(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Tracing = TracingOptions{
		Parent:          TraceParentIgnore,
		Sampler:         SampleRatio(1),
		RecordUserAgent: true,
	}
	message := buildError(t, New(opts))
	for _, want := range []string{"Tracing.Sampler is set", "Tracing.Parent is set", "Tracing.RecordUserAgent is set"} {
		if !strings.Contains(message, want) {
			t.Errorf("the build error does not mention %q:\n%s", want, message)
		}
	}
	if !strings.HasPrefix(message, "muzak: ") {
		t.Errorf("build error %q does not start with the package prefix", message)
	}
	opts, _ = tracedOptions()
	opts.Tracing.Parent = TraceParentPolicy(7)
	if message := buildError(t, New(opts)); !strings.HasPrefix(message, "muzak: Tracing.Parent is 7") {
		t.Errorf("build error = %s", message)
	}
}

// lifecycleTracer is a Tracer that is also a lifecycle component, as an
// exporter is.
type lifecycleTracer struct {
	recordingTracer
	lifecycleCounts
}

// lifecycleObserver is an observer that is also a lifecycle component.
type lifecycleObserver struct {
	lifecycleCounts
}

func (*lifecycleObserver) ObserveRequest(RequestObservation) {}

// lifecycleCounts counts starts and stops.
type lifecycleCounts struct {
	mu             sync.Mutex
	starts, stops  int
	componentLabel string
}

func (c *lifecycleCounts) Name() string { return "counted" + c.componentLabel }
func (c *lifecycleCounts) Start(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	return nil
}

func (c *lifecycleCounts) Stop(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops++
	return nil
}

// valueComponent is a lifecycle component that is not a pointer, and so is
// registered as it is given.
type valueComponent struct{ counts *lifecycleCounts }

func (v valueComponent) ObserveRequest(RequestObservation)         {}
func (v valueComponent) Name() string                              { return "value" }
func (v valueComponent) Start(ctx context.Context) error           { return v.counts.Start(ctx) }
func (v valueComponent) Stop(ctx context.Context) error            { return v.counts.Stop(ctx) }
func (v valueComponent) StartSpan(context.Context, SpanStart) Span { return noopSpan{} }

func TestTracerAndObserverLifecycles(t *testing.T) {
	t.Parallel()
	t.Run("started and stopped once each, even when registered too", func(t *testing.T) {
		t.Parallel()
		tracer, observer := &lifecycleTracer{}, &lifecycleObserver{}
		opts := quietOptions()
		opts.Tracing.Tracer = tracer
		opts.Observer = observer
		app := New(opts, WithLifecycle(tracer))
		app.Get("/x", okHandler)
		mustBuild(t, app)
		if err := app.StartLifecycle(t.Context()); err != nil {
			t.Fatalf("StartLifecycle = %v", err)
		}
		if err := app.StopLifecycle(t.Context()); err != nil {
			t.Fatalf("StopLifecycle = %v", err)
		}
		for name, counts := range map[string]*lifecycleCounts{"tracer": &tracer.lifecycleCounts, "observer": &observer.lifecycleCounts} {
			if counts.starts != 1 || counts.stops != 1 {
				t.Errorf("%s started %d and stopped %d times, want once each", name, counts.starts, counts.stops)
			}
		}
	})
	t.Run("a value that is not a pointer is registered as given", func(t *testing.T) {
		t.Parallel()
		counts := &lifecycleCounts{}
		component := valueComponent{counts: counts}
		opts := quietOptions()
		opts.Tracing.Tracer = component
		opts.Observer = component
		app := New(opts)
		app.Get("/x", okHandler)
		mustBuild(t, app)
		if err := app.StartLifecycle(t.Context()); err != nil {
			t.Fatalf("StartLifecycle = %v", err)
		}
		if err := app.StopLifecycle(t.Context()); err != nil {
			t.Fatalf("StopLifecycle = %v", err)
		}
		if counts.starts != 2 || counts.stops != 2 {
			t.Errorf("started %d and stopped %d times, want twice each, since values cannot be told apart safely", counts.starts, counts.stops)
		}
	})
}

// TestTracingDoesNotLeakGoroutines drives traced requests of every shape and
// checks that nothing is left running.
func TestTracingDoesNotLeakGoroutines(t *testing.T) {
	opts, tracer := tracedOptions()
	opts.Observer = RequestObserverFunc(func(RequestObservation) {})
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.Get("/items/{id}", func(ctx *Context, in itemIn) (itemOut, error) { return itemOut{Name: in.ID}, nil })
		app.SSE("/stream", streamItems("a", "b"))
		app.WS("/ws", wsEcho)
	})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := http.Get(fmt.Sprintf("%s/items/%d", server.URL, i))
			if err == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}
		}()
	}
	wg.Wait()
	reader := openStream(t, server.URL, "/stream")
	nextEvent(t, reader)
	conn := dialClient(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", WSDialOptions{})
	_ = conn.WriteText(t.Context(), "x")
	_, _ = conn.ReadText(t.Context())
	_ = conn.Close(WSStatusNormalClosure, "")
	_ = reader.Close()
	waitFor(t, func() bool {
		for _, span := range tracer.all() {
			if span.ended() != 1 {
				return false
			}
		}
		return len(tracer.all()) == 22
	}, "every span to end")
	server.Close()
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	assertNoGoroutineLeaks(t)
}
