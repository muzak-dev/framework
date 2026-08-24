package validate

import (
	"errors"
	"fmt"
	"reflect"
	"time"
)

// ValueRules collects the checks applied to a field of any type.
//
// It is the escape hatch for anything the typed rule sets do not cover: a
// custom type, an enum with its own String method, a struct compared as a
// whole. Because it is generic over the field's type, Must and OneOf take that
// type directly and the compiler checks the values written at the call site.
type ValueRules[T any] struct {
	target   any
	label    string
	required bool
	steps    []step[T]
}

// Value returns an unbound rule set for reuse, or for describing the elements
// of a slice:
//
//	v.Slice(&in.Scores).Each(validate.Value[int]().Must(positive))
func Value[T any]() *ValueRules[T] { return &ValueRules[T]{} }

// For binds the rule set to a field, identified by its address. See
// [StringRules.For].
func (r *ValueRules[T]) For(target *T) *ValueRules[T] {
	r.target = target
	return r
}

// Reset returns the rule set to its unbound, ruleless state while keeping the
// memory it has already claimed. See [StringRules.Reset].
func (r *ValueRules[T]) Reset() {
	r.target = nil
	r.label = ""
	r.required = false
	r.steps = r.steps[:0]
}

// Target implements [Evaluator].
func (r *ValueRules[T]) Target() any { return r.target }

// Label implements [Evaluator].
func (r *ValueRules[T]) Label() string { return r.label }

// Describe implements [Evaluator].
func (r *ValueRules[T]) Describe() Constraints { return r.describe() }

func (r *ValueRules[T]) describe() Constraints {
	c := describeAll(r.steps)
	c.Required = r.required
	return c
}

// Evaluate implements [Evaluator].
func (r *ValueRules[T]) Evaluate() []Problem {
	pointer, ok := r.target.(*T)
	if !ok || pointer == nil {
		// coverage: the entry point always binds a *T of the field's own type.
		return nil
	}
	return r.applyTo(pointer)
}

// applyTo implements [ElementRules].
func (r *ValueRules[T]) applyTo(value *T) []Problem {
	return run(value, r.steps, isZeroValue[T], r.required, customApplier)
}

// isZeroValue reports whether a value counts as absent, which for an arbitrary
// type means the zero value of that type.
func isZeroValue[T any](value T) bool {
	return reflect.ValueOf(&value).Elem().IsZero()
}

// add appends a step and returns the rule set for chaining.
func (r *ValueRules[T]) add(s step[T]) *ValueRules[T] {
	r.steps = append(r.steps, s)
	return r
}

// As renames the field in the messages this rule set produces.
func (r *ValueRules[T]) As(name string) *ValueRules[T] {
	r.label = name
	return r
}

// Message overrides the wording of the check written immediately before it.
func (r *ValueRules[T]) Message(message string) *ValueRules[T] {
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
// Use [ValueRules.Message] instead when the wording is fixed in one language on
// purpose.
func (r *ValueRules[T]) MessageKey(key string, args ...any) *ValueRules[T] {
	setMessageKey(r.steps, key, args)
	return r
}

// Required rejects the zero value of the field's type.
func (r *ValueRules[T]) Required() *ValueRules[T] {
	r.required = true
	return r.add(step[T]{
		kind: kindRequired,
		check: func(value T) error {
			if isZeroValue(value) {
				return errRequired
			}
			return nil
		},
	})
}

// OneOf restricts the value to a fixed set. The type argument is inferred from
// the field, so a value of the wrong type is a compile error rather than a
// check that can never pass.
func (r *ValueRules[T]) OneOf(allowed ...T) *ValueRules[T] {
	enum := make([]any, len(allowed))
	for i, value := range allowed {
		enum[i] = value
	}
	return r.add(step[T]{
		kind: kindOneOfValue,
		enum: enum,
		check: func(value T) error {
			for _, candidate := range allowed {
				if equalValues(value, candidate) {
					return nil
				}
			}
			return fmt.Errorf("must be one of %s", describeList(allowed))
		},
	})
}

// Equal requires the value to match another, which is how a confirmation field
// or a paired setting is checked.
func (r *ValueRules[T]) Equal(other T) *ValueRules[T] {
	return r.add(step[T]{
		kind: kindCustom,
		check: func(value T) error {
			if !equalValues(value, other) {
				return errors.New("does not match")
			}
			return nil
		},
	})
}

// Must applies a rule of your own, receiving the field's own type.
func (r *ValueRules[T]) Must(check func(T) error) *ValueRules[T] {
	return r.add(step[T]{kind: kindCustom, check: check})
}

// Check applies the rule set to a value and returns the first failure.
func (r *ValueRules[T]) Check(value T) error {
	problems := r.applyTo(&value)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(problems[0].Issue)
}

// equalValues compares two values of an arbitrary type.
//
// reflect.DeepEqual is used rather than == because T is not constrained to be
// comparable, and a rule set is more useful over a slice or a struct than it
// would be if those types were excluded.
func equalValues[T any](a, b T) bool {
	return reflect.DeepEqual(a, b)
}

// describeList renders a set of permitted values for an error message.
func describeList[T any](values []T) string {
	rendered := make([]string, len(values))
	for i, value := range values {
		rendered[i] = fmt.Sprintf("%v", value)
	}
	return joinWithOr(rendered)
}

// TimeRules collects the checks applied to a time field.
type TimeRules struct {
	target   any
	label    string
	required bool
	steps    []step[time.Time]
}

// Time returns an unbound rule set for a time field.
func Time() *TimeRules { return &TimeRules{} }

// For binds the rule set to a field, identified by its address. See
// [StringRules.For].
func (r *TimeRules) For(target any) *TimeRules {
	r.target = target
	return r
}

// Reset returns the rule set to its unbound, ruleless state while keeping the
// memory it has already claimed. See [StringRules.Reset].
func (r *TimeRules) Reset() {
	r.target = nil
	r.label = ""
	r.required = false
	r.steps = r.steps[:0]
}

// Target implements [Evaluator].
func (r *TimeRules) Target() any { return r.target }

// Label implements [Evaluator].
func (r *TimeRules) Label() string { return r.label }

// Describe implements [Evaluator].
func (r *TimeRules) Describe() Constraints {
	c := describeAll(r.steps)
	c.Required = r.required
	c.Format = "date-time"
	return c
}

// Evaluate implements [Evaluator].
func (r *TimeRules) Evaluate() []Problem {
	value, ok := resolve(r.target)
	if !ok {
		return nil
	}
	moment, ok := value.Interface().(time.Time)
	if !ok {
		// coverage: the entry point constrains the field to a time.Time.
		return nil
	}
	return run(&moment, r.steps, func(t time.Time) bool { return t.IsZero() }, r.required, customApplier)
}

// add appends a step and returns the rule set for chaining.
func (r *TimeRules) add(s step[time.Time]) *TimeRules {
	r.steps = append(r.steps, s)
	return r
}

// As renames the field in the messages this rule set produces.
func (r *TimeRules) As(name string) *TimeRules {
	r.label = name
	return r
}

// Message overrides the wording of the check written immediately before it.
func (r *TimeRules) Message(message string) *TimeRules {
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
// Use [TimeRules.Message] instead when the wording is fixed in one language on
// purpose.
func (r *TimeRules) MessageKey(key string, args ...any) *TimeRules {
	setMessageKey(r.steps, key, args)
	return r
}

// Required rejects the zero time.
func (r *TimeRules) Required() *TimeRules {
	r.required = true
	return r.add(step[time.Time]{
		kind: kindRequired,
		check: func(t time.Time) error {
			if t.IsZero() {
				return errRequired
			}
			return nil
		},
	})
}

// Before requires a moment strictly earlier than the limit.
func (r *TimeRules) Before(limit time.Time) *TimeRules {
	return r.add(step[time.Time]{
		kind: kindBefore,
		check: func(t time.Time) error {
			if !t.Before(limit) {
				return bounded{
					text: "must be before " + limit.Format(time.RFC3339),
					by:   []any{"time", limit.Format(time.RFC3339)},
				}
			}
			return nil
		},
	})
}

// After requires a moment strictly later than the limit.
func (r *TimeRules) After(limit time.Time) *TimeRules {
	return r.add(step[time.Time]{
		kind: kindAfter,
		check: func(t time.Time) error {
			if !t.After(limit) {
				return bounded{
					text: "must be after " + limit.Format(time.RFC3339),
					by:   []any{"time", limit.Format(time.RFC3339)},
				}
			}
			return nil
		},
	})
}

// Between requires a moment within an inclusive range.
func (r *TimeRules) Between(earliest, latest time.Time) *TimeRules {
	return r.add(step[time.Time]{
		kind: kindTimeBetween,
		check: func(t time.Time) error {
			if t.Before(earliest) || t.After(latest) {
				return bounded{
					text: "must be between " + earliest.Format(time.RFC3339) +
						" and " + latest.Format(time.RFC3339),
					by: []any{
						"earliest", earliest.Format(time.RFC3339),
						"latest", latest.Format(time.RFC3339),
					},
				}
			}
			return nil
		},
	})
}

// Must applies a rule of your own.
func (r *TimeRules) Must(check func(time.Time) error) *TimeRules {
	return r.add(step[time.Time]{kind: kindCustom, check: check})
}

// Check applies the rule set to a value and returns the first failure.
func (r *TimeRules) Check(value time.Time) error {
	problems := run(&value, r.steps, func(t time.Time) bool { return t.IsZero() }, r.required, customApplier)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(problems[0].Issue)
}
