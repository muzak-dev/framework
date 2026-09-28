package muzak

import (
	"bytes"
	"strings"
	"testing"
)

func TestDefaultRedactionCoversSecretBearingKeys(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: &buf})
	for _, key := range []string{
		"passphrase", "pass_phrase", "pwd", "db_pwd", "dsn", "database_dsn",
		"connection_string", "ConnectionString", "access_key", "AWS-Access-Key-Id",
		"signing_key", "master_key", "encryption_key", "hmac", "hmac_key",
		"totp", "totp_code", "otp_code", "basic_auth", "x-auth", "X-Auth-Request-User",
		"auth_header",
	} {
		buf.Reset()
		logger.Info("m", key, "SECRETVALUE")
		if strings.Contains(buf.String(), "SECRETVALUE") {
			t.Errorf("key %q was logged in full", key)
		}
	}
}

func TestDefaultRedactionLeavesCommonNamesAlone(t *testing.T) {
	t.Parallel()
	// Containment errs towards hiding, so every term added to the defaults is
	// a promise about the ordinary keys it will also hide. These are names an
	// application logs without a secret anywhere near them.
	var buf bytes.Buffer
	logger := NewLogger(LoggerOptions{Format: LogFormatJSON, Output: &buf})
	for _, key := range []string{
		"author", "authority", "authenticated", "auth_method", "oauth_provider",
		"passenger", "bypass", "compass", "passed", "hot_path", "footprint",
		"signal", "design", "signup", "assigned", "sigma", "keyboard",
		"refresh_interval", "user", "path", "method", "status",
	} {
		buf.Reset()
		logger.Info("m", key, "VISIBLEVALUE")
		if !strings.Contains(buf.String(), "VISIBLEVALUE") {
			t.Errorf("key %q was redacted, and is not a secret", key)
		}
	}
}
