package muzak

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// These tests cover how references are followed: components renamed, split
// into a request copy, shared, recursive, narrowed beside a reference, and
// documents Muzak itself generates.

type compatItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (in *compatItem) Validate(v *Validation) { v.String(&in.Name).Required() }

type compatPricedItem struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

func (in *compatPricedItem) Validate(v *Validation) { v.String(&in.Name).Required() }

type compatCreated struct {
	ID string `json:"id"`
}

type compatItemID struct {
	ID string `path:"id"`
}

type compatList struct {
	Limit  int    `query:"limit"`
	Cursor string `query:"cursor" required:"true"`
}

type compatShortList struct {
	Limit int `query:"limit"`
}

// compatItemApp is an application whose item type is read by POST /items and,
// when read is set, also answered by GET /items/{id}, which is what splits it
// into a response component and a request copy.
func compatItemApp[T any](t *testing.T, read bool) *Document {
	t.Helper()
	app := New(quietOptions())
	app.Post("/items", func(*Context, T) (compatCreated, error) { return compatCreated{}, nil })
	if read {
		app.Get("/items/{id}", func(*Context, compatItemID) (T, error) { var out T; return out, nil })
	}
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestRequestCopyOfAComponent covers the Item and ItemInput split: once a
// response also uses a type, the request refers to a copy named for it with
// Input after, which is the same schema under another name.
func TestRequestCopyOfAComponent(t *testing.T) {
	t.Parallel()
	requestOnly := compatItemApp[compatItem](t, false)
	both := compatItemApp[compatItem](t, true)
	if both.Components.Schemas["compatItemInput"] == nil {
		t.Fatalf("expected a request copy of compatItem, got %v", both.Components.Schemas)
	}
	assertAPIChanges(t, CompareDocuments(requestOnly, both),
		wantChange{Compatible, "path-added", "/paths/~1items~1{id}"},
		wantChange{Compatible, "request-schema-renamed", "/components/schemas/compatItem"},
	)
	assertAPIChanges(t, CompareDocuments(both, requestOnly),
		wantChange{Breaking, "path-removed", "/paths/~1items~1{id}"},
		wantChange{Compatible, "request-schema-renamed", "/components/schemas/compatItemInput"},
	)
}

// TestMemberOfATypeUsedBothWays checks that a member added to a type both
// directions use is reported for each, where each reads it, and that removing
// it again breaks both.
func TestMemberOfATypeUsedBothWays(t *testing.T) {
	t.Parallel()
	before := compatItemApp[compatItem](t, true)
	after := compatItemApp[compatPricedItem](t, true)
	assertAPIChanges(t, CompareDocuments(before, after),
		wantChange{Compatible, "request-schema-renamed", "/components/schemas/compatItemInput"},
		wantChange{Compatible, "response-schema-renamed", "/components/schemas/compatItem"},
		wantChange{Compatible, "request-property-added", "/components/schemas/compatItemInput/properties/price"},
		wantChange{Compatible, "response-property-added", "/components/schemas/compatItem/properties/price"},
	)
	assertAPIChanges(t, CompareDocuments(after, before),
		wantChange{Compatible, "request-schema-renamed", "/components/schemas/compatPricedItemInput"},
		wantChange{Compatible, "response-schema-renamed", "/components/schemas/compatPricedItem"},
		wantChange{Breaking, "request-property-removed", "/components/schemas/compatPricedItemInput/properties/price"},
		wantChange{Breaking, "response-property-removed", "/components/schemas/compatPricedItem/properties/price"},
	)
}

// TestRequiredQueryParameterAdded drives the parameter rule through real
// applications.
func TestRequiredQueryParameterAdded(t *testing.T) {
	t.Parallel()
	build := func(cursor bool) *Document {
		app := New(quietOptions())
		if cursor {
			app.Get("/items", func(*Context, compatList) (compatCreated, error) { return compatCreated{}, nil })
		} else {
			app.Get("/items", func(*Context, compatShortList) (compatCreated, error) { return compatCreated{}, nil })
		}
		doc, err := app.Document()
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	assertAPIChanges(t, CompareDocuments(build(false), build(true)),
		wantChange{Breaking, "parameter-added", "/paths/~1items/get/parameters/query/cursor"})
}

// sharedDoc describes two operations reading Item and two answering it.
func sharedDoc(item *Schema) *Document {
	body := func() *Schema { return &Schema{Ref: componentPrefix + "Item"} }
	writer := func(id string) *Operation {
		return &Operation{OperationID: id, RequestBody: &RequestBody{Content: map[string]MediaType{"application/json": {Schema: body()}}},
			Responses: map[string]*Response{"204": {}}}
	}
	reader := func(id string) *Operation {
		return &Operation{OperationID: id, Responses: map[string]*Response{"200": {Content: map[string]MediaType{"application/json": {Schema: body()}}}}}
	}
	return compatDoc(map[string]*PathItem{
		"/a": {Post: writer("a"), Get: reader("ga")},
		"/b": {Put: writer("b"), Get: reader("gb")},
	}, map[string]*Schema{"Item": item})
}

// TestSharedComponentIsReportedOnce checks that a change to a component every
// operation reaches is reported once per direction, in components.
func TestSharedComponentIsReportedOnce(t *testing.T) {
	t.Parallel()
	old := sharedDoc(compatObj(map[string]*Schema{"a": compatStr(), "b": compatStr()}, "a", "b"))
	cur := sharedDoc(compatObj(map[string]*Schema{"a": compatStr()}, "a"))
	assertAPIChanges(t, CompareDocuments(old, cur),
		wantChange{PossiblyBreaking, "request-property-removed", "/components/schemas/Item/properties/b"},
		wantChange{Breaking, "response-property-removed", "/components/schemas/Item/properties/b"},
	)
}

// TestRenamedComponentWithTheSameShape checks that a rename alone is
// compatible, and that a change made with it is still found.
func TestRenamedComponentWithTheSameShape(t *testing.T) {
	t.Parallel()
	shape := func() *Schema { return compatObj(map[string]*Schema{"a": compatStr()}, "a") }
	old := compatReadDoc(&Schema{Ref: componentPrefix + "Item"}, map[string]*Schema{"Item": shape()})
	cur := compatReadDoc(&Schema{Ref: componentPrefix + "Thing", Description: "Renamed."}, map[string]*Schema{"Thing": shape()})
	got := CompareDocuments(old, cur)
	assertAPIChanges(t, got, wantChange{Compatible, "request-schema-renamed", "/components/schemas/Item"})
	if !strings.Contains(got[0].Message, `"Item" is now "Thing"`) {
		t.Errorf("message = %q", got[0].Message)
	}

	changed := shape()
	changed.Properties["b"] = compatStr()
	changed.Required = append(changed.Required, "b")
	assertAPIChanges(t, CompareDocuments(old, compatReadDoc(&Schema{Ref: componentPrefix + "Thing"}, map[string]*Schema{"Thing": changed})),
		wantChange{Compatible, "request-schema-renamed", "/components/schemas/Item"},
		wantChange{Breaking, "request-property-added", "/components/schemas/Item/properties/b"},
	)
}

// TestRecursiveComponent checks that a type that holds itself is compared
// once, and a change to it found.
func TestRecursiveComponent(t *testing.T) {
	t.Parallel()
	node := func(required ...string) map[string]*Schema {
		return map[string]*Schema{"Node": compatObj(map[string]*Schema{
			"name":     compatStr(),
			"children": {Type: "array", Items: &Schema{Ref: componentPrefix + "Node"}},
			"parent":   {AnyOf: []*Schema{{Ref: componentPrefix + "Node"}, {Type: "null"}}},
		}, required...)}
	}
	body := &Schema{Ref: componentPrefix + "Node"}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(body, node()), compatReadDoc(body, node())))
	assertAPIChanges(t, CompareDocuments(compatReadDoc(body, node()), compatReadDoc(body, node("name"))),
		wantChange{Breaking, "request-property-became-required", "/components/schemas/Node/properties/name"})
}

// TestReferenceAgainstInlineSchema checks that a type described inline in one
// document and by reference in the other is compared by what it says.
func TestReferenceAgainstInlineSchema(t *testing.T) {
	t.Parallel()
	shape := func() *Schema { return compatObj(map[string]*Schema{"a": compatStr()}) }
	byRef := compatReadDoc(&Schema{Ref: componentPrefix + "Item"}, map[string]*Schema{"Item": shape()})
	inline := compatReadDoc(shape(), nil)
	assertAPIChanges(t, CompareDocuments(byRef, inline))
	assertAPIChanges(t, CompareDocuments(inline, byRef))
	wider := compatObj(map[string]*Schema{"a": {Type: []string{"string", "null"}}})
	assertAPIChanges(t, CompareDocuments(byRef, compatReadDoc(wider, nil)),
		wantChange{Compatible, "request-nullable-added", requestBodyAt + "/properties/a"})
}

// TestNullableReference covers the way a pointer to a named type is written:
// a choice between the reference and null, in either order.
func TestNullableReference(t *testing.T) {
	t.Parallel()
	schemas := func() map[string]*Schema {
		return map[string]*Schema{"Owner": compatObj(map[string]*Schema{"name": compatStr()})}
	}
	member := func(s *Schema) *Schema { return compatObj(map[string]*Schema{"owner": s}, "owner") }
	plain := &Schema{Ref: componentPrefix + "Owner"}
	nullFirst := &Schema{AnyOf: []*Schema{{Type: []string{"null"}}, {Ref: componentPrefix + "Owner"}}, Description: "May be null."}
	nullSecond := &Schema{AnyOf: []*Schema{{Ref: componentPrefix + "Owner"}, {Type: "null"}}}
	assertAPIChanges(t, CompareDocuments(compatAnswerDoc(member(plain), schemas()), compatAnswerDoc(member(nullSecond), schemas())),
		wantChange{Breaking, "response-nullable-added", responseBodyAt + "/properties/owner"})
	assertAPIChanges(t, CompareDocuments(compatAnswerDoc(member(nullSecond), schemas()), compatAnswerDoc(member(nullFirst), schemas())))

	// A choice that also constrains is compared by what it says, not as a
	// reference.
	constrained := &Schema{AnyOf: []*Schema{{Ref: componentPrefix + "Owner"}, {Type: "null"}}, MinLength: compatPtr(1)}
	assertAPIChanges(t, CompareDocuments(compatAnswerDoc(member(nullSecond), schemas()), compatAnswerDoc(member(constrained), schemas())),
		wantChange{Compatible, "response-min-length-tightened", responseBodyAt + "/properties/owner/minLength"})
	odd := &Schema{Ref: componentPrefix + "Owner", AnyOf: []*Schema{{Ref: componentPrefix + "Owner"}, {Type: "null"}}}
	assertAPIChanges(t, CompareDocuments(compatAnswerDoc(member(nullSecond), schemas()), compatAnswerDoc(member(odd), schemas())),
		wantChange{Compatible, "response-nullable-removed", responseBodyAt + "/properties/owner"})
	// A choice between two schemas neither of which is null alone is a
	// choice like any other.
	notNull := &Schema{AnyOf: []*Schema{{Ref: componentPrefix + "Owner"}, {Type: "null", MinLength: compatPtr(1)}}}
	if _, wrapped := nullableCore(notNull); wrapped {
		t.Error("a null alternative that also constrains should not read as a nullable wrapper")
	}
	if isNullOnly(nil) {
		t.Error("isNullOnly(nil) = true")
	}
}

// TestRuleBesideAReference covers the narrowing Muzak writes beside a
// reference for one use of a type: it is compared as the constraint it is.
func TestRuleBesideAReference(t *testing.T) {
	t.Parallel()
	schemas := func() map[string]*Schema {
		return map[string]*Schema{"Money": {Type: "object", AdditionalProperties: false, Properties: map[string]*Schema{
			"amount": {Type: "number"}, "currency": {Ref: componentPrefix + "Currency"},
		}}, "Currency": compatStr()}
	}
	refund := func(price *Schema) *Schema {
		return &Schema{Type: "object", AdditionalProperties: false, Properties: map[string]*Schema{"price": price}}
	}
	plain := &Schema{Ref: componentPrefix + "Money"}
	narrowed := &Schema{Ref: componentPrefix + "Money", Properties: map[string]*Schema{"amount": {ExclusiveMinimum: compatPtr(0.0)}}, Required: []string{"amount"}}
	at := requestBodyAt + "/properties/price/properties/amount"
	assertAPIChanges(t, CompareDocuments(compatReadDoc(refund(plain), schemas()), compatReadDoc(refund(narrowed), schemas())),
		wantChange{Breaking, "request-property-became-required", at},
		wantChange{Breaking, "request-minimum-tightened", at + "/minimum"},
	)
	assertAPIChanges(t, CompareDocuments(compatReadDoc(refund(narrowed), schemas()), compatReadDoc(refund(plain), schemas())),
		wantChange{Compatible, "request-property-became-optional", at},
		wantChange{Compatible, "request-minimum-relaxed", at + "/minimum"},
	)
}

// TestUnresolvableReferences covers what only a Document built in Go can hold,
// since ReadDocument refuses it: a reference to nothing, or to another file,
// says nothing about the value.
func TestUnresolvableReferences(t *testing.T) {
	t.Parallel()
	missing := compatReadDoc(&Schema{Ref: componentPrefix + "Missing"}, nil)
	external := compatReadDoc(&Schema{Ref: "other.json#/components/schemas/Item"}, map[string]*Schema{"Item": compatStr()})
	assertAPIChanges(t, CompareDocuments(missing, external))
	assertAPIChanges(t, CompareDocuments(missing, compatReadDoc(compatStr(), nil)),
		wantChange{Breaking, "request-type-narrowed", requestBodyAt + "/type"},
		wantChange{Breaking, "request-nullable-removed", requestBodyAt},
	)
}

// TestReferenceCycles checks that components which refer to each other
// without ever describing a value terminate.
func TestReferenceCycles(t *testing.T) {
	t.Parallel()
	aliases := func() map[string]*Schema {
		return map[string]*Schema{
			"A": {Ref: componentPrefix + "B"},
			"B": {Ref: componentPrefix + "A"},
			"C": {AllOf: []*Schema{{Ref: componentPrefix + "C"}}, Type: "object"},
		}
	}
	body := compatObj(map[string]*Schema{"a": {Ref: componentPrefix + "A"}, "c": {Ref: componentPrefix + "C"}})
	assertAPIChanges(t, CompareDocuments(compatReadDoc(body, aliases()), compatReadDoc(body, aliases())))
}

// TestPointerCycleInAGoDocument covers a Document built in Go whose schemas
// hold themselves through a pointer, which JSON cannot express: the
// comparison stops at its depth bound and says it is incomplete rather than
// recursing for ever.
func TestPointerCycleInAGoDocument(t *testing.T) {
	t.Parallel()
	loop := func() *Schema {
		s := &Schema{Type: "array"}
		s.Items = s
		return s
	}
	got := CompareDocuments(compatReadDoc(loop(), nil), compatReadDoc(loop(), nil))
	assertAPIChanges(t, got, wantChange{Breaking, "comparison-incomplete", ""})
	// One operation, one response and one schema, however often it holds itself.
	if countDocumentNodes(compatReadDoc(loop(), nil)) != 3 {
		t.Errorf("countDocumentNodes counts a cycle more than once")
	}
}

// TestPointerCycleThroughAnyOfInAGoDocument covers a Document built in Go
// whose union holds itself as an alternative. Whether it admits null is asked
// of every alternative in turn, and that question must end on a cycle too,
// rather than recurse until the stack is exhausted, which kills the process.
func TestPointerCycleThroughAnyOfInAGoDocument(t *testing.T) {
	t.Parallel()
	loop := func() *Schema {
		s := &Schema{AnyOf: []*Schema{nil, compatStr()}}
		s.AnyOf[0] = s
		return s
	}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(loop(), nil), compatReadDoc(loop(), nil)),
		wantChange{Breaking, "comparison-incomplete", ""})
	nested := func() *Schema {
		s := &Schema{AnyOf: []*Schema{compatStr(), nil}}
		s.AnyOf[1] = &Schema{AnyOf: []*Schema{s, {Type: "integer"}}}
		return s
	}
	assertAPIChanges(t, CompareDocuments(compatAnswerDoc(nested(), nil), compatAnswerDoc(nested(), nil)),
		wantChange{Breaking, "comparison-incomplete", ""})
}

type compatMoney struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type compatOwner struct {
	Name string `json:"name"`
}

type compatOrder struct {
	ID       string         `json:"id"`
	Status   string         `json:"status"`
	Price    compatMoney    `json:"price"`
	Note     *string        `json:"note,omitempty"`
	Tags     []string       `json:"tags"`
	Labels   map[string]int `json:"labels"`
	Digest   [4]byte        `json:"digest"`
	Blob     []byte         `json:"blob"`
	Owner    *compatOwner   `json:"owner"`
	Children []compatOrder  `json:"children"`
	Limit    int            `json:"limit" default:"25"`
	Created  time.Time      `json:"created"`
	Extra    any            `json:"extra"`
}

func (in *compatOrder) Validate(v *Validation) {
	v.String(&in.Status).Required().OneOf("open", "closed")
	v.String(&in.ID).Matches(`^[a-z]+$`)
	v.String(&in.ID).Matches(`^.{3,}$`)
	v.Number(&in.Price.Amount).Positive()
	v.Slice(&in.Tags).MaxItems(5).Unique()
	v.String(&in.Note).MaxLen(100)
}

type compatForm struct {
	Title string `form:"title"`
	Count int    `form:"count" default:"1"`
}

type compatSearch struct {
	Query  string     `query:"q" required:"true"`
	Kinds  []string   `query:"kind"`
	Before *time.Time `query:"before"`
	Token  string     `header:"X-Token"`
	Order  string     `path:"order"`
}

// richDocument generates a document exercising most of what Muzak writes:
// enums, defaults, nullable members, maps, byte arrays, a type used both ways,
// a rule beside a reference, several patterns, forms, security and versions.
// changed varies a few of them, so that two documents differ.
func richDocument(t testing.TB, changed bool) *Document {
	t.Helper()
	opts := quietOptions()
	opts.SecuritySchemes = map[string]SecurityScheme{"bearer": BearerAuth("JWT")}
	opts.Servers = []Server{{URL: "https://api.example"}}
	app := New(opts)
	app.Post("/orders", func(*Context, compatOrder) (compatOrder, error) { return compatOrder{}, nil },
		Status(201), WithSecurity(Require("bearer")), WithTags("orders"))
	app.Put("/orders/{order}/loose", func(*Context, compatOrder) (Empty, error) { return Empty{}, nil }, AllowUnknownFields())
	app.Post("/forms", func(*Context, compatForm) (HTML, error) { return "", nil })
	app.Get("/search/{order}", func(*Context, compatSearch) ([]compatOrder, error) { return nil, nil }, Deprecated())
	if changed {
		app.Get("/owners", func(*Context, Empty) (map[string]compatOwner, error) { return nil, nil }, Public())
	}
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func newIntegrationDocument(t testing.TB) *Document {
	t.Helper()
	doc, err := newIntegrationApp().Document()
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestGeneratedDocumentsRoundTrip checks that a document Muzak generates is
// read back as the same document: compared with it in either direction there
// is no change, and it is written out again byte for byte.
func TestGeneratedDocumentsRoundTrip(t *testing.T) {
	t.Parallel()
	for i, doc := range []*Document{newIntegrationDocument(t), richDocument(t, false), richDocument(t, true)} {
		data, err := doc.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		read, err := ReadDocument(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("document %d: ReadDocument = %v", i, err)
		}
		assertAPIChanges(t, CompareDocuments(doc, read))
		assertAPIChanges(t, CompareDocuments(read, doc))
		again, err := read.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, data) {
			t.Errorf("document %d is not written out again as it was read", i)
		}
	}
	assertAPIChanges(t, CompareDocuments(richDocument(t, false), richDocument(t, true)),
		wantChange{Compatible, "path-added", "/paths/~1owners"})
}
