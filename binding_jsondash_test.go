package muzak

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// dashIn is the shape the review found: a located field that also says it is
// never part of the body. Both halves have to hold.
type dashIn struct {
	UserID string `header:"X-User-ID" json:"-" required:"true"`
	Role   string `query:"role" json:"-" default:"viewer"`
}

func TestJSONDashKeepsLocatedBinding(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/dash", func(ctx *Context, in dashIn) (map[string]string, error) {
		return map[string]string{"user": in.UserID, "role": in.Role}, nil
	})
	mustBuild(t, app)

	req := httptest.NewRequest("GET", "/dash?role=admin", nil)
	req.Header.Set("X-User-ID", "alice")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"role":"admin","user":"alice"}`)

	// The default and the required check travel with the binding.
	req = httptest.NewRequest("GET", "/dash", nil)
	req.Header.Set("X-User-ID", "alice")
	rec = doRequest(t, app, req)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"role":"viewer","user":"alice"}`)

	rec = do(t, app, "GET", "/dash")
	assertStatus(t, rec, 422)
	if got := decodeError(t, rec).Error.Details; len(got) != 1 || got[0].Field != "X-User-ID" || got[0].Location != "header" {
		t.Errorf("details = %+v, want the missing header reported", got)
	}
}

type dashBodyIn struct {
	UserID string `header:"X-User-ID" json:"-"`
	Name   string `json:"name"`
}

// TestJSONDashLocatedFieldIsNotABodyMember checks the other half: the body
// cannot reach the field, and the document lists it as a header and not as a
// property of the body.
func TestJSONDashLocatedFieldIsNotABodyMember(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/dash", func(ctx *Context, in dashBodyIn) (map[string]string, error) {
		return map[string]string{"user": in.UserID, "name": in.Name}, nil
	})
	mustBuild(t, app)

	req := httptest.NewRequest("POST", "/dash", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "alice")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"name":"x","user":"alice"}`)

	req = httptest.NewRequest("POST", "/dash", strings.NewReader(`{"name":"x","UserID":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "alice")
	rec = doRequest(t, app, req)
	assertStatus(t, rec, 422)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	op := doc.Paths["/dash"].Post
	if len(op.Parameters) != 1 || op.Parameters[0].Name != "X-User-ID" || op.Parameters[0].In != "header" {
		t.Errorf("parameters = %+v, want the header alone", op.Parameters)
	}
	body := op.RequestBody.Content["application/json"].Schema
	if _, present := body.Properties["UserID"]; present || len(body.Properties) != 1 {
		t.Errorf("body properties = %v, want name alone", keysOf(body.Properties))
	}
}

// DashGroup is embedded with json:"-". Its located parameter binds, and its
// body member does not become one, because the decoder never writes to it.
type DashGroup struct {
	Tenant string `query:"tenant"`
	Note   string `json:"note"`
}

type dashEmbeddedIn struct {
	DashGroup `json:"-"`
	Name      string `json:"name"`
}

// dashSkippedGroup has nothing located, so embedding it with json:"-" drops
// it entirely, as it always did.
type dashSkippedGroup struct {
	Hidden string `json:"hidden"`
}

type dashSkippedIn struct {
	dashSkippedGroup `json:"-"`
	Name             string `json:"name"`
}

func TestJSONDashOnEmbeddedStruct(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/embedded", func(ctx *Context, in dashEmbeddedIn) (map[string]string, error) {
		return map[string]string{"tenant": in.Tenant, "note": in.Note, "name": in.Name}, nil
	})
	app.Post("/skipped", func(ctx *Context, in dashSkippedIn) (map[string]string, error) {
		return map[string]string{"hidden": in.Hidden, "name": in.Name}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "POST", "/embedded?tenant=acme", `{"name":"x"}`)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"name":"x","note":"","tenant":"acme"}`)
	rec = do(t, app, "POST", "/embedded?tenant=acme", `{"name":"x","note":"n"}`)
	assertStatus(t, rec, 422)

	rec = do(t, app, "POST", "/skipped", `{"name":"x"}`)
	assertStatus(t, rec, 200)
	assertJSON(t, rec, `{"hidden":"","name":"x"}`)
}

type dashNestedIn struct {
	Auth struct {
		UserID string `header:"X-User-ID"`
	} `json:"-"`
	Name string `json:"name"`
}

// TestJSONDashDoesNotHideAnUnreachableTag checks that json:"-" is no way
// around the reachability check: a located field inside an excluded struct is
// bound from nowhere, which is the mistake the check exists to report.
func TestJSONDashDoesNotHideAnUnreachableTag(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/nested", func(ctx *Context, in dashNestedIn) (Empty, error) { return Empty{}, nil })
	if message := buildError(t, app); !strings.Contains(message, "field Auth.UserID declares a header parameter") {
		t.Errorf("Build() = %q, want the nested header named", message)
	}
}
