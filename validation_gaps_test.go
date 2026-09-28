package muzak

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"muzak.dev/framework/validate"
)

// mixedFailures fails to bind one field and fails to validate another, which is
// how the two kinds of failure are shown to travel together.
type mixedFailures struct {
	Limit int      `query:"limit"`
	Name  string   `json:"name"`
	Tags  []string `query:"tag"`
}

func (in *mixedFailures) Validate(v *Validation) {
	v.Number(&in.Limit).Between(1, 100)
	v.String(&in.Name).Required().MinLen(4)
	v.Slice(&in.Tags).Required().MaxItems(2).Each(validate.String().MaxLen(3))
}

// TestBindingAndValidationFailuresTravelTogether checks that a client learns
// about both kinds at once, and that the field which failed to bind is reported
// only by the binder.
func TestBindingAndValidationFailuresTravelTogether(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/mixed", func(ctx *Context, in mixedFailures) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/mixed?limit=abc&tag=ok", `{"name":"ab"}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	issues := map[string]string{}
	for _, detail := range decodeError(t, rec).Error.Details {
		issues[detail.Field] = detail.Issue
	}
	if issues["limit"] != "must be a valid integer" {
		t.Errorf("limit = %q, want only the binder's message", issues["limit"])
	}
	if issues["name"] != "must be at least 4 characters" {
		t.Errorf("name = %q, want the validation message", issues["name"])
	}
	if len(issues) != 2 {
		t.Errorf("reported %v, want exactly the two failures", issues)
	}
}

// TestValidatedQueryParameterConstraintsReachTheDocument covers a required
// query parameter and a collection parameter whose elements carry rules.
func TestValidatedQueryParameterConstraintsReachTheDocument(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/mixed", func(ctx *Context, in mixedFailures) (rtOut, error) { return rtOut{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}

	byName := map[string]Parameter{}
	for _, p := range doc.Paths["/mixed"].Post.Parameters {
		byName[p.Name] = p
	}
	tag, described := byName["tag"]
	if !described {
		t.Fatalf("the tag parameter is not described: %v", byName)
	}
	if !tag.Required {
		t.Error("a parameter the rules require is not marked required")
	}
	if tag.Schema.MaxItems == nil || *tag.Schema.MaxItems != 2 {
		t.Errorf("maxItems = %v", tag.Schema.MaxItems)
	}
	if tag.Schema.Items == nil || tag.Schema.Items.MaxLength == nil || *tag.Schema.Items.MaxLength != 3 {
		t.Errorf("the element constraint did not reach items: %+v", tag.Schema.Items)
	}
}

// patterned exercises the constraint keywords the other models do not.
type patterned struct {
	Code   string   `json:"code"`
	Amount float64  `json:"amount"`
	Labels []string `json:"labels"`
}

func (in *patterned) Validate(v *Validation) {
	v.String(&in.Code).Required().Matches(`^[A-Z]{3}$`)
	v.Number(&in.Amount).MultipleOf(0.5)
	v.Slice(&in.Labels).MinItems(1).Unique()
}

func TestEveryConstraintKeywordReachesTheSchema(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/patterned", func(ctx *Context, in patterned) (rtOut, error) { return rtOut{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}

	schema := doc.Paths["/patterned"].Post.RequestBody.Content["application/json"].Schema
	if schema.Ref != "" {
		schema = doc.Components.Schemas[schema.Ref[len(componentPrefix):]]
	}
	if got := schema.Properties["code"]; got == nil || got.Pattern != `^[A-Z]{3}$` {
		t.Errorf("pattern = %+v", got)
	}
	if got := schema.Properties["amount"]; got == nil || got.MultipleOf == nil || *got.MultipleOf != 0.5 {
		t.Errorf("multipleOf = %+v", got)
	}
	labels := schema.Properties["labels"]
	if labels == nil || labels.MinItems == nil || *labels.MinItems != 1 || !labels.UniqueItems {
		t.Errorf("labels = %+v", labels)
	}
	// Code is required by the rules, so it joins the required list even though
	// the Go type alone would not have put it there differently.
	if !sliceContains(schema.Required, "code") {
		t.Errorf("required = %v, want it to include code", schema.Required)
	}
}

// TestApplyConstraintsIgnoresAMissingSchema covers the guard that keeps a
// constraint for a property the document does not describe from panicking.
func TestApplyConstraintsIgnoresAMissingSchema(t *testing.T) {
	t.Parallel()
	applyConstraints(nil, validate.Constraints{Required: true})
}

// TestSetRequiredAddsAndRemoves covers both directions of the reconciliation
// between what the Go type implies and what the rules actually enforce.
func TestSetRequiredAddsAndRemoves(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		required []string
		field    string
		want     bool
		expect   []string
	}{
		{"adds when absent", []string{"a"}, "b", true, []string{"a", "b"}},
		{"leaves an existing entry", []string{"a", "b"}, "b", true, []string{"a", "b"}},
		{"removes when present", []string{"a", "b"}, "b", false, []string{"a"}},
		{"leaves an absent entry", []string{"a"}, "b", false, []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := setRequired(tc.required, tc.field, tc.want)
			if len(got) != len(tc.expect) {
				t.Fatalf("setRequired = %v, want %v", got, tc.expect)
			}
			for i := range got {
				if got[i] != tc.expect[i] {
					t.Fatalf("setRequired = %v, want %v", got, tc.expect)
				}
			}
		})
	}
}

// TestApplyBodyConstraintsWithNothingToDo covers the early returns for a model
// whose rules say nothing and for a schema with no properties.
func TestApplyBodyConstraintsWithNothingToDo(t *testing.T) {
	t.Parallel()
	builder := newSchemaBuilder()
	builder.applyBodyConstraints(&Schema{}, nil, nil, true)
	builder.applyBodyConstraints(&Schema{Type: "string"},
		map[string]validate.Constraints{"a": {Required: true}}, nil, true)
	// A reference that names nothing resolves to nothing rather than panicking.
	builder.applyBodyConstraints(&Schema{Ref: componentPrefix + "absent"},
		map[string]validate.Constraints{"a": {Required: true}}, nil, false)
}

// TestValidationWithoutAModel covers the paths a plan takes when the input type
// declares no rules at all.
func TestValidationWithoutAModel(t *testing.T) {
	t.Parallel()
	type plain struct {
		Name string `json:"name"`
	}
	plan, err := newBindPlan(reflect.TypeFor[plain](), "POST", "/x")
	if err != nil {
		t.Fatalf("newBindPlan = %v", err)
	}
	if plan.validation != nil {
		t.Error("a type with no rules was given a validation plan")
	}
	value := reflect.New(reflect.TypeFor[plain]()).Elem()
	if details := plan.runValidation(value, nil); details != nil {
		t.Errorf("runValidation = %v, want nothing", details)
	}
	if got := plan.describeConstraints(); got != nil {
		t.Errorf("describeConstraints = %v, want nothing", got)
	}
	if got := plan.elementConstraints(); got != nil {
		t.Errorf("elementConstraints = %v, want nothing", got)
	}
}

// TestSkipValidationLeavesTheDocumentHonest checks that a route which does not
// validate does not claim to.
func TestSkipValidationLeavesTheDocumentHonest(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/lenient", func(ctx *Context, in patterned) (rtOut, error) { return rtOut{}, nil },
		SkipValidation())
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	schema := doc.Paths["/lenient"].Post.RequestBody.Content["application/json"].Schema
	if schema.Ref != "" {
		schema = doc.Components.Schemas[schema.Ref[len(componentPrefix):]]
	}
	if got := schema.Properties["code"]; got != nil && got.Pattern != "" {
		t.Errorf("a route that skips validation described a pattern: %+v", got)
	}
}

// TestDescribeFallsBackForAnUnknownPointer covers a rule set bound to something
// that is not a field of the model being validated.
func TestDescribeFallsBackForAnUnknownPointer(t *testing.T) {
	t.Parallel()
	stray := "value"

	bare := &Validation{}
	name, location := bare.describe(&stray, "")
	if name != "" || location != "body" {
		t.Errorf("describe = %q, %q; want an unnamed body failure", name, location)
	}

	// A label still wins, because the developer said what to call it.
	name, location = bare.describe(&stray, "custom")
	if name != "custom" || location != "body" {
		t.Errorf("describe = %q, %q; want the label", name, location)
	}
}

func TestOriginOfRefusesWhatItCannotResolve(t *testing.T) {
	t.Parallel()
	value := "x"
	tests := []struct {
		name string
		v    *Validation
		of   any
	}{
		{"no plan", &Validation{}, &value},
		{"nil target", &Validation{plan: &validationPlan{}, base: 1, size: 1}, nil},
		{"not a pointer", &Validation{plan: &validationPlan{}, base: 1, size: 1}, "plain"},
		{"nil pointer", &Validation{plan: &validationPlan{}, base: 1, size: 1}, (*string)(nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, found := tc.v.originOf(tc.of); found {
				t.Error("originOf resolved something it should not have")
			}
		})
	}
}

// TestNameOfNestedFallsBack covers a nested model that no field of the parent
// points at, which cannot happen through the public API but keeps the scan
// honest about failing.
func TestNameOfNestedFallsBack(t *testing.T) {
	t.Parallel()
	stray := &address{}

	bare := &Validation{}
	if got := bare.nameOfNested(reflect.ValueOf(stray)); got != "" {
		t.Errorf("nameOfNested = %q, want empty", got)
	}

	holder := &delivery{}
	withValue := &Validation{
		plan:  planForType(reflect.TypeFor[delivery]()),
		base:  reflect.ValueOf(holder).Pointer(),
		size:  reflect.TypeFor[delivery]().Size(),
		value: reflect.ValueOf(holder).Elem(),
	}
	if got := withValue.nameOfNested(reflect.ValueOf(stray)); got != "" {
		t.Errorf("nameOfNested = %q, want empty for a model nothing points at", got)
	}
}

// TestCollectOriginsWalksEmbeddedStructsAndSkipsWhatItShould covers the field
// walk that gives every failure a name.
func TestCollectOriginsWalksEmbeddedStructsAndSkipsWhatItShould(t *testing.T) {
	t.Parallel()
	type inner struct {
		Promoted string `json:"promoted"`
	}
	type outer struct {
		inner
		Named   string `json:"named"`
		Ignored string `json:"-"`
		Bare    string
		hidden  string //nolint:unused // present to prove unexported fields are skipped
	}

	fields := map[uintptr]fieldOrigin{}
	collectOrigins(reflect.TypeFor[outer](), 0, fields)

	names := map[string]bool{}
	for _, origin := range fields {
		names[origin.name] = true
	}
	for _, want := range []string{"promoted", "named", "Bare"} {
		if !names[want] {
			t.Errorf("collectOrigins did not record %q: %v", want, names)
		}
	}
	if names["-"] || names["Ignored"] {
		t.Errorf("a field tagged \"-\" was recorded: %v", names)
	}
	if names["hidden"] {
		t.Errorf("an unexported field was recorded: %v", names)
	}

	// A non-struct type records nothing rather than panicking.
	empty := map[uintptr]fieldOrigin{}
	collectOrigins(reflect.TypeFor[string](), 0, empty)
	if len(empty) != 0 {
		t.Errorf("collectOrigins on a non-struct recorded %v", empty)
	}
}

// unnamedRules binds a rule set to something outside the model, which is what
// the constraint description has to skip.
type unnamedRules struct {
	Name string `json:"name"`
}

var strayField = "outside the model"

func (in *unnamedRules) Validate(v *Validation) {
	v.String(&in.Name).Required()
	v.String(&strayField).Required()
	v.Slice(&straySlice).MaxItems(1)
}

var straySlice []string

// namedStrayRule binds outside itself and says so, which is allowed.
type namedStrayRule struct {
	Name string `json:"name"`
}

func (in *namedStrayRule) Validate(v *Validation) {
	v.String(&in.Name).Required()
	v.String(&strayField).As("computed").Required()
}

// TestRuleBoundOutsideTheModelIsRefused covers the startup check.
//
// A rule bound to something that is not a field used to be dropped quietly: its
// failures named no field and its constraints never reached the generated
// document. Both are silent, and neither shows up in a test that asserts a
// status, so the model is refused when the route is compiled instead.
func TestRuleBoundOutsideTheModelIsRefused(t *testing.T) {
	t.Parallel()
	_, err := newBindPlan(reflect.TypeFor[unnamedRules](), "POST", "/x")
	if err == nil {
		t.Fatal("a model binding a rule outside itself was accepted")
	}
	for _, want := range []string{"bound to a value rather than to a field", "&in.Field"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// TestRuleNamedWithAsMayBindAnywhere covers the escape hatch. Naming a rule is
// a statement that the caller knows where it is bound and what it reports as.
func TestRuleNamedWithAsMayBindAnywhere(t *testing.T) {
	t.Parallel()
	plan, err := newBindPlan(reflect.TypeFor[namedStrayRule](), "POST", "/x")
	if err != nil {
		t.Fatalf("a named rule was refused: %v", err)
	}
	constraints := plan.describeConstraints()
	if _, described := constraints["computed"]; !described {
		t.Errorf("the named rule is missing from the document: %v", constraints)
	}
}

// TestNestedModelsStillBuild is the check on the check.
//
// A nested model's fields live outside the parent's memory when the nested
// value is a pointer, so a naive "is this address inside the model" test would
// reject correct code. It does not, because each nesting level validates
// through its own Validation with its own base -- and this is what says so.
func TestNestedModelsStillBuild(t *testing.T) {
	t.Parallel()
	if _, err := newBindPlan(reflect.TypeFor[delivery](), "POST", "/x"); err != nil {
		t.Fatalf("a model with nested and pointer-nested models was refused: %v", err)
	}
}

// TestRuleSetsAreRecycledAcrossRequests covers the free lists a Validation
// keeps, which are what make redeclaring a model's rules cost nothing after the
// first request through a route.
//
// The recycled branch is otherwise reached only when a pooled Validation
// happens to be handed back to a model declaring the same kind of rule, which
// depends on how the pool is scheduled and so cannot be relied on to be
// exercised at all. Driving it directly is what makes it certain.
func TestRuleSetsAreRecycledAcrossRequests(t *testing.T) {
	t.Parallel()
	v := &Validation{}

	first := []any{v.nextString(), v.nextNumber(), v.nextTime()}
	// reset returns the counters to zero while keeping the sets already built,
	// which is exactly the state the next request through a route starts from.
	v.reset()
	second := []any{v.nextString(), v.nextNumber(), v.nextTime()}

	for i := range first {
		if first[i] != second[i] {
			t.Errorf("rule set %d was rebuilt rather than recycled", i)
		}
	}
	// A model that declares more rules than the last one still gets them.
	if extra := v.nextTime(); extra == second[2] {
		t.Error("a second time rule set reused the first")
	}
}
