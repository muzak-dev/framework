package muzak

import (
	"bytes"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

// hostileText is one of each thing a terminal or a reader may act on that JSON
// itself does not escape: a C1 control (CSI), DEL, and the bidirectional
// controls, which reorder how a line is displayed.
const hostileText = "a\u009bb\u0085c\u007fd\u202ee\u2066f\u200fg\u061ch"

func TestJSONLogEscapesWhatAConsoleWould(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: &buf})

	logger.Info("msg "+hostileText,
		slog.String("path", hostileText),
		slog.String(hostileText, "as a key"),
		slog.Any("map", map[string]string{"k": hostileText}),
		slog.Group("g", slog.String("inner", hostileText)),
	)

	out := buf.String()
	for _, r := range out {
		if unsafeInConsole(r) && r != '\n' {
			t.Errorf("the log line carries %U raw:\n%q", r, out)
		}
	}
	if !utf8.ValidString(out) {
		t.Error("the escaping left the line invalid UTF-8")
	}

	// Escaping must not change what the record says, only how it is spelled.
	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("the line is not JSON any more: %v\n%q", err, out)
	}
	if got := record["msg"]; got != "msg "+hostileText {
		t.Errorf("msg = %q, want it unchanged once decoded", got)
	}
	if got := record["path"]; got != hostileText {
		t.Errorf("path = %q, want it unchanged once decoded", got)
	}
	if got := record[hostileText]; got != "as a key" {
		t.Errorf("the hostile key decoded to %q, want it unchanged", got)
	}
	if got := record["map"].(map[string]any)["k"]; got != hostileText {
		t.Errorf("map.k = %q, want it unchanged once decoded", got)
	}
}

func TestJSONLogLeavesOrdinaryTextAlone(t *testing.T) {
	t.Parallel()
	// The escaping looks at every byte that could begin one of the runes it
	// escapes, and these begin the same way without being them.
	const text = "caf\u00e9 \u65e5\u672c\u8a9e \u201cquoted\u201d \u2014 dash \u2026 \u2028 done \u00a0 \u200b \u2067x \U0001F600"
	var plain, wrapped bytes.Buffer
	slog.New(slog.NewJSONHandler(&plain, nil)).Info("m", "v", text)
	NewLogger(LoggerOptions{Format: LogFormatJSON, Output: &wrapped}).Info("m", "v", text)

	strip := func(b []byte) string {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("not JSON: %v\n%q", err, b)
		}
		return m["v"].(string)
	}
	if got := strip(wrapped.Bytes()); got != text {
		t.Errorf("value = %q, want %q", got, text)
	}
	// U+2067 is a bidi control and is the one rune here that is escaped; the
	// rest of the line is what the standard handler writes.
	want := strings.ReplaceAll(plain.String(), "\u2067", `\u2067`)
	got := wrapped.String()
	// The timestamps differ, so compare from the message on.
	if trim := func(s string) string { return s[strings.Index(s, `"level"`):] }; trim(got) != trim(want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestJSONLogWritesEachRecordOnce(t *testing.T) {
	t.Parallel()
	// A record is one Write, so lines from concurrent loggers cannot interleave
	// on an output that only makes single writes atomic.
	var out countingWriter
	logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: &out})
	logger.Info("one " + hostileText)
	logger.Info("two")
	if out.writes != 2 {
		t.Errorf("two records took %d writes", out.writes)
	}
}

type countingWriter struct{ writes int }

func (c *countingWriter) Write(p []byte) (int, error) { c.writes++; return len(p), nil }
