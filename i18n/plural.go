package i18n

import (
	"bytes"
	"math"
	"strconv"
	"strings"
)

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

// PluralOperands are the numbers CLDR states its plural rules over, read off a
// count as it is written in decimal.
//
// A rule needs more than the count's whole part because languages disagree
// about fractions: English puts 1.5 in other, French puts it in one, Czech has
// a form for any fraction at all, and Latvian looks at the digits after the
// point. A whole count is all I, and the fraction operands are zero; 1.5 is I
// 1, V 1, F 5, and 0.25 is I 0, V 2, F 25.
//
// Every operand is read off the absolute value, as CLDR states them, so minus
// one counts the way one does. V and F count trailing zeros where W and T do
// not, which matters only for a count written with them, as "1.50" is: a float
// is read in the shortest decimal that names it, which has none, so the two
// pairs agree for every count [Store.Translate] is given.
type PluralOperands struct {
	// N is the absolute value of the count. Above 2^53 it is the nearest
	// float64, so a rule that compares a whole count with a number reads I.
	N float64
	// I is the integer digits of N.
	I int64
	// V is how many digits follow the point, trailing zeros included.
	V int
	// W is how many digits follow the point, trailing zeros left out.
	W int
	// F is the digits that follow the point, trailing zeros included, read as
	// a whole number.
	F int64
	// T is the digits that follow the point, trailing zeros left out, read as
	// a whole number.
	T int64
}

// whole reports whether the count is a whole number, which is what every
// condition CLDR writes on n assumes: "n = 1" holds for 1 and for 1.0, and a
// range such as "n = 3..10" holds only for the whole numbers in it, so neither
// holds for 1.5.
func (n PluralOperands) whole() bool { return n.T == 0 }

// maxCount is the first magnitude a count may not have: 2^63, the first whole
// number an int64 cannot hold, and not a number of anything a sentence counts.
const maxCount = 1 << 63

// PluralOperandsOf reads the operands off a count, and reports whether the
// value is one a count can be.
//
// It accepts what [Store.Translate] accepts as a count: any integer or
// floating-point type. It refuses anything else, NaN and the infinities, and a
// magnitude of 2^63 or more. A float is read in the shortest decimal that
// names it, which is the number a translation prints for it, so 0.1 is one
// digit after the point rather than the seventeen its binary value would take.
//
// A whole count takes no formatting and allocates nothing, so a rule can be
// asked about one on every request.
func PluralOperandsOf(count any) (PluralOperands, bool) {
	switch n := count.(type) {
	case int:
		return wholeOperands(int64(n))
	case int8:
		return wholeOperands(int64(n))
	case int16:
		return wholeOperands(int64(n))
	case int32:
		return wholeOperands(int64(n))
	case int64:
		return wholeOperands(n)
	case uint:
		return unsignedOperands(uint64(n))
	case uint8:
		return wholeOperands(int64(n))
	case uint16:
		return wholeOperands(int64(n))
	case uint32:
		return wholeOperands(int64(n))
	case uint64:
		return unsignedOperands(n)
	case float32:
		return floatOperands(float64(n), 32)
	case float64:
		return floatOperands(n, 64)
	default:
		return PluralOperands{}, false
	}
}

// wholeOperands are the operands of a whole count, refusing the one int64
// whose magnitude it cannot hold.
func wholeOperands(n int64) (PluralOperands, bool) {
	if n == math.MinInt64 {
		return PluralOperands{}, false
	}
	if n < 0 {
		n = -n
	}
	return PluralOperands{N: float64(n), I: n}, true
}

// unsignedOperands are the operands of an unsigned count, refusing one too
// large to be read as an int64 rather than narrowing it into a different
// number entirely.
func unsignedOperands(n uint64) (PluralOperands, bool) {
	if n > math.MaxInt64 {
		return PluralOperands{}, false
	}
	return wholeOperands(int64(n))
}

// floatOperands are the operands of a floating-point count of the given size.
func floatOperands(f float64, bits int) (PluralOperands, bool) {
	n := math.Abs(f)
	// Written this way round so that NaN, which compares false with
	// everything, is refused along with the infinities.
	if !(n < maxCount) {
		return PluralOperands{}, false
	}
	whole := math.Trunc(n)
	operands := PluralOperands{N: n, I: int64(whole)}
	if whole == n {
		return operands, true
	}

	// The digits after the point are those of the shortest decimal that reads
	// back as the same float. A fraction never formats without a point, and
	// shortest form has at most seventeen significant digits, so F fits an
	// int64 however many zeros lead it. Nor does shortest form ever end in a
	// zero after the point, since dropping it would be shorter, so W and T
	// are V and F. The buffer holds every count down to about 1e-40 without
	// reaching the heap.
	var buf [64]byte
	text := strconv.AppendFloat(buf[:0], n, 'f', -1, bits)
	digits := text[bytes.IndexByte(text, '.')+1:]
	operands.V, operands.F = len(digits), digitsValue(digits)
	operands.W, operands.T = operands.V, operands.F
	return operands, true
}

// digitsValue reads a run of decimal digits as a number.
func digitsValue(digits []byte) int64 {
	var value int64
	for _, d := range digits {
		value = value*10 + int64(d-'0')
	}
	return value
}

// PluralRule maps a count onto the category a language uses for it.
//
// A rule is a function rather than data because that is what a rule is: CLDR
// states them as arithmetic over [PluralOperands], and there are far fewer
// distinct calculations than there are languages. Supply one of your own
// through [StoreOptions.PluralRules] for a language this package does not
// know, or to override one it does. A rule that only counts whole things reads
// I and checks that V is zero; one that is asked about 1.5 is asked about
// I 1, V 1, F 5, and should say what CLDR says that language does with it.
type PluralRule func(n PluralOperands) PluralCategory

// EnglishPluralRule is the rule for English and the many languages that count
// the same way: one for a count of exactly one, other for everything else,
// including 1.5 and every other fraction. CLDR states it as "i = 1 and v = 0".
//
// It is also the rule a language with no entry of its own is given. A plural
// that is wrong in the right language still reads; a missing string does not.
func EnglishPluralRule(n PluralOperands) PluralCategory {
	if n.I == 1 && n.V == 0 {
		return One
	}
	return Other
}

// slavicWhole is the arithmetic Russian, Ukrainian and Belarusian share for a
// whole count, which puts every whole number in one, few or many.
func slavicWhole(i int64) PluralCategory {
	switch mod10, mod100 := i%10, i%100; {
	case mod10 == 1 && mod100 != 11:
		return One
	case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
		return Few
	default:
		return Many
	}
}

// pluralRules are the distinct calculations CLDR defines, by the name a locale
// file refers to them by.
//
// There are far fewer of these than there are languages, which is why they are
// arithmetic here rather than a data file: twenty-three functions cover every
// language in [pluralLanguages], and adding a language is one line rather than
// a download. Each states its CLDR rule, and conditions on n hold only for a
// whole count, which is what CLDR means by them.
var pluralRules = map[string]PluralRule{
	// No inflection for number at all.
	"other": func(PluralOperands) PluralCategory { return Other },

	// One for exactly one. English, German, Spanish and most of Europe.
	"one_other": EnglishPluralRule,

	// One for anything below two, fractions included. French and Portuguese.
	// one: i = 0,1
	"one_other_zero_one": func(n PluralOperands) PluralCategory {
		if n.I == 0 || n.I == 1 {
			return One
		}
		return Other
	},

	// One for anything up to one, so 0.5 is singular and 1.5 is not. Hindi,
	// Bengali, Persian. one: i = 0 or n = 1
	"hindi": func(n PluralOperands) PluralCategory {
		if n.I == 0 || n.I == 1 && n.whole() {
			return One
		}
		return Other
	},

	// One for none and for one, and for no fraction. Punjabi, Lingala, Tigrinya.
	// one: n = 0..1
	"punjabi": func(n PluralOperands) PluralCategory {
		if n.whole() && (n.I == 0 || n.I == 1) {
			return One
		}
		return Other
	},

	// Russian and Ukrainian, which put every fraction in other.
	// one: v = 0 and i % 10 = 1 and i % 100 != 11
	// few: v = 0 and i % 10 = 2..4 and i % 100 != 12..14
	// many: v = 0 and (i % 10 = 0 or i % 10 = 5..9 or i % 100 = 11..14)
	"slavic": func(n PluralOperands) PluralCategory {
		if n.V != 0 {
			return Other
		}
		return slavicWhole(n.I)
	},

	// Belarusian, which states the Russian arithmetic over n rather than i, so
	// that 1.0 is one where Russian makes it other.
	"belarusian": func(n PluralOperands) PluralCategory {
		if !n.whole() {
			return Other
		}
		return slavicWhole(n.I)
	},

	// Serbian, Croatian, Bosnian. The Slavic arithmetic, but the largest counts
	// fall into Other rather than into Many, and a fraction is judged by its
	// digits after the point.
	// one: v = 0 and i % 10 = 1 and i % 100 != 11 or f % 10 = 1 and f % 100 != 11
	// few: v = 0 and i % 10 = 2..4 and i % 100 != 12..14 or f % 10 = 2..4 and f % 100 != 12..14
	"serbo_croatian": func(n PluralOperands) PluralCategory {
		i10, i100 := n.I%10, n.I%100
		f10, f100 := n.F%10, n.F%100
		switch {
		case n.V == 0 && i10 == 1 && i100 != 11 || f10 == 1 && f100 != 11:
			return One
		case n.V == 0 && i10 >= 2 && i10 <= 4 && (i100 < 12 || i100 > 14) ||
			f10 >= 2 && f10 <= 4 && (f100 < 12 || f100 > 14):
			return Few
		default:
			return Other
		}
	},

	// Czech and Slovak, which keep Many for fractions alone.
	// one: i = 1 and v = 0; few: i = 2..4 and v = 0; many: v != 0
	"west_slavic": func(n PluralOperands) PluralCategory {
		switch {
		case n.V != 0:
			return Many
		case n.I == 1:
			return One
		case n.I >= 2 && n.I <= 4:
			return Few
		default:
			return Other
		}
	},

	// Polish, which puts every fraction in other.
	// one: i = 1 and v = 0
	// few: v = 0 and i % 10 = 2..4 and i % 100 != 12..14
	// many: every other whole count
	"polish": func(n PluralOperands) PluralCategory {
		if n.V != 0 {
			return Other
		}
		mod10, mod100 := n.I%10, n.I%100
		switch {
		case n.I == 1:
			return One
		case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
			return Few
		default:
			return Many
		}
	},

	// Lithuanian, which keeps Many for fractions alone.
	// one: n % 10 = 1 and n % 100 != 11..19
	// few: n % 10 = 2..9 and n % 100 != 11..19
	// many: f != 0
	"lithuanian": func(n PluralOperands) PluralCategory {
		if !n.whole() {
			return Many
		}
		mod10, mod100 := n.I%10, n.I%100
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

	// Latvian, which is the common language with a distinct zero, and which
	// reads the digits after the point as well as the whole part.
	// zero: n % 10 = 0 or n % 100 = 11..19 or v = 2 and f % 100 = 11..19
	// one: n % 10 = 1 and n % 100 != 11 or v = 2 and f % 10 = 1 and f % 100 != 11 or v != 2 and f % 10 = 1
	"latvian": func(n PluralOperands) PluralCategory {
		whole := n.whole()
		i10, i100 := n.I%10, n.I%100
		f10, f100 := n.F%10, n.F%100
		switch {
		case whole && (i10 == 0 || i100 >= 11 && i100 <= 19) || n.V == 2 && f100 >= 11 && f100 <= 19:
			return Zero
		case whole && i10 == 1 && i100 != 11 || n.V == 2 && f10 == 1 && f100 != 11 || n.V != 2 && f10 == 1:
			return One
		default:
			return Other
		}
	},

	// Romanian, which puts every fraction in few.
	// one: i = 1 and v = 0
	// few: v != 0 or n = 0 or n != 1 and n % 100 = 1..19
	"romanian": func(n PluralOperands) PluralCategory {
		mod100 := n.I % 100
		switch {
		case n.I == 1 && n.V == 0:
			return One
		case n.V != 0 || n.I == 0 || mod100 >= 1 && mod100 <= 19:
			return Few
		default:
			return Other
		}
	},

	// Arabic, the language that uses all six categories, every one of them for
	// whole counts only.
	"arabic": func(n PluralOperands) PluralCategory {
		if !n.whole() {
			return Other
		}
		mod100 := n.I % 100
		switch {
		case n.I == 0:
			return Zero
		case n.I == 1:
			return One
		case n.I == 2:
			return Two
		case mod100 >= 3 && mod100 <= 10:
			return Few
		case mod100 >= 11 && mod100 <= 99:
			return Many
		default:
			return Other
		}
	},

	// Welsh, whole counts only.
	"welsh": func(n PluralOperands) PluralCategory {
		if !n.whole() {
			return Other
		}
		switch n.I {
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

	// Irish, whole counts only.
	"irish": func(n PluralOperands) PluralCategory {
		if !n.whole() {
			return Other
		}
		switch i := n.I; {
		case i == 1:
			return One
		case i == 2:
			return Two
		case i >= 3 && i <= 6:
			return Few
		case i >= 7 && i <= 10:
			return Many
		default:
			return Other
		}
	},

	// Scottish Gaelic, whole counts only.
	"scottish_gaelic": func(n PluralOperands) PluralCategory {
		if !n.whole() {
			return Other
		}
		switch i := n.I; {
		case i == 1 || i == 11:
			return One
		case i == 2 || i == 12:
			return Two
		case (i >= 3 && i <= 10) || (i >= 13 && i <= 19):
			return Few
		default:
			return Other
		}
	},

	// Maltese, whole counts only.
	"maltese": func(n PluralOperands) PluralCategory {
		if !n.whole() {
			return Other
		}
		mod100 := n.I % 100
		switch {
		case n.I == 1:
			return One
		case n.I == 0 || (mod100 >= 2 && mod100 <= 10):
			return Few
		case mod100 >= 11 && mod100 <= 19:
			return Many
		default:
			return Other
		}
	},

	// Slovenian, which inflects on the last two digits and puts every fraction
	// in few.
	// one: v = 0 and i % 100 = 1; two: v = 0 and i % 100 = 2
	// few: v = 0 and i % 100 = 3..4 or v != 0
	"slovenian": func(n PluralOperands) PluralCategory {
		if n.V != 0 {
			return Few
		}
		switch n.I % 100 {
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

	// Icelandic, which counts by the last digit rather than by the whole, of
	// the fraction as well as of the integer.
	// one: t = 0 and i % 10 = 1 and i % 100 != 11 or t % 10 = 1 and t % 100 != 11
	"icelandic": func(n PluralOperands) PluralCategory {
		if n.T == 0 && n.I%10 == 1 && n.I%100 != 11 || n.T%10 == 1 && n.T%100 != 11 {
			return One
		}
		return Other
	},

	// Macedonian, the Icelandic arithmetic stated over v and f, so that 1.10
	// is other where Icelandic makes it one.
	// one: v = 0 and i % 10 = 1 and i % 100 != 11 or f % 10 = 1 and f % 100 != 11
	"macedonian": func(n PluralOperands) PluralCategory {
		if n.V == 0 && n.I%10 == 1 && n.I%100 != 11 || n.F%10 == 1 && n.F%100 != 11 {
			return One
		}
		return Other
	},

	// Hebrew, which puts a fraction of less than one in one.
	// one: i = 1 and v = 0 or i = 0 and v != 0; two: i = 2 and v = 0
	"hebrew": func(n PluralOperands) PluralCategory {
		switch {
		case n.I == 1 && n.V == 0 || n.I == 0 && n.V != 0:
			return One
		case n.I == 2 && n.V == 0:
			return Two
		default:
			return Other
		}
	},

	// Filipino and Tagalog, which judge a whole count by its last digit and a
	// fraction by the last digit after the point. The first clause CLDR writes
	// is implied by the second, since 1, 2 and 3 end in none of 4, 6 and 9.
	// one: v = 0 and i = 1,2,3 or v = 0 and i % 10 != 4,6,9 or v != 0 and f % 10 != 4,6,9
	"filipino": func(n PluralOperands) PluralCategory {
		last := n.F % 10
		if n.V == 0 {
			last = n.I % 10
		}
		switch last {
		case 4, 6, 9:
			return Other
		default:
			return One
		}
	},
}

// pluralLanguages maps a language onto the rule it counts by.
//
// Only languages that count differently from English are listed, with the
// regions that count differently from their language. Everything absent gets
// [EnglishPluralRule], which is both the commonest rule by far and the safest
// thing to guess.
var pluralLanguages = map[string]string{
	// No inflection for number.
	"bo": "other", "dz": "other", "id": "other", "ig": "other", "ii": "other",
	"ja": "other", "jv": "other", "kde": "other", "kea": "other", "km": "other",
	"ko": "other", "lkt": "other", "lo": "other", "ms": "other", "my": "other",
	"sah": "other", "ses": "other", "sg": "other", "th": "other", "to": "other",
	"vi": "other", "wo": "other", "yo": "other", "yue": "other", "zh": "other",

	// One for none as well as for one. The three rules agree on every whole
	// count and part over fractions.
	"ff": "one_other_zero_one", "fr": "one_other_zero_one", "hy": "one_other_zero_one",
	"kab": "one_other_zero_one", "pt": "one_other_zero_one",
	"am": "hindi", "as": "hindi", "bn": "hindi", "fa": "hindi",
	"gu": "hindi", "hi": "hindi", "kn": "hindi", "zu": "hindi",
	"ln": "punjabi", "mg": "punjabi", "nso": "punjabi", "pa": "punjabi",
	"ti": "punjabi", "wa": "punjabi",

	// The Slavic family and its neighbours.
	"ru": "slavic", "uk": "slavic", "be": "belarusian",
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
	"is": "icelandic", "mk": "macedonian",
	"mt": "maltese",
	"ro": "romanian", "mo": "romanian",
	"fil": "filipino", "tl": "filipino",

	// A region CLDR gives a rule different from its language's. European
	// Portuguese counts zero as plural, where Brazilian Portuguese, which is
	// what "pt" means to CLDR, counts it as singular. It is the only region in
	// CLDR's table with a rule of its own.
	"pt-PT": "one_other",
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
// does without an entry of its own. The exception is a region CLDR gives a
// rule of its own, which "pt-PT" is: European Portuguese puts zero in other,
// where "pt" puts it in one. A language this package does not know counts the
// way English does.
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
