package muzak

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// enforcedIn has one member of each kind the document has to tell apart: two a
// rule refuses to be without, a pointer among them, and two nothing does.
type enforcedIn struct {
	Name    string  `json:"name"`
	Note    string  `json:"note"`
	Nick    *string `json:"nick"`
	Comment *string `json:"comment,omitempty"`
}

func (in *enforcedIn) Validate(v *Validation) {
	v.String(&in.Name).Required()
	v.String(&in.Nick).Required()
}

// TestOpenAPIRequiresOnlyWhatTheRuntimeRefuses holds the document to the
// binder: a member is listed required exactly when a body without it fails.
func TestOpenAPIRequiresOnlyWhatTheRuntimeRefuses(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/e", func(ctx *Context, in enforcedIn) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	// Whatever the document says is required must fail when it is left out,
	// and whatever it leaves out must be accepted.
	assertStatus(t, do(t, app, "POST", "/e", `{"name":"a","nick":"b"}`), http.StatusOK)
	assertStatus(t, do(t, app, "POST", "/e", `{"nick":"b","note":"x"}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/e", `{"name":"a"}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/e", `{"name":"a","nick":null}`), http.StatusUnprocessableEntity)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	schema := doc.Components.Schemas["enforcedIn"]
	if schema == nil {
		t.Fatalf("no schema; components are %v", keysOf(doc.Components.Schemas))
	}
	if !slices.Equal(schema.Required, []string{"name", "nick"}) {
		t.Errorf("required = %v, want [name nick]", schema.Required)
	}
	// Required refuses null in a pointer, so the member is not nullable, while
	// a pointer nothing requires still says it may be null.
	if schema.Properties["nick"].Type != "string" {
		t.Errorf("nick is described as %+v, want a plain string: null is refused", schema.Properties["nick"])
	}
	if types, _ := schema.Properties["comment"].Type.([]string); !slices.Contains(types, "null") {
		t.Errorf("comment is described as %+v, want it nullable", schema.Properties["comment"])
	}
	assertStatus(t, do(t, app, "POST", "/e", `{"name":"a","nick":"b","comment":null}`), http.StatusOK)
}

// echoed is a type that is both what a request carries and what a response
// does, which is where the two readings of "required" meet.
type echoed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (in *echoed) Validate(v *Validation) {
	v.String(&in.Name).Required()
}

// TestOpenAPIRequestBodyDoesNotLoosenTheResponse checks that describing a body
// by what is refused leaves alone what a response always carries.
func TestOpenAPIRequestBodyDoesNotLoosenTheResponse(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/echo", func(ctx *Context, in echoed) (echoed, error) { return in, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	op := doc.Paths["/echo"].Post

	response := op.Responses["200"].Content["application/json"].Schema
	if response.Ref != componentPrefix+"echoed" {
		t.Fatalf("the response is %+v, want a reference to echoed", response)
	}
	if got := doc.Components.Schemas["echoed"].Required; !slices.Equal(got, []string{"id", "name"}) {
		t.Errorf("the response type's required = %v, want [id name]: a response always carries both", got)
	}

	request := op.RequestBody.Content["application/json"].Schema
	if request.Ref != componentPrefix+"echoedInput" {
		t.Fatalf("the request body is %+v, want the type's own request schema", request)
	}
	if got := doc.Components.Schemas["echoedInput"].Required; !slices.Equal(got, []string{"name"}) {
		t.Errorf("the request schema's required = %v, want [name]: only Name is refused when absent", got)
	}
	if doc.Components.Schemas["echoedInput"].Properties["name"] == nil {
		t.Error("the request schema lost its members")
	}
}

// TestOpenAPIRequestOnlyTypeKeepsItsName checks that a type only requests use is
// not given a second schema.
func TestOpenAPIRequestOnlyTypeKeepsItsName(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/a", func(ctx *Context, in enforcedIn) (Empty, error) { return Empty{}, nil })
	app.Put("/b", func(ctx *Context, in enforcedIn) (Empty, error) { return Empty{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/a", "/b"} {
		var op *Operation
		if path == "/a" {
			op = doc.Paths[path].Post
		} else {
			op = doc.Paths[path].Put
		}
		if ref := op.RequestBody.Content["application/json"].Schema.Ref; ref != componentPrefix+"enforcedIn" {
			t.Errorf("%s body = %q", path, ref)
		}
	}
	for name := range doc.Components.Schemas {
		if strings.HasSuffix(name, "Input") {
			t.Errorf("unexpected request copy %q", name)
		}
	}
}

// nestedRequired is a body with a model nested in it that the rules speak for.
type nestedRequiredIn struct {
	Owner nestedOwner `json:"owner"`
}

type nestedOwner struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (in *nestedRequiredIn) Validate(v *Validation) { v.Nested(&in.Owner) }

func (o *nestedOwner) Validate(v *Validation) {
	v.String(&o.Name).Required()
}

func TestOpenAPINestedModelRequiresOnlyWhatItsRulesRefuse(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/n", func(ctx *Context, in nestedRequiredIn) (Empty, error) { return Empty{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Components.Schemas["nestedOwner"].Required; !slices.Equal(got, []string{"name"}) {
		t.Errorf("required = %v, want [name]", got)
	}
}
