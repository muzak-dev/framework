package validate

import (
	"errors"
	"regexp"
	"sync/atomic"
	"testing"
	"time"
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

	// A zone is free text to the parser, which is how a header injection or
	// markup used to pass as an address.
	zoned := []string{"fe80::1%eth0", "fe80::1%\r\nSet-Cookie: pwn=1", "::1%<script>alert(1)</script>",
		"fe80::1%../../etc/passwd", "fe80::1%25"}

	accepts(t, "isIP", isIP,
		[]string{"127.0.0.1", "::1", "2001:db8::1", "0.0.0.0"},
		append([]string{"", "999.1.1.1", "127.0.0.1/8", "localhost", "::gg"}, zoned...))

	accepts(t, "isIPv4", isIPv4,
		[]string{"127.0.0.1", "0.0.0.0", "255.255.255.255"},
		[]string{"", "::1", "2001:db8::1", "::ffff:127.0.0.1", "256.0.0.1"})

	// A mapped address is an IPv4 address in an IPv6 spelling, so it is not
	// something an IPv6-only network can route.
	accepts(t, "isIPv6", isIPv6,
		[]string{"::1", "2001:db8::1", "fe80::1"},
		append([]string{"", "127.0.0.1", "::ffff:127.0.0.1", "not an address"}, zoned...))

	accepts(t, "isCIDR", isCIDR,
		[]string{"10.0.0.0/8", "192.168.1.0/24", "2001:db8::/32", "::/0"},
		[]string{"", "10.0.0.0", "10.0.0.0/33", "10.0.0.0/-1", "/8", "fe80::%eth0/64"})

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

	// The dependent territories are assigned codes too, and a service taking an
	// address meets them. Several have their own currency, so leaving them out
	// would accept the money and refuse the place it is spent.
	accepts(t, "isCountryCode territories", isCountryCode,
		[]string{"HK", "MO", "PR", "GI", "KY", "AW", "BM", "FK", "SH", "GL", "FO", "PS", "AQ"},
		[]string{"AN", "CS", "YU", "XK", "UK", "EU"})

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

// TestTimezoneRefusesSpellingsTheDatabaseDoesNot is the regression test for a
// cache of every spelling time.LoadLocation accepted. It cleans a path and, on
// a case-insensitive filesystem, ignores case, so a client could add a new
// entry per request for as long as the process lived.
func TestTimezoneRefusesSpellingsTheDatabaseDoesNot(t *testing.T) {
	t.Parallel()
	for _, spelling := range []string{
		"Europe//Paris", "EUROPE/pArIs", "europe/paris", "Europe/Paris/", "/Europe/Paris",
		"Europe/./Paris", "Local", "posix/Europe/Paris", "Europe/X0000001",
	} {
		if isTimezone(spelling) {
			t.Errorf("%q was accepted", spelling)
		}
	}
	if !TimezoneDataAvailable() {
		t.Skip("this host has no time zone database to accept the correct spellings from")
	}
	for _, name := range []string{"Europe/Paris", "Asia/Calcutta", "Asia/Kolkata", "Etc/GMT-5", "UTC"} {
		if !isTimezone(name) {
			t.Errorf("%q was refused", name)
		}
	}
}

// TestZoneNamesAreSpelledAsTheyLoad checks the table itself: every entry is a
// name the shape check lets through, none is listed twice, and on a host with
// zone data every one of them loads, which is what catches a typo.
func TestZoneNamesAreSpelledAsTheyLoad(t *testing.T) {
	t.Parallel()
	if len(zoneIndex()) != len(zoneNames) {
		t.Errorf("the table lists %d names but indexes %d: one is listed twice", len(zoneNames), len(zoneIndex()))
	}
	available := TimezoneDataAvailable()
	for _, name := range zoneNames {
		if !isZoneShaped(name) {
			t.Errorf("%q is not shaped like a zone name", name)
		}
		if available && !isTimezone(name) {
			t.Errorf("%q does not load", name)
		}
	}
}

// TestResolveZoneLoadsEachNameOnce covers the memory behind a listed name: it
// is loaded the first time and answered from what was learned after that,
// whichever way the load went.
func TestResolveZoneLoadsEachNameOnce(t *testing.T) {
	t.Parallel()
	loads := 0
	found := func(string) (*time.Location, error) { loads++; return time.UTC, nil }
	missing := func(string) (*time.Location, error) { loads++; return nil, errors.New("no zone data") }

	var present, absent atomic.Uint32
	for range 3 {
		if !resolveZone(&present, "Europe/Istanbul", found) {
			t.Fatal("a zone that loads was refused")
		}
		if resolveZone(&absent, "Europe/Istanbul", missing) {
			t.Fatal("a zone that does not load was accepted")
		}
	}
	if loads != 2 {
		t.Errorf("the zone data was read %d times, want once per name", loads)
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

// TestCodeTablesAreComplete pins the size of both registers.
//
// A table is easy to edit and easy to half edit. Counting the entries catches
// the paste that dropped a line, and pinning the number is what makes a later
// change deliberate: regenerating the table moves it, and the number has to be
// updated alongside the date above the table.
func TestCodeTablesAreComplete(t *testing.T) {
	t.Parallel()

	// Every officially assigned ISO 3166-1 alpha-2 code, as of the date on the
	// table: the independent states, the dependent territories and the special
	// areas.
	if got := len(countryCodes); got != 249 {
		t.Errorf("countryCodes holds %d codes, want 249", got)
	}
	if got := len(currencyCodes); got != 155 {
		t.Errorf("currencyCodes holds %d codes, want 155", got)
	}

	// Neither table may hold anything that is not the right shape, which is
	// what the generated document still advertises.
	for code := range countryCodes {
		if len(code) != 2 || !isUpperASCII(code) {
			t.Errorf("countryCodes holds %q, which is not two upper case letters", code)
		}
	}
	for code := range currencyCodes {
		if len(code) != 3 || !isUpperASCII(code) {
			t.Errorf("currencyCodes holds %q, which is not three upper case letters", code)
		}
	}

	// A currency belonging to a territory is useless without the territory, so
	// the two tables have to agree about which places exist.
	for currency, territory := range map[string]string{
		"AWG": "AW", "BMD": "BM", "FKP": "FK", "GIP": "GI",
		"HKD": "HK", "KYD": "KY", "MOP": "MO", "SHP": "SH",
	} {
		if _, has := currencyCodes[currency]; !has {
			continue
		}
		if _, has := countryCodes[territory]; !has {
			t.Errorf("%s is a currency but %s is not a country, so a form taking both would refuse the pair",
				currency, territory)
		}
	}
}

// isUpperASCII reports whether every byte is an upper case ASCII letter.
func isUpperASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 'A' || value[i] > 'Z' {
			return false
		}
	}
	return true
}

// TestIPRulesRefuseAZone is the review's proof of concept through the public
// rules rather than the predicates: every one of these passed IP and IPv6.
func TestIPRulesRefuseAZone(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"fe80::1%\r\nSet-Cookie: pwn=1",
		"::1%<script>alert(1)</script>",
		"fe80::1%../../etc/passwd",
	} {
		if err := String().IP().Check(value); err == nil || err.Error() != "must be a valid IP address" {
			t.Errorf("IP().Check(%q) = %v, want it refused", value, err)
		}
		if err := String().IPv6().Check(value); err == nil {
			t.Errorf("IPv6().Check(%q) accepted it", value)
		}
	}
	if err := String().IPv6().Check("fe80::1"); err != nil {
		t.Errorf("IPv6().Check(fe80::1) = %v, want a link-local address without a zone accepted", err)
	}
}

// runes spells a string from code points, so that the characters below are
// named by number and this file stays plain ASCII.
func runes(codes ...rune) string { return string(codes) }

// TestEmailRefusesHiddenCharacters pins both sides of the email rule: nothing
// invisible gets through, and addresses in other scripts, lookalikes
// included, still do.
func TestEmailRefusesHiddenCharacters(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"admin" + runes(0x202e) + "@example.com",                   // right-to-left override
		"a" + runes(0x0085) + "b@example.com",                      // next line, a C1 control
		"a" + runes(0x200b) + "@example.com",                       // zero-width space
		"a" + runes(0x200d) + "@example.com",                       // zero-width joiner
		"a" + runes(0x00ad) + "b@example.com",                      // soft hyphen
		"a" + runes(0xfeff) + "@example.com",                       // byte order mark
		"a" + runes(0x2066) + "b" + runes(0x2069) + "@example.com", // directional isolates
		"a" + runes(0x2028) + "b@example.com",                      // line separator
		"a" + runes(0x2029) + "b@example.com",                      // paragraph separator
		"a@exam" + runes(0x200b) + "ple.com",                       // hidden in the domain
		"a" + runes(0x7f) + "@example.com",                         // delete, a C0 control
	} {
		if err := String().Email().Check(value); err == nil || err.Error() != "must be a valid email address" {
			t.Errorf("Email().Check(%q) = %v, want it refused", value, err)
		}
	}
	for _, value := range []string{
		"ada@muzak.dev",
		"first.last+tag@example.co.uk",
		"jos" + runes(0xe9) + "@example.com",    // accented Latin
		"adm" + runes(0x0456) + "n@example.com", // Cyrillic i, a lookalike
		runes(0x7528, 0x6237) + "@" + runes(0x4f8b, 0x5b50) + "." + runes(0x5e7f, 0x544a),      // Chinese, local part and domain
		runes(0x0627, 0x0644, 0x0645, 0x0633, 0x062a, 0x062e, 0x062f, 0x0645) + "@example.com", // Arabic, right to left
	} {
		if err := String().Email().Check(value); err != nil {
			t.Errorf("Email().Check(%q) = %v, want it accepted", value, err)
		}
	}
}
