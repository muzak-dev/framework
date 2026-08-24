package yaml

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestCorpus parses the locale files in testdata and checks what came out.
//
// These are written in the shape the published locale corpora use, with the
// constructs those files actually contain: a one-based month array beginning with a null, an
// anchor on the number format merged into the currency format below it, plural
// forms in six categories, and text in four scripts. This test is the only real
// evidence that the subset chosen here is the right one, which is why the scope
// of the parser is defined by these files rather than by judgement.
func TestCorpus(t *testing.T) {
	t.Parallel()
	names, err := filepath.Glob(filepath.Join("testdata", "*.yml"))
	if err != nil || len(names) == 0 {
		t.Fatalf("Glob found %d locale files (%v), want several", len(names), err)
	}
	for _, name := range names {
		t.Run(filepath.Base(name), func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			tree, err := ParseFile(name, data)
			if err != nil {
				t.Fatalf("ParseFile(%s) returned %v, want no error", name, err)
			}
			if len(tree) != 1 {
				t.Fatalf("%s holds %d top-level keys, want exactly the locale", name, len(tree))
			}
		})
	}
}

// dig walks a dotted path through a parsed tree, failing the test rather than
// panicking when a step is missing.
func dig(t *testing.T, tree map[string]any, path ...string) any {
	t.Helper()
	var node any = tree
	for i, step := range path {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%v is not a mapping at step %q", path[:i], step)
		}
		node, ok = m[step]
		if !ok {
			t.Fatalf("%v has no key %q", path[:i], step)
		}
	}
	return node
}

func parseTestdata(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	tree, err := ParseFile(name, data)
	if err != nil {
		t.Fatalf("ParseFile(%s) returned %v, want no error", name, err)
	}
	return tree
}

// TestCorpusGerman checks the constructs the German file leans on, above all
// the anchored number format that its currency format merges in.
func TestCorpusGerman(t *testing.T) {
	t.Parallel()
	tree := parseTestdata(t, "de.yml")

	if got := dig(t, tree, "de", "errors", "messages", "blank"); got != "muss ausgef\u00fcllt werden" {
		t.Errorf("de.errors.messages.blank = %q, want the German sentence", got)
	}
	if got := dig(t, tree, "de", "errors", "messages", "too_long", "other"); got != "ist zu lang (mehr als %{count} Zeichen)" {
		t.Errorf("de.errors.messages.too_long.other = %q, want the plural form with its placeholder", got)
	}

	// The merge key must bring the separator and delimiter down into the
	// currency format while the explicit precision below it wins.
	currency := dig(t, tree, "de", "number", "currency", "format")
	want := map[string]any{
		"delimiter": ".", "separator": ",", "significant": false,
		"strip_insignificant_zeros": false, "precision": int64(2),
		"format": "%n %u", "unit": "\u20ac",
	}
	if !reflect.DeepEqual(currency, want) {
		t.Errorf("de.number.currency.format =\n  %#v\nwant\n  %#v", currency, want)
	}

	// A merged mapping must be a copy: writing the currency precision must not
	// have reached back into the anchor every other format also merges.
	if got := dig(t, tree, "de", "number", "format", "precision"); got != int64(3) {
		t.Errorf("de.number.format.precision = %v, want 3 left untouched by the merge", got)
	}

	months := dig(t, tree, "de", "date", "abbr_month_names").([]any)
	if len(months) != 13 || months[0] != nil || months[3] != "M\u00e4r" {
		t.Errorf("de.date.abbr_month_names = %v, want a one-based array of thirteen", months)
	}
	if days := dig(t, tree, "de", "date", "day_names").([]any); len(days) != 7 || days[0] != "Sonntag" {
		t.Errorf("de.date.day_names = %v, want seven names starting on Sunday", days)
	}
}

// TestCorpusPlurals checks the locales whose plural forms go beyond one and
// other, since those categories are the reason a locale file has to be read at
// all rather than compiled in.
func TestCorpusPlurals(t *testing.T) {
	t.Parallel()

	ru := parseTestdata(t, "ru.yml")
	if got := dig(t, ru, "ru", "i18n", "plural", "rule"); got != "slavic" {
		t.Errorf("ru.i18n.plural.rule = %v, want the named rule", got)
	}
	short := dig(t, ru, "ru", "errors", "messages", "too_short").(map[string]any)
	for _, category := range []string{"one", "few", "many", "other"} {
		if _, ok := short[category]; !ok {
			t.Errorf("ru.errors.messages.too_short has no %q form", category)
		}
	}

	ar := parseTestdata(t, "ar.yml")
	arabic := dig(t, ar, "ar", "errors", "messages", "too_short").(map[string]any)
	for _, category := range []string{"zero", "one", "two", "few", "many", "other"} {
		if _, ok := arabic[category]; !ok {
			t.Errorf("ar.errors.messages.too_short has no %q form", category)
		}
	}

	ja := parseTestdata(t, "ja.yml")
	if got := dig(t, ja, "ja", "note"); got != "\u8907\u6570\u884c\u306e\n\u30c6\u30ad\u30b9\u30c8\n" {
		t.Errorf("ja.note = %q, want the literal block scalar with its breaks kept", got)
	}
	if got := dig(t, ja, "ja", "date", "formats", "long"); got != "%Y\u5e74%m\u6708%d\u65e5" {
		t.Errorf("ja.date.formats.long = %q, want the strftime pattern with its Japanese units", got)
	}
}
