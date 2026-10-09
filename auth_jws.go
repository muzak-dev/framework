package muzak

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"hash"
	"io"
	"strings"
)

// This file reads a JWS compact serialization, the form a JWT travels in, and
// checks its signature. Everything here runs on a token a stranger wrote, so
// every step is bounded before it starts: the token's length, the alphabet of
// each segment, the size and depth of the JSON in it, and the number of keys a
// signature is tried against. Nothing in it is worse than linear in the
// token's length.

// keyFamily is the kind of key an algorithm verifies with. A key is only ever
// used by algorithms of its own family, which is what keeps an RSA public key
// from being used as an HMAC secret.
type keyFamily uint8

const (
	familyHMAC keyFamily = iota + 1
	familyRSA
	familyEC
	familyEd25519
)

// jwsAlgorithm is one signature algorithm Muzak verifies, as RFC 7518 and RFC
// 8037 define it.
type jwsAlgorithm struct {
	name    string
	family  keyFamily
	hash    crypto.Hash
	newHash func() hash.Hash
	// pss selects RSASSA-PSS over RSASSA-PKCS1-v1_5 for an RSA algorithm.
	pss bool
	// curve is the one curve an ECDSA algorithm is defined over.
	curve elliptic.Curve
	// sigLen is the exact length of a signature in bytes, or 0 where it
	// depends on the key, which is the case for RSA.
	sigLen int
}

// jwsAlgorithms lists every algorithm a token may be verified with. "none" is
// not among them and cannot be added: an unsigned token is never accepted.
var jwsAlgorithms = map[string]*jwsAlgorithm{
	"HS256": {name: "HS256", family: familyHMAC, hash: crypto.SHA256, newHash: sha256.New, sigLen: sha256.Size},
	"HS384": {name: "HS384", family: familyHMAC, hash: crypto.SHA384, newHash: sha512.New384, sigLen: sha512.Size384},
	"HS512": {name: "HS512", family: familyHMAC, hash: crypto.SHA512, newHash: sha512.New, sigLen: sha512.Size},
	"RS256": {name: "RS256", family: familyRSA, hash: crypto.SHA256, newHash: sha256.New},
	"RS384": {name: "RS384", family: familyRSA, hash: crypto.SHA384, newHash: sha512.New384},
	"RS512": {name: "RS512", family: familyRSA, hash: crypto.SHA512, newHash: sha512.New},
	"PS256": {name: "PS256", family: familyRSA, hash: crypto.SHA256, newHash: sha256.New, pss: true},
	"PS384": {name: "PS384", family: familyRSA, hash: crypto.SHA384, newHash: sha512.New384, pss: true},
	"PS512": {name: "PS512", family: familyRSA, hash: crypto.SHA512, newHash: sha512.New, pss: true},
	"ES256": {name: "ES256", family: familyEC, hash: crypto.SHA256, newHash: sha256.New, curve: elliptic.P256(), sigLen: 64},
	"ES384": {name: "ES384", family: familyEC, hash: crypto.SHA384, newHash: sha512.New384, curve: elliptic.P384(), sigLen: 96},
	"ES512": {name: "ES512", family: familyEC, hash: crypto.SHA512, newHash: sha512.New, curve: elliptic.P521(), sigLen: 132},
	// EdDSA is RFC 8037's name, which says only "Edwards curve"; Ed25519 is
	// RFC 9864's fully specified one. Both verify with an Ed25519 key, the only
	// Edwards curve Muzak accepts.
	"EdDSA":   {name: "EdDSA", family: familyEd25519, sigLen: ed25519.SignatureSize},
	"Ed25519": {name: "Ed25519", family: familyEd25519, sigLen: ed25519.SignatureSize},
}

// Bounds on what is read from a token before its signature is checked.
const (
	// defaultMaxTokenBytes is the longest token accepted unless
	// [JWTOptions.MaxTokenBytes] says otherwise. An access token is a few
	// hundred bytes; one that carries group memberships can run to a few
	// kilobytes.
	defaultMaxTokenBytes = 8 << 10
	// maxTokenBytesCeiling is the most [JWTOptions.MaxTokenBytes] may raise
	// the bound to, which is already more than the default header limit
	// leaves room for.
	maxTokenBytesCeiling = 64 << 10
	// maxJOSEHeaderBytes bounds the decoded header. A header names an
	// algorithm, a key and a type; the members that make one large, an
	// embedded certificate chain or key, are refused anyway.
	maxJOSEHeaderBytes = 2 << 10
	// maxJOSEHeaderDepth and maxClaimsDepth bound how deeply the JSON of the
	// header and of the claims may nest. Registered claims are flat; the
	// depth is there for the custom claims an application reads itself.
	maxJOSEHeaderDepth = 4
	maxClaimsDepth     = 16
	// maxKeyIDBytes bounds a kid, in a token and in a key set.
	maxKeyIDBytes = 256
)

// errInvalidToken is what every failure to read or verify a token comes down
// to. The reason is never told to the client, which learns from RFC 6750's
// error code that the token was not accepted and nothing about which check
// refused it, and so has nothing to work on.
var errInvalidToken = errors.New("muzak: the token is not valid")

// jwsToken is a token whose structure has been read and whose signature has
// not yet been checked.
type jwsToken struct {
	alg    *jwsAlgorithm
	kid    string
	hasKid bool
	// signingInput is the header and payload segments with the dot between
	// them, exactly as the token carried them, which is what was signed.
	signingInput string
	payload      []byte
	signature    []byte
}

// joseHeader is the header of a token, each member kept raw so that its type
// is checked here rather than coerced by the decoder.
type joseHeader struct {
	Alg  jsontext.Value `json:"alg"`
	Kid  jsontext.Value `json:"kid"`
	Typ  jsontext.Value `json:"typ"`
	Cty  jsontext.Value `json:"cty"`
	Crit jsontext.Value `json:"crit"`
	// A token that names where its own key is, or carries the key, is asking
	// to be verified with a key its sender chose. Muzak never fetches or
	// trusts one, and refuses a token that tries, so that an issuer
	// misconfigured to depend on them is noticed rather than half-working.
	Jku jsontext.Value `json:"jku"`
	Jwk jsontext.Value `json:"jwk"`
	X5u jsontext.Value `json:"x5u"`
	X5c jsontext.Value `json:"x5c"`
	// enc and zip belong to an encrypted token, which is not a JWS at all.
	Enc jsontext.Value `json:"enc"`
	Zip jsontext.Value `json:"zip"`
}

// jwsPolicy is what decides whether a token's structure is acceptable before
// any key is consulted.
type jwsPolicy struct {
	maxBytes   int
	algorithms map[string]*jwsAlgorithm
	// types are the accepted typ values, normalised; see [normalizeJOSEType].
	types []string
}

// parseJWS reads a compact JWS and checks everything about it that does not
// need a key. It is linear in the token's length, and refuses a token longer
// than the policy allows before reading any of it.
func parseJWS(token string, p *jwsPolicy) (*jwsToken, error) {
	if len(token) == 0 || len(token) > p.maxBytes {
		return nil, errInvalidToken
	}
	first := strings.IndexByte(token, '.')
	last := strings.LastIndexByte(token, '.')
	if first <= 0 || last == first || last == len(token)-1 {
		// No dot, one dot, an empty header or an empty signature, which is
		// how an unsigned token is written.
		return nil, errInvalidToken
	}
	headerPart, payloadPart, sigPart := token[:first], token[first+1:last], token[last+1:]
	if payloadPart == "" || !isBase64URL(headerPart) || !isBase64URL(payloadPart) || !isBase64URL(sigPart) {
		// A third dot fails here too, since a dot is not in the alphabet.
		return nil, errInvalidToken
	}
	if base64.RawURLEncoding.DecodedLen(len(headerPart)) > maxJOSEHeaderBytes {
		return nil, errInvalidToken
	}
	headerJSON, err := decodeSegment(headerPart)
	if err != nil {
		return nil, errInvalidToken
	}
	t := &jwsToken{signingInput: token[:last]}
	if err := t.readHeader(headerJSON, p); err != nil {
		return nil, err
	}
	if t.payload, err = decodeSegment(payloadPart); err != nil {
		return nil, errInvalidToken
	}
	if t.signature, err = decodeSegment(sigPart); err != nil {
		return nil, errInvalidToken
	}
	if t.alg.sigLen != 0 && len(t.signature) != t.alg.sigLen {
		// An ECDSA signature is r and s side by side at the curve's exact
		// width. A DER-encoded one, which is what a library that signs for
		// TLS produces, is a different length and is refused here rather
		// than reinterpreted.
		return nil, errInvalidToken
	}
	return t, nil
}

// readHeader checks the decoded header and records the algorithm and key it
// names.
func (t *jwsToken) readHeader(data []byte, p *jwsPolicy) error {
	if err := checkJSONObject(data, maxJOSEHeaderDepth); err != nil {
		return errInvalidToken
	}
	var h joseHeader
	if err := json.Unmarshal(data, &h); err != nil {
		// coverage: checkJSONObject has just read data as one valid object,
		// and every member is decoded into a raw jsontext.Value, which holds
		// any value, so decoding it cannot fail.
		return errInvalidToken
	}
	if len(h.Crit) > 0 || len(h.Jku) > 0 || len(h.Jwk) > 0 || len(h.X5u) > 0 || len(h.X5c) > 0 ||
		len(h.Enc) > 0 || len(h.Zip) > 0 {
		// crit names extensions the recipient must understand or refuse the
		// token, and Muzak understands none.
		return errInvalidToken
	}
	alg, ok := jsonString(h.Alg)
	if !ok {
		return errInvalidToken
	}
	// The policy's map holds only allowed algorithms, so "none", an algorithm
	// Muzak does not implement and one this scheme was not configured for all
	// miss here alike.
	if t.alg = p.algorithms[alg]; t.alg == nil {
		return errInvalidToken
	}
	if len(h.Kid) > 0 {
		kid, ok := jsonString(h.Kid)
		if !ok || kid == "" || len(kid) > maxKeyIDBytes {
			return errInvalidToken
		}
		t.kid, t.hasKid = kid, true
	}
	if len(h.Typ) > 0 {
		typ, ok := jsonString(h.Typ)
		if !ok || !acceptsJOSEType(p.types, typ) {
			return errInvalidToken
		}
	}
	if len(h.Cty) > 0 {
		// A content type of JWT announces a nested token, signed or
		// encrypted inside this one, which is not what an access token is.
		cty, ok := jsonString(h.Cty)
		if !ok || normalizeJOSEType(cty) == "jwt" {
			return errInvalidToken
		}
	}
	return nil
}

// normalizeJOSEType returns a typ or cty value as it is compared: RFC 7515
// lets "application/" be left off a media type there, and media types are
// compared without regard to case.
func normalizeJOSEType(value string) string {
	value = strings.ToLower(value)
	return strings.TrimPrefix(value, "application/")
}

// acceptsJOSEType reports whether a token's typ is one the policy accepts.
func acceptsJOSEType(types []string, typ string) bool {
	typ = normalizeJOSEType(typ)
	for _, accepted := range types {
		if typ == accepted {
			return true
		}
	}
	return false
}

// isBase64URL reports whether s is non-empty and written only in the base64url
// alphabet. The standard decoder skips carriage returns and line feeds, which
// would let two different strings decode to the same bytes, so the alphabet is
// checked first, and padding is refused because a JWS never has any.
func isBase64URL(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// strictBase64URL decodes base64url without padding, and refuses an encoding
// whose unused trailing bits are not zero, so that each byte string has
// exactly one spelling.
var strictBase64URL = base64.RawURLEncoding.Strict()

// decodeSegment decodes one segment of a token or one member of a key, whose
// alphabet has already been checked.
func decodeSegment(s string) ([]byte, error) {
	return strictBase64URL.DecodeString(s)
}

// jsonString returns the value of a JSON string, and false for anything else,
// null included.
func jsonString(v jsontext.Value) (string, bool) {
	if v.Kind() != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		// coverage: the value was read by the decoder as a complete string,
		// with its UTF-8 checked, so decoding it again cannot fail.
		return "", false
	}
	return s, true
}

// errJSONShape reports JSON that is not a single object within the depth
// allowed. Its text never reaches a client.
var errJSONShape = errors.New("muzak: the JSON is not a single object within the depth allowed")

// checkJSONObject reports an error unless data is exactly one JSON object,
// nested no deeper than maxDepth, with nothing after it. The decoder refuses
// duplicate member names and invalid UTF-8 as it reads, which keeps two
// readers of the same claims from disagreeing about which of two "sub" members
// counts. It is one linear pass over the input.
func checkJSONObject(data []byte, maxDepth int) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != '{' {
		return errJSONShape
	}
	for dec.StackDepth() > 0 {
		if _, err := dec.ReadToken(); err != nil {
			return err
		}
		if dec.StackDepth() > maxDepth {
			return errJSONShape
		}
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return errJSONShape
	}
	return nil
}

// verifyKey is one key a token's signature may be checked with, from the
// application's configuration or from a key set.
type verifyKey struct {
	id     string
	hasID  bool
	family keyFamily
	// alg, when set, is the one algorithm the key may be used with, from the
	// key's own declaration.
	alg *jwsAlgorithm

	secret []byte
	rsa    *rsa.PublicKey
	ec     *ecdsa.PublicKey
	ed     ed25519.PublicKey
}

// accepts reports whether the key may verify a signature made with alg: the
// family has to match, an ECDSA key has to be on the algorithm's own curve,
// and a key pinned to one algorithm accepts only that one.
func (k *verifyKey) accepts(alg *jwsAlgorithm) bool {
	if k.family != alg.family || (k.alg != nil && k.alg != alg) {
		return false
	}
	if alg.family == familyEC && k.ec.Curve != alg.curve {
		return false
	}
	return true
}

// verify checks a signature with this key. It is called only for a key that
// accepts the algorithm.
func (k *verifyKey) verify(alg *jwsAlgorithm, signingInput string, sig []byte) bool {
	switch alg.family {
	case familyHMAC:
		mac := hmac.New(alg.newHash, k.secret)
		_, _ = io.WriteString(mac, signingInput)
		// hmac.Equal is constant-time, so the response time says nothing
		// about how much of a forged MAC was right.
		return hmac.Equal(mac.Sum(nil), sig)
	case familyRSA:
		digest := digestOf(alg, signingInput)
		if alg.pss {
			// RFC 7518 fixes the salt at the length of the hash.
			return rsa.VerifyPSS(k.rsa, alg.hash, digest, sig,
				&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: alg.hash}) == nil
		}
		return rsa.VerifyPKCS1v15(k.rsa, alg.hash, digest, sig) == nil
	case familyEC:
		return ecdsa.VerifyASN1(k.ec, digestOf(alg, signingInput), rawSignatureToASN1(sig))
	default:
		return ed25519.Verify(k.ed, []byte(signingInput), sig)
	}
}

// digestOf hashes the signing input with the algorithm's hash.
func digestOf(alg *jwsAlgorithm, signingInput string) []byte {
	h := alg.newHash()
	_, _ = io.WriteString(h, signingInput)
	return h.Sum(nil)
}

// rawSignatureToASN1 rewrites an ECDSA signature from JWS's fixed-width r||s
// into the DER that the standard library verifies. The input's length has
// already been checked against the curve, so it splits evenly.
func rawSignatureToASN1(sig []byte) []byte {
	half := len(sig) / 2
	r, s := asn1Integer(sig[:half]), asn1Integer(sig[half:])
	body := len(r) + len(s)
	out := make([]byte, 0, body+3)
	out = append(out, 0x30)
	if body >= 0x80 {
		// P-521's two integers can reach 138 bytes, past the short form.
		out = append(out, 0x81)
	}
	out = append(out, byte(body)) //nolint:gosec // two integers of at most 67 bytes each, so under 256
	out = append(out, r...)
	return append(out, s...)
}

// asn1Integer encodes a big-endian unsigned integer as a DER INTEGER: leading
// zeros dropped, and one put back where the top bit would read as a sign.
func asn1Integer(b []byte) []byte {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	pad := 0
	if b[0]&0x80 != 0 {
		pad = 1
	}
	out := make([]byte, 0, 2+pad+len(b))
	out = append(out, 0x02, byte(pad+len(b))) //nolint:gosec // at most 67, half of a P-521 signature and a sign byte
	if pad == 1 {
		out = append(out, 0)
	}
	return append(out, b...)
}
