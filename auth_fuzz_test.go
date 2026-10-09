package muzak

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// fuzzPolicy allows every algorithm, so that the fuzzer reaches every branch
// of the parser.
func fuzzPolicy() *jwsPolicy {
	return &jwsPolicy{maxBytes: defaultMaxTokenBytes, algorithms: jwsAlgorithms, types: []string{"jwt", "at+jwt"}}
}

// FuzzParseJWS feeds the token parser arbitrary input. It must never panic,
// and whatever it accepts must be a token whose parts agree with each other:
// an allowed algorithm, a signature of the algorithm's exact length where it
// has one, and a signing input that is the token's own prefix.
func FuzzParseJWS(f *testing.F) {
	f.Add(signJWT(f, "RS256", testRSAKey(), "k1", nil))
	f.Add(signJWT(f, "ES256", testECKey256(), "", nil))
	f.Add(signJWT(f, "HS256", testHMACSecret32, "", map[string]any{"scope": "a b"}))
	f.Add(signJWT(f, "EdDSA", testEdKey(), "e", nil))
	f.Add("eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.")
	f.Add("a.b.c")
	f.Add("..")
	f.Add("eyJhbGciOiJSUzI1NiIsImFsZyI6IkhTMjU2In0.e30.AA")
	f.Add(strings.Repeat("A", 100) + "." + strings.Repeat("B", 100) + "." + strings.Repeat("C", 100))
	policy := fuzzPolicy()
	scheme := &jwtSpec{
		policy:   *policy,
		issuers:  []string{testIssuer},
		audience: testAudience,
		leeway:   DefaultJWTLeeway,
		now:      func() time.Time { return testNow },
	}
	keys := []*verifyKey{
		{family: familyHMAC, secret: testHMACSecret},
		{family: familyRSA, rsa: &testRSAKey().PublicKey},
		{family: familyEC, ec: &testECKey256().PublicKey},
		{family: familyEd25519, ed: publicOf(testEdKey()).(ed25519.PublicKey)},
	}
	f.Fuzz(func(t *testing.T, token string) {
		parsed, err := parseJWS(token, policy)
		if err != nil {
			return
		}
		if parsed.alg == nil || jwsAlgorithms[parsed.alg.name] != parsed.alg {
			t.Fatalf("accepted an algorithm that is not allowed: %+v", parsed.alg)
		}
		if parsed.alg.sigLen != 0 && len(parsed.signature) != parsed.alg.sigLen {
			t.Fatalf("accepted a %s signature of %d bytes", parsed.alg.name, len(parsed.signature))
		}
		if !strings.HasPrefix(token, parsed.signingInput+".") || strings.Count(token, ".") != 2 {
			t.Fatalf("the signing input %q is not the token's prefix", parsed.signingInput)
		}
		if parsed.hasKid && (parsed.kid == "" || len(parsed.kid) > maxKeyIDBytes) {
			t.Fatalf("accepted the kid %q", parsed.kid)
		}
		// Neither checking the signature nor reading the claims may panic,
		// whatever the token holds.
		verifyWith(keys, parsed)
		if claims, err := scheme.readClaims(parsed); err == nil {
			if claims.Issuer != testIssuer || !slices.Contains(claims.Audience, testAudience) {
				t.Fatalf("accepted claims for another issuer or audience: %+v", claims)
			}
		}
	})
}

// FuzzJWTClaims signs arbitrary header and payload JSON with a key the scheme
// holds and verifies the result through the scheme, so that the fuzzer works
// on what a signature protects rather than stopping at it. It must never
// panic, and anything accepted must name the issuer and audience and not be
// expired.
func FuzzJWTClaims(f *testing.F) {
	f.Add([]byte(`{"alg":"HS256","typ":"JWT"}`), mustJSON(f, testClaims(nil)))
	f.Add([]byte(`{"alg":"HS256"}`), mustJSON(f, testClaims(map[string]any{"aud": []string{"x", testAudience}, "scp": []string{"a"}})))
	f.Add([]byte(`{"alg":"HS256","crit":["b64"],"b64":false}`), []byte(`{}`))
	f.Add([]byte(`{"alg":"HS256"}`), []byte(`{"iss":"`+testIssuer+`","aud":"`+testAudience+`","exp":1e400}`))
	f.Add([]byte(`{"alg":"HS256"}`), []byte(`{"iss":"`+testIssuer+`","aud":"`+testAudience+`","exp":1900000000.5,"nbf":-1}`))
	opts := jwtOptions("HS256")
	opts.Keys = []JWTKey{{Secret: testHMACSecret32}}
	spec, errs := newJWTSpec(opts)
	if len(errs) > 0 {
		f.Fatal(errs)
	}
	spec.now = func() time.Time { return testNow }
	scheme := newJWTScheme("jwt", spec, nil)
	f.Fuzz(func(t *testing.T, header, payload []byte) {
		token := tokenFrom(t, "HS256", testHMACSecret32, header, payload)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		outcome := scheme.verify(&Context{r: req})
		if outcome.verdict != verdictValid {
			return
		}
		claims := outcome.principal.(*Claims)
		if claims.Issuer != testIssuer || !slices.Contains(claims.Audience, testAudience) || claims.Scheme != "jwt" {
			t.Fatalf("accepted %+v", claims)
		}
		if claims.ExpiresAt.IsZero() || !testNow.Before(claims.ExpiresAt.Add(DefaultJWTLeeway)) {
			t.Fatalf("accepted a token that is expired or never expires: %v", claims.ExpiresAt)
		}
	})
}

// FuzzParseJWKS feeds the key set reader arbitrary documents. It must never
// panic, never hold more keys than it was allowed, and every key it keeps must
// be usable by an allowed algorithm.
func FuzzParseJWKS(f *testing.F) {
	f.Add(mustJSON(f, map[string]any{"keys": []any{k1(), k2()}}))
	f.Add(mustJSON(f, map[string]any{"keys": []any{ecJWK(&testECKey256().PublicKey, map[string]any{"kid": "e"})}}))
	f.Add(mustJSON(f, map[string]any{"keys": []any{edJWK(publicOf(testEdKey()).(ed25519.PublicKey), nil)}}))
	f.Add([]byte(`{"keys":[{"kty":"oct","k":"AAAA"},{"kty":"RSA","n":"AQAB","e":"AQAB"}]}`))
	f.Add([]byte(`{"keys":[{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}]}`))
	f.Add([]byte(`{"keys":[],"keys":[]}`))
	f.Add([]byte(`[[[[[[[[[[`))
	const limit = 4
	f.Fuzz(func(t *testing.T, data []byte) {
		set, skipped, err := parseJWKS(data, limit, allowedForJWKTests)
		if err != nil {
			return
		}
		if set.count+skipped > limit || skipped < 0 {
			t.Fatalf("held %d keys and skipped %d, over the limit of %d", set.count, skipped, limit)
		}
		all := slices.Clone(set.unnamed)
		for _, keys := range set.byID {
			all = append(all, keys...)
		}
		for _, k := range all {
			usable := false
			for _, alg := range allowedForJWKTests {
				usable = usable || k.accepts(alg)
			}
			if !usable {
				t.Fatalf("kept a key no allowed algorithm uses: %+v", k)
			}
		}
	})
}

// FuzzCacheTTL feeds the Cache-Control reader arbitrary values. Whatever the
// header says, the result stays within the bounds.
func FuzzCacheTTL(f *testing.F) {
	for _, seed := range []string{"max-age=600", "no-store", `max-age="7"`, "max-age=99999999999999999999", ",,,=,max-age", "public, max-age=0"} {
		f.Add(seed)
	}
	lo, hi := time.Minute, time.Hour
	f.Fuzz(func(t *testing.T, value string) {
		h := http.Header{}
		h.Add("Cache-Control", value)
		if got := cacheTTL(h, lo, hi); got < lo || got > hi {
			t.Fatalf("cacheTTL(%q) = %s, outside [%s, %s]", value, got, lo, hi)
		}
	})
}
