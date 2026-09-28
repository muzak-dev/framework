package validate

import (
	"errors"
	"reflect"
	"regexp"
	"slices"
)

// Problem is one thing a rule set found wrong.
//
// Issue is phrased to read after the field name, as in "must be at least 12
// characters", which is the same convention the request binder follows. Path
// is set only for a failure inside a composite value, such as an element of a
// slice or a member of a nested model.
type Problem struct {
	// Issue explains what was wrong with the value, in English. It is always
	// populated, so a caller that does nothing further still has a message.
	Issue string
	// Path locates the failure inside a composite value, as "[2]" for a slice
	// element or ".city" for a nested member. It is empty for a plain field.
	Path string
	// Kind names the rule that failed, which is what a translation of the
	// failure is keyed by. It is [KindNone] for a rule of the caller's own and
	// for one whose wording was overridden with Message, neither of which has a
	// rule for a translator to have translated.
	Kind Kind
	// Key names a translation to render this failure from, set by MessageKey.
	// It wins over Kind when both are present.
	Key string
	// Args carries the values the message interpolates, as alternating names
	// and values: {"count", 12}.
	Args []any
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
	// Minimum and Maximum bound a number's value inclusively.
	Minimum *float64
	Maximum *float64
	// ExclusiveMinimum and ExclusiveMaximum bound it exclusively.
	//
	// They are separate from the inclusive pair because the difference is not
	// cosmetic: a rule that rejects zero described as "minimum: 0" tells a
	// client that zero is allowed, which is the opposite of what it enforces.
	ExclusiveMinimum *float64
	ExclusiveMaximum *float64
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
		c.Minimum == nil && c.Maximum == nil &&
		c.ExclusiveMinimum == nil && c.ExclusiveMaximum == nil &&
		c.MultipleOf == nil &&
		c.MinItems == nil && c.MaxItems == nil &&
		!c.UniqueItems && len(c.Enum) == 0
}

// Evaluator is a rule set bound to a field, ready to run.
//
// Muzak collects one per field during a call to the model's Validate method
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
// there is no value to check, so the rule set is skipped rather than failed,
// with the one exception [absentProblems] makes for Required. The returned
// value is settable, which is what lets a transform write back
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

// absentProblems is what a rule set reports for a field that resolve found
// nothing behind: nothing, unless the set is Required, in which case the field
// is missing and the rule that says so speaks.
//
// A nil pointer skips every rule, since an optional field nobody sent has
// nothing to check, but Required is not a check on a value. It is the
// statement that a value has to be there, and the generated document lists the
// field as required. Skipping it for the one field type whose absence can be
// told from an empty value let `{}` and `{"name":null}` through a
// `Required()` on a `*string`, and handed the handler a nil pointer it had
// been promised was not one.
func absentProblems[T any](target any, steps []step[T]) []Problem {
	if rv := reflect.ValueOf(target); rv.Kind() != reflect.Pointer || rv.IsNil() {
		// The rule set is bound to nothing at all, which the entry points never
		// allow, so there is no field to call missing.
		return nil
	}
	for i := range steps {
		if steps[i].kind == kindRequired {
			return []Problem{problemFor(&steps[i], errRequired)}
		}
	}
	return nil
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

	// Time checks. These carry a closure like a rule of the caller's own does,
	// because a moment cannot live in the numeric fields of a step, but they
	// are named so that their wording is the framework's to translate.
	kindBefore
	kindAfter
	kindTimeBetween
	kindPast
	kindFuture
	kindWithin

	// URLs, narrower than kindURL. A field that names somewhere a client can
	// go usually means a narrower set of schemes than "any absolute URL".
	kindHTTPS
	kindURLScheme

	// Network addresses.
	kindHost
	kindIP
	kindIPv4
	kindIPv6
	kindCIDR
	kindMAC

	// The negative of kindMatches, for a shape a value must not have.
	kindNotMatches

	// Emptiness and the characters a value may hold. These two are the ones
	// that catch real mistakes: a field of spaces satisfies a presence check,
	// and a control character in a value that reaches a header or a log is how
	// an injection starts.
	kindNotBlank
	kindNoControl

	// Character classes.
	kindAlpha
	kindAlphanumeric
	kindNumericString
	kindASCII

	// Formats that parse rather than merely match.
	kindSlug
	kindHex
	kindHexColour
	kindBase64
	kindJSON
	kindSemver
	kindE164
	kindLanguageTag
	kindTimezone
	kindCountryCode
	kindCurrencyCode

	// Comparisons that count bytes rather than characters, and one that
	// ignores case.
	kindEqualFold
	kindMinBytes
	kindMaxBytes

	// Numeric bounds that reject the bound itself, and the two against zero
	// that admit it. Between them these complete a set that had only the
	// inclusive bounds and the strict comparisons against nought.
	kindGreaterThan
	kindLessThan
	kindNonNegative
	kindNonPositive
	kindWhole
	kindPort
	kindOneOfNumber

	// Collection checks.
	kindItems
	kindNotEmpty
	kindContainsItem
	kindExcludesItem
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
	// messageKey overrides the rule's own wording with a translation key, and
	// messageArgs are the values that key interpolates beyond the rule's own.
	messageKey  string
	messageArgs []any
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
// Only the value and time families reach it; strings, numbers and collections
// take written-out paths of their own. Neither of those two families declares
// a transform, so there is no rewriting branch here.
func run[T any](value *T, steps []step[T], isEmpty func(T) bool, required bool, apply applier[T]) []Problem {
	for i := range steps {
		s := &steps[i]
		if !runsOnEmpty(s.kind) && isEmpty(*value) {
			// An optional field that was not supplied has nothing to check,
			// and a required one has already been reported by kindRequired.
			continue
		}
		if err := apply(s, value); err != nil {
			return []Problem{problemFor(s, err)}
		}
	}
	return nil
}

// runsOnEmpty reports whether a rule still applies to a value that was not
// supplied.
//
// Almost every rule is skipped for an empty value, because an optional field
// nobody sent has nothing to check and reporting a format failure for it would
// bury the one message that matters. The exceptions are the rules that are
// about emptiness itself: they exist precisely to say that nothing is not
// acceptable here.
func runsOnEmpty(kind ruleKind) bool {
	switch kind {
	case kindRequired, kindNotBlank, kindNotEmpty:
		return true
	default:
		return false
	}
}

// Errors the built-in rules report. They are package-level values because the
// same wording is produced on every failure, and building the error once keeps
// a rejected request from allocating one.
var (
	errRequired = errors.New("is required")
	errNoMatch  = errors.New("does not match")
	errNotBlank = errors.New("must not be blank")
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
		if !runsOnEmpty(s.kind) && *value == "" {
			continue
		}
		if err := applyStringStep(s, value); err != nil {
			return []Problem{problemFor(s, err)}
		}
	}
	return nil
}

// runNumber is the numeric counterpart to [runString].
//
// The zero skip is narrower here than it is for a string, and the difference is
// the point. "Optional means optional" reads an empty value as an absent one,
// which is fair for a string: a field nobody filled in arrives as "". For a
// number it is a guess, because zero is a value people mean. Skipping every
// rule on it made `Number(&in.Window).Between(1, 720)` accept `0` -- a rule set
// that says in as many words that zero is out of range, quietly letting it
// through, and the caller finding out from a constraint violation two layers
// down instead of a 422. It also made the generated document lie: `minimum: 18`
// with a server that accepts nought.
//
// So the skip applies only when zero would have satisfied the rules anyway. If
// the bounds exclude it, the field cannot be optional *with that value*, and
// the rules run and say so in their own words. See [zeroAdmissible].
func runNumber(value *float64, steps []step[float64], required bool) []Problem {
	skipOnZero := zeroAdmissible(steps)

	for i := range steps {
		s := &steps[i]
		if s.isTransform() {
			_ = applyNumberStep(s, value)
			continue
		}
		if s.kind != kindRequired && *value == 0 && skipOnZero {
			continue
		}
		if err := applyNumberStep(s, value); err != nil {
			return []Problem{problemFor(s, err)}
		}
	}
	return nil
}

// zeroAdmissible reports whether zero satisfies every bound the rule set
// declares, and therefore whether an unset field may skip them.
//
// It reads the declared bounds rather than running the rules, for one reason:
// [NumberRules.Must] takes a function, and evaluating a caller's predicate
// speculatively would call it twice per request with a value that was never
// submitted. A custom rule is treated as admitting zero, so a field guarded
// only by Must behaves exactly as it did before: an explicit
// [NumberRules.Required] is what makes such a field mandatory, as it always was.
func zeroAdmissible(steps []step[float64]) bool {
	for i := range steps {
		s := &steps[i]
		switch s.kind {
		case kindMin:
			if s.lo > 0 {
				return false
			}
		case kindMax:
			if s.hi < 0 {
				return false
			}
		case kindBetween:
			if s.lo > 0 || s.hi < 0 {
				return false
			}
		case kindGreaterThan:
			if s.lo >= 0 {
				return false
			}
		case kindLessThan:
			if s.hi <= 0 {
				return false
			}
		case kindPositive, kindNegative, kindPort:
			// Each excludes zero by definition: Port documents that nought
			// means "any free port" to the operating system rather than a port
			// a client can be told to reach.
			return false
		case kindOneOfNumber:
			if !slices.Contains(s.enum, any(float64(0))) {
				return false
			}
		}
	}
	return true
}

// problemFor describes one failed step: the wording it is reported with, the
// rule it came from, and the values that rule's message interpolates.
//
// The wording and the structure are produced together rather than separately so
// that they cannot disagree. An override replaces the words and clears the
// rule, which is right: a caller who wrote a sentence asked for that sentence,
// and there is nothing left for a translator to have translated.
func problemFor[T any](s *step[T], err error) Problem {
	if s.message != "" {
		return Problem{Issue: s.message}
	}

	kind, args := failureFor(s)
	if carried, ok := err.(argumented); ok {
		args = append(args, carried.args()...)
	}
	if s.messageKey != "" {
		return Problem{
			Issue: err.Error(),
			Key:   s.messageKey,
			Kind:  kind,
			Args:  append(args, s.messageArgs...),
		}
	}
	return Problem{Issue: err.Error(), Kind: kind, Args: args}
}

// failureFor reports the rule a step came from and the values its message
// interpolates.
//
// It reads the parameters back off the step rather than having the appliers
// return them, so that the appliers keep their one-word return and the path a
// passing value takes is exactly what it was: nothing here runs unless a check
// has already failed.
func failureFor[T any](s *step[T]) (Kind, []any) {
	kind, known := kindsOf[s.kind]
	if !known {
		return KindNone, nil
	}
	switch s.kind {
	case kindMinLen, kindMaxLen, kindLen, kindMinItems, kindMaxItems,
		kindItems, kindMinBytes, kindMaxBytes:
		// A count is a number rather than text, because it both prints and
		// chooses which plural form prints it.
		return kind, []any{"count", s.n}
	case kindMin, kindMultipleOf, kindGreaterThan:
		// A bound is passed as a number rather than as text, because count is
		// reserved: it is interpolated and it selects a plural form, and a
		// string can do neither.
		return kind, []any{"count", s.lo}
	case kindMax, kindLessThan:
		return kind, []any{"count", s.hi}
	case kindBetween:
		return kind, []any{"min", formatNumber(s.lo), "max", formatNumber(s.hi)}
	case kindOneOfString, kindURLScheme:
		return kind, []any{"list", quoteList(s.list)}
	case kindOneOfNumber:
		return kind, []any{"list", numberList(s.enum)}
	case kindOneOfValue:
		return kind, []any{"list", describeList(s.enum)}
	case kindPrefix, kindSuffix, kindContains:
		return kind, []any{"value", quoteOne(s.text)}
	default:
		return kind, nil
	}
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
			c.ExclusiveMinimum = floatPtr(0)
		case kindNegative:
			c.ExclusiveMaximum = floatPtr(0)
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
		case kindOneOfValue, kindOneOfNumber:
			c.Enum = append(c.Enum, s.enum...)

		case kindHTTPS, kindURLScheme:
			c.Format = "uri"
		case kindHost:
			c.Format = "hostname"
		case kindIP:
			c.Format = "ip"
		case kindIPv4:
			c.Format = "ipv4"
		case kindIPv6:
			c.Format = "ipv6"
		case kindCIDR:
			c.Format = "cidr"
		case kindMAC:
			c.Format = "mac"
		case kindBase64:
			// The name OpenAPI gives base64, rather than one of its own.
			c.Format = "byte"

		case kindAlpha:
			c.Pattern = patternAlpha
		case kindAlphanumeric:
			c.Pattern = patternAlphanumeric
		case kindNumericString:
			c.Pattern = patternNumeric
		case kindASCII:
			c.Pattern = patternASCII
		case kindSlug:
			c.Pattern = patternSlug
		case kindHex:
			c.Pattern = patternHex
		case kindHexColour:
			c.Pattern = patternHexColour
		case kindE164:
			c.Pattern = patternE164
		case kindCountryCode:
			c.Pattern = patternCountryCode
		case kindCurrencyCode:
			c.Pattern = patternCurrencyCode

		case kindGreaterThan:
			c.ExclusiveMinimum = floatPtr(s.lo)
		case kindLessThan:
			c.ExclusiveMaximum = floatPtr(s.hi)
		case kindNonNegative:
			c.Minimum = floatPtr(0)
		case kindNonPositive:
			c.Maximum = floatPtr(0)
		case kindWhole:
			c.MultipleOf = floatPtr(1)
		case kindPort:
			c.Minimum, c.Maximum = floatPtr(1), floatPtr(65535)

		case kindItems:
			c.MinItems, c.MaxItems = intPtr(s.n), intPtr(s.n)
		case kindNotEmpty:
			c.MinItems = intPtr(1)
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

// setMessageKey attaches a translation key to the most recently added check,
// skipping transforms for the same reason [setMessage] does.
func setMessageKey[T any](steps []step[T], key string, args []any) {
	for i := len(steps) - 1; i >= 0; i-- {
		if !steps[i].isTransform() {
			steps[i].messageKey = key
			steps[i].messageArgs = args
			return
		}
	}
}
