package otlp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	muzak "muzak.dev/framework"
)

// stringer is a value with a String method, which is recorded as its text.
type stringer struct{}

func (stringer) String() string { return "from String" }

func TestConvertValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{name: "int64 slice", value: []int64{1, 2}, want: "array[int(1) int(2)]"},
		{name: "float slice", value: []float64{0.5}, want: "array[double(0.5)]"},
		{name: "bool slice", value: []bool{true, false}, want: "array[bool(true) bool(false)]"},
		{name: "int slice", value: []int{7}, want: "array[int(7)]"},
		{name: "a Stringer", value: stringer{}, want: "string(from String)"},
		{name: "bytes", value: []byte("ab"), want: "bytes(ab)"},
	}
	for _, tc := range cases {
		kv, ok := convertAttr(slog.Any("k", tc.value), 0)
		if !ok {
			t.Fatalf("%s: refused", tc.name)
		}
		if got := describe(kv.value); got != tc.want {
			t.Errorf("%s: converted to %s, want %s", tc.name, got, tc.want)
		}
	}
	if _, ok := convertAttr(slog.Int("", 1), 0); ok {
		t.Error("an attribute with no key was converted")
	}
	members := make([]any, 0, 400)
	for i := range 200 {
		members = append(members, slog.Int(fmt.Sprintf("m%d", i), i))
	}
	kv, _ := convertAttr(slog.Group("g", members...), 0)
	if len(kv.value.kvs) != maxListLength {
		t.Errorf("a group of 200 kept %d members, want %d", len(kv.value.kvs), maxListLength)
	}
	big := make([]byte, 10_000)
	kv, _ = convertAttr(slog.Any("b", big), 0)
	if len(kv.value.bytes) != maxStringLength {
		t.Errorf("10000 bytes were kept as %d", len(kv.value.bytes))
	}
	big[0] = 'x'
	if kv.value.bytes[0] == 'x' {
		t.Error("the bytes were kept by reference, where the caller can still change them")
	}
}

// describe renders a converted value compactly for a comparison.
func describe(v value) string {
	switch v.kind {
	case kindString:
		return "string(" + v.str + ")"
	case kindInt:
		return fmt.Sprintf("int(%d)", v.num)
	case kindDouble:
		return fmt.Sprintf("double(%v)", v.float)
	case kindBool:
		return fmt.Sprintf("bool(%v)", v.flag)
	case kindBytes:
		return "bytes(" + string(v.bytes) + ")"
	case kindArray:
		parts := make([]string, len(v.list))
		for i, element := range v.list {
			parts[i] = describe(element)
		}
		return "array[" + strings.Join(parts, " ") + "]"
	}
	return fmt.Sprintf("kind(%d)", v.kind)
}

func TestTruncateKeepsUTF8Whole(t *testing.T) {
	t.Parallel()
	euro := "\xe2\x82\xac"
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{in: "short", limit: 10, want: "short"},
		{in: "ab" + euro, limit: 3, want: "ab"},
		{in: "ab" + euro, limit: 4, want: "ab"},
		{in: "ab" + euro, limit: 5, want: "ab" + euro},
		{in: strings.Repeat("v", 4095) + euro, limit: maxStringLength, want: strings.Repeat("v", 4095)},
		{in: "\x80\x80\x80\x80\x80", limit: 2, want: ""},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.limit); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}

func TestSpanBudget(t *testing.T) {
	t.Parallel()
	e, err := New(Options{Endpoint: "http://c"})
	if err != nil {
		t.Fatal(err)
	}
	s := newSpan(e, muzak.SpanStart{Name: "x"})
	if s.start.IsZero() {
		t.Error("a span given no start time has none")
	}
	filler := strings.Repeat("f", maxStringLength)
	for i := 0; s.size+maxStringLength < maxSpanBytes; i++ {
		s.SetAttributes(slog.String(fmt.Sprintf("a%02d", i), filler))
	}
	before := len(s.attrs)
	// Replacing a small value with one that does not fit is refused.
	s.SetAttributes(slog.String("small", "s"))
	s.SetAttributes(slog.String("small", filler))
	if got, _ := lookup(s, "small"); got != "s" || s.droppedAttrs != 1 {
		t.Errorf("small = %q dropped = %d, want the replacement refused", got, s.droppedAttrs)
	}
	// An event whose attribute does not fit keeps its name and drops the
	// attribute.
	s.AddEvent("big", slog.String("x", filler))
	if len(s.events) != 1 || s.events[0].dropped != 1 {
		t.Fatalf("events = %d, want one with its attribute dropped", len(s.events))
	}
	// With the budget all but spent, an event is dropped whole.
	remaining := maxSpanBytes - s.size
	s.SetAttributes(slog.String("z", strings.Repeat("z", remaining-len("z")-valueOverhead-8)))
	s.AddEvent("too late")
	if s.droppedEvents != 1 || len(s.events) != 1 {
		t.Errorf("events = %d dropped = %d, want the second dropped", len(s.events), s.droppedEvents)
	}
	// An event's attributes past the bound, or with no key, are dropped.
	s2 := newSpan(e, muzak.SpanStart{Name: "y"})
	attrs := make([]slog.Attr, 0, maxAttributes+3)
	for i := range maxAttributes + 2 {
		attrs = append(attrs, slog.Int(fmt.Sprintf("e%d", i), i))
	}
	attrs = append(attrs, slog.Int("", 1))
	s2.AddEvent("many", attrs...)
	if len(s2.events) != 1 || len(s2.events[0].attrs) != maxAttributes || s2.events[0].dropped != 2 {
		t.Errorf("event attrs = %d dropped = %d", len(s2.events[0].attrs), s2.events[0].dropped)
	}
	if s.size > maxSpanBytes || before == 0 {
		t.Errorf("the span holds %d bytes, past the %d budget", s.size, maxSpanBytes)
	}
}

// TestSpanBudgetCountsEveryValue checks that the size a span is held to counts
// what each value it keeps costs, and not only its text: a list of numbers, of
// empty strings or of nils has next to no text, and counted by its text alone a
// span of them held many times the bound, and the queue many times what
// QueueSize times the bound promises.
func TestSpanBudgetCountsEveryValue(t *testing.T) {
	t.Parallel()
	if size := int(reflect.TypeFor[keyValue]().Size()); size > valueOverhead {
		t.Fatalf("a keyValue is %d bytes, more than the %d counted for each value", size, valueOverhead)
	}
	e, err := New(Options{Endpoint: "http://c"})
	if err != nil {
		t.Fatal(err)
	}
	row := make([]any, maxListLength)
	grid := make([]any, maxListLength)
	for i := range grid {
		grid[i] = row
	}
	s := newSpan(e, muzak.SpanStart{Name: "grid"})
	for i := range maxAttributes {
		s.SetAttributes(slog.Any(fmt.Sprintf("grid%d", i), grid), slog.Any(fmt.Sprintf("ints%d", i), make([]int, maxListLength)))
	}
	s.AddEvent("e", slog.Any("grid", grid))
	held := 0
	for _, kv := range s.attrs {
		held += valuesIn(kv.value) * valueOverhead
	}
	for _, ev := range s.events {
		for _, kv := range ev.attrs {
			held += valuesIn(kv.value) * valueOverhead
		}
	}
	if held > maxSpanBytes {
		t.Errorf("the span holds %d values, %d bytes, past the %d budget", held/valueOverhead, held, maxSpanBytes)
	}
}

// TestConversionStopsAtTheBudget checks that converting a value stops once it
// is past what any span may hold, which it is then dropped for. A list of
// lists that share their elements costs the application nothing to build, and
// converted in full it is the list length to the power of the depth: 128 to
// the third is two million values for an attribute that is then thrown away.
func TestConversionStopsAtTheBudget(t *testing.T) {
	t.Parallel()
	nested := any(make([]any, maxListLength))
	for range maxValueDepth - 2 {
		level := make([]any, maxListLength)
		for i := range level {
			level[i] = nested
		}
		nested = level
	}
	kv, ok := convertAttr(slog.Any("nested", nested), 0)
	if !ok || kv.size <= maxSpanBytes {
		t.Fatalf("converted to %d bytes, want it past the %d budget so it is dropped", kv.size, maxSpanBytes)
	}
	if n := valuesIn(kv.value); n > 2*maxSpanBytes/valueOverhead {
		t.Errorf("converted %d values of an attribute past the budget, want the work bounded by it", n)
	}
}

// valuesIn counts the values a converted value holds, itself included.
func valuesIn(v value) int {
	n := 1
	for _, element := range v.list {
		n += valuesIn(element)
	}
	for _, kv := range v.kvs {
		n += valuesIn(kv.value)
	}
	return n
}

// lookup returns a recorded string attribute.
func lookup(s *span, key string) (string, bool) {
	for _, kv := range s.attrs {
		if kv.key == key {
			return kv.value.str, true
		}
	}
	return "", false
}

func TestModuleVersion(t *testing.T) {
	t.Parallel()
	if got := moduleVersion(nil, false); got != "" {
		t.Errorf("no build information gave %q", got)
	}
	info := &debug.BuildInfo{Deps: []*debug.Module{{Path: "example.com/other", Version: "v9"}, {Path: scopeName, Version: "v0.3.0"}}}
	if got := moduleVersion(info, true); got != "v0.3.0" {
		t.Errorf("moduleVersion = %q", got)
	}
	if got := moduleVersion(&debug.BuildInfo{}, true); got != "" {
		t.Errorf("a program without the framework gave %q", got)
	}
}

// TestScopeVersionIsSent checks that the framework's version, when the program
// knows it, is reported with the scope.
func TestScopeVersionIsSent(t *testing.T) {
	t.Parallel()
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	e := startExporter(t, opts)
	e.scopeVersion = "v0.3.0"
	endSpans(e, 1)
	flush(t, e)
	scope := object(object(list(object(list(c.all()[0].doc["resourceSpans"])[0])["scopeSpans"])[0])["scope"])
	if scope["version"] != "v0.3.0" {
		t.Errorf("scope = %v", scope)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportAndClient(t *testing.T) {
	t.Parallel()
	if newTransport(roundTripperFunc(nil)) == nil {
		t.Error("a default transport of another type did not give a plain one")
	}
	base := &http.Transport{MaxIdleConns: 7}
	if cloned := newTransport(base); cloned == base || cloned.MaxIdleConns != 7 {
		t.Errorf("the default transport was not cloned: %v", cloned)
	}
	c := newCollector(t, nil)
	used := make(chan struct{}, 10)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		used <- struct{}{}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	userClient := &http.Client{Transport: transport, Timeout: time.Hour}
	opts, _ := testOptions(c.server.URL)
	opts.Client = userClient
	e := startExporter(t, opts)
	if e.client == userClient || userClient.CheckRedirect != nil {
		t.Error("the caller's client was changed rather than copied")
	}
	endSpans(e, 1)
	flush(t, e)
	if len(used) == 0 || len(c.spans()) != 1 {
		t.Errorf("the configured client was not used")
	}
}

// idleTracker is a transport that counts the times it was asked to close its
// idle connections.
type idleTracker struct {
	roundTripperFunc
	closed atomic.Int32
}

func (t *idleTracker) CloseIdleConnections() { t.closed.Add(1) }

// TestStopClosesOnlyItsOwnTransport is the regression test for Stop closing
// the idle connections of a transport the exporter did not make: the one in
// Options.Client, and for a client that names none, http.DefaultTransport,
// whose connections every other client in the process shares.
func TestStopClosesOnlyItsOwnTransport(t *testing.T) {
	t.Parallel()
	t.Run("the transport of the client given", func(t *testing.T) {
		t.Parallel()
		tracker := &idleTracker{}
		opts, _ := testOptions("http://collector.invalid")
		opts.Client = &http.Client{Transport: tracker}
		e, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := tracker.closed.Load(); n != 0 {
			t.Errorf("Stop closed the idle connections of the caller's transport %d times", n)
		}
	})
	t.Run("the default transport", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(server.Close)
		// get makes a request on the default transport and waits until its
		// connection is idle, reporting whether it was one already idle.
		get := func() bool {
			reused := false
			idle := make(chan struct{}, 1)
			trace := &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
				PutIdleConn: func(err error) {
					if err == nil {
						idle <- struct{}{}
					}
				},
			}
			req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			select {
			case <-idle:
			case <-time.After(10 * time.Second):
				t.Fatal("the connection never became idle")
			}
			return reused
		}
		get()
		opts, _ := testOptions(server.URL)
		opts.Client = &http.Client{Timeout: time.Minute}
		e, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !get() {
			t.Error("Stop closed the idle connections of http.DefaultTransport")
		}
	})
}

// TestFlushRacingStop checks that a flush asked of a run that has ended
// returns rather than waiting on a worker that has gone, and that one asked
// while the worker is busy returns when its context does.
func TestFlushRacingStop(t *testing.T) {
	t.Parallel()
	r := &run{flush: make(chan chan struct{}), done: make(chan struct{})}
	close(r.done)
	if err := r.requestFlush(context.Background()); err != ErrNotRunning { //nolint:errorlint // the sentinel is returned as it is
		t.Errorf("requestFlush on an ended run = %v", err)
	}
	busy := &run{flush: make(chan chan struct{}), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := busy.requestFlush(ctx); err != context.DeadlineExceeded { //nolint:errorlint // the context's own error
		t.Errorf("requestFlush on a busy run = %v", err)
	}
}

// TestStopRacingStopDoesNotHang checks a second Stop made while the first
// one's final flush is under way, which is what two applications sharing one
// exporter as their Tracer do when they shut down together: the second finds
// the exporter stopped and discards what is still queued, and the flush that
// had counted those spans must not wait for them for ever, and Stop with it.
func TestStopRacingStopDoesNotHang(t *testing.T) {
	t.Parallel()
	var e *Exporter
	c := newCollector(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 0 {
			// The first batch of the final flush is in flight.
			_ = e.Stop(context.Background())
		}
		_, _ = io.WriteString(w, "{}")
	})
	opts, _ := testOptions(c.server.URL)
	opts.BatchSize = 2
	var err error
	if e, err = New(opts); err != nil {
		t.Fatal(err)
	}
	endSpans(e, 10)
	r := &run{stopping: make(chan struct{}), flush: make(chan chan struct{}), done: make(chan struct{})}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	defer r.cancel()
	drained := make(chan []*span, 1)
	go func() { drained <- e.drain(r, nil) }()
	select {
	case rest := <-drained:
		if len(rest) != 0 || len(c.all()) != 1 {
			t.Errorf("drain left %d spans after %d requests, want the one batch sent and nothing left", len(rest), len(c.all()))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the final flush waited for spans another Stop had discarded")
	}
}

// TestStartRacingStop checks that an exporter started again while the run
// before it is still finishing has nothing two workers both write to without
// a lock; run with -race. It is not parallel, since it counts the exporter
// goroutines left running.
func TestStartRacingStop(t *testing.T) {
	c := newCollector(t, nil)
	opts, _ := testOptions(c.server.URL)
	opts.QueueSize = 1
	opts.BatchSize = 1
	opts.BatchInterval = time.Millisecond
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		endSpans(e, 5)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Stop(context.Background())
		}()
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	_ = e.Stop(context.Background())
	assertNoExporterGoroutines(t)
}

// TestStopCancelledDuringARetryWait checks a Stop with no deadline whose
// context is cancelled while the worker waits to retry: the wait ends with
// it.
func TestStopCancelledDuringARetryWait(t *testing.T) {
	t.Parallel()
	c := newCollector(t, statusResponder([]int{503, 503}, http.Header{"Retry-After": {"30"}}))
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
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	began := time.Now()
	err = e.Stop(ctx)
	if took := time.Since(began); took > time.Second {
		t.Errorf("Stop took %v", took)
	}
	if err == nil {
		t.Error("Stop reported nothing lost")
	}
}
