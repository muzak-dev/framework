package muzak

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
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
// apart from Muzak's own at a glance.
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
//	log := muzak.Scoped(app.Logger(), "UsersService")
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
	//
	// A container's stdout is not a terminal, so a service run under Docker or
	// Compose gets JSON even locally. That is the right default for a
	// production container and a surprise in front of `docker compose logs`;
	// name [LogFormatConsole] and set [LoggerOptions.Color] to read it there.
	LogFormatAuto LogFormat = iota
	// LogFormatConsole always writes the aligned, optionally coloured console
	// format.
	//
	// Colour still follows [LoggerOptions.Color], which defaults to a terminal
	// check, so a container wanting coloured output has to ask for both.
	LogFormatConsole
	// LogFormatJSON always writes one JSON object per record. Like the console
	// format it never writes a control character, C1 and DEL included, or a
	// bidirectional control as itself: they are written as \u escapes, which a
	// JSON reader decodes to the same text.
	LogFormatJSON
	// LogFormatNone discards every record. It is the fastest option and is
	// what tests use to keep output clean.
	LogFormatNone
)

// DefaultRedactedKeys lists the terms that mark an attribute key as carrying
// a secret, whose value is replaced with [RedactedPlaceholder] before a record
// is written.
//
// Both the key and each term are normalised first: lower-cased, with '-',
// '_', '.' and spaces removed. A key is redacted when its normalised form
// contains the normalised form of any term, so "token" catches "token",
// "access_token", "X-CSRF-Token" and "github_token" alike, "password" catches
// "db_password", and "api-key" catches "X-Api-Key". The list exists because
// credentials reach logs by accident far more often than by design, most
// often through an attribute carrying a whole header map, and such keys are
// spelled every way there is.
//
// The keys inside such a map are matched too. When an attribute's value is an
// [http.Header], a [url.Values], a map[string][]string or a map[string]string,
// each entry whose key matches has its value replaced, so
// slog.Any("headers", r.Header) is written with its Authorization and Cookie
// values hidden and every other header intact; the map the caller passed is
// left unchanged. Nothing else is looked inside: a struct, a pointer or a map
// of another type is written as the handler renders it, with only the
// attribute's own key checked, so log the fields that are needed rather than
// a whole request or configuration value.
//
// Matching by containment errs towards hiding: a key such as "token_count" or
// "session_count" is redacted too. That is the intended trade, since a count
// missing from a log costs far less than a credential written into one. The
// same rule applies to a list passed as [LoggerOptions.RedactKeys], so a short
// or common term there redacts every key that contains it.
//
// The same rule is why the list has no "auth", "pass", "sig" or "otp": each is
// inside a common word that is not a secret ("author", "passenger", "design",
// "hot_path"), and would hide it. The terms that stand for them are the
// specific spellings, "basic-auth", "auth-header", "x-auth", "passphrase",
// "totp" and "otp-code". A key that is a secret under a shorter name is one
// for [LoggerOptions.RedactKeys] to add.
//
// A key names a group as well as a single value. A group whose key matches,
// whether it was built with [slog.Group], produced by a [slog.LogValuer], or
// opened with [slog.Logger.WithGroup], has every value inside it redacted, in
// the JSON format and the console format alike.
var DefaultRedactedKeys = []string{
	"authorization",
	"cookie",
	"password",
	"passwd",
	"secret",
	"token",
	"api-key",
	"private-key",
	"signature",
	"credential",
	"session",
	"jwt",
	"bearer",
	"passphrase",
	"pwd",
	"dsn",
	"connection-string",
	"access-key",
	"signing-key",
	"master-key",
	"encryption-key",
	"hmac",
	"totp",
	"otp-code",
	"basic-auth",
	"auth-header",
	"x-auth",
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
	// RedactKeys replaces [DefaultRedactedKeys] when non-nil, and is matched
	// the same way: a key is redacted when it contains any of these terms,
	// once both are normalised. Pass an empty, non-nil slice to disable
	// redaction, which is only appropriate when nothing sensitive can reach
	// the logger.
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
		return slog.New(slog.NewJSONHandler(jsonSafeWriter{opts.Output}, &slog.HandlerOptions{
			Level:       opts.Level,
			AddSource:   opts.AddSource,
			ReplaceAttr: redact.replaceAttr,
		}))
	default:
		return slog.New(newConsoleHandler(opts, redact))
	}
}

// jsonSafeWriter writes what the JSON handler renders with every rune the
// console format escapes and JSON does not written as a \u escape instead.
//
// The handler escapes what JSON requires, the C0 controls and the two line
// separators, and writes everything else as it is: a C1 control such as U+009B
// (CSI, which some terminals act on when it arrives as decoded UTF-8), DEL, and
// the bidirectional controls that reorder how a line is displayed. Attribute
// values routinely carry what a client sent, the decoded request path among
// them, so a log read with tail or kubectl logs could otherwise be steered by
// a request.
//
// It works on the finished line rather than as a ReplaceAttr hook because a
// hook sees only the strings an attribute holds directly, and not a message
// built from one, a key, or a map or struct logged with slog.Any. On the
// finished line every such rune is inside a string, since JSON's own syntax is
// ASCII, and \u00XX means the same thing there as the rune does, so nothing
// a reader decodes changes. The handler makes one Write per record, so a
// record is still one write.
type jsonSafeWriter struct{ w io.Writer }

// Write escapes p and passes it on. It returns len(p) on success, as an
// io.Writer must, whatever the escaping did to the length.
func (j jsonSafeWriter) Write(p []byte) (int, error) {
	escaped := appendJSONEscaped(nil, p)
	if escaped == nil {
		return j.w.Write(p)
	}
	n, err := j.w.Write(escaped)
	if err != nil {
		return min(n, len(p)), err
	}
	return len(p), nil
}

// appendJSONEscaped returns p with each rune [unsafeInConsole] rejects written
// as a \u escape, or nil when there is none and p can be used as it is.
//
// Only the bytes that can begin such a rune are decoded: DEL, and the lead
// bytes of U+0080 to U+009F, U+061C and the U+2000 block, which is where the
// bidirectional controls live. Everything else, which is nearly all of any
// line, is skipped a byte at a time.
func appendJSONEscaped(dst, p []byte) []byte {
	const hex = "0123456789abcdef"
	copied := 0
	for i := 0; i < len(p); {
		switch p[i] {
		case 0x7f, 0xc2, 0xd8, 0xe2:
		default:
			i++
			continue
		}
		r, size := utf8.DecodeRune(p[i:])
		if r == utf8.RuneError || !unsafeInConsole(r) {
			i += size
			continue
		}
		if dst == nil {
			dst = make([]byte, 0, len(p)+32)
		}
		dst = append(dst, p[copied:i]...)
		dst = append(dst, '\\', 'u', hex[r>>12&0xf], hex[r>>8&0xf], hex[r>>4&0xf], hex[r&0xf])
		i += size
		copied = i
	}
	if dst == nil {
		return nil
	}
	return append(dst, p[copied:]...)
}

// maxLoggedPanicLength bounds how much of a recovered panic's value a log line
// carries. It is several times [maxQuotedLength] because a panic value is the
// application's own words and often the only account of what went wrong that
// the log will have, but it is still a bound: a message assembled from a
// request's input, a query or a decoded body is as large as that input, and one
// request can panic as often as it likes.
const maxLoggedPanicLength = 4096

// panicValue renders a recovered panic value for a log line, cut to
// [maxLoggedPanicLength].
//
// It is rendered with fmt rather than handed to the handler as it is, for two
// reasons besides the bound. A value that is not a string or an error would
// otherwise be marshalled in full by the JSON handler, however large or deeply
// nested, and fmt reports a String or Error method that itself panics rather
// than losing the record. The stack is logged separately.
func panicValue(recovered any) string {
	return truncateTo(fmt.Sprint(recovered), maxLoggedPanicLength)
}

// failureLevel returns the level to log a failure at: level, or debug level
// when err is the cancellation of ctx, the request's own context.
//
// net/http cancels a request's context when its client goes away, and
// whatever was waiting on the request's behalf, a store or a handler, then
// fails with that cancellation. Nothing failed on the server's side, and a
// client that connects and hangs up must not be able to write errors to the
// log at will, so it is recorded the way an event stream whose client went
// away is. A deadline that passed is not one of these: a store or a handler
// that runs out the clock is failing, and is reported at level.
func failureLevel(ctx context.Context, err error, level slog.Level) slog.Level {
	if errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
		return slog.LevelDebug
	}
	return level
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
	// terms are the configured keys, normalised once.
	terms [][]byte
}

// newRedactor normalises the configured keys once, so that the per-record
// check only has to normalise the key it is given.
//
// A term that normalises to nothing is dropped rather than kept, because
// every key contains the empty string and it would redact everything.
func newRedactor(keys []string) *redactor {
	r := &redactor{}
	for _, k := range keys {
		if term := appendNormalizedKey(nil, k); len(term) > 0 {
			r.terms = append(r.terms, term)
		}
	}
	return r
}

// normalizeKey lower-cases a key and drops the separators that distinguish
// "api_key" from "API-Key", so that one entry covers every spelling.
func normalizeKey(k string) string {
	return string(appendNormalizedKey(nil, k))
}

// appendNormalizedKey appends the normalised form of k to buf; see
// [normalizeKey].
func appendNormalizedKey(buf []byte, k string) []byte {
	for i := range len(k) {
		c := k[i]
		switch {
		case c == '-' || c == '_' || c == '.' || c == ' ':
			continue
		case c >= 'A' && c <= 'Z':
			buf = append(buf, c+('a'-'A'))
		default:
			buf = append(buf, c)
		}
	}
	return buf
}

// shouldRedact reports whether an attribute with this key carries a secret,
// which it does when the normalised key contains any normalised term.
//
// Matching whole keys only, as this once did, let the spellings credentials
// are actually logged under through: db_password, X-Api-Key, jwt, session_id,
// Stripe-Signature. The key is normalised into a buffer on the stack, so the
// check allocates nothing for any key of ordinary length.
func (r *redactor) shouldRedact(key string) bool {
	if len(r.terms) == 0 {
		return false
	}
	var scratch [64]byte
	normalized := appendNormalizedKey(scratch[:0], key)
	for _, term := range r.terms {
		if bytes.Contains(normalized, term) {
			return true
		}
	}
	return false
}

// anyRedacted reports whether any of the enclosing group names carries a
// secret, which makes every value inside the group one.
func (r *redactor) anyRedacted(groups []string) bool {
	for _, group := range groups {
		if r.shouldRedact(group) {
			return true
		}
	}
	return false
}

// replaceAttr is the slog.HandlerOptions hook that applies redaction to the
// JSON handler.
//
// slog never passes a group to this hook: it resolves a [slog.LogValuer],
// and when the result is a group it descends into it and passes each member
// instead, with the group's name in groups. Checking the key alone therefore
// let slog.Group("credentials", ...) and a LogValuer under "authorization"
// write every value in full. The enclosing names are checked as well, which
// covers a group opened with WithGroup too, and a match hides each member's
// value, so the group's shape is still logged but nothing inside it is.
func (r *redactor) replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if r.shouldRedact(a.Key) || r.anyRedacted(groups) {
		return slog.String(a.Key, RedactedPlaceholder)
	}
	if a.Value.Kind() == slog.KindAny {
		if redacted, ok := r.redactEntries(a.Value.Any()); ok {
			return slog.Any(a.Key, redacted)
		}
	}
	return a
}

// redactedValues stands in for the values of a multi-valued entry that is
// redacted, so that the map keeps its type and its shape in the output. It is
// shared by every copy that needs it and never written to.
var redactedValues = []string{RedactedPlaceholder}

// redactEntries returns a copy of v with the value of every entry whose key
// carries a secret replaced, when v is one of the maps a request's credentials
// travel in, and false when there is nothing to hide.
//
// Matching attribute keys alone, as this once did, left exactly the case the
// key list was written for: slog.Any("headers", r.Header) is one attribute
// whose key is "headers", and neither handler looks inside the value, so the
// Authorization and Cookie headers were written in full in both formats. A
// copy is made, and only when an entry matches, because the map is the
// caller's and is usually still in use by the request it came from.
func (r *redactor) redactEntries(v any) (any, bool) {
	if len(r.terms) == 0 {
		return nil, false
	}
	switch m := v.(type) {
	case http.Header:
		return redactMap(r, m, redactedValues)
	case url.Values:
		return redactMap(r, m, redactedValues)
	case map[string][]string:
		return redactMap(r, m, redactedValues)
	case map[string]string:
		return redactMap(r, m, RedactedPlaceholder)
	}
	return nil, false
}

// redactMap is [redactor.redactEntries] for one map type: a copy of m with
// placeholder in place of every matching entry's value, or false when no key
// matches.
func redactMap[M ~map[string]V, V any](r *redactor, m M, placeholder V) (any, bool) {
	var out M
	for key := range m {
		if !r.shouldRedact(key) {
			continue
		}
		if out == nil {
			out = maps.Clone(m)
		}
		out[key] = placeholder
	}
	if out == nil {
		return nil, false
	}
	return out, true
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
// Every record is exactly one line: the message, the scope, keys and values
// are escaped so that nothing a client sent can end the line early or reach
// the terminal as a control sequence; see [appendConsoleEscaped].
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
	// sealed reports that a group opened with WithGroup carries a name that
	// is redacted, so every value logged inside it is.
	sealed bool
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
	clone.sealed = h.sealed || h.redact.shouldRedact(name)
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
		sealed:   h.sealed,
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
	// The message is escaped rather than quoted, because quoting every
	// message would put noise around the common case, and it has to be
	// escaped at all because it is not always the application's own text:
	// the access log's message carries the decoded request path.
	buf = appendConsoleEscaped(buf, r.Message)

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
	buf = appendConsoleEscaped(buf, h.scope)
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
		if a.Key != "" && h.redact.shouldRedact(a.Key) {
			// The check has to come before the descent: once inside, only
			// the members' own keys would be looked at, and a group named
			// "credentials" holding "user" and "pass" would be written in
			// full. The whole group is replaced, which also keeps the
			// names of its members out of the line.
			return h.appendRedactedKey(buf, a.Key, groups)
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

	buf = h.appendKey(buf, a.Key, groups)
	return h.appendValue(buf, a.Key, a.Value)
}

// appendKey writes the " group.key=" part of a pair.
func (h *consoleHandler) appendKey(buf []byte, key string, groups []string) []byte {
	buf = append(buf, ' ', ' ')
	if h.color {
		buf = append(buf, ansiDim...)
	}
	for _, g := range groups {
		buf = appendConsoleEscaped(buf, g)
		buf = append(buf, '.')
	}
	buf = appendConsoleEscaped(buf, key)
	buf = append(buf, '=')
	if h.color {
		buf = append(buf, ansiReset...)
	}
	return buf
}

// appendRedactedKey writes a pair whose value, a whole group, is redacted.
func (h *consoleHandler) appendRedactedKey(buf []byte, key string, groups []string) []byte {
	return append(h.appendKey(buf, key, groups), RedactedPlaceholder...)
}

// appendValue writes an attribute value, quoting it when it contains anything
// that would make the key=value pair ambiguous.
func (h *consoleHandler) appendValue(buf []byte, key string, v slog.Value) []byte {
	if h.sealed || h.redact.shouldRedact(key) {
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
		// A header map or a query is printed with fmt, which knows nothing of
		// redaction, so its secret entries are replaced first; see
		// [redactor.redactEntries].
		if redacted, ok := h.redact.redactEntries(v.Any()); ok {
			v = slog.AnyValue(redacted)
		}
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
//
// A value needs quoting when it would make the key=value pair ambiguous, and
// also when it holds anything [needsConsoleEscape] reports: attribute values
// routinely carry what a client sent (a path, a header, a user agent), and
// strconv's quoting escapes every non-printable rune, so a quoted value can
// neither end the line early nor reach the terminal as a control sequence.
func appendMaybeQuoted(buf []byte, s string) []byte {
	if s == "" {
		return append(buf, '"', '"')
	}
	if strings.ContainsAny(s, " \t\n\r\"=\\") || needsConsoleEscape(s) {
		return strconv.AppendQuote(buf, s)
	}
	return append(buf, s...)
}

// unsafeInConsole reports whether a rune must not reach the console as itself.
//
// The console format promises one record per line, and anything reading it
// (a person at a terminal, a pager, a line-oriented collector) trusts that
// promise. A C0 or C1 control, DEL, or a Unicode line or paragraph separator
// could end the line early and let a client forge a record of its own, and an
// ESC opens a terminal escape sequence that can clear the screen, retitle the
// window or hide what came before it. The bidirectional controls cannot break
// a line, but they reorder how it is displayed, which is the same forgery by
// other means.
func unsafeInConsole(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Bidi_Control, r)
}

// needsConsoleEscape reports whether s holds a rune [unsafeInConsole] rejects
// or a byte that is not valid UTF-8, which a terminal may decode as a C1
// control of its own. The ASCII loop is the fast path for the text almost
// every record consists of.
func needsConsoleEscape(s string) bool {
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c < ' ' || c == 0x7f {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || unsafeInConsole(r) {
			return true
		}
		i += size
	}
	return false
}

// appendConsoleEscaped writes s with every rune [needsConsoleEscape] objects
// to replaced by its Go escape (\n, \x1b, \u2028), and everything else as
// it is.
//
// It escapes without quoting, for the parts of a line that are not
// key=value pairs. A backslash is left alone, so a literal "\x1b" in the text
// reads the same as an escaped ESC; that ambiguity costs nothing, because
// neither form can do anything to the terminal or the line.
func appendConsoleEscaped(buf []byte, s string) []byte {
	if !needsConsoleEscape(s) {
		return append(buf, s...)
	}
	const hex = "0123456789abcdef"
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			buf = append(buf, '\\', 'x', hex[s[i]>>4], hex[s[i]&0x0f])
		case unsafeInConsole(r):
			quoted := strconv.QuoteRuneToASCII(r)
			buf = append(buf, quoted[1:len(quoted)-1]...)
		default:
			buf = append(buf, s[i:i+size]...)
		}
		i += size
	}
	return buf
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
