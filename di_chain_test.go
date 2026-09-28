package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type chainPrincipal struct{ Name string }

type chainOut struct {
	Who string `json:"who"`
}

// chainAdminApp mounts a router whose own provider declares the same type as
// the provider its includer attaches, which is the shape in which a nearer
// declaration used to remove the outer one and with it the admin check.
func chainAdminApp(t *testing.T, inner func(*Context) (chainPrincipal, error)) *App {
	t.Helper()
	requireAdmin := func(c *Context) (chainPrincipal, error) {
		if c.Header("X-Role") != "admin" {
			return chainPrincipal{}, NewHTTPError(http.StatusForbidden, "admin only")
		}
		return chainPrincipal{Name: "admin"}, nil
	}
	child := NewRouter(Needs(inner))
	child.Get("/panel", func(c *Context, _ Empty) (chainOut, error) {
		return chainOut{Who: From[chainPrincipal](c).Name}, nil
	})
	app := New(quietOptions())
	app.Include(child, WithPrefix("/admin"), Needs(requireAdmin))
	return mustBuild(t, app)
}

func TestInnerNeedsCannotDropOuterAuthProvider(t *testing.T) {
	t.Parallel()
	innerRan := false
	app := chainAdminApp(t, func(*Context) (chainPrincipal, error) {
		innerRan = true
		return chainPrincipal{Name: "anyone"}, nil
	})

	assertStatus(t, do(t, app, "GET", "/admin/panel"), http.StatusForbidden)
	if innerRan {
		t.Error("the inner provider ran after the outer one rejected the request")
	}
}

func TestInnerNeedsValueWinsWhenEveryProviderSucceeds(t *testing.T) {
	t.Parallel()
	app := chainAdminApp(t, func(c *Context) (chainPrincipal, error) {
		// The outer value of the same type is already resolved and is what
		// From returns here, so an inner provider can refine it.
		outer := From[chainPrincipal](c)
		return chainPrincipal{Name: outer.Name + "+refined"}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/panel", strings.NewReader(""))
	req.Header.Set("X-Role", "admin")
	assertJSON(t, doRequest(t, app, req), `{"who":"admin+refined"}`)
}

func TestTryFromReturnsTheInnermostValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithSingleton(diValue{Text: "app"}))
	app.Get("/x", func(ctx *Context, _ Empty) (diOut, error) {
		v, _ := TryFrom[diValue](ctx)
		return diOut{Text: v.Text}, nil
	}, Needs(func(*Context) (diValue, error) { return diValue{Text: "route"}, nil }))
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/x"), `{"text":"route"}`)
}
