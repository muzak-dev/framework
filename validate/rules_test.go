package validate

import (
	"errors"
	"testing"
)

func TestNumberRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules *NumberRules
		value float64
		issue string
	}{
		{"required present", Number().Required(), 5, ""},
		{"required absent", Number().Required(), 0, "is required"},
		{"min met", Number().Required().Min(3), 3, ""},
		{"min missed", Number().Required().Min(3), 2, "must be at least 3"},
		{"max met", Number().Required().Max(3), 3, ""},
		{"max missed", Number().Required().Max(3), 4, "must be at most 3"},
		{"between", Number().Required().Between(18, 120), 42, ""},
		{"below the range", Number().Required().Between(18, 120), 12, "must be between 18 and 120"},
		{"above the range", Number().Required().Between(18, 120), 130, "must be between 18 and 120"},
		{"positive", Number().Required().Positive(), 1, ""},
		{"not positive", Number().Required().Positive(), -1, "must be greater than zero"},
		{"negative", Number().Required().Negative(), -1, ""},
		{"not negative", Number().Required().Negative(), 1, "must be less than zero"},
		{"multiple", Number().Required().MultipleOf(5), 15, ""},
		{"not a multiple", Number().Required().MultipleOf(5), 16, "must be a multiple of 5"},
		{"a factor of zero divides nothing", Number().Required().MultipleOf(0), 5, "must be a multiple of 0"},
		{"fractional bound", Number().Required().Min(1.5), 1.25, "must be at least 1.5"},
		{"must", Number().Required().Must(func(float64) error { return errors.New("no") }), 1, "no"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.rules.Check(tc.value)
			switch {
			case tc.issue == "" && err != nil:
				t.Errorf("Check(%v) = %v, want it accepted", tc.value, err)
			case tc.issue != "" && err == nil:
				t.Errorf("Check(%v) was accepted, want %q", tc.value, tc.issue)
			case tc.issue != "" && err.Error() != tc.issue:
				t.Errorf("Check(%v) = %q, want %q", tc.value, err, tc.issue)
			}
		})
	}
}

// TestNumberZeroSkipsOnlyWhenTheBoundsAllowIt is the numeric half of "optional
// means optional", and the half that had to be narrowed.
//
// Zero reads as absent for a number, which is a guess: unlike an empty string,
// zero is a value people mean. Skipping every rule on it let
// `Between(1, 720)` accept nought -- a rule set saying in as many words that
// zero is out of range, quietly letting it through -- and made the generated
// document lie, since it advertised the minimum the server did not enforce.
//
// So the skip now applies only where zero would have passed anyway. A field
// that may legitimately be absent *and* whose bounds exclude zero is a pointer,
// which is what it always should have been.
func TestNumberZeroSkipsOnlyWhenTheBoundsAllowIt(t *testing.T) {
	t.Parallel()

	// Bounds that exclude zero now apply to it.
	for name, rules := range map[string]*NumberRules{
		"Min":         Number().Min(10),
		"Between":     Number().Between(1, 720),
		"Positive":    Number().Positive(),
		"GreaterThan": Number().GreaterThan(0),
		"Port":        Number().Port(),
		"OneOf":       Number().OneOf(1, 2, 3),
		"Max below":   Number().Max(-1),
		"Negative":    Number().Negative(),
	} {
		t.Run(name+" rejects zero", func(t *testing.T) {
			if err := rules.Check(0); err == nil {
				t.Error("zero was accepted by a rule set that excludes it")
			}
		})
	}

	// Bounds that admit zero still skip it, so nothing that was optional and
	// coherent has become mandatory.
	for name, rules := range map[string]*NumberRules{
		"Max":           Number().Max(10),
		"Between spans": Number().Between(0, 10),
		"NonNegative":   Number().NonNegative(),
		"MultipleOf":    Number().MultipleOf(5),
		"OneOf with 0":  Number().OneOf(0, 1, 2),
		// A rule of the caller's own is never evaluated speculatively, so a
		// field guarded only by Must behaves exactly as it did before.
		"Must": Number().Must(func(float64) error { return errors.New("never runs on zero") }),
	} {
		t.Run(name+" still skips zero", func(t *testing.T) {
			if err := rules.Check(0); err != nil {
				t.Errorf("zero was rejected by a rule set that admits it: %v", err)
			}
		})
	}

	// A value that was supplied is checked either way, which is the part that
	// never changed.
	if err := Number().Min(10).Check(5); err == nil {
		t.Error("a supplied value skipped its check")
	}
	if err := Number().Min(10).Check(11); err != nil {
		t.Errorf("a valid value was rejected: %v", err)
	}
}

func TestNumberAcrossEveryNumericKind(t *testing.T) {
	t.Parallel()
	rules := Number().Required().Between(1, 10)

	t.Run("signed", func(t *testing.T) {
		t.Parallel()
		value := int16(5)
		if problems := Number().Required().Between(1, 10).For(&value).Evaluate(); len(problems) != 0 {
			t.Errorf("Evaluate = %v", problems)
		}
		tooBig := int16(50)
		if problems := Number().Required().Between(1, 10).For(&tooBig).Evaluate(); len(problems) != 1 {
			t.Errorf("Evaluate = %v, want one failure", problems)
		}
	})

	t.Run("unsigned", func(t *testing.T) {
		t.Parallel()
		value := uint8(5)
		if problems := Number().Required().Between(1, 10).For(&value).Evaluate(); len(problems) != 0 {
			t.Errorf("Evaluate = %v", problems)
		}
	})

	t.Run("float", func(t *testing.T) {
		t.Parallel()
		value := 2.5
		if problems := Number().Required().Between(1, 10).For(&value).Evaluate(); len(problems) != 0 {
			t.Errorf("Evaluate = %v", problems)
		}
	})

	t.Run("pointer", func(t *testing.T) {
		t.Parallel()
		number := 5
		field := &number
		if problems := Number().Required().Between(1, 10).For(&field).Evaluate(); len(problems) != 0 {
			t.Errorf("Evaluate = %v", problems)
		}
		var absent *int
		if problems := Number().Between(1, 10).For(&absent).Evaluate(); len(problems) != 0 {
			t.Errorf("a nil pointer field reported %v, want it skipped", problems)
		}
	})

	if problems := rules.Evaluate(); len(problems) != 0 {
		t.Errorf("an unbound rule set reported %v", problems)
	}
}

// TestNumberClampWritesBack covers the transform and the path that stores what
// it changed, across each numeric family.
func TestNumberClampWritesBack(t *testing.T) {
	t.Parallel()
	signed := 1
	Number().Clamp(10, 20).For(&signed).Evaluate()
	if signed != 10 {
		t.Errorf("signed = %d, want it clamped up to 10", signed)
	}

	unsigned := uint(50)
	Number().Clamp(10, 20).For(&unsigned).Evaluate()
	if unsigned != 20 {
		t.Errorf("unsigned = %d, want it clamped down to 20", unsigned)
	}

	float := 1.0
	Number().Clamp(10, 20).For(&float).Evaluate()
	if float != 10 {
		t.Errorf("float = %v, want it clamped up to 10", float)
	}

	// A value already inside the range is left alone.
	inside := 15
	Number().Clamp(10, 20).For(&inside).Evaluate()
	if inside != 15 {
		t.Errorf("inside = %d, want it untouched", inside)
	}

	// Clamping runs before the checks, so a clamped value satisfies them.
	if err := Number().Clamp(1, 100).Between(1, 100).Check(500); err != nil {
		t.Errorf("Check = %v, want the clamped value accepted", err)
	}

	c := Number().Clamp(1, 100).Describe()
	if c.Minimum == nil || *c.Minimum != 1 || c.Maximum == nil || *c.Maximum != 100 {
		t.Errorf("Clamp did not describe its range: %+v", c)
	}
}

func TestNumberDescribeAndLabel(t *testing.T) {
	t.Parallel()
	c := Number().Required().Between(1, 10).MultipleOf(2).Describe()
	if !c.Required || c.Minimum == nil || *c.Minimum != 1 || c.Maximum == nil || *c.Maximum != 10 {
		t.Errorf("constraints = %+v", c)
	}
	if c.MultipleOf == nil || *c.MultipleOf != 2 {
		t.Errorf("MultipleOf = %v", c.MultipleOf)
	}
	// Positive rejects zero, so the bound it describes is exclusive. Describing
	// it as "minimum: 0" would tell a client that zero is allowed, which is the
	// opposite of what the rule enforces.
	if got := Number().Positive().Describe(); got.ExclusiveMinimum == nil || *got.ExclusiveMinimum != 0 {
		t.Errorf("Positive did not describe an exclusive lower bound: %+v", got)
	}
	if got := Number().Positive().Describe(); got.Minimum != nil {
		t.Errorf("Positive described an inclusive lower bound as well: %+v", got)
	}
	if got := Number().Negative().Describe(); got.ExclusiveMaximum == nil || *got.ExclusiveMaximum != 0 {
		t.Errorf("Negative did not describe an exclusive upper bound: %+v", got)
	}
	if got := Number().Negative().Describe(); got.Maximum != nil {
		t.Errorf("Negative described an inclusive upper bound as well: %+v", got)
	}
	if got := Number().As("years").Label(); got != "years" {
		t.Errorf("Label = %q", got)
	}
	if got := Number().Required().Message("we need a number").Check(0); got == nil || got.Error() != "we need a number" {
		t.Errorf("Check = %v, want the override", got)
	}
	if Number().Target() != nil {
		t.Error("Target on an unbound rule set is not nil")
	}
}

func TestFormatNumber(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   float64
		want string
	}{
		{18, "18"},
		{-5, "-5"},
		{0, "0"},
		{1.5, "1.5"},
		{1e20, "1e+20"},
	}
	for _, tc := range tests {
		if got := formatNumber(tc.in); got != tc.want {
			t.Errorf("formatNumber(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSliceRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules *SliceRules[string]
		value []string
		issue string
	}{
		{"required present", Slice[string]().Required(), []string{"a"}, ""},
		{"required empty", Slice[string]().Required(), nil, "is required"},
		{"min items met", Slice[string]().Required().MinItems(2), []string{"a", "b"}, ""},
		{"min items missed", Slice[string]().Required().MinItems(2), []string{"a"}, "must have at least 2 items"},
		{"max items met", Slice[string]().Required().MaxItems(2), []string{"a", "b"}, ""},
		{"max items missed", Slice[string]().Required().MaxItems(2), []string{"a", "b", "c"}, "must have at most 2 items"},
		{"singular wording", Slice[string]().Required().MaxItems(1), []string{"a", "b"}, "must have at most 1 item"},
		{"unique", Slice[string]().Required().Unique(), []string{"a", "b"}, ""},
		{"repeated", Slice[string]().Required().Unique(), []string{"a", "a"}, "must not repeat a"},
		{"must", Slice[string]().Required().Must(func([]string) error { return errors.New("no") }), []string{"a"}, "no"},
		{"element rule met", Slice[string]().Required().Each(String().MaxLen(3)), []string{"abc"}, ""},
		{"element rule missed", Slice[string]().Required().Each(String().MaxLen(3)), []string{"abc", "abcd"}, "item 2 must be at most 3 characters"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.rules.Check(tc.value)
			switch {
			case tc.issue == "" && err != nil:
				t.Errorf("Check(%v) = %v, want it accepted", tc.value, err)
			case tc.issue != "" && err == nil:
				t.Errorf("Check(%v) was accepted, want %q", tc.value, tc.issue)
			case tc.issue != "" && err.Error() != tc.issue:
				t.Errorf("Check(%v) = %q, want %q", tc.value, err, tc.issue)
			}
		})
	}
}

func TestSliceEvaluate(t *testing.T) {
	t.Parallel()

	t.Run("element failures carry their position", func(t *testing.T) {
		t.Parallel()
		values := []string{"ok", "far too long", "also far too long"}
		problems := Slice[string]().Each(String().MaxLen(4)).For(&values).Evaluate()
		if len(problems) != 2 {
			t.Fatalf("Evaluate = %v, want two failures", problems)
		}
		if problems[0].Path != "[1]" || problems[1].Path != "[2]" {
			t.Errorf("paths = %q, %q", problems[0].Path, problems[1].Path)
		}
	})

	t.Run("a collection failure stops before the elements", func(t *testing.T) {
		t.Parallel()
		values := []string{"far too long"}
		problems := Slice[string]().MaxItems(0).Each(String().MaxLen(1)).For(&values).Evaluate()
		if len(problems) != 1 || problems[0].Path != "" {
			t.Errorf("Evaluate = %v, want only the collection failure", problems)
		}
	})

	t.Run("element transforms reach the slice", func(t *testing.T) {
		t.Parallel()
		values := []string{"  Padded  "}
		Slice[string]().Each(String().Trim().Lower()).For(&values).Evaluate()
		if values[0] != "padded" {
			t.Errorf("element = %q, want it transformed in place", values[0])
		}
	})

	t.Run("no element rules", func(t *testing.T) {
		t.Parallel()
		values := []string{"anything"}
		if problems := Slice[string]().For(&values).Evaluate(); len(problems) != 0 {
			t.Errorf("Evaluate = %v", problems)
		}
	})

	t.Run("unbound", func(t *testing.T) {
		t.Parallel()
		if problems := Slice[string]().Required().Evaluate(); len(problems) != 0 {
			t.Errorf("Evaluate = %v", problems)
		}
	})
}

func TestSliceDescribe(t *testing.T) {
	t.Parallel()
	rules := Slice[string]().Required().MinItems(1).MaxItems(5).Unique().Each(String().MaxLen(8))

	c := rules.Describe()
	if !c.Required || c.MinItems == nil || *c.MinItems != 1 || c.MaxItems == nil || *c.MaxItems != 5 {
		t.Errorf("constraints = %+v", c)
	}
	if !c.UniqueItems {
		t.Error("UniqueItems is not described")
	}
	element := rules.DescribeElement()
	if element.MaxLength == nil || *element.MaxLength != 8 {
		t.Errorf("element constraints = %+v", element)
	}
	if !Slice[string]().DescribeElement().IsZero() {
		t.Error("a rule set with no element rules described some")
	}
	if got := Slice[string]().As("labels").Label(); got != "labels" {
		t.Errorf("Label = %q", got)
	}
	if got := Slice[string]().Required().Message("give us some").Check(nil); got == nil || got.Error() != "give us some" {
		t.Errorf("Check = %v, want the override", got)
	}
	if Slice[string]().Target() != nil {
		t.Error("Target on an unbound rule set is not nil")
	}
}

// enumRole exercises the generic rule set over a named type.
type enumRole string

func TestValueRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules *ValueRules[enumRole]
		value enumRole
		issue string
	}{
		{"required present", Value[enumRole]().Required(), "admin", ""},
		{"required absent", Value[enumRole]().Required(), "", "is required"},
		{"permitted", Value[enumRole]().Required().OneOf("admin", "viewer"), "viewer", ""},
		{"not permitted", Value[enumRole]().Required().OneOf("admin", "viewer"), "wizard", "must be one of admin or viewer"},
		{"equal", Value[enumRole]().Required().Equal("admin"), "admin", ""},
		{"not equal", Value[enumRole]().Required().Equal("admin"), "viewer", "does not match"},
		{"must", Value[enumRole]().Required().Must(func(enumRole) error { return errors.New("no") }), "admin", "no"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.rules.Check(tc.value)
			switch {
			case tc.issue == "" && err != nil:
				t.Errorf("Check(%v) = %v, want it accepted", tc.value, err)
			case tc.issue != "" && err == nil:
				t.Errorf("Check(%v) was accepted, want %q", tc.value, tc.issue)
			case tc.issue != "" && err.Error() != tc.issue:
				t.Errorf("Check(%v) = %q, want %q", tc.value, err, tc.issue)
			}
		})
	}
}

// TestValueRulesOverAStruct checks that the generic rule set works for a type
// == cannot compare, which is why it uses reflect.DeepEqual.
func TestValueRulesOverAStruct(t *testing.T) {
	t.Parallel()
	type point struct{ Tags []string }

	rules := Value[point]().Required().Equal(point{Tags: []string{"a"}})
	if err := rules.Check(point{Tags: []string{"a"}}); err != nil {
		t.Errorf("Check = %v, want equal structs accepted", err)
	}
	if err := rules.Check(point{Tags: []string{"b"}}); err == nil {
		t.Error("different structs were accepted")
	}
	if err := Value[point]().Required().Check(point{}); err == nil || err.Error() != "is required" {
		t.Errorf("Check = %v, want the zero struct rejected", err)
	}
}

func TestValueEvaluateAndDescribe(t *testing.T) {
	t.Parallel()
	value := enumRole("")
	problems := Value[enumRole]().Required().For(&value).Evaluate()
	if len(problems) != 1 || problems[0].Issue != "is required" {
		t.Errorf("Evaluate = %v", problems)
	}

	c := Value[enumRole]().Required().OneOf("admin", "viewer").Describe()
	if !c.Required || len(c.Enum) != 2 {
		t.Errorf("constraints = %+v", c)
	}
	if got := Value[enumRole]().As("role").Label(); got != "role" {
		t.Errorf("Label = %q", got)
	}
	if got := Value[enumRole]().Required().Message("pick one").Check(""); got == nil || got.Error() != "pick one" {
		t.Errorf("Check = %v, want the override", got)
	}
	if Value[enumRole]().Target() != nil {
		t.Error("Target on an unbound rule set is not nil")
	}
	// An unbound rule set has no value to read.
	if problems := Value[enumRole]().Required().Evaluate(); len(problems) != 0 {
		t.Errorf("Evaluate on an unbound rule set = %v", problems)
	}
}

func TestConstraintsIsZero(t *testing.T) {
	t.Parallel()
	if !(Constraints{}).IsZero() {
		t.Error("the zero Constraints reports itself non-zero")
	}
	tests := []struct {
		name string
		c    Constraints
	}{
		{"required", Constraints{Required: true}},
		{"format", Constraints{Format: "email"}},
		{"pattern", Constraints{Pattern: "^x"}},
		{"min length", Constraints{MinLength: intPtr(1)}},
		{"max length", Constraints{MaxLength: intPtr(1)}},
		{"minimum", Constraints{Minimum: floatPtr(1)}},
		{"maximum", Constraints{Maximum: floatPtr(1)}},
		{"multiple of", Constraints{MultipleOf: floatPtr(1)}},
		{"min items", Constraints{MinItems: intPtr(1)}},
		{"max items", Constraints{MaxItems: intPtr(1)}},
		{"unique items", Constraints{UniqueItems: true}},
		{"enum", Constraints{Enum: []any{"a"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.c.IsZero() {
				t.Errorf("%+v reports itself zero", tc.c)
			}
		})
	}
}

func TestJoinWithOr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   []string
		want string
	}{
		{nil, "no permitted value"},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a or b"},
		{[]string{"a", "b", "c"}, "a, b or c"},
	}
	for _, tc := range tests {
		if got := joinWithOr(tc.in); got != tc.want {
			t.Errorf("joinWithOr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlural(t *testing.T) {
	t.Parallel()
	if got := plural(1, "item"); got != "item" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(2, "item"); got != "items" {
		t.Errorf("plural(2) = %q", got)
	}
}
