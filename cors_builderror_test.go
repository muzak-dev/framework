package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSBuildFailureIsLoggedLikeAnyOther is the regression test for a build
// failure with no reason anywhere: a CORS policy that cannot be served safely
// was recorded only after the start-up log had already said the application
// was built, so ServeHTTP answered every request with a bare 500 while the log
// held nothing but "Registered 1 route".
func TestCORSBuildFailureIsLoggedLikeAnyOther(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.CORS = CORSOptions{AllowedOrigins: []string{"*"}, AllowCredentials: true}
	app := New(opts)
	app.Get("/x", okHandler)

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an application that did not build", rec.Code)
	}
	out := logs.String()
	if !strings.Contains(out, "could not be built") || !strings.Contains(out, "wildcard CORS origin") {
		t.Errorf("the build failure and its reason were not logged:\n%s", out)
	}
	if strings.Contains(out, "Registered") {
		t.Errorf("the routes were reported registered for an application that did not build:\n%s", out)
	}
}
