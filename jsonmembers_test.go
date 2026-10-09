package muzak

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"
)

// memberNames lists the members jsonMembers resolves for a type, in order.
func memberNames(t reflect.Type) []string {
	var names []string
	for _, member := range jsonMembers(t).members {
		names = append(names, member.name)
	}
	return names
}

type SelfLoop struct {
	*SelfLoop
	N int `json:"n"`
}

type LeftFallback struct {
	Left map[string]int `json:",embed"`
}

type RightFallback struct {
	Right map[string]int `json:",embed"`
}

// tiedFallbacks embeds two structs that each collect unknown members, at the
// same depth, so neither is the one that does.
type tiedFallbacks struct {
	LeftFallback
	RightFallback
	N int `json:"n"`
}

// rawFallback collects the members it does not name as raw JSON.
type rawFallback struct {
	N    int            `json:"n"`
	Rest jsontext.Value `json:",embed"`
}

// TestJSONMembersAgreeWithJSON checks the resolution against json/v2 itself,
// for the types it encodes: what it writes is what jsonMembers lists.
func TestJSONMembersAgreeWithJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value any
		want  []string
	}{
		{SelfLoop{SelfLoop: &SelfLoop{N: 2}, N: 1}, []string{"n"}},
		{tiedFallbacks{N: 1}, []string{"n"}},
	} {
		typ := reflect.TypeOf(tc.value)
		if wire := wireMembers(t, tc.value); !slices.Equal(wire, tc.want) {
			t.Fatalf("%v: json/v2 writes %v, the test expects %v", typ, wire, tc.want)
		}
		if got := memberNames(typ); !slices.Equal(got, tc.want) {
			t.Errorf("%v: members = %v, want %v", typ, got, tc.want)
		}
	}

	// Two fallbacks at one depth tie, and json/v2 then collects nothing: an
	// unknown member is refused like any other.
	if jsonMembers(reflect.TypeFor[tiedFallbacks]()).fallback != nil {
		t.Error("a fallback was chosen between two at the same depth")
	}
	if err := json.Unmarshal([]byte(`{"other":1}`), new(tiedFallbacks), json.RejectUnknownMembers(true)); err == nil {
		t.Error("json/v2 collected an unknown member in one of two tied fallbacks")
	}

	// A raw value collects anything, so the object takes any member.
	schema, _ := describedMembers(t, reflect.TypeFor[rawFallback]())
	if extra, ok := schema.AdditionalProperties.(*Schema); !ok || !reflect.DeepEqual(extra, &Schema{}) {
		t.Errorf("additionalProperties = %+v, want any value", schema.AdditionalProperties)
	}
	if err := json.Unmarshal([]byte(`{"n":1,"other":[1]}`), new(rawFallback), json.RejectUnknownMembers(true)); err != nil {
		t.Errorf("json/v2 refused a member the raw fallback collects: %v", err)
	}
}

type counter int //nolint:unused // only ever embedded, to prove it is skipped

type extras map[string]any //nolint:unused // only ever embedded, to prove it is skipped

// namedEmbed, unexportedNonStruct and unexportedFallback are types json/v2
// refuses to encode, which jsonMembers still lists the way json/v2 resolves
// them before it refuses.
type namedEmbed struct {
	Meta EmbeddedMeta `json:"meta,embed"`
}

type unexportedNonStruct struct {
	counter     //nolint:unused // present to prove an unexported non-struct is skipped
	X       int `json:"x"`
}

type unexportedFallback struct {
	extras `json:",embed"` //nolint:unused // present to prove an unexported map is skipped
	X      int             `json:"x"`
}

// TestJSONMembersOfTypesJSONRefuses covers the shapes json/v2 reports an error
// for: an embed option given a name is an ordinary member, and an embedded
// field that is neither exported nor a struct is skipped.
func TestJSONMembersOfTypesJSONRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value any
		want  []string
	}{
		{namedEmbed{}, []string{"meta"}},
		{unexportedNonStruct{}, []string{"x"}},
		{unexportedFallback{}, []string{"x"}},
	} {
		typ := reflect.TypeOf(tc.value)
		if _, err := json.Marshal(tc.value); err == nil {
			t.Errorf("%v: json/v2 encodes it now, so the test should compare against what it writes", typ)
		}
		if got := memberNames(typ); !slices.Equal(got, tc.want) {
			t.Errorf("%v: members = %v, want %v", typ, got, tc.want)
		}
	}
	if jsonMembers(reflect.TypeFor[unexportedFallback]()).fallback != nil {
		t.Error("an unexported map was taken to collect unknown members")
	}
}

// omittedOrNot holds a time and an address under omitempty: json/v2 always
// writes the one, and leaves the other out when it is the zero address, which
// writes itself as empty text.
type omittedOrNot struct {
	When time.Time  `json:"when,omitempty"`
	Addr netip.Addr `json:"addr,omitempty"`
	Text string     `json:"text,string"`
}

func TestOmitEmptyFollowsWhatJSONWrites(t *testing.T) {
	t.Parallel()
	value := omittedOrNot{}
	encoded, err := json.Marshal(value, json.StringifyNumbers(false))
	if err == nil {
		t.Fatalf("json/v2 encodes a string with the string option now: %s", encoded)
	}
	type written struct {
		When time.Time  `json:"when,omitempty"`
		Addr netip.Addr `json:"addr,omitempty"`
	}
	if wire := wireMembers(t, written{}); !slices.Equal(wire, []string{"when"}) {
		t.Fatalf("json/v2 writes %v, the test expects only when", wire)
	}
	schema, _ := describedMembers(t, reflect.TypeFor[omittedOrNot]())
	if !slices.Contains(schema.Required, "when") || slices.Contains(schema.Required, "addr") {
		t.Errorf("required = %v, want when and not addr", schema.Required)
	}
	// The string option means nothing for a string, so it is described as one.
	if text := schema.Properties["text"]; text.Type != "string" || text.Pattern != "" {
		t.Errorf("text = %+v, want a plain string", text)
	}
}

// TestJSONFieldNameReadsTheTagAsJSONDoes covers the single-field reading the
// binder uses for body defaults.
func TestJSONFieldNameReadsTheTagAsJSONDoes(t *testing.T) {
	t.Parallel()
	type sample struct {
		Bare     string
		Named    string `json:"named,omitempty"`
		Count    int    `json:"count,omitempty"`
		Hidden   string `json:"-"`
		Located  string `query:"q"`
		Optional *int   `json:"optional"`
	}
	typ := reflect.TypeFor[sample]()
	for _, tc := range []struct {
		field    string
		name     string
		optional bool
	}{
		{"Bare", "Bare", false},
		{"Named", "named", true},
		{"Count", "count", false},
		{"Hidden", "", false},
		{"Located", "", false},
		{"Optional", "optional", true},
	} {
		field, _ := typ.FieldByName(tc.field)
		if name, optional := jsonFieldName(field); name != tc.name || optional != tc.optional {
			t.Errorf("%s: jsonFieldName = %q, %v, want %q, %v", tc.field, name, optional, tc.name, tc.optional)
		}
	}
	if got := newSchemaBuilder().inline(reflect.TypeFor[*int]()); !admitsNull(got) {
		t.Errorf("a pointer is described as %+v, want it nullable", got)
	}
}
