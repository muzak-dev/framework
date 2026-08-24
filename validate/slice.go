package validate

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// SliceRules collects the checks applied to a slice field.
//
// Rules about the collection itself, such as how many elements it may hold, sit
// alongside rules about each element, which [SliceRules.Each] supplies:
//
//	v.Slice(&in.Tags).MaxItems(10).Each(validate.String().MaxLen(20))
//
// A failure inside an element is reported against that position, as
// "tags[2]", so a client can tell which one to fix.
type SliceRules[E any] struct {
	target   *[]E
	label    string
	required bool
	steps    []step[[]E]
	element  ElementRules[E]
}

// Slice returns an unbound rule set for a collection of E.
func Slice[E any]() *SliceRules[E] { return &SliceRules[E]{} }

// For binds the rule set to a field, identified by its address. See
// [StringRules.For].
func (r *SliceRules[E]) For(target *[]E) *SliceRules[E] {
	r.target = target
	return r
}

// Reset returns the rule set to its unbound, ruleless state while keeping the
// memory it has already claimed. See [StringRules.Reset].
func (r *SliceRules[E]) Reset() {
	r.target = nil
	r.label = ""
	r.required = false
	r.steps = r.steps[:0]
	r.element = nil
}

// Target implements [Evaluator].
//
// An unbound rule set reports no target at all rather than a typed nil pointer,
// so a caller can test the result against nil and get the answer it expects.
func (r *SliceRules[E]) Target() any {
	if r.target == nil {
		return nil
	}
	return r.target
}

// Label implements [Evaluator].
func (r *SliceRules[E]) Label() string { return r.label }

// Describe implements [Evaluator].
func (r *SliceRules[E]) Describe() Constraints {
	c := describeAll(r.steps)
	c.Required = r.required
	return c
}

// DescribeElement reports the constraints on each element, so the generated
// document can describe the items of an array as well as the array itself. It
// returns the zero Constraints when no element rules were declared.
func (r *SliceRules[E]) DescribeElement() Constraints {
	if r.element == nil {
		return Constraints{}
	}
	return r.element.describe()
}

// Evaluate implements [Evaluator].
func (r *SliceRules[E]) Evaluate() []Problem {
	if r.target == nil {
		// coverage: the entry point always binds a non-nil field pointer.
		return nil
	}
	values := *r.target
	if problems := run(&values, r.steps, isEmptySlice[E], r.required, applySliceStep[E]); len(problems) > 0 {
		return problems
	}
	if r.element == nil {
		return nil
	}

	// Elements are validated in place, so a transform declared on the element
	// rules reaches the slice the handler receives.
	var problems []Problem
	for i := range values {
		for _, problem := range r.element.applyTo(&values[i]) {
			problem.Path = "[" + strconv.Itoa(i) + "]" + problem.Path
			problems = append(problems, problem)
		}
	}
	return problems
}

// isEmptySlice reports whether a collection counts as absent.
func isEmptySlice[E any](values []E) bool { return len(values) == 0 }

// add appends a step and returns the rule set for chaining.
func (r *SliceRules[E]) add(s step[[]E]) *SliceRules[E] {
	r.steps = append(r.steps, s)
	return r
}

// As renames the field in the messages this rule set produces.
func (r *SliceRules[E]) As(name string) *SliceRules[E] {
	r.label = name
	return r
}

// Message overrides the wording of the check written immediately before it.
func (r *SliceRules[E]) Message(message string) *SliceRules[E] {
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
// Use [SliceRules.Message] instead when the wording is fixed in one language on
// purpose.
func (r *SliceRules[E]) MessageKey(key string, args ...any) *SliceRules[E] {
	setMessageKey(r.steps, key, args)
	return r
}

// Required rejects an empty or absent collection.
func (r *SliceRules[E]) Required() *SliceRules[E] {
	r.required = true
	return r.add(step[[]E]{kind: kindRequired})
}

// MinItems requires at least n elements.
func (r *SliceRules[E]) MinItems(n int) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindMinItems, n: n})
}

// MaxItems requires at most n elements.
func (r *SliceRules[E]) MaxItems(n int) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindMaxItems, n: n})
}

// Unique requires every element to differ from every other.
//
// Elements are compared with reflect.DeepEqual, so a slice of structs is
// deduplicated by content rather than by identity.
func (r *SliceRules[E]) Unique() *SliceRules[E] {
	return r.add(step[[]E]{kind: kindUnique})
}

// Each applies a rule set to every element.
//
// Only a rule set for the element's own type satisfies the parameter, so
// Each(validate.String()) on a slice of integers does not compile. Use
// [Value] for element types the typed rule sets do not cover:
//
//	v.Slice(&in.Scores).Each(validate.Value[int]().Must(positive))
func (r *SliceRules[E]) Each(rules ElementRules[E]) *SliceRules[E] {
	r.element = rules
	return r
}

// Must applies a rule of your own to the collection as a whole.
func (r *SliceRules[E]) Must(check func([]E) error) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindCustom, check: check})
}

// Check applies the rule set to a value and returns the first failure.
func (r *SliceRules[E]) Check(values []E) error {
	problems := run(&values, r.steps, isEmptySlice[E], r.required, applySliceStep[E])
	if len(problems) == 0 && r.element != nil {
		for i := range values {
			if elementProblems := r.element.applyTo(&values[i]); len(elementProblems) > 0 {
				return fmt.Errorf("item %d %s", i+1, elementProblems[0].Issue)
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(problems[0].Issue)
}

// joinWithOr renders a list of alternatives as prose.
func joinWithOr(values []string) string {
	switch len(values) {
	case 0:
		return "no permitted value"
	case 1:
		return values[0]
	default:
		return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
	}
}

// applySliceStep runs one collection rule. See [applyStringStep] for why the
// rules are dispatched rather than closed over.
func applySliceStep[E any](s *step[[]E], values *[]E) error {
	switch s.kind {
	case kindRequired:
		if len(*values) == 0 {
			return errRequired
		}
	case kindMinItems:
		if len(*values) < s.n {
			return fmt.Errorf("must have at least %d %s", s.n, plural(s.n, "item"))
		}
	case kindMaxItems:
		if len(*values) > s.n {
			return fmt.Errorf("must have at most %d %s", s.n, plural(s.n, "item"))
		}
	case kindUnique:
		list := *values
		for i := range list {
			for j := i + 1; j < len(list); j++ {
				if reflect.DeepEqual(list[i], list[j]) {
					return repeated{
						text:  fmt.Sprintf("must not repeat %v", list[i]),
						value: fmt.Sprintf("%v", list[i]),
					}
				}
			}
		}
	default:
		return s.check(*values)
	}
	return nil
}
