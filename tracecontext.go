package muzak

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// The headers W3C Trace Context carries a trace in, spelled as the
// recommendation spells them. net/http matches header names without regard to
// case, so either spelling reads and writes the same header.
const (
	// HeaderTraceparent carries the trace a request belongs to, the span that
	// sent it and whether the trace is being recorded.
	HeaderTraceparent = "traceparent"
	// HeaderTracestate carries vendor-specific state that travels with the
	// trace, and is only meaningful beside the traceparent it arrived with.
	HeaderTracestate = "tracestate"
)

// Log attribute keys under which a request's trace is recorded when tracing is
// configured: the access log and every record written through
// [Context.Logger] carry both, which is what joins a log line to the span of
// the same request in whatever backend receives them.
const (
	// TraceIDKey is the attribute key holding the trace identifier, as 32
	// lowercase hex digits.
	TraceIDKey = "trace_id"
	// SpanIDKey is the attribute key holding the server span's identifier,
	// as 16 lowercase hex digits.
	SpanIDKey = "span_id"
)

// Bounds on what an inbound tracestate may hold. The recommendation asks a
// vendor to accept at least 32 members and to propagate at least 512 bytes of
// them; a header past either bound is dropped as a whole rather than
// truncated, because which members an upstream vendor can do without is not
// something this server can know.
const (
	maxTracestateMembers = 32
	maxTracestateBytes   = 512
	// maxTracestateInput bounds how much of the raw header is looked at. The
	// optional whitespace and empty members the format allows mean a header
	// can be longer than what it holds, but not by much in practice, and a
	// client sending kilobytes of it is answered without the server reading
	// them.
	maxTracestateInput = 4 * maxTracestateBytes
)

// traceparentLength is the length of a version 00 traceparent, which is also
// the shortest a header of any version may be.
const traceparentLength = 55

// TraceID identifies a trace: every span of one request, across every service
// it reaches, shares it. The zero value is not a valid identifier.
type TraceID [16]byte

// IsValid reports whether the identifier is not all zeroes, which the
// recommendation reserves to mean "no trace".
func (t TraceID) IsValid() bool { return t != TraceID{} }

// String returns the identifier as 32 lowercase hex digits, the form it takes
// in a traceparent header and in a log line.
func (t TraceID) String() string {
	var buf [32]byte
	hex.Encode(buf[:], t[:])
	return string(buf[:])
}

// SpanID identifies one span within a trace. The zero value is not a valid
// identifier.
type SpanID [8]byte

// IsValid reports whether the identifier is not all zeroes.
func (s SpanID) IsValid() bool { return s != SpanID{} }

// String returns the identifier as 16 lowercase hex digits.
func (s SpanID) String() string {
	var buf [16]byte
	hex.Encode(buf[:], s[:])
	return string(buf[:])
}

// TraceFlags carries the trace-flags field of a traceparent.
type TraceFlags byte

// TraceFlagsSampled is the flag a caller sets to say that it is recording the
// trace, which asks every service after it to record its part too.
const TraceFlagsSampled TraceFlags = 0x01

// SpanContext is the part of a span that crosses a process boundary: which
// trace it belongs to, which span it is, whether the trace is being recorded,
// and the vendor state travelling with it.
//
// Read the current one with [SpanContextFromContext], and send it to the next
// service with [InjectTraceContext].
type SpanContext struct {
	// TraceID is the trace the span belongs to.
	TraceID TraceID
	// SpanID is the span itself.
	SpanID SpanID
	// TraceFlags records whether the trace is sampled. A span this server
	// starts carries only [TraceFlagsSampled]; a parent read from a header
	// keeps whatever flags it was sent with.
	TraceFlags TraceFlags
	// TraceState is the validated tracestate the trace arrived with, members
	// joined by commas without whitespace, or empty when there was none or it
	// was not valid.
	TraceState string
	// Remote reports that the span context was read from a request rather
	// than started in this process, which is what a parent from a traceparent
	// header is.
	Remote bool
}

// IsValid reports whether both identifiers are set.
func (sc SpanContext) IsValid() bool { return sc.TraceID.IsValid() && sc.SpanID.IsValid() }

// IsSampled reports whether the trace is being recorded.
func (sc SpanContext) IsSampled() bool { return sc.TraceFlags&TraceFlagsSampled != 0 }

// Traceparent renders the span context as a version 00 traceparent header
// value, or returns the empty string when it is not valid. A parent read from
// a header of a later version is written back as version 00, which is the
// version this implementation speaks, as the recommendation asks.
func (sc SpanContext) Traceparent() string {
	if !sc.IsValid() {
		return ""
	}
	var buf [traceparentLength]byte
	buf[0], buf[1], buf[2] = '0', '0', '-'
	hex.Encode(buf[3:35], sc.TraceID[:])
	buf[35] = '-'
	hex.Encode(buf[36:52], sc.SpanID[:])
	buf[52] = '-'
	hex.Encode(buf[53:55], []byte{byte(sc.TraceFlags)})
	return string(buf[:])
}

// newTraceID draws a trace identifier from the operating system's
// cryptographically secure generator.
//
// A predictable identifier would let anyone who can see one trace guess the
// next, and attach spans of their own to it or correlate traffic they did not
// send; drawing from crypto/rand rules that out. An all-zero draw, which is
// invalid and has a probability of 2^-128, is drawn again.
func newTraceID() TraceID {
	var id TraceID
	for !id.IsValid() {
		drawID(id[:])
	}
	return id
}

// newSpanID draws a span identifier the same way [newTraceID] does.
func newSpanID() SpanID {
	var id SpanID
	for !id.IsValid() {
		drawID(id[:])
	}
	return id
}

// idSource holds bytes drawn from crypto/rand for the identifiers still to be
// handed out, and left counts those not yet handed out, from the end of buf.
//
// A request that is traced draws two identifiers. Drawing each from
// crypto/rand directly cost a call into the operating system apiece and,
// under the race detector, moved every identifier to the heap, since the
// instrumented crypto/rand.Read holds on to the slice it fills as far as the
// compiler can tell. Copying identifiers out of a buffer that lives as long
// as the program costs neither, and one draw serves thirty-two traces.
var idSource struct {
	mu   sync.Mutex
	buf  [512]byte
	left int
}

// drawID fills dst, which is at most 512 bytes, with bytes from crypto/rand
// that no other identifier was given.
func drawID(dst []byte) {
	idSource.mu.Lock()
	defer idSource.mu.Unlock()
	if idSource.left < len(dst) {
		// crypto/rand.Read never returns an error: on the platforms Go
		// supports it either fills the buffer or ends the program.
		_, _ = rand.Read(idSource.buf[:])
		idSource.left = len(idSource.buf)
	}
	start := len(idSource.buf) - idSource.left
	copy(dst, idSource.buf[start:start+len(dst)])
	idSource.left -= len(dst)
}

// extractTraceContext reads the trace a request says it belongs to.
//
// A request carrying more than one traceparent is refused, because there is no
// way to tell which of them is the caller's. The tracestate is only read once
// the traceparent has been accepted, as the recommendation requires, and one
// that is not valid is dropped without dropping the parent with it.
func extractTraceContext(h http.Header) (SpanContext, bool) {
	// Indexed with the canonical spelling rather than through Values, which
	// canonicalizes its argument on every call.
	values := h["Traceparent"]
	if len(values) != 1 {
		return SpanContext{}, false
	}
	sc, ok := parseTraceparent(values[0])
	if !ok {
		return SpanContext{}, false
	}
	if state, valid := parseTracestate(h["Tracestate"]); valid {
		sc.TraceState = state
	}
	return sc, true
}

// parseTraceparent reads one traceparent header value.
//
// Version 00 is read exactly: 55 characters of lowercase hex and dashes in the
// positions the recommendation gives. A later version is read the way the
// recommendation says to read one this implementation does not know: its first
// 55 characters are read as version 00, and anything after them has to start a
// new field with a dash. Version ff is invalid, and so is an identifier of all
// zeroes. The work is constant: nothing past the 56th byte is looked at.
func parseTraceparent(s string) (SpanContext, bool) {
	if len(s) < traceparentLength {
		return SpanContext{}, false
	}
	version, ok := lowerHexByte(s[0], s[1])
	if !ok || version == 0xff {
		return SpanContext{}, false
	}
	if s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return SpanContext{}, false
	}
	if len(s) > traceparentLength && (version == 0 || s[traceparentLength] != '-') {
		return SpanContext{}, false
	}
	var sc SpanContext
	if !decodeLowerHex(sc.TraceID[:], s[3:35]) || !decodeLowerHex(sc.SpanID[:], s[36:52]) {
		return SpanContext{}, false
	}
	flags, ok := lowerHexByte(s[53], s[54])
	if !ok || !sc.IsValid() {
		return SpanContext{}, false
	}
	sc.TraceFlags = TraceFlags(flags)
	sc.Remote = true
	return sc, true
}

// decodeLowerHex decodes src into dst, which is exactly half its length,
// refusing anything but lowercase hex. encoding/hex accepts uppercase too,
// which the recommendation does not.
func decodeLowerHex(dst []byte, src string) bool {
	for i := range dst {
		b, ok := lowerHexByte(src[2*i], src[2*i+1])
		if !ok {
			return false
		}
		dst[i] = b
	}
	return true
}

// lowerHexByte decodes two lowercase hex digits.
func lowerHexByte(hi, lo byte) (byte, bool) {
	h, okHi := lowerHexDigit(hi)
	l, okLo := lowerHexDigit(lo)
	return h<<4 | l, okHi && okLo
}

// lowerHexDigit decodes one lowercase hex digit.
func lowerHexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// parseTracestate validates the tracestate header values a request carried and
// returns them as one list without whitespace, ready to propagate.
//
// Several headers are one list, as HTTP says repeated fields are. Empty members
// and the optional whitespace around commas are dropped. A header that breaks
// any rule is refused as a whole, as the recommendation requires: more than
// [maxTracestateMembers] members, a key or a value outside the grammar, a key
// that appears twice, or a list longer than [maxTracestateBytes] once written
// without whitespace. An absent or empty header is valid and holds nothing.
//
// The work is linear in the input, which is refused unread past
// [maxTracestateInput]; the duplicate check compares at most 32 keys with each
// other.
func parseTracestate(values []string) (string, bool) {
	total := 0
	for _, value := range values {
		total += len(value) + 1
		if total > maxTracestateInput {
			return "", false
		}
	}
	var buf [maxTracestateBytes]byte
	var keys [maxTracestateMembers]string
	out, members := buf[:0], 0
	for _, value := range values {
		for value != "" {
			var member string
			member, value, _ = strings.Cut(value, ",")
			member = strings.Trim(member, " \t")
			if member == "" {
				continue
			}
			key, val, found := strings.Cut(member, "=")
			if !found || members == maxTracestateMembers || !validTracestateKey(key) || !validTracestateValue(val) {
				return "", false
			}
			for _, seen := range keys[:members] {
				if seen == key {
					return "", false
				}
			}
			keys[members] = key
			members++
			separator := 0
			if len(out) > 0 {
				separator = 1
			}
			if len(out)+separator+len(member) > maxTracestateBytes {
				return "", false
			}
			if separator == 1 {
				out = append(out, ',')
			}
			out = append(out, member...)
		}
	}
	return string(out), true
}

// validTracestateKey reports whether key is a tracestate key: a simple key of a
// lowercase letter and up to 255 more key characters, or a tenant of up to 241
// and a system of up to 14 joined by "@", the tenant starting with a lowercase
// letter or a digit and the system with a lowercase letter.
func validTracestateKey(key string) bool {
	tenant, system, multiTenant := strings.Cut(key, "@")
	if !multiTenant {
		return len(key) <= 256 && key != "" && isTracestateLower(key[0]) && allTracestateKeyChars(key[1:])
	}
	return tenant != "" && len(tenant) <= 241 && (isTracestateLower(tenant[0]) || isTracestateDigit(tenant[0])) && allTracestateKeyChars(tenant[1:]) &&
		system != "" && len(system) <= 14 && isTracestateLower(system[0]) && allTracestateKeyChars(system[1:])
}

// allTracestateKeyChars reports whether s holds only the characters a tracestate key may
// continue with.
func allTracestateKeyChars(s string) bool {
	for i := range len(s) {
		c := s[i]
		if !isTracestateLower(c) && !isTracestateDigit(c) && c != '_' && c != '-' && c != '*' && c != '/' {
			return false
		}
	}
	return true
}

// validTracestateValue reports whether value is a tracestate value: one to 256
// printable ASCII characters other than "," and "=", not ending in a space.
func validTracestateValue(value string) bool {
	if value == "" || len(value) > 256 || value[len(value)-1] == ' ' {
		return false
	}
	for i := range len(value) {
		if c := value[i]; c < ' ' || c > '~' || c == ',' || c == '=' {
			return false
		}
	}
	return true
}

func isTracestateLower(c byte) bool { return c >= 'a' && c <= 'z' }

func isTracestateDigit(c byte) bool { return c >= '0' && c <= '9' }

// spanContextKey is the context key the request's active span is stored under.
type spanContextKey struct{}

// activeSpanFrom returns the span ctx carries, or nil.
func activeSpanFrom(ctx context.Context) *activeSpan {
	active, _ := ctx.Value(spanContextKey{}).(*activeSpan)
	return active
}

// SpanContextFromContext returns the span context of the span ctx carries,
// which inside a handler is the request's server span or a child of it
// started with [StartSpan], and reports whether there is one. There never is
// when [AppOptions.Tracing] has no Tracer.
//
// Use it to record the trace somewhere that is not a log line or an outbound
// request, such as a row written to an audit table or a message published to
// a queue.
func SpanContextFromContext(ctx context.Context) (SpanContext, bool) {
	if active := activeSpanFrom(ctx); active != nil {
		return active.sc, true
	}
	return SpanContext{}, false
}

// InjectTraceContext writes the trace ctx carries into the headers of an
// outbound request, so that the service it reaches records its part of the
// work as a child of this one:
//
//	req, _ := http.NewRequestWithContext(ctx.Context(), http.MethodGet, url, nil)
//	muzak.InjectTraceContext(req.Context(), req.Header)
//
// The traceparent names the current span as the parent and carries its
// sampling decision, so a trace this service is not recording is not recorded
// downstream either. The tracestate travels with it, and a tracestate already
// in the headers is removed when the trace has none, because it would belong
// to a different trace. A context carrying no span, which is every context
// when tracing is not configured, leaves the headers as they are.
func InjectTraceContext(ctx context.Context, header http.Header) {
	active := activeSpanFrom(ctx)
	if active == nil || header == nil || !active.sc.IsValid() {
		return
	}
	header.Set(HeaderTraceparent, active.sc.Traceparent())
	if active.sc.TraceState != "" {
		header.Set(HeaderTracestate, active.sc.TraceState)
	} else {
		header.Del(HeaderTracestate)
	}
}
