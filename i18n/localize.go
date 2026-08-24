package i18n

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Localize renders a value the way the locale writes it.
//
//	store.L("de", time.Now())
//	store.L("de", time.Now(), "format", "short")
//	store.L("de", 1234.5)
//
// A time is formatted with the pattern at time.formats, or at date.formats when
// the call passes "as", "date". Go has no Date type distinct from Time, so the
// distinction between them has to be asked for rather than inferred. A
// duration becomes the phrase at datetime.distance_in_words, and a number is
// grouped and separated the way number.format says.
//
// Arguments are alternating names and values, as everywhere else here:
//
//	format     names the pattern, defaulting to "default"
//	as         "date" to format a time as a date
//	precision  digits after the separator
//	unit       the currency symbol, for [Store.NumberToCurrency]
func (s *Store) Localize(locale string, value any, args ...any) string {
	options, err := pairs("localize", args)
	if err != nil {
		text, _ := s.handler(err, locale, "localize")
		return text
	}
	if override, given := options["locale"].(string); given {
		locale = override
	}
	if locale == "" {
		locale = s.def
	}

	switch v := value.(type) {
	case time.Time:
		return s.localizeTime(locale, v, options)
	case *time.Time:
		if v == nil {
			return ""
		}
		return s.localizeTime(locale, *v, options)
	case time.Duration:
		return s.DistanceOfTimeInWords(locale, v, args...)
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		n, _ := toFloat(v)
		return s.NumberWithDelimiter(locale, n, args...)
	default:
		return fmt.Sprint(value)
	}
}

// L is [Store.Localize].
func (s *Store) L(locale string, value any, args ...any) string {
	return s.Localize(locale, value, args...)
}

// localizeTime formats a time with the locale's named pattern.
func (s *Store) localizeTime(locale string, t time.Time, options map[string]any) string {
	name, named := options["format"].(string)
	if !named {
		name = "default"
	}
	scope := "time.formats."
	if as, _ := options["as"].(string); as == "date" {
		scope = "date.formats."
	}
	pattern := s.format(locale, scope+name, name)
	return s.Strftime(locale, pattern, t)
}

// value walks the locale chain for a raw entry, which is how the settings that
// are not sentences are read: a list of month names, or a delimiter.
func (s *Store) value(locale, key string) (any, bool) {
	for _, candidate := range s.chain(locale) {
		if found, ok := s.find(candidate, key); ok {
			return found.value, true
		}
	}
	return nil, false
}

// format reads a setting that is text, falling back to what the caller says the
// framework should do without one.
func (s *Store) format(locale, key, fallback string) string {
	if value, ok := s.value(locale, key); ok {
		if text, isText := value.(string); isText {
			return text
		}
	}
	return fallback
}

// stringList reads a setting that is a list of names.
func (s *Store) stringList(locale, key string) []string {
	value, ok := s.value(locale, key)
	if !ok {
		return nil
	}
	list, isList := value.([]any)
	if !isList {
		return nil
	}
	out := make([]string, len(list))
	for i, item := range list {
		if text, isText := item.(string); isText {
			out[i] = text
		}
	}
	return out
}

// numberFormat is the set of settings that say how a number is written.
type numberFormat struct {
	separator string
	delimiter string
	precision int
	strip     bool
}

// numberSettings reads one scope of number settings as a whole.
//
// The namespace is read as a unit rather than key by key, and that is the point
// of the function. Read key by key, a locale that defines only a currency
// symbol would take its separator from whatever locale the chain reached next:
// a German amount would end up with an English decimal point. A format is a
// coherent set of decisions, so it is taken from one locale or not at all.
func (s *Store) numberSettings(locale, scope string) map[string]any {
	value, ok := s.value(locale, scope)
	if !ok {
		return nil
	}
	node, isNamespace := value.(map[string]any)
	if !isNamespace {
		return nil
	}
	return node
}

// numberFormatFor reads the settings a number is written with.
//
// The general settings at number.format are read first, then the ones for the
// particular scope laid over them, then whatever the call named. Each layer
// comes from one locale, so a scope that says nothing about a separator inherits
// the one its own language uses rather than another language's.
func (s *Store) numberFormatFor(locale, scope string, options map[string]any) numberFormat {
	f := numberFormat{separator: ".", delimiter: ",", precision: 3}

	apply := func(node map[string]any) {
		if node == nil {
			return
		}
		if separator, ok := node["separator"].(string); ok {
			f.separator = separator
		}
		if delimiter, ok := node["delimiter"].(string); ok {
			f.delimiter = delimiter
		}
		if precision, ok := toInt(node["precision"]); ok {
			f.precision = precision
		}
		if strip, ok := node["strip_insignificant_zeros"].(bool); ok {
			f.strip = strip
		}
	}

	apply(s.numberSettings(locale, "number.format"))
	if scope != "" {
		apply(s.numberSettings(locale, scope))
	}

	if separator, given := options["separator"].(string); given {
		f.separator = separator
	}
	if delimiter, given := options["delimiter"].(string); given {
		f.delimiter = delimiter
	}
	if precision, given := toInt(options["precision"]); given {
		f.precision = precision
	}
	if strip, given := options["strip_insignificant_zeros"].(bool); given {
		f.strip = strip
	}
	return f
}

// setting reads a value out of a scope of number settings, falling back to what
// the caller says the framework should do without one.
func settingOf(node map[string]any, key, fallback string) string {
	if text, ok := node[key].(string); ok {
		return text
	}
	return fallback
}

// NumberWithDelimiter writes a number with its thousands grouped, leaving the
// digits after the separator as they are.
//
//	store.NumberWithDelimiter("en", 1234567.89)   // 1,234,567.89
//	store.NumberWithDelimiter("de", 1234567.89)   // 1.234.567,89
func (s *Store) NumberWithDelimiter(locale string, value float64, args ...any) string {
	options, err := pairs("number", args)
	if err != nil {
		return ""
	}
	f := s.numberFormatFor(locale, "", options)
	precision := -1
	if given, ok := toInt(options["precision"]); ok {
		precision = given
	}
	return group(value, precision, f, true)
}

// NumberWithPrecision writes a number rounded to a fixed number of digits.
func (s *Store) NumberWithPrecision(locale string, value float64, args ...any) string {
	options, err := pairs("number", args)
	if err != nil {
		return ""
	}
	f := s.numberFormatFor(locale, "", options)
	return group(value, f.precision, f, true)
}

// NumberToCurrency writes an amount of money the way the locale writes one,
// which is a question of where the symbol goes as much as of which symbol it is.
//
//	store.NumberToCurrency("en", 12.5)   // $12.50
//	store.NumberToCurrency("de", 12.5)   // 12,50 EUR, with a German locale file
func (s *Store) NumberToCurrency(locale string, value float64, args ...any) string {
	options, err := pairs("currency", args)
	if err != nil {
		return ""
	}
	scope := s.numberSettings(locale, "number.currency.format")
	f := s.numberFormatFor(locale, "number.currency.format", options)
	if _, given := toInt(options["precision"]); !given {
		if _, stated := toInt(scope["precision"]); !stated {
			// Money is written to two places unless a locale says otherwise,
			// rather than to the three a plain number takes.
			f.precision = 2
		}
	}
	unit := settingOf(scope, "unit", "$")
	if given, ok := options["unit"].(string); ok {
		unit = given
	}
	pattern := settingOf(scope, "format", "%u%n")

	// The sign goes outside the pattern, not on the number inside it: a debit
	// reads as "-$12.50" rather than as "$-12.50" in every locale that puts its
	// symbol first.
	out := applyNumberPattern(pattern, group(math.Abs(value), f.precision, f, true), unit)
	if value < 0 {
		out = "-" + out
	}
	return out
}

// NumberToPercentage writes a number as a percentage.
func (s *Store) NumberToPercentage(locale string, value float64, args ...any) string {
	options, err := pairs("percentage", args)
	if err != nil {
		return ""
	}
	scope := s.numberSettings(locale, "number.percentage.format")
	f := s.numberFormatFor(locale, "number.percentage.format", options)
	pattern := settingOf(scope, "format", "%n%")
	return applyNumberPattern(pattern, group(value, f.precision, f, true), "")
}

// applyNumberPattern fills the "%n" and "%u" of a number format, which is how a
// locale says whether its currency symbol leads or trails.
func applyNumberPattern(pattern, number, unit string) string {
	out := strings.ReplaceAll(pattern, "%n", number)
	return strings.ReplaceAll(out, "%u", unit)
}

// storageUnits are the keys the sizes are written under, smallest first.
var storageUnits = []string{"byte", "kb", "mb", "gb", "tb"}

// NumberToHumanSize writes a number of bytes at the largest unit it fits.
//
//	store.NumberToHumanSize("en", 1536)   // 1.5 KB
func (s *Store) NumberToHumanSize(locale string, bytes int64, args ...any) string {
	options, err := pairs("storage", args)
	if err != nil {
		return ""
	}
	units := s.numberSettings(locale, "number.human.storage_units")
	pattern := settingOf(units, "format", "%n %u")

	if bytes < 1024 {
		unit := s.T(locale, "number.human.storage_units.units.byte", "count", bytes)
		return applyNumberPattern(pattern, strconv.FormatInt(bytes, 10), unit)
	}

	size := float64(bytes)
	index := 0
	for size >= 1024 && index < len(storageUnits)-1 {
		size /= 1024
		index++
	}

	f := s.numberFormatFor(locale, "", options)
	precision := 1
	if given, ok := toInt(options["precision"]); ok {
		precision = given
	}
	f.strip = true
	unit := s.format(locale, "number.human.storage_units.units."+storageUnits[index], strings.ToUpper(storageUnits[index]))
	return applyNumberPattern(pattern, group(size, precision, f, true), unit)
}

// group writes a number with its thousands separated and its fraction marked
// the way the locale asks for.
func group(value float64, precision int, f numberFormat, delimit bool) string {
	if precision >= 0 {
		// Round half away from zero rather than to even. Go's formatter rounds
		// to even, which is right for statistics and wrong for money: an amount
		// of 12.5 at no decimal places is thirteen to everyone who is going to
		// read it.
		factor := math.Pow(10, float64(precision))
		value = math.Round(value*factor) / factor
	}
	text := strconv.FormatFloat(value, 'f', precision, 64)
	whole, fraction, hasFraction := strings.Cut(text, ".")

	sign := ""
	if strings.HasPrefix(whole, "-") {
		sign, whole = "-", whole[1:]
	}

	if delimit && f.delimiter != "" && len(whole) > 3 {
		var b strings.Builder
		lead := len(whole) % 3
		if lead > 0 {
			b.WriteString(whole[:lead])
		}
		for i := lead; i < len(whole); i += 3 {
			if b.Len() > 0 {
				b.WriteString(f.delimiter)
			}
			b.WriteString(whole[i : i+3])
		}
		whole = b.String()
	}

	if !hasFraction {
		return sign + whole
	}
	if f.strip {
		fraction = strings.TrimRight(fraction, "0")
		if fraction == "" {
			return sign + whole
		}
	}
	return sign + whole + f.separator + fraction
}

// ToSentence joins a list the way the locale joins one, which is a question of
// where the conjunction goes and whether a comma precedes it.
//
//	store.ToSentence("en", []string{"one", "two", "three"})   // one, two, and three
func (s *Store) ToSentence(locale string, items []string) string {
	words := s.format(locale, "support.array.words_connector", ", ")
	two := s.format(locale, "support.array.two_words_connector", " and ")
	last := s.format(locale, "support.array.last_word_connector", ", and ")

	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + two + items[1]
	default:
		return strings.Join(items[:len(items)-1], words) + last + items[len(items)-1]
	}
}

// distancePhrase turns an elapsed time into the key of a phrase, walking the
// thresholds in order.
//
// The phrases are deliberately vague, because that is what makes them readable:
// "about 2 hours" is what a person says, and the exact figure is what a
// timestamp is for.
func (s *Store) distancePhrase(minutes int, seconds int) (string, int) {
	switch {
	case minutes <= 1:
		switch {
		case seconds < 5:
			return "less_than_x_seconds", 5
		case seconds < 10:
			return "less_than_x_seconds", 10
		case seconds < 20:
			return "less_than_x_seconds", 20
		case seconds < 40:
			return "half_a_minute", 0
		case seconds < 60:
			return "less_than_x_minutes", 1
		default:
			return "x_minutes", 1
		}
	case minutes < 45:
		return "x_minutes", minutes
	case minutes < 90:
		return "about_x_hours", 1
	case minutes < 1440:
		return "about_x_hours", int(math.Round(float64(minutes) / 60))
	case minutes < 2520:
		return "x_days", 1
	case minutes < 43200:
		return "x_days", int(math.Round(float64(minutes) / 1440))
	case minutes < 86400:
		return "about_x_months", int(math.Round(float64(minutes) / 43200))
	case minutes < 525600:
		return "x_months", int(math.Round(float64(minutes) / 43200))
	default:
		years := minutes / 525600
		switch remainder := minutes % 525600; {
		case remainder < 131400:
			return "about_x_years", years
		case remainder < 394200:
			return "over_x_years", years
		default:
			return "almost_x_years", years + 1
		}
	}
}

// DistanceOfTimeInWords renders an elapsed time as the phrase a person would
// use for it.
func (s *Store) DistanceOfTimeInWords(locale string, d time.Duration, args ...any) string {
	if d < 0 {
		d = -d
	}
	seconds := int(d.Seconds())
	minutes := int(math.Round(d.Minutes()))

	key, count := s.distancePhrase(minutes, seconds)
	if key == "half_a_minute" {
		return s.T(locale, "datetime.distance_in_words.half_a_minute")
	}
	return s.T(locale, "datetime.distance_in_words."+key, "count", count)
}

// toFloat reads a number written as any of the types one might arrive as.
func toFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	default:
		if i, ok := toInt(value); ok {
			return float64(i), true
		}
		return 0, false
	}
}

// Localize renders a value using the process-wide store.
func Localize(locale string, value any, args ...any) string {
	return Default().Localize(locale, value, args...)
}

// L is [Localize].
func L(locale string, value any, args ...any) string {
	return Default().Localize(locale, value, args...)
}
