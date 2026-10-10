package muzak

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// serverSessionApp is sessionTestApp with a server-side store the test can
// inspect.
func serverSessionApp(t *testing.T, routes func(*App)) (*App, *recordingSessionStore, *sessionClock, *syncBuffer) {
	t.Helper()
	store := newRecordingSessionStore()
	app, clock, logs := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Store: store}
	}, routes)
	return app, store, clock, logs
}

// sessionKeyOf returns the key the store holds a cookie's session under.
func sessionKeyOf(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	id, ok := parseSessionID(cookie.Value)
	if !ok {
		t.Fatalf("the cookie %q does not carry a session identifier", cookie.Value)
	}
	return sessionStoreKey(id)
}

// storeHolds reports whether the store holds a live session under key.
func storeHolds(store *recordingSessionStore, key string) bool {
	_, found, _ := store.MemorySessionStore.Load(context.Background(), key)
	return found
}

// TestServerSessionCookieCarriesOnlyAnIdentifier covers what the cookie and
// the store each hold: the cookie a 256-bit identifier and nothing of the
// data, the store the data under the identifier's digest and never the
// identifier itself.
func TestServerSessionCookieCarriesOnlyAnIdentifier(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=private-data"))
	id, ok := parseSessionID(cookie.Value)
	if !ok || len(id) != 32 {
		t.Fatalf("the cookie %q is not a 256-bit identifier", cookie.Value)
	}
	sum := sha256.Sum256(id)
	if key := sessionKeyOf(t, cookie); key != cookieEncoding.EncodeToString(sum[:]) || !storeHolds(store, key) {
		t.Fatalf("the store does not hold the session under the identifier's digest")
	}
	if slices.Contains(store.seenKeys(), cookie.Value) {
		t.Fatal("the store was given the identifier itself")
	}
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Value != "private-data" {
		t.Fatalf("the session reads %+v", out)
	}
}

// TestServerSessionNeverAdoptsAClientIdentifier covers session fixation by
// adoption: an identifier the application never issued, in the right shape,
// is looked up and found missing, and the session the request then begins is
// given an identifier of the application's own.
func TestServerSessionNeverAdoptsAClientIdentifier(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, nil)
	planted, plantedKey := newSessionID()
	rec := sessionRequest(t, app, "POST", "/write?value=victim", &http.Cookie{Name: "__Host-session", Value: planted})
	issued := mustSessionCookie(t, rec)
	if issued.Value == planted {
		t.Fatal("the session was written under the identifier the client chose")
	}
	if storeHolds(store, plantedKey) {
		t.Fatal("the store holds a session under the planted identifier")
	}
}

// TestServerSessionIgnoresMalformedIdentifiers covers cookies that cannot be
// an identifier this application issued: the store is not asked about them
// at all.
func TestServerSessionIgnoresMalformedIdentifiers(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, nil)
	valid, _ := newSessionID()
	// The last character of an identifier carries two bits nothing uses,
	// which must be zero; one that sets them spells the same bytes another
	// way.
	trailing := strings.Repeat("A", 42) + "B"
	for _, value := range []string{"short", valid + "A", valid[:42], strings.Repeat("A", 4000), valid[:20] + "+" + valid[21:], trailing} {
		before := store.loadCount()
		rec := sessionRequest(t, app, "GET", "/read", &http.Cookie{Name: "__Host-session", Value: value})
		if out := decodeSessionOut(t, rec); out.Found {
			t.Errorf("the cookie %.20q read %+v", value, out)
		}
		if store.loadCount() != before {
			t.Errorf("the store was asked about %.20q", value)
		}
	}
}

// TestServerSessionRegenerate covers what signing in must do: the identifier
// changes, the entry under the old one is deleted, so the identifier an
// attacker planted or saw is worthless, and the values carry over.
func TestServerSessionRegenerate(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, nil)
	before := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=cart"))
	rec := sessionRequest(t, app, "POST", "/regenerate", before)
	after := mustSessionCookie(t, rec)
	if after.Value == before.Value {
		t.Fatal("Regenerate kept the identifier")
	}
	if storeHolds(store, sessionKeyOf(t, before)) {
		t.Fatal("the old identifier's entry survived Regenerate")
	}
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", before)); out.Found {
		t.Errorf("the old identifier still reads %+v", out)
	}
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", after)); out.Value != "cart" {
		t.Errorf("the regenerated session reads %+v, want the values kept", out)
	}
}

// TestCookieSessionRegenerate covers the same for the cookie store: a new
// ciphertext with the values kept, and a new start for the lifetime.
func TestCookieSessionRegenerate(t *testing.T) {
	t.Parallel()
	app, clock, _ := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, IdleTimeout: time.Hour, MaxLifetime: 2 * time.Hour}
	}, nil)
	first := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=cart"))
	clock.advance(50 * time.Minute)
	renewed := mustSessionCookie(t, sessionRequest(t, app, "GET", "/read", first))
	clock.advance(40 * time.Minute)
	// Ninety minutes in, the same session is renewed once more and, from the
	// same cookie, regenerated.
	before := mustSessionCookie(t, sessionRequest(t, app, "GET", "/read", renewed))
	after := mustSessionCookie(t, sessionRequest(t, app, "POST", "/regenerate", renewed))
	if after.Value == before.Value || after.Value == renewed.Value {
		t.Fatal("Regenerate kept the ciphertext")
	}
	clock.advance(45 * time.Minute)
	// 135 minutes after the session began and 45 after either was written:
	// the renewed one is past the lifetime it began with, the regenerated one
	// is 45 minutes into a new one.
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", before)); out.Found {
		t.Errorf("the session read %+v past the lifetime it began with", out)
	}
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", after)); out.Value != "cart" {
		t.Errorf("the regenerated session reads %+v", out)
	}
}

// TestServerSessionDestroyDeletesTheEntry covers signing out with a store:
// the entry goes, so a copy of the cookie taken earlier is worthless.
func TestServerSessionDestroyDeletesTheEntry(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, func(app *App) {
		app.Post("/switch", func(ctx *Context, in sessionIn) (sessionOut, error) {
			s := ctx.Session()
			s.Destroy()
			return readSession(s), s.Set("v", in.Value)
		})
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	stolen := &http.Cookie{Name: cookie.Name, Value: cookie.Value}
	mustSessionCookie(t, sessionRequest(t, app, "POST", "/destroy", cookie))
	if storeHolds(store, sessionKeyOf(t, cookie)) {
		t.Fatal("Destroy left the entry in the store")
	}
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", stolen)); out.Found {
		t.Fatalf("a copy of the cookie still reads %+v after Destroy", out)
	}

	cookie = mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	switched := mustSessionCookie(t, sessionRequest(t, app, "POST", "/switch?value=y", cookie))
	if switched.Value == cookie.Value || storeHolds(store, sessionKeyOf(t, cookie)) {
		t.Fatal("a session begun after Destroy reused the destroyed identifier")
	}
}

// TestServerSessionWriteDoesNotResurrect covers the race a single Save would
// lose: a request that read the session before another request destroyed or
// regenerated it finishes afterwards, and must neither bring the entry back
// nor overwrite the browser's newer cookie.
func TestServerSessionWriteDoesNotResurrect(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"/destroy", "/regenerate"} {
		release := make(chan struct{})
		loaded := make(chan struct{})
		app, store, _, _ := serverSessionApp(t, func(app *App) {
			app.Post("/slow", func(ctx *Context, _ Empty) (sessionOut, error) {
				s := ctx.Session()
				close(loaded)
				<-release
				return readSession(s), s.Set("v", "written late")
			})
		})
		cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- sessionRequest(t, app, "POST", "/slow", cookie) }()
		<-loaded
		sessionRequest(t, app, "POST", ending, cookie)
		close(release)
		rec := <-done
		assertStatus(t, rec, http.StatusOK)
		assertNoSessionCookie(t, rec)
		if storeHolds(store, sessionKeyOf(t, cookie)) {
			t.Errorf("after %s, a request that finished later brought the old session back", ending)
		}
	}
}

// TestServerSessionSavedAfterItEndedIsForgotten covers a request that saves
// a session another request destroyed while it ran. The save writes nothing,
// as the end of the request would, and what the request read of the session
// goes with it: a value set afterwards begins a new session holding that
// value alone. Kept, the values read would be written under a new identifier
// by the next write, and the user who signed out would be signed back in.
func TestServerSessionSavedAfterItEndedIsForgotten(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	loaded := make(chan struct{})
	app, _, _, _ := serverSessionApp(t, func(app *App) {
		app.Post("/slow-save", func(ctx *Context, _ Empty) (sessionOut, error) {
			s := ctx.Session()
			close(loaded)
			<-release
			if err := s.Set("seen", true); err != nil {
				return sessionOut{}, err
			}
			if err := s.Save(); err != nil {
				return sessionOut{}, err
			}
			if !s.IsNew() || s.Has("seen") {
				t.Errorf("after a save that found the session gone, IsNew = %v and Has(seen) = %v", s.IsNew(), s.Has("seen"))
			}
			out := readSession(s)
			return out, s.Set("after", 1)
		})
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=signed-in"))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- sessionRequest(t, app, "POST", "/slow-save", cookie) }()
	<-loaded
	sessionRequest(t, app, "POST", "/destroy", cookie)
	close(release)
	rec := <-done
	if out := decodeSessionOut(t, rec); out.Found {
		t.Errorf("the session reads %+v after another request destroyed it", out)
	}
	if issued := sessionCookieOf(t, rec, "__Host-session"); issued != nil {
		if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", issued)); out.Found {
			t.Errorf("the session written after the destroy carries the destroyed session's value: %+v", out)
		}
	}
}

// TestServerSessionRegeneratedAfterItEndedIsNotRecreated covers a request
// that regenerates a session another request destroyed while it ran, a change
// of privilege in one tab racing a sign-out in another. Regenerating creates
// a new entry rather than updating the old one, so nothing about the write
// itself shows that the old one is gone, and the values it carries over are
// the destroyed session's: written under a new identifier, they would undo
// the sign-out. The request writes nothing instead, and a store that cannot
// say whether the session still exists fails it.
func TestServerSessionRegeneratedAfterItEndedIsNotRecreated(t *testing.T) {
	t.Parallel()
	for _, outage := range []bool{false, true} {
		release := make(chan struct{})
		loaded := make(chan struct{})
		app, store, _, _ := serverSessionApp(t, func(app *App) {
			app.Post("/slow-regenerate", func(ctx *Context, _ Empty) (sessionOut, error) {
				s := ctx.Session()
				close(loaded)
				<-release
				return readSession(s), s.Regenerate()
			})
		})
		cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=signed-in"))
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- sessionRequest(t, app, "POST", "/slow-regenerate", cookie) }()
		<-loaded
		sessionRequest(t, app, "POST", "/destroy", cookie)
		if outage {
			store.mu.Lock()
			store.failLoad = errors.New("store down")
			store.mu.Unlock()
		}
		close(release)
		rec := <-done
		if outage {
			assertStatus(t, rec, http.StatusInternalServerError)
		} else {
			assertStatus(t, rec, http.StatusOK)
		}
		assertNoSessionCookie(t, rec)
		if n := store.Len(); n != 0 {
			t.Errorf("with outage %v, the store holds %d sessions after the only one was destroyed", outage, n)
		}
	}
}

// TestServerSessionLoadFailure covers a store that cannot be read: the
// request proceeds with an empty session that refuses changes, so the user's
// real session is not overwritten by an empty one, the failure is logged
// without the identifier, and signing out still removes the cookie and the
// entry once the store answers again.
func TestServerSessionLoadFailure(t *testing.T) {
	t.Parallel()
	var setErr error
	app, store, _, logs := serverSessionApp(t, func(app *App) {
		app.Post("/try", func(ctx *Context, _ Empty) (sessionOut, error) {
			s := ctx.Session()
			setErr = s.Set("v", "overwrite")
			if err := s.Regenerate(); !errors.Is(err, ErrSessionUnavailable) {
				t.Errorf("Regenerate on an unreadable session = %v", err)
			}
			if err := s.Save(); !errors.Is(err, ErrSessionUnavailable) {
				t.Errorf("Save on an unreadable session = %v", err)
			}
			if !errors.Is(s.Err(), ErrSessionUnavailable) {
				t.Errorf("Err on an unreadable session = %v", s.Err())
			}
			s.Delete("v")
			s.Clear()
			return readSession(s), nil
		})
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=kept"))
	store.mu.Lock()
	store.failLoad = errors.New("connection refused")
	store.mu.Unlock()

	rec := sessionRequest(t, app, "POST", "/try", cookie)
	if out := decodeSessionOut(t, rec); out.Found {
		t.Errorf("an unreadable session reads %+v", out)
	}
	if !errors.Is(setErr, ErrSessionUnavailable) || !strings.Contains(setErr.Error(), "connection refused") {
		t.Errorf("Set on an unreadable session = %v", setErr)
	}
	assertNoSessionCookie(t, rec)
	if !strings.Contains(logs.String(), "the session store could not be read") || containsAny(logs.String(), cookie.Value, sessionKeyOf(t, cookie)) {
		t.Errorf("the failure is not logged, or is logged with the identifier:\n%s", logs.String())
	}

	rec = sessionRequest(t, app, "POST", "/destroy", cookie)
	if cleared := mustSessionCookie(t, rec); cleared.MaxAge >= 0 {
		t.Errorf("Destroy on an unreadable session wrote %q", rec.Header().Get("Set-Cookie"))
	}
	if storeHolds(store, sessionKeyOf(t, cookie)) {
		t.Error("Destroy on an unreadable session left the entry")
	}
}

// hungSessionStore answers no read until the read's context ends, as a store
// does when the network to it has stopped.
type hungSessionStore struct {
	*MemorySessionStore
}

func (hungSessionStore) Load(ctx context.Context, _ string) ([]byte, bool, error) {
	<-ctx.Done()
	return nil, false, ctx.Err()
}

// TestServerSessionLoadCancelledByClient is the regression test for a client
// that hangs up while its session is read being logged as a store failure,
// at error level, which any client could produce at will. The read its own
// request's cancellation ended is logged at debug level, and the request is
// answered as before; a read StoreTimeout ended is the store failing, and
// is still an error.
func TestServerSessionLoadCancelledByClient(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cancel bool
		level  string
	}{
		"the client went away": {true, "DEBUG"},
		"the store timed out":  {false, "ERROR"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := hungSessionStore{NewMemorySessionStore(MemorySessionOptions{})}
			app, _, logs := sessionTestApp(t, func(o *AppOptions) {
				o.Sessions = &SessionOptions{Store: store, StoreTimeout: time.Millisecond}
			}, nil)
			id, _ := newSessionID()
			req := httptest.NewRequest(http.MethodGet, "/read", nil)
			req.AddCookie(&http.Cookie{Name: app.sessions.name, Value: id})
			if tc.cancel {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			rec := doRequest(t, app, req)
			assertStatus(t, rec, http.StatusOK)
			assertNoSessionCookie(t, rec)
			if out := decodeSessionOut(t, rec); out.Found {
				t.Errorf("an unreadable session reads %+v", out)
			}
			want := `"level":"` + tc.level + `","msg":"muzak: the session store could not be read`
			if !strings.Contains(logs.String(), want) {
				t.Errorf("want a line beginning %s; the log holds:\n%s", want, logs.String())
			}
		})
	}
}

// TestServerSessionDeleteFailure covers a store that refuses to delete at
// sign-out: the browser's cookie is removed anyway, and the request fails so
// the user knows the session may outlive it.
func TestServerSessionDeleteFailure(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	store.mu.Lock()
	store.failWrite = errors.New("read-only replica")
	store.mu.Unlock()
	rec := sessionRequest(t, app, "POST", "/destroy", cookie)
	assertStatus(t, rec, http.StatusInternalServerError)
	if cleared := mustSessionCookie(t, rec); cleared.MaxAge >= 0 {
		t.Errorf("a failed deletion did not remove the browser's cookie: %q", rec.Header().Get("Set-Cookie"))
	}
	// A renewal that cannot be written fails the request too, and carries no
	// cookie that would name a session the store does not hold.
	rec = sessionRequest(t, app, "POST", "/write?value=y", cookie)
	assertStatus(t, rec, http.StatusInternalServerError)
	assertNoSessionCookie(t, rec)
	// So does a regeneration whose old entry cannot be deleted: a new
	// session must not be issued while the old one may still be valid.
	rec = sessionRequest(t, app, "POST", "/regenerate", cookie)
	assertStatus(t, rec, http.StatusInternalServerError)
	assertNoSessionCookie(t, rec)
}

// TestServerSessionStreamedWriteFailure covers a store that fails the write
// made as a streamed response starts: the status is already chosen, so the
// failure ends the request the way a late failure does, by aborting the
// response rather than letting it pass for complete, and no cookie names a
// session the store does not hold.
func TestServerSessionStreamedWriteFailure(t *testing.T) {
	t.Parallel()
	app, store, _, logs := serverSessionApp(t, func(app *App) {
		app.Get("/stream", func(ctx *Context, _ Empty) (Empty, error) {
			_ = ctx.Session().Set("v", "streamed")
			_, _ = ctx.ResponseWriter().Write([]byte("first chunk"))
			_ = http.NewResponseController(ctx.ResponseWriter()).Flush()
			return Empty{}, nil
		})
	})
	store.mu.Lock()
	store.failWrite = errors.New("store down")
	store.mu.Unlock()
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	res, err := http.Get(server.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Error("a response whose session could not be saved was completed")
	}
	if len(res.Cookies()) != 0 {
		t.Errorf("the response sets %v", res.Cookies())
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "the session could not be saved") }, "the failure to be logged")
}

// TestServerSessionStreamedAfterLoadFailure covers a streamed response from
// a request whose session could not be read: nothing is written for it, and
// the response is served.
func TestServerSessionStreamedAfterLoadFailure(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, func(app *App) {
		app.Get("/stream", func(ctx *Context, _ Empty) (Empty, error) {
			_ = ctx.Session()
			_, _ = ctx.ResponseWriter().Write([]byte("served"))
			return Empty{}, nil
		})
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	store.mu.Lock()
	store.failLoad = errors.New("store down")
	store.mu.Unlock()
	rec := sessionRequest(t, app, "GET", "/stream", cookie)
	assertStatus(t, rec, http.StatusOK)
	assertNoSessionCookie(t, rec)
	if rec.Body.String() != "served" {
		t.Errorf("the body is %q", rec.Body.String())
	}
}

// TestServerSessionExpiredEntryIsReplaced covers an entry the store still
// holds past the session's own idle timeout: it reads as no session, and the
// session the request begins replaces it under a new identifier.
func TestServerSessionExpiredEntryIsReplaced(t *testing.T) {
	t.Parallel()
	app, store, clock, _ := serverSessionApp(t, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	clock.advance(DefaultSessionIdleTimeout + time.Second)
	rec := sessionRequest(t, app, "POST", "/write?value=y", cookie)
	fresh := mustSessionCookie(t, rec)
	if fresh.Value == cookie.Value || storeHolds(store, sessionKeyOf(t, cookie)) {
		t.Fatal("an expired session was continued, or its entry kept")
	}
}

// TestSessionStoreLifecycle shows a store that manages something is started
// with the application and stopped with it.
func TestSessionStoreLifecycle(t *testing.T) {
	t.Parallel()
	store := &lifecycleSessionStore{MemorySessionStore: NewMemorySessionStore(MemorySessionOptions{})}
	options := quietOptions()
	options.Sessions = &SessionOptions{Store: store}
	options.CrossOriginProtection = &CrossOriginOptions{}
	app := mustBuild(t, New(options))
	if !slices.ContainsFunc(app.lifecycle.components, func(c Lifecycle) bool { return c == Lifecycle(store) }) {
		t.Fatal("a session store implementing Lifecycle is not one of the application's components")
	}
}

// lifecycleSessionStore is a store that is also a component.
type lifecycleSessionStore struct {
	*MemorySessionStore
}

func (*lifecycleSessionStore) Name() string                { return "session-store" }
func (*lifecycleSessionStore) Start(context.Context) error { return nil }
func (*lifecycleSessionStore) Stop(context.Context) error  { return nil }
