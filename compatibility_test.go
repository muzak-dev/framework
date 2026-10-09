package muzak

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The rule tables below compare hand-written documents, so that each rule is
// exercised alone and in both directions: the same change to a schema is
// judged one way when clients send it and the other when they read it.

// requestBodyAt and responseBodyAt are where the schema of the documents
// compatReadDoc and compatAnswerDoc describe sits.
const (
	requestBodyAt  = "/paths/~1items/post/requestBody/content/application~1json/schema"
	responseBodyAt = "/paths/~1items/get/responses/200/content/application~1json/schema"
)

// compatDoc returns a document with the given paths and component schemas.
func compatDoc(paths map[string]*PathItem, schemas map[string]*Schema) *Document {
	doc := &Document{OpenAPI: OpenAPIVersion, Info: Info{Title: "T", Version: "1"}, Paths: paths}
	if schemas != nil {
		doc.Components = &Components{Schemas: schemas}
	}
	return doc
}

// compatReadDoc describes POST /items, which reads body.
func compatReadDoc(body *Schema, schemas map[string]*Schema) *Document {
	return compatDoc(map[string]*PathItem{"/items": {Post: &Operation{
		OperationID: "post_items",
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{"application/json": {Schema: body}}},
		Responses:   map[string]*Response{"204": {Description: "No Content"}},
	}}}, schemas)
}

// compatAnswerDoc describes GET /items, which answers body.
func compatAnswerDoc(body *Schema, schemas map[string]*Schema) *Document {
	return compatDoc(map[string]*PathItem{"/items": {Get: &Operation{
		OperationID: "get_items",
		Responses:   map[string]*Response{"200": {Description: "OK", Content: map[string]MediaType{"application/json": {Schema: body}}}},
	}}}, schemas)
}

// wantChange is one expected change.
type wantChange struct {
	severity ChangeSeverity
	kind     string
	location string
}

// assertAPIChanges fails unless the changes are exactly the wanted ones, in any
// order, compared by severity, kind and location.
func assertAPIChanges(t *testing.T, got []APIChange, wanted ...wantChange) {
	t.Helper()
	var have []wantChange
	for _, c := range got {
		have = append(have, wantChange{c.Severity, c.Kind, c.Location})
		if c.Message == "" || !strings.HasSuffix(c.Message, ".") {
			t.Errorf("change %+v has no sentence for a message", c)
		}
	}
	key := func(w wantChange) string { return fmt.Sprintf("%s %s %s", w.severity, w.kind, w.location) }
	var haveKeys, wantKeys []string
	for _, w := range have {
		haveKeys = append(haveKeys, key(w))
	}
	for _, w := range wanted {
		wantKeys = append(wantKeys, key(w))
	}
	slices.Sort(haveKeys)
	slices.Sort(wantKeys)
	if !slices.Equal(haveKeys, wantKeys) {
		t.Errorf("changes:\n  %s\nwant:\n  %s\nmessages:\n%s", strings.Join(haveKeys, "\n  "), strings.Join(wantKeys, "\n  "), renderAPIChanges(got))
	}
}

func renderAPIChanges(changes []APIChange) string {
	var b bytes.Buffer
	_ = WriteChanges(&b, changes)
	return b.String()
}

func compatPtr[T any](v T) *T { return &v }

func compatObj(properties map[string]*Schema, required ...string) *Schema {
	return &Schema{Type: "object", Properties: properties, Required: required}
}

func compatStr() *Schema { return &Schema{Type: "string"} }

// TestSchemaRules exercises every rule that judges a change to a schema, each
// in a request and in a response, and checks that it is the only change
// reported.
func TestSchemaRules(t *testing.T) {
	t.Parallel()
	closed := func(s *Schema) *Schema { s.AdditionalProperties = false; return s }
	tests := []struct {
		name              string
		old, cur          *Schema
		kind, at          string
		request, response ChangeSeverity
	}{
		{"optional member added", compatObj(map[string]*Schema{"a": compatStr()}), compatObj(map[string]*Schema{"a": compatStr(), "b": compatStr()}),
			"property-added", "/properties/b", Compatible, Compatible},
		{"required member added", compatObj(map[string]*Schema{"a": compatStr()}), compatObj(map[string]*Schema{"a": compatStr(), "b": compatStr()}, "b"),
			"property-added", "/properties/b", Breaking, Compatible},
		{"optional member removed from a closed object", closed(compatObj(map[string]*Schema{"a": compatStr(), "b": compatStr()})), closed(compatObj(map[string]*Schema{"a": compatStr()})),
			"property-removed", "/properties/b", Breaking, PossiblyBreaking},
		{"required member removed from an open object", compatObj(map[string]*Schema{"a": compatStr(), "b": compatStr()}, "b"), compatObj(map[string]*Schema{"a": compatStr()}),
			"property-removed", "/properties/b", PossiblyBreaking, Breaking},
		{"deprecated member removed", closed(compatObj(map[string]*Schema{"b": {Type: "string", Deprecated: true}}, "b")), closed(compatObj(map[string]*Schema{})),
			"property-removed", "/properties/b", PossiblyBreaking, PossiblyBreaking},
		{"member only required removed", compatObj(nil, "b"), compatObj(nil),
			"property-removed", "/properties/b", PossiblyBreaking, Breaking},
		{"member became required", compatObj(map[string]*Schema{"a": compatStr()}), compatObj(map[string]*Schema{"a": compatStr()}, "a"),
			"property-became-required", "/properties/a", Breaking, Compatible},
		{"member became optional", compatObj(map[string]*Schema{"a": compatStr()}, "a"), compatObj(map[string]*Schema{"a": compatStr()}),
			"property-became-optional", "/properties/a", Compatible, Breaking},
		{"type narrowed", &Schema{Type: []string{"string", "integer"}}, compatStr(), "type-narrowed", "/type", Breaking, Compatible},
		{"type widened", compatStr(), &Schema{Type: []string{"string", "integer"}}, "type-widened", "/type", Compatible, Breaking},
		{"type changed", compatStr(), &Schema{Type: "integer"}, "type-changed", "/type", Breaking, Breaking},
		{"integer widened to number", &Schema{Type: "integer"}, &Schema{Type: "number"}, "type-widened", "/type", Compatible, Breaking},
		{"number narrowed to integer", &Schema{Type: "number"}, &Schema{Type: "integer"}, "type-narrowed", "/type", Breaking, Compatible},
		{"type given to a value of any type", &Schema{Type: []string{"null", "string", "integer", "number", "boolean", "object", "array"}}, &Schema{Type: []string{"string", "null"}},
			"type-narrowed", "/type", Breaking, Compatible},
		{"nullable added", compatStr(), &Schema{Type: []string{"string", "null"}}, "nullable-added", "", Compatible, Breaking},
		{"nullable removed", &Schema{Type: []any{"string", "null"}}, compatStr(), "nullable-removed", "", Breaking, Compatible},
		{"enum added", compatStr(), &Schema{Type: "string", Enum: []any{"a", "b"}}, "enum-added", "/enum", Breaking, Compatible},
		{"enum removed", &Schema{Type: "string", Enum: []any{"a"}}, compatStr(), "enum-removed", "/enum", Compatible, PossiblyBreaking},
		{"enum values added", &Schema{Type: "string", Enum: []any{"a"}}, &Schema{Type: "string", Enum: []any{"a", "b"}},
			"enum-values-added", "/enum", Compatible, PossiblyBreaking},
		{"enum values removed", &Schema{Type: "string", Enum: []any{"a", "b"}}, &Schema{Type: "string", Enum: []any{"b"}},
			"enum-values-removed", "/enum", Breaking, Compatible},
		{"maximum length tightened", &Schema{Type: "string", MaxLength: compatPtr(10)}, &Schema{Type: "string", MaxLength: compatPtr(5)},
			"max-length-tightened", "/maxLength", Breaking, Compatible},
		{"maximum length relaxed", &Schema{Type: "string", MaxLength: compatPtr(5)}, &Schema{Type: "string", MaxLength: compatPtr(10)},
			"max-length-relaxed", "/maxLength", Compatible, PossiblyBreaking},
		{"maximum length added", compatStr(), &Schema{Type: "string", MaxLength: compatPtr(5)}, "max-length-tightened", "/maxLength", Breaking, Compatible},
		{"maximum length removed", &Schema{Type: "string", MaxLength: compatPtr(5)}, compatStr(), "max-length-relaxed", "/maxLength", Compatible, PossiblyBreaking},
		{"minimum length tightened", &Schema{Type: "string", MinLength: compatPtr(1)}, &Schema{Type: "string", MinLength: compatPtr(3)},
			"min-length-tightened", "/minLength", Breaking, Compatible},
		{"minimum tightened", &Schema{Type: "integer", Minimum: compatPtr(0.0)}, &Schema{Type: "integer", Minimum: compatPtr(1.0)},
			"minimum-tightened", "/minimum", Breaking, Compatible},
		{"minimum made exclusive", &Schema{Type: "number", Minimum: compatPtr(0.0)}, &Schema{Type: "number", ExclusiveMinimum: compatPtr(0.0)},
			"minimum-tightened", "/minimum", Breaking, Compatible},
		{"minimum relaxed below an exclusive one", &Schema{Type: "number", Minimum: compatPtr(10.0)}, &Schema{Type: "number", ExclusiveMinimum: compatPtr(5.0)},
			"minimum-relaxed", "/minimum", Compatible, PossiblyBreaking},
		{"maximum relaxed", &Schema{Type: "number", Maximum: compatPtr(5.0)}, &Schema{Type: "number"}, "maximum-relaxed", "/maximum", Compatible, PossiblyBreaking},
		{"exclusive maximum tightened", &Schema{Type: "number", Maximum: compatPtr(5.0)}, &Schema{Type: "number", Maximum: compatPtr(9.0), ExclusiveMaximum: compatPtr(5.0)},
			"maximum-tightened", "/maximum", Breaking, Compatible},
		{"minimum number of items tightened", &Schema{Type: "array"}, &Schema{Type: "array", MinItems: compatPtr(1)},
			"min-items-tightened", "/minItems", Breaking, Compatible},
		{"maximum number of items relaxed", &Schema{Type: "array", MaxItems: compatPtr(3)}, &Schema{Type: "array", MaxItems: compatPtr(4)},
			"max-items-relaxed", "/maxItems", Compatible, PossiblyBreaking},
		{"multiple tightened", &Schema{Type: "number", MultipleOf: compatPtr(2.0)}, &Schema{Type: "number", MultipleOf: compatPtr(4.0)},
			"multiple-of-tightened", "/multipleOf", Breaking, Compatible},
		{"multiple relaxed", &Schema{Type: "number", MultipleOf: compatPtr(4.0)}, &Schema{Type: "number", MultipleOf: compatPtr(2.0)},
			"multiple-of-relaxed", "/multipleOf", Compatible, PossiblyBreaking},
		{"multiple added", &Schema{Type: "number"}, &Schema{Type: "number", MultipleOf: compatPtr(0.5)},
			"multiple-of-tightened", "/multipleOf", Breaking, Compatible},
		{"unique items added", &Schema{Type: "array"}, &Schema{Type: "array", UniqueItems: true}, "unique-items-added", "/uniqueItems", Breaking, Compatible},
		{"unique items removed", &Schema{Type: "array", UniqueItems: true}, &Schema{Type: "array"}, "unique-items-removed", "/uniqueItems", Compatible, PossiblyBreaking},
		{"pattern added", compatStr(), &Schema{Type: "string", Pattern: "^a"}, "pattern-added", "/pattern", Breaking, Compatible},
		{"pattern removed", &Schema{Type: "string", Pattern: "^a"}, compatStr(), "pattern-removed", "/pattern", Compatible, PossiblyBreaking},
		{"pattern changed", &Schema{Type: "string", Pattern: "^a"}, &Schema{Type: "string", Pattern: "^b"}, "pattern-changed", "/pattern", Breaking, PossiblyBreaking},
		{"pattern moved into allOf beside a new one", &Schema{Type: "string", Pattern: "^a"}, &Schema{Type: "string", AllOf: []*Schema{{Pattern: "^a"}, {Pattern: "b$"}}},
			"pattern-added", "/pattern", Breaking, Compatible},
		{"format added", compatStr(), &Schema{Type: "string", Format: "uuid"}, "format-added", "/format", Breaking, Compatible},
		{"format removed", &Schema{Type: "string", Format: "date-time"}, compatStr(), "format-removed", "/format", Compatible, Breaking},
		{"format widened", &Schema{Type: "integer", Format: "int32"}, &Schema{Type: "integer", Format: "int64"}, "format-widened", "/format", Compatible, Breaking},
		{"format narrowed", &Schema{Type: "number", Format: "double"}, &Schema{Type: "number", Format: "float"}, "format-narrowed", "/format", Breaking, Compatible},
		{"format changed", &Schema{Type: "string", Format: "uuid"}, &Schema{Type: "string", Format: "date-time"}, "format-changed", "/format", Breaking, Breaking},
		{"content encoding changed", &Schema{Type: "string", ContentEncoding: "base64"}, compatStr(), "content-encoding-changed", "/contentEncoding", Breaking, Breaking},
		{"unknown members allowed", closed(compatObj(nil)), compatObj(nil), "unknown-members-allowed", "/additionalProperties", Compatible, PossiblyBreaking},
		{"unknown members refused", compatObj(nil), closed(compatObj(nil)), "unknown-members-refused", "/additionalProperties", Breaking, Compatible},
		{"map values narrowed", &Schema{Type: "object", AdditionalProperties: &Schema{}}, &Schema{Type: "object", AdditionalProperties: &Schema{Type: []string{"string", "integer", "number", "boolean", "object", "array", "null"}, Enum: []any{"x", nil}}},
			"enum-added", "/additionalProperties/enum", Breaking, Compatible},
		{"map values widened to anything", &Schema{Type: "object", AdditionalProperties: Schema{Type: []string{"null", "string", "integer", "number", "boolean", "object", "array"}, MaxLength: compatPtr(3)}}, &Schema{Type: "object", AdditionalProperties: true},
			"max-length-relaxed", "/additionalProperties/maxLength", Compatible, PossiblyBreaking},
		{"items narrowed", &Schema{Type: "array", Items: &Schema{Type: []string{"string", "integer"}}}, &Schema{Type: "array", Items: compatStr()},
			"type-narrowed", "/items/type", Breaking, Compatible},
		{"alternative added", &Schema{AnyOf: []*Schema{compatStr(), {Type: "integer"}}}, &Schema{AnyOf: []*Schema{compatStr(), {Type: "integer"}, {Type: "boolean"}}},
			"any-of-alternative-added", "/anyOf/2", Compatible, Breaking},
		{"alternative removed", &Schema{AnyOf: []*Schema{compatStr(), {Type: "integer"}}}, &Schema{AnyOf: []*Schema{compatStr()}},
			"any-of-alternative-removed", "/anyOf/1", Breaking, Compatible},
		{"choice added", compatStr(), &Schema{Type: "string", AnyOf: []*Schema{{MinLength: compatPtr(1)}, {Pattern: "x"}}},
			"any-of-added", "/anyOf", Breaking, Compatible},
		{"choice removed", &Schema{Type: "string", AnyOf: []*Schema{{MinLength: compatPtr(1)}, {Pattern: "x"}}}, compatStr(),
			"any-of-removed", "/anyOf", Compatible, Breaking},
		{"default added", &Schema{Type: "integer"}, &Schema{Type: "integer", Default: 10}, "default-added", "/default", PossiblyBreaking, Compatible},
		{"default changed", &Schema{Type: "integer", Default: int64(10)}, &Schema{Type: "integer", Default: 20.0}, "default-changed", "/default", PossiblyBreaking, Compatible},
		{"default removed", &Schema{Type: "integer", Default: 10.0}, &Schema{Type: "integer"}, "default-removed", "/default", PossiblyBreaking, Compatible},
		{"deprecated", compatStr(), &Schema{Type: "string", Deprecated: true}, "deprecated-added", "", Compatible, Compatible},
		{"no longer deprecated", &Schema{Type: "string", Deprecated: true}, compatStr(), "deprecated-removed", "", Compatible, Compatible},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAPIChanges(t, CompareDocuments(compatReadDoc(tt.old, nil), compatReadDoc(tt.cur, nil)),
				wantChange{tt.request, "request-" + tt.kind, requestBodyAt + tt.at})
			assertAPIChanges(t, CompareDocuments(compatAnswerDoc(tt.old, nil), compatAnswerDoc(tt.cur, nil)),
				wantChange{tt.response, "response-" + tt.kind, responseBodyAt + tt.at})
			// The reverse comparison reports the opposite change, so the rule
			// is checked from both sides.
			reversed := CompareDocuments(compatReadDoc(tt.cur, nil), compatReadDoc(tt.old, nil))
			if len(reversed) != 1 || reversed[0].Kind != "request-"+inverseKind(tt.kind) {
				t.Errorf("reversed = %+v, want one %s", reversed, inverseKind(tt.kind))
			}
		})
	}
}

// inverseKind is the kind of change that undoes a change of the given kind.
func inverseKind(kind string) string {
	for _, pair := range [][2]string{
		{"-added", "-removed"}, {"-widened", "-narrowed"}, {"-tightened", "-relaxed"}, {"-tightened", "-loosened"},
		{"-became-required", "-became-optional"}, {"-allowed", "-refused"}, {"-deprecated", "-undeprecated"},
	} {
		if base, ok := strings.CutSuffix(kind, pair[0]); ok {
			if kind == "security-tightened" {
				return "security-loosened"
			}
			return base + pair[1]
		}
		if base, ok := strings.CutSuffix(kind, pair[1]); ok {
			return base + pair[0]
		}
	}
	return kind
}

func TestInverseKind(t *testing.T) {
	t.Parallel()
	for kind, inverse := range map[string]string{
		"request-property-added": "request-property-removed", "response-type-widened": "response-type-narrowed",
		"request-minimum-relaxed": "request-minimum-tightened", "security-tightened": "security-loosened",
		"security-loosened": "security-tightened", "operation-deprecated": "operation-undeprecated",
		"operation-undeprecated": "operation-deprecated", "request-unknown-members-refused": "request-unknown-members-allowed",
		"parameter-became-optional": "parameter-became-required", "path-renamed": "path-renamed",
	} {
		if got := inverseKind(kind); got != inverse {
			t.Errorf("inverseKind(%q) = %q, want %q", kind, got, inverse)
		}
	}
}

// TestRemovedRequestMemberOfAnOpenObjectIsIgnored separates the two reasons a
// removed request member matters: a closed object refuses it, an open one
// ignores what is sent in it.
func TestRemovedRequestMemberOfAnOpenObjectIsIgnored(t *testing.T) {
	t.Parallel()
	old := compatObj(map[string]*Schema{"a": compatStr(), "b": compatStr()})
	cur := compatObj(map[string]*Schema{"a": compatStr()})
	got := CompareDocuments(compatReadDoc(old, nil), compatReadDoc(cur, nil))
	assertAPIChanges(t, got, wantChange{PossiblyBreaking, "request-property-removed", requestBodyAt + "/properties/b"})
	if !strings.Contains(got[0].Message, "ignored") {
		t.Errorf("message = %q, want it to say the member is ignored", got[0].Message)
	}
}

// TestResponseMemberAddedToAClosedObject covers the one response that is not
// open: a member added to an object that said it held no others may trip a
// client that validates.
func TestResponseMemberAddedToAClosedObject(t *testing.T) {
	t.Parallel()
	old := &Schema{Type: "object", AdditionalProperties: false}
	cur := &Schema{Type: "object", AdditionalProperties: false, Properties: map[string]*Schema{"a": compatStr()}}
	assertAPIChanges(t, CompareDocuments(compatAnswerDoc(old, nil), compatAnswerDoc(cur, nil)),
		wantChange{PossiblyBreaking, "response-property-added", responseBodyAt + "/properties/a"})
}

// TestDisjointTypesStopTheComparison checks that a type replaced by an
// unrelated one is the only change reported, rather than every keyword of the
// old type that the new one does not have.
func TestDisjointTypesStopTheComparison(t *testing.T) {
	t.Parallel()
	old := compatObj(map[string]*Schema{"a": compatStr()}, "a")
	cur := &Schema{Type: "string", MaxLength: compatPtr(3)}
	assertAPIChanges(t, CompareDocuments(compatAnswerDoc(old, nil), compatAnswerDoc(cur, nil)),
		wantChange{Breaking, "response-type-changed", responseBodyAt + "/type"})
}

// TestTypeChangeKeepsComparingWhatOverlaps checks that a widened type still
// has its shared keywords compared.
func TestTypeChangeKeepsComparingWhatOverlaps(t *testing.T) {
	t.Parallel()
	old := &Schema{Type: "string", MaxLength: compatPtr(3)}
	cur := &Schema{Type: []string{"string", "integer"}, MaxLength: compatPtr(2)}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(old, nil), compatReadDoc(cur, nil)),
		wantChange{Compatible, "request-type-widened", requestBodyAt + "/type"},
		wantChange{Breaking, "request-max-length-tightened", requestBodyAt + "/maxLength"})
}

func TestJSONTypesString(t *testing.T) {
	t.Parallel()
	for set, text := range map[jsonTypes]string{
		allTypes:                    "any type",
		0:                           "null alone",
		typeInteger:                 "integer",
		typeInteger | typeFraction:  "number",
		typeFraction:                "number",
		typeString | typeBoolean:    "string or boolean",
		typeObject | typeArray:      "object or array",
		typeString | typeInteger:    "string or integer",
		allTypes &^ typeObject:      "string or number or boolean or array",
		typeArray | typeFraction:    "number or array",
		typeBoolean | typeInteger:   "integer or boolean",
		typeObject | typeString:     "string or object",
		typeArray | typeString:      "string or array",
		typeObject | typeBoolean:    "boolean or object",
		typeBoolean:                 "boolean",
		typeString | typeArray | 64: "string or array",
	} {
		if got := set.String(); got != text {
			t.Errorf("%08b = %q, want %q", set, got, text)
		}
	}
}

// TestUnreadableTypeSaysNothing covers a type keyword a Document built in Go
// holds as something other than a name or a list of them, and unknown names
// in a list: neither constrains anything.
func TestUnreadableTypeSaysNothing(t *testing.T) {
	t.Parallel()
	odd := &Schema{Type: 42}
	if !typeAdmitsNull(odd.Type) {
		t.Error("an unreadable type should be taken to admit null")
	}
	if set, typed := typesOf(odd); typed || set != allTypes {
		t.Errorf("typesOf(42) = %v, %v", set, typed)
	}
	if set, _ := typesOf(&Schema{Type: []any{"string", 7, "spaceship"}}); set != typeString {
		t.Errorf("typesOf([string 7 spaceship]) = %v, want string", set)
	}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(odd, nil), compatReadDoc(&Schema{}, nil)))
}

// TestEnumsCompareByJSONValue checks that a value a Go program put in a
// document and the same value read back from JSON are the same value, and
// that null is left to nullability.
func TestEnumsCompareByJSONValue(t *testing.T) {
	t.Parallel()
	type color string
	old := &Schema{Type: []string{"string", "integer", "null"}, Enum: []any{color("red"), 3, int64(1) << 53, nil}}
	cur := &Schema{Type: []string{"string", "integer", "null"}, Enum: []any{"red", 3.0, float64(int64(1) << 53), nil}}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(old, nil), compatReadDoc(cur, nil)))

	// Two enums limit a value to what both allow.
	both := &Schema{AllOf: []*Schema{{Enum: []any{"a", "b"}}, {Enum: []any{"b", "c"}}}}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(both, nil), compatReadDoc(&Schema{Enum: []any{"b"}}, nil)))
	// An enum nothing satisfies says so.
	none := CompareDocuments(compatReadDoc(compatStr(), nil), compatReadDoc(&Schema{Type: "string", Enum: []any{}}, nil))
	assertAPIChanges(t, none, wantChange{Breaking, "request-enum-added", requestBodyAt + "/enum"})
	if !strings.Contains(none[0].Message, "no value at all") {
		t.Errorf("message = %q", none[0].Message)
	}
}

// TestLongValueListsAreShortened checks that a message stays one sentence
// however many values changed.
func TestLongValueListsAreShortened(t *testing.T) {
	t.Parallel()
	old := &Schema{Type: "string", Enum: []any{"a"}}
	cur := &Schema{Type: "string", Enum: []any{"a", "b", "c", "d", "e", "f", "g", "h"}}
	got := CompareDocuments(compatReadDoc(old, nil), compatReadDoc(cur, nil))
	assertAPIChanges(t, got, wantChange{Compatible, "request-enum-values-added", requestBodyAt + "/enum"})
	if !strings.Contains(got[0].Message, `"b", "c", "d", "e", "f" and 2 more`) {
		t.Errorf("message = %q", got[0].Message)
	}
}

func TestCanonicalValue(t *testing.T) {
	t.Parallel()
	type label string
	tests := []struct {
		value any
		want  string
	}{
		{nil, "null"},
		{"x", `"x"`},
		{true, "true"},
		{3.0, "3"},
		{int8(3), "3"},
		{label("x"), `"x"`},
		{map[string]any{"b": 1, "a": []int{2}}, `{"a":[2],"b":1}`},
		{compatHugeNumber{}, "1e400"},
		{make(chan int), "chan int"},
		{"\xff", "string(\xff)"},
	}
	for _, tt := range tests {
		got := canonicalValue(tt.value)
		if !strings.HasPrefix(got, tt.want) {
			t.Errorf("canonicalValue(%#v) = %q, want %q", tt.value, got, tt.want)
		}
	}
}

// compatHugeNumber marshals as a number JSON can write but a float cannot hold.
type compatHugeNumber struct{}

func (compatHugeNumber) MarshalJSON() ([]byte, error) { return []byte("1e400"), nil }

func TestIsMultipleOf(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		a, b float64
		want bool
	}{
		{4, 2, true}, {2, 4, false}, {0.3, 0.1, true}, {1, 0, false}, {1e308, 1e-308, false}, {6, 1.5, true},
	} {
		if got := isMultipleOf(tt.a, tt.b); got != tt.want {
			t.Errorf("isMultipleOf(%v, %v) = %v", tt.a, tt.b, got)
		}
	}
}

func TestChangeSeverityString(t *testing.T) {
	t.Parallel()
	for severity, text := range map[ChangeSeverity]string{
		Compatible: "compatible", PossiblyBreaking: "possibly breaking", Breaking: "breaking", 0: "ChangeSeverity(0)", 9: "ChangeSeverity(9)",
	} {
		if got := severity.String(); got != text {
			t.Errorf("%d.String() = %q, want %q", int(severity), got, text)
		}
	}
	if Breaking <= PossiblyBreaking || PossiblyBreaking <= Compatible || Compatible <= 0 {
		t.Error("the severities should order from the least to the most serious, with the zero value below all of them")
	}
}

// TestWriteChanges checks the report: grouped by severity, one change per
// line, and nothing a terminal would act on.
func TestWriteChanges(t *testing.T) {
	t.Parallel()
	var empty bytes.Buffer
	if err := WriteChanges(&empty, nil); err != nil || empty.String() != "No changes.\n" {
		t.Errorf("empty report = %q, %v", empty.String(), err)
	}

	changes := []APIChange{
		{Severity: Compatible, Kind: "path-added", Location: "/paths/~1b", Message: "The path \"/b\" is new."},
		{Severity: Breaking, Kind: "path-removed", Location: "/paths/~1a", Message: "The path \"/a\" is no longer served."},
		{Severity: 0, Kind: "made-up", Message: "Built by hand."},
		{Severity: PossiblyBreaking, Kind: "operation-id-changed", Location: "/paths/~1c\x1b[31m/get", Message: "Red \x1b[31mtext\xff\u200b."},
		{Severity: Breaking, Kind: "comparison-incomplete", Message: "Incomplete."},
	}
	var report bytes.Buffer
	if err := WriteChanges(&report, changes); err != nil {
		t.Fatal(err)
	}
	wantReport := "Breaking (2):\n" +
		"  /paths/~1a: The path \"/a\" is no longer served. [path-removed]\n" +
		"  Incomplete. [comparison-incomplete]\n" +
		"Possibly breaking (1):\n" +
		"  /paths/~1c\\x1b[31m/get: Red \\x1b[31mtext\\xff\\u200b. [operation-id-changed]\n" +
		"Compatible (1):\n" +
		"  /paths/~1b: The path \"/b\" is new. [path-added]\n" +
		"Unclassified (1):\n" +
		"  Built by hand. [made-up]\n"
	if report.String() != wantReport {
		t.Errorf("report:\n%s\nwant:\n%s", report.String(), wantReport)
	}
	if got := changes[3].String(); got != "possibly breaking: /paths/~1c\\x1b[31m/get: Red \\x1b[31mtext\\xff\\u200b. [operation-id-changed]" {
		t.Errorf("String() = %q", got)
	}
	if err := WriteChanges(failingWriter{}, changes); !errors.Is(err, errWriteFailed) {
		t.Errorf("WriteChanges to a failing writer = %v", err)
	}
}

func TestPointerToken(t *testing.T) {
	t.Parallel()
	for in, out := range map[string]string{"plain": "plain", "/a/b": "~1a~1b", "~x/": "~0x~1", "": ""} {
		if got := pointerToken(in); got != out {
			t.Errorf("pointerToken(%q) = %q, want %q", in, got, out)
		}
	}
}

// TestOddShapesOfAGoDocument covers shapes only a Document built in Go can
// take, since ReadDocument refuses them: a position with no schema, and a
// member whose schema is nil.
func TestOddShapesOfAGoDocument(t *testing.T) {
	t.Parallel()
	o := compatOp("list")
	o.Parameters = []Parameter{{Name: "q", In: "query"}}
	typed := compatOp("list")
	typed.Parameters = []Parameter{{Name: "q", In: "query", Schema: &Schema{Type: "boolean"}}}
	at := "/paths/~1items/get/parameters/query/q/schema"
	assertAPIChanges(t, CompareDocuments(compatDoc(map[string]*PathItem{"/items": {Get: o}}, nil), compatDoc(map[string]*PathItem{"/items": {Get: typed}}, nil)),
		wantChange{Breaking, "request-type-narrowed", at + "/type"},
		wantChange{Breaking, "request-nullable-removed", at},
	)

	nilMember := &Schema{Type: "object", Properties: map[string]*Schema{"a": nil, "b": compatStr()}, Required: []string{"a", "b", "b"}}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(nilMember, nil), compatReadDoc(compatObj(map[string]*Schema{"a": {}, "b": compatStr()}), nil)),
		wantChange{Compatible, "request-property-became-optional", requestBodyAt + "/properties/a"},
		wantChange{Compatible, "request-property-became-optional", requestBodyAt + "/properties/b"},
	)
}

// TestBothNumberBoundsAtOnce covers a schema setting an inclusive and an
// exclusive bound together, where the stricter of the two is the bound.
func TestBothNumberBoundsAtOnce(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		inclusive, exclusive float64
		lower                bool
		value                float64
		isExclusive          bool
	}{
		{10, 5, true, 10, false},
		{5, 10, true, 10, true},
		{5, 10, false, 5, false},
		{5, 5, true, 5, true},
	} {
		value, exclusive, set := floatBound(&tt.inclusive, &tt.exclusive, tt.lower)
		if !set || value != tt.value || exclusive != tt.isExclusive {
			t.Errorf("floatBound(%v, %v, %v) = %v, %v, %v", tt.inclusive, tt.exclusive, tt.lower, value, exclusive, set)
		}
	}
}
