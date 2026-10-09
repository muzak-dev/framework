package muzak

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRedactInsideAHeaderMap is the regression test for the case redaction
// was documented to exist for. A whole header map logged under one key, as
// slog.Any("headers", r.Header), was matched by that key alone, and "headers"
// is not a secret, so the Authorization and Cookie values inside it were
// written in full in both formats. The entries of a header map, a query and a
// plain string map are now matched by their own keys.
func TestRedactInsideAHeaderMap(t *testing.T) {
	t.Parallel()
	header := http.Header{
		"Authorization": {"Bearer LEAK-AUTHORIZATION"},
		"Cookie":        {"sid=LEAK-COOKIE"},
		"Accept":        {"VISIBLE-ACCEPT"},
	}
	for _, format := range []LogFormat{LogFormatJSON, LogFormatConsole} {
		var buf bytes.Buffer
		logger := NewLogger(LoggerOptions{Format: format, Output: &buf})

		logger.Info("header", slog.Any("headers", header))
		logger.Info("raw", slog.Any("raw", map[string][]string{"X-Api-Key": {"LEAK-RAW"}, "Accept": {"VISIBLE-RAW"}}))
		logger.Info("query", slog.Any("query", url.Values{"access_token": {"LEAK-QUERY"}, "page": {"VISIBLE-PAGE"}}))
		logger.Info("strings", slog.Any("fields", map[string]string{"password": "LEAK-STRINGS", "user": "VISIBLE-USER"}))
		// Inside a group, and attached ahead of time, which both formats
		// render through a different path from a record's own attributes.
		logger.Info("grouped", slog.Group("request", slog.Any("headers", http.Header{"Cookie": {"LEAK-GROUPED"}})))
		logger.With(slog.Any("headers", http.Header{"Authorization": {"LEAK-WITH"}})).Info("with")

		out := buf.String()
		for _, leaked := range []string{"LEAK-AUTHORIZATION", "LEAK-COOKIE", "LEAK-RAW", "LEAK-QUERY", "LEAK-STRINGS", "LEAK-GROUPED", "LEAK-WITH"} {
			if strings.Contains(out, leaked) {
				t.Errorf("format %d: %s reached the log from inside a map:\n%s", format, leaked, out)
			}
		}
		for _, kept := range []string{"VISIBLE-ACCEPT", "VISIBLE-RAW", "VISIBLE-PAGE", "VISIBLE-USER", "Authorization", "access_token"} {
			if !strings.Contains(out, kept) {
				t.Errorf("format %d: %s was hidden, want only the secret values redacted:\n%s", format, kept, out)
			}
		}
		if !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("format %d: nothing was marked as redacted:\n%s", format, out)
		}
	}
	// The map the caller logged is its own, and is still in use by the
	// request it came from, so redaction works on a copy.
	if got := header.Get("Authorization"); got != "Bearer LEAK-AUTHORIZATION" {
		t.Errorf("logging the header changed it: Authorization = %q", got)
	}
}

// TestRedactLeavesAnOrdinaryMapAlone checks that a map with nothing to hide is
// logged as it was, in the shape it was given.
func TestRedactLeavesAnOrdinaryMapAlone(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: &buf})
	logger.Info("m", slog.Any("headers", http.Header{"Accept": {"text/html"}}))
	if out := buf.String(); !strings.Contains(out, `"headers":{"Accept":["text/html"]}`) {
		t.Errorf("log = %s, want the map unchanged", out)
	}
}

// TestRedactDisabledReachesNoMap checks that an empty RedactKeys, which turns
// redaction off, turns it off inside a map as well.
func TestRedactDisabledReachesNoMap(t *testing.T) {
	t.Parallel()
	for _, format := range []LogFormat{LogFormatJSON, LogFormatConsole} {
		var buf bytes.Buffer
		logger := NewLogger(LoggerOptions{Format: format, Output: &buf, RedactKeys: []string{}})
		logger.Info("m", slog.Any("headers", http.Header{"Authorization": {"VISIBLE-ON-PURPOSE"}}))
		if out := buf.String(); !strings.Contains(out, "VISIBLE-ON-PURPOSE") || strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("format %d: log = %s, want the map written in full", format, out)
		}
	}
}
