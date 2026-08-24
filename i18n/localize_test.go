package i18n

import (
	"testing"
	"time"
)

func TestLocalizeDispatch(t *testing.T) {
	t.Parallel()
	store := Builtin()

	cases := []struct {
		name  string
		value any
		args  []any
		want  string
	}{
		{name: "a time", value: moment, want: "Sat, 07 Mar 2026 09:05:03 +0000"},
		{name: "a named format", value: moment, args: []any{"format", "short"}, want: "07 Mar 09:05"},
		{name: "a time as a date", value: moment, args: []any{"as", "date"}, want: "2026-03-07"},
		{name: "a date in its long form", value: moment,
			args: []any{"as", "date", "format", "long"}, want: "March 07, 2026"},
		{name: "a pointer to a time", value: &moment, want: "Sat, 07 Mar 2026 09:05:03 +0000"},
		{name: "a duration", value: 3 * time.Second, want: "less than 5 seconds"},
		{name: "a float", value: 1234.5, want: "1,234.5"},
		{name: "an integer", value: 1234567, want: "1,234,567"},
		{name: "something else entirely", value: "as it stands", want: "as it stands"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := store.L("en", tc.value, tc.args...); got != tc.want {
				t.Errorf("L(en, %v, %v) = %q, want %q", tc.value, tc.args, got, tc.want)
			}
		})
	}

	var absent *time.Time
	if got := store.L("en", absent); got != "" {
		t.Errorf("L of a nil time = %q, want the empty string", got)
	}
	if got := store.L("", moment, "locale", "en"); got != "Sat, 07 Mar 2026 09:05:03 +0000" {
		t.Errorf("L with the locale given as an option = %q", got)
	}
	if got := store.L("en", moment, "format"); got != "" {
		t.Errorf("L with a malformed argument list = %q, want the empty string", got)
	}
}

func TestNumberHelpers(t *testing.T) {
	t.Parallel()
	store := Builtin()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "delimiter", got: store.NumberWithDelimiter("en", 1234567.89), want: "1,234,567.89"},
		{name: "delimiter below a thousand", got: store.NumberWithDelimiter("en", 999), want: "999"},
		{name: "delimiter on a negative", got: store.NumberWithDelimiter("en", -1234567), want: "-1,234,567"},
		{name: "delimiter with a precision", got: store.NumberWithDelimiter("en", 1234.5678, "precision", 2), want: "1,234.57"},
		{name: "precision", got: store.NumberWithPrecision("en", 1.23456), want: "1.235"},
		{name: "precision named", got: store.NumberWithPrecision("en", 1.23456, "precision", 1), want: "1.2"},
		{name: "currency", got: store.NumberToCurrency("en", 12.5), want: "$12.50"},
		{name: "currency on a debit", got: store.NumberToCurrency("en", -12.5), want: "-$12.50"},
		{name: "currency with a unit", got: store.NumberToCurrency("en", 12.5, "unit", "GBP"), want: "GBP12.50"},
		{name: "currency with a precision", got: store.NumberToCurrency("en", 12.5, "precision", 0), want: "$13"},
		{name: "percentage", got: store.NumberToPercentage("en", 100), want: "100.000%"},
		{name: "percentage with a precision", got: store.NumberToPercentage("en", 12.345, "precision", 1), want: "12.3%"},
		{name: "one byte", got: store.NumberToHumanSize("en", 1), want: "1 Byte"},
		{name: "several bytes", got: store.NumberToHumanSize("en", 512), want: "512 Bytes"},
		{name: "kilobytes", got: store.NumberToHumanSize("en", 1536), want: "1.5 KB"},
		{name: "a round number of kilobytes", got: store.NumberToHumanSize("en", 1024), want: "1 KB"},
		{name: "megabytes", got: store.NumberToHumanSize("en", 3*1024*1024), want: "3 MB"},
		{name: "the largest unit", got: store.NumberToHumanSize("en", 5*1024*1024*1024*1024*1024), want: "5,120 TB"},
		{name: "a size with a precision", got: store.NumberToHumanSize("en", 1536, "precision", 3), want: "1.5 KB"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	for _, got := range []string{
		store.NumberWithDelimiter("en", 1, "precision"),
		store.NumberWithPrecision("en", 1, "precision"),
		store.NumberToCurrency("en", 1, "unit"),
		store.NumberToPercentage("en", 1, "precision"),
		store.NumberToHumanSize("en", 1, "precision"),
	} {
		if got != "" {
			t.Errorf("a helper given a malformed argument list returned %q, want the empty string", got)
		}
	}
}

// TestNumberHelpersFollowTheLocale is the point of reading these from a locale
// file rather than hard-coding them: a German reader expects the separator and
// the delimiter the other way round.
func TestNumberHelpersFollowTheLocale(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  a: a\n",
		"de.yml": `
de:
  number:
    format:
      separator: ","
      delimiter: "."
      precision: 3
    currency:
      format:
        format: "%n %u"
        unit: "EUR"
        precision: 2
`,
	})

	if got := store.NumberWithDelimiter("de", 1234567.89); got != "1.234.567,89" {
		t.Errorf("a German delimiter gave %q, want 1.234.567,89", got)
	}
	if got := store.NumberToCurrency("de", 12.5); got != "12,50 EUR" {
		t.Errorf("a German currency gave %q, want the symbol after the amount", got)
	}
	if got := store.NumberWithDelimiter("de", 1234, "delimiter", " ", "separator", "'"); got != "1 234" {
		t.Errorf("an overridden delimiter gave %q, want the one passed in", got)
	}
}

func TestToSentence(t *testing.T) {
	t.Parallel()
	store := Builtin()
	cases := []struct {
		items []string
		want  string
	}{
		{items: nil, want: ""},
		{items: []string{"one"}, want: "one"},
		{items: []string{"one", "two"}, want: "one and two"},
		{items: []string{"one", "two", "three"}, want: "one, two, and three"},
		{items: []string{"one", "two", "three", "four"}, want: "one, two, three, and four"},
	}
	for _, tc := range cases {
		if got := store.ToSentence("en", tc.items); got != tc.want {
			t.Errorf("ToSentence(%v) = %q, want %q", tc.items, got, tc.want)
		}
	}
}

func TestDistanceOfTimeInWords(t *testing.T) {
	t.Parallel()
	store := Builtin()
	cases := []struct {
		d    time.Duration
		want string
	}{
		{d: 3 * time.Second, want: "less than 5 seconds"},
		{d: -3 * time.Second, want: "less than 5 seconds"},
		{d: 7 * time.Second, want: "less than 10 seconds"},
		{d: 15 * time.Second, want: "less than 20 seconds"},
		{d: 30 * time.Second, want: "half a minute"},
		{d: 50 * time.Second, want: "less than a minute"},
		{d: 65 * time.Second, want: "1 minute"},
		{d: 20 * time.Minute, want: "20 minutes"},
		{d: 45 * time.Minute, want: "about 1 hour"},
		{d: 2 * time.Hour, want: "about 2 hours"},
		{d: 25 * time.Hour, want: "1 day"},
		{d: 72 * time.Hour, want: "3 days"},
		{d: 30 * 24 * time.Hour, want: "about 1 month"},
		{d: 120 * 24 * time.Hour, want: "4 months"},
		{d: 366 * 24 * time.Hour, want: "about 1 year"},
		{d: 500 * 24 * time.Hour, want: "over 1 year"},
		{d: 700 * 24 * time.Hour, want: "almost 2 years"},
	}
	for _, tc := range cases {
		if got := store.DistanceOfTimeInWords("en", tc.d); got != tc.want {
			t.Errorf("DistanceOfTimeInWords(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestPackageLevelLocalize covers the forms that answer from the process-wide
// store, which is what code outside a request has to use.
func TestPackageLevelLocalize(t *testing.T) {
	t.Parallel()
	if got := L("en", moment, "as", "date"); got != "2026-03-07" {
		t.Errorf("the package-level L = %q, want the date", got)
	}
	if got := Localize("en", 1234.5); got != "1,234.5" {
		t.Errorf("the package-level Localize = %q, want the grouped number", got)
	}
}

// TestLocalizeEdges covers the remaining shapes a value can arrive in and the
// settings a locale file can get wrong.
func TestLocalizeEdges(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  date:\n    day_names: not a list\n",
	})

	// A setting written as text where a list belongs falls back rather than
	// taking the framework down.
	if got := store.Strftime("en", "%A", moment); got != "Saturday" {
		t.Errorf("a day_names that is not a list gave %q, want the fallback", got)
	}

	built := Builtin()
	if got := built.L("en", float32(1234.5)); got != "1,234.5" {
		t.Errorf("L of a float32 = %q, want the grouped number", got)
	}
	if got := built.L("", 1234); got != "1,234" {
		t.Errorf("L with no locale = %q, want the default locale to answer", got)
	}

	if _, ok := toFloat("text"); ok {
		t.Error("toFloat accepted a string, want it refused")
	}
	if got, ok := toFloat(float32(1.5)); !ok || got != 1.5 {
		t.Errorf("toFloat(float32) = %v, %v, want 1.5, true", got, ok)
	}

	// Insignificant zeroes are dropped when the locale asks for it, which is
	// what a size of exactly one kilobyte relies on.
	if got := built.NumberWithDelimiter("en", 1.500, "precision", 3, "strip_insignificant_zeros", true); got != "1.5" {
		t.Errorf("a stripped number gave %q, want 1.5", got)
	}
}
