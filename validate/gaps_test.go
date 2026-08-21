package validate

import (
	"reflect"
	"testing"
)

// TestDescribeCoversEveryKeyword walks the rules whose only job is to
// contribute a constraint, so that a rule which stops describing itself is
// caught rather than silently dropping out of the generated document.
func TestDescribeCoversEveryKeyword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		got    Constraints
		verify func(Constraints) bool
	}{
		{"min", Number().Min(3).Describe(), func(c Constraints) bool { return c.Minimum != nil && *c.Minimum == 3 }},
		{"max", Number().Max(9).Describe(), func(c Constraints) bool { return c.Maximum != nil && *c.Maximum == 9 }},
		{"url", String().URL().Describe(), func(c Constraints) bool { return c.Format == "uri" }},
		{"uuid", String().UUID().Describe(), func(c Constraints) bool { return c.Format == "uuid" }},
		{"value required", Value[string]().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"string required", String().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"number required", Number().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"slice required", Slice[string]().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"time required", Time().Required().Describe(), func(c Constraints) bool { return c.Required }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !tc.verify(tc.got) {
				t.Errorf("constraints = %+v, want the keyword described", tc.got)
			}
		})
	}
}

// TestRequiredAfterAnotherCheckSkipsIt covers the ordering where a check is
// written before Required and the value turns out to be empty. The earlier
// check has nothing to say, and Required is left to report the emptiness on its
// own rather than both complaining at once.
func TestRequiredAfterAnotherCheckSkipsIt(t *testing.T) {
	t.Parallel()
	err := String().MinLen(5).Required().Check("")
	if err == nil {
		t.Fatal("an empty required value was accepted")
	}
	if err.Error() != "is required" {
		t.Errorf("Check = %q, want only the presence failure", err)
	}
}

// TestToFloatRefusesANonNumber covers the conversion's fallback. The entry
// point constrains a field to a numeric type, so this is only reachable by
// calling the helper directly.
func TestToFloatRefusesANonNumber(t *testing.T) {
	t.Parallel()
	if _, ok := toFloat(reflect.ValueOf("text")); ok {
		t.Error("toFloat accepted a string")
	}
	if value, ok := toFloat(reflect.ValueOf(int8(3))); !ok || value != 3 {
		t.Errorf("toFloat(int8) = %v, %v", value, ok)
	}
	if value, ok := toFloat(reflect.ValueOf(uint16(3))); !ok || value != 3 {
		t.Errorf("toFloat(uint16) = %v, %v", value, ok)
	}
	if value, ok := toFloat(reflect.ValueOf(float32(1.5))); !ok || value != 1.5 {
		t.Errorf("toFloat(float32) = %v, %v", value, ok)
	}
}

// TestBoundTargets checks that each rule set reports the field it was bound to,
// which is how the framework works out the name to report a failure under.
func TestBoundTargets(t *testing.T) {
	t.Parallel()

	text := "value"
	if got := String().For(&text).Target(); got != any(&text) {
		t.Errorf("string target = %v", got)
	}

	number := 1
	if got := Number().For(&number).Target(); got != any(&number) {
		t.Errorf("number target = %v", got)
	}

	values := []string{"a"}
	if got := Slice[string]().For(&values).Target(); got == nil {
		t.Error("slice target is nil after For")
	}

	role := "admin"
	if got := Value[string]().For(&role).Target(); got != any(&role) {
		t.Errorf("value target = %v", got)
	}
}
