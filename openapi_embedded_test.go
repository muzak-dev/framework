package muzak

import (
	"net/http"
	"slices"
	"strings"
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

type namedPage[T any] struct {
	Items []T `json:"items"`
}

type namedThing struct {
	ID string `json:"id"`
}

// A generic type is named with the import path of its arguments, and a
// reference to a component whose key holds a slash or a bracket does not
// resolve.
func TestAGenericTypeGetsAComponentKeyAReferenceCanName(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/things", func(ctx *Context, _ Empty) (namedPage[namedThing], error) { return namedPage[namedThing]{}, nil })
	mustBuild(t, app)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Components.Schemas["namedPage_framework.namedThing"]; !ok {
		t.Errorf("components = %v, want the generic type named namedPage_framework.namedThing", keysOf(doc.Components.Schemas))
	}
	for name := range doc.Components.Schemas {
		if strings.ContainsAny(name, "/[] ") {
			t.Errorf("component key %q cannot be the target of a reference", name)
		}
	}
	response := doc.Paths["/things"].Get.Responses["200"].Content["application/json"].Schema
	if response == nil || strings.ContainsAny(response.Ref, "[] ") || strings.Count(response.Ref, "/") != 3 {
		t.Errorf("response = %+v, want a reference whose only slashes are the pointer's", response)
	}
}
