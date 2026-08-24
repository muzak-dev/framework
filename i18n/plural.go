package i18n

import "strings"

// PluralCategory is one of the forms a language uses to count with.
//
// The names are CLDR's, and so are the ones a locale file writes its forms
// under. English uses two of them, Russian four and Arabic all six; a language
// that does not inflect for number uses only Other.
type PluralCategory string

// The plural categories, in the order CLDR lists them.
const (
	// Zero is used by languages with a distinct form for none, and by English
	// as an optional form for a count of zero.
	Zero PluralCategory = "zero"
	// One is the singular.
	One PluralCategory = "one"
	// Two is the dual, which Arabic, Welsh and Hebrew inflect for.
	Two PluralCategory = "two"
	// Few is the paucal.
	Few PluralCategory = "few"
	// Many is the form Slavic and Celtic languages use for larger counts.
	Many PluralCategory = "many"
	// Other is the form every language has, and the one a lookup falls back to.
	Other PluralCategory = "other"
)

// PluralRule maps a count onto the category a language uses for it.
//
// A rule is a function rather than data because that is what a rule is: CLDR
// states them as arithmetic, and there are far fewer distinct calculations than
// there are languages. Supply one of your own through
// [StoreOptions.PluralRules] for a language this package does not know, or to
// override one it does.
type PluralRule func(n int) PluralCategory

// EnglishPluralRule is the rule for English and the many languages that count
// the same way: one for a count of one, other for everything else.
//
// The count is judged by its absolute value, as every CLDR rule is, so minus
// one is a singular in every language that has one.
//
// It is also the rule a language with no entry of its own is given. A plural
// that is wrong in the right language still reads; a missing string does not.
func EnglishPluralRule(n int) PluralCategory {
	if abs(n) == 1 {
		return One
	}
	return Other
}

// abs is the count a rule reasons about. CLDR states its rules over the
// absolute value, so that minus one is a singular in every language that has
// one.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// pluralRules are the distinct calculations CLDR defines, by the name a locale
// file refers to them by.
//
// There are far fewer of these than there are languages, which is why they are
// arithmetic here rather than a data file: twenty functions cover the ninety
// languages in [pluralLanguages], and adding a language is one line rather than
// a download.
var pluralRules = map[string]PluralRule{
	// No inflection for number at all.
	"other": func(int) PluralCategory { return Other },

	// One for exactly one. English, German, Spanish and most of Europe.
	"one_other": EnglishPluralRule,

	// One for none as well as for one. French, Portuguese, Hindi.
	"one_other_zero_one": func(n int) PluralCategory {
		if abs(n) <= 1 {
			return One
		}
		return Other
	},

	// Russian, Ukrainian, Belarusian.
	"slavic": func(n int) PluralCategory {
		n = abs(n)
		switch mod10, mod100 := n%10, n%100; {
		case mod10 == 1 && mod100 != 11:
			return One
		case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
			return Few
		default:
			return Many
		}
	},

	// Serbian, Croatian, Bosnian. The same arithmetic as Slavic above, but the
	// largest counts fall into Other rather than into Many.
	"serbo_croatian": func(n int) PluralCategory {
		n = abs(n)
		switch mod10, mod100 := n%10, n%100; {
		case mod10 == 1 && mod100 != 11:
			return One
		case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
			return Few
		default:
			return Other
		}
	},

	// Czech and Slovak.
	"west_slavic": func(n int) PluralCategory {
		switch n = abs(n); {
		case n == 1:
			return One
		case n >= 2 && n <= 4:
			return Few
		default:
			return Other
		}
	},

	// Polish.
	"polish": func(n int) PluralCategory {
		n = abs(n)
		mod10, mod100 := n%10, n%100
		switch {
		case n == 1:
			return One
		case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
			return Few
		default:
			return Many
		}
	},

	// Lithuanian.
	"lithuanian": func(n int) PluralCategory {
		n = abs(n)
		mod10, mod100 := n%10, n%100
		teens := mod100 >= 11 && mod100 <= 19
		switch {
		case mod10 == 1 && !teens:
			return One
		case mod10 >= 2 && mod10 <= 9 && !teens:
			return Few
		default:
			return Other
		}
	},

	// Latvian, which is the common language with a distinct zero.
	"latvian": func(n int) PluralCategory {
		n = abs(n)
		mod10, mod100 := n%10, n%100
		switch {
		case mod10 == 0 || (mod100 >= 11 && mod100 <= 19):
			return Zero
		case mod10 == 1 && mod100 != 11:
			return One
		default:
			return Other
		}
	},

	// Romanian.
	"romanian": func(n int) PluralCategory {
		n = abs(n)
		mod100 := n % 100
		switch {
		case n == 1:
			return One
		case n == 0 || (mod100 >= 1 && mod100 <= 19):
			return Few
		default:
			return Other
		}
	},

	// Arabic, the language that uses all six categories.
	"arabic": func(n int) PluralCategory {
		n = abs(n)
		mod100 := n % 100
		switch {
		case n == 0:
			return Zero
		case n == 1:
			return One
		case n == 2:
			return Two
		case mod100 >= 3 && mod100 <= 10:
			return Few
		case mod100 >= 11 && mod100 <= 99:
			return Many
		default:
			return Other
		}
	},

	// Welsh.
	"welsh": func(n int) PluralCategory {
		switch abs(n) {
		case 0:
			return Zero
		case 1:
			return One
		case 2:
			return Two
		case 3:
			return Few
		case 6:
			return Many
		default:
			return Other
		}
	},

	// Irish.
	"irish": func(n int) PluralCategory {
		switch n = abs(n); {
		case n == 1:
			return One
		case n == 2:
			return Two
		case n >= 3 && n <= 6:
			return Few
		case n >= 7 && n <= 10:
			return Many
		default:
			return Other
		}
	},

	// Scottish Gaelic.
	"scottish_gaelic": func(n int) PluralCategory {
		switch n = abs(n); {
		case n == 1 || n == 11:
			return One
		case n == 2 || n == 12:
			return Two
		case (n >= 3 && n <= 10) || (n >= 13 && n <= 19):
			return Few
		default:
			return Other
		}
	},

	// Maltese.
	"maltese": func(n int) PluralCategory {
		n = abs(n)
		mod100 := n % 100
		switch {
		case n == 1:
			return One
		case n == 0 || (mod100 >= 2 && mod100 <= 10):
			return Few
		case mod100 >= 11 && mod100 <= 19:
			return Many
		default:
			return Other
		}
	},

	// Slovenian, which inflects on the last two digits.
	"slovenian": func(n int) PluralCategory {
		switch abs(n) % 100 {
		case 1:
			return One
		case 2:
			return Two
		case 3, 4:
			return Few
		default:
			return Other
		}
	},

	// Icelandic, which counts by the last digit rather than by the whole.
	"icelandic": func(n int) PluralCategory {
		n = abs(n)
		if n%10 == 1 && n%100 != 11 {
			return One
		}
		return Other
	},

	// Hebrew.
	"hebrew": func(n int) PluralCategory {
		switch abs(n) {
		case 1:
			return One
		case 2:
			return Two
		default:
			return Other
		}
	},

	// Filipino and Tagalog.
	"filipino": func(n int) PluralCategory {
		n = abs(n)
		switch mod10 := n % 10; {
		case n == 1 || n == 2 || n == 3:
			return One
		case mod10 == 4 || mod10 == 6 || mod10 == 9:
			return Other
		default:
			return One
		}
	},
}

// pluralLanguages maps a language onto the rule it counts by.
//
// Only languages that count differently from English are listed. Everything
// absent gets [EnglishPluralRule], which is both the commonest rule by far and
// the safest thing to guess.
var pluralLanguages = map[string]string{
	// No inflection for number.
	"bo": "other", "dz": "other", "id": "other", "ig": "other", "ii": "other",
	"ja": "other", "jv": "other", "kde": "other", "kea": "other", "km": "other",
	"ko": "other", "lkt": "other", "lo": "other", "ms": "other", "my": "other",
	"sah": "other", "ses": "other", "sg": "other", "th": "other", "to": "other",
	"vi": "other", "wo": "other", "yo": "other", "yue": "other", "zh": "other",

	// One for none as well as for one.
	"am": "one_other_zero_one", "as": "one_other_zero_one", "bn": "one_other_zero_one",
	"fa": "one_other_zero_one", "ff": "one_other_zero_one", "fr": "one_other_zero_one",
	"gu": "one_other_zero_one", "hi": "one_other_zero_one", "hy": "one_other_zero_one",
	"kab": "one_other_zero_one", "kn": "one_other_zero_one", "ln": "one_other_zero_one",
	"mg": "one_other_zero_one", "mr": "one_other_zero_one", "nso": "one_other_zero_one",
	"pa": "one_other_zero_one", "pt": "one_other_zero_one", "ti": "one_other_zero_one",
	"wa": "one_other_zero_one", "zu": "one_other_zero_one",

	// The Slavic family and its neighbours.
	"be": "slavic", "ru": "slavic", "uk": "slavic",
	"bs": "serbo_croatian", "hr": "serbo_croatian", "sh": "serbo_croatian", "sr": "serbo_croatian",
	"cs": "west_slavic", "sk": "west_slavic",
	"pl": "polish",
	"sl": "slovenian",

	// The Baltic languages.
	"lt": "lithuanian", "lv": "latvian",

	// The rest, each its own shape.
	"ar": "arabic", "ars": "arabic",
	"cy": "welsh",
	"ga": "irish",
	"gd": "scottish_gaelic",
	"he": "hebrew", "iw": "hebrew",
	"is": "icelandic", "mk": "icelandic",
	"mt": "maltese",
	"ro": "romanian", "mo": "romanian",
	"fil": "filipino", "tl": "filipino",
}

// PluralRuleNamed returns a rule by the name a locale file declares it under at
// "i18n.plural.rule", and reports whether that name is one this package knows.
//
// That entry is conventionally a function, which a YAML file in Go cannot hold.
// A locale file therefore names a rule rather than defining one, and a language
// whose arithmetic is genuinely its own supplies a function through
// [StoreOptions.PluralRules] instead.
func PluralRuleNamed(name string) (PluralRule, bool) {
	rule, known := pluralRules[name]
	return rule, known
}

// PluralRuleNames lists the rules this package knows by name, for an error
// message or a test that means to cover all of them.
func PluralRuleNames() []string {
	names := make([]string, 0, len(pluralRules))
	for name := range pluralRules {
		names = append(names, name)
	}
	return names
}

// PluralRuleFor returns the rule a locale counts by.
//
// A regional locale falls back to its language, so "pt-BR" counts the way "pt"
// does without an entry of its own. A language this package does not know
// counts the way English does.
func PluralRuleFor(locale string) PluralRule {
	if name, known := pluralLanguages[locale]; known {
		return pluralRules[name]
	}
	if language, _, regional := strings.Cut(locale, "-"); regional {
		if name, known := pluralLanguages[language]; known {
			return pluralRules[name]
		}
	}
	return EnglishPluralRule
}
