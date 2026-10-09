package muzak

import (
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// observations collects what an observer is told.
type observations struct {
	mu  sync.Mutex
	all []RequestObservation
}

func (o *observations) ObserveRequest(observation RequestObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.all = append(o.all, observation)
}

// list returns a copy of what was observed so far.
func (o *observations) list() []RequestObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]RequestObservation(nil), o.all...)
}

// only returns the single observation, failing the test unless there is
// exactly one.
func (o *observations) only(tb testing.TB) RequestObservation {
	tb.Helper()
	all := o.list()
	if len(all) != 1 {
		tb.Fatalf("observed %d times, want exactly once: %+v", len(all), all)
	}
	return all[0]
}

// observedApp builds an application with an observer and the routes the
// observer tests use.
func observedApp(t *testing.T, opts AppOptions) (*App, *observations) {
	t.Helper()
	observer := &observations{}
	opts.Observer = observer
	app := New(opts)
	app.Get("/items/{id}", func(ctx *Context, in itemIn) (itemOut, error) { return itemOut{Name: in.ID}, nil })
	app.Post("/items", func(ctx *Context, in itemOut) (itemOut, error) { return in, nil })
	app.Get("/fail", func(*Context, Empty) (itemOut, error) { return itemOut{}, errors.New("broken") })
	app.Get("/panic", func(*Context, Empty) (itemOut, error) { panic("handler bug") })
	app.Get("/export", func(ctx *Context, _ Empty) (itemOut, error) {
		_, _ = ctx.ResponseWriter().Write([]byte("partial"))
		return itemOut{}, errors.New("the cursor broke")
	})
	app.Handle("PURGE", "/cache", func(*Context, Empty) (itemOut, error) { return itemOut{}, nil })
	return app, observer
}

func TestObserverIsCalledOncePerRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, method, target, body string
		want                       RequestObservation
	}{
		{name: "routed", method: "GET", target: "/items/42",
			want: RequestObservation{Method: "GET", Route: "/items/{id}", Status: 200, ResponseSize: int64(len(`{"name":"42"}`))}},
		{name: "a body", method: "POST", target: "/items", body: `{"name":"ab"}`,
			want: RequestObservation{Method: "POST", Route: "/items", Status: 200, RequestSize: 13, ResponseSize: 13}},
		{name: "no route", method: "GET", target: "/nowhere",
			want: RequestObservation{Method: "GET", Status: 404}},
		{name: "a method the path has no route for", method: "DELETE", target: "/items/1",
			want: RequestObservation{Method: "DELETE", Status: 405}},
		{name: "a method nobody defines", method: "BREW", target: "/nowhere",
			want: RequestObservation{Method: "_OTHER", Status: 404}},
		{name: "a method a route was registered for", method: "PURGE", target: "/cache",
			want: RequestObservation{Method: "PURGE", Route: "/cache", Status: 200}},
		{name: "OPTIONS from the route table", method: "OPTIONS", target: "/items/1",
			want: RequestObservation{Method: "OPTIONS", Status: 204}},
		{name: "a handler error", method: "GET", target: "/fail",
			want: RequestObservation{Method: "GET", Route: "/fail", Status: 500}},
		{name: "a handler panic", method: "GET", target: "/panic",
			want: RequestObservation{Method: "GET", Route: "/panic", Status: 500}},
		{name: "the documentation", method: "GET", target: "/openapi.json",
			want: RequestObservation{Method: "GET", Status: 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, observer := observedApp(t, quietOptions())
			mustBuild(t, app)
			var rec *httptest.ResponseRecorder
			if tc.body != "" {
				rec = do(t, app, tc.method, tc.target, tc.body)
			} else {
				rec = do(t, app, tc.method, tc.target)
			}
			assertStatus(t, rec, tc.want.Status)
			got := observer.only(t)
			if got.Duration <= 0 {
				t.Errorf("Duration = %v, want it measured", got.Duration)
			}
			if got.SpanContext.IsValid() {
				t.Errorf("SpanContext = %+v with tracing off", got.SpanContext)
			}
			got.Duration = 0
			if tc.want.ResponseSize == 0 {
				got.ResponseSize = 0
			}
			if got != tc.want {
				t.Errorf("observed %+v\nwant     %+v", got, tc.want)
			}
		})
	}
}

// TestObserverSeesAnAbortedResponse checks the one outcome a status cannot
// show.
func TestObserverSeesAnAbortedResponse(t *testing.T) {
	t.Parallel()
	app, observer := observedApp(t, quietOptions())
	mustBuild(t, app)
	if recovered := catchPanic(func() { do(t, app, "GET", "/export") }); recovered != http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared by identity
		t.Fatalf("recovered %v, want the abort sentinel", recovered)
	}
	got := observer.only(t)
	if !got.Aborted || got.Status != 200 || got.ResponseSize != int64(len("partial")) || got.Route != "/export" {
		t.Errorf("observed %+v, want an aborted 200 with the bytes that went out", got)
	}
}

// TestObserverSeesAPanicInMiddleware checks a request that recovery, rather
// than the route, turned into a 500.
func TestObserverSeesAPanicInMiddleware(t *testing.T) {
	t.Parallel()
	app, observer := observedApp(t, quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/items/boom" {
				panic("middleware bug")
			}
			next.ServeHTTP(w, r)
		})
	})
	assertStatus(t, do(t, mustBuild(t, app), "GET", "/items/boom"), http.StatusInternalServerError)
	if got := observer.only(t); got.Status != 500 || got.Aborted {
		t.Errorf("observed %+v, want the 500 recovery wrote", got)
	}
}

// TestObserverSeesStreamsOnceTheyEnd checks that an event stream and a
// WebSocket are each observed once, when they end, with the time they were
// open.
func TestObserverSeesStreamsOnceTheyEnd(t *testing.T) {
	t.Parallel()
	observer := &observations{}
	opts := quietOptions()
	opts.Observer = observer
	release := make(chan struct{})
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.SSE("/stream", func(ctx *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: "a"}); err != nil {
				return err
			}
			<-release
			return nil
		})
		app.WS("/ws", wsEcho)
	})
	reader := openStream(t, server.URL, "/stream")
	nextEvent(t, reader)
	time.Sleep(20 * time.Millisecond)
	if n := len(observer.list()); n != 0 {
		t.Fatalf("observed %d times while the stream was open", n)
	}
	close(release)
	assertStreamEnded(t, reader)
	waitFor(t, func() bool { return len(observer.list()) == 1 }, "the stream to be observed")
	stream := observer.only(t)
	if stream.Route != "/stream" || stream.Status != 200 || stream.Duration < 20*time.Millisecond {
		t.Errorf("observed %+v", stream)
	}

	conn := dialClient(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", WSDialOptions{})
	if err := conn.WriteText(t.Context(), "x"); err != nil {
		t.Fatalf("WriteText = %v", err)
	}
	if _, err := conn.ReadText(t.Context()); err != nil {
		t.Fatalf("ReadText = %v", err)
	}
	if n := len(observer.list()); n != 1 {
		t.Fatalf("observed %d times while the connection was open", n)
	}
	_ = conn.Close(WSStatusNormalClosure, "")
	waitFor(t, func() bool { return len(observer.list()) == 2 }, "the connection to be observed")
	if ws := observer.list()[1]; ws.Route != "/ws" || ws.Status != http.StatusSwitchingProtocols || ws.Aborted {
		t.Errorf("observed %+v", ws)
	}
}

// TestObserverPanicIsContained checks that a broken observer costs a log line
// and nothing the client can see.
func TestObserverPanicIsContained(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	logger, logs := captureLogger(t)
	opts.Logger = logger
	opts.Observer = RequestObserverFunc(func(RequestObservation) { panic("observer bug") })
	app := New(opts)
	app.Get("/x", okHandler)
	rec := do(t, mustBuild(t, app), "GET", "/x")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"ok":true}`)
	if !strings.Contains(logs.String(), "the request observer panicked") || !strings.Contains(logs.String(), "observer bug") {
		t.Errorf("the panic was not logged:\n%s", logs.String())
	}
}

// TestObserverCarriesTheSpanContext checks the field an exemplar is built
// from.
func TestObserverCarriesTheSpanContext(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	app, observer := observedApp(t, opts)
	do(t, mustBuild(t, app), "GET", "/items/1")
	if got := observer.only(t).SpanContext; got != tracer.only(t).start.SpanContext {
		t.Errorf("SpanContext = %+v, want the server span's", got)
	}
}

// TestRequestBodyCounting checks that the size observed is what the server
// read: a body the handler read part of and the framework drained after it,
// and a body refused at its limit, which is not read past it.
func TestRequestBodyCounting(t *testing.T) {
	t.Parallel()
	observer := &observations{}
	opts := quietOptions()
	opts.Observer = observer
	app := New(opts)
	app.Post("/upload", func(ctx *Context, _ Empty) (itemOut, error) {
		buf := make([]byte, 4)
		n, _ := ctx.Request().Body.Read(buf)
		return itemOut{Name: string(buf[:n])}, nil
	})
	app.Post("/small", func(ctx *Context, in itemOut) (itemOut, error) { return in, nil }, MaxBodySize(16))
	mustBuild(t, app)
	req := httptest.NewRequest("POST", "/upload", strings.NewReader("abcdefgh"))
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
	if got := observer.list()[0].RequestSize; got != 8 {
		t.Errorf("RequestSize = %d, want the 8 bytes read by the handler and the drain after it", got)
	}
	huge := `{"name":"` + strings.Repeat("x", 100_000) + `"}`
	assertStatus(t, do(t, app, "POST", "/small", huge), http.StatusRequestEntityTooLarge)
	if got := observer.list()[1].RequestSize; got > 16+4<<10 {
		t.Errorf("RequestSize = %d for a body refused at 16 bytes, want it bounded by the limit and the drain", got)
	}
	// A request with no body is not wrapped at all.
	assertStatus(t, do(t, app, "GET", "/upload"), http.StatusMethodNotAllowed)
	if got := observer.list()[2].RequestSize; got != 0 {
		t.Errorf("RequestSize = %d for a request with no body", got)
	}
}

// logRecords decodes every JSON record in a log buffer.
func logRecords(t *testing.T, logs *syncBuffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %v\n%s", err, line)
		}
		records = append(records, record)
	}
	return records
}

// recordWithMessage returns the first record with the given message.
func recordWithMessage(t *testing.T, records []map[string]any, message string) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["msg"] == message {
			return record
		}
	}
	t.Fatalf("no record says %q", message)
	return nil
}

func TestLogCorrelation(t *testing.T) {
	t.Parallel()
	serve := func(t *testing.T, opts AppOptions) []map[string]any {
		t.Helper()
		logger, logs := captureLogger(t)
		opts.Logger = logger
		app := New(opts)
		app.Get("/x", func(ctx *Context, _ Empty) (itemOut, error) {
			ctx.Logger().Info("from the handler")
			return itemOut{}, nil
		})
		mustBuild(t, app)
		do(t, app, "GET", "/x")
		return logRecords(t, logs)
	}
	t.Run("on", func(t *testing.T) {
		t.Parallel()
		opts, tracer := tracedOptions()
		records := serve(t, opts)
		sc := tracer.only(t).start.SpanContext
		for _, message := range []string{"from the handler", "GET /x"} {
			record := recordWithMessage(t, records, message)
			if record[TraceIDKey] != sc.TraceID.String() || record[SpanIDKey] != sc.SpanID.String() {
				t.Errorf("%q carries trace %v span %v, want %s %s", message, record[TraceIDKey], record[SpanIDKey], sc.TraceID, sc.SpanID)
			}
		}
	})
	t.Run("on but not sampled", func(t *testing.T) {
		t.Parallel()
		opts, _ := tracedOptions()
		opts.Tracing.Sampler = SampleRatio(0)
		records := serve(t, opts)
		record := recordWithMessage(t, records, "from the handler")
		if id, _ := record[TraceIDKey].(string); len(id) != 32 {
			t.Errorf("an unsampled request logged trace_id %v, want its identifier all the same", record[TraceIDKey])
		}
	})
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		for _, record := range serve(t, quietOptions()) {
			if _, ok := record[TraceIDKey]; ok {
				t.Errorf("a record carries trace_id with tracing off: %v", record)
			}
			if _, ok := record[SpanIDKey]; ok {
				t.Errorf("a record carries span_id with tracing off: %v", record)
			}
		}
	})
	t.Run("a pooled context carries nothing over", func(t *testing.T) {
		t.Parallel()
		opts, tracer := tracedOptions()
		logger, logs := captureLogger(t)
		opts.Logger = logger
		app := New(opts)
		app.Get("/x", func(ctx *Context, _ Empty) (itemOut, error) {
			ctx.Logger().Info("from the handler")
			return itemOut{}, nil
		})
		mustBuild(t, app)
		do(t, app, "GET", "/x")
		do(t, app, "GET", "/x")
		var ids []any
		for _, record := range logRecords(t, logs) {
			if record["msg"] == "from the handler" {
				ids = append(ids, record[TraceIDKey])
			}
		}
		spans := tracer.all()
		if len(ids) != 2 || ids[0] == ids[1] || ids[1] != spans[1].start.SpanContext.TraceID.String() {
			t.Errorf("trace ids logged = %v, want each request's own", ids)
		}
	})
}

// TestObservabilityOffCostsNothing checks that an application configuring
// neither tracing nor an observer installs nothing and that every hook the
// rest of the package calls allocates nothing.
func TestObservabilityOffCostsNothing(t *testing.T) {
	app := New(quietOptions())
	app.Get("/x", okHandler)
	mustBuild(t, app)
	if app.obs != nil {
		t.Fatal("an application with neither tracing nor an observer has observability state")
	}
	withMiddleware := New(quietOptions())
	if len(withMiddleware.middleware) != len(app.middleware) {
		t.Fatal("the chain differs between two identical applications")
	}
	traced, _ := tracedOptions()
	if got := len(New(traced).middleware); got != len(app.middleware)+1 {
		t.Fatalf("tracing installed %d middleware, want exactly one", got-len(app.middleware))
	}

	req := httptest.NewRequest("GET", "/x", nil)
	c := &Context{w: asResponseWriter(httptest.NewRecorder()), r: req, app: app, logger: app.logger}
	err := errors.New("x")
	if allocs := testing.AllocsPerRun(100, func() {
		app.observeFailure(c, err)
		app.observeStreamFailure(c, err)
		_, _ = SpanContextFromContext(req.Context())
		InjectTraceContext(req.Context(), req.Header)
	}); allocs != 0 {
		t.Errorf("the hooks allocate %v times per request with tracing off", allocs)
	}

	// Context.Logger costs what it did before tracing existed, which is what
	// this reproduces line for line.
	c.requestID = "id"
	baseline := testing.AllocsPerRun(100, func() {
		attrs := []any{
			slog.String("method", truncateForMessage(c.r.Method)),
			slog.String("path", truncateForMessage(c.r.URL.Path)),
		}
		if c.requestID != "" {
			attrs = append(attrs, slog.String(RequestIDKey, c.requestID))
		}
		_ = c.logger.With(attrs...)
	})
	got := testing.AllocsPerRun(100, func() {
		c.requestLogger = nil
		_ = c.Logger()
	})
	if got != baseline {
		t.Errorf("Context.Logger allocates %v times with tracing off, want the %v it did before", got, baseline)
	}
}

// TestObservabilityUnderConcurrency drives traced, observed requests from many
// goroutines at once, for the race detector.
func TestObservabilityUnderConcurrency(t *testing.T) {
	t.Parallel()
	opts, tracer := tracedOptions()
	app, observer := observedApp(t, opts)
	app.Get("/children", func(ctx *Context, _ Empty) (itemOut, error) {
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				child, span := StartSpan(ctx.Context(), "work", SpanKindInternal)
				SpanFromContext(child).SetAttributes(slog.Int("n", 1))
				span.End()
			}()
		}
		wg.Wait()
		return itemOut{}, nil
	})
	mustBuild(t, app)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := "/items/1"
			if i%2 == 0 {
				target = "/children"
			}
			do(t, app, "GET", target)
		}()
	}
	wg.Wait()
	if got := len(observer.list()); got != 50 {
		t.Errorf("observed %d requests, want 50", got)
	}
	if got := len(tracer.all()); got != 50+25*4 {
		t.Errorf("recorded %d spans, want 150", got)
	}
}
