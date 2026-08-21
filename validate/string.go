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
// Obtain one from badele.Validation.String for a field, or from [String] to
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
// badele.Validation.String calls it; application code uses that entry point
// instead, which also registers the rule set so it actually runs.
func (r *StringRules) For(target any) *StringRules {
	r.target = target
	return r
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
	return run(element, r.steps, isEmptyString, r.required)
}

// applyToValue runs the rules against a settable reflect value, writing any
// transform back through it.
func (r *StringRules) applyToValue(value reflect.Value) []Problem {
	text := value.String()
	problems := run(&text, r.steps, isEmptyString, r.required)
	if value.String() != text {
		value.SetString(text)
	}
	return problems
}

// isEmptyString reports whether a value counts as absent.
func isEmptyString(s string) bool { return s == "" }

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

// Trim removes leading and trailing whitespace from the value.
//
// It is a transform: the handler receives the trimmed string. Writing it before
// Required is what makes a field of nothing but spaces count as absent.
func (r *StringRules) Trim() *StringRules {
	return r.add(step[string]{id: "trim", change: strings.TrimSpace})
}

// Lower folds the value to lower case, which is what an email address or a
// username usually wants before it is compared or stored.
func (r *StringRules) Lower() *StringRules {
	return r.add(step[string]{id: "lower", change: strings.ToLower})
}

// Upper folds the value to upper case.
func (r *StringRules) Upper() *StringRules {
	return r.add(step[string]{id: "upper", change: strings.ToUpper})
}

// Required rejects an empty value.
//
// Without it a field is optional, and every other check is skipped when the
// value is empty, which is what lets MaxLen coexist with a field nobody sent.
func (r *StringRules) Required() *StringRules {
	r.required = true
	return r.add(step[string]{
		id: requiredRuleID,
		check: func(s string) error {
			if s == "" {
				return errors.New("is required")
			}
			return nil
		},
		describe: func(c *Constraints) { c.Required = true },
	})
}

// MinLen requires at least n characters, counted as runes rather than bytes so
// that a name in any script is measured the way a person would count it.
func (r *StringRules) MinLen(n int) *StringRules {
	return r.add(step[string]{
		id: "min_len",
		check: func(s string) error {
			if utf8.RuneCountInString(s) < n {
				return fmt.Errorf("must be at least %d %s", n, plural(n, "character"))
			}
			return nil
		},
		describe: func(c *Constraints) { c.MinLength = intPtr(n) },
	})
}

// MaxLen requires at most n characters, counted as runes.
func (r *StringRules) MaxLen(n int) *StringRules {
	return r.add(step[string]{
		id: "max_len",
		check: func(s string) error {
			if utf8.RuneCountInString(s) > n {
				return fmt.Errorf("must be at most %d %s", n, plural(n, "character"))
			}
			return nil
		},
		describe: func(c *Constraints) { c.MaxLength = intPtr(n) },
	})
}

// Len requires exactly n characters.
func (r *StringRules) Len(n int) *StringRules {
	return r.add(step[string]{
		id: "len",
		check: func(s string) error {
			if utf8.RuneCountInString(s) != n {
				return fmt.Errorf("must be exactly %d %s", n, plural(n, "character"))
			}
			return nil
		},
		describe: func(c *Constraints) {
			c.MinLength, c.MaxLength = intPtr(n), intPtr(n)
		},
	})
}

// Email requires a valid address.
//
// The address is parsed with net/mail, so what passes here is what a mail
// library will accept. It deliberately does not try to prove the mailbox
// exists, which only sending to it can establish.
func (r *StringRules) Email() *StringRules {
	return r.add(step[string]{
		id: "email",
		check: func(s string) error {
			address, err := mail.ParseAddress(s)
			if err != nil || address.Address != s {
				return errors.New("must be a valid email address")
			}
			return nil
		},
		describe: func(c *Constraints) { c.Format = "email" },
	})
}

// URL requires an absolute URL with a scheme and a host.
//
// A relative reference is rejected, because a field asking for a URL almost
// always means somewhere a client can actually go.
func (r *StringRules) URL() *StringRules {
	return r.add(step[string]{
		id: "url",
		check: func(s string) error {
			parsed, err := url.Parse(s)
			if err != nil || parsed.Scheme == "" || parsed.Host == "" {
				return errors.New("must be a valid absolute URL")
			}
			return nil
		},
		describe: func(c *Constraints) { c.Format = "uri" },
	})
}

// UUID requires a value that parses as a UUID in any of its usual spellings.
func (r *StringRules) UUID() *StringRules {
	return r.add(step[string]{
		id: "uuid",
		check: func(s string) error {
			if _, err := uuid.Parse(s); err != nil {
				return errors.New("must be a valid UUID")
			}
			return nil
		},
		describe: func(c *Constraints) { c.Format = "uuid" },
	})
}

// Matches requires the value to match a regular expression.
//
// The expression is compiled once, when the rule is declared, so a malformed
// pattern is a panic at start-up rather than a failure on the first request
// that happens to reach it.
func (r *StringRules) Matches(pattern string) *StringRules {
	expression := regexp.MustCompile(pattern)
	return r.add(step[string]{
		id: "matches",
		check: func(s string) error {
			if !expression.MatchString(s) {
				return errors.New("is not in the expected format")
			}
			return nil
		},
		describe: func(c *Constraints) { c.Pattern = pattern },
	})
}

// OneOf restricts the value to a fixed set, which also becomes the enum in the
// generated documentation.
func (r *StringRules) OneOf(allowed ...string) *StringRules {
	return r.add(step[string]{
		id: "one_of",
		check: func(s string) error {
			if !slices.Contains(allowed, s) {
				return fmt.Errorf("must be one of %s", quoteList(allowed))
			}
			return nil
		},
		describe: func(c *Constraints) {
			for _, value := range allowed {
				c.Enum = append(c.Enum, value)
			}
		},
	})
}

// NotOneOf rejects a fixed set of values, for the handful a field must never
// carry, such as a reserved username.
func (r *StringRules) NotOneOf(rejected ...string) *StringRules {
	return r.add(step[string]{
		id: "not_one_of",
		check: func(s string) error {
			if slices.Contains(rejected, s) {
				return errors.New("is not available")
			}
			return nil
		},
	})
}

// Equal requires the value to match another, which is how a confirmation field
// is checked:
//
//	v.String(&in.Confirm).Equal(in.Password).Message("must match the password")
//
// The comparison is an ordinary Go expression against the model's own field,
// so no special support for cross-field rules is needed.
func (r *StringRules) Equal(other string) *StringRules {
	return r.add(step[string]{
		id: "equal",
		check: func(s string) error {
			if s != other {
				return errors.New("does not match")
			}
			return nil
		},
	})
}

// Prefix requires the value to begin with the given text.
func (r *StringRules) Prefix(prefix string) *StringRules {
	return r.add(step[string]{
		id: "prefix",
		check: func(s string) error {
			if !strings.HasPrefix(s, prefix) {
				return fmt.Errorf("must begin with %q", prefix)
			}
			return nil
		},
	})
}

// Suffix requires the value to end with the given text.
func (r *StringRules) Suffix(suffix string) *StringRules {
	return r.add(step[string]{
		id: "suffix",
		check: func(s string) error {
			if !strings.HasSuffix(s, suffix) {
				return fmt.Errorf("must end with %q", suffix)
			}
			return nil
		},
	})
}

// Contains requires the value to hold the given text somewhere.
func (r *StringRules) Contains(substring string) *StringRules {
	return r.add(step[string]{
		id: "contains",
		check: func(s string) error {
			if !strings.Contains(s, substring) {
				return fmt.Errorf("must contain %q", substring)
			}
			return nil
		},
	})
}

// Must applies a rule of your own.
//
// The function is an ordinary func(string) error, so it needs no registration
// and can be tested on its own. Phrase its error to read after the field name,
// as "is too common, choose something less guessable", which is how every
// built-in rule words its own failures.
func (r *StringRules) Must(check func(string) error) *StringRules {
	return r.add(step[string]{id: "must", check: check})
}

// Check applies the rule set to a value and returns the first failure, without
// any of the machinery that binds rules to a field.
//
// It is what makes a rule set testable in isolation, and what lets one be used
// as an ordinary func(string) error elsewhere.
func (r *StringRules) Check(value string) error {
	problems := run(&value, r.steps, isEmptyString, r.required)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(problems[0].Issue)
}

// plural returns word with an "s" unless n is one.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// quoteList renders a set of allowed values for an error message.
func quoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return joinWithOr(quoted)
}
