package otlp

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"io"
	"log/slog"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	muzak "muzak.dev/framework"
)

// scopeName is the instrumentation scope every span is reported under, which
// is the library that recorded it.
const scopeName = "muzak.dev/framework"

// frameworkVersion is the version of the framework built into the program,
// read once.
var frameworkVersion = sync.OnceValue(func() string { return moduleVersion(debug.ReadBuildInfo()) })

// moduleVersion finds the framework among a program's dependencies, and
// returns nothing when it is not one, as in the framework's own tests, or when
// the program carries no build information.
func moduleVersion(info *debug.BuildInfo, ok bool) string {
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path == scopeName {
			return dep.Version
		}
	}
	return ""
}

// encode writes a batch as an OTLP/HTTP JSON ExportTraceServiceRequest.
//
// The encoding is protobuf's JSON mapping with OTLP's own exceptions: trace
// and span identifiers are lowercase hex rather than base64, field names are
// lowerCamelCase, enums are their integer values, 64-bit integers, timestamps
// among them, are decimal strings, and a field holding its default value is
// left out. Invalid UTF-8 in a string is written as U+FFFD rather than
// refused, so that one bad attribute cannot cost a whole batch.
func (e *Exporter) encode(out io.Writer, batch []*span) error {
	w := &jsonWriter{enc: jsontext.NewEncoder(out, jsontext.AllowInvalidUTF8(true))}
	w.begin()
	w.key("resourceSpans")
	w.beginArray()
	w.begin()
	w.key("resource")
	w.begin()
	w.key("attributes")
	w.keyValues(e.resource)
	w.end()
	w.key("scopeSpans")
	w.beginArray()
	w.begin()
	w.key("scope")
	w.begin()
	w.field("name", scopeName)
	if e.scopeVersion != "" {
		w.field("version", e.scopeVersion)
	}
	w.end()
	w.key("spans")
	w.beginArray()
	for _, s := range batch {
		w.span(s)
	}
	w.endArray()
	w.end()
	w.endArray()
	w.end()
	w.endArray()
	w.end()
	return w.err
}

// buildResource returns the resource attributes: the configured ones, then
// service.name and service.version, which replace any configured attribute
// with the same key, then the SDK's own.
func buildResource(cfg config) []keyValue {
	attrs := make([]slog.Attr, 0, len(cfg.resource)+4)
	for _, a := range cfg.resource {
		if a.Key != "service.name" && (cfg.serviceVersion == "" || a.Key != "service.version") {
			attrs = append(attrs, a)
		}
	}
	attrs = append(attrs, slog.String("service.name", cfg.serviceName))
	if cfg.serviceVersion != "" {
		attrs = append(attrs, slog.String("service.version", cfg.serviceVersion))
	}
	attrs = append(attrs, slog.String("telemetry.sdk.name", "muzak"), slog.String("telemetry.sdk.language", "go"))
	out := make([]keyValue, 0, len(attrs))
	for _, a := range attrs {
		if kv, ok := convertAttr(a, 0); ok {
			out = append(out, kv)
		}
	}
	return out
}

// jsonWriter writes tokens and keeps the first error, so that encoding reads
// as the document it produces rather than as a check after every token.
type jsonWriter struct {
	enc *jsontext.Encoder
	err error
}

func (w *jsonWriter) token(t jsontext.Token) {
	if w.err == nil {
		w.err = w.enc.WriteToken(t)
	}
}

func (w *jsonWriter) begin()                   { w.token(jsontext.BeginObject) }
func (w *jsonWriter) end()                     { w.token(jsontext.EndObject) }
func (w *jsonWriter) beginArray()              { w.token(jsontext.BeginArray) }
func (w *jsonWriter) endArray()                { w.token(jsontext.EndArray) }
func (w *jsonWriter) key(name string)          { w.token(jsontext.String(name)) }
func (w *jsonWriter) field(name, value string) { w.key(name); w.token(jsontext.String(value)) }

// int64Field writes a 64-bit integer the way the JSON encoding of protobuf
// does, as a decimal string.
func (w *jsonWriter) int64Field(name string, v int64) { w.field(name, strconv.FormatInt(v, 10)) }

// timeField writes a timestamp as nanoseconds since the epoch.
func (w *jsonWriter) timeField(name string, t time.Time) { w.int64Field(name, t.UnixNano()) }

// span writes one span.
func (w *jsonWriter) span(s *span) {
	w.begin()
	w.field("traceId", s.traceID.String())
	w.field("spanId", s.spanID.String())
	if s.traceState != "" {
		w.field("traceState", s.traceState)
	}
	if s.parentID.IsValid() {
		w.field("parentSpanId", s.parentID.String())
	}
	w.key("flags")
	w.token(jsontext.Uint(uint64(s.flags)))
	w.field("name", s.name)
	w.key("kind")
	w.token(jsontext.Int(int64(spanKind(s.kind))))
	w.timeField("startTimeUnixNano", s.start)
	w.timeField("endTimeUnixNano", s.end)
	if len(s.attrs) > 0 {
		w.key("attributes")
		w.keyValues(s.attrs)
	}
	if s.droppedAttrs > 0 {
		w.key("droppedAttributesCount")
		w.token(jsontext.Int(int64(s.droppedAttrs)))
	}
	if len(s.events) > 0 {
		w.key("events")
		w.beginArray()
		for _, e := range s.events {
			w.event(e)
		}
		w.endArray()
	}
	if s.droppedEvents > 0 {
		w.key("droppedEventsCount")
		w.token(jsontext.Int(int64(s.droppedEvents)))
	}
	if s.status != muzak.SpanStatusUnset {
		w.key("status")
		w.begin()
		if s.statusMessage != "" {
			w.field("message", s.statusMessage)
		}
		w.key("code")
		w.token(jsontext.Int(int64(s.status)))
		w.end()
	}
	w.end()
}

// spanKind returns a kind OTLP defines, writing one it does not as
// unspecified rather than sending a value a collector would refuse.
func spanKind(kind muzak.SpanKind) muzak.SpanKind {
	if kind < muzak.SpanKindInternal || kind > muzak.SpanKindConsumer {
		return 0
	}
	return kind
}

// event writes one span event.
func (w *jsonWriter) event(e event) {
	w.begin()
	w.timeField("timeUnixNano", e.time)
	w.field("name", e.name)
	if len(e.attrs) > 0 {
		w.key("attributes")
		w.keyValues(e.attrs)
	}
	if e.dropped > 0 {
		w.key("droppedAttributesCount")
		w.token(jsontext.Int(int64(e.dropped)))
	}
	w.end()
}

// keyValues writes a list of attributes.
func (w *jsonWriter) keyValues(kvs []keyValue) {
	w.beginArray()
	for _, kv := range kvs {
		w.begin()
		w.field("key", kv.key)
		w.key("value")
		w.anyValue(kv.value)
		w.end()
	}
	w.endArray()
}

// anyValue writes an OTLP AnyValue, which sets exactly one of its fields, or
// none for an empty value.
func (w *jsonWriter) anyValue(v value) {
	w.begin()
	switch v.kind {
	case kindString:
		w.field("stringValue", v.str)
	case kindBool:
		w.key("boolValue")
		w.token(jsontext.Bool(v.flag))
	case kindInt:
		w.int64Field("intValue", v.num)
	case kindDouble:
		// NaN and the infinities are written as the strings the JSON
		// encoding of protobuf uses for them.
		w.key("doubleValue")
		w.token(jsontext.Float(v.float))
	case kindBytes:
		w.field("bytesValue", base64.StdEncoding.EncodeToString(v.bytes))
	case kindArray:
		w.key("arrayValue")
		w.begin()
		w.key("values")
		w.beginArray()
		for _, element := range v.list {
			w.anyValue(element)
		}
		w.endArray()
		w.end()
	case kindKVList:
		w.key("kvlistValue")
		w.begin()
		w.key("values")
		w.keyValues(v.kvs)
		w.end()
	case kindEmpty:
	}
	w.end()
}
