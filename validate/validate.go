package validate

import (
	"errors"
	"reflect"
	"regexp"
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

// IsZero reports whether a set of constraints says nothing at all, which is
// what a rule set of nothing but Must rules produces.
//
// Constraints holds a slice, so it cannot be compared with ==; this is the
// comparison callers actually want anyway, since a nil Enum and an empty one
// mean the same thing.
func (c Constraints) IsZero() bool {
	return !c.Required && c.Format == "" && c.Pattern == "" &&
		c.MinLength == nil && c.MaxLength == nil &&
		c.Minimum == nil && c.Maximum == nil && c.MultipleOf == nil &&
		c.MinItems == nil && c.MaxItems == nil &&
		!c.UniqueItems && len(c.Enum) == 0
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

// ruleKind identifies a built-in rule without a closure.
//
// Carrying the rule's parameters as data rather than capturing them in a
// closure is what lets a rule set be declared without allocating: MinLen(12)
// appends a value to a slice instead of building a function. Only a rule of
// the caller's own, which is a function by definition, keeps a closure.
type ruleKind uint8

const (
	// kindCustom applies the step's check function. It covers Must and every
	// rule whose comparand is of the field's own type, which a shared step
	// cannot carry without being generic over it.
	kindCustom ruleKind = iota
	// kindRequired is the presence check, the one rule that runs against an
	// empty value.
	kindRequired

	// String transforms.
	kindTrim
	kindLower
	kindUpper

	// String checks.
	kindMinLen
	kindMaxLen
	kindLen
	kindEmail
	kindURL
	kindUUID
	kindMatches
	kindOneOfString
	kindNotOneOfString
	kindEqualString
	kindPrefix
	kindSuffix
	kindContains

	// Number transform.
	kindClamp

	// Number checks.
	kindMin
	kindMax
	kindBetween
	kindPositive
	kindNegative
	kindMultipleOf

	// Collection checks.
	kindMinItems
	kindMaxItems
	kindUnique

	// kindOneOfValue restricts a field to a set of values of its own type,
	// which the string list cannot hold.
	kindOneOfValue
)

// step is one transform or check in a rule set.
//
// Transforms are kept in the same list as checks so that the order written is
// the order applied, which is what makes Trim().Required() mean "trim, then
// insist on something left".
//
// The struct is wider than a closure pair would be, and deliberately so: one
// slice of steps costs a single allocation where a closure per rule costs one
// each, and a rule set is rebuilt on every request.
type step[T any] struct {
	// kind selects the built-in rule, or kindCustom to call check.
	kind ruleKind
	// n carries a length or item count.
	n int
	// lo and hi carry numeric bounds.
	lo, hi float64
	// text carries a comparand, prefix, suffix or substring.
	text string
	// list carries a set of permitted or rejected values.
	list []string
	// pattern carries a compiled expression, compiled when the rule was
	// declared rather than when it runs.
	pattern *regexp.Regexp
	// check is the rule's own function, used only by kindCustom.
	check func(T) error
	// message overrides the rule's own wording.
	message string
	// enum carries the permitted values of a rule whose comparands are of the
	// field's own type, which the string list cannot hold.
	enum []any
}

// isTransform reports whether a step rewrites the value rather than judging it.
func (s *step[T]) isTransform() bool {
	switch s.kind {
	case kindTrim, kindLower, kindUpper, kindClamp:
		return true
	default:
		return false
	}
}

// applier applies one step to a value, rewriting it for a transform and
// reporting what is wrong with it for a check.
//
// Each rule set family supplies one as a package-level function rather than a
// closure, so dispatching costs nothing per request.
type applier[T any] func(s *step[T], value *T) error

// run applies a list of steps to a value, returning the first failure.
//
// Checks stop at the first failure on purpose: once a field is empty, telling
// the client it is also too short and not an email address adds noise rather
// than information.
//
// Only the collection, value and time families reach it; strings and numbers
// take the written-out paths above. None of those three families declares a
// transform, so there is no rewriting branch here.
func run[T any](value *T, steps []step[T], isEmpty func(T) bool, required bool, apply applier[T]) []Problem {
	for i := range steps {
		s := &steps[i]
		if s.kind != kindRequired && isEmpty(*value) {
			// An optional field that was not supplied has nothing to check,
			// and a required one has already been reported by kindRequired.
			continue
		}
		if err := apply(s, value); err != nil {
			issue := err.Error()
			if s.message != "" {
				issue = s.message
			}
			return []Problem{{Issue: issue}}
		}
	}
	return nil
}

// Errors the built-in rules report. They are package-level values because the
// same wording is produced on every failure, and building the error once keeps
// a rejected request from allocating one.
var (
	errRequired = errors.New("is required")
	errNoMatch  = errors.New("does not match")
)

// customApplier is the applier for a rule set whose rules are all functions,
// which is what the generic families use.
func customApplier[T any](s *step[T], value *T) error {
	return s.check(*value)
}

// runString and runNumber are the string and numeric halves of [run], written
// out rather than reached through an applier value.
//
// The generic version passes the value pointer into an indirect call, which
// forces the compiler to assume the pointer escapes and so to heap-allocate the
// value on every request. Calling the applier directly keeps it on the stack.
// The duplication buys one fewer allocation per validated field, on the two
// paths that carry almost all the traffic.
func runString(value *string, steps []step[string], required bool) []Problem {
	for i := range steps {
		s := &steps[i]
		if s.isTransform() {
			_ = applyStringStep(s, value)
			continue
		}
		if s.kind != kindRequired && *value == "" {
			continue
		}
		if err := applyStringStep(s, value); err != nil {
			return []Problem{{Issue: issueFor(s, err)}}
		}
	}
	return nil
}

// runNumber is the numeric counterpart to [runString].
func runNumber(value *float64, steps []step[float64], required bool) []Problem {
	for i := range steps {
		s := &steps[i]
		if s.isTransform() {
			_ = applyNumberStep(s, value)
			continue
		}
		if s.kind != kindRequired && *value == 0 {
			continue
		}
		if err := applyNumberStep(s, value); err != nil {
			return []Problem{{Issue: issueFor(s, err)}}
		}
	}
	return nil
}

// issueFor picks the wording a failure is reported with, preferring the
// override a rule set attached over the rule's own message.
func issueFor[T any](s *step[T], err error) string {
	if s.message != "" {
		return s.message
	}
	return err.Error()
}

// describeAll folds every step's contribution into one set of constraints.
//
// What a rule demands is derived from its kind and its parameters, so declaring
// one costs no closure. A rule with nothing a document can express, which is
// every rule of the caller's own, contributes nothing.
func describeAll[T any](steps []step[T]) Constraints {
	var c Constraints
	for i := range steps {
		s := &steps[i]
		switch s.kind {
		case kindRequired:
			c.Required = true
		case kindMinLen:
			c.MinLength = intPtr(s.n)
		case kindMaxLen:
			c.MaxLength = intPtr(s.n)
		case kindLen:
			c.MinLength, c.MaxLength = intPtr(s.n), intPtr(s.n)
		case kindEmail:
			c.Format = "email"
		case kindURL:
			c.Format = "uri"
		case kindUUID:
			c.Format = "uuid"
		case kindMatches:
			c.Pattern = s.text
		case kindMin:
			c.Minimum = floatPtr(s.lo)
		case kindMax:
			c.Maximum = floatPtr(s.hi)
		case kindBetween, kindClamp:
			c.Minimum, c.Maximum = floatPtr(s.lo), floatPtr(s.hi)
		case kindPositive:
			c.Minimum = floatPtr(0)
		case kindNegative:
			c.Maximum = floatPtr(0)
		case kindMultipleOf:
			c.MultipleOf = floatPtr(s.lo)
		case kindMinItems:
			c.MinItems = intPtr(s.n)
		case kindMaxItems:
			c.MaxItems = intPtr(s.n)
		case kindUnique:
			c.UniqueItems = true
		case kindOneOfString:
			for _, value := range s.list {
				c.Enum = append(c.Enum, value)
			}
		case kindOneOfValue:
			c.Enum = append(c.Enum, s.enum...)
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
		if !steps[i].isTransform() {
			steps[i].message = message
			return
		}
	}
}
