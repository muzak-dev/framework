package muzak

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoggedPanicValueIsBounded(t *testing.T) {
	t.Parallel()
	// An application that panics with a message built from a request's input
	// would otherwise write a log line as large as the input.
	huge := "boom " + strings.Repeat("A", 100_000)

	t.Run("a handler", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		opts := quietOptions()
		opts.LoggerOptions = LoggerOptions{Format: LogFormatJSON, Output: &buf}
		app := New(opts)
		app.Handle(http.MethodGet, "/x", func(*Context, Empty) (Empty, error) { panic(huge) })
		mustBuild(t, app)
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
		checkPanicLine(t, buf.String(), "recovered from a panic in a handler")
	})

	t.Run("the recovery middleware", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: &buf})
		handler := Recovery(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(huge) }))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
		checkPanicLine(t, buf.String(), "recovered from a panic")
	})
}

func checkPanicLine(t *testing.T, out, message string) {
	t.Helper()
	if !strings.Contains(out, message) {
		t.Fatalf("no panic was logged:\n%.300s", out)
	}
	// The stack is logged too, so the line is not small, but it is not the size
	// of the value.
	if len(out) > 32<<10 {
		t.Errorf("the panic produced %d bytes of log, want the value bounded", len(out))
	}
	if !strings.Contains(out, truncatedMarker) {
		t.Errorf("the panic value was not marked as cut:\n%.300s", out)
	}
	if !strings.Contains(out, "boom AAAA") {
		t.Errorf("the start of the panic value was lost:\n%.300s", out)
	}
}

func TestLoggedPanicValueKeepsWhatFits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		recovered any
		want      string
	}{
		{"a string", "index out of range", "index out of range"},
		{"an error", http.ErrNotSupported, http.ErrNotSupported.Error()},
		{"a number", 42, "42"},
	} {
		if got := panicValue(tc.recovered); got != tc.want {
			t.Errorf("%s: panicValue() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
