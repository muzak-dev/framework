package muzak

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testSealer returns a sealer under the given secrets for the default name.
func testSealer(t *testing.T, name string, secrets ...string) *cookieSealer {
	t.Helper()
	sealer, err := newCookieSealer(secrets, name)
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

// testRecord is a well-formed record holding {"v":"x"}.
func testRecord() []byte {
	record := []byte{recordVersion}
	record = append(record, make([]byte, 16)...)
	return append(record, `{"v":"x"}`...)
}

// TestCookieSealerRoundTrip covers the plain case, and that sealing the same
// record twice gives two unrelated ciphertexts, so a cookie says nothing
// about whether the session behind it changed.
func TestCookieSealerRoundTrip(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t, "__Host-session", testSessionSecret)
	record := testRecord()
	first, err := sealer.seal(record)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := sealer.seal(record)
	if first == second {
		t.Fatal("sealing one record twice gave the same cookie; the nonce is not random")
	}
	for _, value := range []string{first, second} {
		got, ok := sealer.open(value)
		if !ok || !bytes.Equal(got, record) {
			t.Fatalf("open(seal(record)) = %q, %v", got, ok)
		}
		if strings.Contains(value, "v") && strings.Contains(value, `"x"`) {
			t.Fatalf("the cookie %q carries the plaintext", value)
		}
		if len(value) != cookieEncoding.EncodedLen(cookieOverhead+len(record)) {
			t.Fatalf("the cookie is %d characters, want %d", len(value), cookieEncoding.EncodedLen(cookieOverhead+len(record)))
		}
	}
}

// TestCookieSealerRefusesEveryBitFlip flips every bit of a sealed cookie,
// the version, the nonce, the ciphertext and the tag alike, and requires each
// forgery to be refused.
func TestCookieSealerRefusesEveryBitFlip(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t, "__Host-session", testSessionSecret)
	value, _ := sealer.seal(testRecord())
	raw, err := cookieEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		for bit := range 8 {
			forged := bytes.Clone(raw)
			forged[i] ^= 1 << bit
			if _, ok := sealer.open(cookieEncoding.EncodeToString(forged)); ok {
				t.Fatalf("a cookie with bit %d of byte %d flipped was accepted", bit, i)
			}
		}
	}
}

// TestCookieSealerRefusesTruncationAndExtension cuts a cookie at every
// length, in its encoded and its decoded form, and extends it, and requires
// each to be refused.
func TestCookieSealerRefusesTruncationAndExtension(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t, "__Host-session", testSessionSecret)
	value, _ := sealer.seal(testRecord())
	for n := range len(value) {
		if _, ok := sealer.open(value[:n]); ok {
			t.Fatalf("the cookie cut to %d characters was accepted", n)
		}
	}
	raw, _ := cookieEncoding.DecodeString(value)
	for n := range len(raw) {
		if _, ok := sealer.open(cookieEncoding.EncodeToString(raw[:n])); ok {
			t.Fatalf("the cookie cut to %d bytes was accepted", n)
		}
	}
	for _, extended := range []string{value + "A", value + "AA", value + "AAAA", "AAAA" + value} {
		if _, ok := sealer.open(extended); ok {
			t.Fatalf("the extended cookie %q was accepted", extended)
		}
	}
}

// TestCookieSealerRefusesOtherSpellings covers the encodings that decode to
// the same bytes as a valid cookie, which are refused so that a cookie has one
// spelling, and every character outside the alphabet.
func TestCookieSealerRefusesOtherSpellings(t *testing.T) {
	t.Parallel()
	sealer := testSealer(t, "__Host-session", testSessionSecret)
	// A value holding a character the standard alphabet spells differently,
	// so that its standard spelling below is another spelling and not the
	// value itself: about one value in fifteen holds neither.
	value, _ := sealer.seal(testRecord())
	for range 100 {
		if strings.ContainsAny(value, "-_") {
			break
		}
		value, _ = sealer.seal(testRecord())
	}
	if !strings.ContainsAny(value, "-_") {
		t.Fatal("a hundred cookies in a row were spelled the same in both alphabets")
	}
	spellings := []string{
		value[:10] + "\n" + value[10:],
		value[:10] + "\r" + value[10:],
		value + "=",
		value + "==",
		strings.NewReplacer("-", "+", "_", "/").Replace(value),
		" " + value,
		value + " ",
		strings.Repeat("A", maxCookieBytes+1),
	}
	// A value whose final character carries bits the decoder would ignore.
	if raw, _ := cookieEncoding.DecodeString(value); len(raw)%3 != 0 {
		last := value[len(value)-1]
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		index := strings.IndexByte(alphabet, last)
		spellings = append(spellings, value[:len(value)-1]+string(alphabet[(index+1)%64]))
	}
	for _, spelling := range spellings {
		if _, ok := sealer.open(spelling); ok {
			t.Errorf("the spelling %q was accepted", spelling)
		}
	}
}

// TestCookieSealerRefusesOtherKeys covers a cookie sealed under a secret the
// application does not list, which is what an attacker who guessed wrong or a
// cookie from another application holds.
func TestCookieSealerRefusesOtherKeys(t *testing.T) {
	t.Parallel()
	value, _ := testSealer(t, "__Host-session", otherSessionSecret).seal(testRecord())
	if _, ok := testSealer(t, "__Host-session", testSessionSecret).open(value); ok {
		t.Fatal("a cookie sealed under another secret was accepted")
	}
}

// TestCookieSealerRotation covers key rotation: the first secret seals, every
// secret opens, and an application that has dropped a secret no longer opens
// what it sealed.
func TestCookieSealerRotation(t *testing.T) {
	t.Parallel()
	oldOnly := testSealer(t, "__Host-session", testSessionSecret)
	rotated := testSealer(t, "__Host-session", otherSessionSecret, testSessionSecret)
	newOnly := testSealer(t, "__Host-session", otherSessionSecret)

	old, _ := oldOnly.seal(testRecord())
	if _, ok := rotated.open(old); !ok {
		t.Fatal("a cookie sealed under the old secret does not open once the new one is added")
	}
	fresh, _ := rotated.seal(testRecord())
	if _, ok := newOnly.open(fresh); !ok {
		t.Fatal("the rotated application does not seal under its first secret")
	}
	if _, ok := oldOnly.open(fresh); ok {
		t.Fatal("a cookie sealed after rotation opens under the old secret alone")
	}
	if _, ok := newOnly.open(old); ok {
		t.Fatal("a cookie sealed under a dropped secret still opens")
	}
}

// TestCookieSealerBindsTheName covers replay under another name: a cookie
// lifted from one application's session cookie and presented as another's,
// sharing the same secrets, is refused.
func TestCookieSealerBindsTheName(t *testing.T) {
	t.Parallel()
	value, _ := testSealer(t, "__Host-session", testSessionSecret).seal(testRecord())
	for _, name := range []string{"__Host-admin", "__host-session", "__Host-session2", "session"} {
		if _, ok := testSealer(t, name, testSessionSecret).open(value); ok {
			t.Errorf("a cookie sealed for __Host-session opened as %q", name)
		}
	}
}

// TestSessionCookieReplayedUnderAnotherName covers the same attack end to
// end: two applications on one set of secrets, and the first one's cookie
// sent to the second under the second's name.
func TestSessionCookieReplayedUnderAnotherName(t *testing.T) {
	t.Parallel()
	user, _, _ := sessionTestApp(t, nil, nil)
	admin, _, _ := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, Name: "__Host-admin"}
	}, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, user, "POST", "/write?value=user"))
	replayed := &http.Cookie{Name: "__Host-admin", Value: cookie.Value}
	if out := decodeSessionOut(t, sessionRequest(t, admin, "GET", "/read", replayed)); out.Found || !out.New {
		t.Fatalf("the replayed cookie read %+v", out)
	}
}

// TestSessionTamperedCookiesReadAsNoSession covers the whole request path for
// cookies that fail to open: each reads as no session, the request succeeds,
// and nothing of the cookie reaches the log.
func TestSessionTamperedCookiesReadAsNoSession(t *testing.T) {
	t.Parallel()
	app, clock, logs := sessionTestApp(t, func(o *AppOptions) {
		o.LoggerOptions = LoggerOptions{}
		o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}}
	}, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=secret-value"))
	raw, _ := cookieEncoding.DecodeString(cookie.Value)
	flipped := bytes.Clone(raw)
	flipped[len(flipped)/2] ^= 0x80
	other, _ := testSealer(t, "__Host-session", otherSessionSecret).seal(testRecord())
	values := []string{
		cookie.Value[:len(cookie.Value)/2],
		cookieEncoding.EncodeToString(flipped),
		other,
		"not base64 at all!",
		strings.Repeat("A", 5000),
	}
	for _, value := range values {
		rec := sessionRequest(t, app, "GET", "/read", &http.Cookie{Name: cookie.Name, Value: value})
		if out := decodeSessionOut(t, rec); out.Found || !out.New {
			t.Errorf("the cookie %.20q... read %+v", value, out)
		}
	}
	clock.advance(DefaultSessionMaxLifetime + time.Second)
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Found {
		t.Errorf("an expired cookie read %+v", out)
	}
	if containsAny(logs.String(), cookie.Value, "secret-value", values[0], values[1]) {
		t.Errorf("the log carries a cookie or its contents:\n%s", logs.String())
	}
}

// TestSessionRecordRefusals covers records that open but must not be read: a
// version this package does not know, timestamps out of order, a body that is
// not an object, and one larger than the session may be.
func TestSessionRecordRefusals(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, nil)
	m := app.sessions
	now := time.UnixMilli(1_000_000)
	m.now = func() time.Time { return now }
	header := func(version byte, created, issued int64) []byte {
		record := []byte{version}
		record = append(record, byte(created>>56), byte(created>>48), byte(created>>40), byte(created>>32),
			byte(created>>24), byte(created>>16), byte(created>>8), byte(created))
		return append(record, byte(issued>>56), byte(issued>>48), byte(issued>>40), byte(issued>>32),
			byte(issued>>24), byte(issued>>16), byte(issued>>8), byte(issued))
	}
	ms := now.UnixMilli()
	cases := map[string][]byte{
		"empty":         nil,
		"header only":   header(recordVersion, ms, ms),
		"version":       append(header(2, ms, ms), "{}"...),
		"issued first":  append(header(recordVersion, ms, ms-1), "{}"...),
		"array":         append(header(recordVersion, ms, ms), "[1]"...),
		"null":          append(header(recordVersion, ms, ms), "null"...),
		"duplicate":     append(header(recordVersion, ms, ms), `{"a":1,"a":2}`...),
		"invalid utf-8": append(header(recordVersion, ms, ms), "{\"\xff\":1}"...),
		"trailing":      append(header(recordVersion, ms, ms), "{} {}"...),
		"oversized":     append(header(recordVersion, ms, ms), `{"v":"`+strings.Repeat("a", m.maxSize)+`"}`...),
		"too old":       append(header(recordVersion, ms-int64(m.lifetime/time.Millisecond)-1, ms), "{}"...),
		"idle":          append(header(recordVersion, ms-int64(m.idle/time.Millisecond)-1, ms-int64(m.idle/time.Millisecond)-1), "{}"...),
	}
	for name, record := range cases {
		s := &Session{m: m, data: map[string]sessionValue{}}
		if m.decodeRecord(s, record) {
			t.Errorf("the %s record was read: %v", name, s.data)
		}
	}
	s := &Session{m: m, data: map[string]sessionValue{}}
	if !m.decodeRecord(s, append(header(recordVersion, ms, ms), `{"v":"x"}`...)) {
		t.Fatal("a well-formed record was refused")
	}
}
