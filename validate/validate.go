package validate

import (
	"reflect"
)

// Problem is one thing a rule set found wrong.
//
// Issue is phrased to read after the field name, as in "must be at least 12
// characters", which is the same convention the request binder follows. Path
// is set only for a failure inside a composite value, such as an element of a
// slice or a member of a nested model.
type Problem struct {
	// Issue explains what was wrong with the value.
	Issue string
	// Path locates the failure inside a composite value, as "[2]" for a slice
	// element or ".city" for a nested member. It is empty for a plain field.
	Path string
}

// Constraints records what a rule set demands, so that the generated OpenAPI
// document can describe the same limits the code enforces.
//
// A nil field means the rule set says nothing about that aspect. Only the
// rules that map onto JSON Schema keywords contribute; a Must rule is opaque by
// nature and adds nothing.
type Constraints struct {
	// Required reports whether the field must be present and non-empty.
	Required bool
	// Format is a JSON Schema format such as "email", "uri" or "uuid".
	Format string
	// Pattern is a regular expression the value must match.
	Pattern string
	// MinLength and MaxLength bound a string's length in characters.
	MinLength *int
	MaxLength *int
	// Minimum and Maximum bound a number's value.
	Minimum *float64
	Maximum *float64
	// MultipleOf requires a number to be a multiple of this value.
	MultipleOf *float64
	// MinItems and MaxItems bound a collection's length.
	MinItems *int
	MaxItems *int
	// UniqueItems requires a collection's elements to differ.
	UniqueItems bool
	// Enum lists the permitted values.
	Enum []any
}

// Evaluator is a rule set bound to a field, ready to run.
//
// Badele collects one per field during a call to the model's Validate method
// and then evaluates them all, so the rules a model declares are data rather
// than control flow. The interface is deliberately closed: only this package
// implements it, which keeps the set of rule shapes small enough to reason
// about.
type Evaluator interface {
	// Evaluate applies the transforms and then the checks, returning what
	// failed. An empty result means the field is acceptable.
	Evaluate() []Problem
	// Describe reports the constraints for the OpenAPI document.
	Describe() Constraints
	// Label returns the name the field should be reported under, or the empty
	// string to use the one derived from its struct tag.
	Label() string
	// Target returns the pointer the rule set was bound to, which is how the
	// field it belongs to is identified.
	Target() any
}

// ElementRules is a rule set that can be applied to each element of a
// collection. Only rule sets for the element's own type satisfy it, so
// [SliceRules.Each] cannot be handed rules meant for another type.
type ElementRules[E any] interface {
	applyTo(*E) []Problem
	describe() Constraints
}

// intPtr and floatPtr keep the constraint fields readable at the call site.
func intPtr(v int) *int           { return new(v) }
func floatPtr(v float64) *float64 { return new(v) }

// resolve walks a field pointer down to the value the rules act on.
//
// A field declared as a pointer is optional by construction: when it is nil
// there is no value to check, so the rule set is skipped rather than failed.
// The returned value is settable, which is what lets a transform write back
// through the pointer.
func resolve(target any) (reflect.Value, bool) {
	if target == nil {
		return reflect.Value{}, false
	}
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return reflect.Value{}, false
	}
	rv = rv.Elem()
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return reflect.Value{}, false
		}
		rv = rv.Elem()
	}
	return rv, true
}

// step is one transform or check in a rule set.
//
// Exactly one of change and check is set. Transforms are kept in the same list
// as checks so that the order written is the order applied, which is what makes
// Trim().Required() mean "trim, then insist on something left".
type step[T any] struct {
	// id names the rule for the constraint description and for tests.
	id string
	// change rewrites the value in place.
	change func(T) T
	// check reports what is wrong with the value.
	check func(T) error
	// message overrides the check's own wording.
	message string
	// describe contributes to the OpenAPI constraints.
	describe func(*Constraints)
}

// run applies a list of steps to a value, returning the first failure.
//
// Checks stop at the first failure on purpose: once a field is empty, telling
// the client it is also too short and not an email address adds noise rather
// than information.
func run[T any](value *T, steps []step[T], isEmpty func(T) bool, required bool) []Problem {
	for i := range steps {
		s := &steps[i]
		if s.change != nil {
			*value = s.change(*value)
			continue
		}
		if s.id != requiredRuleID && !required && isEmpty(*value) {
			// An optional field that was not supplied has nothing to check.
			continue
		}
		if s.id != requiredRuleID && required && isEmpty(*value) {
			// Required already reported the emptiness; do not pile on.
			continue
		}
		if err := s.check(*value); err != nil {
			issue := err.Error()
			if s.message != "" {
				issue = s.message
			}
			return []Problem{{Issue: issue}}
		}
	}
	return nil
}

// requiredRuleID marks the presence check, which is the one rule that must run
// against an empty value.
const requiredRuleID = "required"

// describeAll folds every step's contribution into one set of constraints.
func describeAll[T any](steps []step[T]) Constraints {
	var c Constraints
	for i := range steps {
		if steps[i].describe != nil {
			steps[i].describe(&c)
		}
	}
	return c
}

// setMessage attaches an override to the most recently added check.
//
// Transforms are skipped when searching backwards, because a message on
// Trim().Required() plainly belongs to the presence check rather than to the
// trimming.
func setMessage[T any](steps []step[T], message string) {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].check != nil {
			steps[i].message = message
			return
		}
	}
}
