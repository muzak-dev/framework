package muzak

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// What instrumentation needs from the framework: the template a request
// matched, not the path it arrived on.

// TestRouteFromContextGivesTheTemplateNotThePath is the point of the whole
// thing. Naming a span or labelling a metric with the concrete path is one
// time series per identifier, which is how a tracing backend falls over and
// how "is this endpoint slow" stops being answerable.
func TestRouteFromContextGivesTheTemplateNotThePath(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		template string
		matched  bool
	)

	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			mu.Lock()
			template, matched = RouteFromContext(r.Context())
			mu.Unlock()
		})
	})
	app.Get("/orgs/{org_id}/apps/{app_id}", func(ctx *Context, _ Empty) (map[string]string, error) {
		return map[string]string{"ok": "yes"}, nil
	})
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET",
		"/orgs/01a06391-0a5b-7695-af40-babc7981a63e/apps/01a063ad-d99f-7a05-98b6-d12f11b24ea3", ""),
		http.StatusOK)

	mu.Lock()
	defer mu.Unlock()
	if !matched {
		t.Fatal("RouteFromContext reported no route for a request that matched one")
	}
	if want := "/orgs/{org_id}/apps/{app_id}"; template != want {
		t.Errorf("route = %q, want %q", template, want)
	}
	if strings.Contains(template, "01a0") {
		t.Error("the route carries an identifier; that is the cardinality this exists to avoid")
	}
}

// TestRouteFromContextReportsNothingForAMiss keeps a 404 counted as a 404
// rather than as traffic to a route named "".
func TestRouteFromContextReportsNothingForAMiss(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		matched bool
	)

	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			mu.Lock()
			_, matched = RouteFromContext(r.Context())
			mu.Unlock()
		})
	})
	app.Get("/known", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/unknown", ""), http.StatusNotFound)

	mu.Lock()
	defer mu.Unlock()
	if matched {
		t.Error("a request that matched no route reported one")
	}
}

// TestRouteFromContextIsAbsentBeforeRouting: read before next.ServeHTTP there
// is nothing to report, because routing has not happened.
func TestRouteFromContextIsAbsentBeforeRouting(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		early bool
		late  bool
	)

	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			_, early = RouteFromContext(r.Context())
			mu.Unlock()
			next.ServeHTTP(w, r)
			mu.Lock()
			_, late = RouteFromContext(r.Context())
			mu.Unlock()
		})
	})
	app.Get("/items/{id}", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/items/7", ""), http.StatusOK)

	mu.Lock()
	defer mu.Unlock()
	if early {
		t.Error("a route was reported before routing happened")
	}
	if !late {
		t.Error("no route was reported after routing happened")
	}
}

// TestStatusRecorderReadsTheStatusWithoutWrappingAgain saves every middleware
// the httpsnoop-shaped wrapper the framework already installs.
func TestStatusRecorderReadsTheStatusWithoutWrappingAgain(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		status int
		ok     bool
	)

	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			mu.Lock()
			var recorder StatusRecorder
			recorder, ok = w.(StatusRecorder)
			if ok {
				status = recorder.Status()
			}
			mu.Unlock()
		})
	})
	app.Get("/teapot", func(ctx *Context, _ Empty) (Empty, error) {
		return Empty{}, NewHTTPError(http.StatusTeapot, "no coffee here")
	})
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/teapot", ""), http.StatusTeapot)

	mu.Lock()
	defer mu.Unlock()
	if !ok {
		t.Fatal("the response writer does not satisfy StatusRecorder")
	}
	if status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", status, http.StatusTeapot)
	}
}
