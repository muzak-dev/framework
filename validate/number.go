package validate

import (
	"errors"
	"fmt"
	"math"
	"reflect"
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
// Bounds are compared exactly for integers and as floating point for the rest.
// A bound beyond 2^53 loses precision, which no realistic validation limit
// reaches.
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
		return nil
	}
	number, ok := toFloat(value)
	if !ok {
		// coverage: the entry point constrains the field to a numeric type, so
		// a rule set can only ever be bound to one that converts.
		return nil
	}
	problems := runNumber(&number, r.steps, r.required)
	writeBack(value, number)
	return problems
}

// toFloat reads any numeric kind as a float64.
func toFloat(value reflect.Value) (float64, bool) {
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(value.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(value.Uint()), true
	case reflect.Float32, reflect.Float64:
		return value.Float(), true
	default:
		return 0, false
	}
}

// writeBack stores a possibly transformed number into the field it came from.
func writeBack(value reflect.Value, number float64) {
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if int64(number) != value.Int() {
			value.SetInt(int64(number))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if uint64(number) != value.Uint() {
			value.SetUint(uint64(number))
		}
	default:
		if number != value.Float() {
			value.SetFloat(number)
		}
	}
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
func (r *NumberRules) MultipleOf(factor float64) *NumberRules {
	return r.add(step[float64]{
		kind: kindMultipleOf,
		lo:   factor,
	})
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
func applyNumberStep(s *step[float64], value *float64) error {
	switch s.kind {
	case kindClamp:
		*value = min(max(*value, s.lo), s.hi)
	case kindRequired:
		if *value == 0 {
			return errRequired
		}
	case kindMin:
		if *value < s.lo {
			return fmt.Errorf("must be at least %s", formatNumber(s.lo))
		}
	case kindMax:
		if *value > s.hi {
			return fmt.Errorf("must be at most %s", formatNumber(s.hi))
		}
	case kindBetween:
		if *value < s.lo || *value > s.hi {
			return fmt.Errorf("must be between %s and %s", formatNumber(s.lo), formatNumber(s.hi))
		}
	case kindPositive:
		if *value <= 0 {
			return errors.New("must be greater than zero")
		}
	case kindNegative:
		if *value >= 0 {
			return errors.New("must be less than zero")
		}
	case kindMultipleOf:
		if s.lo == 0 || math.Abs(math.Mod(*value, s.lo)) > 1e-9 {
			return fmt.Errorf("must be a multiple of %s", formatNumber(s.lo))
		}
	default:
		return s.check(*value)
	}
	return nil
}
