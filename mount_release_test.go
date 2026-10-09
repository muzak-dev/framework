package muzak

import (
	"net/http"
	"testing"
)

// TestMountReleasesWhatItAcquired is the regression test for a handler mount
// whose providers acquired something: the releases were left to the safety
// net that runs when the Context returns to the pool, after the response, and
// were told the request had failed, so a lock or a transaction held for a
// mount was rolled back behind every response the mount served.
func TestMountReleasesWhatItAcquired(t *testing.T) {
	t.Parallel()

	t.Run("a served request releases with no failure", func(t *testing.T) {
		t.Parallel()
		log := newReleaseLog()
		app := New(quietOptions())
		app.Mount("/legacy", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}), acquireAs[relA](log, "a", nil))
		mustBuild(t, app)
		assertStatus(t, do(t, app, http.MethodGet, "/legacy/x", ""), http.StatusOK)
		if failure := log.failure(t, "a"); failure != nil {
			t.Errorf("the release was told a served request failed: %v", failure)
		}
		assertEvents(t, log, "acquire a, release a")
	})

	t.Run("a panic releases with its failure", func(t *testing.T) {
		t.Parallel()
		log := newReleaseLog()
		app := New(quietOptions())
		app.Mount("/legacy", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("legacy code fell over")
		}), acquireAs[relA](log, "a", nil))
		mustBuild(t, app)
		assertStatus(t, do(t, app, http.MethodGet, "/legacy/x", ""), http.StatusInternalServerError)
		if log.failure(t, "a") == nil {
			t.Error("the release was told a panicking mount succeeded")
		}
		assertEvents(t, log, "acquire a, release a")
	})
}
