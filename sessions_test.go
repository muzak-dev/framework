package muzak

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// TestSessionRoundTrip covers the plain case: a value set by one request is
// read back by the next one that carries the cookie, and a request carrying
// no cookie has no session.
func TestSessionRoundTrip(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, nil)

	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read")); out.Found || !out.New {
		t.Fatalf("a request with no cookie read %+v, want an empty new session", out)
	}
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=hello"))
	out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie))
	if !out.Found || out.Value != "hello" || out.New {
		t.Fatalf("the next request read %+v, want hello from an existing session", out)
	}
}

// TestSessionCookieDefaults pins the attributes the cookie is written with,
// which are what keep it from scripts, from plain HTTP, from other sites and
// from sibling subdomains.
func TestSessionCookieDefaults(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, nil)
	rec := sessionRequest(t, app, "POST", "/write?value=x")
	raw := rec.Header().Get("Set-Cookie")
	cookie := mustSessionCookie(t, rec)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Errorf("the session cookie is %q, want HttpOnly, Secure, SameSite=Lax, Path=/ and no Domain", raw)
	}
	if want := int(DefaultSessionIdleTimeout / time.Second); cookie.MaxAge != want {
		t.Errorf("Max-Age = %d, want the idle timeout, %d", cookie.MaxAge, want)
	}
	if strings.Contains(raw, "Expires") {
		t.Errorf("the cookie carries Expires, which is redundant with Max-Age: %q", raw)
	}
	if got := rec.Header().Get("Cache-Control"); got != privateCacheControl {
		t.Errorf("Cache-Control = %q, want %q on a response that sets a session", got, privateCacheControl)
	}
	if !varyNames(rec.Header().Values("Vary"), "Cookie") {
		t.Errorf("Vary = %q, want it to name Cookie", rec.Header().Values("Vary"))
	}
}

// TestSessionCookieNames covers the default name each combination of
// attributes earns: the __Host- prefix needs Secure, Path=/ and no Domain,
// __Secure- needs Secure, and a cookie that is not Secure gets neither.
func TestSessionCookieNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		opts   SessionOptions
		want   string
		secure bool
	}{
		{"default", SessionOptions{}, "__Host-session", true},
		{"domain", SessionOptions{Domain: "example.com"}, "__Secure-session", true},
		{"path", SessionOptions{Path: "/app"}, "__Secure-session", true},
		{"insecure", SessionOptions{AllowInsecure: true}, "session", false},
		{"named", SessionOptions{Name: "sid"}, "sid", true},
		{"strict", SessionOptions{SameSite: http.SameSiteStrictMode}, "__Host-session", true},
		{"none", SessionOptions{SameSite: http.SameSiteNoneMode}, "__Host-session", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, _, _ := sessionTestApp(t, func(o *AppOptions) {
				opts := tc.opts
				opts.Secrets = []string{testSessionSecret}
				o.Sessions = &opts
			}, nil)
			target := "/write?value=x"
			if tc.opts.Path != "" {
				// The route answers at /write, outside the cookie's path,
				// which a browser would honour and the response does not
				// care about.
				target = "/write?value=y"
			}
			rec := sessionRequest(t, app, "POST", target)
			cookie := sessionCookieOf(t, rec, tc.want)
			if cookie == nil {
				t.Fatalf("no cookie named %q in %q", tc.want, rec.Header().Values("Set-Cookie"))
			}
			if cookie.Secure != tc.secure {
				t.Errorf("Secure = %v, want %v", cookie.Secure, tc.secure)
			}
			if tc.opts.SameSite != 0 && cookie.SameSite != tc.opts.SameSite {
				t.Errorf("SameSite = %v, want %v", cookie.SameSite, tc.opts.SameSite)
			}
			if tc.opts.Domain != "" && cookie.Domain != tc.opts.Domain {
				t.Errorf("Domain = %q, want %q", cookie.Domain, tc.opts.Domain)
			}
		})
	}
}

// TestSessionOptionsAreValidated covers every configuration that is refused
// when the application is built rather than served as a weaker cookie than it
// appears to be, or one a browser would silently drop.
func TestSessionOptionsAreValidated(t *testing.T) {
	t.Parallel()
	secret := []string{testSessionSecret}
	cases := []struct {
		name  string
		opts  *SessionOptions
		csrf  bool
		wants []string
	}{
		{"no secret", &SessionOptions{}, true, []string{"Secrets is empty"}},
		{"short secret", &SessionOptions{Secrets: []string{testSessionSecret, "too-short"}}, true, []string{"Secrets[1] is shorter than 32 bytes"}},
		{"negative idle", &SessionOptions{Secrets: secret, IdleTimeout: -time.Second}, true, []string{"IdleTimeout is -1s"}},
		{"negative lifetime", &SessionOptions{Secrets: secret, MaxLifetime: -time.Second}, true, []string{"MaxLifetime is -1s"}},
		{"idle past lifetime", &SessionOptions{Secrets: secret, IdleTimeout: 2 * time.Hour, MaxLifetime: time.Hour}, true, []string{"could never end a session"}},
		{"negative timeout", &SessionOptions{Secrets: secret, StoreTimeout: -time.Second}, true, []string{"StoreTimeout is -1s"}},
		{"negative size", &SessionOptions{Secrets: secret, MaxSize: -1}, true, []string{"MaxSize is -1"}},
		{"size past a cookie", &SessionOptions{Secrets: secret, MaxSize: 4000}, true, []string{"an encrypted cookie can carry at most"}},
		{"none without secure", &SessionOptions{Secrets: secret, SameSite: http.SameSiteNoneMode, AllowInsecure: true}, true, []string{"SameSite None requires a Secure cookie"}},
		{"no samesite", &SessionOptions{Secrets: secret, SameSite: http.SameSiteDefaultMode}, true, []string{"is not Lax, Strict or None"}},
		{"name not a token", &SessionOptions{Secrets: secret, Name: "my session"}, true, []string{"is not one a browser accepts"}},
		{"host prefix with domain", &SessionOptions{Secrets: secret, Name: "__Host-sid", Domain: "example.com"}, true, []string{"__Host- prefix"}},
		{"host prefix with path", &SessionOptions{Secrets: secret, Name: "__Host-sid", Path: "/app"}, true, []string{"__Host- prefix"}},
		{"host prefix insecure", &SessionOptions{Secrets: secret, Name: "__Host-sid", AllowInsecure: true}, true, []string{"__Host- prefix"}},
		{"host prefix in another case", &SessionOptions{Secrets: secret, Name: "__HOST-sid", Domain: "example.com"}, true, []string{"__Host- prefix"}},
		{"secure prefix insecure", &SessionOptions{Secrets: secret, Name: "__secure-sid", AllowInsecure: true}, true, []string{"__Secure- prefix"}},
		{"relative path", &SessionOptions{Secrets: secret, Path: "app"}, true, []string{"must begin with a slash"}},
		{"bad domain", &SessionOptions{Secrets: secret, Domain: "exa mple.com"}, true, []string{"is not one a browser accepts"}},
		{"name leaves no room", &SessionOptions{Secrets: secret, Name: strings.Repeat("n", 3800)}, true, []string{"leave room for only"}},
		{"no cross-origin protection", &SessionOptions{Secrets: secret}, false, []string{"without AppOptions.CrossOriginProtection"}},
		{"everything at once", &SessionOptions{IdleTimeout: -1, StoreTimeout: -1, MaxSize: -1, SameSite: 9}, false, []string{
			"IdleTimeout", "StoreTimeout", "MaxSize", "Lax, Strict or None", "Secrets is empty", "CrossOriginProtection",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := quietOptions()
			options.Sessions = tc.opts
			if tc.csrf {
				options.CrossOriginProtection = &CrossOriginOptions{}
			}
			message := buildError(t, New(options))
			for _, want := range tc.wants {
				if !strings.Contains(message, want) {
					t.Errorf("the build error does not mention %q:\n%s", want, message)
				}
			}
			for _, secret := range tc.opts.Secrets {
				if len(secret) > 0 && strings.Contains(message, secret) {
					t.Errorf("the build error quotes a secret:\n%s", message)
				}
			}
		})
	}
}

// TestSessionMaxSizeWithinACookie shows a bound smaller than a cookie's is
// the one enforced.
func TestSessionMaxSizeWithinACookie(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, MaxSize: 100}
	}, nil)
	if app.sessions.maxSize != 100 {
		t.Fatalf("the bound is %d, want 100", app.sessions.maxSize)
	}
	s := &Session{m: app.sessions, c: &Context{}, data: map[string]sessionValue{}, isNew: true}
	if err := s.Set("v", strings.Repeat("a", 92)); err != nil {
		t.Fatalf("a session of exactly 100 bytes was refused: %v", err)
	}
	if err := s.Set("w", 1); !errors.Is(err, ErrSessionTooLarge) {
		t.Fatalf("a session past 100 bytes = %v", err)
	}
}

// TestSessionServerStoreNeedsNoSecret shows the secrets are the cookie
// store's alone, and that a store's own bound applies.
func TestSessionServerStoreNeedsNoSecret(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.Sessions = &SessionOptions{Store: NewMemorySessionStore(MemorySessionOptions{})}
	options.CrossOriginProtection = &CrossOriginOptions{}
	app := mustBuild(t, New(options))
	if app.sessions.maxSize != DefaultSessionMaxSize {
		t.Errorf("a server store's bound is %d, want %d", app.sessions.maxSize, DefaultSessionMaxSize)
	}
}

// TestSessionUntouchedCostsNothing shows that a request whose handler never
// asks for its session writes no cookie, varies on nothing and does not even
// look at the cookie it carries, however broken.
func TestSessionUntouchedCostsNothing(t *testing.T) {
	t.Parallel()
	store := newRecordingSessionStore()
	app, _, _ := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Store: store}
	}, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	loads := store.loadCount()
	for _, sent := range []*http.Cookie{nil, cookie, {Name: cookie.Name, Value: "garbage"}} {
		rec := sessionRequest(t, app, "GET", "/untouched", sent)
		assertStatus(t, rec, http.StatusOK)
		assertNoSessionCookie(t, rec)
		if varyNames(rec.Header().Values("Vary"), "Cookie") {
			t.Errorf("an untouched session added Cookie to Vary: %q", rec.Header().Values("Vary"))
		}
		if got := rec.Header().Get("Cache-Control"); got != "" {
			t.Errorf("an untouched session set Cache-Control %q", got)
		}
	}
	if store.loadCount() != loads {
		t.Errorf("the store was read %d times by requests that never asked", store.loadCount()-loads)
	}
}

// TestSessionReadOnlyWritesNothing shows that reading a session is not a
// reason to rewrite it, though the response does depend on it.
func TestSessionReadOnlyWritesNothing(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	rec := sessionRequest(t, app, "GET", "/read", cookie)
	assertNoSessionCookie(t, rec)
	if !varyNames(rec.Header().Values("Vary"), "Cookie") {
		t.Errorf("a response that read the session does not vary on Cookie: %q", rec.Header().Values("Vary"))
	}
	if got := rec.Header().Get("Cache-Control"); got != privateCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, privateCacheControl)
	}
	// An empty session that is read and left alone writes nothing either.
	assertNoSessionCookie(t, sessionRequest(t, app, "GET", "/read"))
}

// TestSessionRenewal covers the sliding renewal: nothing is written until a
// tenth of the idle timeout has passed since the cookie was written, and then
// the session is written again with the same values and a fresh idle window.
func TestSessionRenewal(t *testing.T) {
	t.Parallel()
	app, clock, _ := sessionTestApp(t, nil, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	renew := DefaultSessionIdleTimeout / 10

	clock.advance(renew - time.Second)
	assertNoSessionCookie(t, sessionRequest(t, app, "GET", "/read", cookie))

	clock.advance(time.Second)
	rec := sessionRequest(t, app, "GET", "/read", cookie)
	renewed := mustSessionCookie(t, rec)
	if renewed.Value == cookie.Value {
		t.Fatal("the renewed cookie is the same ciphertext")
	}
	if out := decodeSessionOut(t, rec); out.Value != "x" {
		t.Fatalf("the renewing request read %+v", out)
	}
	// The renewal restarted the idle window: past the first cookie's idle
	// timeout, the renewed one still reads.
	clock.advance(DefaultSessionIdleTimeout - renew + time.Minute)
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Found {
		t.Errorf("the original cookie still reads %+v past its idle timeout", out)
	}
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", renewed)); !out.Found {
		t.Errorf("the renewed cookie does not read: %+v", out)
	}
}

// TestSessionIdleTimeout covers the idle bound, which is kept inside the
// encrypted cookie: a cookie replayed after the timeout reads as no session,
// whatever Max-Age the browser was told.
func TestSessionIdleTimeout(t *testing.T) {
	t.Parallel()
	for _, store := range []SessionStore{nil, NewMemorySessionStore(MemorySessionOptions{})} {
		app, clock, _ := sessionTestApp(t, func(o *AppOptions) {
			o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, Store: store, IdleTimeout: time.Minute, MaxLifetime: time.Hour}
		}, nil)
		// Two sessions written together, so that reading the first, which
		// renews it, leaves the second as it was.
		atLimit := sessionCookieOf(t, sessionRequest(t, app, "POST", "/write?value=x"), app.sessions.name)
		pastLimit := sessionCookieOf(t, sessionRequest(t, app, "POST", "/write?value=x"), app.sessions.name)
		clock.advance(time.Minute)
		if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", atLimit)); !out.Found {
			t.Fatalf("a session exactly at its idle timeout does not read: %+v", out)
		}
		clock.advance(time.Millisecond)
		if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", pastLimit)); out.Found || !out.New {
			t.Errorf("a session past its idle timeout reads %+v", out)
		}
	}
}

// TestSessionMaxLifetime covers the absolute bound: a session kept busy, and
// so renewed again and again, still ends when its lifetime does.
func TestSessionMaxLifetime(t *testing.T) {
	t.Parallel()
	for _, store := range []SessionStore{nil, NewMemorySessionStore(MemorySessionOptions{})} {
		app, clock, _ := sessionTestApp(t, func(o *AppOptions) {
			o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, Store: store, IdleTimeout: 10 * time.Minute, MaxLifetime: time.Hour}
		}, nil)
		name := app.sessions.name
		cookie := sessionCookieOf(t, sessionRequest(t, app, "POST", "/write?value=x"), name)
		for elapsed := time.Duration(0); elapsed < time.Hour-5*time.Minute; elapsed += 5 * time.Minute {
			clock.advance(5 * time.Minute)
			rec := sessionRequest(t, app, "GET", "/read", cookie)
			if out := decodeSessionOut(t, rec); !out.Found {
				t.Fatalf("a busy session ended after %s, before its lifetime", elapsed+5*time.Minute)
			}
			if renewed := sessionCookieOf(t, rec, name); renewed != nil {
				cookie = renewed
				// Max-Age never promises more than the lifetime has left.
				if left := time.Hour - elapsed - 5*time.Minute; time.Duration(renewed.MaxAge)*time.Second > left {
					t.Errorf("Max-Age %ds outlives the %s left of the lifetime", renewed.MaxAge, left)
				}
			}
		}
		clock.advance(5*time.Minute + time.Millisecond)
		if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Found {
			t.Errorf("a session read %+v past its lifetime", out)
		}
	}
}

// TestSessionFailedRequestsDoNotWrite covers the rule that only a request
// that succeeds keeps its changes: one that returns an error, one that
// answers with an error status, and one that panics write nothing.
func TestSessionFailedRequestsDoNotWrite(t *testing.T) {
	t.Parallel()
	app, _, logs := sessionTestApp(t, nil, func(app *App) {
		app.Post("/panic", func(ctx *Context, _ Empty) (sessionOut, error) {
			_ = ctx.Session().Set("v", "panicked")
			panic("boom")
		})
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=before"))
	for _, target := range []string{"/fail?value=after", "/status?value=after", "/panic"} {
		rec := sessionRequest(t, app, "POST", target, cookie)
		if rec.Code < 400 {
			t.Fatalf("%s answered %d", target, rec.Code)
		}
		assertNoSessionCookie(t, rec)
	}
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Value != "before" {
		t.Errorf("the session reads %+v after failed requests, want before", out)
	}
	if containsAny(logs.String(), cookie.Value) {
		t.Errorf("a log line carries the session cookie:\n%s", logs.String())
	}
}

// TestSessionSaveKeepsAChangeOnFailure covers Save: a change saved before a
// failure is kept, a change made after it is not, and the response carries
// one cookie for the session, never two.
func TestSessionSaveKeepsAChangeOnFailure(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, func(app *App) {
		app.Post("/attempt", func(ctx *Context, in sessionIn) (sessionOut, error) {
			s := ctx.Session()
			if err := s.Set("v", in.Value); err != nil {
				return sessionOut{}, err
			}
			if err := s.Save(); err != nil {
				return sessionOut{}, err
			}
			if err := s.Set("v", in.Value+"-unsaved"); err != nil {
				return sessionOut{}, err
			}
			return sessionOut{}, Unauthorized("wrong password")
		})
		app.Post("/twice", func(ctx *Context, in sessionIn) (sessionOut, error) {
			s := ctx.Session()
			_ = s.Set("v", in.Value)
			if err := s.Save(); err != nil {
				return sessionOut{}, err
			}
			_ = s.Set("v", in.Value+"-final")
			return readSession(s), nil
		})
	})
	rec := sessionRequest(t, app, "POST", "/attempt?value=one")
	assertStatus(t, rec, http.StatusUnauthorized)
	cookie := mustSessionCookie(t, rec)
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Value != "one" {
		t.Errorf("the saved change reads %+v, want one", out)
	}

	rec = sessionRequest(t, app, "POST", "/twice?value=two", cookie)
	cookie = mustSessionCookie(t, rec)
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Value != "two-final" {
		t.Errorf("the change after Save reads %+v, want two-final", out)
	}
}

// TestSessionReleaseFailureDiscardsTheChange shows the session is written
// after the releases, so a transaction whose commit fails takes the session
// change with it.
func TestSessionReleaseFailureDiscardsTheChange(t *testing.T) {
	t.Parallel()
	type tx struct{}
	app, _, _ := sessionTestApp(t, nil, func(app *App) {
		app.Post("/commit", func(ctx *Context, _ Empty) (sessionOut, error) {
			_ = From[tx](ctx)
			_ = ctx.Session().Set("v", "committed")
			return sessionOut{}, nil
		}, Acquire(func(*Context) (tx, Release, error) {
			return tx{}, func(failure error) error { return errors.New("the commit failed") }, nil
		}))
	})
	rec := sessionRequest(t, app, "POST", "/commit")
	assertStatus(t, rec, http.StatusInternalServerError)
	assertNoSessionCookie(t, rec)
}

// TestSessionDestroy covers signing out: the cookie is removed, the next
// request has no session, and a value set after Destroy in the same request
// begins a new one.
func TestSessionDestroy(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, func(app *App) {
		app.Post("/switch", func(ctx *Context, in sessionIn) (sessionOut, error) {
			s := ctx.Session()
			s.Destroy()
			if err := s.Set("v", in.Value); err != nil {
				return sessionOut{}, err
			}
			return readSession(s), nil
		})
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	rec := sessionRequest(t, app, "POST", "/destroy", cookie)
	cleared := mustSessionCookie(t, rec)
	if cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("Destroy wrote %q, want the cookie removed", rec.Header().Get("Set-Cookie"))
	}
	if !cleared.Secure || !cleared.HttpOnly || cleared.Path != "/" {
		t.Errorf("the removal %q does not carry the attributes a browser needs to match the cookie", rec.Header().Get("Set-Cookie"))
	}
	// Destroying a session the request did not have writes nothing.
	assertNoSessionCookie(t, sessionRequest(t, app, "POST", "/destroy"))

	rec = sessionRequest(t, app, "POST", "/switch?value=y", cookie)
	switched := mustSessionCookie(t, rec)
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", switched)); out.Value != "y" {
		t.Errorf("the session begun after Destroy reads %+v", out)
	}
}

// TestSessionClear covers Clear: an emptied session is removed from the
// browser rather than written as an empty one, and an empty new session is
// never written at all.
func TestSessionClear(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, func(app *App) {
		app.Post("/set-delete", func(ctx *Context, _ Empty) (sessionOut, error) {
			s := ctx.Session()
			_ = s.Set("v", "x")
			s.Delete("v")
			s.Delete("missing")
			return readSession(s), nil
		})
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=x"))
	rec := sessionRequest(t, app, "POST", "/clear", cookie)
	if cleared := mustSessionCookie(t, rec); cleared.MaxAge >= 0 {
		t.Errorf("an emptied session wrote %q, want the cookie removed", rec.Header().Get("Set-Cookie"))
	}
	assertNoSessionCookie(t, sessionRequest(t, app, "POST", "/clear"))
	assertNoSessionCookie(t, sessionRequest(t, app, "POST", "/set-delete"))
}

// fillIn asks for a session value of N bytes.
type fillIn struct {
	N int `query:"n"`
}

// TestSessionSizeIsBounded drives the bound to its edge: the largest value
// that fits is accepted and produces a cookie within 4096 bytes, and one byte
// more is refused with ErrSessionTooLarge where it is set, leaving the session
// as it was.
func TestSessionSizeIsBounded(t *testing.T) {
	t.Parallel()
	var setErr error
	app, _, _ := sessionTestApp(t, nil, func(app *App) {
		app.Post("/fill", func(ctx *Context, in fillIn) (sessionOut, error) {
			s := ctx.Session()
			setErr = s.Set("v", strings.Repeat("a", in.N))
			return readSession(s), nil
		})
	})
	limit := app.sessions.maxSize
	// {"v":"..."} is nine bytes around the value.
	fits := limit - len(`{"v":""}`)

	rec := sessionRequest(t, app, "POST", "/fill?n="+strconv.Itoa(fits))
	if setErr != nil {
		t.Fatalf("a value that fits exactly was refused: %v", setErr)
	}
	raw := rec.Header().Get("Set-Cookie")
	if len(raw) > maxCookieBytes {
		t.Fatalf("the fullest session is a %d-byte cookie, over %d", len(raw), maxCookieBytes)
	}
	cookie := mustSessionCookie(t, rec)
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); len(out.Value) != fits {
		t.Fatalf("the fullest session reads back %d bytes, want %d", len(out.Value), fits)
	}

	rec = sessionRequest(t, app, "POST", "/fill?n="+strconv.Itoa(fits+1), cookie)
	if !errors.Is(setErr, ErrSessionTooLarge) {
		t.Fatalf("one byte over the bound returned %v, want ErrSessionTooLarge", setErr)
	}
	// Nothing changed, so nothing is written, and the session still holds
	// what fitted.
	assertNoSessionCookie(t, rec)
	if out := decodeSessionOut(t, rec); len(out.Value) != fits {
		t.Errorf("a refused Set changed the session to %d bytes", len(out.Value))
	}
}

// TestSessionSizeWithManyKeys checks the running size against the encoding
// itself, over keys that need escaping and values replaced and deleted.
func TestSessionSizeWithManyKeys(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Store: NewMemorySessionStore(MemorySessionOptions{})}
	}, nil)
	ctx := &Context{app: app}
	s := &Session{m: app.sessions, c: ctx, data: map[string]sessionValue{}, isNew: true}
	keys := []string{"plain", "with \"quotes\"", "tab\there", "<html>", "\u00e9t\u00e9", "\u2028"}
	for i, key := range keys {
		if err := s.Set(key, map[string]any{"i": i, "s": key}); err != nil {
			t.Fatal(err)
		}
		if err := s.Set(key, []int{i, i}); err != nil {
			t.Fatal(err)
		}
	}
	s.Delete(keys[2])
	record := s.encode(time.Unix(0, 0), time.Unix(0, 0))
	if got, want := len(record)-recordHeaderSize, s.encodedSize(); got != want {
		t.Fatalf("the session encodes to %d bytes, and its running size says %d", got, want)
	}
	decoded := &Session{m: app.sessions, data: map[string]sessionValue{}}
	app.sessions.now = func() time.Time { return time.Unix(0, 0) }
	if !app.sessions.decodeRecord(decoded, record) {
		t.Fatal("the record does not decode")
	}
	if decoded.encodedSize() != s.encodedSize() || len(decoded.data) != len(keys)-1 {
		t.Errorf("decoding gives %d bytes in %d keys, want %d in %d", decoded.encodedSize(), len(decoded.data), s.encodedSize(), len(keys)-1)
	}
}

// TestSessionSetRefusals covers the values Set refuses, each leaving the
// session untouched.
func TestSessionSetRefusals(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, nil)
	s := &Session{m: app.sessions, c: &Context{}, data: map[string]sessionValue{}, isNew: true}
	cases := []struct {
		key   string
		value any
		want  string
	}{
		{"", "x", "must not be empty"},
		{"bad\xffkey", "x", "not valid UTF-8"},
		{"v", make(chan int), "could not be encoded as JSON"},
		{"v", func() {}, "could not be encoded as JSON"},
	}
	for _, tc := range cases {
		err := s.Set(tc.key, tc.value)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "muzak: ") {
			t.Errorf("Set(%q, %T) = %v, want an error saying %q", tc.key, tc.value, err, tc.want)
		}
	}
	if len(s.data) != 0 || s.changed {
		t.Errorf("refused values changed the session: %v", s.data)
	}
}

// TestSessionGetDecodes covers SessionGet: a value reads back as the type it
// was stored as, a missing one and one that does not decode as the type asked
// for read as absent.
func TestSessionGetDecodes(t *testing.T) {
	t.Parallel()
	type profile struct {
		ID    int64    `json:"id"`
		Roles []string `json:"roles"`
	}
	app, _, _ := sessionTestApp(t, nil, nil)
	s := &Session{m: app.sessions, c: &Context{}, data: map[string]sessionValue{}, isNew: true}
	want := profile{ID: 7, Roles: []string{"admin"}}
	if err := s.Set("profile", want); err != nil {
		t.Fatal(err)
	}
	got, ok := SessionGet[profile](s, "profile")
	if !ok || got.ID != want.ID || len(got.Roles) != 1 || got.Roles[0] != "admin" {
		t.Errorf("SessionGet = %+v, %v", got, ok)
	}
	if n, ok := SessionGet[int](s, "profile"); ok || n != 0 {
		t.Errorf("a value that is not an int read as %d, %v", n, ok)
	}
	if _, ok := SessionGet[string](s, "missing"); ok || s.Has("missing") || !s.Has("profile") {
		t.Error("a missing key reads as present")
	}
}

// TestSessionStreamedResponse covers a handler that writes its response
// itself: the session goes out with the status line when that status is a
// success, and not at all when it is an error the handler wrote.
func TestSessionStreamedResponse(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, nil, func(app *App) {
		app.Post("/stream", func(ctx *Context, in sessionIn) (Empty, error) {
			_ = ctx.Session().Set("v", in.Value)
			w := ctx.ResponseWriter()
			_, _ = w.Write([]byte("first chunk"))
			http.NewResponseController(w).Flush()
			// Too late: the header is gone.
			_ = ctx.Session().Set("v", "too late")
			return Empty{}, nil
		})
		app.Post("/stream-error", func(ctx *Context, in sessionIn) (Empty, error) {
			_ = ctx.Session().Set("v", in.Value)
			ctx.ResponseWriter().WriteHeader(http.StatusInternalServerError)
			return Empty{}, nil
		})
		app.Post("/flush-first", func(ctx *Context, in sessionIn) (Empty, error) {
			_ = ctx.Session().Set("v", in.Value)
			_ = http.NewResponseController(ctx.ResponseWriter()).Flush()
			return Empty{}, nil
		})
		app.Post("/save-late", func(ctx *Context, _ Empty) (Empty, error) {
			_, _ = ctx.ResponseWriter().Write([]byte("x"))
			if err := ctx.Session().Save(); !errors.Is(err, errSessionStarted) {
				t.Errorf("Save after the response started = %v", err)
			}
			return Empty{}, nil
		})
	})
	rec := sessionRequest(t, app, "POST", "/stream?value=streamed")
	cookie := mustSessionCookie(t, rec)
	if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie)); out.Value != "streamed" {
		t.Errorf("the streamed session reads %+v, want the value set before the first byte", out)
	}
	assertNoSessionCookie(t, sessionRequest(t, app, "POST", "/stream-error?value=x"))
	mustSessionCookie(t, sessionRequest(t, app, "POST", "/flush-first?value=x"))
	assertNoSessionCookie(t, sessionRequest(t, app, "POST", "/save-late"))
}

// TestSessionChangedAfterTheResponseStarted covers the change no cookie can
// carry any more: it is not saved, and a warning says so.
func TestSessionChangedAfterTheResponseStarted(t *testing.T) {
	t.Parallel()
	app, _, logs := sessionTestApp(t, nil, func(app *App) {
		app.Post("/late", func(ctx *Context, _ Empty) (Empty, error) {
			_, _ = ctx.ResponseWriter().Write([]byte("x"))
			_ = ctx.Session().Set("v", "late")
			return Empty{}, nil
		})
	})
	rec := sessionRequest(t, app, "POST", "/late")
	assertNoSessionCookie(t, rec)
	if !strings.Contains(logs.String(), "the session changed after the response had started") {
		t.Errorf("no warning for a lost session change:\n%s", logs.String())
	}
}

// TestSessionChangedAfterItWentOutWithTheResponse covers a change lost the
// same way by a session that was read before the response started, and so
// was settled as it did: by a handler writing its own response, and by an
// event stream whose guard reads the session. The change is not saved, and is
// warned of once, while a change a failed response discarded on purpose is
// not warned of at all.
func TestSessionChangedAfterItWentOutWithTheResponse(t *testing.T) {
	t.Parallel()
	const warning = "the session changed after the response had started"
	app, _, logs := sessionTestApp(t, nil, func(app *App) {
		app.Post("/read-then-late", func(ctx *Context, _ Empty) (Empty, error) {
			s := ctx.Session()
			_, _ = ctx.ResponseWriter().Write([]byte("x"))
			return Empty{}, s.Set("v", "late")
		})
		app.Post("/failed-then-late", func(ctx *Context, _ Empty) (Empty, error) {
			s := ctx.Session()
			ctx.ResponseWriter().WriteHeader(http.StatusConflict)
			return Empty{}, s.Set("v", "late")
		})
		app.SSE("/events", func(ctx *Context, _ Empty, _ *SSEStream[string]) error {
			return ctx.Session().Set("v", "late")
		}, WithDependencies(func(ctx *Context) error {
			_ = ctx.Session()
			return nil
		}))
	})
	for _, c := range []struct {
		method, target string
		warnings       int
	}{
		{"POST", "/read-then-late", 1},
		{"POST", "/failed-then-late", 0},
		{"GET", "/events", 1},
	} {
		before := strings.Count(logs.String(), warning)
		rec := sessionRequest(t, app, c.method, c.target)
		assertNoSessionCookie(t, rec)
		if got := strings.Count(logs.String(), warning) - before; got != c.warnings {
			t.Errorf("%s %s logged %d warnings of a lost change, want %d:\n%s", c.method, c.target, got, c.warnings, logs.String())
		}
	}
}

// TestSessionWithoutConfigurationPanics covers the programming error of
// asking for a session an application never configured: a 500, with the
// reason in the log.
func TestSessionWithoutConfigurationPanics(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.Get("/s", func(ctx *Context, _ Empty) (sessionOut, error) { return readSession(ctx.Session()), nil })
	rec := do(t, mustBuild(t, app), "GET", "/s")
	assertStatus(t, rec, http.StatusInternalServerError)
	if !strings.Contains(logs.String(), "AppOptions.Sessions is not set") {
		t.Errorf("the log does not say why:\n%s", logs.String())
	}
	if got := catchPanic(func() { (&Context{}).Session() }); got != errSessionsOff { //nolint:errorlint // the panic value itself
		t.Errorf("a Context without an application panicked with %v", got)
	}
}

// TestSessionKeptPastItsRequest covers a handler that keeps its session: once
// the request is over, every change fails and nothing reaches the next
// request's response.
func TestSessionKeptPastItsRequest(t *testing.T) {
	t.Parallel()
	var kept *Session
	app, _, _ := sessionTestApp(t, nil, func(app *App) {
		app.Post("/keep", func(ctx *Context, _ Empty) (Empty, error) {
			kept = ctx.Session()
			return Empty{}, nil
		})
	})
	sessionRequest(t, app, "POST", "/keep")
	if err := kept.Set("v", "x"); !errors.Is(err, errSessionEnded) {
		t.Errorf("Set after the request = %v", err)
	}
	if err := kept.Save(); !errors.Is(err, errSessionEnded) {
		t.Errorf("Save after the request = %v", err)
	}
	if err := kept.Regenerate(); !errors.Is(err, errSessionEnded) {
		t.Errorf("Regenerate after the request = %v", err)
	}
	kept.Delete("v")
	kept.Clear()
	kept.Destroy()
	if kept.changed {
		t.Error("a session past its request recorded a change")
	}
	assertNoSessionCookie(t, sessionRequest(t, app, "GET", "/untouched"))
}

// TestSessionCookieKeepsResponsesOutOfSharedCaches covers a handler that
// marked its response public: a response carrying a session cookie is made
// private, since a shared cache that kept it would hand the session to the
// next user, while a policy that is already private is left alone.
func TestSessionCookieKeepsResponsesOutOfSharedCaches(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"public, max-age=60":  privateCacheControl,
		"max-age=600":         privateCacheControl,
		"s-maxage=600":        privateCacheControl,
		"private, max-age=60": "private, max-age=60",
		"no-store":            "no-store",
		"No-Store":            "No-Store",
		"public, no-store":    "public, no-store",
	}
	for policy, want := range cases {
		app, _, _ := sessionTestApp(t, nil, func(app *App) {
			app.Post("/cached", func(ctx *Context, _ Empty) (Empty, error) {
				ctx.SetHeader("Cache-Control", policy)
				_ = ctx.Session().Set("v", "x")
				return Empty{}, nil
			})
		})
		rec := sessionRequest(t, app, "POST", "/cached")
		mustSessionCookie(t, rec)
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("with %q, Cache-Control = %q, want %q", policy, got, want)
		}
	}
}

// TestSessionCookieStaysPrivateWhateverFollows covers a cookie put on the
// response before the last word on its caching was said: by Save, followed
// by a public Cache-Control the handler set or an error renderer chose, or by
// the file server, which drops Cache-Control from a range it cannot serve.
// The response still carries the session, so it must still be kept out of
// shared caches; a cache that kept it would hand the session to whoever
// asked next with no cookie of their own.
func TestSessionCookieStaysPrivateWhateverFollows(t *testing.T) {
	t.Parallel()
	saved := func(ctx *Context) error {
		s := ctx.Session()
		if err := s.Set("v", "x"); err != nil {
			return err
		}
		if err := s.Save(); err != nil {
			return err
		}
		ctx.SetHeader("Cache-Control", "public, max-age=600")
		return nil
	}
	app, _, _ := sessionTestApp(t, func(o *AppOptions) {
		o.ErrorRenderer = func(ctx *Context, err error) (int, any) {
			ctx.SetHeader("Cache-Control", "public, max-age=60")
			return DefaultErrorRenderer(ctx, err)
		}
	}, func(app *App) {
		app.Post("/saved", func(ctx *Context, _ Empty) (Empty, error) {
			return Empty{}, saved(ctx)
		})
		app.Post("/saved-stream", func(ctx *Context, _ Empty) (Empty, error) {
			if err := saved(ctx); err != nil {
				return Empty{}, err
			}
			_, err := ctx.ResponseWriter().Write([]byte("streamed"))
			return Empty{}, err
		})
		app.Post("/saved-error", func(ctx *Context, _ Empty) (Empty, error) {
			if err := saved(ctx); err != nil {
				return Empty{}, err
			}
			return Empty{}, BadRequest("refused after saving the session")
		})
		app.Post("/file", func(ctx *Context, _ Empty) (FileResponse, error) {
			if err := ctx.Session().Set("v", "x"); err != nil {
				return FileResponse{}, err
			}
			return FileResponse{FS: fstest.MapFS{"a.txt": {Data: []byte("abc")}}, Name: "a.txt"}, nil
		})
	})
	for target, status := range map[string]int{
		"/saved":        http.StatusOK,
		"/saved-stream": http.StatusOK,
		"/saved-error":  http.StatusBadRequest,
		"/file":         http.StatusRequestedRangeNotSatisfiable,
	} {
		req := httptest.NewRequest("POST", target, nil)
		req.Header.Set("Range", "bytes=100-")
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		assertStatus(t, rec, status)
		mustSessionCookie(t, rec)
		if got := rec.Header().Get("Cache-Control"); !keepsPrivate([]string{got}) {
			t.Errorf("%s answers %d carrying the session cookie with Cache-Control %q", target, status, got)
		}
	}
}

// TestSessionEventStreamKeepsNoTransform covers an event stream whose guard
// reads the session, as one that signs its subscribers in does. The stream
// keeps its own "no-cache, no-transform", as it does behind any guard, since
// a proxy free to transform it may hold its events back to compress them;
// renewed at the start of the stream, the session makes it private without
// taking no-transform away.
func TestSessionEventStreamKeepsNoTransform(t *testing.T) {
	t.Parallel()
	app, clock, _ := sessionTestApp(t, nil, func(app *App) {
		app.SSE("/events", func(*Context, Empty, *SSEStream[string]) error { return nil },
			WithDependencies(func(ctx *Context) error {
				if _, ok := SessionGet[string](ctx.Session(), "v"); !ok {
					return Unauthorized("sign in first")
				}
				return nil
			}))
	})
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=subscriber"))
	rec := sessionRequest(t, app, "GET", "/events", cookie)
	assertStatus(t, rec, http.StatusOK)
	assertNoSessionCookie(t, rec)
	if got := rec.Header().Get("Cache-Control"); got != "no-cache, no-transform" {
		t.Errorf("the event stream is sent with Cache-Control %q", got)
	}
	if !varyNames(rec.Header().Values("Vary"), "Cookie") {
		t.Errorf("the event stream does not vary on Cookie: %q", rec.Header().Values("Vary"))
	}
	clock.advance(DefaultSessionIdleTimeout / 2)
	rec = sessionRequest(t, app, "GET", "/events", cookie)
	assertStatus(t, rec, http.StatusOK)
	mustSessionCookie(t, rec)
	if got := rec.Header().Get("Cache-Control"); got != "private, no-cache, no-transform" {
		t.Errorf("the event stream that renewed the session is sent with Cache-Control %q", got)
	}
}

// TestSessionWriteFailureSkipsBackgroundTasks covers a session that cannot
// be saved after a handler succeeded: the request fails, as a release
// failing would make it, and the work registered to follow a success does
// not run.
func TestSessionWriteFailureSkipsBackgroundTasks(t *testing.T) {
	t.Parallel()
	store := newRecordingSessionStore()
	store.failWrite = errors.New("store down")
	var ran atomic.Int32
	app, _, logs := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Store: store}
	}, func(app *App) {
		app.Post("/signup", func(ctx *Context, _ Empty) (Empty, error) {
			if err := ctx.AfterResponse(func(context.Context) { ran.Add(1) }); err != nil {
				return Empty{}, err
			}
			return Empty{}, ctx.Session().Set("v", "x")
		})
	})
	rec := sessionRequest(t, app, "POST", "/signup")
	assertStatus(t, rec, http.StatusInternalServerError)
	assertNoSessionCookie(t, rec)
	waitPoolIdle(t, app)
	if n := ran.Load(); n != 0 {
		t.Errorf("%d background tasks ran for a request whose session could not be saved", n)
	}
	if !strings.Contains(logs.String(), "the session could not be saved") {
		t.Errorf("the failure is not logged:\n%s", logs.String())
	}
}
