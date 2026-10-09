package i18n

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestPluralRules covers every rule this package ships, with the counts CLDR
// gives as examples for each category.
//
// The table is exhaustive on purpose. A plural rule is arithmetic transcribed
// from a specification, and the way it goes wrong is a boundary being off by
// one in a language nobody on the team reads, which no amount of spot checking
// finds.
func TestPluralRules(t *testing.T) {
	t.Parallel()
	cases := map[string]map[int]PluralCategory{
		"other": {0: Other, 1: Other, 2: Other, 5: Other, 100: Other},

		"one_other": {0: Other, 1: One, 2: Other, 11: Other, 21: Other, -1: One},

		"one_other_zero_one": {0: One, 1: One, 2: Other, 11: Other, -1: One},
		"hindi":              {0: One, 1: One, 2: Other, 11: Other, -1: One},
		"punjabi":            {0: One, 1: One, 2: Other, 11: Other, -1: One},

		"slavic": {
			1: One, 21: One, 31: One, 101: One,
			2: Few, 3: Few, 4: Few, 22: Few, 104: Few,
			0: Many, 5: Many, 9: Many, 11: Many, 12: Many, 14: Many, 25: Many, 111: Many,
		},

		"belarusian": {
			1: One, 21: One, 101: One,
			2: Few, 4: Few, 22: Few,
			0: Many, 5: Many, 11: Many, 12: Many, 14: Many, 111: Many,
		},

		"serbo_croatian": {
			1: One, 21: One, 101: One,
			2: Few, 4: Few, 22: Few,
			0: Other, 5: Other, 11: Other, 12: Other, 14: Other,
		},

		"west_slavic": {1: One, 2: Few, 3: Few, 4: Few, 0: Other, 5: Other, 22: Other},

		"polish": {
			1: One,
			2: Few, 4: Few, 22: Few, 104: Few,
			0: Many, 5: Many, 12: Many, 14: Many, 21: Many, 111: Many,
		},

		"lithuanian": {
			1: One, 21: One, 101: One,
			2: Few, 9: Few, 22: Few,
			0: Other, 10: Other, 11: Other, 12: Other, 19: Other, 111: Other,
		},

		"latvian": {
			0: Zero, 10: Zero, 11: Zero, 19: Zero, 20: Zero, 111: Zero,
			1: One, 21: One, 101: One,
			2: Other, 22: Other, 9: Other,
		},

		"romanian": {
			1: One,
			0: Few, 2: Few, 19: Few, 101: Few, 119: Few,
			20: Other, 100: Other, 120: Other,
		},

		"arabic": {
			0: Zero,
			1: One,
			2: Two,
			3: Few, 10: Few, 103: Few, 110: Few,
			11: Many, 26: Many, 99: Many, 111: Many,
			100: Other, 101: Other, 102: Other, 200: Other,
		},

		"welsh": {0: Zero, 1: One, 2: Two, 3: Few, 6: Many, 4: Other, 5: Other, 7: Other},

		"irish": {
			1: One, 2: Two,
			3: Few, 4: Few, 6: Few,
			7: Many, 9: Many, 10: Many,
			0: Other, 11: Other, 20: Other,
		},

		"scottish_gaelic": {
			1: One, 11: One,
			2: Two, 12: Two,
			3: Few, 10: Few, 13: Few, 19: Few,
			0: Other, 20: Other, 21: Other,
		},

		"maltese": {
			1: One,
			0: Few, 2: Few, 10: Few, 102: Few, 110: Few,
			11: Many, 19: Many, 111: Many,
			20: Other, 100: Other, 101: Other,
		},

		"slovenian": {
			1: One, 101: One,
			2: Two, 102: Two,
			3: Few, 4: Few, 103: Few,
			0: Other, 5: Other, 100: Other,
		},

		"icelandic":  {1: One, 21: One, 31: One, 0: Other, 2: Other, 11: Other, 111: Other},
		"macedonian": {1: One, 21: One, 31: One, 0: Other, 2: Other, 11: Other, 111: Other},

		"hebrew": {1: One, 2: Two, 0: Other, 3: Other, 10: Other, 20: Other},

		"filipino": {
			1: One, 2: One, 3: One, 5: One, 7: One, 10: One,
			4: Other, 6: Other, 9: Other, 14: Other, 16: Other, 19: Other,
		},
	}

	// Every rule the package ships must appear above, so that adding one
	// without a test is a failure rather than an omission.
	names := PluralRuleNames()
	sort.Strings(names)
	for _, name := range names {
		if _, tested := cases[name]; !tested {
			t.Errorf("the rule %q has no test cases", name)
		}
	}
	if len(names) != len(cases) {
		t.Errorf("the package ships %d rules and the table covers %d", len(names), len(cases))
	}

	for name, expectations := range cases {
		rule, known := PluralRuleNamed(name)
		if !known {
			t.Errorf("PluralRuleNamed(%q) reported unknown", name)
			continue
		}
		for n, want := range expectations {
			if got := rule(whole(n)); got != want {
				t.Errorf("the %s rule puts %d in %q, want %q", name, n, got, want)
			}
		}
	}
}

// TestPluralRulesOverFractions covers every rule with counts that have digits
// after the point, taken from the samples CLDR lists for each category.
//
// It is the half of the table a rule over whole numbers could not get wrong,
// because it could not be asked. Several samples keep their trailing zeros,
// since that is where V and F part from W and T: 1.0 is a whole count to a
// rule stated over n and a fraction to one stated over v.
func TestPluralRulesOverFractions(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]PluralCategory{
		"other":              {"0.0": Other, "1.0": Other, "1.5": Other},
		"one_other":          {"1.0": Other, "1.5": Other, "0.5": Other, "0.0": Other},
		"one_other_zero_one": {"0.0": One, "0.5": One, "1.0": One, "1.5": One, "1.99": One, "2.0": Other, "2.5": Other},
		"hindi":              {"0.0": One, "0.5": One, "1.0": One, "1.00": One, "1.5": Other, "2.0": Other},
		"punjabi":            {"0.0": One, "1.0": One, "1.00": One, "0.5": Other, "1.5": Other, "2.0": Other},
		"slavic":             {"1.0": Other, "1.5": Other, "21.0": Other, "0.0": Other},
		"belarusian": {
			"1.0": One, "21.0": One, "2.0": Few, "22.0": Few,
			"0.0": Many, "5.0": Many, "11.0": Many,
			"0.1": Other, "1.5": Other, "10.1": Other,
		},
		"serbo_croatian": {
			"0.1": One, "1.1": One, "2.1": One, "10.1": One,
			"0.2": Few, "1.2": Few, "2.4": Few,
			"0.0": Other, "0.5": Other, "1.0": Other, "1.5": Other, "0.11": Other, "0.12": Other,
		},
		"west_slavic": {"0.0": Many, "1.0": Many, "1.5": Many, "2.0": Many},
		"polish":      {"0.0": Other, "1.0": Other, "1.5": Other, "2.0": Other},
		"lithuanian": {
			"1.0": One, "21.0": One, "2.0": Few, "9.0": Few,
			"0.1": Many, "1.5": Many, "10.1": Many,
			"0.0": Other, "10.0": Other, "11.0": Other,
		},
		// Three digits after the point are judged by the last alone, so 0.111
		// is one where 0.11 is zero.
		"latvian": {
			"0.0": Zero, "10.0": Zero, "11.0": Zero, "19.0": Zero, "0.11": Zero, "1.19": Zero,
			"0.1": One, "1.0": One, "1.1": One, "10.1": One, "0.01": One, "0.21": One, "0.001": One, "0.111": One,
			"0.2": Other, "1.5": Other, "0.22": Other, "0.112": Other,
		},
		"romanian":        {"0.0": Few, "1.0": Few, "1.5": Few, "20.0": Few},
		"arabic":          {"0.0": Zero, "1.0": One, "2.0": Two, "3.0": Few, "11.0": Many, "100.0": Other, "0.1": Other, "1.5": Other},
		"welsh":           {"0.0": Zero, "1.0": One, "2.0": Two, "3.0": Few, "6.0": Many, "0.5": Other, "1.5": Other},
		"irish":           {"1.0": One, "2.0": Two, "3.0": Few, "7.0": Many, "0.0": Other, "1.5": Other},
		"scottish_gaelic": {"1.0": One, "11.0": One, "2.0": Two, "12.0": Two, "13.0": Few, "0.0": Other, "1.5": Other},
		"maltese":         {"1.0": One, "0.0": Few, "2.0": Few, "11.0": Many, "0.5": Other, "1.5": Other},
		"slovenian":       {"0.0": Few, "1.0": Few, "1.5": Few, "5.0": Few},
		"icelandic": {
			"0.1": One, "1.0": One, "1.1": One, "10.1": One, "1.10": One, "0.21": One,
			"0.0": Other, "0.2": Other, "1.5": Other, "10.0": Other, "0.11": Other,
		},
		"macedonian": {
			"0.1": One, "1.1": One, "10.1": One, "0.21": One,
			"0.0": Other, "1.0": Other, "1.10": Other, "0.2": Other, "0.11": Other,
		},
		"hebrew":   {"0.0": One, "0.5": One, "0.05": One, "1.0": Other, "1.5": Other, "2.0": Other},
		"filipino": {"0.0": One, "0.1": One, "1.0": One, "1.5": One, "0.4": Other, "0.6": Other, "1.9": Other, "2.4": Other},
	}

	for _, name := range PluralRuleNames() {
		if _, tested := cases[name]; !tested {
			t.Errorf("the rule %q has no fractional test cases", name)
		}
	}
	for name, expectations := range cases {
		rule, known := PluralRuleNamed(name)
		if !known {
			t.Errorf("PluralRuleNamed(%q) reported unknown", name)
			continue
		}
		for text, want := range expectations {
			if got := rule(decimal(t, text)); got != want {
				t.Errorf("the %s rule puts %s in %q, want %q", name, text, got, want)
			}
		}
	}
}

// TestPluralOperandsOf checks the operands read off each kind of count a
// caller can give, which are what every rule above is handed.
func TestPluralOperandsOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		count any
		want  PluralOperands
	}{
		{count: 0, want: PluralOperands{}},
		{count: 12, want: PluralOperands{N: 12, I: 12}},
		{count: -3, want: PluralOperands{N: 3, I: 3}},
		{count: int8(-8), want: PluralOperands{N: 8, I: 8}},
		{count: int16(16), want: PluralOperands{N: 16, I: 16}},
		{count: int32(32), want: PluralOperands{N: 32, I: 32}},
		{count: int64(math.MaxInt64), want: PluralOperands{N: math.MaxInt64, I: math.MaxInt64}},
		{count: uint(7), want: PluralOperands{N: 7, I: 7}},
		{count: uint8(8), want: PluralOperands{N: 8, I: 8}},
		{count: uint16(16), want: PluralOperands{N: 16, I: 16}},
		{count: uint32(32), want: PluralOperands{N: 32, I: 32}},
		{count: uint64(64), want: PluralOperands{N: 64, I: 64}},
		{count: 2.0, want: PluralOperands{N: 2, I: 2}},
		{count: 1.5, want: PluralOperands{N: 1.5, I: 1, V: 1, W: 1, F: 5, T: 5}},
		{count: -2.25, want: PluralOperands{N: 2.25, I: 2, V: 2, W: 2, F: 25, T: 25}},
		// The digits a float is printed with, not the ones its binary value
		// would take: 0.1 is one digit after the point, and so is the float32
		// nearest 1.1.
		{count: 0.1, want: PluralOperands{N: 0.1, I: 0, V: 1, W: 1, F: 1, T: 1}},
		{count: float32(1.1), want: PluralOperands{N: float64(float32(1.1)), I: 1, V: 1, W: 1, F: 1, T: 1}},
		{count: 1e-7, want: PluralOperands{N: 1e-7, I: 0, V: 7, W: 7, F: 1, T: 1}},
		{count: 9e18, want: PluralOperands{N: 9e18, I: 9e18}},
	}
	for _, tc := range cases {
		got, ok := PluralOperandsOf(tc.count)
		if !ok || got != tc.want {
			t.Errorf("PluralOperandsOf(%T %v) = %+v, %v, want %+v", tc.count, tc.count, got, ok, tc.want)
		}
	}

	// None of these is a number of anything a sentence counts.
	for _, count := range []any{
		"3", nil, true, math.NaN(), math.Inf(1), math.Inf(-1), float32(math.Inf(1)),
		1e19, -1e19, math.Ldexp(1, 63), int64(math.MinInt64),
		uint64(math.MaxInt64) + 1, uint(math.MaxUint),
	} {
		if got, ok := PluralOperandsOf(count); ok {
			t.Errorf("PluralOperandsOf(%T %v) = %+v, want it refused", count, count, got)
		}
	}
}

// TestPluralOperandsAllocateNothing holds the path every counted message takes
// to what it cost before a rule read operands rather than an int: reading a
// count and asking a rule about it allocates nothing, for a fraction as well
// as for a whole count.
func TestPluralOperandsAllocateNothing(t *testing.T) {
	rule := PluralRuleFor("ru")
	counts := []any{12, int64(1_000_000), 1.5}
	allocs := testing.AllocsPerRun(100, func() {
		for _, count := range counts {
			operands, _ := PluralOperandsOf(count)
			_ = rule(operands)
		}
	})
	if allocs != 0 {
		t.Errorf("reading a count and choosing its form allocated %.0f times, want none", allocs)
	}
}

func TestPluralRuleNamedRefusesUnknown(t *testing.T) {
	t.Parallel()
	if _, known := PluralRuleNamed("klingon"); known {
		t.Error("PluralRuleNamed reported a rule this package does not ship")
	}
}

// TestPluralRuleFor checks how a locale finds its rule, which is the part an
// application actually depends on: a language it never configured still has to
// count correctly.
func TestPluralRuleFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		locale string
		count  int
		want   PluralCategory
	}{
		{locale: "en", count: 1, want: One},
		{locale: "en", count: 2, want: Other},
		{locale: "de", count: 2, want: Other},
		{locale: "ja", count: 1, want: Other},
		{locale: "fr", count: 0, want: One},
		{locale: "ru", count: 3, want: Few},
		{locale: "ar", count: 2, want: Two},
		{locale: "pl", count: 5, want: Many},
		{locale: "cy", count: 6, want: Many},
		{locale: "be", count: 21, want: One},
		{locale: "mk", count: 21, want: One},
		{locale: "hi", count: 0, want: One},
		{locale: "pa", count: 0, want: One},

		// Marathi counts as English does, one for exactly one, which is CLDR's
		// "n = 1"; it was once listed with the languages that put zero in one.
		{locale: "mr", count: 0, want: Other},
		{locale: "mr", count: 1, want: One},

		// A regional locale counts the way its language does, with nothing
		// configured for the region itself.
		{locale: "pt-BR", count: 0, want: One},
		{locale: "ru-RU", count: 3, want: Few},
		{locale: "en-GB", count: 1, want: One},

		// A language nobody has an entry for counts the way English does, which
		// is both the commonest rule and the safest guess.
		{locale: "tlh", count: 1, want: One},
		{locale: "tlh", count: 2, want: Other},
		{locale: "zz-ZZ", count: 1, want: One},
	}
	for _, tc := range cases {
		if got := PluralRuleFor(tc.locale)(whole(tc.count)); got != tc.want {
			t.Errorf("PluralRuleFor(%q)(%d) = %q, want %q", tc.locale, tc.count, got, tc.want)
		}
	}
}

// TestRegionalPluralRuleThatDiffersFromItsLanguage is the regression test for
// European Portuguese counting the way Brazilian Portuguese does. CLDR gives
// pt-PT a rule of its own, one for exactly one and other for everything else,
// where pt puts zero in one as well; the region fell back to its language and
// so wrote "0 ficheiro" where it means "0 ficheiros".
func TestRegionalPluralRuleThatDiffersFromItsLanguage(t *testing.T) {
	t.Parallel()
	cases := map[int]PluralCategory{0: Other, 1: One, 2: Other, -1: One}
	for count, want := range cases {
		if got := PluralRuleFor("pt-PT")(whole(count)); got != want {
			t.Errorf("PluralRuleFor(pt-PT)(%d) = %q, want %q", count, got, want)
		}
	}
	// Brazil, and the language on its own, keep the rule CLDR gives pt.
	for _, locale := range []string{"pt", "pt-BR"} {
		if got := PluralRuleFor(locale)(whole(0)); got != One {
			t.Errorf("PluralRuleFor(%s)(0) = %q, want one", locale, got)
		}
	}

	store := storeFor(t, map[string]string{
		"pt-PT.yml": "pt-PT:\n  files:\n    one: \"%{count} ficheiro\"\n    other: \"%{count} ficheiros\"\n",
		"en.yml":    "en:\n  a: a\n",
	})
	if got := store.T("pt-PT", "files", "count", 0); got != "0 ficheiros" {
		t.Errorf("T(pt-PT, files, count=0) = %q, want 0 ficheiros", got)
	}
}

// TestFractionalCountChoosesByCLDROperands is the regression test for a count
// with a fraction being cut down to a whole number before its plural form was
// chosen, so 1.5 kilometres read "1.5 kilometre". CLDR chooses by the digits as
// written: English puts 1.5 in other, French puts it in one, Czech has a form
// of its own for any fraction, and Russian puts every fraction in other.
func TestFractionalCountChoosesByCLDROperands(t *testing.T) {
	t.Parallel()
	forms := "    one: \"%{count} one\"\n    few: \"%{count} few\"\n    many: \"%{count} many\"\n    other: \"%{count} other\"\n"
	files := map[string]string{}
	for _, locale := range []string{"en", "fr", "cs", "ru", "lv", "is", "he"} {
		files[locale+".yml"] = locale + ":\n  km:\n" + forms
	}
	store := storeFor(t, files)

	cases := []struct {
		locale string
		count  any
		want   string
	}{
		{"en", 1.5, "1.5 other"},
		{"en", 0.5, "0.5 other"},
		{"en", float32(2.5), "2.5 other"},
		// A float with no fraction is the whole number it holds, and prints as
		// one: 1.0 is 1, the singular.
		{"en", 1.0, "1 one"},
		{"en", -1.0, "-1 one"},
		{"fr", 1.5, "1.5 one"},
		{"fr", 2.5, "2.5 other"},
		{"cs", 1.5, "1.5 many"},
		{"cs", 2.0, "2 few"},
		{"ru", 1.5, "1.5 other"},
		{"ru", 21.0, "21 one"},
		// Latvian and Icelandic read the digits after the point as well.
		{"lv", 0.1, "0.1 one"},
		{"is", 0.1, "0.1 one"},
		{"is", 0.2, "0.2 other"},
		// Hebrew puts any fraction of less than one in one.
		{"he", 0.5, "0.5 one"},
	}
	for _, tc := range cases {
		if got := store.T(tc.locale, "km", "count", tc.count); got != tc.want {
			t.Errorf("T(%s, km, count=%v) = %q, want %q", tc.locale, tc.count, got, tc.want)
		}
	}
}

// TestZeroFormIsForNothingAtAll checks the one departure from CLDR against a
// count with a fraction: a zero form is used for none, and 0.0 is none, but
// half of something is not.
func TestZeroFormIsForNothingAtAll(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  km:\n    zero: none\n    one: \"%{count} kilometre\"\n    other: \"%{count} kilometres\"\n",
	})
	for count, want := range map[any]string{0: "none", 0.0: "none", 0.5: "0.5 kilometres", 1: "1 kilometre"} {
		if got := store.T("en", "km", "count", count); got != want {
			t.Errorf("T(en, km, count=%v) = %q, want %q", count, got, want)
		}
	}
}

// TestCountThatIsNotANumberIsRefused covers the counts a plural form cannot be
// chosen by, which used to be narrowed to an int whatever that made of them:
// NaN became whatever the platform's conversion gave.
func TestCountThatIsNotANumberIsRefused(t *testing.T) {
	t.Parallel()
	var reported error
	store := storeFor(t, map[string]string{"en.yml": "en:\n  km:\n    one: one\n    other: other\n"},
		func(o *StoreOptions) {
			o.ExceptionHandler = func(err error, _, _ string) (string, error) {
				reported = err
				return "refused", err
			}
		})

	if got := store.T("en", "km", "count", math.NaN()); got != "refused" {
		t.Errorf("T with a NaN count = %q, want the exception handler's text", got)
	}
	var bad *ArgumentError
	if !errors.As(reported, &bad) || !errors.Is(reported, ErrMalformedArguments) {
		t.Fatalf("a NaN count reported %v, want an ArgumentError", reported)
	}
	for _, mention := range []string{`"km"`, "NaN", "count"} {
		if !strings.Contains(bad.Error(), mention) {
			t.Errorf("the error does not mention %s: %v", mention, bad)
		}
	}

	// A Count set on a Lookup is held to the same bound: the one int whose
	// magnitude has no int64 is refused rather than counted as negative.
	minimum := math.MinInt
	if _, err := store.Get("en", Lookup{Key: "km", Count: &minimum}); !errors.As(err, &bad) {
		t.Errorf("Get with a Count of MinInt returned %v, want an ArgumentError", err)
	}
}

// TestPluralErrorQuotesTheCountAsGiven checks that a count that cannot choose
// a form is reported as the caller wrote it, fraction and all, rather than as
// the whole number it used to be cut down to.
func TestPluralErrorQuotesTheCountAsGiven(t *testing.T) {
	t.Parallel()
	var reported error
	store := storeFor(t, map[string]string{"en.yml": "en:\n  km:\n    one: one\n    two: two\n"},
		func(o *StoreOptions) {
			o.ExceptionHandler = func(err error, _, _ string) (string, error) {
				reported = err
				return "", err
			}
		})

	store.T("en", "km", "count", 1.5)
	var bad *InvalidPluralizationDataError
	if !errors.As(reported, &bad) {
		t.Fatalf("a count with no form to choose reported %v, want an InvalidPluralizationDataError", reported)
	}
	if bad.Count != 1.5 || bad.Category != Other || !strings.Contains(bad.Error(), "count of 1.5") {
		t.Errorf("the error quotes the count as %v in %q, want 1.5 and the other form", bad.Count, bad.Error())
	}

	// A Count set on a Lookup is quoted as the int it is.
	if _, err := store.Get("en", Lookup{Key: "km", Count: intPtr(5)}); !errors.As(err, &bad) || bad.Count != 5 {
		t.Errorf("Get with a Count of 5 returned %v, want an error quoting 5", err)
	}
}

// TestSuppliedPluralRule proves a language this package does not know can still
// be counted correctly, which is the escape hatch for the rule that a YAML file
// cannot hold a function.
func TestSuppliedPluralRule(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"xx.yml": "xx:\n  things:\n    one: singular\n    few: paucal\n    many: fraction\n    other: plural\n",
		"en.yml": "en:\n  things:\n    one: one thing\n    other: things\n",
	}, func(o *StoreOptions) {
		o.PluralRules = map[string]PluralRule{
			"xx": func(n PluralOperands) PluralCategory {
				switch {
				case n.V != 0:
					return Many
				case n.I == 1:
					return One
				case n.I < 10:
					return Few
				default:
					return Other
				}
			},
		}
	})

	// A supplied rule is handed the operands of a fraction as well, so it can
	// say what its language does with one.
	cases := map[any]string{1: "singular", 5: "paucal", 50: "plural", 1.5: "fraction"}
	for count, want := range cases {
		if got := store.T("xx", "things", "count", count); got != want {
			t.Errorf("T(xx, things, count=%v) = %q, want %q", count, got, want)
		}
	}
}

// TestPluralRuleFallsBackToOther checks the case a locale file gets wrong: a
// rule choosing a form the translator did not write.
func TestPluralRuleFallsBackToOther(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"ru.yml": "ru:\n  i18n:\n    plural:\n      rule: slavic\n  things:\n    one: one\n    other: other\n",
		"en.yml": "en:\n  a: a\n",
	}, func(o *StoreOptions) { o.ExceptionHandler = StrictExceptionHandler })

	// Three is "few" in Russian, which this file does not define, so the lookup
	// falls back to "other" rather than failing.
	if got := store.T("ru", "things", "count", 3); got != "other" {
		t.Errorf("a missing few form gave %q, want the other form", got)
	}
}

// TestPluralRuleWithNoUsableForm is the case that cannot fall back, because the
// entry has no other form either.
func TestPluralRuleWithNoUsableForm(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  things:\n    one: one\n    two: two\n",
	}, func(o *StoreOptions) { o.ExceptionHandler = StrictExceptionHandler })

	_, err := store.Get("en", Lookup{Key: "things", Count: intPtr(5)})
	var bad *InvalidPluralizationDataError
	if !errors.As(err, &bad) {
		t.Fatalf("Get returned %v, want an InvalidPluralizationDataError", err)
	}
	if len(bad.Have) != 2 || bad.Have[0] != One || bad.Have[1] != Two {
		t.Errorf("the error lists %v as present, want one and two in CLDR order", bad.Have)
	}
}

func intPtr(n int) *int { return &n }

// whole is the operands of a whole count, for a table written in ints.
func whole(n int) PluralOperands {
	operands, _ := PluralOperandsOf(n)
	return operands
}

// decimal reads the operands off a count written out in decimal, trailing
// zeros and all, which is how CLDR writes the samples its rules are checked
// against and which a float cannot carry.
func decimal(t *testing.T, text string) PluralOperands {
	t.Helper()
	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		t.Fatalf("%q is not a decimal: %v", text, err)
	}
	integer, digits, _ := strings.Cut(text, ".")
	trimmed := strings.TrimRight(digits, "0")
	read := func(s string) int64 {
		if s == "" {
			return 0
		}
		value, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("%q in %q is not a run of digits: %v", s, text, err)
		}
		return value
	}
	return PluralOperands{
		N: n, I: read(integer),
		V: len(digits), W: len(trimmed),
		F: read(digits), T: read(trimmed),
	}
}
