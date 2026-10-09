package otlp

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	muzak "muzak.dev/framework"
)

// Bounds on what one span may hold. A span is application code's to fill, and
// a loop that adds an attribute per row would otherwise grow one without limit
// and every span waiting in the queue with it. What does not fit is dropped
// and counted, and the counts are exported with the span, which is how
// OpenTelemetry reports a limit being reached.
const (
	// maxAttributes bounds the attributes of a span, and of each event.
	maxAttributes = 128
	// maxEvents bounds the events of a span.
	maxEvents = 128
	// maxStringLength bounds a string value, a name or a key, in bytes.
	maxStringLength = 4096
	// maxListLength bounds the elements of an array or group value.
	maxListLength = 128
	// maxValueDepth bounds how deeply groups and arrays may nest.
	maxValueDepth = 4
	// maxSpanBytes bounds the estimated size of everything a span records,
	// so that the queue's memory is bounded by QueueSize times this.
	maxSpanBytes = 64 << 10
)

// valueKind is which of the OTLP AnyValue fields a value fills.
type valueKind uint8

const (
	kindEmpty valueKind = iota
	kindString
	kindBool
	kindInt
	kindDouble
	kindBytes
	kindArray
	kindKVList
)

// value is an attribute value converted at the moment it was recorded, so
// that nothing the application passed is read again on the exporter's
// goroutine, where the application may be changing it.
type value struct {
	kind  valueKind
	str   string
	num   int64
	float float64
	flag  bool
	bytes []byte
	list  []value
	kvs   []keyValue
}

// keyValue is one converted attribute, with its estimated size.
type keyValue struct {
	key   string
	value value
	size  int
}

// event is one span event.
type event struct {
	name    string
	time    time.Time
	attrs   []keyValue
	dropped int
}

// span is a span being recorded. Everything is set under mu until End; from
// then on it belongs to the exporter, which reads it without the lock, and
// every method is a no-op.
type span struct {
	exporter *Exporter

	traceID    muzak.TraceID
	spanID     muzak.SpanID
	parentID   muzak.SpanID
	traceState string
	flags      uint32
	kind       muzak.SpanKind
	start      time.Time

	mu            sync.Mutex
	ended         bool
	end           time.Time
	name          string
	attrs         []keyValue
	droppedAttrs  int
	events        []event
	droppedEvents int
	status        muzak.SpanStatusCode
	statusMessage string
	size          int
}

// The flags OTLP defines on a span beside the trace flags, saying whether its
// parent is known to be remote.
const (
	flagHasIsRemote = 0x100
	flagIsRemote    = 0x200
)

// newSpan records the start of a span.
func newSpan(e *Exporter, start muzak.SpanStart) *span {
	s := &span{
		exporter:   e,
		traceID:    start.SpanContext.TraceID,
		spanID:     start.SpanContext.SpanID,
		parentID:   start.Parent.SpanID,
		traceState: start.SpanContext.TraceState,
		flags:      uint32(start.SpanContext.TraceFlags) | flagHasIsRemote,
		kind:       start.Kind,
		start:      start.StartTime,
		name:       truncate(start.Name, maxStringLength),
	}
	if start.Parent.Remote {
		s.flags |= flagIsRemote
	}
	if s.start.IsZero() {
		s.start = time.Now()
	}
	for _, a := range start.Attributes {
		s.setAttr(a)
	}
	return s
}

// SetName replaces the span's name.
func (s *span) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.name = truncate(name, maxStringLength)
	}
}

// SetAttributes records attributes, replacing any with the same key.
func (s *span) SetAttributes(attrs ...slog.Attr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for _, a := range attrs {
		s.setAttr(a)
	}
}

// setAttr records one attribute, within the span's bounds. The caller holds
// mu, or is the only one with the span. Finding an existing key is a scan of
// at most [maxAttributes] entries.
func (s *span) setAttr(a slog.Attr) {
	kv, ok := convertAttr(a, 0)
	if !ok {
		return
	}
	for i := range s.attrs {
		if s.attrs[i].key == kv.key {
			if s.size-s.attrs[i].size+kv.size > maxSpanBytes {
				s.droppedAttrs++
				return
			}
			s.size += kv.size - s.attrs[i].size
			s.attrs[i] = kv
			return
		}
	}
	if len(s.attrs) >= maxAttributes || s.size+kv.size > maxSpanBytes {
		s.droppedAttrs++
		return
	}
	s.size += kv.size
	s.attrs = append(s.attrs, kv)
}

// AddEvent records an event at the current time.
func (s *span) AddEvent(name string, attrs ...slog.Attr) {
	at := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	e := event{name: truncate(name, maxStringLength), time: at}
	size := len(e.name) + 16
	for _, a := range attrs {
		kv, ok := convertAttr(a, 0)
		if !ok {
			continue
		}
		if len(e.attrs) >= maxAttributes || s.size+size+kv.size > maxSpanBytes {
			e.dropped++
			continue
		}
		size += kv.size
		e.attrs = append(e.attrs, kv)
	}
	if len(s.events) >= maxEvents || s.size+size > maxSpanBytes {
		s.droppedEvents++
		return
	}
	s.size += size
	s.events = append(s.events, e)
}

// SetStatus records the span's outcome, the way OpenTelemetry defines it: an
// unset status changes nothing, OK is final, and a description is kept only
// for an error.
func (s *span) SetStatus(code muzak.SpanStatusCode, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || code == muzak.SpanStatusUnset || s.status == muzak.SpanStatusOK {
		return
	}
	if code != muzak.SpanStatusOK && code != muzak.SpanStatusError {
		return
	}
	s.status = code
	s.statusMessage = ""
	if code == muzak.SpanStatusError {
		s.statusMessage = truncate(description, maxStringLength)
	}
}

// End completes the span and hands it to the exporter. Only the first call
// counts.
func (s *span) End() {
	at := time.Now()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.end = at
	s.mu.Unlock()
	s.exporter.enqueue(s)
}

// convertAttr converts an attribute, reporting false for one with no key,
// which OTLP has no way to carry.
func convertAttr(a slog.Attr, depth int) (keyValue, bool) {
	if a.Key == "" {
		return keyValue{}, false
	}
	key := truncate(a.Key, maxStringLength)
	v, size := convertValue(a.Value, depth)
	return keyValue{key: key, value: v, size: len(key) + size}, true
}

// convertValue converts an attribute value into one OTLP can carry, returning
// it with its estimated size. A string is cut to [maxStringLength], a list to
// [maxListLength], and anything nested past [maxValueDepth] is replaced with
// a string saying so.
func convertValue(v slog.Value, depth int) (value, int) {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		s := truncate(v.String(), maxStringLength)
		return value{kind: kindString, str: s}, len(s)
	case slog.KindInt64:
		return value{kind: kindInt, num: v.Int64()}, 8
	case slog.KindUint64:
		if u := v.Uint64(); u <= math.MaxInt64 {
			return value{kind: kindInt, num: int64(u)}, 8
		}
		// Past what OTLP's signed integer holds, so carried exactly as
		// text rather than wrapped round to a negative number.
		return value{kind: kindString, str: strconv.FormatUint(v.Uint64(), 10)}, 20
	case slog.KindFloat64:
		return value{kind: kindDouble, float: v.Float64()}, 8
	case slog.KindBool:
		return value{kind: kindBool, flag: v.Bool()}, 1
	case slog.KindDuration:
		return value{kind: kindInt, num: int64(v.Duration())}, 8
	case slog.KindTime:
		s := v.Time().Format(time.RFC3339Nano)
		return value{kind: kindString, str: s}, len(s)
	case slog.KindGroup:
		if depth >= maxValueDepth {
			return tooDeep()
		}
		out := value{kind: kindKVList}
		size := 0
		for _, member := range v.Group() {
			if len(out.kvs) >= maxListLength {
				break
			}
			if kv, ok := convertAttr(member, depth+1); ok {
				out.kvs = append(out.kvs, kv)
				size += kv.size
			}
		}
		return out, size
	default:
		return convertAny(v.Any(), depth)
	}
}

// convertAny converts a value slog holds as an interface: a slice of a basic
// type becomes an array, bytes stay bytes, an error or a Stringer becomes its
// text, and anything else is formatted with fmt.
func convertAny(a any, depth int) (value, int) {
	switch x := a.(type) {
	case nil:
		return value{kind: kindEmpty}, 0
	case []byte:
		b := x[:min(len(x), maxStringLength)]
		return value{kind: kindBytes, bytes: append([]byte(nil), b...)}, len(b)
	case []string:
		return convertList(x, depth, slog.StringValue)
	case []int:
		return convertList(x, depth, slog.IntValue)
	case []int64:
		return convertList(x, depth, slog.Int64Value)
	case []float64:
		return convertList(x, depth, slog.Float64Value)
	case []bool:
		return convertList(x, depth, slog.BoolValue)
	case []any:
		return convertList(x, depth, slog.AnyValue)
	case error:
		s := truncate(x.Error(), maxStringLength)
		return value{kind: kindString, str: s}, len(s)
	case fmt.Stringer:
		s := truncate(x.String(), maxStringLength)
		return value{kind: kindString, str: s}, len(s)
	default:
		s := truncate(fmt.Sprint(x), maxStringLength)
		return value{kind: kindString, str: s}, len(s)
	}
}

// convertList converts a slice into an array value, one element at a time.
func convertList[T any](list []T, depth int, toValue func(T) slog.Value) (value, int) {
	if depth >= maxValueDepth {
		return tooDeep()
	}
	out := value{kind: kindArray}
	size := 0
	for _, element := range list[:min(len(list), maxListLength)] {
		v, n := convertValue(toValue(element), depth+1)
		out.list = append(out.list, v)
		size += n
	}
	return out, size
}

// tooDeep stands in for a value nested past [maxValueDepth].
func tooDeep() (value, int) {
	const text = "[nested too deeply]"
	return value{kind: kindString, str: text}, len(text)
}

// truncate cuts s to at most limit bytes, moving the cut back to the start of
// a UTF-8 sequence so that a valid string stays valid.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for i := 0; i < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); i++ {
		cut--
	}
	return s[:cut]
}
