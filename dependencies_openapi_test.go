package muzak

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestDocumentLeavesDepsOut reads the served document the way a client
// generator does and checks that no Dep is described anywhere: not as a
// parameter, not as a body member, and not as a schema of its own.
func TestDocumentLeavesDepsOut(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(),
		Needs(func(*Context) (depUser, error) { return depUser{Name: "alice"}, nil }),
		Needs(func(*Context) (depAccount, error) { return depAccount{ID: 1}, nil }))
	app.Post("/items/{id}", func(*Context, depMixedIn) (relOut, error) { return relOut{}, nil })
	app.Post("/claims", func(*Context, depBodyIn) (relOut, error) { return relOut{}, nil })
	app.Post("/embedded/{id}", func(*Context, depEmbeddingIn) (relOut, error) { return relOut{}, nil })
	app.Get("/me", func(*Context, depOnlyIn) (relOut, error) { return relOut{}, nil })
	app.SSE("/stream", func(*Context, depOnlyIn, *SSEStream[relEvent]) error { return nil })
	app.WS("/socket", func(*Context, depOnlyIn, *WSConn) error { return nil })
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/openapi.json")
	assertStatus(t, rec, http.StatusOK)
	wire := rec.Body.String()
	for _, leaked := range []string{`"User"`, `"Account"`, `Dep[`, `Dep_`, `depUser`, `depAccount`, `filled`} {
		if strings.Contains(wire, leaked) {
			t.Errorf("the document mentions %s:\n%s", leaked, wire)
		}
	}

	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
			RequestBody *struct {
				Content map[string]struct {
					Schema struct {
						Ref        string         `json:"$ref"`
						Properties map[string]any `json:"properties"`
						Required   []string       `json:"required"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	body := func(path string) []string {
		t.Helper()
		op := doc.Paths[path]["post"]
		if op.RequestBody == nil {
			t.Fatalf("%s has no request body", path)
		}
		schema := op.RequestBody.Content["application/json"].Schema
		if schema.Ref != "" {
			t.Fatalf("%s refers to %s, the input type itself, which holds the Dep", path, schema.Ref)
		}
		var names []string
		for name := range schema.Properties {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	if got := body("/items/{id}"); !slices.Equal(got, []string{"name"}) {
		t.Errorf("/items/{id} body members = %v, want [name]", got)
	}
	if got := body("/claims"); !slices.Equal(got, []string{"name"}) {
		t.Errorf("/claims body members = %v, want [name]", got)
	}
	if got := body("/embedded/{id}"); !slices.Equal(got, []string{"note"}) {
		t.Errorf("/embedded/{id} body members = %v, want [note]", got)
	}
	var params []string
	for _, p := range doc.Paths["/items/{id}"]["post"].Parameters {
		params = append(params, p.In+":"+p.Name)
	}
	slices.Sort(params)
	if !slices.Equal(params, []string{"path:id", "query:verbose"}) {
		t.Errorf("parameters = %v, want only the bound ones", params)
	}
	for _, path := range []string{"/me", "/stream", "/socket"} {
		op := doc.Paths[path]["get"]
		if op.RequestBody != nil || len(op.Parameters) != 0 {
			t.Errorf("%s describes input, but its only field is a Dep: %+v", path, op)
		}
	}
}

// FuzzDepNeverBoundFromRequest throws arbitrary bodies, query strings and
// header values at routes holding a Dep beside every kind of bound field, and
// checks the invariant an attacker would try to break: whatever the request
// says, a handler that runs reads exactly the value its provider resolved.
func FuzzDepNeverBoundFromRequest(f *testing.F) {
	for _, seed := range []struct{ body, query, header string }{
		{`{"name":"n"}`, "verbose=true", "alice"},
		{`{"name":"n","User":{"Name":"mallory"}}`, "User=mallory", "mallory"},
		{`{"name":"n","user":{"value":{"Name":"mallory"}},"Account":{"ID":9}}`, "Account=9&account=9", ""},
		{`{"note":"n","User":null,"depShared":{"User":{}}}`, "id=x", "x"},
		{`{"name":"n","User":"\u0000"}`, "%00=%00", "\x00"},
		{`[]`, "", ""},
		{`{"name":"n","name":"m"}`, "verbose=maybe", "alice"},
	} {
		f.Add(seed.body, seed.query, seed.header)
	}

	const resolved = "the provider"
	build := func(opts ...RouteOption) *App {
		app := New(quietOptions(),
			Needs(func(*Context) (depUser, error) { return depUser{Name: resolved}, nil }),
			Needs(func(*Context) (depAccount, error) { return depAccount{ID: 42}, nil }))
		check := func(user depUser, account depAccount) error {
			if user.Name != resolved || account.ID != 42 {
				panic("a Dep held a value the provider did not resolve: " + user.Name)
			}
			return nil
		}
		app.Post("/mixed/{id}", func(_ *Context, in depMixedIn) (relOut, error) {
			return relOut{}, check(in.User.Get(), in.Account.Get())
		}, opts...)
		app.Post("/body", func(_ *Context, in depBodyIn) (relOut, error) {
			return relOut{}, check(in.User.Get(), depAccount{ID: 42})
		}, opts...)
		app.Post("/embedded/{id}", func(_ *Context, in depEmbeddingIn) (relOut, error) {
			return relOut{}, check(in.User.Get(), in.Account.Get())
		}, opts...)
		app.Get("/only", func(_ *Context, in depOnlyIn) (relOut, error) {
			return relOut{}, check(in.User.Get(), depAccount{ID: 42})
		}, opts...)
		if err := app.Build(); err != nil {
			f.Fatal(err)
		}
		return app
	}
	apps := []*App{build(), build(AllowUnknownFields())}

	f.Fuzz(func(t *testing.T, body, query, header string) {
		for _, app := range apps {
			for _, target := range []string{"/mixed/a", "/body", "/embedded/b", "/only"} {
				method := http.MethodPost
				if target == "/only" {
					method = http.MethodGet
				}
				req := httptest.NewRequest(method, target, strings.NewReader(body))
				req.URL.RawQuery = query
				req.Header.Set("Content-Type", "application/json")
				for _, name := range []string{"User", "Account", "X-User"} {
					req.Header[name] = []string{header}
				}
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, req)
				if rec.Code >= http.StatusInternalServerError {
					t.Fatalf("%s %s?%q with %q: status %d, body %s", method, target, query, body, rec.Code, rec.Body.String())
				}
			}
		}
	})
}
