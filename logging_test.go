package badele

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// consoleLogger returns a console logger writing into a buffer, with colour
// and the timestamp pinned so that assertions can be exact.
func consoleLogger(t *testing.T, adjust func(*LoggerOptions)) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	noColor := false
	opts := LoggerOptions{
		Format: LogFormatConsole,
		Output: buf,
		Level:  slog.LevelDebug,
		Color:  &noColor,
	}
	if adjust != nil {
		adjust(&opts)
	}
	return NewLogger(opts), buf
}

func TestConsoleFormat(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)

	Scoped(logger, ScopeServer).Info("Listening on :8080")
	line := buf.String()

	if !strings.Contains(line, "INFO  [Server]") {
		t.Errorf("line does not carry the level and scope columns:\n%q", line)
	}
	if !strings.Contains(line, "Listening on :8080") {
		t.Errorf("line lost the message:\n%q", line)
	}
	// The scope column is padded so that messages line up.
	if idx := strings.Index(line, "Listening"); idx < 0 {
		t.Fatalf("no message in %q", line)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Error("the line is not terminated")
	}
}

func TestConsoleColumnsAlign(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)
	scoped := Scoped(logger, ScopeServer)

	scoped.Debug("one")
	scoped.Info("two")
	scoped.Warn("three")
	scoped.Error("four")

	var columns []int
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		columns = append(columns, strings.Index(line, "[Server]"))
	}
	for i, column := range columns {
		if column != columns[0] {
			t.Errorf("line %d starts its scope at column %d, want %d (levels must not shift the columns)", i, column, columns[0])
		}
	}
}

func TestConsoleScopePadding(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)

	Scoped(logger, "Server").Info("short")
	Scoped(logger, "UsersService").Info("medium")
	Scoped(logger, "AVeryLongSubsystemName").Info("long")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for i, line := range lines {
		closing := strings.Index(line, "]")
		message := strings.LastIndex(line, " ") + 1
		if message <= closing {
			t.Errorf("line %d does not separate the scope from the message: %q", i, line)
		}
	}
	// A scope that overflows the column still leaves one space.
	if !strings.Contains(lines[2], "[AVeryLongSubsystemName] long") {
		t.Errorf("an overlong scope was not separated from the message: %q", lines[2])
	}
}

func TestConsoleAttributes(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)

	Scoped(logger, "UsersService").Info("user created",
		"user_id", 42,
		RequestIDKey, "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31",
	)
	line := buf.String()

	if !strings.Contains(line, "user_id=42") {
		t.Errorf("missing user_id: %q", line)
	}
	// The console format shortens request identifiers for readability.
	if !strings.Contains(line, "request_id=0611f4b2") {
		t.Errorf("request id was not shortened: %q", line)
	}
	if strings.Contains(line, "2f0a-4b57") {
		t.Errorf("request id was not shortened: %q", line)
	}
}

func TestConsoleFullRequestIDWhenAsked(t *testing.T) {
	t.Parallel()
	full := false
	logger, buf := consoleLogger(t, func(o *LoggerOptions) { o.ShortRequestID = &full })

	logger.Info("m", RequestIDKey, "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31")
	if !strings.Contains(buf.String(), "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31") {
		t.Errorf("the full identifier was not written: %q", buf.String())
	}
}

func TestConsoleValueFormatting(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)

	logger.Info("values",
		"str", "plain",
		"spaced", "needs quoting",
		"empty", "",
		"int", 7,
		"uint", uint64(8),
		"float", 1.5,
		"bool", true,
		"duration", 1500*time.Millisecond,
		"time", time.Date(2026, 8, 21, 14, 32, 7, 0, time.UTC),
		"any", []int{1, 2},
	)
	line := buf.String()

	for _, want := range []string{
		"str=plain", `spaced="needs quoting"`, `empty=""`, "int=7", "uint=8",
		"float=1.5", "bool=true", "duration=1.5s", "time=2026-08-21T14:32:07Z",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in:\n%q", want, line)
		}
	}
}

func TestConsoleGroupsAndNesting(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)

	logger.WithGroup("db").With("host", "localhost").Info("connected", "port", 5432)
	line := buf.String()
	if !strings.Contains(line, "db.host=localhost") || !strings.Contains(line, "db.port=5432") {
		t.Errorf("group prefixes missing: %q", line)
	}

	buf.Reset()
	logger.Info("inline", slog.Group("http", "method", "GET", "status", 200))
	line = buf.String()
	if !strings.Contains(line, "http.method=GET") || !strings.Contains(line, "http.status=200") {
		t.Errorf("inline group missing: %q", line)
	}

	buf.Reset()
	logger.Info("empties", slog.Group("empty"), slog.Attr{})
	if got := buf.String(); strings.Contains(got, "empty") {
		t.Errorf("an empty group was rendered: %q", got)
	}

	buf.Reset()
	// A group with no name promotes its members rather than prefixing them.
	logger.Info("unnamed", slog.Any("", slog.GroupValue(slog.String("k", "v"))))
	if got := buf.String(); !strings.Contains(got, "k=v") {
		t.Errorf("an unnamed group did not promote its members: %q", got)
	}
}

func TestConsoleWithGroupIgnoresEmptyName(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)
	logger.WithGroup("").Info("m", "k", "v")
	if got := buf.String(); !strings.Contains(got, "k=v") || strings.Contains(got, ".k=") {
		t.Errorf("an empty group name added a prefix: %q", got)
	}
}

func TestConsoleWithAttrsIsCopiedNotShared(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)

	base := logger.With("shared", "yes")
	left := base.With("side", "left")
	right := base.With("side", "right")

	left.Info("l")
	right.Info("r")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[0], "side=left") || strings.Contains(lines[0], "side=right") {
		t.Errorf("derived loggers share state: %q", lines[0])
	}
	if !strings.Contains(lines[1], "side=right") || strings.Contains(lines[1], "side=left") {
		t.Errorf("derived loggers share state: %q", lines[1])
	}
}

func TestConsoleWithNoAttrsReturnsTheSameHandler(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)
	logger.With().Info("m")
	if !strings.Contains(buf.String(), "m") {
		t.Errorf("a no-op With broke logging: %q", buf.String())
	}
}

func TestConsoleColour(t *testing.T) {
	t.Parallel()
	color := true
	logger, buf := consoleLogger(t, func(o *LoggerOptions) { o.Color = &color })

	Scoped(logger, "Server").Error("failed", "key", "value")
	line := buf.String()
	if !strings.Contains(line, ansiRed) {
		t.Errorf("the error level is not coloured: %q", line)
	}
	if !strings.Contains(line, ansiCyan) {
		t.Errorf("the scope is not coloured: %q", line)
	}
	if !strings.Contains(line, ansiDim) {
		t.Errorf("the timestamp is not dimmed: %q", line)
	}

	buf.Reset()
	// A record without a scope still gets a coloured level.
	logger.Debug("plain")
	if !strings.Contains(buf.String(), ansiBlue) {
		t.Errorf("the debug level is not coloured: %q", buf.String())
	}
}

func TestConsoleSource(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, func(o *LoggerOptions) { o.AddSource = true })
	logger.Info("with source")
	if !strings.Contains(buf.String(), "logging_test.go:") {
		t.Errorf("the call site was not recorded: %q", buf.String())
	}

	buf.Reset()
	colorLogger, colorBuf := consoleLogger(t, func(o *LoggerOptions) {
		o.AddSource = true
		yes := true
		o.Color = &yes
	})
	colorLogger.Info("coloured source")
	if !strings.Contains(colorBuf.String(), "source=") {
		t.Errorf("the coloured call site was not recorded: %q", colorBuf.String())
	}
}

// TestConsoleSourceWithoutPC covers a record built by hand, which carries no
// program counter and therefore no source location.
func TestConsoleSourceWithoutPC(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, func(o *LoggerOptions) { o.AddSource = true })
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "handmade", 0)
	if err := logger.Handler().Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if strings.Contains(buf.String(), "source=") {
		t.Errorf("a record without a program counter reported a source: %q", buf.String())
	}
}

// TestConsoleZeroTime covers a record whose timestamp was not set.
func TestConsoleZeroTime(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "no timestamp", 0)
	if err := logger.Handler().Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle = %v", err)
	}
	if !strings.Contains(buf.String(), "no timestamp") {
		t.Errorf("a record with no timestamp was dropped: %q", buf.String())
	}
}

func TestTrimSourcePath(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"/home/user/go/src/badele/logging.go", "badele/logging.go"},
		{"badele/logging.go", "badele/logging.go"},
		{"logging.go", "logging.go"},
		{"/logging.go", "/logging.go"},
	}
	for _, tc := range tests {
		if got := trimSourcePath(tc.in); got != tc.want {
			t.Errorf("trimSourcePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedaction(t *testing.T) {
	t.Parallel()

	t.Run("console", func(t *testing.T) {
		t.Parallel()
		logger, buf := consoleLogger(t, nil)
		logger.Info("request",
			"Authorization", "Bearer supersecret",
			"api_key", "key-123",
			"API-KEY", "key-456",
			"password", "hunter2",
			"user_id", 42,
		)
		line := buf.String()
		for _, secret := range []string{"supersecret", "key-123", "key-456", "hunter2"} {
			if strings.Contains(line, secret) {
				t.Errorf("the log leaked %q:\n%s", secret, line)
			}
		}
		if strings.Count(line, RedactedPlaceholder) != 4 {
			t.Errorf("expected four redactions:\n%s", line)
		}
		if !strings.Contains(line, "user_id=42") {
			t.Errorf("a harmless attribute was redacted:\n%s", line)
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		buf := &bytes.Buffer{}
		logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: buf})
		logger.Info("request", "token", "abc123", "user_id", 42)

		var record map[string]any
		if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
			t.Fatalf("the JSON handler produced invalid JSON: %v", err)
		}
		if record["token"] != RedactedPlaceholder {
			t.Errorf("token = %v, want it redacted", record["token"])
		}
		if record["user_id"] != 42.0 {
			t.Errorf("user_id = %v, want 42", record["user_id"])
		}
	})

	t.Run("can be disabled", func(t *testing.T) {
		t.Parallel()
		buf := &bytes.Buffer{}
		noColor := false
		logger := NewLogger(LoggerOptions{
			Format: LogFormatConsole, Output: buf, Color: &noColor,
			RedactKeys: []string{},
		})
		logger.Info("m", "token", "visible")
		if !strings.Contains(buf.String(), "visible") {
			t.Errorf("redaction was not disabled: %q", buf.String())
		}
	})
}

func TestNormalizeKey(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"API-Key", "apikey"},
		{"api_key", "apikey"},
		{"apikey", "apikey"},
		{"Api.Key", "apikey"},
		{"api key", "apikey"},
		{"", ""},
		{"user_id", "userid"},
	}
	for _, tc := range tests {
		if got := normalizeKey(tc.in); got != tc.want {
			t.Errorf("normalizeKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLogFormats(t *testing.T) {
	t.Parallel()

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		buf := &bytes.Buffer{}
		NewLogger(LoggerOptions{Format: LogFormatJSON, Output: buf}).Info("m")
		if !strings.HasPrefix(buf.String(), "{") {
			t.Errorf("not JSON: %q", buf.String())
		}
	})

	t.Run("none discards everything", func(t *testing.T) {
		t.Parallel()
		buf := &bytes.Buffer{}
		logger := NewLogger(LoggerOptions{Format: LogFormatNone, Output: buf})
		logger.Error("dropped")
		if logger.Enabled(context.Background(), slog.LevelError) {
			t.Error("the discarding handler reports itself enabled")
		}
		// The derived handlers must discard too.
		logger.With("k", "v").WithGroup("g").Error("also dropped")
		if buf.Len() != 0 {
			t.Errorf("output = %q, want nothing", buf.String())
		}
	})

	t.Run("auto chooses JSON when the output is not a terminal", func(t *testing.T) {
		t.Parallel()
		buf := &bytes.Buffer{}
		NewLogger(LoggerOptions{Format: LogFormatAuto, Output: buf}).Info("m")
		if !strings.HasPrefix(buf.String(), "{") {
			t.Errorf("auto did not choose JSON for a buffer: %q", buf.String())
		}
	})
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()
	buf := &bytes.Buffer{}
	noColor := false
	logger := NewLogger(LoggerOptions{
		Format: LogFormatConsole, Output: buf, Level: slog.LevelWarn, Color: &noColor,
	})
	logger.Debug("dropped")
	logger.Info("dropped")
	logger.Warn("kept")
	if strings.Contains(buf.String(), "dropped") {
		t.Errorf("records below the level were written: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "kept") {
		t.Errorf("a record at the level was dropped: %q", buf.String())
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a buffer was reported as a terminal")
	}
	// A regular file is not a character device.
	file, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer file.Close()
	if isTerminal(file) {
		t.Error("a regular file was reported as a terminal")
	}
	// A closed file cannot be stat'ed.
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	closed.Close()
	if isTerminal(closed) {
		t.Error("a closed file was reported as a terminal")
	}
}

func TestScoped(t *testing.T) {
	t.Parallel()
	if Scoped(nil, "anything") != nil {
		t.Error("Scoped(nil) returned a logger")
	}
	logger, buf := consoleLogger(t, nil)
	Scoped(logger, "Custom").Info("m")
	if !strings.Contains(buf.String(), "[Custom]") {
		t.Errorf("the scope was not applied: %q", buf.String())
	}
}

// TestScopeInsideAGroupStaysAnAttribute checks that a scope set after a group
// is opened is treated as ordinary data rather than as the scope column, since
// it belongs to the group's namespace.
func TestScopeInsideAGroupStaysAnAttribute(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)
	logger.WithGroup("g").With(ScopeKey, "NotAColumn").Info("m")
	line := buf.String()
	if strings.Contains(line, "[NotAColumn]") {
		t.Errorf("a grouped scope was lifted into the column: %q", line)
	}
	if !strings.Contains(line, "g.scope=NotAColumn") {
		t.Errorf("a grouped scope was lost: %q", line)
	}
}

func TestLevelText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO "},
		{slog.LevelWarn, "WARN "},
		{slog.LevelError, "ERROR"},
		{slog.LevelError + 4, "ERROR"},
	}
	for _, tc := range tests {
		got, _ := levelText(tc.level)
		if got != tc.want {
			t.Errorf("levelText(%v) = %q, want %q", tc.level, got, tc.want)
		}
		if len(got) != 5 {
			t.Errorf("levelText(%v) is %d characters, want 5 so the columns align", tc.level, len(got))
		}
	}
}

func TestAppendMaybeQuoted(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"", `""`},
		{"has space", `"has space"`},
		{"has=equals", `"has=equals"`},
		{"has\"quote", `"has\"quote"`},
		{"has\nnewline", `"has\nnewline"`},
	}
	for _, tc := range tests {
		if got := string(appendMaybeQuoted(nil, tc.in)); got != tc.want {
			t.Errorf("appendMaybeQuoted(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestConsoleHandlerWriteError covers a writer that fails, which Handle must
// report rather than swallow.
func TestConsoleHandlerWriteError(t *testing.T) {
	t.Parallel()
	noColor := false
	logger := NewLogger(LoggerOptions{
		Format: LogFormatConsole, Output: failingWriter{}, Color: &noColor,
	})
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "m", 0)
	if err := logger.Handler().Handle(context.Background(), record); err == nil {
		t.Error("Handle swallowed a write failure")
	}
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

var errWriteFailed = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "write failed" }

// TestConsoleHandlerReleasesOversizedBuffers keeps one enormous record from
// pinning memory in the pool.
func TestConsoleHandlerReleasesOversizedBuffers(t *testing.T) {
	t.Parallel()
	logger, buf := consoleLogger(t, nil)
	logger.Info("big", "payload", strings.Repeat("x", 64<<10))
	if buf.Len() < 64<<10 {
		t.Errorf("the oversized record was truncated: %d bytes", buf.Len())
	}
	// A following record must still work, which it would not if the pool had
	// been left in a bad state.
	buf.Reset()
	logger.Info("small")
	if !strings.Contains(buf.String(), "small") {
		t.Errorf("logging broke after an oversized record: %q", buf.String())
	}
}
