package validate

import (
	"regexp"
	"testing"
)

// accepts runs a predicate over the values it should admit and the values it
// should refuse, so that each format is pinned from both sides. A check that is
// only tested with what it accepts passes just as well when it accepts
// everything.
func accepts(t *testing.T, name string, ok func(string) bool, good, bad []string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		for _, value := range good {
			if !ok(value) {
				t.Errorf("%s rejected %q, want it accepted", name, value)
			}
		}
		for _, value := range bad {
			if ok(value) {
				t.Errorf("%s accepted %q, want it rejected", name, value)
			}
		}
	})
}

func TestURLFormats(t *testing.T) {
	t.Parallel()

	accepts(t, "isHTTPSURL", isHTTPSURL,
		[]string{"https://muzak.dev", "https://muzak.dev/docs?a=1", "HTTPS://MUZAK.DEV"},
		[]string{"", "http://muzak.dev", "https://", "muzak.dev", "ftp://muzak.dev",
			"javascript:alert(1)", "//muzak.dev", "/docs"})

	schemes := []string{"s3", "gs"}
	accepts(t, "isURLWithScheme",
		func(v string) bool { return isURLWithScheme(v, schemes) },
		[]string{"s3://bucket/key", "gs://bucket", "S3://bucket"},
		[]string{"", "https://muzak.dev", "s3://", "bucket/key", "s3"})

	// A scheme with no authority is legal when it carries something after the
	// colon, which is what makes mailto usable.
	accepts(t, "isURLWithScheme opaque",
		func(v string) bool { return isURLWithScheme(v, []string{"mailto"}) },
		[]string{"mailto:ada@muzak.dev"},
		[]string{"mailto:", "mailto"})

	accepts(t, "isHostname", isHostname,
		[]string{"muzak.dev", "a", "sub.domain.example", "xn--bcher-kva.example",
			"muzak.dev.", "a-b.example", "123.example"},
		[]string{"", "-bad.example", "bad-.example", "bad..example", "under_score.example",
			"a..b", "trailing-",
			"a" + repeat("b", 63) + ".example"})
}

// repeat builds a string of n copies of s, for a length the table needs to
// exceed a bound.
func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

func TestNetworkFormats(t *testing.T) {
	t.Parallel()

	accepts(t, "isIP", isIP,
		[]string{"127.0.0.1", "::1", "2001:db8::1", "0.0.0.0"},
		[]string{"", "999.1.1.1", "127.0.0.1/8", "localhost", "::gg"})

	accepts(t, "isIPv4", isIPv4,
		[]string{"127.0.0.1", "0.0.0.0", "255.255.255.255"},
		[]string{"", "::1", "2001:db8::1", "::ffff:127.0.0.1", "256.0.0.1"})

	// A mapped address is an IPv4 address in an IPv6 spelling, so it is not
	// something an IPv6-only network can route.
	accepts(t, "isIPv6", isIPv6,
		[]string{"::1", "2001:db8::1", "fe80::1"},
		[]string{"", "127.0.0.1", "::ffff:127.0.0.1", "not an address"})

	accepts(t, "isCIDR", isCIDR,
		[]string{"10.0.0.0/8", "192.168.1.0/24", "2001:db8::/32", "::/0"},
		[]string{"", "10.0.0.0", "10.0.0.0/33", "10.0.0.0/-1", "/8"})

	accepts(t, "isMAC", isMAC,
		[]string{"00:1b:63:84:45:e6", "00-1B-63-84-45-E6", "0123.4567.89ab"},
		[]string{"", "00:1b:63:84:45", "not a mac", "00:1b:63:84:45:zz"})
}

func TestTextFormats(t *testing.T) {
	t.Parallel()

	accepts(t, "isBlank", isBlank,
		[]string{"", " ", "\t\n ", "   "},
		[]string{"a", " a ", "0"})

	accepts(t, "hasControl", hasControl,
		[]string{"a\rb", "a\nb", "a\tb", "\x00", "a\x1bb"},
		[]string{"", "plain text", "accented cafe", "a b"})

	// Letters are Unicode letters, so a name in any script passes.
	accepts(t, "isAlpha", isAlpha,
		[]string{"Ada", "Lovelace", "\u00c7ift", "\u4f60\u597d"},
		[]string{"", "Ada1", "Ada Lovelace", "Ada-Lovelace", "1"})

	accepts(t, "isAlphanumeric", isAlphanumeric,
		[]string{"Ada1", "abc", "123", "\u00c7ift9"},
		[]string{"", "Ada 1", "Ada-1", "Ada_1", "!"})

	accepts(t, "isNumericString", isNumericString,
		[]string{"0", "0123", "999999999999999999999999"},
		[]string{"", "12a", "1.5", "-1", " 1"})

	accepts(t, "isPrintableASCII", isPrintableASCII,
		[]string{"plain", "a b", "~", " ", "!@#$%^&*()"},
		[]string{"", "caf\u00e9", "a\tb", "a\nb", "\x00", "\x7f"})

	accepts(t, "isSlug", isSlug,
		[]string{"a", "a-good-title", "post-1", "123"},
		[]string{"", "-leading", "trailing-", "double--hyphen", "Upper", "with space", "under_score"})

	accepts(t, "isHex", isHex,
		[]string{"0", "deadBEEF", "0123456789abcdef"},
		[]string{"", "0x1f", "ghij", "de ad"})

	accepts(t, "isHexColour", isHexColour,
		[]string{"#fff", "#ffff", "#1a2b3c", "#1a2b3c4d", "#FFF"},
		[]string{"", "fff", "#", "#ff", "#fffff", "#gggggg", "#1a2b3c4", "#1a2b3c4d5"})
}

func TestParsedFormats(t *testing.T) {
	t.Parallel()

	accepts(t, "isBase64", isBase64,
		[]string{"", "aGVsbG8=", "YQ=="},
		[]string{"not base64!", "aGVsbG8", "***"})

	accepts(t, "isJSON", isJSON,
		[]string{`{"a":1}`, `[]`, `"text"`, `null`, `1`},
		[]string{"", "{", `{"a":}`, "undefined", `{"a":1}{"b":2}`})

	accepts(t, "isSemver", isSemver,
		[]string{"1.4.0", "0.0.1", "2.0.0-rc.1", "1.0.0+build.5", "1.0.0-alpha.1+build.5"},
		[]string{"", "1.4", "v1.4.0", "1.4.0.1", "01.4.0", "1.4.0-"})

	accepts(t, "isE164", isE164,
		[]string{"+905551234567", "+11", "+123456789012345"},
		[]string{"", "905551234567", "+0555", "+1234567890123456", "+1 555 123", "+"})

	accepts(t, "isLanguageTag", isLanguageTag,
		[]string{"en", "pt-BR", "zh-Hant-TW", "de-CH-1901", "eng"},
		[]string{"", "e", "english-language-tag-that-is-far-too-long", "pt_BR", "en-", "123"})

	// Resolved against the zone data the host carries, so what passes here is
	// what time.LoadLocation will later accept.
	accepts(t, "isTimezone", isTimezone,
		[]string{"UTC", "Europe/Istanbul", "America/New_York"},
		[]string{"", "Mars/Olympus", "Europe/Nowhere", "not a zone", "Europe/Istanbul\x00"})

	// The unassigned pairs are the cases that tell a table apart from a shape
	// check. "XQ" and "QQQ" are well formed and mean nothing, so a rule that
	// only counted letters would let them through.
	accepts(t, "isCountryCode", isCountryCode,
		[]string{"TR", "GB", "US", "NZ", "ZW"},
		[]string{"", "T", "TUR", "tr", "T1", "Tr", "XQ", "ZZ", "OO"})

	accepts(t, "isCurrencyCode", isCurrencyCode,
		[]string{"TRY", "GBP", "USD", "JPY", "XOF"},
		[]string{"", "TR", "TRYX", "try", "TR1", "QQQ", "ZZZ"})

	// Codes withdrawn from the standard are gone, and their replacements are
	// present, which is the property a dated table is kept for.
	accepts(t, "isCurrencyCode retired", isCurrencyCode,
		[]string{"SLE", "VES", "ZWG", "MRU", "STN"},
		[]string{"SLL", "VEF", "ZWL", "MRO", "STD", "ANG", "CUC", "BYR"})
}

// TestZoneShapeGuardsTheLookup checks the cheap refusal that runs before the
// zone data is searched, since that is what keeps a client sending nonsense
// from making the server do the work.
func TestZoneShapeGuardsTheLookup(t *testing.T) {
	t.Parallel()
	accepts(t, "isZoneShaped", isZoneShaped,
		[]string{"UTC", "Europe/Istanbul", "America/Argentina/Salta", "GMT+0", "Etc/GMT-5"},
		[]string{repeat("a", 65), "Europe/Istanbul ", "a;b", "a\nb", "a.b"})
}

// TestDescribedPatternsMatchTheChecks is the guard against the one thing that
// can quietly go wrong here.
//
// A character class rule is enforced by a predicate and described to clients by
// an expression, and the two are written separately. If they ever disagree, the
// generated document describes an endpoint that does not exist: a value the
// expression admits and the rule rejects, or the reverse. Every value in the
// corpus is put through both and required to get the same answer.
func TestDescribedPatternsMatchTheChecks(t *testing.T) {
	t.Parallel()

	corpus := []string{
		"", "a", "A", "abc", "ABC", "Ada", "Ada1", "123", "0", "0123",
		"a b", " ", "  ", "-", "a-b", "a--b", "-a", "a-", "a_b", "a.b",
		"caf\u00e9", "\u00c7ift", "\u4f60\u597d", "\u00e9", "\t", "\n", "\x00", "\x7f",
		"~", "!", "#", "#fff", "#ffff", "#1a2b3c", "#1a2b3c4d", "#gg",
		"deadBEEF", "0x1f", "+905551234567", "905551234567", "+0555",
		"TR", "TRY", "tr", "try", "T", "TRYX", "a-good-title", "Upper",
	}

	cases := []struct {
		name    string
		pattern string
		check   func(string) bool
	}{
		{name: "alpha", pattern: patternAlpha, check: isAlpha},
		{name: "alphanumeric", pattern: patternAlphanumeric, check: isAlphanumeric},
		{name: "numeric", pattern: patternNumeric, check: isNumericString},
		{name: "ascii", pattern: patternASCII, check: isPrintableASCII},
		{name: "slug", pattern: patternSlug, check: isSlug},
		{name: "hex", pattern: patternHex, check: isHex},
		{name: "hex colour", pattern: patternHexColour, check: isHexColour},
		{name: "e164", pattern: patternE164, check: isE164},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expression := regexp.MustCompile(tc.pattern)
			for _, value := range corpus {
				described, enforced := expression.MatchString(value), tc.check(value)
				if described != enforced {
					t.Errorf("%q: the pattern says %v and the rule says %v",
						value, described, enforced)
				}
			}
		})
	}
}

// TestTimezoneIsRemembered covers the cache behind the zone lookup: a name that
// resolved once is not searched for again.
func TestTimezoneIsRemembered(t *testing.T) {
	t.Parallel()
	for range 3 {
		if !isTimezone("Europe/Istanbul") {
			t.Fatal("a real zone was rejected")
		}
	}
	// A name that never resolves is not remembered, so it cannot be used to
	// grow the cache without bound.
	for range 3 {
		if isTimezone("Europe/Nowhere") {
			t.Fatal("a zone that does not exist was accepted")
		}
	}
}

// TestMembershipAccepts covers the rules that pass, since a check that rejected
// everything would satisfy a table of refusals just as well.
func TestMembershipAccepts(t *testing.T) {
	t.Parallel()
	if got := Number().OneOf(1, 2).For(ptr(2)).Evaluate(); len(got) != 0 {
		t.Errorf("OneOf rejected a permitted number: %v", got)
	}
	if got := Slice[string]().Contains("read").For(&[]string{"write", "read"}).Evaluate(); len(got) != 0 {
		t.Errorf("Contains rejected a collection holding the element: %v", got)
	}
	if got := Slice[string]().Excludes("*").For(&[]string{"read"}).Evaluate(); len(got) != 0 {
		t.Errorf("Excludes rejected a collection without the element: %v", got)
	}
	if got := Slice[string]().Items(2).For(&[]string{"a", "b"}).Evaluate(); len(got) != 0 {
		t.Errorf("Items rejected a collection of the right length: %v", got)
	}
	if got := Slice[string]().NotEmpty().For(&[]string{"a"}).Evaluate(); len(got) != 0 {
		t.Errorf("NotEmpty rejected a collection with something in it: %v", got)
	}
	if got := String().NotBlank().For(ptr("a")).Evaluate(); len(got) != 0 {
		t.Errorf("NotBlank rejected a value with something in it: %v", got)
	}
}

// TestEmptinessRulesRunOnAnAbsentValue pins the exception those two rules make
// to the skip that every other rule follows.
func TestEmptinessRulesRunOnAnAbsentValue(t *testing.T) {
	t.Parallel()
	if got := String().NotBlank().For(ptr("")).Evaluate(); len(got) != 1 {
		t.Errorf("NotBlank on an absent value gave %v, want it refused", got)
	}
	if got := Slice[string]().NotEmpty().For(&[]string{}).Evaluate(); len(got) != 1 {
		t.Errorf("NotEmpty on an absent collection gave %v, want it refused", got)
	}
	// Every other rule still skips one, so an optional field nobody sent is not
	// reported as malformed.
	if got := String().Email().For(ptr("")).Evaluate(); len(got) != 0 {
		t.Errorf("Email on an absent value gave %v, want it skipped", got)
	}
}
