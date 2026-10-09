package muzak

import (
	"cmp"
	"crypto"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bounds and defaults of a [JWTBearer] scheme.
const (
	// DefaultJWTLeeway is how far a token's exp, nbf and iat may be off the
	// server's clock and still be believed, unless [JWTOptions.Leeway] says
	// otherwise.
	DefaultJWTLeeway = 60 * time.Second
	// MaxJWTLeeway is the most [JWTOptions.Leeway] may be. Leeway exists for
	// clocks that disagree by seconds; minutes of it is an expired token
	// accepted for minutes.
	MaxJWTLeeway = 5 * time.Minute
	// defaultBearerRealm is the realm a challenge names when none is
	// configured, the one [RequireBearerToken] has always used.
	defaultBearerRealm = "api"
	// maxNumericDate is the end of the year 9999, past which a date is not a
	// date anyone means.
	maxNumericDate = 253402300799
	// maxNumericDateText bounds the text of a number before it is parsed,
	// since a number of thousands of digits costs more to parse than its
	// value is worth.
	maxNumericDateText = 32
)

// JWTOptions configures [JWTBearer].
//
// There are no defaults for what decides whose tokens are accepted: the
// issuers, the audience and the algorithms all have to be named, and a scheme
// that leaves one of them out is a build error rather than a scheme that
// accepts more than its author meant.
type JWTOptions struct {
	// Issuers lists the issuers a token's iss claim must equal, compared
	// exactly. It is required: a token signed by a key this scheme trusts but
	// issued by someone else for some other purpose is refused.
	//
	// Every key of the scheme is trusted for every issuer listed, so a token
	// signed by one issuer's key may name another listed issuer as its own.
	// List several only when they share their keys, as the issuer spellings of
	// one identity provider do, and give each identity provider a scheme of
	// its own, offered as alternatives in [WithSecurity].
	Issuers []string

	// Audience is the value a token's aud claim must contain, as a string or
	// as one entry of an array, which is how a token says which API it was
	// issued for. It is required, because without it a token issued for any
	// other API of the same identity provider would be accepted here.
	Audience string

	// Algorithms lists the JWS algorithms a token may be signed with, from
	// HS256, HS384, HS512, RS256, RS384, RS512, PS256, PS384, PS512, ES256,
	// ES384, ES512, EdDSA and Ed25519. It is required, and a token whose
	// header names any other algorithm is refused before a key is looked at.
	// "none" is never accepted, here or in a token.
	//
	// Name only what the issuer uses. A key is used only by algorithms of its
	// own kind, so an RSA public key is never treated as an HMAC secret, but
	// every algorithm allowed is one more an attacker may choose.
	Algorithms []string

	// Keys are keys configured in the application, checked when it is built.
	// They are consulted before the key set at JWKS, and either may be used
	// alone.
	Keys []JWTKey

	// JWKS fetches the issuer's keys from a JSON Web Key Set and keeps them
	// fresh. It is unused unless its URL is set; see [JWKSOptions].
	JWKS JWKSOptions

	// Leeway is how far exp, nbf and iat may be off the server's clock. Zero
	// means [DefaultJWTLeeway], a negative value means none, and more than
	// [MaxJWTLeeway] is a build error.
	Leeway time.Duration

	// AllowNoExpiry accepts a token that has no exp claim. A token without
	// one is valid until its key is retired, so it is off by default, and is
	// only for an issuer that revokes tokens some other way.
	AllowNoExpiry bool

	// Types lists the typ header values accepted, compared without regard to
	// case and with any "application/" prefix ignored. It defaults to "JWT"
	// and "at+jwt", RFC 9068's type for an access token. A token with no typ
	// is accepted whatever this says.
	Types []string

	// MaxTokenBytes bounds the length of a token, which is refused unread when
	// it is longer. It defaults to 8 KiB and may be raised to 64 KiB, for an
	// issuer that puts large claims in its tokens.
	MaxTokenBytes int

	// Realm is the realm the WWW-Authenticate challenge of a refused request
	// names, defaulting to "api". It is quoted in a header, so it may hold
	// only printable ASCII other than a double quote and a backslash.
	Realm string

	// Description explains the scheme in the OpenAPI document, as CommonMark.
	Description string

	// ResourceMetadata, when set, publishes RFC 9728 protected resource
	// metadata for the API and points to it from every challenge, which is
	// how an OAuth client, an MCP client among them, discovers where to get
	// a token. It is nil by default, which publishes nothing; see
	// [ResourceMetadata].
	ResourceMetadata *ResourceMetadata
}

// JWTKey is one key a [JWTBearer] scheme verifies signatures with. Exactly one
// of Key and Secret is set.
type JWTKey struct {
	// ID is the kid a token names this key by. A key with an ID verifies only
	// tokens that name it, and a key without one verifies only tokens that
	// name no key, so that a token cannot be steered to a key that was not
	// meant for it. IDs must be unique among the keys.
	ID string

	// Key is a public key: an *rsa.PublicKey of at least 2048 bits, an
	// *ecdsa.PublicKey on P-256, P-384 or P-521, or an ed25519.PublicKey. It
	// is used only by the algorithms of its own kind, and an ECDSA key only
	// by the algorithm of its curve.
	Key crypto.PublicKey

	// Secret is an HMAC secret, used only by the HS algorithms. It must be at
	// least as long as the output of every hash it may be used with, 32 bytes
	// for HS256 and 64 for HS512, and should be random: a token signed with a
	// guessable secret can be forged offline.
	Secret []byte

	// Algorithm, when set, is the only algorithm this key verifies, which
	// must be one of JWTOptions.Algorithms. It is how an HMAC secret of 32
	// bytes is used for HS256 alone while HS512 is allowed for another key.
	Algorithm string
}

// Claims is what a verified JWT says about its subject: the registered claims
// read and checked by [JWTBearer], and the whole payload for anything else.
//
// A route behind a JWTBearer scheme receives it through [From], [TryFrom] or a
// [Dep] field of type *Claims, and its custom claims through [ClaimsAs]. A new
// Claims is made for every request and shared with none.
type Claims struct {
	// Scheme is the name of the security scheme that verified the token, as
	// it is declared in [OpenAPIOptions.SecuritySchemes].
	Scheme string
	// Issuer is the iss claim, one of [JWTOptions.Issuers].
	Issuer string
	// Subject is the sub claim, usually the identity of the user or client
	// the token was issued to. It is empty when the token has none.
	Subject string
	// Audience is the aud claim, as a list even when the token wrote one
	// string.
	Audience []string
	// ExpiresAt, NotBefore and IssuedAt are the exp, nbf and iat claims, and
	// are zero when the token has none.
	ExpiresAt time.Time
	NotBefore time.Time
	IssuedAt  time.Time
	// ID is the jti claim.
	ID string
	// Scopes are the scopes the token grants, from a scope claim of
	// space-separated values, RFC 8693's form, or an scp claim that is an
	// array or such a string, the form some identity providers use.
	Scopes []string
	// KeyID and Algorithm are the kid and alg of the token's header.
	KeyID     string
	Algorithm string
	// Raw is the payload as it was signed, a JSON object, for claims Muzak
	// does not read. [ClaimsAs] decodes it into a type of the application's
	// own.
	Raw jsontext.Value

	// decoded caches what ClaimsAs decoded, by type. It is a pointer so that
	// a Claims copied by value still shares it, and holds one entry for each
	// distinct type a program decodes into.
	decoded *claimsCache
}

// claimsCache is what [ClaimsAs] has decoded from one token.
type claimsCache struct {
	mu      sync.Mutex
	entries []decodedClaims
}

// decodedClaims is one type's decoding of a token's payload.
type decodedClaims struct {
	typ reflect.Type
	val any
	err error
}

// HasScope reports whether the token grants the scope.
func (c *Claims) HasScope(scope string) bool {
	return c != nil && slices.Contains(c.Scopes, scope)
}

// errNoClaims is why [ClaimsAs] fails on a request that carried no verified
// token. It is logged, and the client is told only that it is not authorized.
var errNoClaims = errors.New("muzak: ClaimsAs found no verified token for this request; " +
	"the route names no JWTBearer scheme in WithSecurity, or it was admitted by an alternative that is not one")

// ClaimsAs decodes the payload of the request's verified JWT into a type of the
// application's own, for the claims [Claims] does not read:
//
//	type TenantClaims struct {
//		Tenant string   `json:"tid"`
//		Roles  []string `json:"roles"`
//	}
//
//	tenant, err := muzak.ClaimsAs[TenantClaims](ctx)
//
// The payload is decoded once per request and type, and later calls return
// the same value, so a provider and the handler may both ask. The value is
// decoded as JSON with encoding/json/v2, members T does not name are ignored,
// and a value of the wrong type fails it.
//
// It fails with a 401, its cause logged, when the request carries no verified
// token, which is the case on a route that names no [JWTBearer] scheme and on
// one admitted by an alternative that does not, and when the payload does not
// decode into T. A handler can return that error as it is.
//
// To hand the result to a handler as a dependency, wrap it in a provider:
//
//	muzak.Needs(func(ctx *muzak.Context) (TenantClaims, error) {
//		return muzak.ClaimsAs[TenantClaims](ctx)
//	})
func ClaimsAs[T any](ctx *Context) (T, error) {
	var zero T
	claims, _ := TryFrom[*Claims](ctx)
	if claims == nil || claims.decoded == nil {
		return zero, Unauthorized("").Wrap(errNoClaims)
	}
	typ := reflect.TypeFor[T]()
	cache := claims.decoded
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, entry := range cache.entries {
		if entry.typ == typ {
			v, _ := entry.val.(T)
			return v, entry.err
		}
	}
	var out T
	err := json.Unmarshal(claims.Raw, &out)
	if err != nil {
		out = zero
		err = Unauthorized("").Wrap(fmt.Errorf("muzak: the token's claims do not decode into %s: %w", typ, err))
	}
	cache.entries = append(cache.entries, decodedClaims{typ: typ, val: out, err: err})
	return out, err
}

// JWTBearer is a security scheme that verifies a JWT sent as a bearer token,
// "Authorization: Bearer <token>", and also describes it in the OpenAPI
// document exactly as BearerAuth("JWT") does:
//
//	app := muzak.New(muzak.AppOptions{
//		SecuritySchemes: map[string]muzak.SecurityScheme{
//			"oidc": muzak.JWTBearer(muzak.JWTOptions{
//				Issuers:    []string{"https://login.example.com/"},
//				Audience:   "https://api.example.com",
//				Algorithms: []string{"RS256"},
//				JWKS:       muzak.JWKSOptions{URL: "https://login.example.com/.well-known/jwks.json"},
//			}),
//		},
//	})
//	r.Get("/me", me, muzak.WithSecurity(muzak.Require("oidc", "profile:read")))
//
// A route that names it in [WithSecurity] refuses a request without a valid
// token before any guard, provider or handler runs, and hands the verified
// [Claims] to the rest; see WithSecurity for how requirements combine. A token
// is valid when it is a JWS compact serialization no longer than
// MaxTokenBytes, in strict base64url, with a header naming an allowed
// algorithm, no crit, jku, jwk, x5u or x5c member, and an accepted typ; when
// its signature verifies with a key of the algorithm's own kind that the
// token's kid selects; and when its claims name an allowed issuer and the
// audience, its exp is in the future and its nbf and iat are not, each within
// the leeway. A token that fails any of these is refused with the same 401,
// and nothing says which check failed.
//
// Options that cannot be enforced as written, such as a missing issuer, an
// unknown algorithm, an RSA key under 2048 bits or an HMAC secret shorter than
// its hash, are reported when the application is built.
func JWTBearer(opts JWTOptions) SecurityScheme {
	spec, errs := newJWTSpec(opts)
	return SecurityScheme{
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "JWT",
		Description:  opts.Description,
		verifier:     &verifierSpec{jwt: spec, errs: errs},
	}
}

// jwtSpec is a [JWTBearer] scheme's options checked and compiled into what
// verifying needs. It is immutable, and shared by every application the
// scheme is declared in; what changes while an application runs, the key set
// fetched from JWKS, belongs to the application. See [jwtScheme].
type jwtSpec struct {
	policy     jwsPolicy
	static     *keySet
	issuers    []string
	audience   string
	leeway     time.Duration
	requireExp bool
	realm      string
	jwks       *JWKSOptions
	metadata   *resourceMetadataSpec
	// now is the clock, replaced in tests.
	now func() time.Time
}

// newJWTSpec checks every option and compiles them, returning every problem
// found rather than the first.
func newJWTSpec(opts JWTOptions) (*jwtSpec, []error) {
	var errs []error
	spec := &jwtSpec{
		issuers:    slices.Clone(opts.Issuers),
		audience:   opts.Audience,
		requireExp: !opts.AllowNoExpiry,
		realm:      cmp.Or(opts.Realm, defaultBearerRealm),
		now:        time.Now,
	}
	if len(opts.Issuers) == 0 {
		errs = append(errs, errors.New("JWTOptions.Issuers names no issuer; name the iss value of every issuer whose tokens are accepted"))
	}
	for i, issuer := range opts.Issuers {
		if issuer == "" {
			errs = append(errs, fmt.Errorf("JWTOptions.Issuers[%d] is empty", i))
		}
	}
	if opts.Audience == "" {
		errs = append(errs, errors.New("JWTOptions.Audience is empty; name the aud value tokens for this API carry, "+
			"or a token issued for any other API of the same issuer would be accepted"))
	}
	spec.policy, errs = compileJWSPolicy(opts, errs)

	switch {
	case opts.Leeway == 0:
		spec.leeway = DefaultJWTLeeway
	case opts.Leeway < 0:
		spec.leeway = 0
	case opts.Leeway > MaxJWTLeeway:
		errs = append(errs, fmt.Errorf("JWTOptions.Leeway is %s, over the limit of %s", opts.Leeway, MaxJWTLeeway))
	default:
		spec.leeway = opts.Leeway
	}
	if !isQuotableHeaderText(spec.realm) {
		errs = append(errs, fmt.Errorf("JWTOptions.Realm %q cannot be quoted in a header; use printable ASCII without '\"' or '\\'", spec.realm))
	}

	spec.static, errs = compileStaticKeys(opts.Keys, spec.policy, errs)
	if opts.JWKS.URL != "" {
		jwks, problems := opts.JWKS.normalize()
		errs = append(errs, problems...)
		spec.jwks = &jwks
	} else if opts.JWKS.configured() {
		errs = append(errs, errors.New("JWTOptions.JWKS is configured but has no URL"))
	}
	if len(opts.Keys) == 0 && opts.JWKS.URL == "" {
		errs = append(errs, errors.New("JWTOptions names no key; set Keys, or JWKS.URL to fetch the issuer's keys"))
	}
	if opts.ResourceMetadata != nil {
		meta, problems := opts.ResourceMetadata.compile(spec.issuers)
		errs = append(errs, problems...)
		spec.metadata = meta
	}
	return spec, errs
}

// compileJWSPolicy checks the options that decide whether a token's structure
// is acceptable.
func compileJWSPolicy(opts JWTOptions, errs []error) (jwsPolicy, []error) {
	p := jwsPolicy{maxBytes: defaultMaxTokenBytes, algorithms: map[string]*jwsAlgorithm{}}
	if len(opts.Algorithms) == 0 {
		errs = append(errs, errors.New("JWTOptions.Algorithms names no algorithm; name the ones the issuer signs with, such as RS256"))
	}
	for _, name := range opts.Algorithms {
		alg := jwsAlgorithms[name]
		switch {
		case strings.EqualFold(name, "none"):
			errs = append(errs, errors.New(`JWTOptions.Algorithms names "none"; an unsigned token is never accepted`))
		case alg == nil:
			errs = append(errs, fmt.Errorf("JWTOptions.Algorithms names %q, which is not one Muzak verifies; use HS256, HS384, HS512, "+
				"RS256, RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512, EdDSA or Ed25519", name))
		default:
			p.algorithms[name] = alg
		}
	}
	switch {
	case opts.MaxTokenBytes == 0:
	case opts.MaxTokenBytes < 0 || opts.MaxTokenBytes > maxTokenBytesCeiling:
		errs = append(errs, fmt.Errorf("JWTOptions.MaxTokenBytes is %d; it must be between 1 and %d", opts.MaxTokenBytes, maxTokenBytesCeiling))
	default:
		p.maxBytes = opts.MaxTokenBytes
	}
	types := opts.Types
	if types == nil {
		types = []string{"JWT", "at+jwt"}
	}
	for i, typ := range types {
		if typ == "" {
			errs = append(errs, fmt.Errorf("JWTOptions.Types[%d] is empty", i))
			continue
		}
		p.types = append(p.types, normalizeJOSEType(typ))
	}
	return p, errs
}

// compileStaticKeys checks every configured key and builds the set tokens are
// looked up in.
func compileStaticKeys(keys []JWTKey, p jwsPolicy, errs []error) (*keySet, []error) {
	if len(keys) == 0 {
		return nil, errs
	}
	order := slices.SortedFunc(maps.Values(p.algorithms), func(a, b *jwsAlgorithm) int { return strings.Compare(a.name, b.name) })
	set := &keySet{}
	seen := map[string]int{}
	for i, key := range keys {
		k, err := staticKey(key, p.algorithms, order)
		if err != nil {
			errs = append(errs, fmt.Errorf("JWTOptions.Keys[%d]: %w", i, err))
			continue
		}
		if k.hasID {
			if first, dup := seen[k.id]; dup {
				errs = append(errs, fmt.Errorf("JWTOptions.Keys[%d] has the ID %q, as Keys[%d] does; IDs must be unique", i, k.id, first))
				continue
			}
			seen[k.id] = i
		}
		set.add(k)
	}
	return set, errs
}

// isQuotableHeaderText reports whether s can be written inside a quoted string
// in a header without escaping: printable ASCII, with no double quote and no
// backslash.
func isQuotableHeaderText(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// registeredClaims are the claims [JWTBearer] reads, each kept raw so that its
// type is checked here rather than coerced.
type registeredClaims struct {
	Iss   jsontext.Value `json:"iss"`
	Sub   jsontext.Value `json:"sub"`
	Aud   jsontext.Value `json:"aud"`
	Exp   jsontext.Value `json:"exp"`
	Nbf   jsontext.Value `json:"nbf"`
	Iat   jsontext.Value `json:"iat"`
	Jti   jsontext.Value `json:"jti"`
	Scope jsontext.Value `json:"scope"`
	Scp   jsontext.Value `json:"scp"`
}

// readClaims checks the payload of a token whose signature has verified, and
// returns its claims. Every check refuses with the same error.
func (s *jwtSpec) readClaims(t *jwsToken) (*Claims, error) {
	if err := checkJSONObject(t.payload, maxClaimsDepth); err != nil {
		return nil, errInvalidToken
	}
	var rc registeredClaims
	if err := json.Unmarshal(t.payload, &rc); err != nil {
		// coverage: checkJSONObject has just read the payload as one valid
		// object, and every claim is decoded into a raw jsontext.Value, which
		// holds any value, so decoding it cannot fail.
		return nil, errInvalidToken
	}
	claims := &Claims{KeyID: t.kid, Algorithm: t.alg.name, Raw: jsontext.Value(t.payload), decoded: &claimsCache{}}
	var ok bool
	if claims.Issuer, ok = jsonString(rc.Iss); !ok || !slices.Contains(s.issuers, claims.Issuer) {
		return nil, errInvalidToken
	}
	if claims.Audience, ok = stringOrStrings(rc.Aud); !ok || !slices.Contains(claims.Audience, s.audience) {
		return nil, errInvalidToken
	}
	if err := s.checkTimes(&rc, claims); err != nil {
		return nil, err
	}
	if len(rc.Sub) > 0 {
		if claims.Subject, ok = jsonString(rc.Sub); !ok {
			return nil, errInvalidToken
		}
	}
	if len(rc.Jti) > 0 {
		if claims.ID, ok = jsonString(rc.Jti); !ok {
			return nil, errInvalidToken
		}
	}
	if claims.Scopes, ok = readScopes(rc.Scope, rc.Scp); !ok {
		return nil, errInvalidToken
	}
	return claims, nil
}

// checkTimes checks exp, nbf and iat against the clock, each with the leeway.
// A token is expired from the instant exp names, so with no leeway a token
// whose exp is now is already refused.
func (s *jwtSpec) checkTimes(rc *registeredClaims, claims *Claims) error {
	now := s.now()
	var ok bool
	if len(rc.Exp) > 0 {
		if claims.ExpiresAt, ok = numericDate(rc.Exp); !ok || !now.Before(claims.ExpiresAt.Add(s.leeway)) {
			return errInvalidToken
		}
	} else if s.requireExp {
		return errInvalidToken
	}
	if len(rc.Nbf) > 0 {
		if claims.NotBefore, ok = numericDate(rc.Nbf); !ok || now.Add(s.leeway).Before(claims.NotBefore) {
			return errInvalidToken
		}
	}
	if len(rc.Iat) > 0 {
		// A token issued in the future was issued by a clock that is wrong, or
		// by someone choosing its dates; either way its exp is not to be
		// trusted to mean what it says.
		if claims.IssuedAt, ok = numericDate(rc.Iat); !ok || now.Add(s.leeway).Before(claims.IssuedAt) {
			return errInvalidToken
		}
	}
	return nil
}

// numericDate reads a NumericDate, RFC 7519's count of seconds since the
// epoch: a JSON number, possibly fractional, that is neither negative nor past
// the year 9999. A date written as a string, a number too long to be one and
// a number too large to represent are all refused.
func numericDate(v jsontext.Value) (time.Time, bool) {
	if v.Kind() != '0' || len(v) > maxNumericDateText {
		return time.Time{}, false
	}
	f, err := strconv.ParseFloat(string(v), 64)
	if err != nil || f < 0 || f > maxNumericDate {
		return time.Time{}, false
	}
	whole := math.Floor(f)
	return time.Unix(int64(whole), int64((f-whole)*1e9)), true
}

// stringOrStrings reads a claim that may be one string or an array of them,
// as aud may. An array must hold at least one entry, every one a string.
func stringOrStrings(v jsontext.Value) ([]string, bool) {
	switch v.Kind() {
	case '"':
		s, ok := jsonString(v)
		return []string{s}, ok
	case '[':
		var list []string
		if err := json.Unmarshal(v, &list); err != nil || len(list) == 0 {
			return nil, false
		}
		return list, true
	}
	return nil, false
}

// readScopes collects the scopes a token grants from scope, a space-separated
// string, and scp, an array or such a string. Both are optional, and a token
// that carries both grants what either names.
func readScopes(scope, scp jsontext.Value) ([]string, bool) {
	var out []string
	if len(scope) > 0 {
		s, ok := jsonString(scope)
		if !ok {
			return nil, false
		}
		out = appendScopes(out, s)
	}
	if len(scp) > 0 {
		if scp.Kind() == '"' {
			s, _ := jsonString(scp)
			out = appendScopes(out, s)
		} else {
			var list []string
			if err := json.Unmarshal(scp, &list); err != nil {
				return nil, false
			}
			out = append(out, list...)
		}
	}
	return out, true
}

// appendScopes appends the space-separated scopes of s, skipping the empty
// strings that doubled spaces leave.
func appendScopes(out []string, s string) []string {
	for scope := range strings.SplitSeq(s, " ") {
		if scope != "" {
			out = append(out, scope)
		}
	}
	return out
}
