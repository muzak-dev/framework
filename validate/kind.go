package validate

import "sort"

// Kind names the rule a failure came from.
//
// A rule set reports what went wrong twice over: once as English in
// [Problem.Issue], which is what a caller reads with no translation configured,
// and once as a Kind, which is what a translation is keyed by. The English is
// always there, so nothing has to be translated for a message to exist; the
// Kind is what lets the framework render the same failure in another language.
//
// The names match Rails wherever the rules mean the same thing, so a locale
// file written for a Rails application already translates most of them.
type Kind string

// The rules a failure can come from. Each is the last segment of the key its
// message is written under, as "errors.messages.blank".
const (
	// KindNone is the zero value: a failure with no rule behind it, which is
	// what a rule of the caller's own and an explicit message both produce.
	// There is nothing for a translator to have translated.
	KindNone Kind = ""

	// KindBlank is a required field that was not supplied.
	KindBlank Kind = "blank"
	// KindConfirmation is a field that had to equal another and did not.
	KindConfirmation Kind = "confirmation"
	// KindInvalid is a field that did not match the expected pattern.
	KindInvalid Kind = "invalid"
	// KindInclusion is a field outside the set of permitted values.
	KindInclusion Kind = "inclusion"
	// KindExclusion is a field inside a set of rejected values.
	KindExclusion Kind = "exclusion"
	// KindTaken is a collection with an element appearing twice.
	KindTaken Kind = "taken"
	// KindNotANumber is a numeric field that is not finite.
	KindNotANumber Kind = "not_a_number"

	// KindEmail, KindURL and KindUUID are the formats a string can be held to.
	KindEmail Kind = "email"
	KindURL   Kind = "url"
	KindUUID  Kind = "uuid"

	// KindTooShort, KindTooLong and KindWrongLength bound a string's length,
	// and interpolate the bound as count.
	KindTooShort    Kind = "too_short"
	KindTooLong     Kind = "too_long"
	KindWrongLength Kind = "wrong_length"

	// KindTooFewItems and KindTooManyItems bound a collection's length, and
	// interpolate the bound as count.
	KindTooFewItems  Kind = "too_few_items"
	KindTooManyItems Kind = "too_many_items"

	// KindPrefix, KindSuffix and KindContains hold a string to a fragment,
	// which they interpolate as value.
	KindPrefix   Kind = "prefix"
	KindSuffix   Kind = "suffix"
	KindContains Kind = "contains"

	// The numeric bounds, each interpolating its limit as count.
	KindGreaterThanOrEqualTo Kind = "greater_than_or_equal_to"
	KindLessThanOrEqualTo    Kind = "less_than_or_equal_to"
	KindMultipleOf           Kind = "multiple_of"

	// KindPositive and KindNegative are the bounds against zero, which name it
	// in words rather than interpolating it. Rails writes these as greater_than
	// and less_than with a count of nought; they are separate here because the
	// wording differs, and wording is what a rule name selects.
	KindPositive Kind = "positive"
	KindNegative Kind = "negative"
	// KindBetween bounds a value on both sides, interpolating min and max.
	KindBetween Kind = "between"

	// The time bounds. KindTimeBetween interpolates earliest and latest; the
	// other two interpolate time.
	KindBefore      Kind = "before"
	KindAfter       Kind = "after"
	KindTimeBetween Kind = "time_between"

	// KindPast and KindFuture are the bounds against now, and KindWithin is a
	// window around it, which it interpolates as duration.
	KindPast   Kind = "past"
	KindFuture Kind = "future"
	KindWithin Kind = "within"

	// KindHTTPS and KindURLScheme narrow a URL to the schemes a field will
	// accept. KindURLScheme interpolates the permitted set as list.
	KindHTTPS     Kind = "https"
	KindURLScheme Kind = "url_scheme"

	// The network addresses.
	KindHost Kind = "host"
	KindIP   Kind = "ip"
	KindIPv4 Kind = "ipv4"
	KindIPv6 Kind = "ipv6"
	KindCIDR Kind = "cidr"
	KindMAC  Kind = "mac"

	// KindNotMatches is a shape a value must not have.
	KindNotMatches Kind = "not_matches"

	// KindNotBlank is a value that is nothing but whitespace, and KindNoControl
	// one holding a character that should never reach a header or a log.
	KindNotBlank  Kind = "not_blank"
	KindNoControl Kind = "no_control"

	// The character classes.
	KindAlpha        Kind = "alpha"
	KindAlphanumeric Kind = "alphanumeric"
	KindNumeric      Kind = "numeric"
	KindASCII        Kind = "ascii"

	// The formats that parse rather than merely match.
	KindSlug         Kind = "slug"
	KindHex          Kind = "hex"
	KindHexColour    Kind = "hex_colour"
	KindBase64       Kind = "base64"
	KindJSON         Kind = "json"
	KindSemver       Kind = "semver"
	KindE164         Kind = "e164"
	KindLanguageTag  Kind = "language_tag"
	KindTimezone     Kind = "timezone"
	KindCountryCode  Kind = "country_code"
	KindCurrencyCode Kind = "currency_code"

	// KindMinBytes and KindMaxBytes bound a value in bytes rather than in
	// characters, which is what a storage column or a protocol field means.
	KindMinBytes Kind = "min_bytes"
	KindMaxBytes Kind = "max_bytes"

	// The exclusive numeric bounds, each interpolating its limit as count, and
	// the two against zero that admit it.
	KindGreaterThan Kind = "greater_than"
	KindLessThan    Kind = "less_than"
	KindNonNegative Kind = "non_negative"
	KindNonPositive Kind = "non_positive"
	KindWhole       Kind = "whole"
	KindPort        Kind = "port"

	// The collection checks. KindWrongItems interpolates count; the membership
	// pair interpolates value.
	KindWrongItems   Kind = "wrong_items"
	KindNotEmpty     Kind = "not_empty"
	KindContainsItem Kind = "contains_item"
	KindExcludesItem Kind = "excludes_item"
)

// kindsOf maps each built-in rule onto the name its message is keyed by.
//
// A rule missing from this table reports [KindNone], which is correct for the
// transforms, for a rule of the caller's own, and for anything whose wording is
// not the framework's to translate.
var kindsOf = map[ruleKind]Kind{
	kindRequired:       KindBlank,
	kindMinLen:         KindTooShort,
	kindMaxLen:         KindTooLong,
	kindLen:            KindWrongLength,
	kindEmail:          KindEmail,
	kindURL:            KindURL,
	kindUUID:           KindUUID,
	kindMatches:        KindInvalid,
	kindOneOfString:    KindInclusion,
	kindOneOfValue:     KindInclusion,
	kindNotOneOfString: KindExclusion,
	kindEqualString:    KindConfirmation,
	kindPrefix:         KindPrefix,
	kindSuffix:         KindSuffix,
	kindContains:       KindContains,
	kindMin:            KindGreaterThanOrEqualTo,
	kindMax:            KindLessThanOrEqualTo,
	kindBetween:        KindBetween,
	kindPositive:       KindPositive,
	kindNegative:       KindNegative,
	kindMultipleOf:     KindMultipleOf,
	kindMinItems:       KindTooFewItems,
	kindMaxItems:       KindTooManyItems,
	kindUnique:         KindTaken,
	kindBefore:         KindBefore,
	kindAfter:          KindAfter,
	kindTimeBetween:    KindTimeBetween,
	kindPast:           KindPast,
	kindFuture:         KindFuture,
	kindWithin:         KindWithin,

	kindHTTPS:     KindHTTPS,
	kindURLScheme: KindURLScheme,

	kindHost: KindHost,
	kindIP:   KindIP,
	kindIPv4: KindIPv4,
	kindIPv6: KindIPv6,
	kindCIDR: KindCIDR,
	kindMAC:  KindMAC,

	kindNotMatches: KindNotMatches,
	kindNotBlank:   KindNotBlank,
	kindNoControl:  KindNoControl,

	kindAlpha:         KindAlpha,
	kindAlphanumeric:  KindAlphanumeric,
	kindNumericString: KindNumeric,
	kindASCII:         KindASCII,

	kindSlug:         KindSlug,
	kindHex:          KindHex,
	kindHexColour:    KindHexColour,
	kindBase64:       KindBase64,
	kindJSON:         KindJSON,
	kindSemver:       KindSemver,
	kindE164:         KindE164,
	kindLanguageTag:  KindLanguageTag,
	kindTimezone:     KindTimezone,
	kindCountryCode:  KindCountryCode,
	kindCurrencyCode: KindCurrencyCode,

	// The same failure as Equal: a value that had to match another and did not.
	kindEqualFold: KindConfirmation,
	kindMinBytes:  KindMinBytes,
	kindMaxBytes:  KindMaxBytes,

	kindGreaterThan: KindGreaterThan,
	kindLessThan:    KindLessThan,
	kindNonNegative: KindNonNegative,
	kindNonPositive: KindNonPositive,
	kindWhole:       KindWhole,
	kindPort:        KindPort,
	// The same failure as a string outside its permitted set.
	kindOneOfNumber: KindInclusion,

	kindItems:        KindWrongItems,
	kindNotEmpty:     KindNotEmpty,
	kindContainsItem: KindContainsItem,
	kindExcludesItem: KindExcludesItem,
}

// argumented is implemented by an error a rule returns when its message
// interpolates something only the check itself could have known, such as the
// element a uniqueness check found twice.
//
// A rule whose parameters all live in its step needs none of this: the values
// are read back off the step after it fails.
type argumented interface {
	// args returns the interpolation arguments, as alternating names and
	// values.
	args() []any
}

// element is the failure a collection check reports, carrying the element it
// found or failed to find. Uniqueness names the one that repeated, and a
// membership check names the one that was wanted or forbidden.
type element struct {
	// text is the English message, which the error interface has to produce.
	text string
	// value is the element that repeated.
	value any
}

// Error renders the English wording.
func (e element) Error() string { return e.text }

// args reports the element for interpolation.
func (e element) args() []any { return []any{"value", e.value} }

// bounded is the failure a time bound reports, carrying the limits it was held
// to. They live in the closure rather than on the step, because a moment does
// not fit in the numeric fields a step carries.
type bounded struct {
	// text is the English message.
	text string
	// by are the limits, as alternating names and values.
	by []any
}

// Error renders the English wording.
func (e bounded) Error() string { return e.text }

// args reports the limits for interpolation.
func (e bounded) args() []any { return e.by }

// Kinds lists every rule a built-in failure can name, in a stable order.
//
// It exists so that the locale shipped with the framework can be checked
// against the rules that actually exist: a rule added without a message is then
// a failing test rather than a "translation missing" reaching a client.
func Kinds() []Kind {
	out := make([]Kind, 0, len(kindsOf)+1)
	seen := map[Kind]bool{}
	// Reported before any rule runs rather than by one of them, since a value
	// that is not a number cannot be held to a bound.
	for _, kind := range append(kinds(), KindNotANumber) {
		if !seen[kind] {
			seen[kind] = true
			out = append(out, kind)
		}
	}
	// The map is walked in an unspecified order, so the result is sorted to be
	// the same on every run.
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// kinds lists the rules in the table, which is every failure produced by a rule
// set rather than before one.
func kinds() []Kind {
	out := make([]Kind, 0, len(kindsOf))
	for _, kind := range kindsOf {
		out = append(out, kind)
	}
	return out
}
