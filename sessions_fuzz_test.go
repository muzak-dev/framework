package muzak

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fuzzRecords are the only records the cookie fuzz target ever seals, so any
// value that opens must be one of them.
var fuzzRecords = [][]byte{
	testRecord(),
	append([]byte{recordVersion, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 2}, `{}`...),
	append([]byte{recordVersion, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0}, `{"user":42,"roles":["a","b"]}`...),
}

// FuzzSessionCookie feeds arbitrary cookie values to the decoder. Nothing
// may panic, and nothing may open that this package did not seal: a value
// that opens must decrypt to exactly one of the records sealed for the
// corpus, which is the property that makes a forged session impossible.
func FuzzSessionCookie(f *testing.F) {
	sealer, err := newCookieSealer([]string{testSessionSecret, otherSessionSecret}, "__Host-session")
	if err != nil {
		f.Fatal(err)
	}
	for _, record := range fuzzRecords {
		value, _ := sealer.seal(record)
		f.Add(value)
		f.Add(value[:len(value)-1])
		f.Add(value + "A")
	}
	f.Add("")
	f.Add("AAAA")
	f.Add("not=base64;")
	f.Fuzz(func(t *testing.T, value string) {
		record, ok := sealer.open(value)
		if !ok {
			return
		}
		if len(value) > maxCookieBytes {
			t.Fatalf("a %d-byte value opened", len(value))
		}
		for _, known := range fuzzRecords {
			if bytes.Equal(record, known) {
				return
			}
		}
		t.Fatalf("a value opened to a record nobody sealed: %q", record)
	})
}

// FuzzSessionRecord feeds arbitrary records to the decoder a store's data
// goes through. Nothing may panic, a record that is read is within the
// bound, and encoding what was read and reading it again gives the same
// session.
func FuzzSessionRecord(f *testing.F) {
	m := &sessionManager{
		idle:     time.Hour,
		lifetime: 24 * time.Hour,
		maxSize:  2048,
		now:      func() time.Time { return time.UnixMilli(1 << 40) },
	}
	stamp := []byte{0, 0, 1, 0, 0, 0, 0, 0}
	header := append(append([]byte{recordVersion}, stamp...), stamp...)
	for _, body := range []string{`{}`, `{"a":1}`, `{"a":{"b":[1,2,{"c":null}]}}`, "{\"\u00e9\":\"\xf0\x9f\x98\x80\"}", `{"a":1,"a":2}`, `[]`, `{`} {
		f.Add(append(bytes.Clone(header), body...))
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, record []byte) {
		s := &Session{m: m, data: map[string]sessionValue{}}
		if !m.decodeRecord(s, record) {
			if len(s.data) != 0 && s.encodedSize() > m.maxSize {
				t.Fatal("a refused record left an oversized session behind")
			}
			return
		}
		if s.encodedSize() > m.maxSize {
			t.Fatalf("a record of %d bytes was read past the bound of %d", s.encodedSize(), m.maxSize)
		}
		encoded := s.encode(s.created, s.issued)
		if len(encoded)-recordHeaderSize != s.encodedSize() {
			t.Fatalf("the running size %d disagrees with the encoding, %d bytes", s.encodedSize(), len(encoded)-recordHeaderSize)
		}
		again := &Session{m: m, data: map[string]sessionValue{}}
		if !m.decodeRecord(again, encoded) {
			t.Fatalf("a re-encoded record was refused: %q", encoded)
		}
		if !bytes.Equal(again.encode(again.created, again.issued), encoded) {
			t.Fatal("encoding is not stable across a round trip")
		}
	})
}

// FuzzSessionID feeds arbitrary values to the parser of a server-side
// session's cookie. Whatever it accepts is 256 bits spelled the one way this
// package spells them.
func FuzzSessionID(f *testing.F) {
	id, _ := newSessionID()
	f.Add(id)
	f.Add(id[:42])
	f.Add(id + "A")
	f.Add("")
	f.Fuzz(func(t *testing.T, value string) {
		raw, ok := parseSessionID(value)
		if !ok {
			return
		}
		if len(raw) != sessionIDBytes || cookieEncoding.EncodeToString(raw) != value {
			t.Fatalf("%q was accepted as %x", value, raw)
		}
	})
}

// FuzzSessionRequest sends arbitrary Cookie headers to an application that
// reads its session, through net/http's own parsing. Every one is answered
// 200, signed in or not, and none writes a cookie, since nothing changed.
func FuzzSessionRequest(f *testing.F) {
	clock := newSessionClock()
	options := quietOptions()
	options.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, now: clock.now}
	options.CrossOriginProtection = &CrossOriginOptions{}
	app := New(options)
	app.Get("/read", func(ctx *Context, _ Empty) (sessionOut, error) { return readSession(ctx.Session()), nil })
	if err := app.Build(); err != nil {
		f.Fatal(err)
	}
	// A session that is valid on the test's clock, so the corpus starts from
	// a request that is signed in.
	member := `"v":"x"`
	live := &Session{data: map[string]sessionValue{"v": {raw: []byte(`"x"`), size: len(member)}}, entries: len(member)}
	value, _ := app.sessions.sealer.seal(live.encode(clock.now(), clock.now()))
	f.Add("__Host-session=" + value)
	f.Add("__Host-session=" + value + "; __Host-session=x")
	f.Add("__Host-session=\"" + value + "\"")
	f.Add("a=b; __Host-session=; c")
	f.Fuzz(func(t *testing.T, header string) {
		req := httptest.NewRequest("GET", "/read", nil)
		req.Header.Set("Cookie", header)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Cookie %q answered %d: %s", header, rec.Code, rec.Body.String())
		}
		if len(rec.Header().Values("Set-Cookie")) != 0 {
			t.Fatalf("Cookie %q wrote %q", header, rec.Header().Values("Set-Cookie"))
		}
	})
}

// FuzzCrossOriginHeaders sends arbitrary Origin and Sec-Fetch-Site headers
// with a state-changing request. Each is either served or refused with the
// cross-origin refusal, and a browser's own same-origin request never is.
func FuzzCrossOriginHeaders(f *testing.F) {
	options := quietOptions()
	options.CrossOriginProtection = &CrossOriginOptions{TrustedOrigins: []string{"https://app.example.com"}}
	app := New(options)
	app.Post("/items", func(*Context, Empty) (crossOriginOut, error) { return crossOriginOut{OK: true}, nil })
	if err := app.Build(); err != nil {
		f.Fatal(err)
	}
	f.Add("cross-site", "https://evil.test")
	f.Add("same-origin", "https://api.example.com")
	f.Add("", "null")
	f.Add("same-site", "https://app.example.com")
	f.Fuzz(func(t *testing.T, site, origin string) {
		req := httptest.NewRequest("POST", "/items", nil)
		req.Host = "api.example.com"
		req.Header["Sec-Fetch-Site"] = []string{site}
		req.Header["Origin"] = []string{origin}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusOK:
		case http.StatusForbidden:
			if site == "same-origin" || site == "none" {
				t.Fatalf("a same-origin request was refused: Origin %q", origin)
			}
		default:
			t.Fatalf("Sec-Fetch-Site %q and Origin %q answered %d", site, origin, rec.Code)
		}
	})
}
