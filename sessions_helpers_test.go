package muzak

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Secrets the session tests encrypt under: exactly the shortest length
// accepted, and a second one for rotation.
const (
	testSessionSecret  = "0123456789abcdef0123456789abcdef"
	otherSessionSecret = "fedcba9876543210fedcba9876543210"
)

// sessionClock is a clock a test moves by hand, so that an idle timeout or a
// lifetime can pass without the test waiting for it.
type sessionClock struct {
	mu sync.Mutex
	at time.Time
}

func newSessionClock() *sessionClock {
	return &sessionClock{at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *sessionClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *sessionClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// sessionIn names the value a session route writes.
type sessionIn struct {
	Value string `query:"value"`
}

// sessionOut reports what a session route saw.
type sessionOut struct {
	Value string `json:"value"`
	Found bool   `json:"found"`
	New   bool   `json:"new"`
}

// readSession reports the value under "v".
func readSession(s *Session) sessionOut {
	value, found := SessionGet[string](s, "v")
	return sessionOut{Value: value, Found: found, New: s.IsNew()}
}

// sessionTestApp builds an application with sessions on a clock the test
// controls, and the routes the session tests share:
//
//	GET  /read        reports "v"
//	GET  /untouched   never asks for the session
//	POST /write       sets "v" to ?value
//	POST /destroy     destroys the session
//	POST /regenerate  regenerates it
//	POST /clear       clears it
//	POST /fail        sets "v", then fails with 400
//	POST /status      sets "v", then answers 404 without an error
//
// configure runs before the application is created and may replace the
// session options entirely; routes adds more.
func sessionTestApp(t *testing.T, configure func(*AppOptions), routes func(*App)) (*App, *sessionClock, *syncBuffer) {
	t.Helper()
	clock := newSessionClock()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	options.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}}
	options.CrossOriginProtection = &CrossOriginOptions{}
	if configure != nil {
		configure(&options)
	}
	if options.Sessions != nil && options.Sessions.now == nil {
		options.Sessions.now = clock.now
	}
	app := New(options)
	app.Get("/read", func(ctx *Context, _ Empty) (sessionOut, error) {
		return readSession(ctx.Session()), nil
	})
	app.Get("/untouched", func(*Context, Empty) (sessionOut, error) { return sessionOut{}, nil })
	app.Post("/write", func(ctx *Context, in sessionIn) (sessionOut, error) {
		s := ctx.Session()
		if err := s.Set("v", in.Value); err != nil {
			return sessionOut{}, err
		}
		return readSession(s), nil
	})
	app.Post("/destroy", func(ctx *Context, _ Empty) (sessionOut, error) {
		ctx.Session().Destroy()
		return sessionOut{}, nil
	})
	app.Post("/regenerate", func(ctx *Context, _ Empty) (sessionOut, error) {
		s := ctx.Session()
		if err := s.Regenerate(); err != nil {
			return sessionOut{}, err
		}
		return readSession(s), nil
	})
	app.Post("/clear", func(ctx *Context, _ Empty) (sessionOut, error) {
		ctx.Session().Clear()
		return sessionOut{}, nil
	})
	app.Post("/fail", func(ctx *Context, in sessionIn) (sessionOut, error) {
		if err := ctx.Session().Set("v", in.Value); err != nil {
			return sessionOut{}, err
		}
		return sessionOut{}, BadRequest("refused after changing the session")
	})
	app.Post("/status", func(ctx *Context, in sessionIn) (sessionOut, error) {
		if err := ctx.Session().Set("v", in.Value); err != nil {
			return sessionOut{}, err
		}
		ctx.SetStatus(http.StatusNotFound)
		return sessionOut{}, nil
	})
	if routes != nil {
		routes(app)
	}
	return mustBuild(t, app), clock, logs
}

// sessionRequest sends one request carrying the given cookies.
func sessionRequest(t *testing.T, app *App, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for _, cookie := range cookies {
		if cookie != nil {
			req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
		}
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

// setCookies returns every Set-Cookie a response carries, parsed.
func setCookies(rec *httptest.ResponseRecorder) []*http.Cookie {
	return (&http.Response{Header: rec.Header()}).Cookies()
}

// sessionCookieOf returns the one cookie named name a response sets, failing
// the test if it sets that name more than once, and nil if it sets none.
func sessionCookieOf(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	var found *http.Cookie
	for _, cookie := range setCookies(rec) {
		if cookie.Name != name {
			continue
		}
		if found != nil {
			t.Fatalf("the response sets %q twice: %q", name, rec.Header().Values("Set-Cookie"))
		}
		found = cookie
	}
	return found
}

// mustSessionCookie is sessionCookieOf for the default name, failing the test
// when the response sets none.
func mustSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cookie := sessionCookieOf(t, rec, "__Host-session")
	if cookie == nil {
		t.Fatalf("the response sets no session cookie; status %d, Set-Cookie %q, body %s",
			rec.Code, rec.Header().Values("Set-Cookie"), rec.Body.String())
	}
	return cookie
}

// assertNoSessionCookie fails the test when a response sets any cookie.
func assertNoSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if values := rec.Header().Values("Set-Cookie"); len(values) > 0 {
		t.Fatalf("the response sets %q, want no cookie", values)
	}
}

// decodeSessionOut decodes a session route's answer.
func decodeSessionOut(t *testing.T, rec *httptest.ResponseRecorder) sessionOut {
	t.Helper()
	assertStatus(t, rec, http.StatusOK)
	var out sessionOut
	decodeJSONBody(t, rec, &out)
	return out
}

// decodeJSONBody decodes a JSON response body into target.
func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("the body is not the JSON expected: %v\n%s", err, rec.Body.String())
	}
}

// recordingSessionStore wraps a memory store, counting calls and optionally
// failing them, and records every key it was given.
type recordingSessionStore struct {
	*MemorySessionStore

	mu        sync.Mutex
	loads     int
	keys      []string
	failLoad  error
	failWrite error
}

func newRecordingSessionStore() *recordingSessionStore {
	return &recordingSessionStore{MemorySessionStore: NewMemorySessionStore(MemorySessionOptions{})}
}

func (s *recordingSessionStore) note(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, key)
}

func (s *recordingSessionStore) Load(ctx context.Context, key string) ([]byte, bool, error) {
	s.note(key)
	s.mu.Lock()
	s.loads++
	fail := s.failLoad
	s.mu.Unlock()
	if fail != nil {
		return nil, false, fail
	}
	return s.MemorySessionStore.Load(ctx, key)
}

func (s *recordingSessionStore) Create(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	s.note(key)
	if err := s.writeFailure(); err != nil {
		return err
	}
	return s.MemorySessionStore.Create(ctx, key, data, ttl)
}

func (s *recordingSessionStore) Update(ctx context.Context, key string, data []byte, ttl time.Duration) (bool, error) {
	s.note(key)
	if err := s.writeFailure(); err != nil {
		return false, err
	}
	return s.MemorySessionStore.Update(ctx, key, data, ttl)
}

func (s *recordingSessionStore) Delete(ctx context.Context, key string) error {
	s.note(key)
	if err := s.writeFailure(); err != nil {
		return err
	}
	return s.MemorySessionStore.Delete(ctx, key)
}

func (s *recordingSessionStore) writeFailure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failWrite
}

func (s *recordingSessionStore) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func (s *recordingSessionStore) seenKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
