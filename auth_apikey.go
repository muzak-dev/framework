package muzak

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
)

// Bounds of an [APIKeyVerifier] scheme.
const (
	// minAPIKeyBytes is the shortest key [APIKeyOptions.Keys] accepts. A key
	// is a password nobody has to remember, so there is no reason for one
	// short enough to guess; sixteen random bytes, written in hex or base64,
	// are longer than this.
	minAPIKeyBytes = 16
	// maxAPIKeyBytes is the longest key read from a request. Anything longer
	// is refused without being hashed.
	maxAPIKeyBytes = 1 << 10
	// maxStaticAPIKeys bounds [APIKeyOptions.Keys], which every request is
	// compared against in full; a set larger than this belongs behind
	// [APIKeyOptions.Lookup].
	maxStaticAPIKeys = 1024
)

// APIKeyOptions configures [APIKeyVerifier].
type APIKeyOptions struct {
	// In is where the key is sent: "header", the default, "query" or
	// "cookie". A key in a query string is written to logs and browser
	// history along with the URL, so prefer a header.
	In string

	// Name is the header, query parameter or cookie the key is sent in, such
	// as "X-API-Key". It is required.
	Name string

	// Keys are the keys accepted, each naming the principal it identifies.
	// They are held as SHA-256 digests, and a request's key is hashed and
	// compared with every one of them in constant time, so neither which key
	// matched, nor how much of one did, nor how long one is shows in the
	// response time.
	Keys []APIKey

	// Lookup resolves a key that is not among Keys, such as one stored in a
	// database, and returns the principal it identifies, or nil when it
	// identifies nobody. It runs only for a request that presents a key no
	// static key matched. An error fails the request with 503 and is logged,
	// so it must not quote the key. Comparing the key in constant time, and
	// storing it hashed, is the lookup's to do; [SecureCompare] does the
	// first.
	Lookup func(ctx *Context, key string) (*APIKeyPrincipal, error)

	// Description explains the scheme in the OpenAPI document, as CommonMark.
	Description string
}

// APIKey is one key an [APIKeyVerifier] scheme accepts.
type APIKey struct {
	// ID identifies the principal the key belongs to, such as a client's
	// name. It is required, must be unique among the keys, and is what a
	// handler and the logs see in place of the key.
	ID string
	// Key is the secret itself, at least 16 bytes. Two principals may not
	// share one.
	Key string
	// Scopes are what the key grants, checked against the scopes a route's
	// [Require] names.
	Scopes []string
}

// APIKeyPrincipal is who an API key identified. A route behind an
// [APIKeyVerifier] scheme receives it through [From], [TryFrom] or a [Dep]
// field of type *APIKeyPrincipal. A new one is made for every request.
type APIKeyPrincipal struct {
	// Scheme is the name of the security scheme that verified the key.
	Scheme string
	// ID identifies the principal, as [APIKey.ID] or the lookup set it.
	ID string
	// Scopes are what the key grants.
	Scopes []string
}

// HasScope reports whether the key grants the scope.
func (p *APIKeyPrincipal) HasScope(scope string) bool {
	return p != nil && slices.Contains(p.Scopes, scope)
}

// APIKeyVerifier is a security scheme that verifies an API key, and also
// describes it in the OpenAPI document exactly as [APIKeyHeader],
// [APIKeyQuery] or [APIKeyCookie] does:
//
//	"partners": muzak.APIKeyVerifier(muzak.APIKeyOptions{
//		Name: "X-API-Key",
//		Keys: []muzak.APIKey{{ID: "acme", Key: settings.AcmeKey, Scopes: []string{"orders:read"}}},
//	}),
//
// A route that names it in [WithSecurity] refuses a request without a valid
// key before any guard, provider or handler runs, with a 401 that says nothing
// about why, and hands the verified [APIKeyPrincipal] to the rest. A request
// that sends the key twice, in two headers, parameters or cookies of the
// name, is refused, since which of the two a proxy in front would have read
// is not knowable here. Neither the key nor its digest is ever logged.
//
// Options that cannot be enforced, such as a short key, two principals sharing
// one or neither Keys nor Lookup, are reported when the application is built.
func APIKeyVerifier(opts APIKeyOptions) SecurityScheme {
	in := opts.In
	if in == "" {
		in = "header"
	}
	spec, errs := newAPIKeySpec(in, opts)
	return SecurityScheme{
		Type:        "apiKey",
		In:          in,
		Name:        opts.Name,
		Description: opts.Description,
		verifier:    &verifierSpec{apiKey: spec, errs: errs},
	}
}

// apiKeySpec is an [APIKeyVerifier] scheme's options checked and compiled.
type apiKeySpec struct {
	in   string
	name string
	// declared is the name as it was given, which the scheme describes.
	declared string
	keys     []apiKeyEntry
	lookup   func(ctx *Context, key string) (*APIKeyPrincipal, error)
}

// apiKeyEntry is one static key, held only as its digest.
type apiKeyEntry struct {
	digest [sha256.Size]byte
	id     string
	scopes []string
}

// newAPIKeySpec checks every option and compiles them, returning every problem
// found rather than the first.
func newAPIKeySpec(in string, opts APIKeyOptions) (*apiKeySpec, []error) {
	var errs []error
	spec := &apiKeySpec{in: in, name: opts.Name, declared: opts.Name, lookup: opts.Lookup}
	switch in {
	case "header":
		if !isHTTPToken(opts.Name) {
			errs = append(errs, errors.New("APIKeyOptions.Name must be a valid header name"))
		}
		spec.name = http.CanonicalHeaderKey(opts.Name)
	case "query", "cookie":
		if opts.Name == "" {
			errs = append(errs, fmt.Errorf("APIKeyOptions.Name must name the %s the key is sent in", in))
		}
	default:
		errs = append(errs, fmt.Errorf(`APIKeyOptions.In must be "header", "query" or "cookie", not %q`, in))
	}
	if len(opts.Keys) == 0 && opts.Lookup == nil {
		errs = append(errs, errors.New("APIKeyOptions names no key; set Keys, or Lookup to resolve keys yourself"))
	}
	if len(opts.Keys) > maxStaticAPIKeys {
		errs = append(errs, fmt.Errorf("APIKeyOptions.Keys holds %d keys, over the limit of %d; resolve a set that large with Lookup", len(opts.Keys), maxStaticAPIKeys))
	}
	ids := map[string]int{}
	digests := map[[sha256.Size]byte]int{}
	for i, key := range opts.Keys {
		entry := apiKeyEntry{digest: sha256.Sum256([]byte(key.Key)), id: key.ID, scopes: slices.Clone(key.Scopes)}
		switch {
		case key.ID == "":
			errs = append(errs, fmt.Errorf("APIKeyOptions.Keys[%d] has no ID", i))
		case len(key.Key) < minAPIKeyBytes:
			// The key itself is never quoted, here or anywhere else.
			errs = append(errs, fmt.Errorf("APIKeyOptions.Keys[%d] (%q) is %d bytes, shorter than the minimum of %d; use a random key", i, key.ID, len(key.Key), minAPIKeyBytes))
		case len(key.Key) > maxAPIKeyBytes:
			errs = append(errs, fmt.Errorf("APIKeyOptions.Keys[%d] (%q) is longer than the %d bytes read from a request, so it could never match", i, key.ID, maxAPIKeyBytes))
		}
		if first, dup := ids[key.ID]; dup && key.ID != "" {
			errs = append(errs, fmt.Errorf("APIKeyOptions.Keys[%d] has the ID %q, as Keys[%d] does; IDs must be unique", i, key.ID, first))
		}
		if first, dup := digests[entry.digest]; dup {
			errs = append(errs, fmt.Errorf("APIKeyOptions.Keys[%d] (%q) has the same key as Keys[%d] (%q); a key must identify one principal",
				i, key.ID, first, opts.Keys[first].ID))
		}
		for _, scope := range key.Scopes {
			if !isScopeToken(scope) {
				errs = append(errs, fmt.Errorf("APIKeyOptions.Keys[%d] (%q) grants %q, which is not a valid scope", i, key.ID, scope))
			}
		}
		ids[key.ID], digests[entry.digest] = i, i
		spec.keys = append(spec.keys, entry)
	}
	return spec, errs
}

// apiKeyScheme is an [APIKeyVerifier] scheme as one application runs it.
type apiKeyScheme struct {
	scheme string
	spec   *apiKeySpec
}

// newAPIKeyScheme builds an application's instance of an API key scheme. It
// holds nothing that changes, so it is the spec under a name.
func newAPIKeyScheme(name string, spec *apiKeySpec) *apiKeyScheme {
	return &apiKeyScheme{scheme: name, spec: spec}
}

func (s *apiKeyScheme) principalType() reflect.Type { return apiKeyPrincipalType }
func (s *apiKeyScheme) challenge() *bearerChallenge { return nil }

// varyOn names the header a key in a header or a cookie is read from. A key
// in the query is part of the URL a cache keys on already.
func (s *apiKeyScheme) varyOn() string {
	switch s.spec.in {
	case "header":
		return s.spec.name
	case "cookie":
		return "Cookie"
	}
	return ""
}

// values returns every value the request carries under the scheme's name.
func (s *apiKeyScheme) values(c *Context) []string {
	switch s.spec.in {
	case "header":
		return c.r.Header[s.spec.name]
	case "query":
		return c.r.URL.Query()[s.spec.name]
	}
	cookies := c.r.CookiesNamed(s.spec.name)
	if len(cookies) == 0 {
		return nil
	}
	values := make([]string, len(cookies))
	for i, cookie := range cookies {
		values[i] = cookie.Value
	}
	return values
}

// present reports whether the request names the key at all, empty or not.
func (s *apiKeyScheme) present(c *Context) bool {
	return len(s.values(c)) > 0
}

// verify checks the request's key against every static key, in constant time
// and without stopping at a match, and then against the lookup.
func (s *apiKeyScheme) verify(c *Context) schemeOutcome {
	invalid := schemeOutcome{verdict: verdictInvalid}
	values := s.values(c)
	if len(values) != 1 || values[0] == "" || len(values[0]) > maxAPIKeyBytes {
		return invalid
	}
	key := values[0]
	digest := sha256.Sum256([]byte(key))
	match := -1
	for i := range s.spec.keys {
		// Every key is compared, and the index is chosen without a branch,
		// so the time taken is the same whichever key matched, or none.
		equal := subtle.ConstantTimeCompare(digest[:], s.spec.keys[i].digest[:])
		match = subtle.ConstantTimeSelect(equal, i, match)
	}
	if match >= 0 {
		entry := &s.spec.keys[match]
		return schemeOutcome{verdict: verdictValid, principal: &APIKeyPrincipal{Scheme: s.scheme, ID: entry.id, Scopes: slices.Clone(entry.scopes)}, scopes: entry.scopes}
	}
	if s.spec.lookup == nil {
		return invalid
	}
	principal, err := s.spec.lookup(c, key)
	if err != nil {
		return schemeOutcome{verdict: verdictUnavailable, err: fmt.Errorf("muzak: the API key lookup of scheme %q failed: %w", s.scheme, err)}
	}
	if principal == nil {
		return invalid
	}
	// A copy, so that the lookup's value is not shared with whatever it
	// keeps, and named after this scheme whatever the lookup said.
	out := &APIKeyPrincipal{Scheme: s.scheme, ID: principal.ID, Scopes: slices.Clone(principal.Scopes)}
	return schemeOutcome{verdict: verdictValid, principal: out, scopes: out.Scopes}
}
