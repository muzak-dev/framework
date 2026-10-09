package muzak

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// requestOf returns the schema a route's JSON body is described with, followed
// to the component a reference names.
func requestOf(t *testing.T, doc *Document, op *Operation) *Schema {
	t.Helper()
	if op == nil || op.RequestBody == nil {
		t.Fatal("the operation describes no request body")
	}
	return componentOf(t, doc, op.RequestBody.Content["application/json"].Schema)
}

// responseOf returns the schema of a route's 200 response, followed to its
// component.
func responseOf(t *testing.T, doc *Document, op *Operation) *Schema {
	t.Helper()
	return componentOf(t, doc, op.Responses["200"].Content["application/json"].Schema)
}

// componentOf follows a schema to the component it references, if it does.
func componentOf(t *testing.T, doc *Document, schema *Schema) *Schema {
	t.Helper()
	if schema == nil {
		t.Fatal("no schema")
	}
	if schema.Ref == "" {
		return schema
	}
	component := doc.Components.Schemas[strings.TrimPrefix(schema.Ref, componentPrefix)]
	if component == nil {
		t.Fatalf("%s names no component; components are %v", schema.Ref, keysOf(doc.Components.Schemas))
	}
	return component
}

// propertyRef returns the component a member refers to, looking through the
// widening a pointer gets and the elements of a collection.
func propertyRef(t *testing.T, doc *Document, object *Schema, member string) *Schema {
	t.Helper()
	property := object.Properties[member]
	if property == nil {
		t.Fatalf("no member %q among %v", member, keysOf(object.Properties))
	}
	for property.Ref == "" {
		switch {
		case property.Items != nil:
			property = property.Items
		case len(property.AnyOf) > 0:
			property = property.AnyOf[0]
		default:
			t.Fatalf("member %q is %+v, which refers to no component", member, property)
		}
	}
	return componentOf(t, doc, property)
}

type requestLine struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type requestParty struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type requestRuledParty struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (p *requestRuledParty) Validate(v *Validation) { v.String(&p.Name).Required() }

// requestOrder nests a type only requests use, one a response also uses, and
// a model whose rules require one of its members, which a response uses too.
type requestOrder struct {
	Line   requestLine       `json:"line"`
	Lines  []requestLine     `json:"lines"`
	Party  *requestParty     `json:"party"`
	Signer requestRuledParty `json:"signer"`
}

func (in *requestOrder) Validate(v *Validation) { v.Nested(&in.Signer) }

// TestOpenAPINestedRequestTypesRequireOnlyWhatIsEnforced holds every object a
// body nests to the rule the top level follows: a member is required only when
// a rule refuses the body without it. The nested types kept the shape a
// response has, so a client was told to send members the server never asked
// for, and one a response shares has a copy of its own for the request.
func TestOpenAPINestedRequestTypesRequireOnlyWhatIsEnforced(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/orders", func(ctx *Context, in requestOrder) (Empty, error) { return Empty{}, nil })
	app.Get("/party", func(ctx *Context, _ Empty) (requestParty, error) { return requestParty{}, nil })
	app.Get("/signer", func(ctx *Context, _ Empty) (requestRuledParty, error) { return requestRuledParty{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "POST", "/orders", `{"line":{},"lines":[{}],"party":{},"signer":{"name":"a"}}`), http.StatusOK)
	assertStatus(t, do(t, app, "POST", "/orders", `{"signer":{}}`), http.StatusUnprocessableEntity)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	order := requestOf(t, doc, doc.Paths["/orders"].Post)
	for _, member := range []string{"line", "lines", "party"} {
		if nested := propertyRef(t, doc, order, member); len(nested.Required) != 0 {
			t.Errorf("%s: required = %v, want none: nothing refuses a body that leaves them out", member, nested.Required)
		}
	}
	if signer := propertyRef(t, doc, order, "signer"); !slices.Equal(signer.Required, []string{"name"}) {
		t.Errorf("signer: required = %v, want [name], as its rules say", signer.Required)
	}

	// The responses that share the types still say what they always write.
	if got := responseOf(t, doc, doc.Paths["/party"].Get).Required; !slices.Equal(got, []string{"email", "name"}) {
		t.Errorf("the party response requires %v, want [email name]", got)
	}
	if got := responseOf(t, doc, doc.Paths["/signer"].Get).Required; !slices.Equal(got, []string{"email", "name"}) {
		t.Errorf("the signer response requires %v, want [email name]", got)
	}
}

// closedBody nests an object, a collection of them and an anonymous struct,
// each of which the decoder refuses an unknown member in.
type closedBody struct {
	Line  requestLine   `json:"line"`
	Lines []requestLine `json:"lines"`
	Note  struct {
		Text string `json:"text"`
	} `json:"note"`
}

// mixedClosedBody reads one field from the path and the rest from the body.
type mixedClosedBody struct {
	ID   string `path:"id"`
	Name string `json:"name"`
}

// openBody collects whatever members it does not name in an embedded map.
type openBody struct {
	Name  string         `json:"name"`
	Extra map[string]any `json:",embed"`
}

// selfDecoded reads its own JSON, so what it accepts is up to it.
type selfDecoded struct {
	Name string `json:"name"`
}

func (s *selfDecoded) UnmarshalJSON(data []byte) error {
	s.Name = string(data)
	return nil
}

// TestOpenAPIRequestObjectsRefuseUnknownMembers holds the document to the
// decoder, which refuses a member it does not know at any depth unless the
// route allows them. The request schemas said nothing about it, so a client
// generated from them, or a gateway validating against them, let through what
// the server answers with a 422. A response is left open, since a client must
// be free to read a response that gains a member.
func TestOpenAPIRequestObjectsRefuseUnknownMembers(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/closed", func(ctx *Context, in closedBody) (Empty, error) { return Empty{}, nil })
	app.Post("/mixed/{id}", func(ctx *Context, in mixedClosedBody) (Empty, error) { return Empty{}, nil })
	app.Post("/open", func(ctx *Context, in openBody) (Empty, error) { return Empty{}, nil })
	app.Post("/allowed", func(ctx *Context, in closedBody) (Empty, error) { return Empty{}, nil }, AllowUnknownFields())
	app.Post("/self", func(ctx *Context, in selfDecoded) (Empty, error) { return Empty{}, nil })
	app.Get("/line", func(ctx *Context, _ Empty) (requestLine, error) { return requestLine{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "POST", "/self", `{"other":1}`), http.StatusOK)
	assertStatus(t, do(t, app, "POST", "/closed", `{"other":1}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/closed", `{"line":{"other":1}}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/closed", `{"note":{"other":1}}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/mixed/1", `{"other":1}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/open", `{"other":1}`), http.StatusOK)
	assertStatus(t, do(t, app, "POST", "/allowed", `{"other":1,"line":{"other":1}}`), http.StatusOK)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	closed := requestOf(t, doc, doc.Paths["/closed"].Post)
	for name, object := range map[string]*Schema{
		"the body":  closed,
		"line":      propertyRef(t, doc, closed, "line"),
		"lines":     propertyRef(t, doc, closed, "lines"),
		"note":      closed.Properties["note"],
		"the mixed": requestOf(t, doc, doc.Paths["/mixed/{id}"].Post),
	} {
		if object.AdditionalProperties != false {
			t.Errorf("%s: additionalProperties = %v, want false", name, object.AdditionalProperties)
		}
	}
	mixed := requestOf(t, doc, doc.Paths["/mixed/{id}"].Post)
	if _, located := mixed.Properties["ID"]; located || len(mixed.Properties) != 1 {
		t.Errorf("the mixed body describes %v, want only name: the path parameter is not a member", keysOf(mixed.Properties))
	}

	allowed := requestOf(t, doc, doc.Paths["/allowed"].Post)
	if allowed.AdditionalProperties != nil || propertyRef(t, doc, allowed, "line").AdditionalProperties != nil {
		t.Errorf("a route that allows unknown members describes them as refused: %+v", allowed)
	}
	if extra, ok := requestOf(t, doc, doc.Paths["/open"].Post).AdditionalProperties.(*Schema); !ok || extra == nil {
		t.Errorf("an embedded map takes any member, but the body says %v", requestOf(t, doc, doc.Paths["/open"].Post).AdditionalProperties)
	}
	if line := responseOf(t, doc, doc.Paths["/line"].Get); line.AdditionalProperties != nil {
		t.Errorf("the response is closed to new members: %v", line.AdditionalProperties)
	}
	if self := requestOf(t, doc, doc.Paths["/self"].Post); self.AdditionalProperties != nil {
		t.Errorf("a type that decodes itself is described as refusing unknown members: %v", self.AdditionalProperties)
	}
}

// sparsePatch has nothing but optional members, so a response and a request
// that allows unknown members read it alike.
type sparsePatch struct {
	Name *string `json:"name"`
	Note *string `json:"note,omitempty"`
}

// requiredOwner requires a member that is a pointer to a struct.
type requiredOwner struct {
	Owner *requestParty `json:"owner"`
	*EmbeddedBase
	Lines []requestLine `json:"lines"`
}

func (in *requiredOwner) Validate(v *Validation) { v.Value(&in.Owner).Required() }

// TestOpenAPIRequestReadingEdgeCases covers a request that reads a response's
// component exactly as it stands, which then shares it, and a required pointer
// to a struct, which refuses null as a required scalar does.
func TestOpenAPIRequestReadingEdgeCases(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/sparse", func(ctx *Context, _ Empty) (sparsePatch, error) { return sparsePatch{}, nil })
	app.Patch("/sparse", func(ctx *Context, in sparsePatch) (Empty, error) { return Empty{}, nil }, AllowUnknownFields())
	app.Post("/owner", func(ctx *Context, in requiredOwner) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "POST", "/owner", `{"owner":null}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/owner", `{"owner":{},"id":"x"}`), http.StatusOK)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	patch := doc.Paths["/sparse"].Patch.RequestBody.Content["application/json"].Schema
	if response := doc.Paths["/sparse"].Get.Responses["200"].Content["application/json"].Schema; patch.Ref != response.Ref {
		t.Errorf("the patch reads %s and the response %s, though they say the same", patch.Ref, response.Ref)
	}
	owner := requestOf(t, doc, doc.Paths["/owner"].Post)
	if member := owner.Properties["owner"]; member.AnyOf != nil || member.Ref == "" || !slices.Equal(owner.Required, []string{"owner"}) {
		t.Errorf("owner = %+v (required %v), want a plain reference that is required", member, owner.Required)
	}
}

// requestTree is a body type that contains itself, which the request copy has
// to follow without describing it for ever.
type requestTree struct {
	Name     string        `json:"name"`
	Children []requestTree `json:"children"`
}

// TestOpenAPIRecursiveRequestTypeIsDescribedOnce checks that a type nesting
// itself, which a response shares, is copied for the request once, and that
// the copy's elements are the copy.
func TestOpenAPIRecursiveRequestTypeIsDescribedOnce(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/tree", func(ctx *Context, in requestTree) (requestTree, error) { return in, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	op := doc.Paths["/tree"].Post
	request := op.RequestBody.Content["application/json"].Schema
	response := op.Responses["200"].Content["application/json"].Schema
	if request.Ref == response.Ref {
		t.Fatalf("the request and the response share %s, which cannot say both what is written and what is required", request.Ref)
	}
	tree := componentOf(t, doc, request)
	if len(tree.Required) != 0 {
		t.Errorf("required = %v, want none", tree.Required)
	}
	if items := tree.Properties["children"].Items; items == nil || items.Ref != request.Ref {
		t.Errorf("children = %+v, want the elements to be the request copy %s", tree.Properties["children"], request.Ref)
	}
	if got := componentOf(t, doc, response).Required; !slices.Equal(got, []string{"children", "name"}) {
		t.Errorf("the response requires %v, want [children name]", got)
	}
}

// TestOpenAPIRoutesShareARequestCopyOnlyWhenTheyReadItAlike checks that routes
// reading a type the same way share one copy of it, and that a route reading
// it differently, here because it skips the rules, gets one of its own rather
// than rewriting the other's.
func TestOpenAPIRoutesShareARequestCopyOnlyWhenTheyReadItAlike(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/signer", func(ctx *Context, _ Empty) (requestRuledParty, error) { return requestRuledParty{}, nil })
	app.Post("/a", func(ctx *Context, in requestRuledParty) (Empty, error) { return Empty{}, nil })
	app.Post("/b", func(ctx *Context, in requestRuledParty) (Empty, error) { return Empty{}, nil }, SkipValidation())
	app.Put("/c", func(ctx *Context, in requestRuledParty) (Empty, error) { return Empty{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	refOf := func(op *Operation) string { return op.RequestBody.Content["application/json"].Schema.Ref }
	a, b, c := refOf(doc.Paths["/a"].Post), refOf(doc.Paths["/b"].Post), refOf(doc.Paths["/c"].Put)
	if a != c {
		t.Errorf("/a reads %s and /c %s, want the one copy they agree on", a, c)
	}
	if a == b {
		t.Errorf("/a and /b both read %s, though only /a requires name", a)
	}
	if got := requestOf(t, doc, doc.Paths["/a"].Post).Required; !slices.Equal(got, []string{"name"}) {
		t.Errorf("/a requires %v, want [name]", got)
	}
	if got := requestOf(t, doc, doc.Paths["/b"].Post).Required; len(got) != 0 {
		t.Errorf("/b requires %v, want nothing: it runs no rules", got)
	}
}
