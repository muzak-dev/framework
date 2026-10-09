package validate

import (
	"slices"
	"testing"
)

// TestDescribeHoldsEveryStepOfAChain is the regression test for a rule set
// described by its last step of each kind: Min(10).Min(5) was documented as a
// minimum of 5 while the server refused 7, two Matches documented only the
// second pattern, and two OneOf lists were joined into one that allowed values
// neither rule would.
func TestDescribeHoldsEveryStepOfAChain(t *testing.T) {
	t.Parallel()

	t.Run("bounds keep the tighter of each kind", func(t *testing.T) {
		t.Parallel()
		c := Number().Min(10).Min(5).Max(20).Max(30).Between(0, 25).Describe()
		if c.Minimum == nil || *c.Minimum != 10 || c.Maximum == nil || *c.Maximum != 20 {
			t.Errorf("minimum, maximum = %v, %v, want 10 and 20", deref(c.Minimum), deref(c.Maximum))
		}
		s := String().MinLen(8).MinLen(3).MaxLen(5).Len(6).Describe()
		if s.MinLength == nil || *s.MinLength != 8 || s.MaxLength == nil || *s.MaxLength != 5 {
			t.Errorf("minLength, maxLength = %v, %v, want 8 and 5", deref(s.MinLength), deref(s.MaxLength))
		}
		items := Slice[int]().MaxItems(4).NotEmpty().MinItems(2).MaxItems(9).Describe()
		if items.MinItems == nil || *items.MinItems != 2 || items.MaxItems == nil || *items.MaxItems != 4 {
			t.Errorf("minItems, maxItems = %v, %v, want 2 and 4", deref(items.MinItems), deref(items.MaxItems))
		}
	})

	t.Run("lists keep only what every one permits", func(t *testing.T) {
		t.Parallel()
		c := String().OneOf("a", "b", "c").OneOf("b", "c", "d").Describe()
		if !slices.Equal(c.Enum, []any{"b", "c"}) {
			t.Errorf("enum = %v, want [b c]", c.Enum)
		}
	})

	t.Run("further patterns, formats and multiples are kept", func(t *testing.T) {
		t.Parallel()
		c := String().Matches(`^[A-Z]`).Matches(`[0-9]$`).Matches(`^[A-Z]`).Email().UUID().Describe()
		if c.Pattern != `^[A-Z]` || c.Format != "email" {
			t.Errorf("pattern, format = %q, %q, want the first of each", c.Pattern, c.Format)
		}
		want := []Constraints{{Pattern: `[0-9]$`}, {Format: "uuid"}}
		if len(c.AllOf) != len(want) || c.AllOf[0].Pattern != want[0].Pattern || c.AllOf[1].Format != want[1].Format {
			t.Errorf("allOf = %+v, want the second pattern and the second format once each", c.AllOf)
		}
		n := Number().MultipleOf(3).MultipleOf(5).MultipleOf(3).Describe()
		if n.MultipleOf == nil || *n.MultipleOf != 3 || len(n.AllOf) != 1 || n.AllOf[0].MultipleOf == nil || *n.AllOf[0].MultipleOf != 5 {
			t.Errorf("multipleOf = %v, allOf = %+v, want 3 with 5 beside it", deref(n.MultipleOf), n.AllOf)
		}
		if n.IsZero() || (Constraints{AllOf: []Constraints{{Pattern: "x"}}}).IsZero() {
			t.Error("a set with only further constraints reported itself as saying nothing")
		}
	})
}

// deref renders an optional bound for a failure message.
func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
