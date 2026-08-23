package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type diValue struct{ Text string }

type diOther struct{ Number int }

type diOut struct {
	Text   string `json:"text"`
	Number int    `json:"number,omitzero"`
}

func TestValueDependency(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, Needs(func(ctx *Context) (diValue, error) {
		return diValue{Text: "resolved"}, nil
	}))
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/x"), `{"text":"resolved"}`)
}

func TestValueDependencyError(t *testing.T) {
	t.Parallel()
	reached := false
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		reached = true
		return diOut{}, nil
	}, Needs(func(ctx *Context) (diValue, error) {
		return diValue{}, NewHTTPError(http.StatusForbidden, "no")
	}))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/x"), http.StatusForbidden)
	if reached {
		t.Error("the handler ran even though a dependency failed")
	}
}

func TestDependenciesResolveInDeclarationOrder(t *testing.T) {
	t.Parallel()
	var order []string
	var mu sync.Mutex
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

	child := NewRouter(WithDependencies(func(ctx *Context) error {
		record("router-guard")
		return nil
	}))
	child.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		record("handler")
		return diOut{Text: From[diValue](ctx).Text, Number: From[diOther](ctx).Number}, nil
	},
		WithDependencies(func(ctx *Context) error {
			record("route-guard")
			return nil
		}),
		Needs(func(ctx *Context) (diValue, error) {
			record("provider-one")
			return diValue{Text: "one"}, nil
		}),
		Needs(func(ctx *Context) (diOther, error) {
			// A later provider may read an earlier one, which is why order is
			// part of the contract.
			record("provider-two:" + From[diValue](ctx).Text)
			return diOther{Number: 2}, nil
		}),
	)

	app := New(quietOptions(), WithDependencies(func(ctx *Context) error {
		record("app-guard")
		return nil
	}))
	app.Include(child)
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/x"), `{"text":"one","number":2}`)

	want := []string{"app-guard", "router-guard", "route-guard", "provider-one", "provider-two:one", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestGuardShortCircuits(t *testing.T) {
	t.Parallel()
	secondRan := false
	app := New(quietOptions(),
		WithDependencies(func(ctx *Context) error {
			return NewHTTPError(http.StatusTeapot, "stop")
		}),
		WithDependencies(func(ctx *Context) error {
			secondRan = true
			return nil
		}),
	)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/x"), http.StatusTeapot)
	if secondRan {
		t.Error("a guard ran after an earlier one rejected the request")
	}
}

func TestRouteProviderOverridesRouterProvider(t *testing.T) {
	t.Parallel()
	var routerCalls, routeCalls atomic.Int32

	child := NewRouter(Needs(func(ctx *Context) (diValue, error) {
		routerCalls.Add(1)
		return diValue{Text: "router"}, nil
	}))
	child.Get("/overridden", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, Needs(func(ctx *Context) (diValue, error) {
		routeCalls.Add(1)
		return diValue{Text: "route"}, nil
	}))
	child.Get("/inherited", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	})

	app := New(quietOptions())
	app.Include(child)
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/overridden"), `{"text":"route"}`)
	assertJSON(t, do(t, app, "GET", "/inherited"), `{"text":"router"}`)

	if routerCalls.Load() != 1 {
		t.Errorf("the overridden router provider ran %d times, want 1", routerCalls.Load())
	}
	if routeCalls.Load() != 1 {
		t.Errorf("the route provider ran %d times, want 1", routeCalls.Load())
	}
}

func TestDedupeProviders(t *testing.T) {
	t.Parallel()
	a := &provider{typ: emptyType}
	b := &provider{typ: emptyType}
	c := &provider{typ: uuidType}

	tests := []struct {
		name string
		in   []*provider
		want []*provider
	}{
		{"nil", nil, nil},
		{"single", []*provider{a}, []*provider{a}},
		{"last of a type wins", []*provider{a, b}, []*provider{b}},
		{"order is preserved", []*provider{a, c, b}, []*provider{c, b}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dedupeProviders(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("dedupeProviders = %d entries, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("entry %d = %p, want %p", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestFromPanicsForUndeclaredTypes(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	assertStatus(t, rec, http.StatusInternalServerError)

	// The client learns nothing about the mistake.
	if body := rec.Body.String(); strings.Contains(body, "diValue") {
		t.Errorf("the response named the missing type: %s", body)
	}
	// The developer learns everything about it.
	if got := logs.String(); !strings.Contains(got, "diValue") || !strings.Contains(got, "muzak.Needs") {
		t.Errorf("the log does not explain the missing dependency:\n%s", got)
	}
}

func TestTryFrom(t *testing.T) {
	t.Parallel()
	type out struct {
		Present bool   `json:"present"`
		Text    string `json:"text"`
	}
	app := New(quietOptions())
	app.Get("/declared", func(ctx *Context, _ Empty) (out, error) {
		value, ok := TryFrom[diValue](ctx)
		return out{Present: ok, Text: value.Text}, nil
	}, Needs(func(ctx *Context) (diValue, error) { return diValue{Text: "here"}, nil }))
	app.Get("/undeclared", func(ctx *Context, _ Empty) (out, error) {
		value, ok := TryFrom[diValue](ctx)
		return out{Present: ok, Text: value.Text}, nil
	})
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/declared"), `{"present":true,"text":"here"}`)
	assertJSON(t, do(t, app, "GET", "/undeclared"), `{"present":false,"text":""}`)
}

func TestSingletonResolvesOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, Singleton(func(ctx *Context) (diValue, error) {
		calls.Add(1)
		return diValue{Text: "once"}, nil
	}))
	mustBuild(t, app)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			app.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the singleton provider ran %d times, want 1", got)
	}
}

func TestSingletonCachesItsError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	app := New(quietOptions())
	app.Get("/x", okHandler, Singleton(func(ctx *Context) (diValue, error) {
		calls.Add(1)
		return diValue{}, NewHTTPError(http.StatusServiceUnavailable, "not today")
	}))
	mustBuild(t, app)

	for range 3 {
		assertStatus(t, do(t, app, "GET", "/x"), http.StatusServiceUnavailable)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a failing singleton was retried %d times, want it cached after 1", got)
	}
}

func TestWithSingletonPublishesAValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithSingleton(diValue{Text: "prebuilt"}))
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	})
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/x"), `{"text":"prebuilt"}`)
}

// TestWithSingletonAtRouteLevel covers publishing a value for one route only.
func TestWithSingletonAtRouteLevel(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		return diOut{Text: From[diValue](ctx).Text}, nil
	}, WithSingleton(diValue{Text: "route-scoped"}))
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/x"), `{"text":"route-scoped"}`)
}

// TestDependencyValuesDoNotSurvivePooling checks that a Context returned to the
// pool cannot hand a previous request's dependency to the next one.
func TestDependencyValuesDoNotSurvivePooling(t *testing.T) {
	t.Parallel()
	type out struct {
		Present bool `json:"present"`
	}
	app := New(quietOptions())
	app.Get("/with", func(ctx *Context, _ Empty) (out, error) {
		_, ok := TryFrom[diValue](ctx)
		return out{Present: ok}, nil
	}, Needs(func(ctx *Context) (diValue, error) { return diValue{Text: "x"}, nil }))
	app.Get("/without", func(ctx *Context, _ Empty) (out, error) {
		_, ok := TryFrom[diValue](ctx)
		return out{Present: ok}, nil
	})
	mustBuild(t, app)

	// Run the dependency-carrying route enough times to fill the pool, then
	// check that a route declaring nothing still sees nothing.
	for range 20 {
		assertJSON(t, do(t, app, "GET", "/with"), `{"present":true}`)
	}
	for range 20 {
		assertJSON(t, do(t, app, "GET", "/without"), `{"present":false}`)
	}
}

func TestGuardCanSetResponseHeaders(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithDependencies(func(ctx *Context) error {
		ctx.SetHeader("X-Checked", "yes")
		return nil
	}))
	app.Get("/x", okHandler)
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	if got := rec.Header().Get("X-Checked"); got != "yes" {
		t.Errorf("X-Checked = %q, want %q", got, "yes")
	}
}
