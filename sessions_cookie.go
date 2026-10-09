package muzak

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// The encrypted cookie a session is kept in by default, before it is encoded
// as unpadded base64url:
//
//	version (1 byte) | nonce (24 bytes) | AES-256-GCM ciphertext of the record | tag (16 bytes)
//
// Each configured secret is reduced once, at build, to a pseudorandom key
// with HKDF-Extract under a label of this package's own. Each cookie then
// draws a fresh random 192-bit nonce, and HKDF-Expand turns the key and that
// nonce into an AES-256 key and a GCM nonce used for that cookie alone.
//
// The detour is what lets one secret encrypt any number of cookies. AES-GCM
// with a random 96-bit nonce under a single key may only be used for about
// 2^32 messages before the chance of repeating a nonce becomes a concern, and
// a repeated nonce lets an attacker forge cookies. A busy application
// renewing sessions could reach that in months. With a key per cookie drawn
// from 192 random bits, no key is ever expected to be used twice.
//
// The cookie's name, the version and a label naming the purpose are bound as
// additional data, so a cookie cannot be decrypted under any other name, by
// any other use of the same secrets, or as any other version of the format.
const (
	cookieVersion   = 1
	cookieNonceSize = 24
	cookieTagSize   = 16
	// cookieOverhead is what sealing adds to a record.
	cookieOverhead = 1 + cookieNonceSize + cookieTagSize
	// cookieKeyLabel and cookieMessageLabel separate the keys this package
	// derives from anything else the same secret might be used for.
	cookieKeyLabel     = "muzak.dev/framework session cookie v1"
	cookieMessageLabel = "muzak.dev/framework session cookie message key v1"
	// cookieAAD prefixes the additional data with the purpose.
	cookieAAD = "muzak.dev/framework session v1"
)

// cookieEncoding is unpadded base64url, decoded strictly: a cookie value is
// only ever spelled the one way this package writes it, so a value that
// decodes to the same bytes through different spelling is refused rather than
// accepted twice.
var cookieEncoding = base64.RawURLEncoding.Strict()

// cookieSealer encrypts and decrypts session records under the configured
// secrets.
type cookieSealer struct {
	// keys holds one pseudorandom key per secret, the first of them the one
	// that encrypts.
	keys [][]byte
	// aad is the additional data every cookie is bound to.
	aad []byte
}

// newCookieSealer derives the keys from the secrets, refusing a list that is
// empty and a secret that is too short to be a key.
func newCookieSealer(secrets []string, name string) (*cookieSealer, error) {
	if len(secrets) == 0 {
		return nil, errors.New("muzak: SessionOptions.Secrets is empty; the cookie store encrypts every session " +
			"and needs at least one secret of 32 random bytes, or configure a SessionOptions.Store")
	}
	var errs []error
	keys := make([][]byte, 0, len(secrets))
	for i, secret := range secrets {
		if len(secret) < MinSessionSecretLength {
			// The secret itself is never quoted, not even its length past
			// what the message needs: it is what every cookie is encrypted
			// under.
			errs = append(errs, fmt.Errorf("muzak: SessionOptions.Secrets[%d] is shorter than %d bytes; generate one "+
				"with \"openssl rand -base64 32\"", i, MinSessionSecretLength))
			continue
		}
		key, err := hkdf.Extract(sha256.New, []byte(secret), []byte(cookieKeyLabel))
		if err != nil {
			// coverage: HKDF-Extract fails only for a hash Go's FIPS mode
			// refuses, and SHA-256 is not one of them.
			errs = append(errs, fmt.Errorf("muzak: SessionOptions.Secrets[%d] could not be turned into a key: %w", i, err))
			continue
		}
		keys = append(keys, key)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	aad := make([]byte, 0, 2+len(cookieAAD)+len(name))
	aad = append(aad, cookieVersion)
	aad = append(aad, cookieAAD...)
	aad = append(aad, 0)
	aad = append(aad, name...)
	return &cookieSealer{keys: keys, aad: aad}, nil
}

// aead returns the cipher and GCM nonce one cookie is sealed with, derived
// from a key and the cookie's own nonce.
func (s *cookieSealer) aead(key, nonce []byte) (cipher.AEAD, []byte, error) {
	material, err := hkdf.Expand(sha256.New, key, cookieMessageLabel+string(nonce), 32+12)
	if err != nil {
		// coverage: HKDF-Expand fails only when asked for more than 255
		// blocks of output, and this asks for less than two.
		return nil, nil, err
	}
	block, err := aes.NewCipher(material[:32])
	if err != nil {
		// coverage: a 32-byte key is always a valid AES key.
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		// coverage: NewGCM fails only for a block size other than 16 bytes.
		return nil, nil, err
	}
	return gcm, material[32:], nil
}

// seal encrypts a record under the first secret and encodes it as a cookie
// value.
func (s *cookieSealer) seal(record []byte) (string, error) {
	out := make([]byte, 1+cookieNonceSize, cookieOverhead+len(record))
	out[0] = cookieVersion
	nonce := out[1 : 1+cookieNonceSize]
	// crypto/rand does not fail; a platform that cannot supply randomness
	// ends the process rather than return an error.
	_, _ = rand.Read(nonce)
	gcm, gcmNonce, err := s.aead(s.keys[0], nonce)
	if err != nil {
		// coverage: aead cannot fail for the fixed sizes it is given; see its comments.
		return "", fmt.Errorf("muzak: the session could not be encrypted: %w", err)
	}
	out = gcm.Seal(out, gcmNonce, record, s.aad)
	return cookieEncoding.EncodeToString(out), nil
}

// open decodes and decrypts a cookie value, trying every secret, and reports
// false for anything that is not a cookie this application sealed under one
// of them.
//
// The work is bounded before any is done: a value longer than a cookie can be
// is refused unread, and anything that survives is decrypted at most once per
// configured secret, each attempt linear in its length.
func (s *cookieSealer) open(value string) ([]byte, bool) {
	if len(value) > maxCookieBytes || len(value) < cookieEncoding.EncodedLen(cookieOverhead+recordHeaderSize) || !onlyBase64URL(value) {
		return nil, false
	}
	raw, err := cookieEncoding.DecodeString(value)
	if err != nil || raw[0] != cookieVersion {
		return nil, false
	}
	nonce, sealed := raw[1:1+cookieNonceSize], raw[1+cookieNonceSize:]
	for _, key := range s.keys {
		gcm, gcmNonce, err := s.aead(key, nonce)
		if err != nil {
			// coverage: aead cannot fail for the fixed sizes it is given; see its comments.
			return nil, false
		}
		if record, err := gcm.Open(nil, gcmNonce, sealed, s.aad); err == nil {
			return record, true
		}
	}
	return nil, false
}

// onlyBase64URL reports whether s holds only the base64url alphabet. The
// decoder skips line breaks wherever they appear, which would let one cookie
// be spelled many ways; nothing this package writes contains one.
func onlyBase64URL(s string) bool {
	for i := range len(s) {
		switch b := s[i]; {
		case 'A' <= b && b <= 'Z', 'a' <= b && b <= 'z', '0' <= b && b <= '9', b == '-', b == '_':
		default:
			return false
		}
	}
	return true
}
