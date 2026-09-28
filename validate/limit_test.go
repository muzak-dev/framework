package validate

import (
	"errors"
	"testing"
	"time"
)

// TestEvaluateUpToStopsAtTheLimit checks the ceiling a caller puts on a
// collection a client chose the length of: the report stops at the limit, and
// so does the work.
func TestEvaluateUpToStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	checked := 0
	values := make([]string, 50)
	for i := range values {
		// A value, because an empty one skips every rule but the presence
		// checks.
		values[i] = "x"
	}
	rules := Slice[string]().For(&values).Each(Value[string]().Must(func(string) error {
		checked++
		return errors.New("is refused")
	}))

	problems := rules.EvaluateUpTo(3)
	if len(problems) != 3 || problems[2].Path != "[2]" {
		t.Errorf("EvaluateUpTo(3) = %+v, want the first three elements", problems)
	}
	if checked != 3 {
		t.Errorf("the rule ran %d times, want 3", checked)
	}

	// A limit below one still reports the first failure, which is what a
	// caller with no room left asks for to learn whether there was one.
	if problems := rules.EvaluateUpTo(0); len(problems) != 1 {
		t.Errorf("EvaluateUpTo(0) = %d problems, want 1", len(problems))
	}

	// Evaluate keeps its meaning: every failing element.
	if problems := rules.Evaluate(); len(problems) != len(values) {
		t.Errorf("Evaluate = %d problems, want %d", len(problems), len(values))
	}
}

// TestResetDropsWhatTheStepsHeld checks that a rule set waiting to be reused
// holds nothing from the request that last used it: truncating the steps alone
// would leave their closures and comparands reachable from the pool.
func TestResetDropsWhatTheStepsHeld(t *testing.T) {
	t.Parallel()
	fail := errors.New("no")

	text := String().Must(func(string) error { return fail })
	text.Reset()
	if held := text.steps[:1][0]; held.check != nil {
		t.Error("a string rule set kept its closure")
	}

	number := Number().Must(func(float64) error { return fail })
	number.Reset()
	if held := number.steps[:1][0]; held.check != nil {
		t.Error("a number rule set kept its closure")
	}

	moment := Time().Must(func(time.Time) error { return fail })
	moment.Reset()
	if held := moment.steps[:1][0]; held.check != nil {
		t.Error("a time rule set kept its closure")
	}

	value := Value[int]().Must(func(int) error { return fail })
	value.Reset()
	if held := value.steps[:1][0]; held.check != nil {
		t.Error("a value rule set kept its closure")
	}

	list := Slice[int]().Must(func([]int) error { return fail })
	list.Reset()
	if held := list.steps[:1][0]; held.check != nil {
		t.Error("a slice rule set kept its closure")
	}
}
