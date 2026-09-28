package muzak

import (
	"net/http"
	"slices"
	"testing"
)

type EmbeddedBase struct {
	ID string `json:"id"`
}

type embedsAPointer struct {
	*EmbeddedBase
	Name string `json:"name"`
}

// The decoder inlines an embedded pointer's members, so that is where the
// document puts them, optional because the pointer may stay nil.
func TestAnEmbeddedPointerIsPromotedInTheDocument(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/e", func(ctx *Context, in embedsAPointer) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	assertStatus(t, do(t, app, "POST", "/e", `{"id":"1","name":"a"}`), http.StatusOK)
	assertStatus(t, do(t, app, "POST", "/e", `{"EmbeddedBase":{"id":"1"},"name":"a"}`), http.StatusUnprocessableEntity)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	schema := doc.Components.Schemas["embedsAPointer"]
	if schema == nil {
		t.Fatalf("no schema; components are %v", keysOf(doc.Components.Schemas))
	}
	if _, promoted := schema.Properties["id"]; !promoted {
		t.Errorf("properties = %v, want the embedded pointer's members promoted", keysOf(schema.Properties))
	}
	if _, nested := schema.Properties["EmbeddedBase"]; nested {
		t.Errorf("properties = %v, want no member named for the embedded type", keysOf(schema.Properties))
	}
	if slices.Contains(schema.Required, "id") || !slices.Contains(schema.Required, "name") {
		t.Errorf("required = %v, want name only", schema.Required)
	}
}
