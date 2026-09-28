package muzak

import (
	"net/http"
	"strings"
	"testing"
)

type rtOut struct {
	OK bool `json:"ok"`
}

// okHandler is a handler that binds nothing and always succeeds.
func okHandler(ctx *Context, _ Empty) (rtOut, error) { return rtOut{OK: true}, nil }

func TestRegistrationMethods(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/thing", okHandler)
	app.Post("/thing", okHandler)
	app.Put("/thing", okHandler)
	app.Patch("/thing", okHandler)
	app.Delete("/thing", okHandler)
	app.Head("/other", okHandler)
	app.Handle("OPTIONS", "/other", okHandler)
	app.Handle("purge", "/thing", okHandler)
	mustBuild(t, app)

	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "PURGE"} {
		t.Run(method, func(t *testing.T) {
			rec := do(t, app, method, "/thing")
			assertStatus(t, rec, http.StatusOK)
			assertJSON(t, rec, `{"ok":true}`)
		})
	}
	t.Run("HEAD", func(t *testing.T) {
		assertStatus(t, do(t, app, "HEAD", "/other"), http.StatusOK)
	})
	t.Run("OPTIONS", func(t *testing.T) {
		assertStatus(t, do(t, app, "OPTIONS", "/other"), http.StatusOK)
	})
}

func TestRegistrationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(*App)
		want  string
	}{
		{
			name:  "path without a leading slash",
			build: func(a *App) { a.Get("thing", okHandler) },
			want:  `path must begin with "/"`,
		},
		{
			name:  "nil handler",
			build: func(a *App) { a.Get[Empty, rtOut]("/thing", nil) },
			want:  "handler is nil",
		},
		{
			name: "duplicate route",
			build: func(a *App) {
				a.Get("/thing", okHandler)
				a.Get("/thing", okHandler)
			},
			want: "is registered twice",
		},
		{
			name: "conflicting parameter names",
			build: func(a *App) {
				a.Get("/thing/{id}", okHandler)
				a.Get("/thing/{name}/x", okHandler)
			},
			want: "conflicting parameter name",
		},
		{
			name:  "parameter name declared twice in one path",
			build: func(a *App) { a.Get("/orgs/{id}/users/{id}", okHandler) },
			want:  "declared twice",
		},
		{
			name: "duplicate operation id",
			build: func(a *App) {
				a.Get("/one", okHandler, OperationID("shared"))
				a.Get("/two", okHandler, OperationID("shared"))
			},
			want: "operation id",
		},
		{
			name:  "status outside the HTTP range",
			build: func(a *App) { a.Get("/thing", okHandler, Status(999)) },
			want:  "is not a valid HTTP status code",
		},
		{
			name:  "prefix without a leading slash",
			build: func(a *App) { a.Include(NewRouter(), WithPrefix("admin")) },
			want:  `must begin with "/"`,
		},
		{
			name:  "prefix with a trailing slash",
			build: func(a *App) { a.Include(NewRouter(), WithPrefix("/admin/")) },
			want:  `must not end with "/"`,
		},
		{
			name:  "nil included router",
			build: func(a *App) { a.Include(nil) },
			want:  "nil router",
		},
		{
			name:  "router including itself",
			build: func(a *App) { a.Include(a.Router) },
			want:  "cannot include itself",
		},
		{
			name: "router included twice",
			build: func(a *App) {
				shared := NewRouter()
				shared.Get("/shared", okHandler)
				a.Include(shared)
				a.Include(shared, WithPrefix("/again"))
			},
			want: "included more than once",
		},
		{
			name: "path parameter no field binds",
			build: func(a *App) {
				a.Get("/thing/{id}", func(ctx *Context, in struct {
					Other string `path:"other"`
				}) (rtOut, error) {
					return rtOut{}, nil
				})
			},
			want: "the route template does not declare",
		},
		{
			name: "unbindable input type",
			build: func(a *App) {
				a.Get("/thing", func(ctx *Context, in string) (rtOut, error) { return rtOut{}, nil })
			},
			want: "must be a struct or muzak.Empty",
		},
		{
			name: "unsupported parameter type",
			build: func(a *App) {
				a.Get("/thing", func(ctx *Context, in struct {
					Bad chan int `query:"bad"`
				}) (rtOut, error) {
					return rtOut{}, nil
				})
			},
			want: "cannot be bound from a request parameter",
		},
		{
			name: "empty parameter name",
			build: func(a *App) {
				a.Get("/thing", func(ctx *Context, in struct {
					Bad string `query:""`
				}) (rtOut, error) {
					return rtOut{}, nil
				})
			},
			want: "empty query parameter name",
		},
		{
			name: "required and defaulted at once",
			build: func(a *App) {
				a.Get("/thing", func(ctx *Context, in struct {
					Bad string `query:"bad" required:"true" default:"x"`
				}) (rtOut, error) {
					return rtOut{}, nil
				})
			},
			want: "both required and given a default",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			tc.build(app)
			if got := buildError(t, app); !strings.Contains(got, tc.want) {
				t.Errorf("build error = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// TestBuildReportsEveryProblemAtOnce pins the promise that a misconfigured
// application lists all its problems rather than one per attempt.
func TestBuildReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("one", okHandler)
	app.Get("two", okHandler)
	app.Get[Empty, rtOut]("/three", nil)

	message := buildError(t, app)
	if got := strings.Count(message, "muzak:"); got < 3 {
		t.Errorf("the build reported %d problems, want at least 3\n%s", got, message)
	}
}

// TestServeHTTPOnBrokenApp checks that an application that cannot be built
// answers every request with an opaque 500 rather than panicking.
func TestServeHTTPOnBrokenApp(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("bad-path", okHandler)

	rec := do(t, app, "GET", "/anything")
	assertStatus(t, rec, http.StatusInternalServerError)
	if code := decodeError(t, rec).Error.Code; code != CodeInternalError {
		t.Errorf("code = %q, want %q", code, CodeInternalError)
	}
	if body := rec.Body.String(); strings.Contains(body, "bad-path") {
		t.Errorf("the response leaked the build error: %s", body)
	}
}

func TestInheritance(t *testing.T) {
	t.Parallel()

	child := NewRouter(WithTags("child"), WithResponseDoc(410, "gone"))
	child.Get("/leaf", okHandler, WithTags("leaf"), Summary("Leaf"), Description("A leaf."))

	middle := NewRouter(WithTags("middle"))
	middle.Include(child, WithPrefix("/child"))

	app := New(quietOptions())
	app.Include(middle, WithPrefix("/middle"), WithTags("mounted"))
	mustBuild(t, app)

	route := app.routes[0]
	if route.Path != "/middle/child/leaf" {
		t.Errorf("path = %q, want %q", route.Path, "/middle/child/leaf")
	}
	wantTags := []string{"mounted", "middle", "child", "leaf"}
	if len(route.Tags) != len(wantTags) {
		t.Fatalf("tags = %v, want %v", route.Tags, wantTags)
	}
	for i, want := range wantTags {
		if route.Tags[i] != want {
			t.Fatalf("tags = %v, want %v", route.Tags, wantTags)
		}
	}
	if len(route.responses) != 1 || route.responses[0].code != 410 {
		t.Errorf("responses = %v, want one entry for 410", route.responses)
	}
	if route.Summary != "Leaf" || route.Description != "A leaf." {
		t.Errorf("summary/description = %q/%q", route.Summary, route.Description)
	}
}

func TestTagsAreDeduplicated(t *testing.T) {
	t.Parallel()
	child := NewRouter(WithTags("shared"))
	child.Get("/leaf", okHandler, WithTags("shared", "leaf", "leaf"))
	app := New(quietOptions())
	app.Include(child, WithTags("shared"))
	mustBuild(t, app)

	got := app.routes[0].Tags
	if len(got) != 2 || got[0] != "shared" || got[1] != "leaf" {
		t.Errorf("tags = %v, want [shared leaf]", got)
	}
}

func TestDeprecatedAndHiddenInherit(t *testing.T) {
	t.Parallel()
	child := NewRouter()
	child.Get("/leaf", okHandler)
	app := New(quietOptions())
	app.Include(child, Deprecated(), Hidden())
	mustBuild(t, app)

	route := app.routes[0]
	if !route.Deprecated || !route.Hidden {
		t.Errorf("deprecated = %v, hidden = %v; want both true", route.Deprecated, route.Hidden)
	}
	if _, described := app.spec.Paths["/leaf"]; described {
		t.Error("a hidden route appears in the OpenAPI document")
	}
}

func TestRoutesSnapshot(t *testing.T) {
	t.Parallel()
	r := NewRouter()
	r.Get("/a", okHandler)
	r.Get("/b", okHandler)

	routes := r.Routes()
	if len(routes) != 2 {
		t.Fatalf("Routes() = %d entries, want 2", len(routes))
	}
	routes[0] = nil
	if r.Routes()[0] == nil {
		t.Error("Routes() returned the router's own slice, not a copy")
	}
}

func TestDeriveOperationID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{"GET", "/users", "get_users"},
		{"GET", "/users/", "get_users"},
		{"POST", "/users/{username}", "post_users_by_username"},
		{"GET", "/files/{path...}", "get_files_by_path"},
		{"DELETE", "/", "delete"},
		{"GET", "/a-b/c.d", "get_a_b_c_d"},
		{"GET", "/CamelCase", "get_camelcase"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			if got := deriveOperationID(tc.method, tc.path); got != tc.want {
				t.Errorf("deriveOperationID(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestDedupeStrings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"single", []string{"a"}, []string{"a"}},
		{"no duplicates", []string{"a", "b"}, []string{"a", "b"}},
		{"duplicates keep first order", []string{"b", "a", "b", "a"}, []string{"b", "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dedupeStrings(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("dedupeStrings(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dedupeStrings(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestConcatDoesNotShareBackingArrays(t *testing.T) {
	t.Parallel()
	base := make([]string, 0, 8)
	base = append(base, "shared")

	left := concat(base, []string{"left"})
	right := concat(base, []string{"right"})

	if left[1] != "left" || right[1] != "right" {
		t.Errorf("concat aliased its input: left = %v, right = %v", left, right)
	}
	if got := concat(base, nil); len(got) != 1 {
		t.Errorf("concat with nothing to add = %v, want the original", got)
	}
}

func TestMaxBodySizeInheritsAndOverrides(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.MaxBodySize = 40

	child := NewRouter(MaxBodySize(30))
	child.Post("/small", func(ctx *Context, in intgCreateBody) (rtOut, error) { return rtOut{OK: true}, nil })
	child.Post("/tiny", func(ctx *Context, in intgCreateBody) (rtOut, error) { return rtOut{OK: true}, nil },
		MaxBodySize(10))

	app := New(opts)
	app.Post("/default", func(ctx *Context, in intgCreateBody) (rtOut, error) { return rtOut{OK: true}, nil })
	app.Include(child)
	mustBuild(t, app)

	byPath := map[string]int64{}
	for _, route := range app.routes {
		byPath[route.Path] = route.maxBodySize
	}
	if byPath["/default"] != 40 || byPath["/small"] != 30 || byPath["/tiny"] != 10 {
		t.Errorf("limits = %v, want /default 40, /small 30, /tiny 10", byPath)
	}

	rec := do(t, app, "POST", "/tiny", `{"name":"far too long to fit in ten bytes"}`)
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
	if code := decodeError(t, rec).Error.Code; code != CodePayloadTooLarge {
		t.Errorf("code = %q, want %q", code, CodePayloadTooLarge)
	}
}

func TestUseAddsMiddleware(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Custom", "yes")
			next.ServeHTTP(w, r)
		})
	})
	app.Get("/thing", okHandler)
	mustBuild(t, app)

	rec := do(t, app, "GET", "/thing")
	if got := rec.Header().Get("X-Custom"); got != "yes" {
		t.Errorf("X-Custom = %q, want %q", got, "yes")
	}
}

func TestAppOptionsAccessors(t *testing.T) {
	t.Parallel()
	app := New(AppOptions{LoggerOptions: LoggerOptions{Format: LogFormatNone}})
	config := app.Config()
	if config.Addr != ":8080" {
		t.Errorf("Addr = %q, want the default", config.Addr)
	}
	if config.MaxBodySize != DefaultMaxBodySize {
		t.Errorf("MaxBodySize = %d, want the default", config.MaxBodySize)
	}
	if config.DocsPath != "/docs" || config.OpenAPIPath != "/openapi.json" {
		t.Errorf("documentation paths = %q, %q", config.DocsPath, config.OpenAPIPath)
	}
	if app.Logger() == nil {
		t.Error("Logger() returned nil")
	}
	if app.Addr() != "" {
		t.Errorf("Addr() = %q before the server started, want empty", app.Addr())
	}
}

// TestOptionsAddsAfterNew covers publishing a dependency discovered after the
// application was created.
func TestOptionsAddsAfterNew(t *testing.T) {
	t.Parallel()
	type late struct{ Value string }
	type out struct {
		Value string `json:"value"`
	}

	app := New(quietOptions())
	app.Options(WithSingleton(late{Value: "arrived"}))
	app.Get("/late", func(ctx *Context, _ Empty) (out, error) {
		return out{Value: From[late](ctx).Value}, nil
	})
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/late"), `{"value":"arrived"}`)
}

func TestPlural(t *testing.T) {
	t.Parallel()
	if got := plural(1, "route"); got != "route" {
		t.Errorf("plural(1) = %q, want %q", got, "route")
	}
	if got := plural(2, "route"); got != "routes" {
		t.Errorf("plural(2) = %q, want %q", got, "routes")
	}
	if got := plural(0, "route"); got != "routes" {
		t.Errorf("plural(0) = %q, want %q", got, "routes")
	}
}

func TestAllowHeaderIncludesAutomaticMethods(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		methods []string
		want    string
	}{
		{"get gains head and options", []string{"GET"}, "GET, HEAD, OPTIONS"},
		{"post gains only options", []string{"POST"}, "OPTIONS, POST"},
		{"explicit head is not duplicated", []string{"GET", "HEAD"}, "GET, HEAD, OPTIONS"},
		{"explicit options is not duplicated", []string{"GET", "OPTIONS"}, "GET, HEAD, OPTIONS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			methods := map[string][]*Route{}
			for _, m := range tc.methods {
				methods[m] = []*Route{{Method: m}}
			}
			if got := allowHeader(methods); got != tc.want {
				t.Errorf("allowHeader(%v) = %q, want %q", tc.methods, got, tc.want)
			}
		})
	}
}

func TestPathOrRequest(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequest("GET", "http://example.test/actual", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	var missing *Route
	if got := missing.pathOrRequest(req); got != "/actual" {
		t.Errorf("a nil route reported %q, want the request path", got)
	}
	route := &Route{Path: "/template/{id}"}
	if got := route.pathOrRequest(req); got != "/template/{id}" {
		t.Errorf("route reported %q, want its template", got)
	}
}
