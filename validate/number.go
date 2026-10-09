package validate

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
)

// NumberRules collects the checks applied to a numeric field.
//
// The rule set is not generic over the field's own type: bounds are written as
// ordinary untyped constants, so Between(18, 120) reads the same whether the
// field is an int, an int64 or a float64. The entry point that produces the
// rule set is what enforces that the field is numeric at all, so asking for
// Between on a string does not compile.
//
// A field of an integer type is compared with its bounds exactly, whatever its
// size: an int64 of 2^53 + 1 is over Max(1 << 53), although the two are the
// same number once both are a float64. A float field is compared as floating
// point. The bounds themselves are float64, so one written beyond 2^53 is
// rounded to the nearest float64 where it is declared, and the value is then
// compared exactly with that. Two things still see an integer through float64:
// a fractional MultipleOf step, and the function given to Must.
type NumberRules struct {
	target   any
	label    string
	required bool
	steps    []step[float64]
}

// Number returns an unbound rule set, for reuse across models.
func Number() *NumberRules { return &NumberRules{} }

// For binds the rule set to a field, identified by its address. See
// [StringRules.For].
func (r *NumberRules) For(target any) *NumberRules {
	r.target = target
	return r
}

// Reset returns the rule set to its unbound, ruleless state while keeping the
// memory it has already claimed. See [StringRules.Reset].
func (r *NumberRules) Reset() {
	r.target = nil
	r.label = ""
	r.required = false
	clear(r.steps)
	r.steps = r.steps[:0]
}

// Target implements [Evaluator].
func (r *NumberRules) Target() any { return r.target }

// Label implements [Evaluator].
func (r *NumberRules) Label() string { return r.label }

// Describe implements [Evaluator].
func (r *NumberRules) Describe() Constraints {
	c := describeAll(r.steps)
	c.Required = r.required
	return c
}

// Evaluate implements [Evaluator].
func (r *NumberRules) Evaluate() []Problem {
	value, ok := resolve(r.target)
	if !ok {
		return absentProblems(r.target, r.steps)
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return runInteger(value, integer{i: value.Int()}, r.steps)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return runInteger(value, integer{u: value.Uint(), unsigned: true}, r.steps)
	case reflect.Float32, reflect.Float64:
	default:
		// coverage: the entry point constrains the field to a numeric type, so
		// a rule set can only ever be bound to an integer or a float.
		return nil
	}
	number := value.Float()
	// NaN and the infinities parse cleanly out of a path, query, header or form
	// value (strconv.ParseFloat accepts "NaN" and "Inf"), and every comparison
	// against one of them is false. Left unchecked that satisfies Min, Max,
	// Between, Positive, Negative and MultipleOf simultaneously, and even
	// Required, whose zero check "value == 0" is also false for NaN. None of
	// them is a value a numeric field ever legitimately holds, so they are
	// rejected before a single rule runs.
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return []Problem{{Issue: "must be a finite number", Kind: KindNotANumber}}
	}
	original := number
	problems := runNumber(&number, r.steps, r.required)
	// Only a transform (Clamp is the only one) changes the number, and a field
	// is written only when one did.
	if number != original {
		value.SetFloat(number)
	}
	return problems
}

// integer is the value of an integer field, held exactly. Exactly one of i and
// u is in use, which unsigned says.
type integer struct {
	i        int64
	u        uint64
	unsigned bool
}

// unordered is what [integer.compare] reports for a bound that is not a
// number, which no comparison is true of, so that a NaN bound fails nothing for
// an integer just as it fails nothing for a float.
const unordered = 2

// compare reports whether n is below, equal to or above f, as -1, 0 or +1,
// without rounding either side.
//
// A float64 is either beyond the integer's range, which settles the answer, or
// within it, where its integer part converts exactly. When the integer parts
// are equal, the fraction of f decides.
func (n integer) compare(f float64) int {
	if math.IsNaN(f) {
		return unordered
	}
	whole := math.Trunc(f)
	if n.unsigned {
		switch {
		case f < 0:
			return 1
		case f >= 0x1p64:
			return -1
		case n.u < uint64(whole):
			return -1
		case n.u > uint64(whole):
			return 1
		case f > whole:
			return -1
		}
		return 0
	}
	switch {
	case f < -0x1p63:
		return 1
	case f >= 0x1p63:
		return -1
	case n.i < int64(whole):
		return -1
	case n.i > int64(whole):
		return 1
	case f > whole:
		return -1
	case f < whole:
		return 1
	}
	return 0
}

// sign reports whether n is negative, zero or positive, as -1, 0 or +1.
func (n integer) sign() int {
	switch {
	case n.unsigned && n.u > 0, !n.unsigned && n.i > 0:
		return 1
	case !n.unsigned && n.i < 0:
		return -1
	}
	return 0
}

// float is n as a float64, rounded, for the rules that can only see one.
func (n integer) float() float64 {
	if n.unsigned {
		return float64(n.u)
	}
	return float64(n.i)
}

// multipleOf reports whether n is a whole number of factors.
//
// A whole factor divides exactly. A fractional one is judged in floating point
// by [isMultiple], since n divided by it is not an integer question; every
// integer is a multiple of 0.5 whichever way it is asked.
func (n integer) multipleOf(factor float64) bool {
	step := math.Abs(factor)
	switch {
	case factor == 0 || math.IsNaN(factor):
		return false
	case step >= 0x1p64:
		// Past every integer, so only zero is a multiple of it. An infinite
		// factor is one of these.
		return n.sign() == 0
	case step != math.Trunc(step):
		return isMultiple(n.float(), factor)
	}
	magnitude := n.u
	if !n.unsigned {
		// The magnitude of math.MinInt64 does not fit an int64, but it does fit
		// a uint64, which is what the conversion of its two's complement gives.
		magnitude = uint64(n.i) //nolint:gosec // the two's complement is wanted, and negated below
		if n.i < 0 {
			magnitude = -magnitude
		}
	}
	return magnitude%uint64(step) == 0
}

// clampTo is Clamp for an integer: raised to the least integer at or above
// lowest, then lowered to the greatest at or below highest, in that order, as
// min(max(v, lowest), highest) does for a float.
func (n integer) clampTo(lowest, highest float64) integer {
	if n.compare(lowest) == -1 {
		n = integerNear(math.Ceil(lowest), n.unsigned)
	}
	if n.compare(highest) == 1 {
		n = integerNear(math.Floor(highest), n.unsigned)
	}
	return n
}

// integerNear returns the integer of the given signedness nearest a whole
// float64, which is the float itself when it is in range and the end of the
// range when it is not.
func integerNear(f float64, unsigned bool) integer {
	if unsigned {
		switch {
		case f <= 0:
			return integer{unsigned: true}
		case f >= 0x1p64:
			return integer{u: math.MaxUint64, unsigned: true}
		}
		return integer{u: uint64(f), unsigned: true}
	}
	switch {
	case f < -0x1p63:
		return integer{i: math.MinInt64}
	case f >= 0x1p63:
		return integer{i: math.MaxInt64}
	}
	return integer{i: int64(f)}
}

// store writes n into an integer field, stopping at the end of the field's own
// range: a Clamp whose bounds an int8 cannot hold leaves it at 127 or -128
// rather than wrapping round to a number on the other side.
func (n integer) store(field reflect.Value) {
	bits := field.Type().Bits()
	if n.unsigned {
		field.SetUint(min(n.u, uint64(math.MaxUint64)>>(64-bits)))
		return
	}
	highest := int64(math.MaxInt64 >> (64 - bits))
	field.SetInt(max(min(n.i, highest), -highest-1))
}

// runInteger is [runNumber] for a field of an integer type, comparing exactly
// rather than through float64.
func runInteger(field reflect.Value, n integer, steps []step[float64]) []Problem {
	original := n
	skipOnZero := zeroAdmissible(steps)
	var problems []Problem
	for i := range steps {
		s := &steps[i]
		if s.kind == kindClamp {
			n = n.clampTo(s.lo, s.hi)
			continue
		}
		if s.kind != kindRequired && n.sign() == 0 && skipOnZero {
			continue
		}
		if err := applyIntegerStep(s, n); err != nil {
			problems = []Problem{problemFor(s, err)}
			break
		}
	}
	if n != original {
		n.store(field)
	}
	return problems
}

// applyIntegerStep runs one numeric rule against an integer. It reports the
// same failure [applyNumberStep] would, so the two differ only in what they
// count as a failure.
func applyIntegerStep(s *step[float64], n integer) error {
	var holds bool
	switch s.kind {
	case kindRequired:
		holds = n.sign() != 0
	case kindMin:
		holds = n.compare(s.lo) != -1
	case kindMax:
		holds = n.compare(s.hi) != 1
	case kindBetween:
		holds = n.compare(s.lo) != -1 && n.compare(s.hi) != 1
	case kindPositive:
		holds = n.sign() > 0
	case kindNegative:
		holds = n.sign() < 0
	case kindMultipleOf:
		holds = n.multipleOf(s.lo)
	case kindGreaterThan:
		c := n.compare(s.lo)
		holds = c == 1 || c == unordered
	case kindLessThan:
		c := n.compare(s.hi)
		holds = c == -1 || c == unordered
	case kindNonNegative:
		holds = n.sign() >= 0
	case kindNonPositive:
		holds = n.sign() <= 0
	case kindWhole:
		holds = true
	case kindPort:
		holds = n.compare(1) != -1 && n.compare(65535) != 1
	case kindOneOfNumber:
		holds = slices.ContainsFunc(s.enum, func(item any) bool {
			number, ok := item.(float64)
			return ok && n.compare(number) == 0
		})
	default:
		return s.check(n.float())
	}
	if holds {
		return nil
	}
	return numberFailure(s)
}

// add appends a step and returns the rule set for chaining.
func (r *NumberRules) add(s step[float64]) *NumberRules {
	r.steps = append(r.steps, s)
	return r
}

// As renames the field in the messages this rule set produces.
func (r *NumberRules) As(name string) *NumberRules {
	r.label = name
	return r
}

// Message overrides the wording of the check written immediately before it.
func (r *NumberRules) Message(message string) *NumberRules {
	setMessage(r.steps, message)
	return r
}

// MessageKey overrides the wording of the check written immediately before it
// with a translation key, so that an override is translated like every built-in
// rule rather than fixed in one language.
//
//	v.String(&in.Password).MinLen(12).MessageKey("errors.password.too_short")
//
// Arguments are alternating names and values, and are interpolated alongside
// the ones the rule supplies itself, so the key may use %{count} exactly as
// the built-in message does. The rule's English wording still reaches
// [Problem.Issue], so an application that does not translate reads the same
// sentence it always did.
//
// Use [NumberRules.Message] instead when the wording is fixed in one language on
// purpose.
func (r *NumberRules) MessageKey(key string, args ...any) *NumberRules {
	setMessageKey(r.steps, key, args)
	return r
}

// Required rejects a zero value.
//
// Zero is indistinguishable from absent for a numeric field, so a field that
// may legitimately be zero should be declared as a pointer and left optional
// rather than marked Required.
func (r *NumberRules) Required() *NumberRules {
	r.required = true
	return r.add(step[float64]{kind: kindRequired})
}

// Clamp pulls a value inside a range instead of rejecting it.
//
// It is a transform, so the handler receives the clamped number, and like every
// transform it runs before the checks. Reach for it where a value outside the
// range is a client being imprecise rather than a client being wrong, such as a
// page size that should quietly cap rather than fail:
//
//	v.Number(&in.Limit).Clamp(1, 100)
func (r *NumberRules) Clamp(lowest, highest float64) *NumberRules {
	return r.add(step[float64]{
		kind: kindClamp,
		lo:   lowest,
		hi:   highest,
	})
}

// Min requires the value to be at least min.
func (r *NumberRules) Min(lowest float64) *NumberRules {
	return r.add(step[float64]{
		kind: kindMin,
		lo:   lowest,
	})
}

// Max requires the value to be at most max.
func (r *NumberRules) Max(highest float64) *NumberRules {
	return r.add(step[float64]{
		kind: kindMax,
		hi:   highest,
	})
}

// Between requires the value to fall within an inclusive range, which is the
// pair of bounds written as one rule so that the failure names both.
func (r *NumberRules) Between(lowest, highest float64) *NumberRules {
	return r.add(step[float64]{
		kind: kindBetween,
		lo:   lowest,
		hi:   highest,
	})
}

// Positive requires a value greater than zero.
func (r *NumberRules) Positive() *NumberRules {
	return r.add(step[float64]{kind: kindPositive})
}

// Negative requires a value less than zero.
func (r *NumberRules) Negative() *NumberRules {
	return r.add(step[float64]{kind: kindNegative})
}

// MultipleOf requires the value to divide evenly by factor, for a quantity
// that only makes sense in steps.
//
// A decimal step is judged as the decimal it is written as, not as the binary
// fraction that holds it: 19.99 is a multiple of 0.01 although neither is
// exact as a float64. The quotient may be off from a whole number by only the
// few units in the last place that the arithmetic itself can account for, so
// 5.0000000001 is not a multiple of 5.
func (r *NumberRules) MultipleOf(factor float64) *NumberRules {
	return r.add(step[float64]{
		kind: kindMultipleOf,
		lo:   factor,
	})
}

// multipleTolerance is how far the quotient of a value and its factor may be
// from a whole number, in units of the quotient's own precision, before the
// value is not a multiple. A float64 is exact to about 1.1e-16 relative to
// its magnitude, and the value, the factor and the division each contribute
// half of that, so a few units cover every decimal step honestly written and
// leave nothing for a value that is really off.
const multipleTolerance = 8 * 0x1p-52

// isMultiple reports whether value is a whole number of factors. A factor of
// zero divides nothing, and one that is not finite divides only zero.
func isMultiple(value, factor float64) bool {
	if factor == 0 || math.IsNaN(factor) {
		return false
	}
	if math.IsInf(factor, 0) {
		return value == 0
	}
	quotient := value / factor
	if math.IsInf(quotient, 0) {
		// A value too many steps from zero for the quotient to be held is one
		// for which only an exact remainder can be trusted.
		return math.Mod(value, factor) == 0
	}
	return math.Abs(quotient-math.Round(quotient)) <= multipleTolerance*max(1, math.Abs(quotient))
}

// GreaterThan requires a value strictly above a bound.
//
// It is [NumberRules.Min] with the bound itself excluded. The pair matters more
// than it looks: a price that must be above zero and a quantity that may be
// zero are different rules, and describing one as the other in the generated
// document tells a client the wrong thing.
func (r *NumberRules) GreaterThan(bound float64) *NumberRules {
	return r.add(step[float64]{kind: kindGreaterThan, lo: bound})
}

// LessThan requires a value strictly below a bound, as [NumberRules.Max] with
// the bound excluded.
func (r *NumberRules) LessThan(bound float64) *NumberRules {
	return r.add(step[float64]{kind: kindLessThan, hi: bound})
}

// NonNegative requires zero or more, which is [NumberRules.Positive] with zero
// admitted: a count, a balance or an offset that may legitimately be nothing.
func (r *NumberRules) NonNegative() *NumberRules {
	return r.add(step[float64]{kind: kindNonNegative})
}

// NonPositive requires zero or less.
func (r *NumberRules) NonPositive() *NumberRules {
	return r.add(step[float64]{kind: kindNonPositive})
}

// Whole requires a value with nothing after the decimal point.
//
// An integer field is already whole by its type, so this is for a float that
// carries a count: a number of pages or of items that arrived as JSON, where
// every number is a float until something says otherwise.
func (r *NumberRules) Whole() *NumberRules {
	return r.add(step[float64]{kind: kindWhole})
}

// Port requires a whole number that could be a TCP or UDP port, which is 1 to
// 65535. Nought is excluded because it means "any free port" to the operating
// system rather than a port a client can be told to reach.
func (r *NumberRules) Port() *NumberRules {
	return r.add(step[float64]{kind: kindPort})
}

// OneOf restricts the value to a fixed set, which also becomes the enum in the
// generated documentation.
func (r *NumberRules) OneOf(allowed ...float64) *NumberRules {
	enum := make([]any, len(allowed))
	for i, value := range allowed {
		enum[i] = value
	}
	return r.add(step[float64]{kind: kindOneOfNumber, enum: enum})
}

// Must applies a rule of your own, receiving the value as a float64 whatever
// the field's own numeric type.
func (r *NumberRules) Must(check func(float64) error) *NumberRules {
	return r.add(step[float64]{kind: kindCustom, check: check})
}

// Check applies the rule set to a value and returns the first failure, which is
// what makes a rule set testable on its own.
func (r *NumberRules) Check(value float64) error {
	problems := runNumber(&value, r.steps, r.required)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(problems[0].Issue)
}

// formatNumber renders a bound without a trailing ".0", so an integer limit
// reads as an integer.
func formatNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// applyNumberStep runs one numeric rule. See [applyStringStep] for why the
// rules are dispatched rather than closed over.
//
// Each comparison is written the way round that a NaN bound fails nothing, as
// it always has: "not below the minimum" rather than "at least the minimum".
func applyNumberStep(s *step[float64], value *float64) error {
	v := *value
	var holds bool
	switch s.kind {
	case kindClamp:
		*value = min(max(v, s.lo), s.hi)
		return nil
	case kindRequired:
		holds = v != 0
	case kindMin:
		holds = !(v < s.lo)
	case kindMax:
		holds = !(v > s.hi)
	case kindBetween:
		holds = !(v < s.lo || v > s.hi)
	case kindPositive:
		holds = v > 0
	case kindNegative:
		holds = v < 0
	case kindMultipleOf:
		holds = isMultiple(v, s.lo)
	case kindGreaterThan:
		holds = !(v <= s.lo)
	case kindLessThan:
		holds = !(v >= s.hi)
	case kindNonNegative:
		holds = !(v < 0)
	case kindNonPositive:
		holds = !(v > 0)
	case kindWhole:
		holds = v == math.Trunc(v)
	case kindPort:
		holds = v == math.Trunc(v) && v >= 1 && v <= 65535
	case kindOneOfNumber:
		holds = containsNumber(s.enum, v)
	default:
		return s.check(v)
	}
	if holds {
		return nil
	}
	return numberFailure(s)
}

// numberFailure is the failure a numeric rule reports, which is the same for a
// float and an integer field.
func numberFailure(s *step[float64]) error {
	switch s.kind {
	case kindRequired:
		return errRequired
	case kindMin:
		return fmt.Errorf("must be at least %s", formatNumber(s.lo))
	case kindMax:
		return fmt.Errorf("must be at most %s", formatNumber(s.hi))
	case kindBetween:
		return fmt.Errorf("must be between %s and %s", formatNumber(s.lo), formatNumber(s.hi))
	case kindPositive:
		return errors.New("must be greater than zero")
	case kindNegative:
		return errors.New("must be less than zero")
	case kindMultipleOf:
		return fmt.Errorf("must be a multiple of %s", formatNumber(s.lo))
	case kindGreaterThan:
		return fmt.Errorf("must be greater than %s", formatNumber(s.lo))
	case kindLessThan:
		return fmt.Errorf("must be less than %s", formatNumber(s.hi))
	case kindNonNegative:
		return errors.New("must not be negative")
	case kindNonPositive:
		return errors.New("must not be positive")
	case kindWhole:
		return errors.New("must be a whole number")
	case kindPort:
		return errors.New("must be a port number between 1 and 65535")
	default:
		return fmt.Errorf("must be one of %s", numberList(s.enum))
	}
}

// containsNumber reports whether a value appears in a permitted set.
func containsNumber(allowed []any, value float64) bool {
	for _, item := range allowed {
		if number, ok := item.(float64); ok && number == value {
			return true
		}
	}
	return false
}

// numberList renders a permitted set for an error message.
func numberList(allowed []any) string {
	rendered := make([]string, 0, len(allowed))
	for _, item := range allowed {
		if number, ok := item.(float64); ok {
			rendered = append(rendered, formatNumber(number))
		}
	}
	return joinWithOr(rendered)
}
