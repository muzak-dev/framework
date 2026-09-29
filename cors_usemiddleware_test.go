package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Middleware installed with Use sits inside the CORS policy. A browser never
// sends credentials on a preflight, so an authenticating middleware outside
// CORS would refuse every one, and its own 401 or 429 would carry no
// Access-Control-Allow-Origin, which the browser reports as a network error
// rather than the status the client needed to see.
func TestUseMiddlewareRunsInsideCORS(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.CORS = CORSOptions{
		AllowedOrigins: []string{"https://app.example"},
	}
	app := New(opts)
	var seen []string
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Method)
			// An authentication middleware that wants a token on everything.
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	app.Get("/x", okHandler)
	mustBuild(t, app)

	t.Run("preflight is answered by the policy", func(t *testing.T) {
		seen = nil
		req := httptest.NewRequest(http.MethodOptions, "/x", nil)
		req.Header.Set("Origin", "https://app.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusNoContent)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
			t.Errorf("Allow-Origin = %q", got)
		}
		if len(seen) != 0 {
			t.Errorf("the middleware saw %v; a preflight is the policy's to answer", seen)
		}
	})

	t.Run("a short circuit carries the policy's headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Origin", "https://app.example")
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusUnauthorized)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
			t.Errorf("Allow-Origin = %q on the middleware's own 401", got)
		}
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary = %q, want it to include Origin", got)
		}
	})

	t.Run("a denied origin still gets nothing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusUnauthorized)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("a denied origin received %q", got)
		}
	})
}
