package muzak

import (
	"errors"
	"net/http"
	"testing"
)

// TestErrorRendererStatusWithNoBodyIsClamped is the regression test for a
// status that reached net/http unchecked. A renderer's status was clamped to
// the range HTTP defines when it came with a body, but written as it stood
// when it came without one, so a renderer answering 0, the zero value of a
// status it forgot to set, made net/http panic and the client lose the
// connection instead of receiving an error.
func TestErrorRendererStatusWithNoBodyIsClamped(t *testing.T) {
	t.Parallel()
	for _, status := range []int{0, -1, 99, 600, 1000} {
		opts := quietOptions()
		opts.ErrorRenderer = func(*Context, error) (int, any) { return status, nil }
		app := New(opts)
		app.Get("/x", func(*Context, Empty) (rtOut, error) {
			return rtOut{}, errors.New("anything")
		})
		mustBuild(t, app)

		rec := do(t, app, "GET", "/x")
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("renderer status %d: response status = %d, want 500", status, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("renderer status %d: body = %q, want empty", status, rec.Body.String())
		}
	}
}
