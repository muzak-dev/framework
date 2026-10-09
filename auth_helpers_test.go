package muzak

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json/v2"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The keys every authentication test signs with, generated once per test
// binary because an RSA key takes a noticeable moment to make.
var (
	testRSAKey       = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey(2048) })
	testRSAKeyOther  = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey(2048) })
	testRSAKey1024   = sync.OnceValue(func() *rsa.PrivateKey { return mustRSAKey(1024) })
	testECKey256     = sync.OnceValue(func() *ecdsa.PrivateKey { return mustECKey(elliptic.P256()) })
	testECKey384     = sync.OnceValue(func() *ecdsa.PrivateKey { return mustECKey(elliptic.P384()) })
	testECKey521     = sync.OnceValue(func() *ecdsa.PrivateKey { return mustECKey(elliptic.P521()) })
	testEdKey        = sync.OnceValue(func() ed25519.PrivateKey { _, k, _ := ed25519.GenerateKey(rand.Reader); return k })
	testHMACSecret   = []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	testHMACSecret32 = []byte("0123456789abcdef0123456789abcdef")
)

func mustRSAKey(bits int) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		panic(err)
	}
	return k
}

func mustECKey(c elliptic.Curve) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

// The issuer and audience the test schemes accept.
const (
	testIssuer   = "https://issuer.example.com/"
	testAudience = "https://api.example.com"
)

// testNow is the fixed instant the test schemes' clock reads.
var testNow = time.Unix(1_800_000_000, 0)

// b64 encodes a token segment.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// mustJSON encodes a header or claims for a test token.
func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// signWith signs the signing input with the algorithm and key, producing the
// signature as a JWS carries it.
func signWith(t testing.TB, alg string, key any, input string) []byte {
	t.Helper()
	a := jwsAlgorithms[alg]
	switch alg {
	case "HS256", "HS384", "HS512":
		mac := hmac.New(a.newHash, key.([]byte))
		mac.Write([]byte(input))
		return mac.Sum(nil)
	case "RS256", "RS384", "RS512":
		sig, err := rsa.SignPKCS1v15(rand.Reader, key.(*rsa.PrivateKey), a.hash, digestOf(a, input))
		if err != nil {
			t.Fatal(err)
		}
		return sig
	case "PS256", "PS384", "PS512":
		sig, err := rsa.SignPSS(rand.Reader, key.(*rsa.PrivateKey), a.hash, digestOf(a, input),
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		if err != nil {
			t.Fatal(err)
		}
		return sig
	case "ES256", "ES384", "ES512":
		r, s, err := ecdsa.Sign(rand.Reader, key.(*ecdsa.PrivateKey), digestOf(a, input))
		if err != nil {
			t.Fatal(err)
		}
		size := a.sigLen / 2
		out := make([]byte, 2*size)
		r.FillBytes(out[:size])
		s.FillBytes(out[size:])
		return out
	case "EdDSA", "Ed25519":
		return ed25519.Sign(key.(ed25519.PrivateKey), []byte(input))
	}
	t.Fatalf("no signer for %s", alg)
	return nil
}

// tokenFrom assembles a token from raw header and payload JSON, signed with
// the algorithm and key.
func tokenFrom(t testing.TB, alg string, key any, header, payload []byte) string {
	t.Helper()
	input := b64(header) + "." + b64(payload)
	return input + "." + b64(signWith(t, alg, key, input))
}

// signJWT signs claims with the algorithm and key, naming kid when it is not
// empty. Claims start from a valid set for the test schemes, and a nil value
// in extra removes a claim.
func signJWT(t testing.TB, alg string, key any, kid string, extra map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	return tokenFrom(t, alg, key, mustJSON(t, header), mustJSON(t, testClaims(extra)))
}

// testClaims returns a valid payload for the test schemes, with extra applied.
func testClaims(extra map[string]any) map[string]any {
	claims := map[string]any{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "user-1",
		"exp": testNow.Add(time.Hour).Unix(),
		"iat": testNow.Add(-time.Minute).Unix(),
	}
	for k, v := range extra {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	return claims
}

// jwtOptions is a valid JWTBearer configuration with an RSA key and an HMAC
// secret, to which a test applies its own changes.
func jwtOptions(algs ...string) JWTOptions {
	if len(algs) == 0 {
		algs = []string{"RS256"}
	}
	return JWTOptions{
		Issuers:    []string{testIssuer},
		Audience:   testAudience,
		Algorithms: algs,
		Keys:       []JWTKey{{Key: &testRSAKey().PublicKey}},
	}
}

// testScheme builds a JWTBearer scheme whose clock reads testNow.
func testScheme(opts JWTOptions) SecurityScheme {
	s := JWTBearer(opts)
	if s.verifier.jwt != nil {
		s.verifier.jwt.now = func() time.Time { return testNow }
	}
	return s
}

// meOut is what the test route answers with: who the token was for.
type meOut struct {
	Subject string   `json:"subject"`
	Scheme  string   `json:"scheme"`
	Scopes  []string `json:"scopes"`
}

// me reads the verified claims, as a handler behind a JWTBearer scheme does.
func me(ctx *Context, _ struct{}) (meOut, error) {
	claims, ok := TryFrom[*Claims](ctx)
	if !ok {
		return meOut{Subject: "anonymous"}, nil
	}
	return meOut{Subject: claims.Subject, Scheme: claims.Scheme, Scopes: claims.Scopes}, nil
}

// authApp builds an application declaring the schemes, with GET /me behind
// the requirements.
func authApp(t testing.TB, schemes map[string]SecurityScheme, requirements ...SecurityRequirement) *App {
	t.Helper()
	opts := quietOptions()
	opts.SecuritySchemes = schemes
	app := New(opts)
	app.Get("/me", me, WithSecurity(requirements...))
	return app
}

// jwtApp builds an application with one JWT scheme named "jwt" and GET /me
// behind it.
func jwtApp(t testing.TB, opts JWTOptions, scopes ...string) *App {
	t.Helper()
	return authApp(t, map[string]SecurityScheme{"jwt": testScheme(opts)}, Require("jwt", scopes...))
}

// withBearer sends a GET with the token as a bearer credential.
func withBearer(t testing.TB, app *App, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

// assertRefused fails unless the response is the 401 a JWT scheme answers a
// refused token with, carrying nothing of the token.
func assertRefused(t testing.TB, rec *httptest.ResponseRecorder, token string) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api", error="invalid_token"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	assertNoTokenLeak(t, rec, token)
}

// assertNoTokenLeak fails if any part of the token reached the response.
func assertNoTokenLeak(t testing.TB, rec *httptest.ResponseRecorder, token string) {
	t.Helper()
	if token == "" {
		return
	}
	var dump strings.Builder
	dump.WriteString(rec.Body.String())
	for name, values := range rec.Header() {
		dump.WriteString(name + ": " + strings.Join(values, ",") + "\n")
	}
	for _, part := range strings.Split(token, ".") {
		if len(part) >= 8 && strings.Contains(dump.String(), part) {
			t.Fatalf("the response carries part of the token %q:\n%s", part, dump.String())
		}
	}
}

// rsaJWK writes an RSA public key as a JWK with the given extra members.
func rsaJWK(pub *rsa.PublicKey, extra map[string]any) map[string]any {
	jwk := map[string]any{
		"kty": "RSA",
		"n":   b64(pub.N.Bytes()),
		"e":   b64(big.NewInt(int64(pub.E)).Bytes()),
	}
	for k, v := range extra {
		if v == nil {
			delete(jwk, k)
			continue
		}
		jwk[k] = v
	}
	return jwk
}

// ecJWK writes an ECDSA public key as a JWK.
func ecJWK(pub *ecdsa.PublicKey, extra map[string]any) map[string]any {
	raw, err := pub.Bytes()
	if err != nil {
		panic(err)
	}
	size := (len(raw) - 1) / 2
	jwk := map[string]any{
		"kty": "EC",
		"crv": pub.Curve.Params().Name,
		"x":   b64(raw[1 : 1+size]),
		"y":   b64(raw[1+size:]),
	}
	for k, v := range extra {
		jwk[k] = v
	}
	return jwk
}

// edJWK writes an Ed25519 public key as a JWK.
func edJWK(pub ed25519.PublicKey, extra map[string]any) map[string]any {
	jwk := map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64(pub)}
	for k, v := range extra {
		jwk[k] = v
	}
	return jwk
}

// publicOf returns the public half of a test key.
func publicOf(key any) crypto.PublicKey {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	case ed25519.PrivateKey:
		return k.Public()
	}
	return nil
}
