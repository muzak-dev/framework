// Package webhook verifies signed incoming webhook payloads before JSON
// binding using Muzak's [muzak.CaptureBody] and [muzak.Context.RawBody].
package webhook

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"muzak.dev/framework"
)

// Defaults for [Options].
const (
	DefaultHeader          = "X-Webhook-Signature"
	DefaultTimestampHeader = "X-Webhook-Timestamp"
	DefaultTolerance       = 5 * time.Minute
	DefaultMaxBodyBytes    = 1 << 20
)

// Algorithm selects the webhook signature algorithm.
type Algorithm uint8

const (
	// HMACSHA256 verifies a hex-encoded HMAC-SHA256 digest (accepting an
	// optional "sha256=" prefix).
	HMACSHA256 Algorithm = iota
	// Ed25519 verifies a hex- or base64url-encoded Ed25519 signature.
	Ed25519
)

// Options configures [Guard].
type Options struct {
	// Algorithm selects HMACSHA256 (default) or Ed25519.
	Algorithm Algorithm

	// Secret is the shared secret used for [HMACSHA256].
	Secret []byte

	// PublicKey is the verifying key used for [Ed25519].
	PublicKey ed25519.PublicKey

	// Header names the request header carrying the signature, defaulting to
	// [DefaultHeader] ("X-Webhook-Signature").
	Header string

	// TimestampHeader names the request header carrying a Unix-seconds
	// timestamp, defaulting to [DefaultTimestampHeader] ("X-Webhook-Timestamp").
	TimestampHeader string

	// Tolerance bounds the allowed clock skew / replay window for
	// TimestampHeader, defaulting to [DefaultTolerance] (5m). A negative value
	// disables timestamp checking and verifies over raw body bytes alone.
	Tolerance time.Duration

	// MaxBodyBytes bounds the captured raw body size, defaulting to
	// [DefaultMaxBodyBytes] (1 MiB). Negative removes this guard-level bound
	// (the route's own MaxBodySize still applies).
	MaxBodyBytes int64

	// Now provides the current time for timestamp validation, defaulting to
	// [time.Now].
	Now func() time.Time
}

// Guard returns a [muzak.Guard] that verifies incoming webhook signatures over
// the raw bytes captured by [muzak.CaptureBody].
//
// Register the route with both [muzak.CaptureBody] and [muzak.WithDependencies]:
//
//	r.Post("/webhooks/stripe", handleWebhook,
//		muzak.CaptureBody(),
//		muzak.WithDependencies(webhook.Guard(webhook.Options{Secret: secret})))
func Guard(opts Options) muzak.Guard {
	header := opts.Header
	if header == "" {
		header = DefaultHeader
	}
	tsHeader := opts.TimestampHeader
	if tsHeader == "" {
		tsHeader = DefaultTimestampHeader
	}
	tolerance := opts.Tolerance
	if tolerance == 0 {
		tolerance = DefaultTolerance
	}
	maxBody := opts.MaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultMaxBodyBytes
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	secret := append([]byte(nil), opts.Secret...)
	pub := append(ed25519.PublicKey(nil), opts.PublicKey...)

	return func(ctx *muzak.Context) error {
		body, ok := ctx.RawBody()
		if !ok {
			return muzak.NewHTTPError(http.StatusInternalServerError, "webhook verification requires muzak.CaptureBody() on this route")
		}
		if maxBody > 0 && int64(len(body)) > maxBody {
			return muzak.NewHTTPErrorf(http.StatusRequestEntityTooLarge, "webhook payload exceeds the %d byte limit", maxBody)
		}
		sigValues := ctx.Request().Header.Values(header)
		if len(sigValues) != 1 || strings.TrimSpace(sigValues[0]) == "" {
			return muzak.NewHTTPError(http.StatusUnauthorized, "missing or duplicate webhook signature header")
		}
		sigHeader := strings.TrimSpace(sigValues[0])

		var signed []byte
		if tolerance > 0 {
			tsValues := ctx.Request().Header.Values(tsHeader)
			if len(tsValues) != 1 || strings.TrimSpace(tsValues[0]) == "" {
				return muzak.NewHTTPError(http.StatusUnauthorized, "missing or duplicate webhook timestamp header")
			}
			tsText := strings.TrimSpace(tsValues[0])
			sec, err := strconv.ParseInt(tsText, 10, 64)
			if err != nil {
				return muzak.NewHTTPError(http.StatusUnauthorized, "invalid webhook timestamp")
			}
			skew := nowFn().Sub(time.Unix(sec, 0))
			if skew < 0 {
				skew = -skew
			}
			if skew > tolerance {
				return muzak.NewHTTPError(http.StatusUnauthorized, "webhook timestamp is outside the allowed replay window")
			}
			signed = make([]byte, 0, len(tsText)+1+len(body))
			signed = append(signed, tsText...)
			signed = append(signed, '.')
			signed = append(signed, body...)
		} else {
			signed = body
		}

		switch opts.Algorithm {
		case HMACSHA256:
			if len(secret) == 0 {
				return muzak.NewHTTPError(http.StatusInternalServerError, "webhook HMAC secret is not configured")
			}
			rawHex := strings.TrimPrefix(sigHeader, "sha256=")
			sig, err := hex.DecodeString(rawHex)
			if err != nil {
				return muzak.NewHTTPError(http.StatusUnauthorized, "invalid webhook signature")
			}
			mac := hmac.New(sha256.New, secret)
			_, _ = mac.Write(signed)
			if !hmac.Equal(sig, mac.Sum(nil)) {
				return muzak.NewHTTPError(http.StatusUnauthorized, "invalid webhook signature")
			}
		case Ed25519:
			if len(pub) != ed25519.PublicKeySize {
				return muzak.NewHTTPError(http.StatusInternalServerError, "webhook Ed25519 public key is not configured")
			}
			sig, err := decodeEd25519Signature(sigHeader)
			if err != nil || !ed25519.Verify(pub, signed, sig) {
				return muzak.NewHTTPError(http.StatusUnauthorized, "invalid webhook signature")
			}
		default:
			return muzak.NewHTTPError(http.StatusInternalServerError, "unsupported webhook signature algorithm")
		}
		return nil
	}
}

func decodeEd25519Signature(value string) ([]byte, error) {
	if sig, err := hex.DecodeString(value); err == nil && len(sig) == ed25519.SignatureSize {
		return sig, nil
	}
	if sig, err := base64.RawURLEncoding.DecodeString(value); err == nil && len(sig) == ed25519.SignatureSize {
		return sig, nil
	}
	return base64.StdEncoding.DecodeString(value)
}
