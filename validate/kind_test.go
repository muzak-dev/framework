package validate

import (
	"slices"
	"testing"
	"time"
)

// TestKindsCoversEveryRule checks the enumeration against the table it is
// derived from, and that it is stable.
func TestKindsCoversEveryRule(t *testing.T) {
	t.Parallel()
	kinds := Kinds()
	if len(kinds) == 0 {
		t.Fatal("Kinds returned nothing")
	}
	if !slices.IsSorted(kinds) {
		t.Errorf("Kinds returned %v, want them sorted so that the order is the same on every run", kinds)
	}
	for _, kind := range kindsOf {
		if !slices.Contains(kinds, kind) {
			t.Errorf("Kinds omits %q, which a rule reports", kind)
		}
	}
	// Reported before any rule runs, so it is not in the table but is still a
	// failure the framework has to have a message for.
	if !slices.Contains(kinds, KindNotANumber) {
		t.Error("Kinds omits not_a_number")
	}
	// A rule appearing twice in the table is listed once.
	seen := map[Kind]bool{}
	for _, kind := range kinds {
		if seen[kind] {
			t.Errorf("Kinds lists %q more than once", kind)
		}
		seen[kind] = true
	}
}

// TestFailuresCarryTheirRule checks what each rule reports beyond its English,
// which is what a translation of it is looked up and filled in with.
func TestFailuresCarryTheirRule(t *testing.T) {
	t.Parallel()
	epoch := time.Unix(0, 0).UTC()
	later := epoch.Add(time.Hour)

	cases := []struct {
		name string
		got  []Problem
		kind Kind
		args []any
	}{
		{name: "required", got: String().Required().For(ptr("")).Evaluate(), kind: KindBlank},
		{name: "min length", got: String().MinLen(5).For(ptr("ab")).Evaluate(),
			kind: KindTooShort, args: []any{"count", 5}},
		{name: "max length", got: String().MaxLen(1).For(ptr("ab")).Evaluate(),
			kind: KindTooLong, args: []any{"count", 1}},
		{name: "exact length", got: String().Len(4).For(ptr("ab")).Evaluate(),
			kind: KindWrongLength, args: []any{"count", 4}},
		{name: "email", got: String().Email().For(ptr("nope")).Evaluate(), kind: KindEmail},
		{name: "url", got: String().URL().For(ptr("nope")).Evaluate(), kind: KindURL},
		{name: "uuid", got: String().UUID().For(ptr("nope")).Evaluate(), kind: KindUUID},
		{name: "pattern", got: String().Matches(`^[0-9]+$`).For(ptr("ab")).Evaluate(), kind: KindInvalid},
		{name: "one of", got: String().OneOf("red").For(ptr("blue")).Evaluate(),
			kind: KindInclusion, args: []any{"list", `"red"`}},
		{name: "not one of", got: String().NotOneOf("taken").For(ptr("taken")).Evaluate(), kind: KindExclusion},
		{name: "equal", got: String().Equal("a").For(ptr("b")).Evaluate(), kind: KindConfirmation},
		{name: "prefix", got: String().Prefix("pre").For(ptr("no")).Evaluate(),
			kind: KindPrefix, args: []any{"value", `"pre"`}},
		{name: "suffix", got: String().Suffix("post").For(ptr("no")).Evaluate(),
			kind: KindSuffix, args: []any{"value", `"post"`}},
		{name: "contains", got: String().Contains("mid").For(ptr("no")).Evaluate(),
			kind: KindContains, args: []any{"value", `"mid"`}},
		{name: "minimum", got: Number().Min(10).For(ptr(1)).Evaluate(),
			kind: KindGreaterThanOrEqualTo, args: []any{"count", float64(10)}},
		{name: "maximum", got: Number().Max(1).For(ptr(5)).Evaluate(),
			kind: KindLessThanOrEqualTo, args: []any{"count", float64(1)}},
		{name: "between", got: Number().Between(10, 20).For(ptr(50)).Evaluate(),
			kind: KindBetween, args: []any{"min", "10", "max", "20"}},
		{name: "positive", got: Number().Positive().For(ptr(-1)).Evaluate(), kind: KindPositive},
		{name: "negative", got: Number().Negative().For(ptr(1)).Evaluate(), kind: KindNegative},
		{name: "multiple of", got: Number().MultipleOf(7).For(ptr(5)).Evaluate(),
			kind: KindMultipleOf, args: []any{"count", float64(7)}},
		{name: "not a number", got: Number().For(ptr(mustNaN())).Evaluate(), kind: KindNotANumber},
		{name: "too few items", got: Slice[string]().MinItems(3).For(&[]string{"a"}).Evaluate(),
			kind: KindTooFewItems, args: []any{"count", 3}},
		{name: "too many items", got: Slice[string]().MaxItems(1).For(&[]string{"a", "b"}).Evaluate(),
			kind: KindTooManyItems, args: []any{"count", 1}},
		{name: "repeated", got: Slice[string]().Unique().For(&[]string{"a", "a"}).Evaluate(),
			kind: KindTaken, args: []any{"value", "a"}},
		{name: "before", got: Time().Before(epoch).For(&later).Evaluate(),
			kind: KindBefore, args: []any{"time", epoch.Format(time.RFC3339)}},
		{name: "after", got: Time().After(later).For(&epoch).Evaluate(),
			kind: KindAfter, args: []any{"time", later.Format(time.RFC3339)}},
		{name: "between times", got: Time().Between(epoch, later).For(ptr(later.Add(time.Hour))).Evaluate(),
			kind: KindTimeBetween,
			args: []any{"earliest", epoch.Format(time.RFC3339), "latest", later.Format(time.RFC3339)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.got) != 1 {
				t.Fatalf("the rule reported %d problems, want exactly one", len(tc.got))
			}
			problem := tc.got[0]
			if problem.Issue == "" {
				t.Error("the problem carries no English, which every caller relies on")
			}
			if problem.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", problem.Kind, tc.kind)
			}
			if !sameArgs(problem.Args, tc.args) {
				t.Errorf("Args = %v, want %v", problem.Args, tc.args)
			}
		})
	}
}

// sameArgs compares two alternating name and value lists.
func sameArgs(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestMessageOverridesClearTheRule checks the two ways an override behaves.
//
// A literal message replaces the words and drops the rule, because a caller who
// wrote a sentence asked for that sentence. A key keeps the rule, because the
// key still has to be filled in with what the rule knows.
func TestMessageOverridesClearTheRule(t *testing.T) {
	t.Parallel()

	literal := String().MinLen(5).Message("too wee").For(ptr("ab")).Evaluate()
	if len(literal) != 1 || literal[0].Issue != "too wee" {
		t.Fatalf("Message gave %v, want the sentence it was handed", literal)
	}
	if literal[0].Kind != KindNone || literal[0].Key != "" {
		t.Errorf("Message left %q and %q behind, want both cleared", literal[0].Kind, literal[0].Key)
	}

	keyed := String().MinLen(5).MessageKey("my.key", "extra", "value").For(ptr("ab")).Evaluate()
	if len(keyed) != 1 {
		t.Fatalf("MessageKey gave %v, want one problem", keyed)
	}
	if keyed[0].Key != "my.key" {
		t.Errorf("Key = %q, want the key that was named", keyed[0].Key)
	}
	if keyed[0].Kind != KindTooShort {
		t.Errorf("Kind = %q, want the rule kept so its values can fill the key", keyed[0].Kind)
	}
	if keyed[0].Issue != "must be at least 5 characters" {
		t.Errorf("Issue = %q, want the rule's English left as the fallback", keyed[0].Issue)
	}
	if !sameArgs(keyed[0].Args, []any{"count", 5, "extra", "value"}) {
		t.Errorf("Args = %v, want the rule's own followed by the ones named", keyed[0].Args)
	}
}

// TestMessageKeyOnEveryRuleSet checks that the override exists on all five
// families, since a rule set that lacked it would be the one an application
// happened to need.
func TestMessageKeyOnEveryRuleSet(t *testing.T) {
	t.Parallel()
	epoch := time.Unix(0, 0).UTC()

	cases := map[string][]Problem{
		"string":     String().Required().MessageKey("k").For(ptr("")).Evaluate(),
		"number":     Number().Min(5).MessageKey("k").For(ptr(1)).Evaluate(),
		"slice":      Slice[string]().MinItems(2).MessageKey("k").For(&[]string{"a"}).Evaluate(),
		"value":      Value[string]().OneOf("a").MessageKey("k").For(ptr("b")).Evaluate(),
		"time bound": Time().After(epoch.Add(time.Hour)).MessageKey("k").For(&epoch).Evaluate(),
	}
	for name, problems := range cases {
		if len(problems) != 1 || problems[0].Key != "k" {
			t.Errorf("%s: MessageKey gave %v, want the key recorded", name, problems)
		}
	}
}

// TestMessageKeySkipsTransforms checks that an override attaches to the check
// written before it rather than to a transform, which is the same rule
// [StringRules.Message] follows.
func TestMessageKeySkipsTransforms(t *testing.T) {
	t.Parallel()
	problems := String().Trim().Required().MessageKey("k").For(ptr("   ")).Evaluate()
	if len(problems) != 1 || problems[0].Key != "k" {
		t.Errorf("the override landed on %v, want the presence check", problems)
	}
}

// ptr is the address of a value, for a rule set bound to a literal.
func ptr[T any](v T) *T { return &v }

// mustNaN returns a value no numeric field legitimately holds.
func mustNaN() float64 {
	var zero float64
	return zero / zero
}
