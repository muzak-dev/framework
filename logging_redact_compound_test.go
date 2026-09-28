package muzak

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestRedactCompoundKeys is the regression test for redaction that matched
// whole keys only, which let the spellings credentials are actually logged
// under through in clear.
func TestRedactCompoundKeys(t *testing.T) {
	t.Parallel()
	secrets := []string{
		"db_password", "new_password", "X-Api-Key", "x-auth-token", "api_token",
		"github_token", "jwt", "bearer", "aws_secret_access_key", "jwt_secret",
		"session_id", "sessionid", "private_key_pem", "Stripe-Signature", "X-CSRF-Token",
		"client_secret", "access_token", "Proxy-Authorization", "Set-Cookie", "credentials",
		"passwd", "refresh.token",
	}
	public := []string{"user_id", "method", "path", "status", "author", "keyboard", "duration"}
	for _, format := range []LogFormat{LogFormatJSON, LogFormatConsole} {
		var buf bytes.Buffer
		logger := NewLogger(LoggerOptions{Format: format, Output: &buf})
		for _, key := range append(append([]string(nil), secrets...), public...) {
			logger.Info("k", slog.String(key, "VALUE-OF-"+key))
		}
		out := buf.String()
		for _, key := range secrets {
			if strings.Contains(out, "VALUE-OF-"+key) {
				t.Errorf("format %d: %s was written in clear", format, key)
			}
		}
		for _, key := range public {
			if !strings.Contains(out, "VALUE-OF-"+key) {
				t.Errorf("format %d: %s, which carries no secret, was redacted", format, key)
			}
		}
	}
}

// TestRedactCustomTerms covers a list passed as RedactKeys, which is matched
// by the same rule, and a term that normalises to nothing, which must not
// redact every key.
func TestRedactCustomTerms(t *testing.T) {
	t.Parallel()
	r := newRedactor([]string{"PIN", "", "-_"})
	for key, want := range map[string]bool{
		"pin":       true,
		"card_pin":  true,
		"PIN-Code":  true,
		"user_id":   false,
		"token":     false,
		"":          false,
		"user_name": false,
	} {
		if got := r.shouldRedact(key); got != want {
			t.Errorf("shouldRedact(%q) = %v, want %v", key, got, want)
		}
	}
	if len(r.terms) != 1 {
		t.Errorf("kept %d terms, want the empty ones dropped", len(r.terms))
	}
}

// TestRedactAllocatesNothing pins the cost of the check on the logging path.
// It does not run in parallel, because AllocsPerRun counts every allocation
// in the process and a test running beside it would be counted too.
func TestRedactAllocatesNothing(t *testing.T) {
	r := newRedactor(DefaultRedactedKeys)
	allocs := testing.AllocsPerRun(100, func() {
		_ = r.shouldRedact("X-Request-Correlation-Identifier")
		_ = r.shouldRedact("aws_secret_access_key")
	})
	if allocs != 0 {
		t.Errorf("shouldRedact allocated %v times per run, want none", allocs)
	}
	// A key longer than the stack buffer still matches.
	if !r.shouldRedact(strings.Repeat("x", 100) + "_password") {
		t.Error("a long key was not matched")
	}
}
