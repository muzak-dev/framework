package i18n

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// storeFor builds a store over locale files written inline, which is how every
// test here supplies translations: the shape of a file is part of what is being
// tested, so a test that declared Go maps would be testing something else.
func storeFor(t *testing.T, files map[string]string, opts ...func(*StoreOptions)) *Store {
	t.Helper()
	mapped := fstest.MapFS{}
	for name, body := range files {
		mapped["locales/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	options := StoreOptions{FS: mapped, Dir: "locales"}
	for _, apply := range opts {
		apply(&options)
	}
	store, err := New(options)
	if err != nil {
		t.Fatalf("New returned %v, want no error", err)
	}
	return store
}

func TestTranslate(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": `
en:
  hello: Hello world
  store:
    title: The Store
  product_price: "$%{price}"
  greeting: "Hello, %<name>s!"
  percent: "100%% sure"
`,
		"es.yml": `
es:
  hello: Hola mundo
  product_price: "%{price} EUR"
`,
	})

	cases := []struct {
		name   string
		locale string
		key    string
		args   []any
		want   string
	}{
		{name: "plain key", locale: "en", key: "hello", want: "Hello world"},
		{name: "nested key", locale: "en", key: "store.title", want: "The Store"},
		{name: "another locale", locale: "es", key: "hello", want: "Hola mundo"},
		{name: "interpolation", locale: "en", key: "product_price", args: []any{"price", 10}, want: "$10"},
		{name: "interpolation puts the currency where the locale wants it",
			locale: "es", key: "product_price", args: []any{"price", 10}, want: "10 EUR"},
		{name: "explicit verb", locale: "en", key: "greeting", args: []any{"name", "Ada"}, want: "Hello, Ada!"},
		{name: "escaped percent", locale: "en", key: "percent", want: "100% sure"},
		{name: "scope as a string", locale: "en", key: "title", args: []any{"scope", "store"}, want: "The Store"},
		{name: "scope as a list", locale: "en", key: "title",
			args: []any{"scope", []string{"store"}}, want: "The Store"},
		{name: "missing key falls back to the default locale",
			locale: "es", key: "store.title", want: "The Store"},
		{name: "default text", locale: "en", key: "nope", args: []any{"default", "Not here"}, want: "Not here"},
		{name: "default chain reaches the second entry", locale: "en", key: "nope",
			args: []any{"default", []any{Key("also.missing"), "Not here"}}, want: "Not here"},
		{name: "default key is looked up", locale: "en", key: "nope",
			args: []any{"default", Key("hello")}, want: "Hello world"},
		{name: "locale option overrides the argument", locale: "en", key: "hello",
			args: []any{"locale", "es"}, want: "Hola mundo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := store.T(tc.locale, tc.key, tc.args...); got != tc.want {
				t.Errorf("T(%q, %q, %v) = %q, want %q", tc.locale, tc.key, tc.args, got, tc.want)
			}
		})
	}
}

func TestTranslateMissing(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  hello: Hello\n"})

	if got := store.T("en", "nope.at.all"); got != "translation missing: en.nope.at.all" {
		t.Errorf("T of a missing key = %q, want the marker naming the key", got)
	}

	_, err := store.Get("en", Lookup{Key: "nope"})
	var missing *MissingTranslationError
	if !errors.As(err, &missing) {
		t.Fatalf("Get of a missing key returned %v, want a MissingTranslationError", err)
	}
	if !errors.Is(err, ErrMissingTranslation) {
		t.Error("the error does not report ErrMissingTranslation")
	}
	if len(missing.Tried) == 0 {
		t.Error("the error lists no locales as tried, want the chain it walked")
	}
}

func TestPluralization(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": `
en:
  inbox:
    zero: no messages
    one: one message
    other: "%{count} messages"
  apples:
    one: one apple
    other: "%{count} apples"
`,
		"ru.yml": `
ru:
  i18n:
    plural:
      rule: slavic
  files:
    one: "%{count} file one"
    few: "%{count} file few"
    many: "%{count} file many"
`,
	})

	cases := []struct {
		locale string
		key    string
		count  int
		want   string
	}{
		{locale: "en", key: "inbox", count: 0, want: "no messages"},
		{locale: "en", key: "inbox", count: 1, want: "one message"},
		{locale: "en", key: "inbox", count: 2, want: "2 messages"},
		{locale: "en", key: "apples", count: 0, want: "0 apples"},
		{locale: "ru", key: "files", count: 1, want: "1 file one"},
		{locale: "ru", key: "files", count: 3, want: "3 file few"},
		{locale: "ru", key: "files", count: 5, want: "5 file many"},
		{locale: "ru", key: "files", count: 11, want: "11 file many"},
		{locale: "ru", key: "files", count: 21, want: "21 file one"},
	}
	for _, tc := range cases {
		got := store.T(tc.locale, tc.key, "count", tc.count)
		if got != tc.want {
			t.Errorf("T(%q, %q, count=%d) = %q, want %q", tc.locale, tc.key, tc.count, got, tc.want)
		}
	}
}

func TestPluralizationRefusesUnusableData(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  forms:\n    one: one\n    other: many\n  plain: just text\n",
	}, func(o *StoreOptions) { o.ExceptionHandler = StrictExceptionHandler })

	// A set of forms with no count has nothing to choose between.
	if _, err := store.Get("en", Lookup{Key: "forms"}); !errors.Is(err, ErrInvalidPluralizationData) {
		t.Errorf("Get of plural forms without a count returned %v, want ErrInvalidPluralizationData", err)
	}

	// A single string with a count is not an error: the count is interpolated,
	// which is what a language with one form needs.
	count := 5
	if got, err := store.Get("en", Lookup{Key: "plain", Count: &count}); err != nil || got != "just text" {
		t.Errorf("Get of a single string with a count = %q, %v, want the text and no error", got, err)
	}
}

func TestFallbacks(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml":    "en:\n  a: en a\n  b: en b\n  c: en c\n",
		"pt.yml":    "pt:\n  a: pt a\n  b: pt b\n",
		"pt-BR.yml": "pt-BR:\n  a: pt-BR a\n",
	})

	cases := map[string]string{"a": "pt-BR a", "b": "pt b", "c": "en c"}
	for key, want := range cases {
		if got := store.T("pt-BR", key); got != want {
			t.Errorf("T(pt-BR, %q) = %q, want %q", key, got, want)
		}
	}

	chain := store.FallbacksFor("pt-BR")
	if len(chain) != 3 || chain[0] != "pt-BR" || chain[1] != "pt" || chain[2] != "en" {
		t.Errorf("FallbacksFor(pt-BR) = %v, want the regional locale, its language, then the default", chain)
	}
}

func TestDeclaredFallbacks(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  a: en a\n",
		"nb.yml": "nb:\n  a: nb a\n  b: nb b\n",
		"nn.yml": "nn:\n  a: nn a\n",
	}, func(o *StoreOptions) {
		o.Fallbacks = map[string][]string{"nn": {"nb"}}
	})

	if got := store.T("nn", "b"); got != "nb b" {
		t.Errorf("T(nn, b) = %q, want the declared fallback to answer", got)
	}
	if got := store.T("nn", "a"); got != "nn a" {
		t.Errorf("T(nn, a) = %q, want the locale's own translation to win", got)
	}
}

func TestEnforceAvailableLocales(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: en a\n"}, func(o *StoreOptions) {
		o.AvailableLocales = []string{"en"}
		o.EnforceAvailable = true
		o.ExceptionHandler = StrictExceptionHandler
	})

	if _, err := store.Get("de", Lookup{Key: "a"}); !errors.Is(err, ErrInvalidLocale) {
		t.Errorf("Get in an unavailable locale returned %v, want ErrInvalidLocale", err)
	}
	if _, err := store.Get("en", Lookup{Key: "a"}); err != nil {
		t.Errorf("Get in an available locale returned %v, want no error", err)
	}
}

func TestNamespace(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  welcome:\n    title: Welcome\n    body:\n      lead: Hello\n",
	})

	group, ok := store.Namespace("en", "welcome")
	if !ok {
		t.Fatal("Namespace reported nothing for a key that names one")
	}
	want := Group{"title": "Welcome", "body.lead": "Hello"}
	if len(group) != len(want) {
		t.Fatalf("Namespace = %v, want %v", group, want)
	}
	for key, text := range want {
		if group[key] != text {
			t.Errorf("Namespace[%q] = %q, want %q", key, group[key], text)
		}
	}

	if _, ok := store.Namespace("en", "welcome.title"); ok {
		t.Error("Namespace answered for a key that names a leaf, want nothing")
	}
}

func TestExists(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: one\n  ns:\n    b: two\n"})
	if !store.Exists("en", "a") || !store.Exists("en", "ns.b") {
		t.Error("Exists reported false for keys that are present")
	}
	if store.Exists("en", "ns") || store.Exists("en", "nope") {
		t.Error("Exists reported true for a namespace or a missing key")
	}
}

func TestBuiltinLocaleLoads(t *testing.T) {
	t.Parallel()
	store := Builtin()

	// Every message the framework produces has to be reachable, and the built-in
	// file is the only place they are written down once i18n is turned on.
	cases := map[string]string{
		"errors.messages.blank":             "is required",
		"errors.messages.email":             "must be a valid email address",
		"muzak.http.404":                    "The requested resource was not found.",
		"muzak.validation.summary":          "The request could not be validated.",
		"muzak.binding.integer":             "must be a valid integer",
		"support.array.two_words_connector": " and ",
	}
	for key, want := range cases {
		if got := store.T("en", key); got != want {
			t.Errorf("the built-in locale has %q = %q, want %q", key, got, want)
		}
	}

	if got := store.T("en", "errors.messages.too_short", "count", 12); got != "must be at least 12 characters" {
		t.Errorf("too_short with a count = %q, want the plural form filled in", got)
	}
	if got := store.T("en", "errors.messages.too_short", "count", 1); got != "must be at least 1 character" {
		t.Errorf("too_short with a count of one = %q, want the singular", got)
	}

	// The month arrays are one-based, which the strftime formatter relies on.
	months, ok := store.Backend().Lookup("en", "date.month_names")
	if !ok {
		t.Fatal("the built-in locale has no date.month_names")
	}
	list, isList := months.([]any)
	if !isList || len(list) != 13 || list[0] != nil || list[1] != "January" {
		t.Errorf("date.month_names = %v, want a one-based array of thirteen", months)
	}
}

func TestDefaultStore(t *testing.T) {
	t.Parallel()
	// The package-level functions answer from the built-in locale until an
	// application installs a store of its own, which is what lets code outside a
	// request translate at all.
	if got := T("en", "errors.messages.blank"); got != "is required" {
		t.Errorf("the package-level T = %q, want the built-in translation", got)
	}
	if !Exists("en", "errors.messages.blank") {
		t.Error("the package-level Exists reported false for a built-in key")
	}
	if !contains(AvailableLocales(), "en") {
		t.Errorf("AvailableLocales = %v, want it to include en", AvailableLocales())
	}
	if _, ok := Namespace("en", "errors.messages"); !ok {
		t.Error("the package-level Namespace reported nothing for the error messages")
	}
	if Default().DefaultLocale() != "en" {
		t.Errorf("the default store answers in %q, want en", Default().DefaultLocale())
	}
}

func TestInterpolateErrors(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{
		"en.yml": "en:\n  needs: \"a %{value} here\"\n  reserved: \"a %{scope} here\"\n",
	}, func(o *StoreOptions) { o.ExceptionHandler = StrictExceptionHandler })

	_, err := store.Get("en", Lookup{Key: "needs"})
	if !errors.Is(err, ErrMissingInterpolationArgument) {
		t.Errorf("Get without a needed value returned %v, want ErrMissingInterpolationArgument", err)
	}

	_, err = store.Get("en", Lookup{Key: "reserved", Vars: map[string]any{"scope": "x"}})
	if !errors.Is(err, ErrReservedInterpolationKey) {
		t.Errorf("Get of a translation naming a reserved key returned %v, want ErrReservedInterpolationKey", err)
	}
}

func TestMalformedArguments(t *testing.T) {
	t.Parallel()
	store := storeFor(t, map[string]string{"en.yml": "en:\n  a: one\n"})

	if got := store.T("en", "a", "count"); got != "" {
		t.Errorf("T with a name and no value = %q, want the empty string", got)
	}
	if got := store.T("en", "a", 42, "x"); got != "" {
		t.Errorf("T with a name that is not a string = %q, want the empty string", got)
	}

	_, err := Interpolate("a %{b}", "b")
	if !errors.Is(err, ErrMalformedArguments) {
		t.Errorf("Interpolate with an odd argument list returned %v, want ErrMalformedArguments", err)
	}
	if !strings.Contains((&ArgumentError{Key: "k"}).Error(), "no value") {
		t.Error("an ArgumentError with no value does not say the list ended early")
	}
}
