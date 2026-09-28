package muzak

import (
	"reflect"
	"strings"
	"testing"
)

// The types below each hide a location tag somewhere the binder does not look.
// Before the reachability check every one of them built, bound nothing from
// the location, and let the JSON body write the field instead.

type ReachAuth struct {
	UserID string `header:"X-User-ID"`
	Tenant string `query:"tenant"`
}

type reachPtrEmbedIn struct {
	*ReachAuth
	Name string `json:"name"`
}

type reachNestedIn struct {
	Auth ReachAuth `json:"auth"`
	Name string    `json:"name"`
}

type reachSliceIn struct {
	Members []ReachAuth `json:"members"`
}

type reachMapIn struct {
	ByName map[string]*ReachAuth `json:"by_name"`
}

type reachDeepIn struct {
	Outer struct {
		Inner *struct {
			Session string `cookie:"session"`
		} `json:"inner"`
	} `json:"outer"`
}

type reachFileIn struct {
	Upload struct {
		Avatar File `file:"avatar"`
	} `json:"upload"`
}

type reachFormIn struct {
	Login struct {
		Password string `form:"password"`
	} `json:"login"`
}

// reachEmbeddedHolder is embedded by value, which the binder follows, but it
// holds a named field that the binder does not.
type reachEmbeddedHolder struct {
	Page  int       `query:"page"`
	Owner ReachAuth `json:"owner"`
}

type reachEmbeddedNestedIn struct {
	reachEmbeddedHolder
	Name string `json:"name"`
}

type reachUnexportedIn struct {
	userID string `header:"X-User-ID"`
	Name   string `json:"name"`
}

type reachAuthLower struct {
	Tenant string `query:"tenant"`
}

type reachUnexportedPtrEmbedIn struct {
	*reachAuthLower
	Name string `json:"name"`
}

func TestLocationTagTheBinderCannotReachIsABuildError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{"embedded pointer", reflect.TypeFor[reachPtrEmbedIn](),
			[]string{"field ReachAuth.UserID declares a header parameter", "embedded pointer *muzak.ReachAuth", "embed muzak.ReachAuth by value rather than by pointer"}},
		{"named nested struct", reflect.TypeFor[reachNestedIn](),
			[]string{"field Auth.UserID declares a header parameter inside the field Auth", "move the field to the top level"}},
		{"slice of structs", reflect.TypeFor[reachSliceIn](),
			[]string{"field Members.UserID declares a header parameter"}},
		{"map of pointers", reflect.TypeFor[reachMapIn](),
			[]string{"field ByName.UserID declares a header parameter"}},
		{"deep through a pointer", reflect.TypeFor[reachDeepIn](),
			[]string{"field Outer.Inner.Session declares a cookie parameter"}},
		{"file tag", reflect.TypeFor[reachFileIn](),
			[]string{"field Upload.Avatar declares a file parameter"}},
		{"form tag", reflect.TypeFor[reachFormIn](),
			[]string{"field Login.Password declares a form parameter"}},
		{"nested inside an embedded value", reflect.TypeFor[reachEmbeddedNestedIn](),
			[]string{"field Owner.UserID declares a header parameter"}},
		{"unexported field", reflect.TypeFor[reachUnexportedIn](),
			[]string{"field userID declares a header parameter but is unexported", "export the field"}},
		{"unexported embedded pointer", reflect.TypeFor[reachUnexportedPtrEmbedIn](),
			[]string{"field reachAuthLower.Tenant declares a query parameter", "embed muzak.reachAuthLower by value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := newBindPlan(tc.typ, "POST", "/x")
			if err == nil {
				t.Fatal("newBindPlan succeeded, want a build error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if !strings.HasPrefix(err.Error(), "muzak: POST /x: ") {
				t.Errorf("error = %q, want the muzak prefix and the route", err)
			}
		})
	}
}

// TestEmbeddedPointerLocatedFieldsRefusedAtBuild is the proof of concept from
// the review turned into the assertion it should have been: the header a
// gateway sets can no longer be overridden by a body member, because the route
// that would have allowed it does not build.
func TestEmbeddedPointerLocatedFieldsRefusedAtBuild(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/ptr", func(ctx *Context, in reachPtrEmbedIn) (Empty, error) { return Empty{}, nil })
	app.Post("/nested", func(ctx *Context, in reachNestedIn) (Empty, error) { return Empty{}, nil })
	message := buildError(t, app)
	for _, want := range []string{"POST /ptr", "POST /nested", "embed muzak.ReachAuth by value", "Auth.UserID"} {
		if !strings.Contains(message, want) {
			t.Errorf("Build() = %q, want it to mention %q", message, want)
		}
	}
}

// reachTree is recursive and carries no location tag. The search has to
// terminate on it and let it through as body content.
type reachTree struct {
	Name     string      `json:"name"`
	Children []reachTree `json:"children"`
	Left     *reachLeaf  `json:"left"`
	Right    *reachLeaf  `json:"right"`
}

type reachLeaf struct {
	Value int `json:"value"`
	// hidden is unexported and so invisible to encoding/json; the search
	// skips it for the same reason.
	hidden *ReachAuth
}

type ReachPlain struct {
	Note string `json:"note"`
}

type reachBodyIn struct {
	*ReachPlain
	ID   string    `path:"id"`
	Tree reachTree `json:"tree"`
}

func TestBodyMembersWithoutLocationTagsStillBind(t *testing.T) {
	t.Parallel()
	_, _ = reachLeaf{}.hidden, reachUnexportedIn{}.userID
	app := New(quietOptions())
	app.Post("/trees/{id}", func(ctx *Context, in reachBodyIn) (map[string]any, error) {
		return map[string]any{"id": in.ID, "tree": in.Tree.Name, "child": in.Tree.Children[0].Name, "left": in.Tree.Left.Value}, nil
	})
	mustBuild(t, app)
	rec := do(t, app, "POST", "/trees/7", `{"note":"n","tree":{"name":"root","children":[{"name":"kid","children":[]}],"left":{"value":3},"right":null}}`)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"child":"kid","id":"7","left":3,"tree":"root"}`)
}
