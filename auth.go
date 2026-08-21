package badele

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// SecureCompare reports whether two secrets are equal, in time that does not
// depend on where they first differ.
//
// Comparing a credential with == leaks its contents: the comparison returns as
// soon as two bytes differ, so an attacker who can time the response can
// recover the secret one byte at a time. SecureCompare hashes both inputs
// first and compares the digests with [subtle.ConstantTimeCompare], which also
// keeps the length of the expected secret from leaking through the comparison.
//
// Use it for tokens, API keys and signatures. It is not a password
// verification function: a password must be checked against a slow,
// memory-hard hash such as the one golang.org/x/crypto/argon2 provides.
func SecureCompare(given, expected string) bool {
	givenSum := sha256.Sum256([]byte(given))
	expectedSum := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(givenSum[:], expectedSum[:]) == 1
}

// BearerToken returns the token from the request's Authorization header and
// reports whether a well-formed bearer credential was present. The scheme is
// matched case-insensitively, as RFC 9110 requires.
func BearerToken(ctx *Context) (string, bool) {
	value := ctx.Header("Authorization")
	scheme, token, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// RequireBearerToken returns a guard that rejects any request not carrying the
// given bearer token.
//
// The comparison is constant-time, and a missing credential is answered with a
// WWW-Authenticate header so that a client knows which scheme to use. It is
// meant for the shared-secret case, such as an internal service or a webhook
// receiver; anything involving per-user credentials wants a value dependency
// that resolves the user instead.
//
//	app.Include(admin.NewRouter(),
//		badele.WithPrefix("/admin"),
//		badele.WithDependencies(badele.RequireBearerToken(settings.AdminToken)),
//	)
func RequireBearerToken(expected string) Guard {
	return func(ctx *Context) error {
		token, present := BearerToken(ctx)
		if !present {
			ctx.SetHeader("WWW-Authenticate", `Bearer realm="api"`)
			return NewHTTPError(http.StatusUnauthorized, "a bearer token is required")
		}
		if !SecureCompare(token, expected) {
			return NewHTTPError(http.StatusUnauthorized, "the bearer token is not valid")
		}
		return nil
	}
}

// RequireHeaderToken returns a guard that rejects any request whose named
// header does not carry the expected value, compared in constant time.
//
// It covers the shared-secret headers that are not bearer credentials, such as
// the X-Token header in the FastAPI tutorial or a webhook signing key:
//
//	badele.WithDependencies(badele.RequireHeaderToken("X-Token", "coneofsilence"))
func RequireHeaderToken(header, expected string) Guard {
	return func(ctx *Context) error {
		given := ctx.Header(header)
		if given == "" {
			return NewHTTPErrorf(http.StatusUnauthorized, "the %s header is required", header)
		}
		if !SecureCompare(given, expected) {
			return NewHTTPErrorf(http.StatusUnauthorized, "the %s header is not valid", header)
		}
		return nil
	}
}
