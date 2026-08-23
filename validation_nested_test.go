package muzak

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"muzak.dev/framework/validate"
)

// address is a model held inside another, which is what Nested exists for.
type address struct {
	Street string `json:"street"`
	City   string `json:"city"`
	Post   string `json:"postcode"`
}

func (a *address) Validate(v *Validation) {
	v.String(&a.Street).Trim().Required().MaxLen(60)
	v.String(&a.City).Trim().Required().MaxLen(40)
	v.String(&a.Post).Trim().Upper().Required().Matches(`^[A-Z0-9 ]{3,10}$`)
}

// delivery exercises every entry point the other tests do not: Time, Value,
// Nested, and a label override.
type delivery struct {
	Reference string     `json:"reference"`
	When      time.Time  `json:"when"`
	Priority  priority   `json:"priority"`
	Ship      address    `json:"ship_to"`
	Bill      *address   `json:"bill_to,omitzero"`
	Window    *time.Time `json:"window,omitzero"`
	Scores    []int      `json:"scores,omitzero"`
}

// priority is a named string, which the generic rule set covers.
type priority string

func (in *delivery) Validate(v *Validation) {
	v.String(&in.Reference).As("ref").Required().MinLen(4)
	v.Time(&in.When).Required().After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	v.Time(&in.Window).After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	v.Value(&in.Priority).Required().OneOf("standard", "express")
	v.Slice(&in.Scores).Each(validate.Value[int]().Must(func(score int) error {
		if score < 0 {
			return errors.New("must not be negative")
		}
		return nil
	}))

	v.Nested(&in.Ship)
	v.Nested(in.Bill)
}

func deliveryApp(t *testing.T) *App {
	t.Helper()
	app := New(quietOptions())
	app.Post("/deliveries", func(ctx *Context, in delivery) (delivery, error) {
		return in, nil
	})
	return mustBuild(t, app)
}

func TestNestedModelFailuresCarryADottedPath(t *testing.T) {
	t.Parallel()
	app := deliveryApp(t)

	rec := do(t, app, "POST", "/deliveries", `{
		"reference": "ABCD",
		"when": "2026-01-01T00:00:00Z",
		"priority": "express",
		"ship_to": {"street": "", "city": "Ankara", "postcode": "06000"}
	}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 {
		t.Fatalf("details = %d entries, want 1\n%s", len(details), rec.Body.String())
	}
	if details[0].Field != "ship_to.street" || details[0].Issue != "is required" {
		t.Errorf("detail = %+v, want the nested field named", details[0])
	}
}

func TestNestedPointerModelIsValidatedWhenPresent(t *testing.T) {
	t.Parallel()
	app := deliveryApp(t)

	rec := do(t, app, "POST", "/deliveries", `{
		"reference": "ABCD",
		"when": "2026-01-01T00:00:00Z",
		"priority": "express",
		"ship_to": {"street": "A road", "city": "Ankara", "postcode": "06000"},
		"bill_to": {"street": "A road", "city": "", "postcode": "06000"}
	}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Field != "bill_to.city" {
		t.Fatalf("details = %+v, want the nested pointer's field named", details)
	}
}

func TestNestedNilPointerModelIsSkipped(t *testing.T) {
	t.Parallel()
	app := deliveryApp(t)

	rec := do(t, app, "POST", "/deliveries", `{
		"reference": "ABCD",
		"when": "2026-01-01T00:00:00Z",
		"priority": "express",
		"ship_to": {"street": "A road", "city": "Ankara", "postcode": "06000"}
	}`)
	assertStatus(t, rec, http.StatusOK)
}

func TestNestedTransformsReachTheHandler(t *testing.T) {
	t.Parallel()
	app := deliveryApp(t)

	rec := do(t, app, "POST", "/deliveries", `{
		"reference": "ABCD",
		"when": "2026-01-01T00:00:00Z",
		"priority": "express",
		"ship_to": {"street": "  A road  ", "city": " Ankara ", "postcode": " 06000 "}
	}`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{
		"reference":"ABCD",
		"when":"2026-01-01T00:00:00Z",
		"priority":"express",
		"ship_to":{"street":"A road","city":"Ankara","postcode":"06000"}
	}`)
}

func TestTimeAndValueEntryPoints(t *testing.T) {
	t.Parallel()
	app := deliveryApp(t)

	tests := []struct {
		name  string
		body  string
		field string
		issue string
	}{
		{
			name: "a required time",
			body: `{"reference":"ABCD","priority":"express",
				"ship_to":{"street":"A road","city":"Ankara","postcode":"06000"}}`,
			field: "when", issue: "is required",
		},
		{
			name: "a time out of range",
			body: `{"reference":"ABCD","when":"1999-01-01T00:00:00Z","priority":"express",
				"ship_to":{"street":"A road","city":"Ankara","postcode":"06000"}}`,
			field: "when", issue: "must be after 2020-01-01T00:00:00Z",
		},
		{
			name: "an optional time is checked when supplied",
			body: `{"reference":"ABCD","when":"2026-01-01T00:00:00Z","priority":"express",
				"window":"1999-01-01T00:00:00Z",
				"ship_to":{"street":"A road","city":"Ankara","postcode":"06000"}}`,
			field: "window", issue: "must be after 2020-01-01T00:00:00Z",
		},
		{
			name: "a named string through the generic rule set",
			body: `{"reference":"ABCD","when":"2026-01-01T00:00:00Z","priority":"urgent",
				"ship_to":{"street":"A road","city":"Ankara","postcode":"06000"}}`,
			field: "priority", issue: "must be one of standard or express",
		},
		{
			name: "an element rule over a numeric collection",
			body: `{"reference":"ABCD","when":"2026-01-01T00:00:00Z","priority":"express",
				"scores":[1,-2],
				"ship_to":{"street":"A road","city":"Ankara","postcode":"06000"}}`,
			field: "scores[1]", issue: "must not be negative",
		},
		{
			name: "a relabelled field",
			body: `{"reference":"AB","when":"2026-01-01T00:00:00Z","priority":"express",
				"ship_to":{"street":"A road","city":"Ankara","postcode":"06000"}}`,
			field: "ref", issue: "must be at least 4 characters",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, app, "POST", "/deliveries", tc.body)
			assertStatus(t, rec, http.StatusUnprocessableEntity)

			details := decodeError(t, rec).Error.Details
			if len(details) != 1 {
				t.Fatalf("details = %+v, want one entry", details)
			}
			if details[0].Field != tc.field || details[0].Issue != tc.issue {
				t.Errorf("detail = %+v, want %s / %s", details[0], tc.field, tc.issue)
			}
		})
	}
}

// TestRejectWithoutAField covers a rejection that names nothing, which is
// reported against the request rather than against a field.
func TestRejectWithoutAField(t *testing.T) {
	t.Parallel()
	v := &Validation{}
	v.Reject(nil, "the request as a whole is wrong")

	details := v.details()
	if len(details) != 1 {
		t.Fatalf("details = %+v", details)
	}
	if details[0].Field != "" || details[0].Location != "body" {
		t.Errorf("detail = %+v, want an unnamed body failure", details[0])
	}
}

// TestWhenDoesNothingWhenTheConditionIsFalse pins the other half of the
// conditional.
func TestWhenDoesNothingWhenTheConditionIsFalse(t *testing.T) {
	t.Parallel()
	v := &Validation{}
	v.When(false).Reject(nil, "should not appear")
	if details := v.details(); len(details) != 0 {
		t.Errorf("details = %+v, want none", details)
	}
}

func TestNestedIgnoresNothingToValidate(t *testing.T) {
	t.Parallel()
	v := &Validation{}
	v.Nested(nil)
	v.Nested((*address)(nil))
	if details := v.details(); len(details) != 0 {
		t.Errorf("details = %+v, want none", details)
	}
}

func TestJoinPath(t *testing.T) {
	t.Parallel()
	tests := []struct{ prefix, name, want string }{
		{"", "city", "city"},
		{"ship_to", "city", "ship_to.city"},
		{"ship_to", "", "ship_to"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := joinPath(tc.prefix, tc.name); got != tc.want {
			t.Errorf("joinPath(%q, %q) = %q, want %q", tc.prefix, tc.name, got, tc.want)
		}
	}
}

// TestPlanForTypeIsCachedPerType checks the cache that keeps a nested model
// from being walked on every request.
func TestPlanForTypeIsCachedPerType(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[address]()
	first := planForType(typ)
	second := planForType(typ)
	if first != second {
		t.Error("planForType built the plan twice for one type")
	}
	if len(first.fields) == 0 {
		t.Error("the plan recorded no fields")
	}
}

// TestValidationConstraintsDescribeNestedRoutes checks that the document also
// carries the constraints of a model reached through Nested.
func TestValidationConstraintsInTheDocument(t *testing.T) {
	t.Parallel()
	app := deliveryApp(t)
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	body := doc.Paths["/deliveries"].Post.RequestBody.Content["application/json"].Schema
	if body.Ref != "" {
		body = doc.Components.Schemas[trimComponentPrefix(body.Ref)]
	}
	reference := body.Properties["reference"]
	if reference == nil {
		t.Fatalf("no schema for reference: %v", keysOf(body.Properties))
	}
	// The rule set relabelled the field, so the constraint lands under the
	// label rather than under the JSON name.
	if labelled := body.Properties["ref"]; labelled != nil {
		if labelled.MinLength == nil || *labelled.MinLength != 4 {
			t.Errorf("relabelled constraint = %+v", labelled)
		}
	}
	priorityField := body.Properties["priority"]
	if priorityField == nil || len(priorityField.Enum) != 2 {
		t.Errorf("priority = %+v, want an enum of two", priorityField)
	}
}

// trimComponentPrefix strips the pointer prefix from a schema reference.
func trimComponentPrefix(ref string) string {
	return ref[len(componentPrefix):]
}
