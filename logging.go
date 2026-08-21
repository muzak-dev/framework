package badele

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ScopeKey is the attribute key that names the subsystem a log record came
// from. The console handler lifts it out of the attribute list and renders it
// as the bracketed column after the level, which is what produces lines like
//
//	14:32:07.492 INFO  [Server]        Listening on :8080
//
// Set it with [Scoped] rather than by hand.
const ScopeKey = "scope"

// Scopes used by the framework itself, so that application logs can be told
// apart from Badele's own at a glance.
const (
	// ScopeServer covers start-up, listening and shutdown.
	ScopeServer = "Server"
	// ScopeRouter covers route registration and the routing table.
	ScopeRouter = "Router"
	// ScopeRequest covers the per-request access log.
	ScopeRequest = "Request"
	// ScopeDocs covers the OpenAPI document and the documentation UI.
	ScopeDocs = "Docs"
)

// Scoped returns a logger that tags every record with the given scope name.
//
// Give each subsystem its own scope so its lines line up in the console and
// can be filtered in production:
//
//	log := badele.Scoped(app.Logger(), "UsersService")
//	log.Info("user created", "user_id", 42)
//
// It returns logger unchanged when logger is nil, so it is safe to call on an
// application that has not been built yet.
func Scoped(logger *slog.Logger, scope string) *slog.Logger {
	if logger == nil {
		return nil
	}
	return logger.With(slog.String(ScopeKey, scope))
}

// LogFormat selects how log records are rendered.
type LogFormat int

const (
	// LogFormatAuto writes the human-readable console format when the output
	// is an interactive terminal and JSON otherwise, which gives readable
	// local development and machine-parseable production logs with no
	// configuration.
	LogFormatAuto LogFormat = iota
	// LogFormatConsole always writes the aligned, optionally coloured console
	// format.
	LogFormatConsole
	// LogFormatJSON always writes one JSON object per record.
	LogFormatJSON
	// LogFormatNone discards every record. It is the fastest option and is
	// what tests use to keep output clean.
	LogFormatNone
)

// DefaultRedactedKeys lists the attribute keys whose values are replaced with
// [RedactedPlaceholder] before a record is written.
//
// Matching is case-insensitive and ignores '-' and '_', so "API-Key",
// "api_key" and "apikey" are all caught. The list exists because credentials
// reach logs by accident far more often than by design, most often through an
// attribute carrying a whole header map or request struct.
var DefaultRedactedKeys = []string{
	"authorization",
	"proxy-authorization",
	"cookie",
	"set-cookie",
	"password",
	"passwd",
	"secret",
	"token",
	"access-token",
	"refresh-token",
	"api-key",
	"apikey",
	"private-key",
	"client-secret",
	"session",
	"credentials",
}

// RedactedPlaceholder is written in place of a redacted value.
const RedactedPlaceholder = "[redacted]"

// LoggerOptions configures the logger returned by [NewLogger].
//
// The zero value is usable and yields an info-level logger that writes the
// console format to standard error when attached to a terminal and JSON
// otherwise, with [DefaultRedactedKeys] redacted.
type LoggerOptions struct {
	// Level is the minimum level to emit. It defaults to slog.LevelInfo. Pass
	// a *slog.LevelVar to be able to change it while the process runs.
	Level slog.Leveler
	// Format selects the rendering. It defaults to [LogFormatAuto].
	Format LogFormat
	// Output is where records are written. It defaults to os.Stderr, which
	// keeps logs out of a program's data output.
	Output io.Writer
	// Color forces ANSI colour on or off for the console format. When nil,
	// colour is used only if the output is a terminal and the NO_COLOR
	// environment variable is unset.
	Color *bool
	// TimeFormat is the layout used for timestamps in the console format. It
	// defaults to "15:04:05.000", which is compact enough to scan and precise
	// enough to order events within a request.
	TimeFormat string
	// ScopeWidth is the column width reserved for the bracketed scope,
	// defaulting to 16. Records with a longer scope push the message right
	// rather than being truncated.
	ScopeWidth int
	// AddSource records the source file and line of the call site. It costs a
	// stack walk per record, so it defaults to off.
	AddSource bool
	// RedactKeys replaces [DefaultRedactedKeys] when non-nil. Pass an empty,
	// non-nil slice to disable redaction, which is only appropriate when
	// nothing sensitive can reach the logger.
	RedactKeys []string
	// ShortRequestID truncates request identifiers to their first eight
	// characters in the console format, which keeps lines narrow while
	// remaining unambiguous in a development session. It never applies to
	// JSON output, where the full identifier is always written. It defaults
	// to true.
	ShortRequestID *bool
}

// NewLogger builds a *slog.Logger from the given options.
//
// The result is safe for concurrent use, redacts sensitive attributes, and
// performs no formatting work for records below its level.
func NewLogger(opts LoggerOptions) *slog.Logger {
	if opts.Output == nil {
		opts.Output = os.Stderr
	}
	if opts.Level == nil {
		opts.Level = slog.LevelInfo
	}
	if opts.TimeFormat == "" {
		opts.TimeFormat = "15:04:05.000"
	}
	if opts.ScopeWidth <= 0 {
		opts.ScopeWidth = 16
	}
	if opts.RedactKeys == nil {
		opts.RedactKeys = DefaultRedactedKeys
	}

	format := opts.Format
	if format == LogFormatAuto {
		format = LogFormatJSON
		if isTerminal(opts.Output) {
			format = LogFormatConsole
		}
	}

	redact := newRedactor(opts.RedactKeys)
	switch format {
	case LogFormatNone:
		return slog.New(discardHandler{})
	case LogFormatJSON:
		return slog.New(slog.NewJSONHandler(opts.Output, &slog.HandlerOptions{
			Level:       opts.Level,
			AddSource:   opts.AddSource,
			ReplaceAttr: redact.replaceAttr,
		}))
	default:
		return slog.New(newConsoleHandler(opts, redact))
	}
}

// isTerminal reports whether w is a character device, which is the closest the
// standard library gets to asking whether a human is going to read the output.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// redactor decides which attribute values must never be written.
type redactor struct {
	keys map[string]struct{}
}

// newRedactor normalises the configured keys once, so that the per-record
// check is a single map lookup on an already normalised key.
func newRedactor(keys []string) *redactor {
	r := &redactor{keys: make(map[string]struct{}, len(keys))}
	for _, k := range keys {
		r.keys[normalizeKey(k)] = struct{}{}
	}
	return r
}

// normalizeKey lower-cases a key and drops the separators that distinguish
// "api_key" from "API-Key", so that one entry covers every spelling.
func normalizeKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	for i := range len(k) {
		c := k[i]
		switch {
		case c == '-' || c == '_' || c == '.' || c == ' ':
			continue
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// shouldRedact reports whether an attribute with this key carries a secret.
func (r *redactor) shouldRedact(key string) bool {
	if len(r.keys) == 0 {
		return false
	}
	_, ok := r.keys[normalizeKey(key)]
	return ok
}

// replaceAttr is the slog.HandlerOptions hook that applies redaction to the
// JSON handler.
func (r *redactor) replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if r.shouldRedact(a.Key) {
		return slog.String(a.Key, RedactedPlaceholder)
	}
	return a
}

// discardHandler drops every record without formatting it.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

// ANSI escape sequences used by the console format.
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

// consoleHandler renders the aligned, human-readable format. It is the default
// when logs are going to a terminal.
//
// Records are formatted into a pooled buffer and written with a single Write
// call under a shared mutex, so lines from concurrent goroutines never
// interleave. Attributes supplied through WithAttrs are formatted once, at the
// time the child logger is built, rather than on every record.
type consoleHandler struct {
	opts     LoggerOptions
	redact   *redactor
	color    bool
	shortID  bool
	mu       *sync.Mutex
	out      io.Writer
	scope    string
	preAttrs []byte
	groups   []string
}

// newConsoleHandler resolves the colour decision once and returns a ready
// handler.
func newConsoleHandler(opts LoggerOptions, redact *redactor) *consoleHandler {
	color := isTerminal(opts.Output) && os.Getenv("NO_COLOR") == ""
	if opts.Color != nil {
		color = *opts.Color
	}
	shortID := true
	if opts.ShortRequestID != nil {
		shortID = *opts.ShortRequestID
	}
	return &consoleHandler{
		opts:    opts,
		redact:  redact,
		color:   color,
		shortID: shortID,
		mu:      new(sync.Mutex),
		out:     opts.Output,
	}
}

// Enabled implements slog.Handler.
func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

// WithAttrs implements slog.Handler, pre-rendering the attributes so that a
// scoped logger pays the formatting cost once instead of once per record.
func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := h.clone()
	for _, a := range attrs {
		if a.Key == ScopeKey && len(clone.groups) == 0 {
			clone.scope = a.Value.String()
			continue
		}
		clone.preAttrs = h.appendAttr(clone.preAttrs, a, clone.groups)
	}
	return clone
}

// WithGroup implements slog.Handler. Group names are prefixed onto the keys of
// every attribute added after the group is opened.
func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := h.clone()
	clone.groups = append(clone.groups, name)
	return clone
}

// clone copies the handler so that a derived logger cannot mutate the parent's
// pre-rendered state.
func (h *consoleHandler) clone() *consoleHandler {
	return &consoleHandler{
		opts:     h.opts,
		redact:   h.redact,
		color:    h.color,
		shortID:  h.shortID,
		mu:       h.mu,
		out:      h.out,
		scope:    h.scope,
		preAttrs: slices.Clip(h.preAttrs),
		groups:   slices.Clip(h.groups),
	}
}

// logBufferPool recycles the byte slices used to format records.
var logBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 512)
		return &b
	},
}

// Handle implements slog.Handler, rendering one aligned line per record.
func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	bufPtr := logBufferPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]
	defer func() {
		if cap(buf) <= 16<<10 {
			*bufPtr = buf
			logBufferPool.Put(bufPtr)
		}
	}()

	buf = h.appendTime(buf, r.Time)
	buf = append(buf, ' ')
	buf = h.appendLevel(buf, r.Level)
	buf = append(buf, ' ')
	buf = h.appendScope(buf)
	buf = append(buf, r.Message...)

	buf = append(buf, h.preAttrs...)
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, a, h.groups)
		return true
	})
	if h.opts.AddSource {
		buf = h.appendSource(buf, r)
	}
	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write(buf)
	return err
}

// appendTime writes the timestamp column, dimmed when colour is on.
func (h *consoleHandler) appendTime(buf []byte, t time.Time) []byte {
	if t.IsZero() {
		t = time.Now()
	}
	if h.color {
		buf = append(buf, ansiDim...)
	}
	buf = t.AppendFormat(buf, h.opts.TimeFormat)
	if h.color {
		buf = append(buf, ansiReset...)
	}
	return buf
}

// levelText returns the fixed-width name and colour for a level. Padding every
// name to five characters keeps the scope and message columns aligned no
// matter which levels appear in a given run.
func levelText(l slog.Level) (string, string) {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG", ansiBlue
	case l < slog.LevelWarn:
		return "INFO ", ansiGreen
	case l < slog.LevelError:
		return "WARN ", ansiYellow
	default:
		return "ERROR", ansiRed
	}
}

// appendLevel writes the level column.
func (h *consoleHandler) appendLevel(buf []byte, l slog.Level) []byte {
	text, color := levelText(l)
	if h.color {
		buf = append(buf, color...)
		buf = append(buf, text...)
		buf = append(buf, ansiReset...)
		return buf
	}
	return append(buf, text...)
}

// appendScope writes the bracketed scope column, padded so that messages line
// up. A record with no scope still consumes the column, which keeps framework
// and application lines in the same shape.
func (h *consoleHandler) appendScope(buf []byte) []byte {
	width := h.opts.ScopeWidth
	if h.scope == "" {
		return append(buf, strings.Repeat(" ", width)...)
	}
	if h.color {
		buf = append(buf, ansiCyan...)
		buf = append(buf, ansiBold...)
	}
	buf = append(buf, '[')
	buf = append(buf, h.scope...)
	buf = append(buf, ']')
	if h.color {
		buf = append(buf, ansiReset...)
	}
	// Two brackets plus the name; pad to the column width, always leaving at
	// least one space so a long scope does not run into the message.
	pad := width - (len(h.scope) + 2)
	if pad < 1 {
		pad = 1
	}
	return append(buf, strings.Repeat(" ", pad)...)
}

// appendAttr writes one " key=value" pair, applying redaction, group prefixes
// and quoting.
func (h *consoleHandler) appendAttr(buf []byte, a slog.Attr, groups []string) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return buf
		}
		nested := groups
		if a.Key != "" {
			nested = append(slices.Clip(groups), a.Key)
		}
		for _, nestedAttr := range attrs {
			buf = h.appendAttr(buf, nestedAttr, nested)
		}
		return buf
	}

	buf = append(buf, ' ', ' ')
	if h.color {
		buf = append(buf, ansiDim...)
	}
	for _, g := range groups {
		buf = append(buf, g...)
		buf = append(buf, '.')
	}
	buf = append(buf, a.Key...)
	buf = append(buf, '=')
	if h.color {
		buf = append(buf, ansiReset...)
	}
	return h.appendValue(buf, a.Key, a.Value)
}

// appendValue writes an attribute value, quoting it when it contains anything
// that would make the key=value pair ambiguous.
func (h *consoleHandler) appendValue(buf []byte, key string, v slog.Value) []byte {
	if h.redact.shouldRedact(key) {
		return append(buf, RedactedPlaceholder...)
	}
	switch v.Kind() {
	case slog.KindString:
		return appendMaybeQuoted(buf, h.maybeShorten(key, v.String()))
	case slog.KindInt64:
		return strconv.AppendInt(buf, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(buf, v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.AppendFloat(buf, v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.AppendBool(buf, v.Bool())
	case slog.KindDuration:
		return append(buf, v.Duration().String()...)
	case slog.KindTime:
		return v.Time().AppendFormat(buf, time.RFC3339Nano)
	default:
		return appendMaybeQuoted(buf, v.String())
	}
}

// maybeShorten trims a request identifier down to a readable prefix in the
// console format, where the full value adds width without adding meaning.
func (h *consoleHandler) maybeShorten(key, value string) string {
	if h.shortID && key == RequestIDKey && len(value) > 8 {
		return value[:8]
	}
	return value
}

// appendMaybeQuoted quotes a string only when it needs it, which keeps the
// common case free of escape noise.
func appendMaybeQuoted(buf []byte, s string) []byte {
	if s == "" {
		return append(buf, '"', '"')
	}
	if strings.ContainsAny(s, " \t\n\r\"=\\") {
		return strconv.AppendQuote(buf, s)
	}
	return append(buf, s...)
}

// appendSource writes the call site when [LoggerOptions.AddSource] is set.
func (h *consoleHandler) appendSource(buf []byte, r slog.Record) []byte {
	frames := r.Source()
	if frames == nil {
		return buf
	}
	buf = append(buf, ' ', ' ')
	if h.color {
		buf = append(buf, ansiDim...)
	}
	buf = append(buf, "source="...)
	buf = append(buf, trimSourcePath(frames.File)...)
	buf = append(buf, ':')
	buf = strconv.AppendInt(buf, int64(frames.Line), 10)
	if h.color {
		buf = append(buf, ansiReset...)
	}
	return buf
}

// trimSourcePath shortens a source path to its last two elements, which is
// enough to identify a file without filling the line with build paths.
func trimSourcePath(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		if j := strings.LastIndexByte(path[:i], '/'); j >= 0 {
			return path[j+1:]
		}
	}
	return path
}
