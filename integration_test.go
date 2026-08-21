package badele

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The types below reproduce the application sketched in the specification, so
// that the integration tests exercise the exact call shapes the public API
// promises.

type intgUserOut struct {
	Username string `json:"username"`
}

type intgLookupParams struct {
	Username string `path:"username" doc:"The username to look up"`
}

type intgListQuery struct {
	Limit  int    `query:"limit" doc:"Max results" default:"20"`
	Cursor string `query:"cursor"`
}

type intgListOut struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

type intgItemOut struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner,omitzero"`
}

type intgCreateBody struct {
	Name  string `json:"name" doc:"New item name"`
	Async bool   `json:"async,omitzero"`
}

type intgAdminIn struct {
	Name string `json:"name"`
}

type intgAdminOut struct {
	Name string `json:"name"`
}

type intgCurrentUser struct {
	Username string
}

// intgQueryToken is the application-wide guard from the specification.
func intgQueryToken(ctx *Context) error {
	if ctx.Query("token") == "" {
		return NewHTTPError(400, "token is required")
	}
	return nil
}

// intgTokenHeader guards the admin subtree.
func intgTokenHeader(ctx *Context) error {
	if ctx.Header("X-Token") != "fake-super-secret-token" {
		return NewHTTPError(400, "X-Token header invalid")
	}
	return nil
}

// intgCurrentUserProvider is the value dependency from the specification.
func intgCurrentUserProvider(ctx *Context) (intgCurrentUser, error) {
	if ctx.Header("Authorization") == "" {
		return intgCurrentUser{}, NewHTTPError(401, "unauthorized")
	}
	return intgCurrentUser{Username: "fakecurrentuser"}, nil
}

func newUsersRouter() *Router {
	r := NewRouter(WithTags("users"))
	r.Get("/users/", func(ctx *Context, in intgListQuery) (intgListOut, error) {
		return intgListOut{Limit: in.Limit, Cursor: in.Cursor}, nil
	})
	r.Get("/users/me", func(ctx *Context, _ Empty) (intgUserOut, error) {
		return intgUserOut{Username: "fakecurrentuser"}, nil
	})
	r.Get("/users/{username}", func(ctx *Context, in intgLookupParams) (intgUserOut, error) {
		return intgUserOut{Username: in.Username}, nil
	})
	return r
}

func newItemsRouter() *Router {
	r := NewRouter(WithTags("items"))
	r.Get("/items/{id}", func(ctx *Context, in struct {
		ID string `path:"id"`
	}) (intgItemOut, error) {
		user := From[intgCurrentUser](ctx)
		return intgItemOut{ID: in.ID, Name: "Plumbus", Owner: user.Username}, nil
	}, Needs(intgCurrentUserProvider))

	r.Post("/items/", func(ctx *Context, in intgCreateBody) (intgItemOut, error) {
		if in.Async {
			ctx.SetStatus(202)
		}
		return intgItemOut{ID: "42", Name: in.Name}, nil
	}, Status(201))
	return r
}

func newAdminRouter() *Router {
	r := NewRouter()
	r.Post("/", func(ctx *Context, in intgAdminIn) (intgAdminOut, error) {
		return intgAdminOut{Name: in.Name}, nil
	}, Status(201), Summary("Admin action"))
	return r
}

// newIntegrationApp composes the specification's example application.
func newIntegrationApp() *App {
	app := New(quietOptions(), WithDependencies(intgQueryToken))
	app.Include(newUsersRouter())
	app.Include(newItemsRouter())
	app.Include(newAdminRouter(),
		WithPrefix("/admin"),
		WithTags("admin"),
		WithDependencies(intgTokenHeader),
		WithResponseDoc(418, "I'm a teapot"),
	)
	return app
}

func TestIntegrationRouting(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	tests := []struct {
		name    string
		method  string
		target  string
		body    string
		headers map[string]string
		status  int
		want    string
	}{
		{
			name: "static route wins over the parameter", method: "GET",
			target: "/users/me?token=jessica", status: 200,
			want: `{"username":"fakecurrentuser"}`,
		},
		{
			name: "path parameter", method: "GET",
			target: "/users/rick?token=jessica", status: 200,
			want: `{"username":"rick"}`,
		},
		{
			name: "query default applies when absent", method: "GET",
			target: "/users/?token=jessica", status: 200,
			want: `{"limit":20,"cursor":""}`,
		},
		{
			name: "query value overrides the default", method: "GET",
			target: "/users/?token=jessica&limit=5&cursor=abc", status: 200,
			want: `{"limit":5,"cursor":"abc"}`,
		},
		{
			name: "trailing slash is a distinct route", method: "GET",
			target: "/users?token=jessica", status: 404,
		},
		{
			name: "application guard rejects", method: "GET",
			target: "/users/me", status: 400,
		},
		{
			name: "value dependency reaches the handler", method: "GET",
			target: "/items/plumbus?token=jessica", status: 200,
			headers: map[string]string{"Authorization": "Bearer x"},
			want:    `{"id":"plumbus","name":"Plumbus","owner":"fakecurrentuser"}`,
		},
		{
			name: "value dependency rejects", method: "GET",
			target: "/items/plumbus?token=jessica", status: 401,
		},
		{
			name: "declared status", method: "POST",
			target: "/items/?token=jessica", body: `{"name":"Portal Gun"}`, status: 201,
			want: `{"id":"42","name":"Portal Gun"}`,
		},
		{
			name: "handler overrides the declared status", method: "POST",
			target: "/items/?token=jessica", body: `{"name":"Portal Gun","async":true}`, status: 202,
			want: `{"id":"42","name":"Portal Gun"}`,
		},
		{
			name: "included prefix and guard", method: "POST",
			target: "/admin/?token=jessica", body: `{"name":"schwifty"}`, status: 201,
			headers: map[string]string{"X-Token": "fake-super-secret-token"},
			want:    `{"name":"schwifty"}`,
		},
		{
			name: "include-level guard rejects", method: "POST",
			target: "/admin/?token=jessica", body: `{"name":"schwifty"}`, status: 400,
		},
		{
			name: "application guard runs before the include guard", method: "POST",
			target: "/admin/", body: `{"name":"schwifty"}`, status: 400,
			headers: map[string]string{"X-Token": "fake-super-secret-token"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := doRequest(t, app, req)
			assertStatus(t, rec, tc.status)
			if tc.want != "" {
				assertJSON(t, rec, tc.want)
			}
		})
	}
}

func TestIntegrationErrorEnvelope(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	rec := do(t, app, "GET", "/users/?token=jessica&limit=abc")
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	envelope := decodeError(t, rec)
	if envelope.Error.Code != CodeValidationError {
		t.Errorf("code = %q, want %q", envelope.Error.Code, CodeValidationError)
	}
	if envelope.Error.Message != validationMessage {
		t.Errorf("message = %q, want %q", envelope.Error.Message, validationMessage)
	}
	if envelope.Error.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", envelope.Error.Status)
	}
	if len(envelope.Error.Details) != 1 {
		t.Fatalf("details = %d entries, want 1", len(envelope.Error.Details))
	}
	detail := envelope.Error.Details[0]
	if detail.Field != "limit" || detail.Location != "query" || detail.Issue != "must be a valid integer" {
		t.Errorf("detail = %+v, want limit/query/must be a valid integer", detail)
	}
	if envelope.RequestID == "" {
		t.Error("the envelope carries no request id")
	}
	if header := rec.Header().Get(HeaderRequestID); header != envelope.RequestID {
		t.Errorf("X-Request-Id = %q, but the body reports %q", header, envelope.RequestID)
	}
}

func TestIntegrationReportsEveryInvalidField(t *testing.T) {
	t.Parallel()
	type manyIn struct {
		Limit  int    `query:"limit"`
		Offset int    `query:"offset"`
		Active bool   `query:"active"`
		Needed string `query:"needed" required:"true"`
	}
	app := New(quietOptions())
	app.Get("/search", func(ctx *Context, in manyIn) (Empty, error) { return Empty{}, nil })

	rec := do(t, mustBuild(t, app), "GET", "/search?limit=x&offset=y&active=maybe")
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	envelope := decodeError(t, rec)
	if len(envelope.Error.Details) != 4 {
		t.Fatalf("details = %d entries, want 4 (one per bad field)\nbody: %s",
			len(envelope.Error.Details), rec.Body.String())
	}
	issues := map[string]string{}
	for _, d := range envelope.Error.Details {
		issues[d.Field] = d.Issue
	}
	want := map[string]string{
		"limit":  "must be a valid integer",
		"offset": "must be a valid integer",
		"active": "must be true or false",
		"needed": "is required",
	}
	for field, wantIssue := range want {
		if issues[field] != wantIssue {
			t.Errorf("issue for %q = %q, want %q", field, issues[field], wantIssue)
		}
	}
}

// TestIntegrationBodyCannotReachLocatedFields is the security property behind
// the scratch-decode strategy: a crafted body must never populate a field that
// is supposed to come from the path.
func TestIntegrationBodyCannotReachLocatedFields(t *testing.T) {
	t.Parallel()
	type updateIn struct {
		ID   string `path:"id"`
		Name string `json:"name"`
	}
	type updateOut struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	app := New(quietOptions())
	app.Put("/items/{id}", func(ctx *Context, in updateIn) (updateOut, error) {
		return updateOut{ID: in.ID, Name: in.Name}, nil
	}, AllowUnknownFields())

	// "ID" is the JSON name of the path-bound field, so an attacker could try
	// to set it through the body. Unknown members are allowed here precisely so
	// that the member reaches the decoder and the copy step is what stops it.
	rec := do(t, mustBuild(t, app), "PUT", "/items/real", `{"name":"renamed","ID":"spoofed"}`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"id":"real","name":"renamed"}`)
}

func TestIntegrationAutomaticMethods(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newIntegrationApp())

	t.Run("HEAD is answered by the GET route", func(t *testing.T) {
		t.Parallel()
		rec := do(t, app, "HEAD", "/users/me?token=jessica")
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("Content-Type = %q, want the JSON type", got)
		}
	})

	t.Run("OPTIONS advertises the methods", func(t *testing.T) {
		t.Parallel()
		rec := do(t, app, "OPTIONS", "/users/me?token=jessica")
		assertStatus(t, rec, http.StatusNoContent)
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD, OPTIONS" {
			t.Errorf("Allow = %q, want %q", allow, "GET, HEAD, OPTIONS")
		}
	})

	t.Run("an unsupported method reports 405 with Allow", func(t *testing.T) {
		t.Parallel()
		rec := do(t, app, "DELETE", "/users/me?token=jessica")
		assertStatus(t, rec, http.StatusMethodNotAllowed)
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD, OPTIONS" {
			t.Errorf("Allow = %q, want %q", allow, "GET, HEAD, OPTIONS")
		}
		if code := decodeError(t, rec).Error.Code; code != CodeMethodNotAllowed {
			t.Errorf("code = %q, want %q", code, CodeMethodNotAllowed)
		}
	})

	t.Run("HEAD on a path with no GET reports 405", func(t *testing.T) {
		t.Parallel()
		rec := do(t, app, "HEAD", "/admin/?token=jessica")
		assertStatus(t, rec, http.StatusMethodNotAllowed)
	})
}

// TestIntegrationDependencyIsolation proves that concurrent requests never
// observe each other's resolved values, which is what makes Context pooling
// safe. It is worth running under -race.
func TestIntegrationDependencyIsolation(t *testing.T) {
	t.Parallel()
	type caller struct{ Name string }
	type out struct {
		Name string `json:"name"`
	}

	app := New(quietOptions())
	app.Get("/who", func(ctx *Context, _ Empty) (out, error) {
		return out{Name: From[caller](ctx).Name}, nil
	}, Needs(func(ctx *Context) (caller, error) {
		return caller{Name: ctx.Header("X-Caller")}, nil
	}))
	mustBuild(t, app)

	const workers = 32
	const rounds = 25
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := string(rune('a' + i%26))
			for range rounds {
				req := httptest.NewRequest("GET", "/who", nil)
				req.Header.Set("X-Caller", name)
				rec := httptest.NewRecorder()
				app.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("status = %d, want 200", rec.Code)
					return
				}
				want := `{"name":"` + name + `"}`
				if got := strings.TrimSpace(rec.Body.String()); got != want {
					t.Errorf("a request for %q saw %s", name, got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
