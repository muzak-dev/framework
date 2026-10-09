package validate

import (
	"errors"
	"math"
	"testing"
)

// The integers either side of 2^53, the last one a float64 holds exactly. An
// integer above it read as a float64 rounds to an even neighbour, so a bound of
// 2^53 compared through float64 cannot tell 2^53 from 2^53 + 1.
const (
	twoTo53     = 1 << 53
	pastTwoTo53 = twoTo53 + 1
)

// TestNumberRulesCompareIntegersExactly pins the promise the NumberRules
// documentation makes: an integer field is compared with its bounds exactly.
// It was compared through float64, so above 2^53 each of these was off by one.
func TestNumberRulesCompareIntegersExactly(t *testing.T) {
	t.Parallel()
	signed := func(v int64, rules *NumberRules) []Problem { return rules.For(&v).Evaluate() }
	unsigned := func(v uint64, rules *NumberRules) []Problem { return rules.For(&v).Evaluate() }

	for _, tc := range []struct {
		name   string
		got    []Problem
		failed bool
	}{
		{"Max admits one past the bound", signed(pastTwoTo53, Number().Max(twoTo53)), true},
		{"Max admits the bound", signed(twoTo53, Number().Max(twoTo53)), false},
		{"Min admits one below a negative bound", signed(-pastTwoTo53, Number().Min(-twoTo53)), true},
		{"Between admits one past the top", signed(pastTwoTo53, Number().Between(0, twoTo53)), true},
		{"GreaterThan refuses one past the bound", signed(pastTwoTo53, Number().GreaterThan(twoTo53)), false},
		{"LessThan admits the bound", signed(twoTo53, Number().LessThan(twoTo53)), true},
		{"OneOf admits a neighbour", signed(pastTwoTo53, Number().OneOf(twoTo53)), true},
		{"MultipleOf admits an odd number", signed(pastTwoTo53, Number().MultipleOf(2)), true},
		{"MultipleOf of a negative factor", signed(-twoTo53, Number().MultipleOf(-2)), false},
		{"MultipleOf of the smallest int64", signed(math.MinInt64, Number().MultipleOf(4)), false},
		// 2^64 - 1025 rounds down to 2^64 - 2048, the largest float64 below
		// 2^64, which is the bound.
		{"Max on a uint64", unsigned(math.MaxUint64-1024, Number().Max(math.MaxUint64-2047)), true},
		{"MultipleOf on a uint64", unsigned(math.MaxUint64, Number().MultipleOf(2)), true},
		{"Positive on a uint64", unsigned(0, Number().Positive()), true},
		{"Negative on a uint64", unsigned(1, Number().Negative()), true},
		{"NonPositive on a uint64", unsigned(1, Number().NonPositive()), true},
	} {
		if failed := len(tc.got) > 0; failed != tc.failed {
			t.Errorf("%s: problems = %v, want failed = %v", tc.name, tc.got, tc.failed)
		}
	}
}

// TestNumberRulesCompareIntegersWithAnyBound covers a bound no integer type can
// hold, past either end of the range, between two integers or not a number at
// all, so that every comparison has a defined answer.
func TestNumberRulesCompareIntegersWithAnyBound(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, name string, value any, rules *NumberRules, failed bool) {
		t.Helper()
		if got := rules.For(value).Evaluate(); (len(got) > 0) != failed {
			t.Errorf("%s: problems = %v, want failed = %v", name, got, failed)
		}
	}
	i := func(v int64) *int64 { return &v }
	u := func(v uint64) *uint64 { return &v }

	check(t, "int below a bound past int64", i(math.MaxInt64), Number().Max(1e19), false)
	check(t, "int above a bound below int64", i(math.MinInt64), Number().Min(-1e19), false)
	check(t, "int above a fractional bound", i(3), Number().Max(2.5), true)
	check(t, "int below a fractional bound", i(2), Number().Min(2.5), true)
	check(t, "int above a negative fractional bound", i(-2), Number().Max(-2.5), true)
	check(t, "int within fractional bounds", i(3), Number().Between(2.5, 3.5), false)
	check(t, "uint above a negative bound", u(0), Number().Min(-1), false)
	check(t, "uint below a bound past uint64", u(math.MaxUint64), Number().Max(1e20), false)
	check(t, "uint above a fractional bound", u(3), Number().Max(2.5), true)
	check(t, "uint equal to its bound", u(3), Number().Max(3).Min(3), false)
	check(t, "uint below a fractional bound", u(2), Number().Min(2.5), true)
	check(t, "uint at the integer part of a fractional bound", u(2), Number().Max(2.5), false)
	check(t, "a bound that is not a number fails nothing", i(3), Number().Min(math.NaN()).Max(math.NaN()), false)
	check(t, "infinite bounds", i(3), Number().Between(math.Inf(-1), math.Inf(1)), false)

	check(t, "Required on zero", i(0), Number().Required(), true)
	check(t, "Positive", i(-1), Number().Positive(), true)
	check(t, "NonNegative", i(-1), Number().NonNegative(), true)
	check(t, "Whole always holds", i(7), Number().Whole(), false)
	check(t, "Port below", i(0), Number().Port(), true)
	check(t, "Port above", u(65536), Number().Port(), true)
	check(t, "Port", u(443), Number().Port(), false)
	check(t, "OneOf", u(2), Number().OneOf(1, 3), true)
	check(t, "MultipleOf zero", i(4), Number().MultipleOf(0), true)
	check(t, "MultipleOf NaN", i(4), Number().MultipleOf(math.NaN()), true)
	check(t, "MultipleOf infinity", i(0), Number().MultipleOf(math.Inf(1)), false)
	check(t, "MultipleOf a fraction", i(3), Number().MultipleOf(0.5), false)
	check(t, "MultipleOf past every integer", u(math.MaxUint64), Number().MultipleOf(0x1p64), true)
	check(t, "Must sees the value", i(4), Number().Must(func(f float64) error {
		if f != 4 {
			return errors.New("must be four")
		}
		return nil
	}), false)
	check(t, "Must sees an unsigned value", u(4), Number().Must(func(f float64) error {
		if f != 4 {
			return errors.New("must be four")
		}
		return nil
	}), false)
}

// TestNumberRulesOnFloats keeps the floating point path honest now that
// integers no longer take it: every rule, once failing and once passing.
func TestNumberRulesOnFloats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		rules  *NumberRules
		bad    float64
		good   float64
		reason string
	}{
		{"Required", Number().Required(), 0, 1, "is required"},
		{"Min", Number().Min(1.5), 1, 2, "must be at least 1.5"},
		{"Max", Number().Max(1.5), 2, 1, "must be at most 1.5"},
		{"Between", Number().Between(1, 2), 3, 1.5, "must be between 1 and 2"},
		{"Positive", Number().Positive(), -1, 1, "must be greater than zero"},
		{"Negative", Number().Negative(), 1, -1, "must be less than zero"},
		{"MultipleOf", Number().MultipleOf(0.5), 0.3, 1.5, "must be a multiple of 0.5"},
		{"GreaterThan", Number().GreaterThan(1), 1, 1.5, "must be greater than 1"},
		{"LessThan", Number().LessThan(1), 1, 0.5, "must be less than 1"},
		{"NonNegative", Number().NonNegative(), -0.5, 0.5, "must not be negative"},
		{"NonPositive", Number().NonPositive(), 0.5, -0.5, "must not be positive"},
		{"Whole", Number().Whole(), 1.5, 2, "must be a whole number"},
		{"Port", Number().Port(), 80.5, 80, "must be a port number between 1 and 65535"},
		{"OneOf", Number().OneOf(1.5, 2), 3, 2, "must be one of 1.5 or 2"},
	} {
		if err := tc.rules.Check(tc.bad); err == nil || err.Error() != tc.reason {
			t.Errorf("%s.Check(%v) = %v, want %q", tc.name, tc.bad, err, tc.reason)
		}
		if err := tc.rules.Check(tc.good); err != nil {
			t.Errorf("%s.Check(%v) = %v, want it to pass", tc.name, tc.good, err)
		}
	}
}

// TestNumberClampIsExactForIntegers covers the transform: a value past the
// bound by less than float64 can see is still pulled back to it, and one pulled
// towards a bound the field cannot hold stops at the end of its type.
func TestNumberClampIsExactForIntegers(t *testing.T) {
	t.Parallel()
	big := int64(pastTwoTo53)
	Number().Clamp(0, twoTo53).For(&big).Evaluate()
	if big != twoTo53 {
		t.Errorf("Clamp(0, 2^53) left %d, want %d", big, int64(twoTo53))
	}
	small := int64(-5)
	Number().Clamp(2.5, 10).For(&small).Evaluate()
	if small != 3 {
		t.Errorf("Clamp(2.5, 10) gave %d, want 3", small)
	}
	tiny := int8(5)
	Number().Clamp(200, 300).For(&tiny).Evaluate()
	if tiny != math.MaxInt8 {
		t.Errorf("Clamp(200, 300) on an int8 gave %d, want %d", tiny, math.MaxInt8)
	}
	low := int8(5)
	Number().Clamp(-300, -200).For(&low).Evaluate()
	if low != math.MinInt8 {
		t.Errorf("Clamp(-300, -200) on an int8 gave %d, want %d", low, math.MinInt8)
	}
	wide := int64(0)
	Number().Clamp(1e19, 2e19).For(&wide).Evaluate()
	if wide != math.MaxInt64 {
		t.Errorf("Clamp(1e19, 2e19) on an int64 gave %d, want %d", wide, int64(math.MaxInt64))
	}
	under := int64(0)
	Number().Clamp(-2e19, -1e19).For(&under).Evaluate()
	if under != math.MinInt64 {
		t.Errorf("Clamp(-2e19, -1e19) on an int64 gave %d, want %d", under, int64(math.MinInt64))
	}
	byteSized := uint8(5)
	Number().Clamp(300, 400).For(&byteSized).Evaluate()
	if byteSized != math.MaxUint8 {
		t.Errorf("Clamp(300, 400) on a uint8 gave %d, want %d", byteSized, math.MaxUint8)
	}
	huge := uint64(0)
	Number().Clamp(1e20, 2e20).For(&huge).Evaluate()
	if huge != math.MaxUint64 {
		t.Errorf("Clamp(1e20, 2e20) on a uint64 gave %d, want %d", huge, uint64(math.MaxUint64))
	}
	capped := uint64(math.MaxUint64)
	Number().Clamp(-5, 10).For(&capped).Evaluate()
	if capped != 10 {
		t.Errorf("Clamp(-5, 10) on a uint64 gave %d, want 10", capped)
	}
	floored := uint64(0)
	Number().Clamp(-5, -1).For(&floored).Evaluate()
	if floored != 0 {
		t.Errorf("Clamp(-5, -1) on a uint64 gave %d, want 0", floored)
	}
	untouched := int64(pastTwoTo53)
	Number().Clamp(0, 1e19).For(&untouched).Evaluate()
	if untouched != pastTwoTo53 {
		t.Errorf("a Clamp that changes nothing rewrote %d as %d", int64(pastTwoTo53), untouched)
	}
}
