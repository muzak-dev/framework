package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// overrideUser stands in for a fake caller.
func overrideUser(name string) func(*Context) (depUser, error) {
	return func(*Context) (depUser, error) { return depUser{Name: name}, nil }
}

// readsUserEveryWay answers with what From, TryFrom and a Dep each read, so
// that a test can see an override reach all three.
func readsUserEveryWay(ctx *Context, in depOnlyIn) (relOut, error) {
	tried, ok := TryFrom[depUser](ctx)
	if !ok {
		return relOut{}, errors.New("TryFrom found nothing")
	}
	return relOut{Value: From[depUser](ctx).Name + "," + tried.Name + "," + in.User.Get().Name}, nil
}

func TestOverrideReplacesARequestScopedProvider(t *testing.T) {
	t.Parallel()
	var realCalls atomic.Int64
	app := New(quietOptions(), Needs(func(*Context) (depUser, error) {
		realCalls.Add(1)
		return depUser{Name: "real"}, nil
	}))
	app.Get("/me", readsUserEveryWay, Needs(func(*Context) (depUser, error) {
		realCalls.Add(1)
		return depUser{Name: "real-inner"}, nil
	}))
	app.Override(overrideUser("fake"))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/me")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"value":"fake,fake,fake"}`)
	if realCalls.Load() != 0 {
		t.Errorf("the overridden provider ran %d times, want never", realCalls.Load())
	}
}

func TestOverrideKeepsASingletonsLifetime(t *testing.T) {
	t.Parallel()
	for _, option := range []SharedOption{
		Singleton(overrideUser("real")),
		WithSingleton(depUser{Name: "real"}),
	} {
		var calls atomic.Int64
		app := New(quietOptions(), option)
		app.Get("/me", readsUserEveryWay)
		app.Get("/again", readsUserEveryWay)
		app.Override(func(*Context) (depUser, error) {
			calls.Add(1)
			return depUser{Name: "fake"}, nil
		})
		mustBuild(t, app)

		for _, target := range []string{"/me", "/again", "/me"} {
			rec := do(t, app, http.MethodGet, target)
			assertJSON(t, rec, `{"value":"fake,fake,fake"}`)
			// A singleton does not make the route per-client, and neither
			// does the override standing in for it.
			if got := rec.Header().Get("Cache-Control"); got == privateCacheControl {
				t.Errorf("Cache-Control = %q, want the singleton's public default kept", got)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("the override of a singleton ran %d times, want once", calls.Load())
		}
	}
}

func TestOverrideKeepsARequestScopedRoutePrivate(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(overrideUser("real")))
	app.Get("/me", readsUserEveryWay)
	app.Override(overrideUser("fake"))
	mustBuild(t, app)
	if got := do(t, app, http.MethodGet, "/me").Header().Get("Cache-Control"); got != privateCacheControl {
		t.Errorf("Cache-Control = %q, want %q kept", got, privateCacheControl)
	}
}

func TestOverrideAppliesToMountsAndTheDocumentation(t *testing.T) {
	t.Parallel()
	refuse := Needs(func(*Context) (depUser, error) { return depUser{}, Unauthorized("no") })
	app := New(quietOptions(), refuse)
	app.Static("/assets", StaticOptions{FS: fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("x")}}})
	mustBuild(t, app)
	assertStatus(t, do(t, app, http.MethodGet, "/assets/app.js"), http.StatusUnauthorized)

	app = New(quietOptions(), refuse)
	app.Static("/assets", StaticOptions{FS: fstest.MapFS{"app.js": &fstest.MapFile{Data: []byte("x")}}})
	app.Override(overrideUser("fake"))
	mustBuild(t, app)
	assertStatus(t, do(t, app, http.MethodGet, "/assets/app.js"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, "/openapi.json"), http.StatusOK)
}

func TestOverrideLeavesGuardsInPlace(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithDependencies(func(ctx *Context) error {
		if ctx.Header("X-Token") != "ok" {
			return Unauthorized("no token")
		}
		return nil
	}), Needs(overrideUser("real")))
	app.Get("/me", readsUserEveryWay)
	app.Override(overrideUser("fake"))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/me"), http.StatusUnauthorized)
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("X-Token", "ok")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
}

// TestOverridesBelongToOneApplication shares one provider option between two
// applications, the way a package-level option is, and overrides it in one.
func TestOverridesBelongToOneApplication(t *testing.T) {
	t.Parallel()
	shared := Needs(overrideUser("real"))
	build := func(override bool) *App {
		app := New(quietOptions(), shared)
		app.Get("/me", readsUserEveryWay)
		if override {
			app.Override(overrideUser("fake"))
		}
		return mustBuild(t, app)
	}
	overridden, plain := build(true), build(false)
	assertJSON(t, do(t, overridden, http.MethodGet, "/me"), `{"value":"fake,fake,fake"}`)
	assertJSON(t, do(t, plain, http.MethodGet, "/me"), `{"value":"real,real,real"}`)
	// And again in the other order, in case building one changed the other.
	plainFirst := build(false)
	_ = build(true)
	assertJSON(t, do(t, plainFirst, http.MethodGet, "/me"), `{"value":"real,real,real"}`)
}

// TestAnOverrideCanRefuse lets a test fake the failure path too: the error an
// override returns is rendered as the provider's would be.
func TestAnOverrideCanRefuse(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(overrideUser("real")))
	app.Get("/me", readsUserEveryWay)
	app.Override(func(*Context) (depUser, error) { return depUser{}, Forbidden("suspended") })
	mustBuild(t, app)
	rec := do(t, app, http.MethodGet, "/me")
	assertStatus(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "suspended") {
		t.Errorf("body = %s, want the override's error", rec.Body.String())
	}
}

// TestEveryUnusedOverrideIsReportedInOrder checks that a build lists every
// override that replaced nothing, sorted by type so the message is stable.
func TestEveryUnusedOverrideIsReportedInOrder(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(overrideUser("real")))
	app.Get("/me", readsUserEveryWay)
	app.Override(func(*Context) (relC, error) { return "", nil })
	app.Override(func(*Context) (relA, error) { return "", nil })
	app.Override(overrideUser("used"))
	err := app.Build()
	if err == nil {
		t.Fatal("Build() = nil, want both unused overrides reported")
	}
	a, c := strings.Index(err.Error(), "provider of muzak.relA"), strings.Index(err.Error(), "provider of muzak.relC")
	if a < 0 || c < 0 || a > c {
		t.Errorf("Build() = %v, want relA then relC reported", err)
	}
	if strings.Contains(err.Error(), "muzak.depUser") {
		t.Errorf("Build() = %v, reports the override that was used", err)
	}
}

func TestTheLastOverrideOfATypeWins(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(overrideUser("real")))
	app.Get("/me", readsUserEveryWay)
	app.Override(overrideUser("first"))
	app.Override(overrideUser("second"))
	mustBuild(t, app)
	assertJSON(t, do(t, app, http.MethodGet, "/me"), `{"value":"second,second,second"}`)
}

func TestOverrideAcquireReleasesItsOwnValue(t *testing.T) {
	t.Parallel()
	realLog, fakeLog := newReleaseLog(), newReleaseLog()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (relOut, error) {
		return relOut{Value: string(From[relA](ctx))}, nil
	}, acquireAs[relA](realLog, "real", nil))
	app.Get("/y", func(ctx *Context, _ Empty) (relOut, error) {
		return relOut{Value: string(From[relA](ctx))}, nil
	}, Needs(func(*Context) (relA, error) { return "plain", nil }))
	app.OverrideAcquire(func(*Context) (relA, Release, error) {
		fakeLog.add("acquire fake")
		return "fake", func(failure error) error {
			fakeLog.released("fake", failure)
			return nil
		}, nil
	})
	mustBuild(t, app)

	assertJSON(t, do(t, app, http.MethodGet, "/x"), `{"value":"fake"}`)
	assertJSON(t, do(t, app, http.MethodGet, "/y"), `{"value":"fake"}`)
	assertEvents(t, realLog, "")
	assertEvents(t, fakeLog, "acquire fake, release fake, acquire fake, release fake")
}

// TestOverrideOfAnAcquireDropsItsRelease checks that the release of a value
// never acquired is never run.
func TestOverrideOfAnAcquireDropsItsRelease(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (relOut, error) {
		return relOut{Value: string(From[relA](ctx))}, nil
	}, acquireAs[relA](log, "real", nil))
	app.Override(func(*Context) (relA, error) { return "fake", nil })
	mustBuild(t, app)

	assertJSON(t, do(t, app, http.MethodGet, "/x"), `{"value":"fake"}`)
	assertEvents(t, log, "")
}

func TestOverrideMistakesAreBuildErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		override func(*App)
		want     []string
	}{
		{"a type nothing provides", func(a *App) { a.Override(func(*Context) (*depUser, error) { return nil, nil }) },
			[]string{"muzak: App.Override was given a provider of *muzak.depUser, but no route, mount or the documentation declares a provider of exactly that type", "a pointer"}},
		{"a nil provider", func(a *App) { a.Override[depUser](nil) },
			[]string{"muzak: App.Override was given a nil provider for muzak.depUser"}},
		{"a nil acquiring provider", func(a *App) { a.OverrideAcquire[depUser](nil) },
			[]string{"muzak: App.OverrideAcquire was given a nil provider for muzak.depUser"}},
		{"an acquiring override of a singleton", func(a *App) {
			a.OverrideAcquire(func(*Context) (depAccount, Release, error) { return depAccount{}, nil, nil })
		}, []string{"muzak: App.OverrideAcquire cannot replace the singleton provider of muzak.depAccount", "override it with App.Override,"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions(), Needs(overrideUser("real")), WithSingleton(depAccount{ID: 1}))
			app.Get("/me", readsUserEveryWay)
			app.Get("/again", readsUserEveryWay)
			tc.override(app)
			err := app.Build()
			if err == nil {
				t.Fatal("Build() = nil, want an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Build() = %v\nwant it to contain %q", err, want)
				}
				if n := strings.Count(err.Error(), want); n != 1 {
					t.Errorf("%q was reported %d times, want once", want, n)
				}
			}
		})
	}
}

// TestOverrideNeverAddsAProvider pins that a route which forgot to declare a
// dependency fails to build under test exactly as it does in production.
func TestOverrideNeverAddsAProvider(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/me", readsUserEveryWay)
	app.Override(overrideUser("fake"))
	err := app.Build()
	if err == nil || !strings.Contains(err.Error(), "field User is a muzak.Dep[muzak.depUser], but no provider") {
		t.Errorf("Build() = %v, want the missing provider reported", err)
	}
}

func TestOverrideAfterTheBuildPanics(t *testing.T) {
	t.Parallel()
	for name, override := range map[string]func(*App){
		"App.Override": func(a *App) { a.Override(overrideUser("late")) },
		"App.OverrideAcquire": func(a *App) {
			a.OverrideAcquire(func(*Context) (depUser, Release, error) { return depUser{}, nil, nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions(), Needs(overrideUser("real")))
			app.Get("/me", readsUserEveryWay)
			mustBuild(t, app)
			defer func() {
				message, _ := recover().(string)
				if !strings.HasPrefix(message, "muzak: "+name+" was called after the application was built") {
					t.Errorf("recovered %q, want a muzak: message naming %s", message, name)
				}
				// Still serving the real value: nothing changed.
				assertJSON(t, do(t, app, http.MethodGet, "/me"), `{"value":"real,real,real"}`)
			}()
			override(app)
			t.Error("overriding a built application did not panic")
		})
	}
}
