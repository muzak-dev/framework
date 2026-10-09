package muzak

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// This file turns keys into something a signature can be checked with: the
// keys an application configures, which are checked when it is built, and the
// keys a JWKS document publishes, which come over the network and are checked
// as strictly as a token is.

// Bounds on the keys Muzak accepts.
const (
	// minRSABits is the smallest RSA modulus accepted, which is what NIST has
	// required since 2015.
	minRSABits = 2048
	// maxJWKRSABits is the largest RSA modulus accepted from a key set. A
	// verification costs more the larger the key, so an absurd one is refused
	// rather than paid for on every token.
	maxJWKRSABits = 8192
	// maxJWKSDepth bounds the nesting of a key set document: the set, its
	// list of keys, a key, and a list inside a key, with room to spare.
	maxJWKSDepth = 8
)

// keySet is a set of keys indexed the way a token looks them up. Lookups are a
// map access and a scan of the keys sharing one kid, which is bounded by the
// number of keys in the set.
type keySet struct {
	byID map[string][]*verifyKey
	// unnamed are the keys with no kid, which only a token naming no key is
	// checked against.
	unnamed []*verifyKey
	count   int
}

// add puts a key in the set.
func (s *keySet) add(k *verifyKey) {
	s.count++
	if !k.hasID {
		s.unnamed = append(s.unnamed, k)
		return
	}
	if s.byID == nil {
		s.byID = make(map[string][]*verifyKey)
	}
	s.byID[k.id] = append(s.byID[k.id], k)
}

// candidates returns the keys a token naming kid, or naming none, is checked
// against. A token that names a key is checked only against keys with that
// kid, and one that names none only against keys that have none, so that a
// kid can never steer a token to a key it does not belong to.
func (s *keySet) candidates(kid string, hasKid bool) []*verifyKey {
	if s == nil {
		return nil
	}
	if !hasKid {
		return s.unnamed
	}
	return s.byID[kid]
}

// staticKey checks one key from [JWTOptions.Keys] against the algorithms the
// scheme allows, given in a fixed order so that the same mistake is always
// reported the same way, and returns it ready for use.
func staticKey(key JWTKey, allowed map[string]*jwsAlgorithm, order []*jwsAlgorithm) (*verifyKey, error) {
	if len(key.ID) > maxKeyIDBytes {
		return nil, fmt.Errorf("the ID is %d bytes, over the limit of %d", len(key.ID), maxKeyIDBytes)
	}
	k := &verifyKey{id: key.ID, hasID: key.ID != ""}
	switch {
	case key.Key != nil && key.Secret != nil:
		return nil, errors.New("set Key or Secret, not both")
	case key.Secret != nil:
		k.family = familyHMAC
		k.secret = slices.Clone(key.Secret)
	case key.Key == nil:
		return nil, errors.New("set Key to a public key, or Secret to an HMAC secret")
	default:
		if err := k.setPublicKey(key.Key); err != nil {
			return nil, err
		}
	}
	if key.Algorithm != "" {
		alg := allowed[key.Algorithm]
		if alg == nil {
			return nil, fmt.Errorf("the Algorithm %q is not one of JWTOptions.Algorithms", key.Algorithm)
		}
		k.alg = alg
	}
	usable := false
	for _, alg := range order {
		if !k.accepts(alg) {
			continue
		}
		usable = true
		if k.family == familyHMAC && len(k.secret) < alg.sigLen {
			// RFC 7518 requires a key at least as long as the hash output; a
			// shorter one is a password, and a token signed with it can be
			// forged by guessing it offline.
			return nil, fmt.Errorf("the secret is %d bytes, shorter than the %d %s requires; use a random secret of at least %d bytes, or pin the key to a shorter hash with Algorithm",
				len(k.secret), alg.sigLen, alg.name, alg.sigLen)
		}
	}
	if !usable {
		return nil, fmt.Errorf("it is %s, which none of JWTOptions.Algorithms verifies with", describeKeyFamily(k))
	}
	return k, nil
}

// setPublicKey records a public key of a supported type, refusing a private
// key, a key too weak to rely on and a point that is not on its curve.
func (k *verifyKey) setPublicKey(key any) error {
	switch pub := key.(type) {
	case *rsa.PublicKey:
		if pub == nil || pub.N == nil {
			return errors.New("the RSA key is nil")
		}
		if bits := pub.N.BitLen(); bits < minRSABits {
			return fmt.Errorf("the RSA key is %d bits, under the minimum of %d", bits, minRSABits)
		}
		k.family, k.rsa = familyRSA, pub
	case *ecdsa.PublicKey:
		if pub == nil || pub.Curve == nil {
			return errors.New("the ECDSA key is nil")
		}
		if curveBytes(pub.Curve) == 0 {
			return errors.New("the ECDSA key is on a curve no JWS algorithm uses; use P-256, P-384 or P-521")
		}
		if _, err := pub.ECDH(); err != nil {
			return errors.New("the ECDSA key is not a valid point on its curve")
		}
		k.family, k.ec = familyEC, pub
	case ed25519.PublicKey:
		if len(pub) != ed25519.PublicKeySize {
			return fmt.Errorf("the Ed25519 key is %d bytes, not %d", len(pub), ed25519.PublicKeySize)
		}
		k.family, k.ed = familyEd25519, slices.Clone(pub)
	case *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey:
		return errors.New("it is a private key; pass the public half, from its Public method, since verifying needs nothing more")
	case []byte, string:
		return errors.New("an HMAC secret goes in Secret, not Key")
	default:
		return fmt.Errorf("a key of type %T is not supported; use *rsa.PublicKey, *ecdsa.PublicKey or ed25519.PublicKey", key)
	}
	return nil
}

// describeKeyFamily names a key's kind for an error message.
func describeKeyFamily(k *verifyKey) string {
	switch k.family {
	case familyHMAC:
		return "an HMAC secret"
	case familyRSA:
		return "an RSA key"
	case familyEC:
		return "an ECDSA key on " + k.ec.Params().Name
	default:
		return "an Ed25519 key"
	}
}

// curveBytes is the width of a coordinate, and of each half of a signature,
// on the curves JWS uses, and zero on any other.
func curveBytes(c elliptic.Curve) int {
	switch c {
	case elliptic.P256():
		return 32
	case elliptic.P384():
		return 48
	case elliptic.P521():
		return 66
	}
	return 0
}

// jwkSetDocument is a JWKS as published. Each key is kept raw so that one
// key Muzak cannot use does not make the whole set unreadable.
type jwkSetDocument struct {
	Keys []jsontext.Value `json:"keys"`
}

// jwkDocument is one JSON Web Key, RFC 7517, with the members of RFC 7518
// that describe the public keys Muzak verifies with.
type jwkDocument struct {
	Kty    string   `json:"kty"`
	Use    *string  `json:"use"`
	KeyOps []string `json:"key_ops"`
	Alg    *string  `json:"alg"`
	Kid    *string  `json:"kid"`
	Crv    string   `json:"crv"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
	// The private members. A key set is public, and one that publishes a
	// private key has leaked it; such a key is refused rather than trusted.
	D   jsontext.Value `json:"d"`
	P   jsontext.Value `json:"p"`
	Q   jsontext.Value `json:"q"`
	DP  jsontext.Value `json:"dp"`
	DQ  jsontext.Value `json:"dq"`
	QI  jsontext.Value `json:"qi"`
	Oth jsontext.Value `json:"oth"`
	K   jsontext.Value `json:"k"`
}

// errJWKS reports a key set document that cannot be read at all. Its text is
// logged, never sent.
var errJWKS = errors.New("muzak: the key set is not a JSON object with a keys array")

// parseJWKS reads a key set document. The document as a whole must be valid:
// a JSON object, no deeper than [maxJWKSDepth], without duplicate members,
// holding a keys array of at most maxKeys entries. Each key in it is then
// judged on its own, and one that is malformed, of a kind Muzak does not
// verify with, or meant for something other than verifying a signature, is
// skipped and counted rather than failing the set, because an identity
// provider publishes encryption keys beside its signing keys. It is linear in
// the size of the document.
func parseJWKS(data []byte, maxKeys int, allowed map[string]*jwsAlgorithm) (set *keySet, skipped int, err error) {
	if err := checkJSONObject(data, maxJWKSDepth); err != nil {
		return nil, 0, errJWKS
	}
	var doc jwkSetDocument
	if err := json.Unmarshal(data, &doc); err != nil || doc.Keys == nil {
		return nil, 0, errJWKS
	}
	if len(doc.Keys) > maxKeys {
		return nil, 0, fmt.Errorf("muzak: the key set holds %d keys, over the limit of %d", len(doc.Keys), maxKeys)
	}
	set = &keySet{}
	for _, raw := range doc.Keys {
		k, err := parseJWK(raw, allowed)
		if err != nil {
			skipped++
			continue
		}
		set.add(k)
	}
	return set, skipped, nil
}

// errJWK reports a key that is skipped. Which rule it broke is not
// distinguished, since nothing is done with the difference.
var errJWK = errors.New("muzak: the key cannot be used to verify a signature")

// parseJWK reads one key from a key set, strictly: a public RSA, EC or OKP key
// whose every member is the type and length its kind requires, whose point is
// on its curve, that is meant for signatures, and whose algorithm, when it
// names one, is one this scheme allows and fits the key.
func parseJWK(raw jsontext.Value, allowed map[string]*jwsAlgorithm) (*verifyKey, error) {
	var doc jwkDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errJWK
	}
	if len(doc.D) > 0 || len(doc.P) > 0 || len(doc.Q) > 0 || len(doc.DP) > 0 || len(doc.DQ) > 0 ||
		len(doc.QI) > 0 || len(doc.Oth) > 0 || len(doc.K) > 0 {
		return nil, errJWK
	}
	if doc.Use != nil && *doc.Use != "sig" {
		return nil, errJWK
	}
	if doc.KeyOps != nil && !slices.Contains(doc.KeyOps, "verify") {
		return nil, errJWK
	}
	k := &verifyKey{}
	if doc.Kid != nil {
		if *doc.Kid == "" || len(*doc.Kid) > maxKeyIDBytes {
			return nil, errJWK
		}
		k.id, k.hasID = *doc.Kid, true
	}
	var err error
	switch doc.Kty {
	case "RSA":
		k.family = familyRSA
		k.rsa, err = jwkRSAKey(doc.N, doc.E)
	case "EC":
		k.family = familyEC
		k.ec, err = jwkECKey(doc.Crv, doc.X, doc.Y)
	case "OKP":
		k.family = familyEd25519
		k.ed, err = jwkEd25519Key(doc.Crv, doc.X)
	default:
		// "oct" among them: a symmetric key published in a key set is a
		// secret anyone can read, and accepting it would let anyone sign.
		return nil, errJWK
	}
	if err != nil {
		return nil, err
	}
	if doc.Alg != nil {
		alg := allowed[*doc.Alg]
		if alg == nil {
			return nil, errJWK
		}
		k.alg = alg
	}
	for _, alg := range allowed {
		if k.accepts(alg) {
			return k, nil
		}
	}
	// A key no allowed algorithm can use is of no use to this scheme.
	return nil, errJWK
}

// jwkMember decodes one base64url member of a key, strictly.
func jwkMember(s string) ([]byte, bool) {
	if !isBase64URL(s) {
		return nil, false
	}
	b, err := decodeSegment(s)
	return b, err == nil
}

// jwkRSAKey reads an RSA public key: a modulus of 2048 to 8192 bits written
// in its minimal form, and an odd public exponent of at least 3 that fits in
// 32 bits.
func jwkRSAKey(n, e string) (*rsa.PublicKey, error) {
	nb, ok := jwkMember(n)
	if !ok || nb[0] == 0 {
		return nil, errJWK
	}
	eb, ok := jwkMember(e)
	if !ok || eb[0] == 0 || len(eb) > 4 {
		return nil, errJWK
	}
	modulus := new(big.Int).SetBytes(nb)
	if bits := modulus.BitLen(); bits < minRSABits || bits > maxJWKRSABits || modulus.Bit(0) == 0 {
		return nil, errJWK
	}
	exponent := 0
	for _, b := range eb {
		exponent = exponent<<8 | int(b)
	}
	if exponent < 3 || exponent%2 == 0 || exponent > 1<<31-1 {
		return nil, errJWK
	}
	return &rsa.PublicKey{N: modulus, E: exponent}, nil
}

// jwkECKey reads an ECDSA public key on one of the three curves JWS uses,
// with coordinates of exactly the curve's width, and refuses a point that is
// not on the curve, which is what an invalid-curve attack sends.
func jwkECKey(crv, x, y string) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, errJWK
	}
	size := curveBytes(curve)
	xb, okX := jwkMember(x)
	yb, okY := jwkMember(y)
	if !okX || !okY || len(xb) != size || len(yb) != size {
		return nil, errJWK
	}
	point := make([]byte, 0, 1+2*size)
	point = append(point, 4)
	point = append(point, xb...)
	point = append(point, yb...)
	pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
	if err != nil {
		return nil, errJWK
	}
	return pub, nil
}

// jwkEd25519Key reads an Ed25519 public key. Ed448 and the X25519 and X448
// key agreement curves are refused: the first is not implemented by the
// standard library, and the others do not sign.
func jwkEd25519Key(crv, x string) (ed25519.PublicKey, error) {
	if crv != "Ed25519" {
		return nil, errJWK
	}
	xb, ok := jwkMember(x)
	if !ok || len(xb) != ed25519.PublicKeySize {
		return nil, errJWK
	}
	return ed25519.PublicKey(xb), nil
}
