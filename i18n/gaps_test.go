package i18n

import (
	"errors"
	"io/fs"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// TestToInt covers each numeric type a count might arrive as, which is most
// often the length of something and so rarely a plain int.
func TestToInt(t *testing.T) {
	t.Parallel()
	values := []any{
		int(3), int8(3), int16(3), int32(3), int64(3),
		uint(3), uint8(3), uint16(3), uint32(3), uint64(3),
		float32(3), float64(3),
	}
	for _, value := range values {
		got, ok := toInt(value)
		if !ok || got != 3 {
			t.Errorf("toInt(%T) = %d, %v, want 3, true", value, got, ok)
		}
	}
	if _, ok := toInt("3"); ok {
		t.Error("toInt accepted a string, want it refused")
	}

	// A count too large to be a number of anything is refused rather than
	// narrowed, since narrowing it would produce a different number.
	for _, huge := range []any{uint64(math.MaxUint64), uint(math.MaxUint)} {
		if _, ok := toInt(huge); ok {
			t.Errorf("toInt(%v) was accepted, want it refused", huge)
		}
	}
}

func TestToScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value any
		want  []string
	}{
		{value: "a", want: []string{"a"}},
		{value: []string{"a", "b"}, want: []string{"a", "b"}},
		{value: []any{"a", "b"}, want: []string{"a", "b"}},
		{value: []any{"a", 42}, want: []string{"a"}},
		{value: 42, want: nil},
	}
	for _, tc := range cases {
		if got := toScope(tc.value); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("toScope(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestToDefaults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value any
		want  []any
	}{
		{value: "text", want: []any{"text"}},
		{value: []string{"a", "b"}, want: []any{"a", "b"}},
		{value: []any{Key("a"), "b"}, want: []any{Key("a"), "b"}},
		{value: 42, want: []any{42}},
	}
	for _, tc := range cases {
		if got := toDefaults(tc.value); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("toDefaults(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestSeparator covers a store keyed by something other than a dot, which is
// worth having only when a key itself has to contain one.
func TestSeparator(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  a:\n    b: nested\n",
	}, func(o *StoreOptions) { o.Separator = "/" })

	if got := store.T("en", "a/b"); got != "nested" {
		t.Errorf("T with a slash separator = %q, want the nested translation", got)
	}
	if got := store.T("en", "b", "scope", "a"); got != "nested" {
		t.Errorf("T with a scope and a slash separator = %q, want the nested translation", got)
	}
	if !store.Exists("en", "a/b") {
		t.Error("Exists with a slash separator reported false")
	}
}

// TestDefaultChainTerminates proves a default that names itself ends rather
// than recurses, which a locale file written by hand can easily ask for.
func TestDefaultChainTerminates(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: one\n"})
	if got := store.T("en", "missing", "default", Key("missing")); got != "translation missing: en.missing" {
		t.Errorf("a default naming itself gave %q, want the missing marker", got)
	}
}

func TestDefaultsOfOtherKinds(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: one\n"})

	// A default that is neither text nor a key is printed as it stands, which is
	// what a caller passing a number means by it.
	if got := store.T("en", "missing", "default", 42); got != "42" {
		t.Errorf("a numeric default gave %q, want 42", got)
	}
	// A nil in a chain is skipped rather than used.
	if got := store.T("en", "missing", "default", []any{nil, "fallback"}); got != "fallback" {
		t.Errorf("a chain beginning with nil gave %q, want the entry after it", got)
	}
	// A default is interpolated like a translation of its own.
	if got := store.T("en", "missing", "default", "at least %{count}", "count", 3); got != "at least 3" {
		t.Errorf("an interpolated default gave %q, want the value filled in", got)
	}
}

// TestDefaultReportsItsOwnFailure checks that a broken default is reported
// rather than quietly skipped, since a default is code and a mistake in it is a
// bug rather than a gap in the content.
func TestDefaultReportsItsOwnFailure(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: one\n"},
		func(o *StoreOptions) { o.ExceptionHandler = StrictExceptionHandler })

	_, err := store.Get("en", Lookup{Key: "missing", Default: []any{"needs %{value}"}})
	if !errors.Is(err, ErrMissingInterpolationArgument) {
		t.Errorf("a default naming a value nobody gave returned %v, want the failure reported", err)
	}
}

// TestGetReportsRenderFailures proves a failure that is not a missing
// translation stops the lookup rather than falling through to the next locale,
// which would otherwise hide a mistake behind a translation in another language.
func TestGetReportsRenderFailures(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"fr.yml": "fr:\n  a: \"needs %{value}\"\n",
		"en.yml": "en:\n  a: plain english\n",
	}, func(o *StoreOptions) { o.ExceptionHandler = StrictExceptionHandler })

	if _, err := store.Get("fr", Lookup{Key: "a"}); !errors.Is(err, ErrMissingInterpolationArgument) {
		t.Errorf("Get returned %v, want the French failure rather than the English fallback", err)
	}

	_, err := store.Get("fr", Lookup{Key: "missing", Default: []any{Key("a")}})
	if !errors.Is(err, ErrMissingInterpolationArgument) {
		t.Errorf("a key default that fails to render returned %v, want the failure", err)
	}
}

// TestCountAgainstSomethingUncountable covers the entry that is neither text
// nor a set of forms, which a count has nothing to do with.
func TestCountAgainstSomethingUncountable(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  days: [Monday, Tuesday]\n"},
		func(o *StoreOptions) { o.ExceptionHandler = StrictExceptionHandler })

	_, err := store.Get("en", Lookup{Key: "days", Count: intPtr(2)})
	if !errors.Is(err, ErrInvalidPluralizationData) {
		t.Errorf("a count against a list returned %v, want ErrInvalidPluralizationData", err)
	}
}

// TestNamedRuleThatIsNotAName covers a locale file whose plural rule is written
// as something other than the name of one.
func TestNamedRuleThatIsNotAName(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  a: one\n",
		"xx.yml": "xx:\n  i18n:\n    plural:\n      rule: 5\n  things:\n    one: one\n    other: many\n",
		"yy.yml": "yy:\n  i18n:\n    plural:\n      rule: klingon\n  things:\n    one: one\n    other: many\n",
	})
	// Neither locale names a rule this package knows, so both count the way
	// English does rather than failing to count at all.
	for _, locale := range []string{"xx", "yy"} {
		if got := store.T(locale, "things", "count", 1); got != "one" {
			t.Errorf("T(%s, things, count=1) = %q, want the singular", locale, got)
		}
		if got := store.T(locale, "things", "count", 7); got != "many" {
			t.Errorf("T(%s, things, count=7) = %q, want the plural", locale, got)
		}
	}
}

// TestTheDefaultLocaleFillsIn covers the calls that name no locale at all,
// which is what code with nothing better to say should get.
func TestTheDefaultLocaleFillsIn(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: one\n  ns:\n    b: two\n"})

	if got := store.T("", "a"); got != "one" {
		t.Errorf("T with no locale = %q, want the default locale to answer", got)
	}
	if !store.Exists("", "a") {
		t.Error("Exists with no locale reported false")
	}
	if group, ok := store.Namespace("", "ns"); !ok || group["b"] != "two" {
		t.Errorf("Namespace with no locale = %v, %v, want the default locale to answer", group, ok)
	}
	if _, ok := store.Namespace("en", "nope"); ok {
		t.Error("Namespace answered for a key nothing holds")
	}
}

// TestFallbacksOfALanguageApplyToItsRegions checks the rule that saves a
// configuration from listing every region: a fallback declared for a language
// is walked by the regional locales beneath it too.
func TestFallbacksOfALanguageApplyToItsRegions(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  a: en\n",
		"nb.yml": "nb:\n  b: nb\n",
		"nn.yml": "nn:\n  c: nn\n",
	}, func(o *StoreOptions) { o.Fallbacks = map[string][]string{"nn": {"nb"}} })

	if got := store.T("nn-NO", "b"); got != "nb" {
		t.Errorf("T(nn-NO, b) = %q, want the fallback declared for nn", got)
	}
	chain := store.FallbacksFor("nn-NO")
	want := []string{"nn-NO", "nn", "nb", "en"}
	if !reflect.DeepEqual(chain, want) {
		t.Errorf("FallbacksFor(nn-NO) = %v, want %v", chain, want)
	}
}

// TestLoadFromAnEmptyConfiguration covers a store told to load from nowhere,
// which is what an application that stores translations in Go gets.
func TestLoadFromAnEmptyConfiguration(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatalf("New returned %v, want no error", err)
	}
	if err := store.Load(nil, ""); err != nil {
		t.Errorf("Load from nowhere returned %v, want no error", err)
	}
	if got := store.T("en", "anything"); got != "translation missing: en.anything" {
		t.Errorf("T against an empty store = %q, want the marker", got)
	}
}

// TestLoadRefusesAnUnreadableTree covers a filesystem that fails partway
// through the walk rather than at the start.
func TestLoadRefusesAnUnreadableTree(t *testing.T) {
	t.Parallel()
	if _, err := Load(fstest.MapFS{}, "nowhere"); err == nil {
		t.Fatal("Load of a directory the filesystem does not have returned no error, want one")
	}
}

// refuses is a backend that lists and reads but will not accept a write, which
// is what a translation service exposing a read replica looks like.
type refuses struct{ *Simple }

func (refuses) Store(string, map[string]any) error {
	return errors.New("this backend does not accept writes")
}

// TestStoreThatRefusesAWrite covers the failure being reported rather than
// swallowed, since a locale file that did not load is worse than one that did
// not exist.
func TestStoreThatRefusesAWrite(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{Backend: refuses{NewSimple()}})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	if err := store.LoadFile("en.yml", []byte("en:\n  a: one\n")); err == nil {
		t.Error("LoadFile into a backend that refuses writes returned no error, want one")
	}
	if err := store.StoreTranslations("en", map[string]any{"a": "one"}); err == nil {
		t.Error("StoreTranslations into a backend that refuses writes returned no error, want one")
	}
}

// unreadable lists a file that then cannot be opened, which is what a locale
// directory being rewritten underneath a starting process looks like.
type unreadable struct{ fs.FS }

func (u unreadable) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".yml") {
		return nil, errors.New("the file went away")
	}
	return u.FS.Open(name)
}

func TestLoadReportsAFileItCannotRead(t *testing.T) {
	t.Parallel()
	files := unreadable{fstest.MapFS{
		"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  a: one\n")},
	}}
	_, err := Load(files, "locales")
	if err == nil {
		t.Fatal("Load returned no error for a file it could not read, want one")
	}
	if !strings.Contains(err.Error(), "en.yml") {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestLoadIntoAnExistingStoreReportsFailure(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	broken := fstest.MapFS{"en.yml": &fstest.MapFile{Data: []byte("en:\n\tbad: tabbed\n")}}
	if err := store.Load(broken, ""); err == nil {
		t.Error("Load of a broken locale returned no error, want one")
	}
}

// TestRemainingOptions covers the two options nothing else in the suite passes,
// and the count that is not a number.
func TestRemainingOptions(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: one\n"})

	if got := store.T("en", "a", "deep", true, "raise", true); got != "one" {
		t.Errorf("T with deep and raise = %q, want the translation", got)
	}
	if got := store.T("en", "a", "count", "many"); got != "" {
		t.Errorf("T with a count that is not a number = %q, want the empty string", got)
	}

	_, err := store.Get("en", Lookup{Key: "a", Deep: true, Raise: true})
	if err != nil {
		t.Errorf("Get with the options set returned %v, want no error", err)
	}
}
