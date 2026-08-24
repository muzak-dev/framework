package validate

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// This file holds the predicates behind the format rules. They are ordinary
// functions rather than closures on a step, so that declaring a rule set still
// allocates nothing and the checks can be tested on their own.

// isHTTPSURL reports whether a value is an absolute https URL with a host.
func isHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") && parsed.Host != ""
}

// isURLWithScheme reports whether a value is an absolute URL using one of the
// permitted schemes.
//
// A scheme with an authority must carry a host, so "https://" alone is refused.
// One without, such as mailto, must carry something after the colon instead.
func isURLWithScheme(value string, schemes []string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return false
	}
	permitted := false
	for _, scheme := range schemes {
		if strings.EqualFold(parsed.Scheme, scheme) {
			permitted = true
			break
		}
	}
	return permitted && (parsed.Host != "" || parsed.Opaque != "")
}

// isHostname reports whether a value is a valid DNS hostname.
//
// The rules are the ones resolvers actually enforce: at most 253 characters,
// labels of 1 to 63 of letters, digits and hyphens, and no label beginning or
// ending with a hyphen. A single trailing dot is allowed, because a fully
// qualified name is written that way.
func isHostname(value string) bool {
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !isHostLabel(label) {
			return false
		}
	}
	return true
}

// isHostLabel reports whether one dot-separated part of a hostname is legal.
func isHostLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// isIP reports whether a value is an IP address of either family.
func isIP(value string) bool {
	_, err := netip.ParseAddr(value)
	return err == nil
}

// isIPv4 reports whether a value is an IPv4 address.
func isIPv4(value string) bool {
	addr, err := netip.ParseAddr(value)
	return err == nil && addr.Is4()
}

// isIPv6 reports whether a value is an IPv6 address.
//
// An IPv4 address written in the mapped form is an IPv4 address wearing a
// costume, so it is not accepted here: a field asking for IPv6 wants an address
// an IPv6-only network can route.
func isIPv6(value string) bool {
	addr, err := netip.ParseAddr(value)
	return err == nil && addr.Is6() && !addr.Is4In6()
}

// isCIDR reports whether a value is a network written in CIDR notation.
func isCIDR(value string) bool {
	_, err := netip.ParsePrefix(value)
	return err == nil
}

// isMAC reports whether a value is a hardware address.
func isMAC(value string) bool {
	_, err := net.ParseMAC(value)
	return err == nil
}

// isBlank reports whether a value is nothing but whitespace.
//
// It is the check a presence rule cannot make. Required rejects the empty
// string, and a field of three spaces is not empty, so it satisfies Required
// while carrying nothing a person would call a value.
func isBlank(value string) bool {
	return strings.TrimSpace(value) == ""
}

// hasControl reports whether a value holds a control character.
//
// Tabs and line breaks count. A value that reaches a response header, a log
// line or a redirect carrying a carriage return is how an injection starts, and
// a field that legitimately contains one is rare enough to be worth declaring
// with a rule of its own.
func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// isAlpha reports whether every character is a letter.
//
// Letters are Unicode letters, not the twenty-six of English, so a name in any
// script passes. Combine with ASCII to narrow it.
func isAlpha(value string) bool {
	return value != "" && everyRune(value, unicode.IsLetter)
}

// isAlphanumeric reports whether every character is a letter or a digit.
func isAlphanumeric(value string) bool {
	return value != "" && everyRune(value, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	})
}

// isNumericString reports whether every character is a digit.
func isNumericString(value string) bool {
	return value != "" && everyRune(value, unicode.IsDigit)
}

// isPrintableASCII reports whether every character is printable ASCII.
//
// Space through tilde, which is the set a protocol field, an identifier or a
// header value can carry without anyone having to think about encoding.
func isPrintableASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return value != ""
}

// everyRune reports whether a predicate holds for every character.
func everyRune(value string, ok func(rune) bool) bool {
	for _, r := range value {
		if !ok(r) {
			return false
		}
	}
	return true
}

// isSlug reports whether a value is a slug: lower case letters and digits in
// groups separated by single hyphens.
func isSlug(value string) bool {
	if value == "" || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	previousHyphen := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '-':
			if previousHyphen {
				return false
			}
			previousHyphen = true
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			previousHyphen = false
		default:
			return false
		}
	}
	return true
}

// isHex reports whether every character is a hexadecimal digit.
func isHex(value string) bool {
	return value != "" && everyRune(value, func(r rune) bool {
		return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
	})
}

// isHexColour reports whether a value is a colour written in hexadecimal, as
// "#1a2b3c". Three, six and eight digits are accepted, the last being the form
// that carries an alpha channel.
func isHexColour(value string) bool {
	if !strings.HasPrefix(value, "#") {
		return false
	}
	digits := value[1:]
	switch len(digits) {
	case 3, 4, 6, 8:
		return isHex(digits)
	default:
		return false
	}
}

// isBase64 reports whether a value decodes as standard base64.
func isBase64(value string) bool {
	_, err := base64.StdEncoding.DecodeString(value)
	return err == nil
}

// isJSON reports whether a value is a well-formed JSON document.
func isJSON(value string) bool {
	return jsontext.Value(value).IsValid()
}

// semverPattern is the expression semver.org publishes for a version.
var semverPattern = regexp.MustCompile(
	`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// e164Pattern is a telephone number in the international format: a plus, a
// country code that cannot begin with nought, and up to fifteen digits in all.
var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)

// languageTagPattern is the shape of a BCP 47 tag: a language, and optionally a
// script, a region and variants.
//
// It checks the shape rather than the registry. A tag of the right shape naming
// a language nobody speaks is a translation nobody wrote, which is a missing
// translation rather than a malformed request; a tag of the wrong shape is a
// mistake worth reporting.
var languageTagPattern = regexp.MustCompile(
	`^[a-zA-Z]{2,8}(?:-[a-zA-Z]{4})?(?:-(?:[a-zA-Z]{2}|[0-9]{3}))?` +
		`(?:-(?:[0-9a-zA-Z]{5,8}|[0-9][0-9a-zA-Z]{3}))*$`)

// isSemver reports whether a value is a semantic version.
func isSemver(value string) bool { return semverPattern.MatchString(value) }

// isE164 reports whether a value is a telephone number in international format.
func isE164(value string) bool { return e164Pattern.MatchString(value) }

// isLanguageTag reports whether a value is shaped like a BCP 47 language tag.
func isLanguageTag(value string) bool { return languageTagPattern.MatchString(value) }

// knownZones remembers the time zone names that turned out to be real.
//
// Loading a zone reads the tzdata the host or the binary carries, which is far
// too much work to repeat on every request. Only successes are remembered, so
// the map is bounded by the number of zones that exist rather than by the
// number of strings a client is willing to send.
var knownZones sync.Map

// isTimezone reports whether a value names a time zone the host knows.
func isTimezone(value string) bool {
	if value == "" || !isZoneShaped(value) {
		return false
	}
	if _, known := knownZones.Load(value); known {
		return true
	}
	if _, err := time.LoadLocation(value); err != nil {
		return false
	}
	knownZones.Store(value, struct{}{})
	return true
}

// isZoneShaped reports whether a value could be a zone name at all.
//
// It runs before the zone is looked up, so that a client sending nonsense pays
// for a scan of the string rather than for a search of the tzdata.
func isZoneShaped(value string) bool {
	if len(value) > 64 {
		return false
	}
	return everyRune(value, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return true
		default:
			return r == '/' || r == '_' || r == '+' || r == '-'
		}
	})
}

// isCountryCode reports whether a value is shaped like an ISO 3166-1 alpha-2
// country code: two upper case letters.
//
// The shape is checked rather than the register. A table of the assigned codes
// would be a few hundred entries that cannot be verified from here and that go
// out of date as countries are added and withdrawn, and a stale table rejects
// valid input, which is worse than admitting a pair of letters nobody has
// assigned yet. The mistakes this does catch are the ones that actually happen:
// a full country name, a lower case code, or a currency code in the wrong
// field.
func isCountryCode(value string) bool {
	return len(value) == 2 && isUpperLetters(value)
}

// isCurrencyCode reports whether a value is shaped like an ISO 4217 currency
// code: three upper case letters. It checks the shape for the same reason
// [isCountryCode] does.
func isCurrencyCode(value string) bool {
	return len(value) == 3 && isUpperLetters(value)
}

// isUpperLetters reports whether every byte is an upper case ASCII letter.
func isUpperLetters(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 'A' || value[i] > 'Z' {
			return false
		}
	}
	return true
}

// The expressions that state, for the generated document, what the character
// class and shape rules enforce.
//
// They are written here beside the predicates they describe rather than at the
// point of use, because the two have to agree and a test in this package checks
// that they do: an expression that admitted something the predicate refuses
// would describe an endpoint that does not exist.
//
// JSON Schema carries one pattern per value, so a rule set declaring two of
// these describes the last of them. The rules still both run.
const (
	patternAlpha        = `^\p{L}+$`
	patternAlphanumeric = `^[\p{L}\p{Nd}]+$`
	patternNumeric      = `^\p{Nd}+$`
	patternASCII        = `^[ -~]+$`
	patternSlug         = `^[a-z0-9]+(?:-[a-z0-9]+)*$`
	patternHex          = `^[0-9a-fA-F]+$`
	patternHexColour    = `^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`
	patternE164         = `^\+[1-9]\d{1,14}$`
	patternCountryCode  = `^[A-Z]{2}$`
	patternCurrencyCode = `^[A-Z]{3}$`
)
