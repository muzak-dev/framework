package i18n

import (
	"errors"
	"sort"
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

		"slavic": {
			1: One, 21: One, 31: One, 101: One,
			2: Few, 3: Few, 4: Few, 22: Few, 104: Few,
			0: Many, 5: Many, 9: Many, 11: Many, 12: Many, 14: Many, 25: Many, 111: Many,
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

		"icelandic": {1: One, 21: One, 31: One, 0: Other, 2: Other, 11: Other, 111: Other},

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
			if got := rule(n); got != want {
				t.Errorf("the %s rule puts %d in %q, want %q", name, n, got, want)
			}
		}
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
		if got := PluralRuleFor(tc.locale)(tc.count); got != tc.want {
			t.Errorf("PluralRuleFor(%q)(%d) = %q, want %q", tc.locale, tc.count, got, tc.want)
		}
	}
}

// TestSuppliedPluralRule proves a language this package does not know can still
// be counted correctly, which is the escape hatch for the rule that a YAML file
// cannot hold a function.
func TestSuppliedPluralRule(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"xx.yml": "xx:\n  things:\n    one: singular\n    few: paucal\n    other: plural\n",
		"en.yml": "en:\n  things:\n    one: one thing\n    other: things\n",
	}, func(o *StoreOptions) {
		o.PluralRules = map[string]PluralRule{
			"xx": func(n int) PluralCategory {
				switch {
				case n == 1:
					return One
				case n < 10:
					return Few
				default:
					return Other
				}
			},
		}
	})

	cases := map[int]string{1: "singular", 5: "paucal", 50: "plural"}
	for count, want := range cases {
		if got := store.T("xx", "things", "count", count); got != want {
			t.Errorf("T(xx, things, count=%d) = %q, want %q", count, got, want)
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
