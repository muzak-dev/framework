package muzak

import (
	"encoding/json/v2"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"
)

// wireMembers marshals a value with encoding/json/v2, the way a response is
// written, and returns the names of the members it carries.
func wireMembers(t *testing.T, value any) []string {
	t.Helper()
	encoded, err := json.Marshal(value, durationJSON)
	if err != nil {
		t.Fatalf("json/v2 refuses %T: %v", value, err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("%s is not an object: %v", encoded, err)
	}
	return slices.Sorted(maps.Keys(object))
}

// describedMembers returns the properties the document gives a struct type.
func describedMembers(t *testing.T, typ reflect.Type) (*Schema, []string) {
	t.Helper()
	builder := newSchemaBuilder()
	schema := builder.resolve(builder.schemaFor(typ))
	if schema == nil {
		t.Fatalf("%v was not described", typ)
	}
	return schema, slices.Sorted(maps.Keys(schema.Properties))
}

type ShadowedBase struct {
	ID   int    `json:"id"`
	Kind string `json:"kind"`
}

// shadowsItsBase declares a member its embedded struct declares too. The outer
// one is shallower, so it is the member, and the embedded one is not encoded.
type shadowsItsBase struct {
	ID string `json:"id"`
	ShadowedBase
}

type LeftHalf struct {
	N int
	L int `json:"l"`
}

type RightHalf struct {
	N string
	R int `json:"r"`
}

// tiedHalves embeds two structs that both declare N at the same depth, neither
// naming it in a tag, so neither is the member and N is not encoded at all.
type tiedHalves struct {
	LeftHalf
	RightHalf
}

type UntaggedHalf struct {
	N bool
}

type TaggedHalf struct {
	N int `json:"N"`
}

// taggedWins ties two members at the same depth, one of them named by its
// tag, which is the one that is kept.
type taggedWins struct {
	UntaggedHalf
	TaggedHalf
}

// TestSchemaResolvesMemberConflictsAsJSONDoes holds the description of a struct
// whose members share a name to what json/v2 writes for it. The outer member
// used to be overwritten by the embedded one, so {"id":"abc"} was documented
// as an integer and listed in required twice.
func TestSchemaResolvesMemberConflictsAsJSONDoes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value any
		want  []string
	}{
		{shadowsItsBase{ID: "abc", ShadowedBase: ShadowedBase{ID: 7, Kind: "k"}}, []string{"id", "kind"}},
		{tiedHalves{}, []string{"l", "r"}},
		{taggedWins{}, []string{"N"}},
	} {
		typ := reflect.TypeOf(tc.value)
		schema, described := describedMembers(t, typ)
		if wire := wireMembers(t, tc.value); !slices.Equal(wire, tc.want) {
			t.Fatalf("%v: json/v2 writes %v, the test expects %v", typ, wire, tc.want)
		}
		if !slices.Equal(described, tc.want) {
			t.Errorf("%v: properties = %v, want %v as json/v2 writes them", typ, described, tc.want)
		}
		if !slices.Equal(schema.Required, slices.Compact(slices.Clone(schema.Required))) {
			t.Errorf("%v: required = %v, which names a member twice", typ, schema.Required)
		}
	}

	schema, _ := describedMembers(t, reflect.TypeFor[shadowsItsBase]())
	if id := schema.Properties["id"]; id == nil || id.Type != "string" {
		t.Errorf("id = %+v, want the outer member, a string", id)
	}
	if !slices.Equal(schema.Required, []string{"id", "kind"}) {
		t.Errorf("required = %v, want [id kind]", schema.Required)
	}
	tagged, _ := describedMembers(t, reflect.TypeFor[taggedWins]())
	if n := tagged.Properties["N"]; n == nil || n.Type != "integer" {
		t.Errorf("N = %+v, want the tagged member, an integer", n)
	}
}

// TestShadowedMemberIsNotABodyMember checks the same resolution on the request
// side: a body that names the shadowed member's type is refused, and the
// document says what is accepted.
func TestShadowedMemberIsNotABodyMember(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/s", func(ctx *Context, in shadowsItsBase) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "POST", "/s", `{"id":"abc"}`), 200)
	assertStatus(t, do(t, app, "POST", "/s", `{"id":5}`), 422)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	body := (&schemaBuilder{schemas: doc.Components.Schemas}).resolve(doc.Paths["/s"].Post.RequestBody.Content["application/json"].Schema)
	if id := body.Properties["id"]; id == nil || id.Type != "string" {
		t.Errorf("id = %+v, want a string", id)
	}
}

type EmbeddedMeta struct {
	X int `json:"x"`
}

type EmbeddedExtra struct {
	Y int `json:"y"`
}

type namedByte byte

// optionsOut carries every json/v2 tag option that changes what the wire
// holds, and the byte array json/v2 encodes as base64.
type optionsOut struct {
	Count     int64            `json:"count,string"`
	Ratio     *float64         `json:"ratio,string"`
	Small     uint8            `json:"small,string"`
	Hash      [4]byte          `json:"hash"`
	Meta      EmbeddedMeta     `json:",embed"`
	Extra     *EmbeddedExtra   `json:",embed"`
	Number    int              `json:"number,omitempty"`
	Flag      bool             `json:"flag,omitempty"`
	Pair      [2]int           `json:"pair,omitempty"`
	Text      string           `json:"text,omitempty"`
	List      []int            `json:"list,omitempty"`
	Zeroable  int              `json:"zeroable,omitzero"`
	NamedByte []namedByte      `json:"named_byte"`
	Rest      map[string]int64 `json:",embed"`
}

// counted holds numbers with the string option, to check the patterns that
// describe them against json/v2 itself.
type counted struct {
	Count int64   `json:"count,string"`
	Small uint8   `json:"small,string"`
	Ratio float64 `json:"ratio,string"`
}

// TestSchemaHonoursJSONTagOptions holds the description of every member to
// what json/v2 writes and reads for it: a number with the string option is a
// quoted number, a byte array is base64, an embedded field's members are
// promoted, omitempty leaves out only what can encode as empty, and the
// members an embedded map collects are any the struct does not name.
func TestSchemaHonoursJSONTagOptions(t *testing.T) {
	t.Parallel()
	value := optionsOut{Rest: map[string]int64{"other": 1}}
	schema, described := describedMembers(t, reflect.TypeFor[optionsOut]())
	wire := wireMembers(t, value)
	wire = slices.DeleteFunc(wire, func(name string) bool { return name == "other" })
	for _, name := range wire {
		if !slices.Contains(described, name) {
			t.Errorf("json/v2 writes %q, which the document does not describe; properties are %v", name, described)
		}
	}
	for _, name := range described {
		if !slices.Contains(wire, name) && !slices.Contains([]string{"y", "text", "list", "zeroable"}, name) {
			t.Errorf("the document describes %q, which json/v2 does not write", name)
		}
	}
	if _, nested := schema.Properties["Meta"]; nested {
		t.Error("an embedded field is described as a member of its own")
	}

	// What omitempty can leave out is optional; a number, a bool and an array
	// of two encode as something that is never empty, so they are always there.
	for _, name := range []string{"number", "flag", "pair", "count", "hash", "small", "x"} {
		if !slices.Contains(schema.Required, name) || !slices.Contains(wire, name) {
			t.Errorf("%q: required = %v and json/v2 writes %v, want it in both", name, schema.Required, wire)
		}
	}
	for _, name := range []string{"text", "list", "zeroable", "y", "ratio"} {
		if slices.Contains(schema.Required, name) {
			t.Errorf("%q is required, but json/v2 can leave it out", name)
		}
	}

	if count := schema.Properties["count"]; count == nil || count.Type != "string" || count.Pattern == "" {
		t.Errorf("count = %+v, want a string holding a number", count)
	}
	if ratio := schema.Properties["ratio"]; ratio == nil || !admitsNull(ratio) || ratio.Pattern == "" {
		t.Errorf("ratio = %+v, want a nullable string holding a number", ratio)
	}
	hash := schema.Properties["hash"]
	if hash == nil || hash.Type != "string" || hash.ContentEncoding != "base64" ||
		hash.MinLength == nil || *hash.MinLength != 8 || hash.MaxLength == nil || *hash.MaxLength != 8 {
		t.Errorf("hash = %+v, want eight characters of base64", hash)
	}
	if named := schema.Properties["named_byte"]; named == nil || named.Type != "array" {
		t.Errorf("named_byte = %+v, want an array, which is what json/v2 makes of a named byte type", named)
	}
	if rest, ok := schema.AdditionalProperties.(*Schema); !ok || rest.Type != "integer" {
		t.Errorf("additionalProperties = %+v, want the embedded map's values", schema.AdditionalProperties)
	}

	countedSchema, _ := describedMembers(t, reflect.TypeFor[counted]())
	for member, texts := range map[string][]string{
		"count": {"5", "-5", "0", "-0", "12345678901", "007", "+5", " 5", "1e2", "5.0", "", "x"},
		"small": {"5", "0", "-1", "05", "1.5"},
		"ratio": {"1.5", "-0.25", "1e3", "2E-2", "0", ".5", "1.", "NaN", "+1", "1e"},
	} {
		property := countedSchema.Properties[member]
		if property == nil || property.Pattern == "" {
			t.Errorf("%s = %+v, want a pattern", member, property)
			continue
		}
		pattern := regexp.MustCompile(property.Pattern)
		for _, text := range texts {
			err := json.Unmarshal([]byte(`{"`+member+`":"`+text+`"}`), new(counted))
			if pattern.MatchString(text) != (err == nil) {
				t.Errorf("%s %q: the pattern says %v, json/v2 says %v", member, text, pattern.MatchString(text), err)
			}
		}
		if err := json.Unmarshal([]byte(`{"`+member+`":5}`), new(counted)); err == nil {
			t.Errorf("%s: json/v2 reads a bare number, so the string option is not what the test assumes", member)
		}
	}

	// A body is read the way the document says it is written.
	app := New(quietOptions())
	app.Post("/o", func(ctx *Context, in optionsOut) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)
	assertStatus(t, do(t, app, "POST", "/o", `{"count":"5","hash":"AQIDBA==","x":1,"y":2,"other":3}`), 200)
	assertStatus(t, do(t, app, "POST", "/o", `{"count":5}`), 422)
	assertStatus(t, do(t, app, "POST", "/o", `{"hash":[1,2,3,4]}`), 422)
	assertStatus(t, do(t, app, "POST", "/o", `{"Meta":{"x":1}}`), 422)
}

// TestTypedDefaultWritesTheValueAsItsSchemaDoes covers every shape a default is
// written in: as the JSON type the schema describes, and as text where the
// schema is text or the default does not parse.
func TestTypedDefaultWritesTheValueAsItsSchemaDoes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ  reflect.Type
		raw  string
		want any
	}{
		{reflect.TypeFor[time.Duration](), "1s", "1s"},
		{reflect.TypeFor[*int](), "3", int64(3)},
		{reflect.TypeFor[[]uint8](), "7", []any{uint64(7)}},
		{reflect.TypeFor[uint16](), "9", uint64(9)},
		{reflect.TypeFor[uint16](), "-9", "-9"},
		{reflect.TypeFor[bool](), "maybe", "maybe"},
	} {
		if got := typedDefault(tc.typ, tc.raw); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("typedDefault(%v, %q) = %#v, want %#v", tc.typ, tc.raw, got, tc.want)
		}
	}

	// A default for a member the schema does not describe, or for a schema
	// with no members, is left out rather than invented.
	builder := newSchemaBuilder()
	builder.applyBodyDefaults(&Schema{Type: "string"}, []bodyDefault{{name: "a", typ: reflect.TypeFor[int](), raw: "1"}})
	object := &Schema{Type: "object", Properties: map[string]*Schema{"b": {Type: "integer"}}}
	builder.applyBodyDefaults(object, []bodyDefault{{name: "a", typ: reflect.TypeFor[int](), raw: "1"}})
	if _, invented := object.Properties["a"]; invented || object.Properties["b"].Default != nil {
		t.Errorf("properties = %+v, want them untouched", object.Properties)
	}
	if got := notNullable(&Schema{Type: []string{"string", "integer"}}); !reflect.DeepEqual(got.Type, []string{"string", "integer"}) {
		t.Errorf("a list of types without null became %+v", got)
	}
}

// formatted carries a format option, which json/v2 in this toolchain refuses
// outright unless an experimental option is set.
type formatted struct {
	When int64 `json:"when,format:unix"`
}

// TestFormatOptionIsRefusedByJSON pins what the document relies on: a struct
// whose field carries a format option cannot be encoded or decoded at all, so
// there is no wire form for the document to describe. Should a toolchain start
// accepting it, this fails, and the schema has to learn what each format means.
func TestFormatOptionIsRefusedByJSON(t *testing.T) {
	t.Parallel()
	if _, err := json.Marshal(formatted{}); err == nil {
		t.Error("json/v2 encodes a format option now; the schema has to describe it")
	}
	if err := json.Unmarshal([]byte(`{"when":1}`), new(formatted)); err == nil {
		t.Error("json/v2 decodes a format option now; the schema has to describe it")
	}
}
