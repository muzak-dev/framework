package otlp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
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
	s.SetAttributes(slog.String("z", strings.Repeat("z", remaining-len("z")-8)))
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
	if _, ok := newTransport(roundTripperFunc(nil)).(*http.Transport); !ok {
		t.Error("a default transport of another type did not give a plain one")
	}
	base := &http.Transport{MaxIdleConns: 7}
	cloned, ok := newTransport(base).(*http.Transport)
	if !ok || cloned == base || cloned.MaxIdleConns != 7 {
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
