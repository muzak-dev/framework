package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// depUser is the caller a provider resolves in these tests.
type depUser struct{ Name string }

// depAccount is a second, unrelated dependency type.
type depAccount struct{ ID int }

// depRole is an interface-typed dependency, which a provider may resolve to nil.
type depRole interface{ Role() string }

// provideDepUser resolves the caller from a header, the way a real provider
// would resolve it from a token.
func provideDepUser(ctx *Context) (depUser, error) {
	name := ctx.Header("X-User")
	if name == "" {
		return depUser{}, Unauthorized("no user")
	}
	return depUser{Name: name}, nil
}

type depNameOut struct {
	User    string `json:"user"`
	Account int    `json:"account,omitzero"`
	Name    string `json:"name,omitzero"`
	ID      string `json:"id,omitzero"`
}

type depOnlyIn struct {
	User Dep[depUser]
}

func TestDepReceivesTheProvidersValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/me", func(_ *Context, in depOnlyIn) (depNameOut, error) {
		return depNameOut{User: in.User.Get().Name}, nil
	}, Needs(provideDepUser))
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("X-User", "alice")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"user":"alice"}`)

	// A provider that refuses still refuses: the handler never runs with an
	// empty Dep.
	assertStatus(t, do(t, app, http.MethodGet, "/me"), http.StatusUnauthorized)
}

// depMixedIn binds a path parameter, a query parameter and a body member next
// to two dependencies.
type depMixedIn struct {
	ID      string `path:"id"`
	Verbose bool   `query:"verbose"`
	User    Dep[depUser]
	Account Dep[depAccount]
	Name    string `json:"name"`
}

func TestDepSitsBesideBoundFields(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(provideDepUser))
	app.Post("/items/{id}", func(_ *Context, in depMixedIn) (depNameOut, error) {
		return depNameOut{User: in.User.Get().Name, Account: in.Account.Get().ID, Name: in.Name, ID: in.ID}, nil
	}, Needs(func(*Context) (depAccount, error) { return depAccount{ID: 7}, nil }))
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodPost, "/items/x1?verbose=true", strings.NewReader(`{"name":"plumbus"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", "alice")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"user":"alice","account":7,"name":"plumbus","id":"x1"}`)
}

// TestDepReceivesTheInnermostValue pins that a Dep reads what From reads when
// the type is declared more than once along the chain.
func TestDepReceivesTheInnermostValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(func(*Context) (depUser, error) { return depUser{Name: "outer"}, nil }))
	app.Get("/me", func(ctx *Context, in depOnlyIn) (depNameOut, error) {
		if from := From[depUser](ctx); from != in.User.Get() {
			t.Errorf("From = %+v, Dep = %+v, want the same value", from, in.User.Get())
		}
		return depNameOut{User: in.User.Get().Name}, nil
	}, Needs(func(*Context) (depUser, error) { return depUser{Name: "inner"}, nil }))
	mustBuild(t, app)

	assertJSON(t, do(t, app, http.MethodGet, "/me"), `{"user":"inner"}`)
}

func TestDepIsFilledFromEveryKindOfProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		option SharedOption
	}{
		{"needs", Needs(func(*Context) (depUser, error) { return depUser{Name: "needs"}, nil })},
		{"acquire", Acquire(func(*Context) (depUser, Release, error) {
			return depUser{Name: "acquire"}, func(error) error { return nil }, nil
		})},
		{"singleton", Singleton(func(*Context) (depUser, error) { return depUser{Name: "singleton"}, nil })},
		{"with-singleton", WithSingleton(depUser{Name: "with-singleton"})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// On the application, so the route's chain inherits it.
			app := New(quietOptions(), tc.option)
			app.Get("/me", func(_ *Context, in depOnlyIn) (depNameOut, error) {
				return depNameOut{User: in.User.Get().Name}, nil
			})
			mustBuild(t, app)
			for range 2 {
				assertJSON(t, do(t, app, http.MethodGet, "/me"), `{"user":"`+tc.name+`"}`)
			}
		})
	}
}

// depShared is a set of dependencies several inputs embed.
type depShared struct {
	User Dep[depUser]
	Note string `json:"note"`
}

// depShared2 is the same set embedded through an unexported type, whose
// promoted fields are still the input's.
type depHidden struct {
	Account Dep[depAccount]
}

type depEmbeddingIn struct {
	depShared
	depHidden
	ID string `path:"id"`
}

func TestDepInAStructEmbeddedByValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/items/{id}", func(_ *Context, in depEmbeddingIn) (depNameOut, error) {
		return depNameOut{User: in.User.Get().Name, Account: in.Account.Get().ID, Name: in.Note, ID: in.ID}, nil
	},
		Needs(func(*Context) (depUser, error) { return depUser{Name: "alice"}, nil }),
		Needs(func(*Context) (depAccount, error) { return depAccount{ID: 3}, nil }))
	mustBuild(t, app)

	rec := do(t, app, http.MethodPost, "/items/a", `{"note":"hi"}`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"user":"alice","account":3,"name":"hi","id":"a"}`)

	// The embedded struct is split into its members rather than decoded
	// whole, so the Dep beside the body member is no member itself.
	rec = do(t, app, http.MethodPost, "/items/a", `{"note":"hi","User":{}}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
}

type depRoleIn struct {
	Role Dep[depRole]
}

func TestDepCarriesANilInterfaceValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/role", func(_ *Context, in depRoleIn) (depNameOut, error) {
		if in.Role.Get() != nil {
			t.Errorf("Get() = %v, want the nil the provider returned", in.Role.Get())
		}
		return depNameOut{User: "anonymous"}, nil
	}, Needs(func(*Context) (depRole, error) { return nil, nil }))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/role"), http.StatusOK)
}

// depValidatedIn refuses a request whose body names someone other than the
// caller, which needs the caller during validation.
type depValidatedIn struct {
	User  Dep[depUser]
	Owner string `json:"owner"`
}

func (in *depValidatedIn) Validate(v *Validation) {
	if in.Owner != in.User.Get().Name {
		v.Reject(&in.Owner, "must be the caller")
	}
}

func TestDepIsReadableFromValidate(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/claims", func(_ *Context, in depValidatedIn) (depNameOut, error) {
		return depNameOut{User: in.Owner}, nil
	}, Needs(func(*Context) (depUser, error) { return depUser{Name: "alice"}, nil }))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodPost, "/claims", `{"owner":"alice"}`), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodPost, "/claims", `{"owner":"mallory"}`), http.StatusUnprocessableEntity)
}

// depBodyIn is an input whose only bound content is the body, which without
// the Dep would be decoded straight into it.
type depBodyIn struct {
	User Dep[depUser]
	Name string `json:"name"`
}

// TestDepCannotBeSetByTheRequest is what an attacker tries first: naming the
// field in the body, in the query string or in a header, in every spelling.
func TestDepCannotBeSetByTheRequest(t *testing.T) {
	t.Parallel()
	build := func(opts ...RouteOption) *App {
		app := New(quietOptions())
		app.Post("/x", func(_ *Context, in depBodyIn) (depNameOut, error) {
			return depNameOut{User: in.User.Get().Name, Name: in.Name}, nil
		}, append(opts, Needs(func(*Context) (depUser, error) { return depUser{Name: "server"}, nil }))...)
		return mustBuild(t, app)
	}
	strict, lenient := build(), build(AllowUnknownFields())

	bodies := []string{
		`{"name":"n","User":{"Name":"mallory"}}`,
		`{"name":"n","user":{"value":{"Name":"mallory"},"filled":true}}`,
		`{"name":"n","User":null}`,
		`{"name":"n","User":"mallory"}`,
	}
	for _, body := range bodies {
		if rec := do(t, strict, http.MethodPost, "/x?User=mallory", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("strict %s: status = %d, want 422 for a member naming the Dep", body, rec.Code)
		}
		req := httptest.NewRequest(http.MethodPost, "/x?User=mallory&user=mallory", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User", "mallory")
		rec := doRequest(t, lenient, req)
		assertStatus(t, rec, http.StatusOK)
		assertJSON(t, rec, `{"user":"server","name":"n"}`)
	}
}

// depSelfDecodingIn decodes its own body, so the binder decodes it into a
// scratch value and copies out the body members alone.
type depSelfDecodingIn struct {
	User Dep[depUser]
	Name string
}

func (in *depSelfDecodingIn) UnmarshalJSON(b []byte) error {
	// What a careless decoder might do: overwrite the whole value.
	*in = depSelfDecodingIn{Name: strings.Trim(string(b), `{}"`)}
	return nil
}

func TestDepSurvivesAnInputThatDecodesItself(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", func(_ *Context, in depSelfDecodingIn) (depNameOut, error) {
		return depNameOut{User: in.User.Get().Name, Name: in.Name}, nil
	}, Needs(func(*Context) (depUser, error) { return depUser{Name: "server"}, nil }))
	mustBuild(t, app)

	rec := do(t, app, http.MethodPost, "/x", `"n"`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"user":"server","name":"n"}`)
}

// depSelfDecodingEmbeddedIn decodes itself and holds its Dep in an embedded
// struct, which must be copied member by member rather than whole.
type depSelfDecodingEmbeddedIn struct {
	depShared
}

func (in *depSelfDecodingEmbeddedIn) UnmarshalJSON(b []byte) error {
	*in = depSelfDecodingEmbeddedIn{depShared{Note: strings.Trim(string(b), `{}"`)}}
	return nil
}

func TestDepSurvivesAnEmbeddingInputThatDecodesItself(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", func(_ *Context, in depSelfDecodingEmbeddedIn) (depNameOut, error) {
		return depNameOut{User: in.User.Get().Name, Name: in.Note}, nil
	}, Needs(func(*Context) (depUser, error) { return depUser{Name: "server"}, nil }))
	mustBuild(t, app)

	rec := do(t, app, http.MethodPost, "/x", `"n"`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"user":"server","name":"n"}`)
}

// depOnlyEmbeddedIn is all body apart from the Dep its embedded struct holds,
// which is the shape that would otherwise be decoded straight into the input.
type depOnlyEmbeddedIn struct {
	depShared
}

// TestDepCannotBeNulledThroughAnEmbeddedStruct is the attack on that shape: a
// body decoded into the whole input could set the embedded Dep to null after
// it was filled.
func TestDepCannotBeNulledThroughAnEmbeddedStruct(t *testing.T) {
	t.Parallel()
	for _, lenient := range []bool{false, true} {
		app := New(quietOptions())
		var opts []RouteOption
		if lenient {
			opts = append(opts, AllowUnknownFields())
		}
		app.Post("/x", func(_ *Context, in depOnlyEmbeddedIn) (depNameOut, error) {
			return depNameOut{User: in.User.Get().Name, Name: in.Note}, nil
		}, append(opts, Needs(func(*Context) (depUser, error) { return depUser{Name: "server"}, nil }))...)
		mustBuild(t, app)

		rec := do(t, app, http.MethodPost, "/x", `{"note":"n","User":null}`)
		if !lenient {
			assertStatus(t, rec, http.StatusUnprocessableEntity)
			continue
		}
		assertStatus(t, rec, http.StatusOK)
		assertJSON(t, rec, `{"user":"server","name":"n"}`)
	}
}

// TestDepGetOnAnInputBuiltByHand pins the zero value outside a request, which
// is also what the build-time dry run of a Validate method sees.
func TestDepGetOnAnInputBuiltByHand(t *testing.T) {
	t.Parallel()
	var in depOnlyIn
	if got := in.User.Get(); got != (depUser{}) {
		t.Errorf("Get() = %+v, want the zero value", got)
	}
}

// Inputs that place a Dep where it would never be filled, each a build error.
type (
	depPointerIn struct {
		User *Dep[depUser]
	}
	depSliceIn struct {
		Users []Dep[depUser]
	}
	depArrayIn struct {
		Users [2]Dep[depUser]
	}
	depMapIn struct {
		Users map[string]Dep[depUser]
	}
	depNestedIn struct {
		Inner struct {
			User Dep[depUser]
		}
	}
	depDeepIn struct {
		Inner *struct {
			Items []struct{ User Dep[depUser] }
		}
	}
	depEmbeddedPointerIn struct {
		*depShared
	}
	depUnexportedIn struct {
		user Dep[depUser]
	}
	depEmbeddedDepIn struct {
		Dep[depUser]
	}
	depTaggedIn struct {
		User Dep[depUser] `query:"user"`
	}
	depFileTaggedIn struct {
		User Dep[depUser] `file:"user"`
	}
	depWrapped struct {
		Dep[depUser]
	}
	depWrappedIn struct {
		User depWrapped
	}
	depPointerUserIn struct {
		User Dep[*depUser]
	}
	depTaggedEmbedIn struct {
		depShared `query:"shared"`
	}
)

// unexportedUse keeps the linter from reporting the field only a build error
// ever looks at.
var _ = depUnexportedIn{}.user

func TestDepPlacementIsCheckedWhenTheApplicationIsBuilt(t *testing.T) {
	t.Parallel()
	needsUser := Needs(func(*Context) (depUser, error) { return depUser{}, nil })
	tests := []struct {
		name     string
		register func(*App)
		want     []string
	}{
		{"behind a pointer", func(a *App) { a.Get("/x", depHandler[depPointerIn], needsUser) },
			[]string{"GET /x", "field User holds a muzak.Dep[muzak.depUser]", "behind a pointer"}},
		{"in a slice", func(a *App) { a.Get("/x", depHandler[depSliceIn], needsUser) },
			[]string{"GET /x", "field Users holds a muzak.Dep[muzak.depUser]"}},
		{"in an array", func(a *App) { a.Get("/x", depHandler[depArrayIn], needsUser) },
			[]string{"field Users holds"}},
		{"in a map", func(a *App) { a.Get("/x", depHandler[depMapIn], needsUser) },
			[]string{"field Users holds"}},
		{"in a nested struct", func(a *App) { a.Get("/x", depHandler[depNestedIn], needsUser) },
			[]string{"field Inner holds a muzak.Dep[muzak.depUser] at Inner.User"}},
		{"deep inside", func(a *App) { a.Get("/x", depHandler[depDeepIn], needsUser) },
			[]string{"at Inner.Items.User"}},
		{"in a struct embedded by pointer", func(a *App) { a.Get("/x", depHandler[depEmbeddedPointerIn], needsUser) },
			[]string{"field depShared holds a muzak.Dep[muzak.depUser] at depShared.User"}},
		{"unexported", func(a *App) { a.Get("/x", depHandler[depUnexportedIn], needsUser) },
			[]string{"field user is a muzak.Dep[muzak.depUser] but is unexported"}},
		{"embedded", func(a *App) { a.Get("/x", depHandler[depEmbeddedDepIn], needsUser) },
			[]string{"embeds a muzak.Dep[muzak.depUser]", "named field"}},
		{"with a location tag", func(a *App) { a.Get("/x", depHandler[depTaggedIn], needsUser) },
			[]string{"never read from the request", "declares a query parameter"}},
		{"with a file tag", func(a *App) { a.Post("/x", depHandler[depFileTaggedIn], needsUser) },
			[]string{"declares a file parameter"}},
		{"wrapped in a struct of the caller's", func(a *App) { a.Get("/x", depHandler[depWrappedIn], needsUser) },
			[]string{"field User holds a muzak.Dep[muzak.depUser] at User.Dep"}},
		{"in a located embedded struct", func(a *App) { a.Get("/x", depHandler[depTaggedEmbedIn], needsUser) },
			[]string{"depShared"}},
		{"with no provider", func(a *App) { a.Get("/me", depHandler[depOnlyIn]) },
			[]string{"GET /me: field User is a muzak.Dep[muzak.depUser]", "no provider of exactly muzak.depUser", "muzak.Needs"}},
		{"with only a pointer provider", func(a *App) {
			a.Get("/me", depHandler[depOnlyIn], Needs(func(*Context) (*depUser, error) { return nil, nil }))
		}, []string{"a provider of *muzak.depUser is declared, which a muzak.Dep[*muzak.depUser] would receive"}},
		{"with only a value provider", func(a *App) { a.Get("/me", depHandler[depPointerUserIn], needsUser) },
			[]string{"a provider of muzak.depUser is declared, which a muzak.Dep[muzak.depUser] would receive"}},
		{"guarded but not provided", func(a *App) {
			a.Get("/me", depHandler[depOnlyIn], WithDependencies(func(*Context) error { return nil }))
		}, []string{"no provider of exactly muzak.depUser"}},
		{"on two routes and two fields", func(a *App) {
			a.Get("/a", depHandler[depOnlyIn])
			a.Post("/b/{id}", depHandler[depMixedIn])
		}, []string{"GET /a: field User", "POST /b/{id}: field User", "POST /b/{id}: field Account is a muzak.Dep[muzak.depAccount]"}},
		{"on an event stream", func(a *App) {
			a.SSE("/s", func(*Context, depOnlyIn, *SSEStream[depNameOut]) error { return nil })
		}, []string{"GET /s: field User"}},
		{"on a websocket", func(a *App) {
			a.WS("/w", func(*Context, depOnlyIn, *WSConn) error { return nil })
		}, []string{"GET /w: field User"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			tc.register(app)
			err := app.Build()
			if err == nil {
				t.Fatal("Build() = nil, want an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Build() = %v\nwant it to contain %q", err, want)
				}
			}
			for line := range strings.SplitSeq(err.Error(), "\n") {
				if !strings.HasPrefix(line, "muzak: ") {
					t.Errorf("error line %q does not start with %q", line, "muzak: ")
				}
			}
		})
	}
}

// TestDepBuildErrorsJoinOthers checks that a missing provider is reported with
// the application's other mistakes rather than ahead of them.
func TestDepBuildErrorsJoinOthers(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/me", depHandler[depOnlyIn])
	app.Get("no-slash", depHandler[Empty])
	err := app.Build()
	if err == nil || !strings.Contains(err.Error(), "field User") || !strings.Contains(err.Error(), "must begin with") {
		t.Fatalf("Build() = %v, want both mistakes reported", err)
	}
	if !errors.Is(app.Build(), err) {
		t.Error("a second Build returned a different error")
	}
}

// depHandler is a handler for any input, for the tests that only build.
func depHandler[In any](*Context, In) (Empty, error) { return Empty{}, nil }

func TestDepOnEventStreamsAndWebSockets(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/s", func(_ *Context, in depOnlyIn, stream *SSEStream[depNameOut]) error {
			return stream.Send(depNameOut{User: in.User.Get().Name})
		})
		app.WS("/w", func(ctx *Context, in depOnlyIn, conn *WSConn) error {
			return conn.WriteText(ctx.Context(), in.User.Get().Name)
		})
	}, Needs(func(*Context) (depUser, error) { return depUser{Name: "alice"}, nil }))

	reader := openStream(t, server.URL, "/s")
	if got := nextEvent(t, reader).Data; got != `{"user":"alice"}` {
		t.Errorf("event = %s, want the Dep's value", got)
	}
	conn := dialWS(t, server.URL, "/w")
	conn.expectText("alice")
	conn.expectClose(1000)
}

// TestDepCostsNoMoreThanFrom measures a request whose handler reads its
// dependency from a Dep against the same request reading it with From. The
// field is filled in place, so it must not allocate.
func TestDepCostsNoMoreThanFrom(t *testing.T) {
	skipAllocationCountsUnderRace(t)
	type viaFrom struct {
		ID string `query:"id"`
	}
	type viaDep struct {
		ID   string `query:"id"`
		User Dep[depUser]
	}
	app := New(quietOptions(), Needs(func(*Context) (depUser, error) { return depUser{Name: "alice"}, nil }))
	app.Get("/from", func(ctx *Context, in viaFrom) (Empty, error) {
		_ = From[depUser](ctx)
		return Empty{}, nil
	}, Status(http.StatusNoContent))
	app.Get("/dep", func(_ *Context, in viaDep) (Empty, error) {
		_ = in.User.Get()
		return Empty{}, nil
	}, Status(http.StatusNoContent))
	mustBuild(t, app)

	measure := func(target string) float64 {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		return testing.AllocsPerRun(500, func() {
			rec.Body.Reset()
			app.ServeHTTP(rec, req)
		})
	}
	from, dep := measure("/from?id=1"), measure("/dep?id=1")
	t.Logf("allocations per request: From %.2f, Dep %.2f", from, dep)
	if dep > from+0.5 {
		t.Errorf("a Dep request made %.2f allocations against %.2f for From, want no more", dep, from)
	}
}

// TestFeatureOffAllocatesNothing pins that the hooks every request passes
// through cost nothing when no route uses Acquire or Dep.
func TestFeatureOffAllocatesNothing(t *testing.T) {
	c := &Context{}
	failure := errors.New("failed")
	plan := &bindPlan{}
	if allocs := testing.AllocsPerRun(1000, func() {
		_ = c.settle(nil)
		_ = c.settle(failure)
		_ = plan.deps != nil
	}); allocs != 0 {
		t.Errorf("settling a request with nothing acquired made %.1f allocations, want 0", allocs)
	}
}

// TestFillDepsFailsClosedWithoutAValue drives the invariant the build check
// guarantees from the other side: were a Dep ever reached with no resolved
// value, the request fails rather than running with an empty one.
func TestFillDepsFailsClosedWithoutAValue(t *testing.T) {
	t.Parallel()
	plan, err := newBindPlan(reflect.TypeFor[depOnlyIn](), http.MethodGet, "/me")
	if err != nil {
		t.Fatal(err)
	}
	var in depOnlyIn
	err = plan.fillDeps(&Context{}, reflect.ValueOf(&in).Elem())
	if err == nil || !strings.Contains(err.Error(), "muzak: no value of type muzak.depUser was resolved for field User") {
		t.Errorf("fillDeps = %v, want the missing value reported", err)
	}
}

// Recursive types the Dep search must walk without looping: one through a
// map alone and one through a struct.
type (
	depTree map[string]depTree
	depNode struct {
		Value string   `json:"value"`
		Next  *depNode `json:"next"`
		Kids  []depNode
	}
	depRecursiveIn struct {
		Tree depTree `json:"tree"`
		Node depNode `json:"node"`
		User Dep[depUser]
	}
)

func TestDepSearchEndsOnRecursiveTypes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", func(_ *Context, in depRecursiveIn) (depNameOut, error) {
		return depNameOut{User: in.User.Get().Name, Name: in.Node.Next.Value}, nil
	}, Needs(func(*Context) (depUser, error) { return depUser{Name: "alice"}, nil }))
	mustBuild(t, app)
	rec := do(t, app, http.MethodPost, "/x", `{"tree":{"a":{}},"node":{"value":"1","next":{"value":"2"}}}`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"user":"alice","name":"2"}`)
}
