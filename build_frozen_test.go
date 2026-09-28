package muzak

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestConfigurationAfterBuildPanics is the regression test for an implicit
// build, by Document writing the OpenAPI file at start-up for instance, that
// froze the application and then silently dropped every guard, middleware and
// route added after it: the application failed open while the code read as
// though it were protected.
func TestConfigurationAfterBuildPanics(t *testing.T) {
	t.Parallel()
	deny := func(*Context) error { return NewHTTPError(http.StatusUnauthorized, "no") }
	denyAll := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusForbidden)
		})
	}
	calls := map[string]func(app *App, child *Router){
		"App.Options":     func(app *App, _ *Router) { app.Options(WithDependencies(deny)) },
		"App.Use":         func(app *App, _ *Router) { app.Use(denyAll) },
		"GET /late":       func(app *App, _ *Router) { app.Get("/late", okHandler) },
		"DELETE /late":    func(app *App, _ *Router) { app.Delete("/late", okHandler) },
		"Include":         func(app *App, _ *Router) { app.Include(NewRouter()) },
		"GET /child/late": func(_ *App, child *Router) { child.Get("/child/late", okHandler) },
	}
	builds := map[string]func(t *testing.T, app *App){
		"Document": func(t *testing.T, app *App) {
			if _, err := app.Document(); err != nil {
				t.Fatal(err)
			}
		},
		"ServeHTTP": func(t *testing.T, app *App) { assertStatus(t, do(t, app, "GET", "/admin"), http.StatusOK) },
		"Build":     func(t *testing.T, app *App) { mustBuild(t, app) },
	}
	for buildName, build := range builds {
		for call, configure := range calls {
			t.Run(buildName+"/"+call, func(t *testing.T) {
				t.Parallel()
				app := New(quietOptions())
				app.Get("/admin", okHandler)
				child := NewRouter()
				child.Get("/child", okHandler)
				app.Include(child)
				build(t, app)

				recovered := catchPanic(func() { configure(app, child) })
				msg := fmt.Sprint(recovered)
				if recovered == nil || !strings.HasPrefix(msg, "muzak: ") || !strings.Contains(msg, "after the application was built") {
					t.Fatalf("%s after %s: recovered %v, want a muzak panic naming the fix", call, buildName, recovered)
				}
				if !strings.Contains(msg, strings.TrimPrefix(call, "registering ")) {
					t.Errorf("the panic %q does not name the call %q", msg, call)
				}
				// Nothing was half-applied: the application still serves as
				// it was built.
				assertStatus(t, do(t, app, "GET", "/admin"), http.StatusOK)
			})
		}
	}
}

// TestConfigurationBeforeBuildIsAccepted pins the other side: everything the
// panic refuses is accepted until the application is built, in any order.
func TestConfigurationBeforeBuildIsAccepted(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/admin", okHandler)
	app.Options(WithDependencies(func(*Context) error { return NewHTTPError(http.StatusUnauthorized, "no") }))
	app.Use(func(next http.Handler) http.Handler { return next })
	app.Include(NewRouter())
	if _, err := app.Document(); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, do(t, app, "GET", "/admin"), http.StatusUnauthorized)
}
