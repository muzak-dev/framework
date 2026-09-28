package muzak

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// nilIdentity is an interface type whose provider may legitimately return no
// value, the natural encoding of an anonymous caller.
type nilIdentity interface{ Name() string }

// TestProviderOfAnInterfaceMayReturnNil is the regression test for a nil
// interface value that panicked when read back: the value was stored as a nil
// any, and the assertion to T in TryFrom, the form that must not panic, does
// not accept one. Every anonymous request was a 500.
func TestProviderOfAnInterfaceMayReturnNil(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	var got nilIdentity = stubIdentity("unset")
	var found bool
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		got, found = TryFrom[nilIdentity](ctx)
		if from := From[nilIdentity](ctx); from != nil {
			t.Errorf("From = %v, want the nil the provider returned", from)
		}
		return rtOut{OK: true}, nil
	}, Needs(func(*Context) (nilIdentity, error) { return nil, nil }))
	mustBuild(t, app)

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !found || got != nil {
		t.Errorf("TryFrom = (%v, %v), want (nil, true): the route did declare the dependency", got, found)
	}
}

type stubIdentity string

func (s stubIdentity) Name() string { return string(s) }
