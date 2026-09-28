package muzak

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type cacheSession struct{ User string }

func cacheSessionFromCookie(ctx *Context) (cacheSession, error) {
	c, err := ctx.Cookie("session")
	if err != nil || c.Value != "alice-session" {
		return cacheSession{}, Unauthorized("login required")
	}
	return cacheSession{User: "alice"}, nil
}

func allowEveryone(*Context) error { return nil }

// A guarded route answers per user, and used to do so with no Cache-Control,
// which leaves a shared cache free to store the answer heuristically and hand
// it to the next user. Any guard or provider, declared on the route or
// inherited from a router or the application, now marks the response private.
func TestGuardedRouteResponsesArePrivate(t *testing.T) {
	t.Parallel()
	account := NewRouter(Needs(cacheSessionFromCookie))
	account.Get("/me", func(ctx *Context, _ Empty) (cacheSession, error) {
		return From[cacheSession](ctx), nil
	})
	account.Get("/public", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.SetHeader("Cache-Control", "public, max-age=60")
		return Empty{}, nil
	})
	account.Get("/stream", func(ctx *Context, _ Empty) (Empty, error) {
		ctx.ResponseWriter().WriteHeader(http.StatusOK)
		_, _ = ctx.ResponseWriter().Write([]byte("row\n"))
		return Empty{}, nil
	})

	app := New(quietOptions())
	app.Include(account, WithPrefix("/account"))
	app.Get("/guarded", func(*Context, Empty) (Empty, error) { return Empty{}, nil }, WithDependencies(allowEveryone))
	app.Get("/singleton", func(*Context, Empty) (Empty, error) { return Empty{}, nil }, WithSingleton(42))
	app.Get("/open", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	for target, want := range map[string]string{
		"/account/me":     "private, no-cache",
		"/account/stream": "private, no-cache",
		"/account/public": "public, max-age=60",
		"/guarded":        "private, no-cache",
		// A singleton is the same for every client, so it alone does not
		// make a response per-client.
		"/singleton": "",
		"/open":      "",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "alice-session"})
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control = %q, want %q", target, got, want)
		}
	}

	// A guard that refuses still answers with an error that is never stored.
	rec := do(t, app, http.MethodGet, "/account/me")
	assertStatus(t, rec, http.StatusUnauthorized)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("refused: Cache-Control = %q, want no-store", got)
	}
}

// A policy a middleware already set is the application's decision and is
// kept.
func TestGuardedRouteKeepsMiddlewareCacheControl(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithDependencies(allowEveryone))
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	})
	app.Get("/x", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want the middleware's no-store", got)
	}
}

// An event stream keeps its own "no-cache, no-transform", which a proxy needs
// to see to leave the stream unbuffered.
func TestGuardedEventStreamKeepsItsCacheControl(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/items/stream", streamItems("Plumbus"))
	}, WithDependencies(allowEveryone))

	reader, response := tryStream(t, server.URL, "/items/stream")
	if reader == nil {
		t.Fatalf("the stream was refused with status %d", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache, no-transform" {
		t.Errorf("Cache-Control = %q, want the stream's own", got)
	}
}
