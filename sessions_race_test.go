package muzak

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestSessionPooledContextIsolation runs many clients at once, each with a
// session of its own, through pooled Contexts under the race detector. Every
// client must only ever read what it wrote last, whichever Context served it
// before, and requests without a session or with a forged one run among them.
func TestSessionPooledContextIsolation(t *testing.T) {
	t.Parallel()
	for _, store := range []SessionStore{nil, NewMemorySessionStore(MemorySessionOptions{})} {
		app, _, _ := sessionTestApp(t, func(o *AppOptions) {
			o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, Store: store}
		}, nil)
		var wg sync.WaitGroup
		for client := range 32 {
			wg.Go(func() {
				var cookie *http.Cookie
				for i := range 25 {
					want := strconv.Itoa(client) + "-" + strconv.Itoa(i)
					rec := sessionRequest(t, app, "POST", "/write?value="+want, cookie)
					if next := sessionCookieOf(t, rec, "__Host-session"); next != nil {
						cookie = next
					}
					out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie))
					if out.Value != want {
						t.Errorf("client %d read %q, want %q", client, out.Value, want)
						return
					}
					// Anonymous and forged requests share the pool.
					if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read")); out.Found {
						t.Errorf("a request without a cookie read %+v", out)
						return
					}
					forged := &http.Cookie{Name: "__Host-session", Value: strings.Repeat("A", 64)}
					if out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", forged)); out.Found {
						t.Errorf("a forged cookie read %+v", out)
						return
					}
				}
			})
		}
		wg.Wait()
	}
}

// TestSessionConcurrentWritersToOneSession sends many writes to one
// server-side session at once. The last writer wins, as documented, but the
// entry is always one writer's whole record: it decodes, and holds a value
// one of them wrote.
func TestSessionConcurrentWritersToOneSession(t *testing.T) {
	t.Parallel()
	app, store, _, _ := serverSessionApp(t, nil)
	cookie := mustSessionCookie(t, sessionRequest(t, app, "POST", "/write?value=start"))
	written := map[string]bool{"start": true}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for writer := range 16 {
		wg.Go(func() {
			for i := range 20 {
				value := "w" + strconv.Itoa(writer) + "-" + strconv.Itoa(i) + strings.Repeat("x", writer*50)
				mu.Lock()
				written[value] = true
				mu.Unlock()
				rec := sessionRequest(t, app, "POST", "/write?value="+value, cookie)
				if rec.Code != http.StatusOK {
					t.Errorf("a concurrent write answered %d", rec.Code)
					return
				}
				decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie))
			}
		})
	}
	wg.Wait()
	out := decodeSessionOut(t, sessionRequest(t, app, "GET", "/read", cookie))
	if !out.Found || !written[out.Value] {
		t.Fatalf("after concurrent writes the session reads %+v", out)
	}
	if !storeHolds(store, sessionKeyOf(t, cookie)) || store.Len() != 1 {
		t.Fatalf("concurrent writes to one session left %d entries", store.Len())
	}
}

// TestSessionsLeaveNoGoroutines drives sessions and cross-origin refusals
// through a real server and asks the runtime whether anything was left
// stuck.
func TestSessionsLeaveNoGoroutines(t *testing.T) {
	for _, store := range []SessionStore{nil, NewMemorySessionStore(MemorySessionOptions{})} {
		app, _, _ := sessionTestApp(t, func(o *AppOptions) {
			o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, Store: store, AllowInsecure: true}
		}, nil)
		server := httptest.NewServer(app)
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}
		for i := range 20 {
			for _, step := range []struct{ method, path string }{
				{"POST", "/write?value=" + strconv.Itoa(i)}, {"GET", "/read"}, {"POST", "/regenerate"},
				{"POST", "/fail?value=x"}, {"POST", "/destroy"}, {"GET", "/untouched"},
			} {
				req, _ := http.NewRequest(step.method, server.URL+step.path, nil)
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			req, _ := http.NewRequest("POST", server.URL+"/write?value=x", nil)
			req.Header.Set("Origin", "https://evil.test")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("a forged request through a real server answered %d", res.StatusCode)
			}
		}
		server.Close()
		client.CloseIdleConnections()
	}
	assertNoGoroutineLeaks(t)
}

// TestSessionOverTheWire signs in, reads and signs out through a real
// server and a cookie jar, which is what a browser does: the cookie the jar
// keeps is the one the next request sends, and the jar drops it at sign-out.
func TestSessionOverTheWire(t *testing.T) {
	t.Parallel()
	app, _, _ := sessionTestApp(t, func(o *AppOptions) {
		o.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}, AllowInsecure: true}
	}, nil)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	call := func(method, path string) string {
		req, _ := http.NewRequest(method, server.URL+path, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return string(body)
	}
	call("POST", "/write?value=signed-in")
	if got := call("GET", "/read"); !strings.Contains(got, `"value":"signed-in"`) {
		t.Fatalf("the jar's next request read %s", got)
	}
	base, _ := url.Parse(server.URL)
	if len(jar.Cookies(base)) != 1 {
		t.Fatalf("the jar holds %d cookies", len(jar.Cookies(base)))
	}
	call("POST", "/destroy")
	if len(jar.Cookies(base)) != 0 {
		t.Fatal("the jar kept the cookie after sign-out")
	}
	if got := call("GET", "/read"); !strings.Contains(got, `"found":false`) {
		t.Fatalf("after sign-out the session reads %s", got)
	}
}

// TestSessionsCostNothingUntouched measures allocations: configuring
// sessions and cross-origin protection adds nothing to a request that never
// asks for its session, with a session cookie on it or not, and the check
// adds nothing to a request it lets through.
func TestSessionsCostNothingUntouched(t *testing.T) {
	plain := quietOptions()
	plain.DisableAccessLog = true
	configured := plain
	configured.Sessions = &SessionOptions{Secrets: []string{testSessionSecret}}
	configured.CrossOriginProtection = &CrossOriginOptions{TrustedOrigins: []string{"https://app.example.com"}}
	build := func(options AppOptions) *App {
		app := New(options)
		app.Get("/untouched", func(*Context, Empty) (sessionOut, error) { return sessionOut{}, nil })
		app.Post("/untouched", func(*Context, Empty) (sessionOut, error) { return sessionOut{}, nil })
		return mustBuild(t, app)
	}
	without, with := build(plain), build(configured)
	measure := func(app *App, method string) float64 {
		return exactAllocs(200, func() {
			req := httptest.NewRequest(method, "/untouched", nil)
			req.Header.Set("Cookie", "__Host-session=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Origin", "https://example.com")
			app.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
	for _, method := range []string{"GET", "POST"} {
		if base, got := measure(without, method), measure(with, method); got > base {
			t.Errorf("%s: configuring sessions and cross-origin protection costs %.0f allocations, up from %.0f", method, got, base)
		}
	}
}
