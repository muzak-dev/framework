package validate

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
	"uuid"
)

// StringRules collects the transforms and checks applied to a string field.
//
// Obtain one from muzak.Validation.String for a field, or from [String] to
// build a reusable set or to describe the elements of a slice. Every method
// returns the same rule set, so calls chain.
type StringRules struct {
	target   any
	label    string
	required bool
	steps    []step[string]
}

// String returns an unbound rule set, for reuse across models or for
// describing the elements of a slice:
//
//	v.Slice(&in.Tags).Each(validate.String().MaxLen(20))
func String() *StringRules { return &StringRules{} }

// For binds the rule set to a field, identified by its address.
//
// muzak.Validation.String calls it; application code uses that entry point
// instead, which also registers the rule set so it actually runs.
func (r *StringRules) For(target any) *StringRules {
	r.target = target
	return r
}

// Reset returns the rule set to its unbound, ruleless state while keeping the
// memory it has already claimed.
//
// It is what lets a Validation hand the same rule set to one request after
// another: the steps slice keeps its capacity, so redeclaring the rules costs
// no allocation once the shape has been seen. Application code has no reason to
// call it.
func (r *StringRules) Reset() {
	r.target = nil
	r.label = ""
	r.required = false
	r.steps = r.steps[:0]
}

// Target implements [Evaluator].
func (r *StringRules) Target() any { return r.target }

// Label implements [Evaluator].
func (r *StringRules) Label() string { return r.label }

// Describe implements [Evaluator].
func (r *StringRules) Describe() Constraints { return r.describe() }

func (r *StringRules) describe() Constraints {
	c := describeAll(r.steps)
	c.Required = r.required
	return c
}

// Evaluate implements [Evaluator].
func (r *StringRules) Evaluate() []Problem {
	value, ok := resolve(r.target)
	if !ok {
		// A nil pointer field carries no value to judge.
		return nil
	}
	return r.applyToValue(value)
}

// applyTo implements [ElementRules] for string collections.
func (r *StringRules) applyTo(element *string) []Problem {
	return runString(element, r.steps, r.required)
}

// applyToValue runs the rules against a settable reflect value, writing any
// transform back through it.
func (r *StringRules) applyToValue(value reflect.Value) []Problem {
	text := value.String()
	problems := runString(&text, r.steps, r.required)
	if value.String() != text {
		value.SetString(text)
	}
	return problems
}

// add appends a step and returns the rule set for chaining.
func (r *StringRules) add(s step[string]) *StringRules {
	r.steps = append(r.steps, s)
	return r
}

// As renames the field in the messages this rule set produces, for a field
// whose struct tag reads worse than its purpose:
//
//	v.String(&in.DOB).As("date_of_birth").Required()
func (r *StringRules) As(name string) *StringRules {
	r.label = name
	return r
}

// Message overrides the wording of the check written immediately before it,
// leaving every other check's message alone.
func (r *StringRules) Message(message string) *StringRules {
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
// Use [StringRules.Message] instead when the wording is fixed in one language on
// purpose.
func (r *StringRules) MessageKey(key string, args ...any) *StringRules {
	setMessageKey(r.steps, key, args)
	return r
}

// Trim removes leading and trailing whitespace from the value.
//
// It is a transform: the handler receives the trimmed string. Writing it before
// Required is what makes a field of nothing but spaces count as absent.
func (r *StringRules) Trim() *StringRules {
	return r.add(step[string]{kind: kindTrim})
}

// Lower folds the value to lower case, which is what an email address or a
// username usually wants before it is compared or stored.
func (r *StringRules) Lower() *StringRules {
	return r.add(step[string]{kind: kindLower})
}

// Upper folds the value to upper case.
func (r *StringRules) Upper() *StringRules {
	return r.add(step[string]{kind: kindUpper})
}

// Required rejects an empty value.
//
// Without it a field is optional, and every other check is skipped when the
// value is empty, which is what lets MaxLen coexist with a field nobody sent.
func (r *StringRules) Required() *StringRules {
	r.required = true
	return r.add(step[string]{kind: kindRequired})
}

// MinLen requires at least n characters, counted as runes rather than bytes so
// that a name in any script is measured the way a person would count it.
func (r *StringRules) MinLen(n int) *StringRules {
	return r.add(step[string]{
		kind: kindMinLen,
		n:    n,
	})
}

// MaxLen requires at most n characters, counted as runes.
func (r *StringRules) MaxLen(n int) *StringRules {
	return r.add(step[string]{
		kind: kindMaxLen,
		n:    n,
	})
}

// Len requires exactly n characters.
func (r *StringRules) Len(n int) *StringRules {
	return r.add(step[string]{
		kind: kindLen,
		n:    n,
	})
}

// Email requires a valid address.
//
// The address is parsed with net/mail, so what passes here is what a mail
// library will accept. It deliberately does not try to prove the mailbox
// exists, which only sending to it can establish.
func (r *StringRules) Email() *StringRules {
	return r.add(step[string]{kind: kindEmail})
}

// URL requires an absolute http or https URL with a host.
//
// A relative reference is rejected, because a field asking for a URL almost
// always means somewhere a client can actually go. So is every other scheme:
// without a restriction, "javascript:", "data:" and "file:" all parse as
// perfectly valid absolute URLs, and a value this check approves is exactly
// the kind of thing that ends up in a redirect, a fetch or an href with the
// validator's blessing.
func (r *StringRules) URL() *StringRules {
	return r.add(step[string]{kind: kindURL})
}

// UUID requires a value that parses as a UUID in any of its usual spellings.
func (r *StringRules) UUID() *StringRules {
	return r.add(step[string]{kind: kindUUID})
}

// Matches requires the value to match a regular expression.
//
// The expression is compiled once, when the rule is declared, so a malformed
// pattern is a panic at start-up rather than a failure on the first request
// that happens to reach it.
func (r *StringRules) Matches(pattern string) *StringRules {
	return r.add(step[string]{
		kind:    kindMatches,
		text:    pattern,
		pattern: regexp.MustCompile(pattern),
	})
}

// OneOf restricts the value to a fixed set, which also becomes the enum in the
// generated documentation.
func (r *StringRules) OneOf(allowed ...string) *StringRules {
	return r.add(step[string]{kind: kindOneOfString, list: allowed})
}

// NotOneOf rejects a fixed set of values, for the handful a field must never
// carry, such as a reserved username.
func (r *StringRules) NotOneOf(rejected ...string) *StringRules {
	return r.add(step[string]{kind: kindNotOneOfString, list: rejected})
}

// Equal requires the value to match another, which is how a confirmation field
// is checked:
//
//	v.String(&in.Confirm).Equal(in.Password).Message("must match the password")
//
// The comparison is an ordinary Go expression against the model's own field,
// so no special support for cross-field rules is needed.
func (r *StringRules) Equal(other string) *StringRules {
	return r.add(step[string]{kind: kindEqualString, text: other})
}

// Prefix requires the value to begin with the given text.
func (r *StringRules) Prefix(prefix string) *StringRules {
	return r.add(step[string]{kind: kindPrefix, text: prefix})
}

// Suffix requires the value to end with the given text.
func (r *StringRules) Suffix(suffix string) *StringRules {
	return r.add(step[string]{kind: kindSuffix, text: suffix})
}

// Contains requires the value to hold the given text somewhere.
func (r *StringRules) Contains(substring string) *StringRules {
	return r.add(step[string]{kind: kindContains, text: substring})
}

// Must applies a rule of your own.
//
// The function is an ordinary func(string) error, so it needs no registration
// and can be tested on its own. Phrase its error to read after the field name,
// as "is too common, choose something less guessable", which is how every
// built-in rule words its own failures.
func (r *StringRules) Must(check func(string) error) *StringRules {
	return r.add(step[string]{kind: kindCustom, check: check})
}

// Check applies the rule set to a value and returns the first failure, without
// any of the machinery that binds rules to a field.
//
// It is what makes a rule set testable in isolation, and what lets one be used
// as an ordinary func(string) error elsewhere.
func (r *StringRules) Check(value string) error {
	problems := runString(&value, r.steps, r.required)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(problems[0].Issue)
}

// isHTTPScheme reports whether a parsed URL's scheme is http or https, the
// only schemes [StringRules.URL] accepts. The comparison is case-insensitive
// because the scheme is, even though url.Parse already lower-cases it.
func isHTTPScheme(scheme string) bool {
	return strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
}

// plural returns word with an "s" unless n is one.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// quoteOne renders a single comparand the way the built-in messages do, so that
// a translated message interpolates exactly what the English one prints.
func quoteOne(text string) string {
	return fmt.Sprintf("%q", text)
}

// quoteList renders a set of allowed values for an error message.
func quoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return joinWithOr(quoted)
}

// applyStringStep runs one string rule.
//
// It is a package-level function rather than a closure so that declaring a rule
// set allocates nothing: the parameters live in the step, and dispatching to
// the right comparison costs a jump.
func applyStringStep(s *step[string], value *string) error {
	switch s.kind {
	case kindTrim:
		*value = strings.TrimSpace(*value)
	case kindLower:
		*value = strings.ToLower(*value)
	case kindUpper:
		*value = strings.ToUpper(*value)

	case kindRequired:
		if *value == "" {
			return errRequired
		}
	case kindMinLen:
		if utf8.RuneCountInString(*value) < s.n {
			return fmt.Errorf("must be at least %d %s", s.n, plural(s.n, "character"))
		}
	case kindMaxLen:
		if utf8.RuneCountInString(*value) > s.n {
			return fmt.Errorf("must be at most %d %s", s.n, plural(s.n, "character"))
		}
	case kindLen:
		if utf8.RuneCountInString(*value) != s.n {
			return fmt.Errorf("must be exactly %d %s", s.n, plural(s.n, "character"))
		}
	case kindEmail:
		address, err := mail.ParseAddress(*value)
		if err != nil || address.Address != *value {
			return errors.New("must be a valid email address")
		}
	case kindURL:
		parsed, err := url.Parse(*value)
		if err != nil || parsed.Host == "" || !isHTTPScheme(parsed.Scheme) {
			return errors.New("must be a valid absolute http or https URL")
		}
	case kindUUID:
		if _, err := uuid.Parse(*value); err != nil {
			return errors.New("must be a valid UUID")
		}
	case kindMatches:
		if !s.pattern.MatchString(*value) {
			return errors.New("is not in the expected format")
		}
	case kindOneOfString:
		if !slices.Contains(s.list, *value) {
			return fmt.Errorf("must be one of %s", quoteList(s.list))
		}
	case kindNotOneOfString:
		if slices.Contains(s.list, *value) {
			return errors.New("is not available")
		}
	case kindEqualString:
		if *value != s.text {
			return errNoMatch
		}
	case kindPrefix:
		if !strings.HasPrefix(*value, s.text) {
			return fmt.Errorf("must begin with %q", s.text)
		}
	case kindSuffix:
		if !strings.HasSuffix(*value, s.text) {
			return fmt.Errorf("must end with %q", s.text)
		}
	case kindContains:
		if !strings.Contains(*value, s.text) {
			return fmt.Errorf("must contain %q", s.text)
		}
	default:
		return s.check(*value)
	}
	return nil
}
