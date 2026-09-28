package muzak

import (
	"net/http"
	"testing"
	"testing/fstest"
)

// TestAnswersPerClient pins which dependencies make a response per-client for
// caching: a guard or a request-scoped provider does, and a singleton, which
// every request shares, does not.
func TestAnswersPerClient(t *testing.T) {
	t.Parallel()

	collect := func(opts ...SharedOption) []*provider {
		cfg := routeConfig{}
		for _, opt := range opts {
			opt.applyRoute(&cfg)
		}
		return cfg.providers
	}
	guard := func(*Context) error { return nil }
	cases := []struct {
		name      string
		guards    []Guard
		providers []*provider
		want      bool
	}{
		{"nothing", nil, nil, false},
		{"a guard", []Guard{guard}, nil, true},
		{"a request-scoped provider", nil, collect(Needs(func(*Context) (int, error) { return 1, nil })), true},
		{"a lazy singleton", nil, collect(Singleton(func(*Context) (int, error) { return 1, nil })), false},
		{"a value singleton", nil, collect(WithSingleton(1)), false},
		{"a singleton beside a provider", nil, collect(
			WithSingleton(1),
			Needs(func(*Context) (string, error) { return "", nil }),
		), true},
	}
	for _, tc := range cases {
		if got := answersPerClient(tc.guards, tc.providers); got != tc.want {
			t.Errorf("%s: answersPerClient = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMountWithOnlyASingletonStaysCacheable pins that a file mount under a
// router that shares a singleton, and nothing else, is not marked private:
// its files are the same for everyone.
func TestMountWithOnlyASingletonStaysCacheable(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("x")}}
	shared := NewRouter(WithSingleton(42))
	shared.Static("/shared", StaticOptions{FS: files})
	scoped := NewRouter(Needs(func(*Context) (int, error) { return 1, nil }))
	scoped.Static("/scoped", StaticOptions{FS: files})

	app := New(quietOptions())
	app.Include(shared)
	app.Include(scoped)
	mustBuild(t, app)

	for target, want := range map[string]string{
		"/shared/app.js": "",
		"/scoped/app.js": privateCacheControl,
	} {
		rec := do(t, app, http.MethodGet, target)
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control = %q, want %q", target, got, want)
		}
	}
}
