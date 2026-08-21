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

// TestResetReturnsARuleSetToTheStart covers the recycling that lets a
// Validation hand the same rule set to one request after another.
func TestResetReturnsARuleSetToTheStart(t *testing.T) {
	t.Parallel()
	text := "value"

	t.Run("string", func(t *testing.T) {
		t.Parallel()
		rules := String().As("label").Required().MinLen(3).For(&text)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Errorf("Reset left state behind: target %v, label %q, %+v",
				rules.Target(), rules.Label(), rules.Describe())
		}
		// The rule set is usable again, and its capacity survived.
		if err := rules.For(&text).MaxLen(1).Evaluate(); len(err) != 1 {
			t.Errorf("a reset rule set did not work again: %v", err)
		}
	})

	t.Run("number", func(t *testing.T) {
		t.Parallel()
		number := 5
		rules := Number().As("label").Required().Min(3).For(&number)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Error("Reset left state behind")
		}
	})

	t.Run("slice", func(t *testing.T) {
		t.Parallel()
		values := []string{"a"}
		rules := Slice[string]().As("label").Required().MaxItems(1).Each(String()).For(&values)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Error("Reset left state behind")
		}
		if !rules.DescribeElement().IsZero() {
			t.Error("Reset left the element rules behind")
		}
	})

	t.Run("value", func(t *testing.T) {
		t.Parallel()
		role := "admin"
		rules := Value[string]().As("label").Required().For(&role)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Error("Reset left state behind")
		}
	})

	t.Run("time", func(t *testing.T) {
		t.Parallel()
		rules := Time().As("label").Required()
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" {
			t.Error("Reset left state behind")
		}
	})
}

// TestOptionalCollectionSkipsItsChecks covers the generic runner's skip, which
// is what makes an absent collection acceptable to a rule set that bounds its
// size.
func TestOptionalCollectionSkipsItsChecks(t *testing.T) {
	t.Parallel()
	if err := Slice[string]().MinItems(2).Check(nil); err != nil {
		t.Errorf("Check(nil) on an optional collection = %v, want it accepted", err)
	}
	if err := Slice[string]().MinItems(2).Check([]string{"a"}); err == nil {
		t.Error("a supplied collection skipped its check")
	}
	if err := Value[string]().Equal("x").Check(""); err != nil {
		t.Errorf("Check on an optional value = %v, want it accepted", err)
	}
}
