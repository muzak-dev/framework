package muzak

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestJWTBearerVerifiesEveryAlgorithm signs a token with a real key of every
// algorithm the scheme supports and checks that each is accepted, with the
// claims handed to the route.
func TestJWTBearerVerifiesEveryAlgorithm(t *testing.T) {
	cases := []struct {
		alg string
		key any
	}{
		{"HS256", testHMACSecret32},
		{"HS384", testHMACSecret},
		{"HS512", testHMACSecret},
		{"RS256", testRSAKey()},
		{"RS384", testRSAKey()},
		{"RS512", testRSAKey()},
		{"PS256", testRSAKey()},
		{"PS384", testRSAKey()},
		{"PS512", testRSAKey()},
		{"ES256", testECKey256()},
		{"ES384", testECKey384()},
		{"ES512", testECKey521()},
		{"EdDSA", testEdKey()},
		{"Ed25519", testEdKey()},
	}
	for _, tc := range cases {
		t.Run(tc.alg, func(t *testing.T) {
			opts := jwtOptions(tc.alg)
			if secret, ok := tc.key.([]byte); ok {
				opts.Keys = []JWTKey{{Secret: secret}}
			} else {
				opts.Keys = []JWTKey{{Key: publicOf(tc.key)}}
			}
			app := mustBuild(t, jwtApp(t, opts))
			rec := withBearer(t, app, http.MethodGet, "/me", signJWT(t, tc.alg, tc.key, "", nil))
			assertStatus(t, rec, http.StatusOK)
			assertJSON(t, rec, `{"subject":"user-1","scheme":"jwt","scopes":[]}`)
			// A token signed by another key of the same kind is refused.
			var other any
			switch tc.alg[:2] {
			case "HS":
				other = []byte(strings.Repeat("z", 64))
			case "RS", "PS":
				other = testRSAKeyOther()
			case "ES":
				other = mustECKey(tc.key.(*ecdsa.PrivateKey).Curve)
			default:
				_, k, _ := ed25519GenerateKey()
				other = k
			}
			forged := signJWT(t, tc.alg, other, "", nil)
			assertRefused(t, withBearer(t, app, http.MethodGet, "/me", forged), forged)
		})
	}
}

// TestJWTBearerRefusesNone covers the unsigned token in every spelling: it is
// never accepted, and the scheme cannot be configured to accept it.
func TestJWTBearerRefusesNone(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions("RS256", "HS256")))
	payload := mustJSON(t, testClaims(nil))
	for _, alg := range []string{"none", "None", "NONE", "nOnE"} {
		header := mustJSON(t, map[string]any{"alg": alg, "typ": "JWT"})
		unsigned := b64(header) + "." + b64(payload) + "."
		assertRefused(t, withBearer(t, app, http.MethodGet, "/me", unsigned), unsigned)
		// With something in the signature slot, it is still not an algorithm.
		signed := b64(header) + "." + b64(payload) + "." + b64([]byte("signature"))
		assertRefused(t, withBearer(t, app, http.MethodGet, "/me", signed), signed)
	}
	msg := buildError(t, jwtApp(t, jwtOptions("RS256", "none")))
	if !strings.Contains(msg, `JWTOptions.Algorithms names "none"`) {
		t.Fatalf("build error = %s", msg)
	}
}

// TestJWTBearerRefusesAlgorithmConfusion is the classic attack on a verifier
// that lets the token pick the algorithm: an HS256 token whose HMAC secret is
// the server's RSA public key, which the attacker knows. A key is only ever
// used by algorithms of its own kind, so the RSA key is never an HMAC secret.
func TestJWTBearerRefusesAlgorithmConfusion(t *testing.T) {
	pub := &testRSAKey().PublicKey
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	app := mustBuild(t, jwtApp(t, jwtOptions("RS256", "HS256", "HS512")))
	for _, secret := range [][]byte{der, pemBytes, pub.N.Bytes(), x509.MarshalPKCS1PublicKey(pub)} {
		for _, alg := range []string{"HS256", "HS512"} {
			forged := signJWT(t, alg, secret, "", nil)
			assertRefused(t, withBearer(t, app, http.MethodGet, "/me", forged), forged)
		}
	}
	// The same with a JWKS-shaped kid: the kid selects the RSA key, which
	// still does not verify an HMAC.
	opts := jwtOptions("RS256", "HS256")
	opts.Keys = []JWTKey{{ID: "rsa", Key: pub}}
	app = mustBuild(t, jwtApp(t, opts))
	forged := signJWT(t, "HS256", der, "rsa", nil)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", forged), forged)
	// And an ECDSA key's encoding is no more an HMAC secret than an RSA one.
	ecDER, _ := x509.MarshalPKIXPublicKey(&testECKey256().PublicKey)
	opts = jwtOptions("ES256", "HS256")
	opts.Keys = []JWTKey{{Key: &testECKey256().PublicKey}}
	app = mustBuild(t, jwtApp(t, opts))
	forged = signJWT(t, "HS256", ecDER, "", nil)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", forged), forged)
}

// TestJWTBearerRefusesAlgorithmsNotAllowed checks that the header's alg is
// matched against the allowlist before any key is looked at, so that a key
// the scheme holds is never used with an algorithm it was not configured for.
func TestJWTBearerRefusesAlgorithmsNotAllowed(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions("RS256")))
	for _, alg := range []string{"RS384", "RS512", "PS256", "rs256", "RS256 ", "HS256"} {
		key := any(testRSAKey())
		signAlg := strings.TrimSpace(strings.ToUpper(alg))
		if signAlg == "HS256" {
			key = testHMACSecret32
		}
		header := mustJSON(t, map[string]any{"alg": alg})
		token := tokenFrom(t, signAlg, key, header, mustJSON(t, testClaims(nil)))
		assertRefused(t, withBearer(t, app, http.MethodGet, "/me", token), token)
	}
	// An algorithm Muzak does not implement is a build error.
	msg := buildError(t, jwtApp(t, jwtOptions("RS256", "ES256K", "RSA-OAEP")))
	for _, want := range []string{`names "ES256K"`, `names "RSA-OAEP"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("build error lacks %s:\n%s", want, msg)
		}
	}
	// So is an empty allowlist: there is no default.
	opts := jwtOptions()
	opts.Algorithms = nil
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "JWTOptions.Algorithms names no algorithm") {
		t.Fatalf("build error = %s", msg)
	}
}

// TestJWTBearerMatchesKeyTypeToAlgorithm checks that a key verifies only the
// algorithms of its kind and, for ECDSA, of its curve.
func TestJWTBearerMatchesKeyTypeToAlgorithm(t *testing.T) {
	opts := jwtOptions("RS256", "ES256", "ES384", "EdDSA")
	opts.Keys = []JWTKey{{Key: &testRSAKey().PublicKey}, {Key: &testECKey256().PublicKey}}
	app := mustBuild(t, jwtApp(t, opts))
	// Each of these is signed correctly, with a key the scheme does not hold
	// for that algorithm.
	for _, tc := range []struct {
		alg string
		key any
	}{
		{"ES384", testECKey384()}, // no P-384 key configured
		{"EdDSA", testEdKey()},    // no Ed25519 key configured
	} {
		token := signJWT(t, tc.alg, tc.key, "", nil)
		assertRefused(t, withBearer(t, app, http.MethodGet, "/me", token), token)
	}
	// A P-256 key claimed to be ES384: the header says ES384 and the
	// signature is P-256 width, which is refused before any key.
	header := mustJSON(t, map[string]any{"alg": "ES384"})
	input := b64(header) + "." + b64(mustJSON(t, testClaims(nil)))
	sig := signWith(t, "ES256", testECKey256(), input)
	token := input + "." + b64(sig)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", token), token)
	// The good tokens still pass.
	for _, tc := range []struct {
		alg string
		key any
	}{{"RS256", testRSAKey()}, {"ES256", testECKey256()}} {
		assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, tc.alg, tc.key, "", nil)), http.StatusOK)
	}
}

// TestJWTBearerRefusesMalformedECDSASignatures checks that only the raw r||s
// form of exactly the curve's width is read: a DER signature, which is what a
// TLS-minded signer produces, and a truncated or padded one are refused.
func TestJWTBearerRefusesMalformedECDSASignatures(t *testing.T) {
	opts := jwtOptions("ES256")
	opts.Keys = []JWTKey{{Key: &testECKey256().PublicKey}}
	app := mustBuild(t, jwtApp(t, opts))
	header := mustJSON(t, map[string]any{"alg": "ES256"})
	input := b64(header) + "." + b64(mustJSON(t, testClaims(nil)))
	raw := signWith(t, "ES256", testECKey256(), input)
	der, err := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])})
	if err != nil {
		t.Fatal(err)
	}
	for name, sig := range map[string][]byte{
		"der":       der,
		"truncated": raw[:63],
		"padded":    append(append([]byte{}, raw...), 0),
		"zero":      make([]byte, 64),
		"swapped":   append(append([]byte{}, raw[32:]...), raw[:32]...),
	} {
		token := input + "." + b64(sig)
		rec := withBearer(t, app, http.MethodGet, "/me", token)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", input+"."+b64(raw)), http.StatusOK)
}

// TestJWTBearerSelectsKeysByKid checks that a kid selects only the keys that
// carry it, and that a token naming no kid is checked only against keys that
// have none, so that a token cannot be steered to a key not meant for it.
func TestJWTBearerSelectsKeysByKid(t *testing.T) {
	opts := jwtOptions("RS256")
	opts.Keys = []JWTKey{
		{ID: "a", Key: &testRSAKey().PublicKey},
		{ID: "b", Key: &testRSAKeyOther().PublicKey},
	}
	app := mustBuild(t, jwtApp(t, opts))
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "a", nil)), http.StatusOK)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKeyOther(), "b", nil)), http.StatusOK)
	for name, token := range map[string]string{
		"signed by a, naming b": signJWT(t, "RS256", testRSAKey(), "b", nil),
		"unknown kid":           signJWT(t, "RS256", testRSAKey(), "c", nil),
		"no kid":                signJWT(t, "RS256", testRSAKey(), "", nil),
	} {
		rec := withBearer(t, app, http.MethodGet, "/me", token)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	// A kid that is not a string, or is empty, or is absurdly long, is refused.
	for _, kid := range []any{42, "", strings.Repeat("k", maxKeyIDBytes+1), nil, []string{"a"}} {
		header := mustJSON(t, map[string]any{"alg": "RS256", "kid": kid})
		token := tokenFrom(t, "RS256", testRSAKey(), header, mustJSON(t, testClaims(nil)))
		assertRefused(t, withBearer(t, app, http.MethodGet, "/me", token), token)
	}
	// Two keys with one ID are a build error.
	opts.Keys = append(opts.Keys, JWTKey{ID: "a", Key: &testRSAKeyOther().PublicKey})
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, `has the ID "a", as Keys[0] does`) {
		t.Fatalf("build error = %s", msg)
	}
}

// TestJWTBearerRefusesWeakOrWrongKeys checks every key that cannot be relied
// on is a build error naming the key.
func TestJWTBearerRefusesWeakOrWrongKeys(t *testing.T) {
	cases := []struct {
		name string
		algs []string
		key  JWTKey
		want string
	}{
		{"short RSA", []string{"RS256"}, JWTKey{Key: &testRSAKey1024().PublicKey}, "the RSA key is 1024 bits, under the minimum of 2048"},
		{"private RSA", []string{"RS256"}, JWTKey{Key: testRSAKey()}, "it is a private key"},
		{"private EC", []string{"ES256"}, JWTKey{Key: testECKey256()}, "it is a private key"},
		{"private Ed", []string{"EdDSA"}, JWTKey{Key: testEdKey()}, "it is a private key"},
		{"secret in Key", []string{"HS256"}, JWTKey{Key: testHMACSecret}, "an HMAC secret goes in Secret"},
		{"short secret", []string{"HS256"}, JWTKey{Secret: []byte("password")}, "the secret is 8 bytes, shorter than the 32 HS256 requires"},
		{"secret short for HS512", []string{"HS256", "HS512"}, JWTKey{Secret: testHMACSecret32}, "shorter than the 64 HS512 requires"},
		{"both", []string{"HS256"}, JWTKey{Key: &testRSAKey().PublicKey, Secret: testHMACSecret}, "set Key or Secret, not both"},
		{"neither", []string{"HS256"}, JWTKey{ID: "x"}, "set Key to a public key"},
		{"RSA without RS", []string{"ES256"}, JWTKey{Key: &testRSAKey().PublicKey}, "it is an RSA key, which none of JWTOptions.Algorithms verifies with"},
		{"P-384 for ES256", []string{"ES256"}, JWTKey{Key: &testECKey384().PublicKey}, "it is an ECDSA key on P-384"},
		{"secret without HS", []string{"RS256"}, JWTKey{Secret: testHMACSecret}, "it is an HMAC secret, which none"},
		{"Ed25519 without EdDSA", []string{"RS256"}, JWTKey{Key: publicOf(testEdKey())}, "it is an Ed25519 key, which none"},
		// An invalid point can only be built through the raw coordinates,
		// which is exactly the key this has to refuse.
		{"invalid point", []string{"ES256"}, JWTKey{Key: &ecdsa.PublicKey{Curve: testECKey256().Curve, X: big.NewInt(1), Y: big.NewInt(1)}}, "not a valid point"}, //nolint:staticcheck // see above
		{"P-224", []string{"ES256"}, JWTKey{Key: &mustP224().PublicKey}, "on a curve no JWS algorithm uses"},
		{"short Ed25519", []string{"EdDSA"}, JWTKey{Key: publicOf(testEdKey()).(ed25519.PublicKey)[:31:31]}, "the Ed25519 key is 31 bytes, not 32"},
		{"unsupported", []string{"RS256"}, JWTKey{Key: "not a key"}, "an HMAC secret goes in Secret"},
		{"unknown type", []string{"RS256"}, JWTKey{Key: 42}, "a key of type int is not supported"},
		{"nil ECDSA", []string{"ES256"}, JWTKey{Key: (*ecdsa.PublicKey)(nil)}, "the ECDSA key is nil"},
		{"nil RSA", []string{"RS256"}, JWTKey{Key: (*rsa.PublicKey)(nil)}, "the RSA key is nil"},
		{"short Ed25519 alone", []string{"EdDSA"}, JWTKey{Key: ed25519.PublicKey{}}, "the Ed25519 key is 0 bytes"},
		{"pinned elsewhere", []string{"RS256"}, JWTKey{Key: &testRSAKey().PublicKey, Algorithm: "RS512"}, `the Algorithm "RS512" is not one of JWTOptions.Algorithms`},
		{"pinned to other family", []string{"RS256", "ES256"}, JWTKey{Key: &testRSAKey().PublicKey, Algorithm: "ES256"}, "which none of JWTOptions.Algorithms verifies with"},
		{"long ID", []string{"RS256"}, JWTKey{ID: strings.Repeat("i", maxKeyIDBytes+1), Key: &testRSAKey().PublicKey}, "the ID is 257 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := jwtOptions(tc.algs...)
			opts.Keys = []JWTKey{tc.key}
			msg := buildError(t, jwtApp(t, opts))
			if !strings.Contains(msg, "JWTOptions.Keys[0]") || !strings.Contains(msg, tc.want) {
				t.Fatalf("build error = %s\nwant it to mention %q", msg, tc.want)
			}
		})
	}
	// Pinning a 32-byte secret to HS256 lets HS512 be allowed for another key.
	opts := jwtOptions("HS256", "HS512")
	opts.Keys = []JWTKey{{Secret: testHMACSecret32, Algorithm: "HS256"}}
	app := mustBuild(t, jwtApp(t, opts))
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "HS256", testHMACSecret32, "", nil)), http.StatusOK)
	token := signJWT(t, "HS512", testHMACSecret32, "", nil)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", token), token)
}

func mustP224() *ecdsa.PrivateKey { return mustECKey(elliptic.P224()) }

func ed25519GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// TestJWTBearerChecksTimesAtTheirBoundaries walks exp, nbf and iat across the
// instant they stop or start being acceptable, with and without leeway.
func TestJWTBearerChecksTimesAtTheirBoundaries(t *testing.T) {
	now := testNow
	leeway := DefaultJWTLeeway
	sec := func(d time.Duration) float64 { return float64(now.Add(d).UnixNano()) / 1e9 }
	cases := []struct {
		name   string
		claims map[string]any
		leeway time.Duration
		ok     bool
	}{
		{"exp an hour ahead", map[string]any{"exp": sec(time.Hour)}, 0, true},
		{"exp within leeway", map[string]any{"exp": sec(-leeway + time.Second)}, 0, true},
		{"exp exactly at leeway", map[string]any{"exp": sec(-leeway)}, 0, false},
		{"exp past leeway", map[string]any{"exp": sec(-leeway - time.Second)}, 0, false},
		{"exp fractional, just valid", map[string]any{"exp": sec(-leeway + 500*time.Millisecond)}, 0, true},
		{"exp fractional, just expired", map[string]any{"exp": sec(-leeway - 500*time.Millisecond)}, 0, false},
		{"exp now without leeway", map[string]any{"exp": sec(0)}, -1, false},
		{"exp a second ahead without leeway", map[string]any{"exp": sec(time.Second)}, -1, true},
		{"exp missing", map[string]any{"exp": nil}, 0, false},
		{"nbf now", map[string]any{"nbf": sec(0)}, 0, true},
		{"nbf within leeway", map[string]any{"nbf": sec(leeway)}, 0, true},
		{"nbf past leeway", map[string]any{"nbf": sec(leeway + time.Second)}, 0, false},
		{"nbf ahead without leeway", map[string]any{"nbf": sec(time.Second)}, -1, false},
		{"iat within leeway", map[string]any{"iat": sec(leeway)}, 0, true},
		{"iat in the future", map[string]any{"iat": sec(leeway + time.Second)}, 0, false},
		{"iat long ago", map[string]any{"iat": sec(-24 * 365 * time.Hour)}, 0, true},
		{"iat missing", map[string]any{"iat": nil}, 0, true},
		{"custom leeway", map[string]any{"exp": sec(-4 * time.Minute)}, 5 * time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := jwtOptions()
			opts.Leeway = tc.leeway
			app := mustBuild(t, jwtApp(t, opts))
			token := signJWT(t, "RS256", testRSAKey(), "", tc.claims)
			rec := withBearer(t, app, http.MethodGet, "/me", token)
			if tc.ok {
				assertStatus(t, rec, http.StatusOK)
			} else {
				assertRefused(t, rec, token)
			}
		})
	}
	opts := jwtOptions()
	opts.AllowNoExpiry = true
	app := mustBuild(t, jwtApp(t, opts))
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", map[string]any{"exp": nil})), http.StatusOK)
	opts.Leeway = MaxJWTLeeway + time.Second
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "JWTOptions.Leeway is 5m1s, over the limit of 5m0s") {
		t.Fatalf("build error = %s", msg)
	}
}

// TestJWTBearerRefusesHostileNumericDates checks that a date is a JSON number
// in range: a string, a negative number, one too large to represent, and one
// written with thousands of digits are all refused.
func TestJWTBearerRefusesHostileNumericDates(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions()))
	future := `1900000000`
	for name, exp := range map[string]string{
		"string":     `"1900000000"`,
		"negative":   `-1`,
		"huge":       `1e400`,
		"year 10000": `253402300800`,
		"long":       `1900000000.` + strings.Repeat("0", 40),
		"digits":     strings.Repeat("9", 400),
		"null":       `null`,
		"bool":       `true`,
		"object":     `{"$date":1900000000}`,
		"array":      `[1900000000]`,
	} {
		payload := `{"iss":"` + testIssuer + `","aud":"` + testAudience + `","exp":` + exp + `}`
		token := tokenFrom(t, "RS256", testRSAKey(), mustJSON(t, map[string]any{"alg": "RS256"}), []byte(payload))
		if rec := withBearer(t, app, http.MethodGet, "/me", token); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	payload := `{"iss":"` + testIssuer + `","aud":"` + testAudience + `","exp":` + future + `.25}`
	token := tokenFrom(t, "RS256", testRSAKey(), mustJSON(t, map[string]any{"alg": "RS256"}), []byte(payload))
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusOK)
}

// TestJWTBearerChecksIssuerAndAudience covers iss and aud in each form a token
// may write them, and the mismatches.
func TestJWTBearerChecksIssuerAndAudience(t *testing.T) {
	opts := jwtOptions()
	opts.Issuers = []string{testIssuer, "https://other.example.com"}
	app := mustBuild(t, jwtApp(t, opts))
	cases := []struct {
		name   string
		claims map[string]any
		ok     bool
	}{
		{"first issuer", nil, true},
		{"second issuer", map[string]any{"iss": "https://other.example.com"}, true},
		{"issuer with a different case", map[string]any{"iss": strings.ToUpper(testIssuer)}, false},
		{"issuer without the trailing slash", map[string]any{"iss": strings.TrimSuffix(testIssuer, "/")}, false},
		{"unknown issuer", map[string]any{"iss": "https://evil.example.com/"}, false},
		{"no issuer", map[string]any{"iss": nil}, false},
		{"issuer as an array", map[string]any{"iss": []string{testIssuer}}, false},
		{"audience in an array", map[string]any{"aud": []string{"https://other.api", testAudience}}, true},
		{"audience alone in an array", map[string]any{"aud": []string{testAudience}}, true},
		{"other audience", map[string]any{"aud": "https://other.api"}, false},
		{"other audiences", map[string]any{"aud": []string{"https://a", "https://b"}}, false},
		{"empty audience array", map[string]any{"aud": []string{}}, false},
		{"audience as a number", map[string]any{"aud": 1}, false},
		{"audience array with a number", map[string]any{"aud": []any{testAudience, 1}}, false},
		{"no audience", map[string]any{"aud": nil}, false},
		{"sub as a number", map[string]any{"sub": 7}, false},
		{"jti as an object", map[string]any{"jti": map[string]any{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := signJWT(t, "RS256", testRSAKey(), "", tc.claims)
			rec := withBearer(t, app, http.MethodGet, "/me", token)
			if tc.ok {
				assertStatus(t, rec, http.StatusOK)
			} else {
				assertRefused(t, rec, token)
			}
		})
	}
	for name, want := range map[string]string{
		"no issuers":  "JWTOptions.Issuers names no issuer",
		"no audience": "JWTOptions.Audience is empty",
		"no keys":     "JWTOptions names no key",
	} {
		opts := jwtOptions()
		switch name {
		case "no issuers":
			opts.Issuers = nil
		case "no audience":
			opts.Audience = ""
		case "no keys":
			opts.Keys = nil
		}
		if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, want) {
			t.Errorf("%s: build error = %s", name, msg)
		}
	}
	opts = jwtOptions()
	opts.Issuers = []string{""}
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "JWTOptions.Issuers[0] is empty") {
		t.Fatalf("build error = %s", msg)
	}
}

// TestJWTBearerChecksTheHeader covers the header members that decide whether a
// token is one this scheme reads at all.
func TestJWTBearerChecksTheHeader(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions()))
	payload := mustJSON(t, testClaims(nil))
	cases := []struct {
		name   string
		header map[string]any
		ok     bool
	}{
		{"typ JWT", map[string]any{"typ": "JWT"}, true},
		{"typ jwt", map[string]any{"typ": "jwt"}, true},
		{"typ at+jwt", map[string]any{"typ": "at+jwt"}, true},
		{"typ application/at+jwt", map[string]any{"typ": "application/at+JWT"}, true},
		{"no typ", map[string]any{}, true},
		{"typ JOSE", map[string]any{"typ": "JOSE"}, false},
		{"typ dpop+jwt", map[string]any{"typ": "dpop+jwt"}, false},
		{"typ number", map[string]any{"typ": 1}, false},
		{"crit", map[string]any{"crit": []string{"exp"}, "exp": 1}, false},
		{"empty crit", map[string]any{"crit": []string{}}, false},
		{"jku", map[string]any{"jku": "https://evil.example.com/jwks"}, false},
		{"x5u", map[string]any{"x5u": "https://evil.example.com/cert"}, false},
		{"jwk", map[string]any{"jwk": rsaJWK(&testRSAKey().PublicKey, nil)}, false},
		{"x5c", map[string]any{"x5c": []string{"MIIB"}}, false},
		{"enc", map[string]any{"enc": "A128GCM"}, false},
		{"zip", map[string]any{"zip": "DEF"}, false},
		{"cty JWT", map[string]any{"cty": "JWT"}, false},
		{"cty application/jwt", map[string]any{"cty": "application/JWT"}, false},
		{"cty json", map[string]any{"cty": "json"}, true},
		{"cty number", map[string]any{"cty": 1}, false},
		{"alg number", map[string]any{"alg": 256}, false},
		{"alg null", map[string]any{"alg": nil}, false},
		{"x5t is ignored", map[string]any{"x5t": "abc"}, true},
		{"deep unknown member", map[string]any{"ext": map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": 1}}}}}, false},
		{"unknown member at the depth allowed", map[string]any{"ext": map[string]any{"a": map[string]any{"b": 1}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := map[string]any{"alg": "RS256"}
			for k, v := range tc.header {
				header[k] = v
			}
			token := tokenFrom(t, "RS256", testRSAKey(), mustJSON(t, header), payload)
			rec := withBearer(t, app, http.MethodGet, "/me", token)
			if tc.ok {
				assertStatus(t, rec, http.StatusOK)
			} else {
				assertRefused(t, rec, token)
			}
		})
	}
	// A scheme may accept a type of its own.
	opts := jwtOptions()
	opts.Types = []string{"application/vnd.example+jwt"}
	app = mustBuild(t, jwtApp(t, opts))
	token := tokenFrom(t, "RS256", testRSAKey(), mustJSON(t, map[string]any{"alg": "RS256", "typ": "vnd.example+JWT"}), payload)
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", token), http.StatusOK)
	token = tokenFrom(t, "RS256", testRSAKey(), mustJSON(t, map[string]any{"alg": "RS256", "typ": "JWT"}), payload)
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", token), token)
	opts.Types = []string{""}
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "JWTOptions.Types[0] is empty") {
		t.Fatalf("build error = %s", msg)
	}
}

// TestJWTBearerRefusesMalformedTokens feeds the scheme every way of writing a
// token that is not one: wrong segment counts, padding, the other base64
// alphabet, whitespace, non-canonical trailing bits, and JSON that is not an
// object or repeats a member.
func TestJWTBearerRefusesMalformedTokens(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions()))
	good := signJWT(t, "RS256", testRSAKey(), "", nil)
	parts := strings.Split(good, ".")
	header, payload, sig := parts[0], parts[1], parts[2]
	sign := func(h, p string) string {
		input := b64([]byte(h)) + "." + b64([]byte(p))
		return input + "." + b64(signWith(t, "RS256", testRSAKey(), input))
	}
	claims := string(mustJSON(t, testClaims(nil)))
	cases := map[string]string{
		"empty":                   "",
		"one segment":             header,
		"two segments":            header + "." + payload,
		"four segments":           good + "." + sig,
		"five segments (JWE)":     header + "." + payload + "." + sig + ".a.b",
		"empty header":            "." + payload + "." + sig,
		"empty payload":           header + ".." + sig,
		"empty signature":         header + "." + payload + ".",
		"padded header":           header + "=." + payload + "." + sig,
		"padded signature":        header + "." + payload + "." + sig + "==",
		"standard alphabet":       header + "." + payload + "." + strings.NewReplacer("-", "+", "_", "/").Replace(sig) + "+/",
		"newline in payload":      header + "." + payload[:10] + "\n" + payload[10:] + "." + sig,
		"space in signature":      header + "." + payload + "." + sig[:10] + " " + sig[10:],
		"tab before signature":    header + "." + payload + ".\t" + sig,
		"non-canonical trailing":  header + "." + payload + "." + sig[:len(sig)-1] + nextBase64(sig[len(sig)-1]),
		"impossible length":       header + "." + payload + "." + sig + "AAA",
		"header not JSON":         b64([]byte("not json")) + "." + payload + "." + sig,
		"header an array":         sign(`["RS256"]`, claims),
		"header with trailing":    sign(`{"alg":"RS256"} {}`, claims),
		"duplicate alg":           sign(`{"alg":"HS256","alg":"RS256"}`, claims),
		"payload not JSON":        sign(`{"alg":"RS256"}`, "claims"),
		"payload an array":        sign(`{"alg":"RS256"}`, `[`+claims+`]`),
		"payload a string":        sign(`{"alg":"RS256"}`, `"`+testIssuer+`"`),
		"duplicate sub":           sign(`{"alg":"RS256"}`, strings.TrimSuffix(claims, "}")+`,"sub":"admin","sub":"user"}`),
		"duplicate exp":           sign(`{"alg":"RS256"}`, strings.TrimSuffix(claims, "}")+`,"exp":1}`),
		"invalid UTF-8":           sign(`{"alg":"RS256"}`, strings.TrimSuffix(claims, "}")+",\"name\":\"\xff\"}"),
		"payload with trailing":   sign(`{"alg":"RS256"}`, claims+`{}`),
		"header too large":        b64([]byte(`{"alg":"RS256","pad":"`+strings.Repeat("x", maxJOSEHeaderBytes)+`"}`)) + "." + payload + "." + sig,
		"bearer scheme only":      "",
		"signature over a prefix": header + "." + payload + "x." + sig,
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			req := newBearerRequest(token)
			if name == "bearer scheme only" {
				req.Header.Set("Authorization", "Bearer")
			}
			rec := doRequest(t, app, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") != `Bearer realm="api", error="invalid_token"` {
				t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", good), http.StatusOK)
}

// nextBase64 returns a base64url character that differs from c only in its
// lowest bits, which a strict decoder must refuse in the last position.
func nextBase64(c byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, c)
	return string(alphabet[i^1])
}

// newBearerRequest builds a GET of /me with the token as its credential.
func newBearerRequest(token string) *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// TestJWTBearerBoundsTokenSize checks the length limit, its default and its
// configuration.
func TestJWTBearerBoundsTokenSize(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions()))
	big := signJWT(t, "RS256", testRSAKey(), "", map[string]any{"pad": strings.Repeat("x", defaultMaxTokenBytes)})
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", big), big)
	opts := jwtOptions()
	opts.MaxTokenBytes = 16 << 10
	app = mustBuild(t, jwtApp(t, opts))
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", big), http.StatusOK)
	for _, size := range []int{-1, maxTokenBytesCeiling + 1} {
		opts.MaxTokenBytes = size
		if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "JWTOptions.MaxTokenBytes") {
			t.Fatalf("build error = %s", msg)
		}
	}
}

// TestJWTBearerBoundsClaimDepth checks that claims may nest as deeply as the
// bound and no deeper.
func TestJWTBearerBoundsClaimDepth(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions()))
	nest := func(depth int) any {
		var v any = 1
		for range depth {
			v = map[string]any{"n": v}
		}
		return v
	}
	// The payload object itself is one level.
	ok := signJWT(t, "RS256", testRSAKey(), "", map[string]any{"deep": nest(maxClaimsDepth - 1)})
	assertStatus(t, withBearer(t, app, http.MethodGet, "/me", ok), http.StatusOK)
	deep := signJWT(t, "RS256", testRSAKey(), "", map[string]any{"deep": nest(maxClaimsDepth)})
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", deep), deep)
	arrays := tokenFrom(t, "RS256", testRSAKey(), mustJSON(t, map[string]any{"alg": "RS256"}),
		[]byte(strings.TrimSuffix(string(mustJSON(t, testClaims(nil))), "}")+`,"a":`+strings.Repeat("[", 5000)+strings.Repeat("]", 5000)+`}`))
	opts := jwtOptions()
	opts.MaxTokenBytes = maxTokenBytesCeiling
	app = mustBuild(t, jwtApp(t, opts))
	assertRefused(t, withBearer(t, app, http.MethodGet, "/me", arrays), arrays)
}

// TestJWTBearerReadsScopes covers scope and scp in each of their forms, and
// the 403 a token without a required scope gets.
func TestJWTBearerReadsScopes(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions(), "items:read"))
	cases := []struct {
		name   string
		claims map[string]any
		status int
	}{
		{"scope string", map[string]any{"scope": "profile items:read"}, http.StatusOK},
		{"scope with doubled spaces", map[string]any{"scope": "  items:read  profile "}, http.StatusOK},
		{"scp array", map[string]any{"scp": []string{"items:read"}}, http.StatusOK},
		{"scp string", map[string]any{"scp": "profile items:read"}, http.StatusOK},
		{"both", map[string]any{"scope": "profile", "scp": []string{"items:read"}}, http.StatusOK},
		{"missing", map[string]any{"scope": "profile"}, http.StatusForbidden},
		{"none", nil, http.StatusForbidden},
		{"prefix only", map[string]any{"scope": "items:rea items:readx"}, http.StatusForbidden},
		{"scope array", map[string]any{"scope": []string{"items:read"}}, http.StatusUnauthorized},
		{"scp with a number", map[string]any{"scp": []any{"items:read", 1}}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := signJWT(t, "RS256", testRSAKey(), "", tc.claims)
			rec := withBearer(t, app, http.MethodGet, "/me", token)
			assertStatus(t, rec, tc.status)
			switch tc.status {
			case http.StatusForbidden:
				want := `Bearer realm="api", error="insufficient_scope", scope="items:read"`
				if got := rec.Header().Get("WWW-Authenticate"); got != want {
					t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
				}
				if e := decodeError(t, rec); e.Error.Code != CodeForbidden {
					t.Fatalf("code = %q", e.Error.Code)
				}
				assertNoTokenLeak(t, rec, token)
			case http.StatusOK:
				if !strings.Contains(rec.Body.String(), "items:read") {
					t.Fatalf("scopes were not handed on: %s", rec.Body.String())
				}
			}
		})
	}
}

// TestJWTBearerValidatesRealm checks the realm is one a header can quote, and
// that a configured one is what the challenge names.
func TestJWTBearerValidatesRealm(t *testing.T) {
	opts := jwtOptions()
	opts.Realm = `bad"realm`
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "JWTOptions.Realm") {
		t.Fatalf("build error = %s", msg)
	}
	opts.Realm = "orders\r\nX-Injected: 1"
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "JWTOptions.Realm") {
		t.Fatalf("build error = %s", msg)
	}
	opts.Realm = "orders"
	app := mustBuild(t, jwtApp(t, opts))
	rec := withBearer(t, app, http.MethodGet, "/me", "")
	assertStatus(t, rec, http.StatusUnauthorized)
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="orders"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}
