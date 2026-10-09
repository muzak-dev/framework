package muzak

import (
	"crypto/ed25519"
	"encoding/asn1"
	"encoding/json/jsontext"
	"math/big"
	"strings"
	"testing"
)

// allowedForJWKTests are the algorithms the key parsing tests allow.
var allowedForJWKTests = map[string]*jwsAlgorithm{
	"RS256": jwsAlgorithms["RS256"],
	"PS256": jwsAlgorithms["PS256"],
	"ES256": jwsAlgorithms["ES256"],
	"ES384": jwsAlgorithms["ES384"],
	"ES512": jwsAlgorithms["ES512"],
	"EdDSA": jwsAlgorithms["EdDSA"],
}

// TestParseJWKAcceptsWellFormedKeys checks every kind of key Muzak verifies
// with is read, with its kid and its algorithm.
func TestParseJWKAcceptsWellFormedKeys(t *testing.T) {
	cases := map[string]map[string]any{
		"RSA":              rsaJWK(&testRSAKey().PublicKey, nil),
		"RSA with members": rsaJWK(&testRSAKey().PublicKey, map[string]any{"kid": "r", "use": "sig", "alg": "RS256", "key_ops": []string{"verify"}, "x5t": "ignored"}),
		"P-256":            ecJWK(&testECKey256().PublicKey, map[string]any{"alg": "ES256"}),
		"P-384":            ecJWK(&testECKey384().PublicKey, nil),
		"P-521":            ecJWK(&testECKey521().PublicKey, nil),
		"Ed25519":          edJWK(publicOf(testEdKey()).(ed25519.PublicKey), map[string]any{"kid": "e"}),
	}
	for name, jwk := range cases {
		t.Run(name, func(t *testing.T) {
			k, err := parseJWK(jsontext.Value(mustJSON(t, jwk)), allowedForJWKTests)
			if err != nil {
				t.Fatalf("parseJWK() = %v", err)
			}
			if kid, ok := jwk["kid"].(string); ok && (!k.hasID || k.id != kid) {
				t.Fatalf("kid = %q, %t", k.id, k.hasID)
			}
			if alg, ok := jwk["alg"].(string); ok && k.alg != jwsAlgorithms[alg] {
				t.Fatalf("alg = %v", k.alg)
			}
		})
	}
}

// TestParseJWKRefusesKeysItCannotTrust covers every key a strict reader skips.
func TestParseJWKRefusesKeysItCannotTrust(t *testing.T) {
	rsa := func(extra map[string]any) map[string]any { return rsaJWK(&testRSAKey().PublicKey, extra) }
	ec := func(extra map[string]any) map[string]any { return ecJWK(&testECKey256().PublicKey, extra) }
	ed := func(extra map[string]any) map[string]any {
		return edJWK(publicOf(testEdKey()).(ed25519.PublicKey), extra)
	}
	pub := testRSAKey().PublicKey
	nBytes := pub.N.Bytes()
	offCurve := ec(map[string]any{"y": b64(make([]byte, 32))})
	big8200 := new(big.Int).Lsh(big.NewInt(1), 8200)
	big8200.Add(big8200, big.NewInt(1))
	cases := map[string]map[string]any{
		"symmetric":           {"kty": "oct", "k": b64(testHMACSecret)},
		"unknown kty":         {"kty": "XYZ"},
		"no kty":              {"n": b64(nBytes), "e": "AQAB"},
		"kty lowercase":       rsa(map[string]any{"kty": "rsa"}),
		"use enc":             rsa(map[string]any{"use": "enc"}),
		"use empty":           rsa(map[string]any{"use": ""}),
		"key_ops sign":        rsa(map[string]any{"key_ops": []string{"sign"}}),
		"key_ops empty":       rsa(map[string]any{"key_ops": []string{}}),
		"key_ops number":      rsa(map[string]any{"key_ops": 1}),
		"alg not allowed":     rsa(map[string]any{"alg": "RS512"}),
		"alg encryption":      rsa(map[string]any{"alg": "RSA-OAEP"}),
		"alg of other family": rsa(map[string]any{"alg": "ES256"}),
		"alg of other curve":  ec(map[string]any{"alg": "ES384"}),
		"kid empty":           rsa(map[string]any{"kid": ""}),
		"kid number":          rsa(map[string]any{"kid": 1}),
		"kid long":            rsa(map[string]any{"kid": strings.Repeat("k", maxKeyIDBytes+1)}),
		"private d":           rsa(map[string]any{"d": "AQAB"}),
		"private p":           rsa(map[string]any{"p": "AQAB"}),
		"private oth":         rsa(map[string]any{"oth": []any{}}),
		"private ec d":        ec(map[string]any{"d": "AQAB"}),
		"private ed d":        ed(map[string]any{"d": "AQAB"}),
		"RSA 1024":            rsaJWK(&testRSAKey1024().PublicKey, nil),
		"RSA too large":       rsa(map[string]any{"n": b64(big8200.Bytes())}),
		"RSA even modulus":    rsa(map[string]any{"n": b64(new(big.Int).Lsh(pub.N, 1).Bytes())}),
		"RSA leading zero":    rsa(map[string]any{"n": b64(append([]byte{0}, nBytes...))}),
		"RSA e even":          rsa(map[string]any{"e": b64([]byte{1, 0, 0})}),
		"RSA e one":           rsa(map[string]any{"e": "AQ"}),
		"RSA e leading zero":  rsa(map[string]any{"e": b64([]byte{0, 1, 0, 1})}),
		"RSA e five bytes":    rsa(map[string]any{"e": b64([]byte{1, 0, 0, 0, 1})}),
		"RSA e too large":     rsa(map[string]any{"e": b64([]byte{0xff, 0xff, 0xff, 0xff})}),
		"RSA n padded":        rsa(map[string]any{"n": b64(nBytes) + "=="}),
		"RSA n standard b64":  rsa(map[string]any{"n": strings.ReplaceAll(strings.ReplaceAll(b64(nBytes), "-", "+"), "_", "/") + "+/"}),
		"RSA n empty":         rsa(map[string]any{"n": ""}),
		"RSA n number":        rsa(map[string]any{"n": 7}),
		"RSA e missing":       rsa(map[string]any{"e": nil}),
		"EC off curve":        offCurve,
		"EC short x":          ec(map[string]any{"x": b64(make([]byte, 31))}),
		"EC long y":           ec(map[string]any{"y": b64(make([]byte, 33))}),
		"EC crv mismatch":     ec(map[string]any{"crv": "P-384"}),
		"EC secp256k1":        ec(map[string]any{"crv": "secp256k1"}),
		"EC no crv":           ec(map[string]any{"crv": nil}),
		"EC identity":         ec(map[string]any{"x": b64(make([]byte, 32)), "y": b64(make([]byte, 32))}),
		"OKP Ed448":           ed(map[string]any{"crv": "Ed448", "x": b64(make([]byte, 57))}),
		"OKP X25519":          ed(map[string]any{"crv": "X25519"}),
		"OKP short":           ed(map[string]any{"x": b64(make([]byte, 31))}),
		"OKP alg ES256":       ed(map[string]any{"alg": "ES256"}),
		"use number":          rsa(map[string]any{"use": 1}),
	}
	withMember := func(jwk map[string]any, member string) string {
		return strings.TrimSuffix(string(mustJSON(t, jwk)), "}") + "," + member + "}"
	}
	raws := map[string]string{
		"not an object":   `["not","a","key"]`,
		"null private d":  withMember(rsa(nil), `"d":null`),
		"duplicate kty":   withMember(rsa(nil), `"kty":"EC"`),
		"invalid UTF-8":   withMember(rsa(nil), "\"kid\":\"\xff\""),
		"private k on EC": withMember(ec(nil), `"k":"AQAB"`),
	}
	for name, jwk := range cases {
		raws[name] = string(mustJSON(t, jwk))
	}
	for name, raw := range raws {
		t.Run(name, func(t *testing.T) {
			if k, err := parseJWK(jsontext.Value(raw), allowedForJWKTests); err == nil {
				t.Fatalf("parseJWK() accepted %s as %+v", raw, k)
			}
		})
	}
	// A key no allowed algorithm can use is of no use.
	onlyRS := map[string]*jwsAlgorithm{"RS256": jwsAlgorithms["RS256"]}
	for _, jwk := range []map[string]any{ec(nil), ed(nil)} {
		if _, err := parseJWK(jsontext.Value(mustJSON(t, jwk)), onlyRS); err == nil {
			t.Fatalf("parseJWK(%v) accepted a key no allowed algorithm uses", jwk["kty"])
		}
	}
	// A null member that is not private is an absent one.
	raw := withMember(rsa(nil), `"use":null,"alg":null,"kid":null,"key_ops":null`)
	if _, err := parseJWK(jsontext.Value(raw), allowedForJWKTests); err != nil {
		t.Fatalf("null members: %v", err)
	}
}

// TestParseJWKSDocument covers the shape of the document as a whole.
func TestParseJWKSDocument(t *testing.T) {
	one := string(mustJSON(t, k1()))
	cases := map[string]struct {
		doc  string
		keys int
		ok   bool
	}{
		"one key":            {`{"keys":[` + one + `]}`, 1, true},
		"other members":      {`{"keys":[` + one + `],"x":{"y":1}}`, 1, true},
		"empty":              {`{"keys":[]}`, 0, true},
		"null keys":          {`{"keys":null}`, 0, false},
		"no keys":            {`{}`, 0, false},
		"keys an object":     {`{"keys":{}}`, 0, false},
		"array":              {`[` + one + `]`, 0, false},
		"trailing":           {`{"keys":[]}{}`, 0, false},
		"duplicate keys":     {`{"keys":[],"keys":[` + one + `]}`, 0, false},
		"duplicate in a key": {`{"keys":[{"kty":"RSA","kty":"EC"}]}`, 0, false},
		"too many":           {`{"keys":[` + strings.Repeat(one+",", 3) + one + `]}`, 0, false},
		"too deep":           {`{"keys":[],"x":` + strings.Repeat("[", maxJWKSDepth) + strings.Repeat("]", maxJWKSDepth) + `}`, 0, false},
		"invalid UTF-8":      {"{\"keys\":[],\"x\":\"\xff\"}", 0, false},
		"skips the bad":      {`{"keys":[{"kty":"oct"},` + one + `,null]}`, 1, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			set, _, err := parseJWKS([]byte(tc.doc), 3, allowedForJWKTests)
			if (err == nil) != tc.ok {
				t.Fatalf("parseJWKS() error = %v, want ok=%t", err, tc.ok)
			}
			if err == nil && set.count != tc.keys {
				t.Fatalf("keys = %d, want %d", set.count, tc.keys)
			}
		})
	}
}

// TestKeySetLookupIsByKid checks a token's kid, or its absence, selects keys.
func TestKeySetLookupIsByKid(t *testing.T) {
	var set *keySet
	if set.candidates("a", true) != nil {
		t.Fatal("a nil set has candidates")
	}
	set = &keySet{}
	a := &verifyKey{id: "a", hasID: true}
	none := &verifyKey{}
	set.add(a)
	set.add(none)
	if got := set.candidates("a", true); len(got) != 1 || got[0] != a {
		t.Fatalf("candidates(a) = %v", got)
	}
	if got := set.candidates("", false); len(got) != 1 || got[0] != none {
		t.Fatalf("candidates(none) = %v", got)
	}
	if got := set.candidates("b", true); len(got) != 0 {
		t.Fatalf("candidates(b) = %v", got)
	}
}

// TestRawSignatureToASN1 checks the conversion against the standard library's
// own encoding, at the widths where the length forms change.
func TestRawSignatureToASN1(t *testing.T) {
	for _, size := range []int{32, 48, 66} {
		for _, fill := range []byte{0x00, 0x01, 0x7f, 0x80, 0xff} {
			raw := make([]byte, 2*size)
			for i := range raw {
				raw[i] = fill
			}
			raw[0], raw[size] = 0, 0x80
			der := rawSignatureToASN1(raw)
			var parsed struct{ R, S *big.Int }
			if rest, err := asn1.Unmarshal(der, &parsed); err != nil || len(rest) != 0 {
				t.Fatalf("size %d fill %x: %v", size, fill, err)
			}
			if parsed.R.Cmp(new(big.Int).SetBytes(raw[:size])) != 0 || parsed.S.Cmp(new(big.Int).SetBytes(raw[size:])) != 0 {
				t.Fatalf("size %d fill %x: round trip differs", size, fill)
			}
		}
	}
}
