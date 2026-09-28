package i18n

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// moment is the instant every strftime case is formatted at. It is chosen so
// that each field is distinguishable from the others: a two-digit day, a
// single-digit hour before noon, and a fraction of a second.
var moment = time.Date(2026, time.March, 7, 9, 5, 3, 123456789, time.UTC)

// TestStrftimeDirectives covers one row per directive.
//
// The table is exhaustive because a formatter is a switch, and the way a switch
// goes wrong is a case nobody looked at rather than a case that misbehaves.
func TestStrftimeDirectives(t *testing.T) {
	t.Parallel()
	store := Builtin()

	cases := map[string]string{
		"%Y": "2026", "%C": "20", "%y": "26",
		"%m": "03", "%d": "07", "%e": " 7", "%j": "066",
		"%H": "09", "%k": " 9", "%I": "09", "%l": " 9",
		"%M": "05", "%S": "03", "%L": "123", "%N": "123456789",
		"%a": "Sat", "%A": "Saturday", "%b": "Mar", "%h": "Mar", "%B": "March",
		"%p": "AM", "%P": "am",
		"%u": "6", "%w": "6",
		"%z": "+0000", "%Z": "UTC",
		"%D": "03/07/26", "%F": "2026-03-07", "%T": "09:05:03", "%R": "09:05",
		"%r": "09:05:03 AM", "%v": " 7-Mar-2026",
		"%x": "2026-03-07", "%X": "09:05:03",
		"%%": "%", "%n": "\n", "%t": "\t",

		// The modifiers, which are what a locale file reaches for when the
		// default padding is not what its language writes.
		"%-d": "7", "%_d": " 7", "%0e": "07", "%^a": "SAT", "%^B": "MARCH",
		"%4d": "0007",

		// A directive this package does not know is written out rather than
		// dropped, so an unfamiliar pattern degrades into visible text.
		"%Q": "%Q",

		// A pattern with no directives at all, and one that ends in a percent.
		"plain text": "plain text",
		"ends in %":  "ends in %",

		// A whole pattern, of the kind a locale file actually holds.
		"%Y-%m-%d":    "2026-03-07",
		"%B %d, %Y":   "March 07, 2026",
		"%d %b %H:%M": "07 Mar 09:05",
	}

	for pattern, want := range cases {
		if got := store.Strftime("en", pattern, moment); got != want {
			t.Errorf("Strftime(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// TestStrftimeWeekNumbers covers the three week counts separately, since they
// differ only at the turn of a year and a single instant cannot show that.
func TestStrftimeWeekNumbers(t *testing.T) {
	t.Parallel()
	store := Builtin()
	cases := []struct {
		when                time.Time
		sunday, monday, iso string
	}{
		{when: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), sunday: "00", monday: "00", iso: "01"},
		{when: time.Date(2026, time.March, 7, 0, 0, 0, 0, time.UTC), sunday: "09", monday: "09", iso: "10"},
		{when: time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC), sunday: "52", monday: "52", iso: "53"},
	}
	for _, tc := range cases {
		if got := store.Strftime("en", "%U", tc.when); got != tc.sunday {
			t.Errorf("%%U on %s = %q, want %q", tc.when.Format("2006-01-02"), got, tc.sunday)
		}
		if got := store.Strftime("en", "%W", tc.when); got != tc.monday {
			t.Errorf("%%W on %s = %q, want %q", tc.when.Format("2006-01-02"), got, tc.monday)
		}
		if got := store.Strftime("en", "%V", tc.when); got != tc.iso {
			t.Errorf("%%V on %s = %q, want %q", tc.when.Format("2006-01-02"), got, tc.iso)
		}
	}
}

// TestStrftimeHour12 covers the clock face at the two instants it is not simply
// the hour: midnight and noon are both twelve.
func TestStrftimeHour12(t *testing.T) {
	t.Parallel()
	store := Builtin()
	cases := map[int]string{0: "12", 1: "01", 11: "11", 12: "12", 13: "01", 23: "11"}
	for hour, want := range cases {
		when := time.Date(2026, time.March, 7, hour, 0, 0, 0, time.UTC)
		if got := store.Strftime("en", "%I", when); got != want {
			t.Errorf("%%I at %02d:00 = %q, want %q", hour, got, want)
		}
	}
	if got := store.Strftime("en", "%p", time.Date(2026, 3, 7, 13, 0, 0, 0, time.UTC)); got != "PM" {
		t.Errorf("%%p in the afternoon = %q, want PM", got)
	}
}

// TestStrftimeTakesNamesFromTheLocale is the whole reason this formatter exists
// rather than a translation to a Go layout: a Go layout cannot say "Marz".
func TestStrftimeTakesNamesFromTheLocale(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  a: a\n",
		"xx.yml": `
xx:
  date:
    day_names: [d0, d1, d2, d3, d4, d5, d6]
    abbr_day_names: [a0, a1, a2, a3, a4, a5, a6]
    month_names: [~, m1, m2, m3, m4, m5, m6, m7, m8, m9, m10, m11, m12]
    abbr_month_names: [~, b1, b2, b3, b4, b5, b6, b7, b8, b9, b10, b11, b12]
    formats:
      default: "%d.%m.%Y"
  time:
    am: vorm
    pm: nachm
`,
	})

	cases := map[string]string{
		"%A": "d6", "%a": "a6", "%B": "m3", "%b": "b3", "%P": "vorm",
		"%x": "07.03.2026",
	}
	for pattern, want := range cases {
		if got := store.Strftime("xx", pattern, moment); got != want {
			t.Errorf("Strftime(xx, %q) = %q, want %q", pattern, got, want)
		}
	}

	// A locale with no names of its own falls back through the chain to English
	// rather than producing nothing.
	if got := store.Strftime("en", "%A %B", moment); got != "Saturday March" {
		t.Errorf("Strftime(en, %%A %%B) = %q, want the English names", got)
	}
}

// TestStrftimeWithoutLocaleData covers a store that has no date names at all,
// where the formatter has to fall back to what Go can produce.
//
// The built-in locale is left out deliberately: chained in, as it is by
// default, it would supply the names and this path would never run.
func TestStrftimeWithoutLocaleData(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{WithoutBuiltin: true})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	cases := map[string]string{
		"%A": "Saturday", "%a": "Sat", "%B": "March", "%b": "Mar",
		"%P": "am", "%c": "Sat Mar  7 09:05:03 2026", "%x": "2026-03-07",
	}
	for pattern, want := range cases {
		if got := store.Strftime("en", pattern, moment); got != want {
			t.Errorf("Strftime with no locale data (%q) = %q, want %q", pattern, got, want)
		}
	}
}

// TestStrftimeGoLayout covers the escape hatch for the formats that are meant
// to be read by a machine rather than by a person.
func TestStrftimeGoLayout(t *testing.T) {
	t.Parallel()
	store := Builtin()
	if got := store.Strftime("en", "go:2006-01-02T15:04:05Z07:00", moment); got != "2026-03-07T09:05:03Z" {
		t.Errorf("a go: pattern gave %q, want the Go layout applied", got)
	}
}

// TestStrftimeIncompletePattern covers a pattern that ends in the middle of a
// directive, which a hand-edited locale file can easily contain.
func TestStrftimeIncompletePattern(t *testing.T) {
	t.Parallel()
	store := Builtin()
	for _, pattern := range []string{"%", "%-", "%^", "%0"} {
		if got := store.Strftime("en", pattern, moment); got == "" {
			t.Errorf("Strftime(%q) produced nothing, want the text left as it stands", pattern)
		}
	}
}

// TestStrftimeRemainingDirectives covers the fields that depend on an instant
// rather than on a calendar, which the shared moment cannot show.
func TestStrftimeRemainingDirectives(t *testing.T) {
	t.Parallel()
	store := Builtin()

	if got := store.Strftime("en", "%s", moment); got != "1772874303" {
		t.Errorf("%%s = %q, want the seconds since the epoch", got)
	}

	// Sunday is the day the two weekday numberings disagree about: %w counts it
	// as nought and %u, which is the ISO numbering, counts it as seven.
	sunday := time.Date(2026, time.March, 8, 0, 0, 0, 0, time.UTC)
	if got := store.Strftime("en", "%u %w", sunday); got != "7 0" {
		t.Errorf("%%u %%w on a Sunday = %q, want 7 0", got)
	}

	// The ISO year differs from the calendar year in the days either side of a
	// new year, which is the only reason it is a separate field.
	cases := map[string]string{
		"2026-01-01": "2026",
		"2027-01-01": "2026",
		"2026-12-31": "2026",
	}
	for day, want := range cases {
		when, err := time.Parse("2006-01-02", day)
		if err != nil {
			t.Fatalf("parsing %s: %v", day, err)
		}
		if got := store.Strftime("en", "%G", when); got != want {
			t.Errorf("%%G on %s = %q, want %q", day, got, want)
		}
	}
}

// TestStrftimeSurvivesAPatternThatNamesItself is the regression test for a
// locale whose default time format is "%c", or whose date and time formats
// name each other: expansion recursed until the stack was exhausted, which Go
// reports as a fatal error that ends the process and that no recover can
// catch, on the first request to localize a time in that locale.
func TestStrftimeSurvivesAPatternThatNamesItself(t *testing.T) {
	t.Parallel()
	load := func(t *testing.T, doc string) *Store {
		t.Helper()
		store, err := New(StoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LoadFile("x.yml", []byte(doc)); err != nil {
			t.Fatal(err)
		}
		return store
	}
	for name, doc := range map[string]string{
		"a format that is %c":           "en:\n  time:\n    formats:\n      default: \"%c\"\n",
		"a date format that is %x":      "en:\n  date:\n    formats:\n      default: \"%x\"\n",
		"two formats naming each other": "en:\n  date:\n    formats:\n      default: \"%c\"\n  time:\n    formats:\n      default: \"%x\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := load(t, doc)
			// Reaching the assertions is the test: before the fix the process
			// died on the way here.
			for _, pattern := range []string{"%c", "%x", "%c and %x"} {
				if got := store.Strftime("en", pattern, moment); len(got) > 64 {
					t.Errorf("Strftime(%q) = %d bytes, want a short answer", pattern, len(got))
				}
			}
			_ = store.Localize("en", moment)
			_ = store.Localize("en", moment, "as", "date")
		})
	}
}

// TestStrftimeWorkIsBounded covers the recursion a depth limit alone does not
// stop: a pattern that names itself many times over expands to the many-th
// power of that, in time, without ever producing any output.
func TestStrftimeWorkIsBounded(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc := "en:\n  time:\n    formats:\n      default: \"" + strings.Repeat("%c", 1000) + "\"\n"
	if err := store.LoadFile("x.yml", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = store.Localize("en", moment)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pattern of a thousand percent-c did not finish: expansion work is not bounded")
	}
}

// TestStrftimeNestedDirectivesStillExpand pins what the bounds must not touch:
// the compound directives reaching the locale's own patterns, one level and
// several.
func TestStrftimeNestedDirectivesStillExpand(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc := "en:\n  date:\n    formats:\n      default: \"%d/%m/%Y\"\n  time:\n    formats:\n      default: \"%x %X\"\n"
	if err := store.LoadFile("x.yml", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	if got, want := store.Strftime("en", "%c", moment), "07/03/2026 09:05:03"; got != want {
		t.Errorf("%%c = %q, want %q", got, want)
	}
	if got, want := store.Localize("en", moment), "07/03/2026 09:05:03"; got != want {
		t.Errorf("Localize = %q, want %q", got, want)
	}
}

// TestStrftimeWidthIsCapped is the regression test for a width that was read
// as written and padded with a loop: "%500000000Y" is eleven bytes and made
// half a gigabyte.
func TestStrftimeWidthIsCapped(t *testing.T) {
	t.Parallel()
	store := Builtin()
	for _, pattern := range []string{
		"%500000000Y", "%99999999999999999999999Y", "%_1000000d", "%0999999999N", "%1000000e",
	} {
		got := store.Strftime("en", pattern, moment)
		if len(got) > maxStrftimeWidth+16 {
			t.Errorf("Strftime(%q) = %d bytes, want it padded to at most %d", pattern, len(got), maxStrftimeWidth)
		}
	}
	if got, want := store.Strftime("en", "%64Y", moment), strings.Repeat("0", 60)+"2026"; got != want {
		t.Errorf("a width at the cap = %q, want %q", got, want)
	}
	if got := store.Strftime("en", "%65Y", moment); len(got) != maxStrftimeWidth {
		t.Errorf("a width past the cap is %d bytes, want the cap of %d", len(got), maxStrftimeWidth)
	}
	if got, want := store.Strftime("en", "%4d", moment), "0007"; got != want {
		t.Errorf("an ordinary width = %q, want %q", got, want)
	}
}

// TestStrftimeOutputIsCapped covers the result growing past what any width
// allows: many directives, each writing text the locale supplies. The cut is at
// a character boundary, so a multi-byte name is not split.
func TestStrftimeOutputIsCapped(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("\u00e9", 500)
	doc := "en:\n  date:\n    month_names: [~, " + long + ", " + long + ", " + long + ", " + long + "]\n"
	if err := store.LoadFile("x.yml", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	pattern := strings.Repeat("%B", 4000)
	got := store.Strftime("en", pattern, time.Date(2026, time.March, 7, 0, 0, 0, 0, time.UTC))
	if len(got) > maxStrftimeOutput {
		t.Errorf("Strftime produced %d bytes, want at most %d", len(got), maxStrftimeOutput)
	}
	if !utf8.ValidString(got) {
		t.Error("the cut split a multi-byte character")
	}
	if len(got) < maxStrftimeOutput-4 {
		t.Errorf("Strftime produced %d bytes, want the output cut at the cap and not sooner", len(got))
	}
}
