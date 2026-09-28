package muzak

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// redactCreds is a credential type that logs itself as a group, which is the
// natural way to give a struct a structured log form.
type redactCreds struct{ User, Pass string }

func (c redactCreds) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user", c.User), slog.String("pass", c.Pass))
}

// TestRedactGroupValuedKeys is the regression test for a redacted key whose
// value is a group: slog never hands a group to ReplaceAttr and the console
// handler descended into one before looking at its key, so every value inside
// was written in full, in both formats.
func TestRedactGroupValuedKeys(t *testing.T) {
	t.Parallel()
	for _, format := range []LogFormat{LogFormatJSON, LogFormatConsole} {
		var buf bytes.Buffer
		logger := NewLogger(LoggerOptions{Format: format, Output: &buf})

		logger.Info("group", slog.Group("credentials", slog.String("user", "bob"), slog.String("pass", "LEAK-GROUP")))
		logger.Info("valuer", slog.Any("authorization", redactCreds{User: "bob", Pass: "LEAK-VALUER"}))
		logger.WithGroup("session").Info("with-group", slog.String("id", "LEAK-WITHGROUP"),
			slog.Group("inner", slog.String("deep", "LEAK-NESTED")))
		logger.WithGroup("session").With(slog.String("pre", "LEAK-PRERENDERED")).Info("with-attrs")
		logger.Info("control", slog.String("credentials", "HIDDEN-CONTROL"))
		// A group whose name is not sensitive is written as before.
		logger.WithGroup("request").Info("public", slog.Group("client", slog.String("agent", "VISIBLE-AGENT")))

		out := buf.String()
		for _, leaked := range []string{"LEAK-GROUP", "LEAK-VALUER", "LEAK-WITHGROUP", "LEAK-NESTED", "LEAK-PRERENDERED", "HIDDEN-CONTROL"} {
			if strings.Contains(out, leaked) {
				t.Errorf("format %d: %s reached the log although its group is redacted:\n%s", format, leaked, out)
			}
		}
		if !strings.Contains(out, "VISIBLE-AGENT") {
			t.Errorf("format %d: a group with an ordinary name was redacted:\n%s", format, out)
		}
		if !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("format %d: nothing was marked as redacted:\n%s", format, out)
		}
	}
}

// TestRedactGroupConsoleShape pins what the console writes for a redacted
// group: the key once, with the placeholder, and none of the members' names.
func TestRedactGroupConsoleShape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(LoggerOptions{Format: LogFormatConsole, Output: &buf})
	logger.WithGroup("req").Info("m", slog.Group("cookie", slog.String("sid", "x")))
	out := buf.String()
	if !strings.Contains(out, "req.cookie="+RedactedPlaceholder) || strings.Contains(out, "sid") {
		t.Errorf("console line = %q, want the group replaced whole", out)
	}
}
