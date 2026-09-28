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
	"sync"
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
// no allocation once the shape has been seen. The steps are cleared as well as
// truncated, so a rule set waiting to be reused holds no comparand or closure
// from the request that last used it. Application code has no reason to call
// it.
func (r *StringRules) Reset() {
	r.target = nil
	r.label = ""
	r.required = false
	clear(r.steps)
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
//
// Characters nobody can see are refused before the parser is consulted:
// control characters, including the C1 set such as U+0085, format characters
// such as the bidirectional override U+202E and the zero-width space U+200B,
// and the line and paragraph separators U+2028 and U+2029. net/mail accepts
// them, and an address holding one displays as a different address from the
// one mail is sent to, or splits a line in whatever it is written into.
//
// Letters from other scripts are accepted, because internationalised
// addresses are real addresses. That includes letters that look like Latin
// ones, so "adm\u0456n@example.com" passes although it renders as a familiar
// name: telling a lookalike from a legitimate address in another script is a
// policy about who may register what, not a question of syntax.
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
// The expression is compiled the first time the rule is declared, and kept, so
// a malformed pattern is a panic at start-up rather than a failure on the
// first request that happens to reach it, and a rule declared on every request
// does not compile it again.
func (r *StringRules) Matches(pattern string) *StringRules {
	return r.add(step[string]{
		kind:    kindMatches,
		text:    pattern,
		pattern: compilePattern(pattern),
	})
}

// patternCacheLimit is how many distinct patterns are kept. A pattern comes
// from the developer and there are a few dozen of them, so the limit is only
// there for the rule that is built from something the client sent: it is
// compiled every time, as it always was, rather than filling memory.
const patternCacheLimit = 1024

// patternStore keeps compiled regular expressions by their text, up to a limit.
type patternStore struct {
	mu    sync.RWMutex
	limit int
	held  map[string]*regexp.Regexp
}

var patterns = &patternStore{limit: patternCacheLimit, held: map[string]*regexp.Regexp{}}

// compile returns the expression kept for a pattern, compiling and keeping it
// when there is none and room for it. It panics on a malformed pattern.
func (s *patternStore) compile(pattern string) *regexp.Regexp {
	s.mu.RLock()
	compiled, ok := s.held[pattern]
	s.mu.RUnlock()
	if ok {
		return compiled
	}
	compiled = regexp.MustCompile(pattern)
	s.mu.Lock()
	defer s.mu.Unlock()
	if kept, ok := s.held[pattern]; ok {
		return kept
	}
	if len(s.held) < s.limit {
		s.held[pattern] = compiled
	}
	return compiled
}

// compilePattern compiles a regular expression, or returns the one already
// compiled for the same text, and panics on a malformed one.
//
// Rules are declared inside a Validate method, which runs on every request,
// so a declaration that compiled its expression cost the request the
// compile: 83 microseconds and 491 allocations for an ordinary slug pattern,
// twenty times the cost of the rest of the validation, and half a millisecond
// for a Unicode class with a repetition count. A compiled expression holds no
// state that a match changes, so one is shared by every request.
func compilePattern(pattern string) *regexp.Regexp {
	return patterns.compile(pattern)
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

// HTTPS requires an absolute https URL with a host.
//
// It is [StringRules.URL] with the one scheme that is encrypted. A field
// holding a webhook target, a redirect or an avatar source almost always means
// https specifically, and saying so here is cheaper than discovering later that
// a client supplied http and the traffic went out in the clear.
func (r *StringRules) HTTPS() *StringRules {
	return r.add(step[string]{kind: kindHTTPS})
}

// URLWithSchemes requires an absolute URL using one of the given schemes.
//
//	v.String(&in.Source).URLWithSchemes("s3", "gs")
//
// Schemes are matched without regard to case. A scheme that carries an
// authority must have a host, and one that does not, such as mailto, must have
// something after the colon, so neither "https://" nor "mailto:" alone passes.
func (r *StringRules) URLWithSchemes(schemes ...string) *StringRules {
	return r.add(step[string]{kind: kindURLScheme, list: schemes})
}

// Host requires a valid DNS hostname.
//
// The rules are the ones a resolver enforces: at most 253 characters, labels of
// 1 to 63 of letters, digits and hyphens, and no label beginning or ending with
// a hyphen. A single trailing dot is allowed, because that is how a fully
// qualified name is written.
func (r *StringRules) Host() *StringRules {
	return r.add(step[string]{kind: kindHost})
}

// IP requires an IP address of either family.
//
// An IPv6 address with a zone, such as "fe80::1%eth0", is refused. The zone is
// free text to the parser, so accepting it let a value carrying a line break or
// markup through as a valid address, and it names an interface on the machine
// that wrote it, which is meaningless to the one reading it. A field that does
// need a link-local zone should take a string and check it with a rule of its
// own.
func (r *StringRules) IP() *StringRules {
	return r.add(step[string]{kind: kindIP})
}

// IPv4 requires an IPv4 address.
func (r *StringRules) IPv4() *StringRules {
	return r.add(step[string]{kind: kindIPv4})
}

// IPv6 requires an IPv6 address.
//
// An IPv4 address written in the mapped form is refused. It is an IPv4 address
// wearing a costume, and a field asking for IPv6 wants one an IPv6-only network
// can route. So is an address with a zone, for the reasons [StringRules.IP]
// gives.
func (r *StringRules) IPv6() *StringRules {
	return r.add(step[string]{kind: kindIPv6})
}

// CIDR requires a network written in CIDR notation, such as "10.0.0.0/8".
func (r *StringRules) CIDR() *StringRules {
	return r.add(step[string]{kind: kindCIDR})
}

// MAC requires a hardware address in any of its usual spellings.
func (r *StringRules) MAC() *StringRules {
	return r.add(step[string]{kind: kindMAC})
}

// MatchesNot rejects a value matching a regular expression.
//
// It exists because Go's expressions cannot state it. The regexp package is
// RE2, which has no lookaround, so there is no pattern for "does not begin with
// tmp-" or "does not contain admin": (?!...) does not compile. A character class
// can be negated, as "^[^-]+$", but a sequence cannot, so the negation has to
// move outside the expression.
//
// That omission in RE2 is deliberate rather than an oversight. Dropping
// lookaround and backreferences is what buys the linear-time guarantee, and it
// is why a pattern from a locale file or a configuration can be run against a
// request at all without a way to make it take forever.
//
// The expression is compiled when the rule is first declared, and kept as
// [StringRules.Matches] keeps its own, so a malformed pattern is a panic at
// start-up rather than a failure on the first request that reaches it.
// Nothing is contributed to the generated document: a JSON Schema
// pattern means the value must match, so describing a negative one with it
// would tell a client the opposite of what is enforced.
func (r *StringRules) MatchesNot(pattern string) *StringRules {
	return r.add(step[string]{
		kind:    kindNotMatches,
		text:    pattern,
		pattern: compilePattern(pattern),
	})
}

// NotBlank rejects a value that is nothing but whitespace.
//
// It is the check [StringRules.Required] cannot make. Required rejects the
// empty string, and a field of three spaces is not empty, so it satisfies
// Required while carrying nothing anyone would call a value.
//
// Unlike almost every other rule, this one runs even when the field was not
// supplied at all, because an absent value is blank too. That makes it a
// stronger presence check rather than something to pair Required with. Reach
// for Trim instead when the spaces should simply be removed.
func (r *StringRules) NotBlank() *StringRules {
	return r.add(step[string]{kind: kindNotBlank})
}

// NoControl rejects a value holding a control character.
//
// Tabs and line breaks count. A value that reaches a response header, a log
// line or a redirect carrying a carriage return is how an injection starts, and
// a field that legitimately holds one is rare enough to be worth declaring
// deliberately.
func (r *StringRules) NoControl() *StringRules {
	return r.add(step[string]{kind: kindNoControl})
}

// Alpha requires every character to be a letter.
//
// Letters are Unicode letters rather than the twenty-six of English, so a name
// in any script passes. Add [StringRules.ASCII] to narrow it.
func (r *StringRules) Alpha() *StringRules {
	return r.add(step[string]{kind: kindAlpha})
}

// Alphanumeric requires every character to be a letter or a digit, judged the
// same way [StringRules.Alpha] judges a letter.
func (r *StringRules) Alphanumeric() *StringRules {
	return r.add(step[string]{kind: kindAlphanumeric})
}

// Numeric requires every character to be a digit.
//
// It is a rule about the characters rather than about the value: a field of
// digits that happens to have a leading zero, or more digits than an integer
// holds, still passes. Bind a numeric field and use [NumberRules] when the
// value is a number rather than a string of digits.
func (r *StringRules) Numeric() *StringRules {
	return r.add(step[string]{kind: kindNumericString})
}

// ASCII requires every character to be printable ASCII, which is space through
// tilde: the set a protocol field or an identifier carries without anyone
// having to think about encoding.
func (r *StringRules) ASCII() *StringRules {
	return r.add(step[string]{kind: kindASCII})
}

// Slug requires a slug: lower case letters and digits in groups separated by
// single hyphens, as "a-good-title".
func (r *StringRules) Slug() *StringRules {
	return r.add(step[string]{kind: kindSlug})
}

// Hex requires every character to be a hexadecimal digit, in either case.
func (r *StringRules) Hex() *StringRules {
	return r.add(step[string]{kind: kindHex})
}

// HexColour requires a colour written in hexadecimal, as "#1a2b3c". Three,
// four, six and eight digits are accepted, the last two being the forms that
// carry an alpha channel.
func (r *StringRules) HexColour() *StringRules {
	return r.add(step[string]{kind: kindHexColour})
}

// Base64 requires a value that decodes as standard base64.
func (r *StringRules) Base64() *StringRules {
	return r.add(step[string]{kind: kindBase64})
}

// JSON requires a value that is a well-formed JSON document.
//
// It is for a field that carries JSON as text, such as a stored configuration
// blob. A field that is JSON in the request body is decoded into a type of its
// own instead, which checks far more than this does.
func (r *StringRules) JSON() *StringRules {
	return r.add(step[string]{kind: kindJSON})
}

// Semver requires a semantic version, as "1.4.0" or "2.0.0-rc.1+build.5".
func (r *StringRules) Semver() *StringRules {
	return r.add(step[string]{kind: kindSemver})
}

// E164 requires a telephone number in the international format: a plus, a
// country code, and up to fifteen digits in all.
//
// It checks the shape and nothing more. Whether a number is assigned, reachable
// or the caller's own is not something a validator can establish, and only
// sending to it will.
func (r *StringRules) E164() *StringRules {
	return r.add(step[string]{kind: kindE164})
}

// LanguageTag requires a BCP 47 language tag, as "en", "pt-BR" or "zh-Hant-TW".
//
// The shape is checked rather than the registry. A well-formed tag naming a
// language nobody has translated is a missing translation, which the i18n
// package already reports; a malformed tag is a mistake in the request.
func (r *StringRules) LanguageTag() *StringRules {
	return r.add(step[string]{kind: kindLanguageTag})
}

// Timezone requires the name of a time zone the host knows, as
// "Europe/Istanbul".
//
// The name has to be spelled exactly as the IANA zone database spells it, a
// backward-compatible link such as "Asia/Calcutta" included, and has to load
// from the zone data the host or the binary carries, so what passes here is
// what time.LoadLocation will later accept. Spellings LoadLocation would also
// take are refused: "Europe//Paris", which it cleans, and "europe/paris",
// which it finds on a case-insensitive filesystem. So is "Local", which names
// the server's own zone rather than one the client can mean. The names are
// checked against a list built into this package, from the database Go 1.27
// ships, so a zone created after that is refused until the list is updated;
// each listed name is loaded from the zone data at most once, and anything
// else costs a map lookup.
//
// # Zone data has to be there
//
// Go does not carry the zone database unless something asks it to. A binary on
// a scratch or distroless image, which is what most container deployments use,
// has no /usr/share/zoneinfo either, and then this rule refuses every value
// including the correct ones. Nothing announces that: the requests simply start
// failing validation.
//
// An application that uses this rule should either carry the database:
//
//	import _ "time/tzdata"
//
// which costs about 450 kilobytes of binary, or run on an image that has one.
// Check at start-up with [TimezoneDataAvailable] either way, so that a missing
// database stops the process rather than every request.
func (r *StringRules) Timezone() *StringRules {
	return r.add(step[string]{kind: kindTimezone})
}

// TimezoneDataAvailable reports whether this binary can resolve time zone names
// at all.
//
// It exists because the alternative is finding out from production. A Go binary
// resolves a zone from the host's database or from one compiled in by importing
// time/tzdata, and a container built from scratch has neither, so every call to
// [StringRules.Timezone] refuses its input and nothing says why.
//
// Call it once, where the application is built, and refuse to start without it:
//
//	if !validate.TimezoneDataAvailable() {
//		log.Fatal("no time zone database: import _ \"time/tzdata\" or use an image that has one")
//	}
//
// It resolves a real zone rather than UTC, which the standard library answers
// without reading anything.
func TimezoneDataAvailable() bool {
	return isTimezone("America/New_York")
}

// CountryCode requires an assigned ISO 3166-1 alpha-2 country code.
//
// The code is looked up in a table rather than merely checked for its shape, so
// "XQ" is refused although it is two upper case letters. That table has a date
// on it where it is declared, and it needs regenerating when the standard
// changes: countries are added and withdrawn, and a stale copy rejects input
// that has become valid.
//
// The generated document describes the shape rather than the table, because two
// hundred values in every schema that names a country would be noise rather than
// documentation. Use [StringRules.OneOf] with your own list when a service
// trades in a known handful, which is both narrower and self-documenting.
func (r *StringRules) CountryCode() *StringRules {
	return r.add(step[string]{kind: kindCountryCode})
}

// CurrencyCode requires an assigned ISO 4217 alphabetic currency code, looked up
// the way [StringRules.CountryCode] looks one up and carrying the same caveat
// about the table going out of date.
func (r *StringRules) CurrencyCode() *StringRules {
	return r.add(step[string]{kind: kindCurrencyCode})
}

// EqualFold requires the value to equal another, ignoring case.
//
// It is [StringRules.Equal] for the comparisons where case is not meaningful,
// such as a confirmation typed a second time or a token echoed back.
func (r *StringRules) EqualFold(other string) *StringRules {
	return r.add(step[string]{kind: kindEqualFold, text: other})
}

// MinBytes requires the value to occupy at least n bytes when encoded.
//
// Lengths elsewhere in this package are counted in characters, which is what a
// person means by the length of a name. Bytes are what a storage column or a
// protocol field means, and the two differ for every value outside ASCII.
func (r *StringRules) MinBytes(n int) *StringRules {
	return r.add(step[string]{kind: kindMinBytes, n: n})
}

// MaxBytes requires the value to occupy at most n bytes when encoded, counted
// the way [StringRules.MinBytes] counts.
func (r *StringRules) MaxBytes(n int) *StringRules {
	return r.add(step[string]{kind: kindMaxBytes, n: n})
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
		if hasHiddenRune(*value) {
			return errors.New("must be a valid email address")
		}
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
	case kindHTTPS:
		if !isHTTPSURL(*value) {
			return errors.New("must be a valid https URL")
		}
	case kindURLScheme:
		if !isURLWithScheme(*value, s.list) {
			return fmt.Errorf("must be a URL using %s", quoteList(s.list))
		}
	case kindHost:
		if !isHostname(*value) {
			return errors.New("must be a valid hostname")
		}
	case kindIP:
		if !isIP(*value) {
			return errors.New("must be a valid IP address")
		}
	case kindIPv4:
		if !isIPv4(*value) {
			return errors.New("must be a valid IPv4 address")
		}
	case kindIPv6:
		if !isIPv6(*value) {
			return errors.New("must be a valid IPv6 address")
		}
	case kindCIDR:
		if !isCIDR(*value) {
			return errors.New("must be a valid network in CIDR notation")
		}
	case kindMAC:
		if !isMAC(*value) {
			return errors.New("must be a valid MAC address")
		}
	case kindNotMatches:
		if s.pattern.MatchString(*value) {
			return errors.New("is in a format that is not accepted")
		}
	case kindNotBlank:
		if isBlank(*value) {
			return errNotBlank
		}
	case kindNoControl:
		if hasControl(*value) {
			return errors.New("must not contain control characters")
		}
	case kindAlpha:
		if !isAlpha(*value) {
			return errors.New("must contain only letters")
		}
	case kindAlphanumeric:
		if !isAlphanumeric(*value) {
			return errors.New("must contain only letters and digits")
		}
	case kindNumericString:
		if !isNumericString(*value) {
			return errors.New("must contain only digits")
		}
	case kindASCII:
		if !isPrintableASCII(*value) {
			return errors.New("must contain only printable ASCII characters")
		}
	case kindSlug:
		if !isSlug(*value) {
			return errors.New("must contain only lower case letters, digits and hyphens")
		}
	case kindHex:
		if !isHex(*value) {
			return errors.New("must be hexadecimal")
		}
	case kindHexColour:
		if !isHexColour(*value) {
			return errors.New("must be a hexadecimal colour, such as #1a2b3c")
		}
	case kindBase64:
		if !isBase64(*value) {
			return errors.New("must be valid base64")
		}
	case kindJSON:
		if !isJSON(*value) {
			return errors.New("must be valid JSON")
		}
	case kindSemver:
		if !isSemver(*value) {
			return errors.New("must be a semantic version, such as 1.4.0")
		}
	case kindE164:
		if !isE164(*value) {
			return errors.New("must be a telephone number in international format")
		}
	case kindLanguageTag:
		if !isLanguageTag(*value) {
			return errors.New("must be a valid language tag, such as pt-BR")
		}
	case kindTimezone:
		if !isTimezone(*value) {
			return errors.New("must be a known time zone, such as Europe/Istanbul")
		}
	case kindCountryCode:
		if !isCountryCode(*value) {
			return errors.New("must be a known country code")
		}
	case kindCurrencyCode:
		if !isCurrencyCode(*value) {
			return errors.New("must be a known currency code")
		}
	case kindEqualFold:
		if !strings.EqualFold(*value, s.text) {
			return errNoMatch
		}
	case kindMinBytes:
		if len(*value) < s.n {
			return fmt.Errorf("must be at least %d %s", s.n, plural(s.n, "byte"))
		}
	case kindMaxBytes:
		if len(*value) > s.n {
			return fmt.Errorf("must be at most %d %s", s.n, plural(s.n, "byte"))
		}
	default:
		return s.check(*value)
	}
	return nil
}
