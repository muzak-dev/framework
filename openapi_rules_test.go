package muzak

import (
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"muzak.dev/framework/validate"
)

type ruledMoney struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type ruledOrder struct {
	Price  ruledMoney `json:"price"`
	Refund ruledMoney `json:"refund"`
	Note   struct {
		Text string `json:"text"`
	} `json:"note"`
}

func (in *ruledOrder) Validate(v *Validation) {
	// The first member of a nested struct sits where the struct does, so only
	// its type tells the two apart.
	v.Number(&in.Price.Amount).Positive()
	v.String(&in.Price.Currency).Required().Len(3)
	v.Value(&in.Price).Must(func(m ruledMoney) error {
		if m.Amount >= 1000 {
			return errors.New("must be under a thousand")
		}
		return nil
	})
	v.String(&in.Note.Text).MaxLen(5)
}

// TestRuleOnANestedMemberNamesAndDescribesThatMember holds a rule declared on a
// member of a struct the body nests to that member. A rule on the first one
// shared its address with the struct and was reported, and documented, as the
// struct's: the failure named "price", and exclusiveMinimum was written onto
// the reference every use of the type shares, refund and the responses
// included. A rule on the second one named nothing at all, and the route was
// refused when it was built.
func TestRuleOnANestedMemberNamesAndDescribesThatMember(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/orders", func(ctx *Context, in ruledOrder) (Empty, error) { return Empty{}, nil })
	app.Get("/money", func(ctx *Context, _ Empty) (ruledMoney, error) { return ruledMoney{}, nil })
	mustBuild(t, app)

	got := fieldsOf(t, app, "/orders", `{"price":{"amount":5000,"currency":"EU"},"note":{"text":"too long"}}`)
	want := []string{"price.currency", "price", "note.text"}
	if !slices.Equal(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
	if got := fieldsOf(t, app, "/orders", `{"price":{"amount":-5,"currency":"EUR"}}`); !slices.Equal(got, []string{"price.amount"}) {
		t.Errorf("fields = %q, want [price.amount]", got)
	}

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	money := responseOf(t, doc, doc.Paths["/money"].Get)
	if !reflect.DeepEqual(money, newSchemaBuilder().describeStruct(reflect.TypeFor[ruledMoney]())) {
		t.Errorf("the type's own schema carries the order's rules: %+v", money)
	}
	order := requestOf(t, doc, doc.Paths["/orders"].Post)
	price, refund := order.Properties["price"], order.Properties["refund"]
	if price.Ref == "" || price.Properties["amount"] == nil || price.Properties["amount"].ExclusiveMinimum == nil {
		t.Fatalf("price = %+v, want the reference narrowed beside it to a positive amount", price)
	}
	if currency := price.Properties["currency"]; currency == nil || currency.MinLength == nil || *currency.MinLength != 3 {
		t.Errorf("price.currency = %+v, want a length of 3", currency)
	}
	if !slices.Equal(price.Required, []string{"currency"}) || !slices.Contains(order.Required, "price") {
		t.Errorf("price requires %v and the order %v, want currency required and price with it", price.Required, order.Required)
	}
	if price.ExclusiveMinimum != nil || refund.ExclusiveMinimum != nil || refund.Properties != nil {
		t.Errorf("the rules reached the reference itself or the refund: price %+v, refund %+v", price, refund)
	}
	if text := order.Properties["note"].Properties["text"]; text == nil || text.MaxLength == nil || *text.MaxLength != 5 || text.Type != "string" {
		t.Errorf("note.text = %+v, want the inline member itself to carry maxLength 5", text)
	}
}

// pickedOrder holds two members of one struct type and a rule for one of them.
type pickedOrder struct {
	Price  ruledMoney `json:"price"`
	Refund ruledMoney `json:"refund"`
}

func (in *pickedOrder) Validate(v *Validation) {
	v.Value(&in.Price).OneOf(ruledMoney{Amount: 1, Currency: "EUR"})
}

// TestRuleOnAStructMemberStaysOnThatMember checks that a rule on a member whose
// type is a named struct is written on that member alone. Every member of the
// type held the same reference, so the rule was written onto all of them.
func TestRuleOnAStructMemberStaysOnThatMember(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/picked", func(ctx *Context, in pickedOrder) (Empty, error) { return Empty{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	order := requestOf(t, doc, doc.Paths["/picked"].Post)
	if price := order.Properties["price"]; price.Ref == "" || len(price.Enum) != 1 {
		t.Errorf("price = %+v, want the reference with the one value it is held to", price)
	}
	if refund := order.Properties["refund"]; refund.Enum != nil {
		t.Errorf("refund = %+v, want no rule: none was declared for it", refund)
	}
}

type overlayInner struct {
	Note *string  `json:"note"`
	Tags []string `json:"tags"`
	Kind *string  `json:"kind"`
	Deep struct {
		Level *int `json:"level"`
	} `json:"deep"`
}

// overlayOuter declares rules deep inside the structs it holds, one through a
// named type and one through an anonymous one, and one named with As() after
// a path that leads nowhere.
type overlayOuter struct {
	Inner overlayInner `json:"inner"`
	Anon  struct {
		Opt  *string  `json:"opt"`
		List []string `json:"list"`
	} `json:"anon"`
	Other string `json:"other"`
}

func (in *overlayOuter) Validate(v *Validation) {
	v.String(&in.Inner.Note).Required()
	v.Slice(&in.Inner.Tags).Each(validate.String().MaxLen(3))
	v.String(&in.Inner.Kind).OneOf("a", "b")
	v.Number(&in.Inner.Deep.Level).Min(1)
	v.String(&in.Anon.Opt).Required()
	v.Slice(&in.Anon.List).Each(validate.String().MaxLen(4))
	v.String(&in.Other).As("inner.bogus").MaxLen(2)
}

// TestRulesDeepInsideAMemberNarrowOnlyThatUse walks the narrowing down a path:
// beside a reference only the narrowing is written, saying again that a
// required pointer refuses null and that one which is not may still be null;
// inside an object described inline, the member itself is narrowed.
func TestRulesDeepInsideAMemberNarrowOnlyThatUse(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/overlay", func(ctx *Context, in overlayOuter) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "POST", "/overlay", `{"inner":{"note":"n","kind":null},"anon":{"opt":"o"}}`), http.StatusOK)
	if got := fieldsOf(t, app, "/overlay", `{"inner":{"note":null,"deep":{"level":0}},"anon":{"opt":"o"}}`); !slices.Equal(got, []string{"inner.note", "inner.deep.level"}) {
		t.Errorf("fields = %q, want [inner.note inner.deep.level]", got)
	}

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	outer := requestOf(t, doc, doc.Paths["/overlay"].Post)
	if !slices.Equal(outer.Required, []string{"anon", "inner"}) {
		t.Errorf("required = %v, want [anon inner]", outer.Required)
	}
	inner := outer.Properties["inner"]
	if inner.Ref == "" || !slices.Equal(inner.Required, []string{"note"}) {
		t.Fatalf("inner = %+v, want the reference, narrowed to require note", inner)
	}
	if note := inner.Properties["note"]; note.Type != "string" {
		t.Errorf("inner.note = %+v, want the type said again without null", note)
	}
	if tags := inner.Properties["tags"]; tags.Items == nil || tags.Items.MaxLength == nil || *tags.Items.MaxLength != 3 {
		t.Errorf("inner.tags = %+v, want its elements held to three characters", tags)
	}
	if kind := inner.Properties["kind"]; !slices.Equal(kind.Enum, []any{"a", "b", nil}) {
		t.Errorf("inner.kind = %+v, want a, b or null", kind)
	}
	if level := inner.Properties["deep"].Properties["level"]; level == nil || level.Minimum == nil || *level.Minimum != 1 {
		t.Errorf("inner.deep.level = %+v, want a minimum of 1", level)
	}
	if _, bogus := inner.Properties["bogus"]; bogus {
		t.Error("a rule named after a member that does not exist narrowed one into being")
	}
	anon := outer.Properties["anon"]
	if opt := anon.Properties["opt"]; opt == nil || opt.Type != "string" || !slices.Equal(anon.Required, []string{"opt"}) {
		t.Errorf("anon = %+v, want opt required and not nullable", anon)
	}
	if list := anon.Properties["list"]; list.Type != "array" || list.Items.Type != "string" || list.Items.MaxLength == nil {
		t.Errorf("anon.list = %+v, want the array of strings held to four characters", list)
	}
}

// declaredTwice declares several rule sets for each of its fields, every one of
// which the server enforces.
type declaredTwice struct {
	Code    string `json:"code"`
	Address string `json:"address"`
	Size    int    `json:"size"`
	Status  string `json:"status"`
}

func (in *declaredTwice) Validate(v *Validation) {
	v.String(&in.Code).Required().Matches(`^[A-Z]`)
	v.String(&in.Code).Matches(`[0-9]$`).MaxLen(8)
	v.String(&in.Address).IP()
	v.String(&in.Address).IPv4()
	v.Number(&in.Size).Min(1).Max(100)
	v.Number(&in.Size).Min(5).MultipleOf(5)
	v.Number(&in.Size).MultipleOf(2)
	v.String(&in.Status).OneOf("a", "b", "c")
	v.String(&in.Status).OneOf("b", "c", "d")
}

// TestEveryRuleSetForAFieldIsDocumented holds the document to every rule set a
// model declares for a field, since each is enforced. The last one used to
// replace the others wholesale, so the document lost a pattern, a format, a
// bound and even Required that the server still checked.
func TestEveryRuleSetForAFieldIsDocumented(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/twice", func(ctx *Context, in declaredTwice) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "POST", "/twice", `{"code":"A1","address":"10.0.0.1","size":10,"status":"b"}`), http.StatusOK)
	for _, body := range []string{
		`{"code":"A","size":10}`, `{"code":"1","size":10}`, `{"size":10}`, `{"code":"A1","address":"::1"}`,
		`{"code":"A1","size":15}`, `{"code":"A1","size":2}`, `{"code":"A1","status":"a"}`, `{"code":"A1","status":"d"}`,
	} {
		assertStatus(t, do(t, app, "POST", "/twice", body), http.StatusUnprocessableEntity)
	}

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	body := requestOf(t, doc, doc.Paths["/twice"].Post)
	if !slices.Equal(body.Required, []string{"code"}) {
		t.Errorf("required = %v, want [code]", body.Required)
	}
	keywords := func(schema *Schema, of func(*Schema) any) []any {
		var found []any
		if value := of(schema); value != nil {
			found = append(found, value)
		}
		for _, part := range schema.AllOf {
			if value := of(part); value != nil {
				found = append(found, value)
			}
		}
		return found
	}
	pattern := func(s *Schema) any {
		if s.Pattern == "" {
			return nil
		}
		return s.Pattern
	}
	format := func(s *Schema) any {
		if s.Format == "" {
			return nil
		}
		return s.Format
	}
	multiple := func(s *Schema) any {
		if s.MultipleOf == nil {
			return nil
		}
		return *s.MultipleOf
	}
	code := body.Properties["code"]
	if got := keywords(code, pattern); !slices.Equal(got, []any{"^[A-Z]", "[0-9]$"}) || code.MaxLength == nil || *code.MaxLength != 8 {
		t.Errorf("code = %+v (patterns %v), want both patterns and a maxLength of 8", code, got)
	}
	if got := keywords(body.Properties["address"], format); !slices.Equal(got, []any{"ip", "ipv4"}) {
		t.Errorf("address formats = %v, want ip and ipv4", got)
	}
	size := body.Properties["size"]
	if size.Minimum == nil || *size.Minimum != 5 || size.Maximum == nil || *size.Maximum != 100 {
		t.Errorf("size = %+v, want the tightest bounds, 5 to 100", size)
	}
	if got := keywords(size, multiple); !slices.Equal(got, []any{5.0, 2.0}) {
		t.Errorf("size multiples = %v, want 5 and 2", got)
	}
	if status := body.Properties["status"]; !slices.Equal(status.Enum, []any{"b", "c"}) {
		t.Errorf("status = %+v, want only the values both lists allow", status)
	}
}

type declaredInOneChain struct {
	Code   string `json:"code"`
	Size   int    `json:"size"`
	Status string `json:"status"`
}

func (in *declaredInOneChain) Validate(v *Validation) {
	v.String(&in.Code).Matches(`^[A-Z]`).Matches(`[0-9]$`)
	v.Number(&in.Size).Min(5).Min(1).MultipleOf(5).MultipleOf(2)
	v.String(&in.Status).OneOf("a", "b", "c").OneOf("b", "c", "d")
}

// TestEveryStepOfOneChainIsDocumented is the counterpart for steps declared in
// a single chain, which the rule set described by the last step of each kind:
// the document kept the second pattern, the looser minimum and the second
// multiple, and offered every value either OneOf listed.
func TestEveryStepOfOneChainIsDocumented(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/chain", func(ctx *Context, in declaredInOneChain) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	for _, body := range []string{`{"code":"A"}`, `{"code":"1"}`, `{"size":2}`, `{"size":5}`, `{"status":"a"}`} {
		assertStatus(t, do(t, app, "POST", "/chain", body), http.StatusUnprocessableEntity)
	}
	assertStatus(t, do(t, app, "POST", "/chain", `{"code":"A1","size":10,"status":"b"}`), http.StatusOK)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	body := requestOf(t, doc, doc.Paths["/chain"].Post)
	collect := func(schema *Schema, of func(*Schema) any) []any {
		var found []any
		for _, s := range append([]*Schema{schema}, schema.AllOf...) {
			if value := of(s); value != nil {
				found = append(found, value)
			}
		}
		return found
	}
	patterns := collect(body.Properties["code"], func(s *Schema) any {
		if s.Pattern == "" {
			return nil
		}
		return s.Pattern
	})
	if !slices.Equal(patterns, []any{"^[A-Z]", "[0-9]$"}) {
		t.Errorf("code patterns = %v, want both", patterns)
	}
	size := body.Properties["size"]
	multiples := collect(size, func(s *Schema) any {
		if s.MultipleOf == nil {
			return nil
		}
		return *s.MultipleOf
	})
	if size.Minimum == nil || *size.Minimum != 5 || !slices.Equal(multiples, []any{5.0, 2.0}) {
		t.Errorf("size = %+v with multiples %v, want a minimum of 5 and both multiples", size, multiples)
	}
	if status := body.Properties["status"]; !slices.Equal(status.Enum, []any{"b", "c"}) {
		t.Errorf("status enum = %v, want only the values both lists allow", status.Enum)
	}
}

// TestMergedRuleSetsKeepTheTightestOfEach covers the combination directly: a
// looser bound declared later does not loosen a tighter one declared earlier,
// and lists that share no value leave none, which a nullable member still
// answers with null.
func TestMergedRuleSetsKeepTheTightestOfEach(t *testing.T) {
	t.Parallel()
	five, one, ten, twenty := 5.0, 1.0, 10.0, 20.0
	c, also := fieldConstraints{
		{Minimum: &five, Maximum: &ten, Enum: []any{"a"}},
		{Minimum: &one, Maximum: &twenty, Enum: []any{"b"}},
	}.merged()
	if *c.Minimum != 5 || *c.Maximum != 10 || len(also) != 0 {
		t.Errorf("merged = %+v %v, want 5 to 10 and nothing more", c, also)
	}
	if c.Enum == nil || len(c.Enum) != 0 {
		t.Errorf("enum = %#v, want an empty list: no value is in both", c.Enum)
	}
	schema := &Schema{Type: []string{"string", "null"}}
	applyConstraints(schema, fieldConstraints{{Enum: []any{"a"}}, {Enum: []any{"b"}}})
	if !slices.Equal(schema.Enum, []any{nil}) {
		t.Errorf("enum = %#v, want only null", schema.Enum)
	}
}

// shadowedRules declares a rule on a field an outer one of the same name
// shadows, which json/v2 never reads and the document does not describe.
type shadowedRules struct {
	ID string `json:"id"`
	ShadowedBase
}

func (in *shadowedRules) Validate(v *Validation) {
	v.Number(&in.ShadowedBase.ID).Min(5)
	v.String(&in.ID).MaxLen(3)
}

// TestRuleOnAShadowedFieldDescribesNothing checks that the rule on the hidden
// field is still reported, under the name it always had, but is not written
// onto the member of that name, which is another field.
func TestRuleOnAShadowedFieldDescribesNothing(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/shadowed", func(ctx *Context, in shadowedRules) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	if got := fieldsOf(t, app, "/shadowed", `{"id":"abc"}`); !slices.Equal(got, []string{"id"}) {
		t.Errorf("fields = %q, want [id]", got)
	}
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	id := requestOf(t, doc, doc.Paths["/shadowed"].Post).Properties["id"]
	if id.Minimum != nil || id.MaxLength == nil {
		t.Errorf("id = %+v, want the outer field's maxLength and not the shadowed one's minimum", id)
	}
}

// embeddingModel embeds a model whose rules are its own, whose first member
// sits where the embedded struct does.
type embeddingModel struct {
	embeddedRules
	Other string `json:"other"`
}

type embeddedRules struct {
	Code string `json:"code"`
}

func (e *embeddedRules) Validate(v *Validation) { v.String(&e.Code).Required() }

func (in *embeddingModel) Validate(v *Validation) { v.Nested(&in.embeddedRules) }

// TestNestedEmbeddedModelIsNamedAsItsMembersAre checks that a model nested by
// embedding reports its failures under its members' own names, which are its
// parent's, rather than under the name of the first one.
func TestNestedEmbeddedModelIsNamedAsItsMembersAre(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/e", func(ctx *Context, in embeddingModel) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	if got := fieldsOf(t, app, "/e", `{"other":"x"}`); !slices.Equal(got, []string{"code"}) {
		t.Errorf("fields = %q, want [code]", got)
	}
}

// statusPatch is a partial update whose members may be left out or sent as
// null, one of them held to a list of values and one of them required.
type statusPatch struct {
	Status   *string `json:"status"`
	Priority *string `json:"priority"`
	Level    *int    `query:"level"`
}

func (in *statusPatch) Validate(v *Validation) {
	v.String(&in.Status).OneOf("open", "closed")
	v.String(&in.Priority).Required().OneOf("low", "high")
	v.Number(&in.Level).OneOf(1, 2)
}

// TestOpenAPIEnumOfANullableMemberAdmitsNull holds the list of values a pointer
// is held to to what the server accepts. A nil pointer skips its rules, so
// {"status":null} is accepted, and a list without null next to a type that
// admits it told a client the request was invalid. A Required rule refuses the
// null, so that member lists none.
func TestOpenAPIEnumOfANullableMemberAdmitsNull(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Patch("/p", func(ctx *Context, in statusPatch) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "PATCH", "/p", `{"status":null,"priority":"low"}`), http.StatusOK)
	assertStatus(t, do(t, app, "PATCH", "/p", `{"status":"open","priority":null}`), http.StatusUnprocessableEntity)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	op := doc.Paths["/p"].Patch
	body := (&schemaBuilder{schemas: doc.Components.Schemas}).resolve(op.RequestBody.Content["application/json"].Schema)
	if body == nil {
		t.Fatalf("no body schema; components are %v", keysOf(doc.Components.Schemas))
	}
	status := body.Properties["status"]
	if types, _ := status.Type.([]string); !slices.Contains(types, "null") || !slices.Contains(status.Enum, any(nil)) {
		t.Errorf("status = %+v, want a nullable string whose enum lists null", status)
	}
	priority := body.Properties["priority"]
	if priority.Type != "string" || slices.Contains(priority.Enum, any(nil)) || len(priority.Enum) != 2 {
		t.Errorf("priority = %+v, want a string whose enum is low and high alone", priority)
	}
	level := op.Parameters[0].Schema
	if !slices.Contains(level.Enum, any(nil)) {
		t.Errorf("level = %+v, want its enum to list null beside the type that admits it", level)
	}
}
