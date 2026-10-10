package muzak

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Values from the W3C Trace Context recommendation and its conformance suite.
const (
	testTraceIDHex = "0af7651916cd43dd8448eb211c80319c"
	testSpanIDHex  = "b7ad6b7169203331"
	testParent     = "00-" + testTraceIDHex + "-" + testSpanIDHex + "-01"
)

func TestParseTraceparent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   string
		ok      bool
		sampled bool
		flags   TraceFlags
	}{
		{name: "sampled", value: testParent, ok: true, sampled: true, flags: 0x01},
		{name: "not sampled", value: "00-" + testTraceIDHex + "-" + testSpanIDHex + "-00", ok: true, flags: 0x00},
		{name: "unknown flags are kept", value: "00-" + testTraceIDHex + "-" + testSpanIDHex + "-09", ok: true, sampled: true, flags: 0x09},
		{name: "a higher version of exactly the same length", value: "cc-" + testTraceIDHex + "-" + testSpanIDHex + "-01", ok: true, sampled: true, flags: 0x01},
		{name: "a higher version followed by more fields", value: "cc-" + testTraceIDHex + "-" + testSpanIDHex + "-01-what-the-future-will-be-like", ok: true, sampled: true, flags: 0x01},
		{name: "a higher version followed by something that is not a field", value: "cc-" + testTraceIDHex + "-" + testSpanIDHex + "-01.what-the-future-will-be-like"},
		{name: "version 00 followed by more fields", value: testParent + "-what-the-future-will-be-like"},
		{name: "version 00 with a trailing space", value: testParent + " "},
		{name: "version ff", value: "ff-" + testTraceIDHex + "-" + testSpanIDHex + "-01"},
		{name: "an uppercase version", value: "0A-" + testTraceIDHex + "-" + testSpanIDHex + "-01"},
		{name: "a version that is not hex", value: "0g-" + testTraceIDHex + "-" + testSpanIDHex + "-01"},
		{name: "an uppercase trace id", value: "00-" + strings.ToUpper(testTraceIDHex) + "-" + testSpanIDHex + "-01"},
		{name: "an uppercase span id", value: "00-" + testTraceIDHex + "-" + strings.ToUpper(testSpanIDHex) + "-01"},
		{name: "uppercase flags", value: "00-" + testTraceIDHex + "-" + testSpanIDHex + "-0A"},
		{name: "an all-zero trace id", value: "00-00000000000000000000000000000000-" + testSpanIDHex + "-01"},
		{name: "an all-zero span id", value: "00-" + testTraceIDHex + "-0000000000000000-01"},
		{name: "a trace id that is not hex", value: "00-0af7651916cd43dd8448eb211c80319g-" + testSpanIDHex + "-01"},
		{name: "a span id that is not hex", value: "00-" + testTraceIDHex + "-b7ad6b716920333z-01"},
		{name: "flags that are not hex", value: "00-" + testTraceIDHex + "-" + testSpanIDHex + "-0z"},
		{name: "one character short", value: testParent[:len(testParent)-1]},
		{name: "a short trace id", value: "00-0af7651916cd43dd8448eb211c8031-" + testSpanIDHex + "-01"},
		{name: "underscores for dashes", value: "00_" + testTraceIDHex + "_" + testSpanIDHex + "_01"},
		{name: "a dash missing after the trace id", value: "00-" + testTraceIDHex + "0" + testSpanIDHex + "-01"},
		{name: "a dash missing after the span id", value: "00-" + testTraceIDHex + "-" + testSpanIDHex + "001"},
		{name: "a leading space", value: " " + testParent[:len(testParent)-1]},
		{name: "empty", value: ""},
		{name: "a version alone", value: "00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sc, ok := parseTraceparent(tc.value)
			if ok != tc.ok {
				t.Fatalf("parseTraceparent(%q) ok = %v, want %v", tc.value, ok, tc.ok)
			}
			if !ok {
				if sc != (SpanContext{}) {
					t.Errorf("a refused value returned %+v, want the zero SpanContext", sc)
				}
				return
			}
			if got := sc.TraceID.String(); got != testTraceIDHex {
				t.Errorf("TraceID = %s, want %s", got, testTraceIDHex)
			}
			if got := sc.SpanID.String(); got != testSpanIDHex {
				t.Errorf("SpanID = %s, want %s", got, testSpanIDHex)
			}
			if sc.TraceFlags != tc.flags || sc.IsSampled() != tc.sampled {
				t.Errorf("flags = %02x sampled = %v, want %02x and %v", byte(sc.TraceFlags), sc.IsSampled(), byte(tc.flags), tc.sampled)
			}
			if !sc.Remote {
				t.Error("a span context read from a header is not marked remote")
			}
			if !sc.IsValid() {
				t.Error("a parsed span context reports itself invalid")
			}
		})
	}
}

// TestTraceparentRoundTrip checks that what is parsed is written back the same
// way, and that a higher version is propagated as the version this
// implementation speaks, as the recommendation asks.
func TestTraceparentRoundTrip(t *testing.T) {
	t.Parallel()
	sc, ok := parseTraceparent(testParent)
	if !ok {
		t.Fatal("the reference traceparent was refused")
	}
	if got := sc.Traceparent(); got != testParent {
		t.Errorf("Traceparent() = %q, want %q", got, testParent)
	}
	future, ok := parseTraceparent("cc-" + testTraceIDHex + "-" + testSpanIDHex + "-01-more")
	if !ok {
		t.Fatal("a higher version was refused")
	}
	if got := future.Traceparent(); got != testParent {
		t.Errorf("a higher version was written back as %q, want %q", got, testParent)
	}
	if got := (SpanContext{}).Traceparent(); got != "" {
		t.Errorf("an invalid span context renders as %q, want nothing", got)
	}
}

func TestParseTracestate(t *testing.T) {
	t.Parallel()
	member := func(i int) string {
		return "k" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "=v"
	}
	members := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = member(i)
		}
		return strings.Join(parts, ",")
	}
	cases := []struct {
		name   string
		values []string
		want   string
		ok     bool
	}{
		{name: "absent", values: nil, want: "", ok: true},
		{name: "empty", values: []string{""}, want: "", ok: true},
		{name: "one member", values: []string{"congo=t61rcWkgMzE"}, want: "congo=t61rcWkgMzE", ok: true},
		{name: "two members", values: []string{"rojo=00f067aa0ba902b7,congo=t61rcWkgMzE"}, want: "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE", ok: true},
		{name: "optional whitespace", values: []string{" rojo=00f067aa0ba902b7 ,\tcongo=t61rcWkgMzE\t"}, want: "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE", ok: true},
		{name: "empty members", values: []string{",,rojo=1,, ,congo=2,"}, want: "rojo=1,congo=2", ok: true},
		{name: "several headers combined", values: []string{"rojo=1", "congo=2"}, want: "rojo=1,congo=2", ok: true},
		{name: "a multi-tenant key", values: []string{"fw529a3039@dt=xyz"}, want: "fw529a3039@dt=xyz", ok: true},
		{name: "a value with an inner space", values: []string{"a=b c"}, want: "a=b c", ok: true},
		{name: "every key character", values: []string{"a0_-*/=1"}, want: "a0_-*/=1", ok: true},
		{name: "32 members", values: []string{members(32)}, want: members(32), ok: true},
		{name: "33 members", values: []string{members(33)}},
		{name: "33 members across headers", values: []string{members(20), members(13)}},
		{name: "a key of 256", values: []string{"a" + strings.Repeat("b", 255) + "=1"}, want: "a" + strings.Repeat("b", 255) + "=1", ok: true},
		{name: "a key of 257", values: []string{"a" + strings.Repeat("b", 256) + "=1"}},
		{name: "a tenant of 241", values: []string{strings.Repeat("t", 241) + "@s=1"}, want: strings.Repeat("t", 241) + "@s=1", ok: true},
		{name: "a tenant of 242", values: []string{strings.Repeat("t", 242) + "@s=1"}},
		{name: "a system of 14", values: []string{"t@" + strings.Repeat("s", 14) + "=1"}, want: "t@" + strings.Repeat("s", 14) + "=1", ok: true},
		{name: "a system of 15", values: []string{"t@" + strings.Repeat("s", 15) + "=1"}},
		{name: "a tenant may start with a digit", values: []string{"1t@s=1"}, want: "1t@s=1", ok: true},
		{name: "a simple key may not start with a digit", values: []string{"1t=1"}},
		{name: "a system may not start with a digit", values: []string{"t@1s=1"}},
		{name: "an empty tenant", values: []string{"@s=1"}},
		{name: "an empty system", values: []string{"t@=1"}},
		{name: "two at signs", values: []string{"t@s@x=1"}},
		{name: "an uppercase key", values: []string{"Rojo=1"}},
		{name: "a key with a dot", values: []string{"ro.jo=1"}},
		{name: "a value of 256", values: []string{"a=" + strings.Repeat("v", 256)}, want: "a=" + strings.Repeat("v", 256), ok: true},
		{name: "a value of 257", values: []string{"a=" + strings.Repeat("v", 257)}},
		{name: "an empty value", values: []string{"a="}},
		{name: "no equals sign", values: []string{"rojo"}},
		{name: "an equals sign in the value", values: []string{"a=b=c"}},
		{name: "a control character in the value", values: []string{"a=b\x01"}},
		{name: "a tab inside the value", values: []string{"a=b\tc"}},
		{name: "DEL in the value", values: []string{"a=b\x7f"}},
		{name: "a non-ASCII value", values: []string{"a=caf\xc3\xa9"}},
		{name: "a duplicated key", values: []string{"rojo=1,congo=2,rojo=3"}},
		{name: "a duplicated key across headers", values: []string{"rojo=1", "rojo=2"}},
		{name: "one bad member spoils the header", values: []string{"rojo=1,BAD=2,congo=3"}},
		{name: "512 bytes", values: []string{"a=" + strings.Repeat("v", 254) + ",b=" + strings.Repeat("v", 253)}, want: "a=" + strings.Repeat("v", 254) + ",b=" + strings.Repeat("v", 253), ok: true},
		{name: "513 bytes", values: []string{"a=" + strings.Repeat("v", 254) + ",b=" + strings.Repeat("v", 254)}},
		{name: "a huge header is refused unread", values: []string{strings.Repeat(" ", 1<<16) + "a=1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseTracestate(tc.values)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseTracestate(%q) = %q, %v; want %q, %v", tc.values, got, ok, tc.want, tc.ok)
			}
		})
	}
	if got := len(members(32)); got > maxTracestateBytes {
		t.Fatalf("the 32 member case is %d bytes, so it tests the byte bound rather than the member bound", got)
	}
}

func TestExtractTraceContext(t *testing.T) {
	t.Parallel()
	header := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(pairs); i += 2 {
			h.Add(pairs[i], pairs[i+1])
		}
		return h
	}
	t.Run("traceparent and tracestate", func(t *testing.T) {
		t.Parallel()
		sc, ok := extractTraceContext(header("traceparent", testParent, "tracestate", "rojo=1", "tracestate", "congo=2"))
		if !ok || sc.TraceState != "rojo=1,congo=2" {
			t.Fatalf("extract = %+v, %v", sc, ok)
		}
	})
	t.Run("an invalid tracestate is dropped and the parent kept", func(t *testing.T) {
		t.Parallel()
		sc, ok := extractTraceContext(header("traceparent", testParent, "tracestate", "BAD=1"))
		if !ok || sc.TraceState != "" {
			t.Fatalf("extract = %+v, %v; want the parent without its state", sc, ok)
		}
	})
	t.Run("two traceparent headers are refused", func(t *testing.T) {
		t.Parallel()
		if sc, ok := extractTraceContext(header("traceparent", testParent, "traceparent", testParent)); ok {
			t.Fatalf("extract = %+v, want a refusal", sc)
		}
	})
	t.Run("no traceparent means no tracestate either", func(t *testing.T) {
		t.Parallel()
		if sc, ok := extractTraceContext(header("tracestate", "rojo=1")); ok {
			t.Fatalf("extract = %+v, want nothing", sc)
		}
	})
	t.Run("an invalid traceparent means its tracestate is not read", func(t *testing.T) {
		t.Parallel()
		if sc, ok := extractTraceContext(header("traceparent", "garbage", "tracestate", "rojo=1")); ok {
			t.Fatalf("extract = %+v, want nothing", sc)
		}
	})
}

// TestGeneratedIDsAreUniqueAndValid draws many identifiers and checks that
// none is zero and none repeats, which a predictable or broken source would
// show long before ten thousand draws.
func TestGeneratedIDsAreUniqueAndValid(t *testing.T) {
	t.Parallel()
	const draws = 10_000
	traces := make(map[TraceID]struct{}, draws)
	spans := make(map[SpanID]struct{}, draws)
	for range draws {
		trace, span := newTraceID(), newSpanID()
		if !trace.IsValid() || !span.IsValid() {
			t.Fatalf("drew an invalid identifier: %s %s", trace, span)
		}
		traces[trace] = struct{}{}
		spans[span] = struct{}{}
	}
	if len(traces) != draws || len(spans) != draws {
		t.Fatalf("drew %d distinct trace ids and %d distinct span ids out of %d", len(traces), len(spans), draws)
	}
}

// TestDrawnIDsAreDistinctAcrossGoroutines draws trace and span identifiers
// from many goroutines at once, across many refills of the buffer they come
// from, and finds no two the same: each is given bytes no other was.
func TestDrawnIDsAreDistinctAcrossGoroutines(t *testing.T) {
	t.Parallel()
	const workers, each = 8, 400
	traces := make([][]TraceID, workers)
	spans := make([][]SpanID, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for range each {
				traces[w] = append(traces[w], newTraceID())
				spans[w] = append(spans[w], newSpanID())
			}
		})
	}
	wg.Wait()
	seenTraces := make(map[TraceID]bool, workers*each)
	seenSpans := make(map[SpanID]bool, workers*each)
	for w := range workers {
		for i := range each {
			if seenTraces[traces[w][i]] || seenSpans[spans[w][i]] {
				t.Fatalf("an identifier was drawn twice: %x %x", traces[w][i], spans[w][i])
			}
			seenTraces[traces[w][i]], seenSpans[spans[w][i]] = true, true
		}
	}
}

// TestDrawingIDsDoesNotAllocate checks that the identifiers a traced request
// draws cost no allocation of their own.
func TestDrawingIDsDoesNotAllocate(t *testing.T) {
	if allocs := testing.AllocsPerRun(100, func() {
		_ = newTraceID()
		_ = newSpanID()
	}); allocs != 0 {
		t.Errorf("drawing identifiers allocates %v times", allocs)
	}
}

func TestSpanContextAccessors(t *testing.T) {
	t.Parallel()
	var zero SpanContext
	if zero.IsValid() || zero.IsSampled() {
		t.Error("the zero SpanContext reports itself valid or sampled")
	}
	if (TraceID{}).IsValid() || (SpanID{}).IsValid() {
		t.Error("a zero identifier reports itself valid")
	}
	sc := SpanContext{TraceID: TraceID{1}, SpanID: SpanID{2}}
	if !sc.IsValid() || sc.IsSampled() {
		t.Errorf("valid = %v sampled = %v, want true and false", sc.IsValid(), sc.IsSampled())
	}
	if (SpanContext{TraceID: TraceID{1}}).IsValid() || (SpanContext{SpanID: SpanID{1}}).IsValid() {
		t.Error("a span context missing one identifier reports itself valid")
	}
	if got := (TraceID{0xab}).String(); got != "ab000000000000000000000000000000" {
		t.Errorf("TraceID.String() = %q", got)
	}
	if got := (SpanID{0xcd, 0x01}).String(); got != "cd01000000000000" {
		t.Errorf("SpanID.String() = %q", got)
	}
}

func TestSpanContextFromContextAndInject(t *testing.T) {
	t.Parallel()
	t.Run("a context with no span injects nothing", func(t *testing.T) {
		t.Parallel()
		if sc, ok := SpanContextFromContext(context.Background()); ok {
			t.Fatalf("SpanContextFromContext = %+v, want nothing", sc)
		}
		h := http.Header{"Traceparent": {"left alone"}}
		InjectTraceContext(context.Background(), h)
		InjectTraceContext(context.Background(), nil)
		if got := h.Get("Traceparent"); got != "left alone" {
			t.Errorf("an existing traceparent became %q", got)
		}
	})
	sc, _ := parseTraceparent(testParent)
	sc.Remote = false
	t.Run("a sampled span with state", func(t *testing.T) {
		t.Parallel()
		withState := sc
		withState.TraceState = "rojo=1"
		ctx := context.WithValue(context.Background(), spanContextKey{}, &activeSpan{sc: withState})
		got, ok := SpanContextFromContext(ctx)
		if !ok || got != withState {
			t.Fatalf("SpanContextFromContext = %+v, %v", got, ok)
		}
		h := http.Header{}
		InjectTraceContext(ctx, h)
		InjectTraceContext(ctx, nil)
		if h.Get("traceparent") != testParent || h.Get("tracestate") != "rojo=1" {
			t.Errorf("injected %v", h)
		}
	})
	t.Run("a stale tracestate is removed", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(context.Background(), spanContextKey{}, &activeSpan{sc: sc})
		h := http.Header{"Tracestate": {"stale=1"}}
		InjectTraceContext(ctx, h)
		if _, present := h["Tracestate"]; present {
			t.Errorf("a tracestate belonging to another trace survived: %v", h)
		}
	})
	t.Run("an unsampled span propagates the decision", func(t *testing.T) {
		t.Parallel()
		unsampled := sc
		unsampled.TraceFlags = 0
		ctx := context.WithValue(context.Background(), spanContextKey{}, &activeSpan{sc: unsampled})
		h := http.Header{}
		InjectTraceContext(ctx, h)
		if got := h.Get("traceparent"); !strings.HasSuffix(got, "-00") {
			t.Errorf("traceparent = %q, want the unsampled flag", got)
		}
	})
}
