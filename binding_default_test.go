package muzak

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

type defaultsBase struct {
	Region string `json:"region" default:"eu"`
}

type bodyDefaultsIn struct {
	defaultsBase
	Role   string `json:"role" default:"user"`
	Limit  int    `json:"limit" default:"25"`
	Live   bool   `json:"live" default:"true"`
	Name   string `json:"name"`
	Detail struct {
		Mode string `json:"mode" default:"fast"`
	} `json:"detail"`
}

type bodyDefaultsOut struct {
	Role   string `json:"role"`
	Limit  int    `json:"limit"`
	Live   bool   `json:"live"`
	Region string `json:"region"`
	Mode   string `json:"mode"`
}

func bodyDefaultsApp(t *testing.T) *App {
	t.Helper()
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, in bodyDefaultsIn) (bodyDefaultsOut, error) {
		return bodyDefaultsOut{Role: in.Role, Limit: in.Limit, Live: in.Live, Region: in.Region, Mode: in.Detail.Mode}, nil
	})
	mustBuild(t, app)
	return app
}

// A default on a member of the JSON body is what the document says it is: a
// client that leaves the member out gets it, and one that sends it wins.
func TestABodyMemberTakesItsDefaultWhenOmitted(t *testing.T) {
	t.Parallel()
	app := bodyDefaultsApp(t)

	rec := do(t, app, "POST", "/x", `{"name":"a"}`)
	assertStatus(t, rec, http.StatusOK)
	var out bodyDefaultsOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Role != "user" || out.Limit != 25 || !out.Live || out.Region != "eu" {
		t.Errorf("defaults = %+v, want role user, limit 25, live and region eu", out)
	}
	// Only the top level of the body is defaulted.
	if out.Mode != "" {
		t.Errorf("a nested member was defaulted to %q", out.Mode)
	}

	rec = do(t, app, "POST", "/x", `{"role":"admin","limit":0,"live":false,"region":"us"}`)
	assertStatus(t, rec, http.StatusOK)
	out = bodyDefaultsOut{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Role != "admin" || out.Limit != 0 || out.Live || out.Region != "us" {
		t.Errorf("sent values = %+v, want them to override the defaults", out)
	}
}

func TestABodyDefaultIsDocumentedAsTheTypeItIs(t *testing.T) {
	t.Parallel()
	doc, err := bodyDefaultsApp(t).Document()
	if err != nil {
		t.Fatal(err)
	}
	body := doc.Components.Schemas["bodyDefaultsIn"]
	if body == nil {
		t.Fatalf("no schema for the body; components are %v", keysOf(doc.Components.Schemas))
	}
	for name, want := range map[string]any{"role": "user", "limit": int64(25), "live": true, "region": "eu"} {
		if got := body.Properties[name].Default; got != want {
			t.Errorf("%s default = %#v, want %#v", name, got, want)
		}
	}
	if got := body.Properties["detail"].Properties["mode"]; got != nil && got.Default != nil {
		t.Errorf("a nested member is documented with the default %v that nothing applies", got.Default)
	}
}

func TestABodyDefaultThatIsNotItsTypeRefusesTheRoute(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, in struct {
		Limit int `json:"limit" default:"many"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	err := app.Build()
	if err == nil || !strings.Contains(err.Error(), `default "many"`) {
		t.Errorf("Build = %v, want the default named", err)
	}
}
