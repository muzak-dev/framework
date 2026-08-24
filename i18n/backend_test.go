package i18n

import (
	"reflect"
	"testing"
)

// fixed is a backend of the kind someone else would write: it answers from a
// map and knows nothing about this package's compiled entries, which is the
// path a database or a translation service would take.
type fixed struct {
	name string
	data map[string]map[string]any
}

func (f *fixed) Name() string { return f.name }

func (f *fixed) Lookup(locale, key string) (any, bool) {
	value, ok := f.data[locale][key]
	return value, ok
}

func (f *fixed) Locales() []string {
	out := make([]string, 0, len(f.data))
	for locale := range f.data {
		out = append(out, locale)
	}
	return out
}

func TestSimpleBackend(t *testing.T) {
	t.Parallel()
	b := NewSimple()
	if b.Name() == "" {
		t.Error("Name is empty, want something an error can print")
	}
	if _, ok := b.Lookup("en", "anything"); ok {
		t.Error("an empty backend answered a lookup")
	}

	if err := b.Store("en", map[string]any{"a": "one", "ns": map[string]any{"b": "two"}}); err != nil {
		t.Fatalf("Store returned %v, want no error", err)
	}
	if value, ok := b.Lookup("en", "ns.b"); !ok || value != "two" {
		t.Errorf("Lookup(en, ns.b) = %v, %v, want two", value, ok)
	}

	// A second Store merges rather than replaces, which is what lets a locale be
	// written across several files.
	if err := b.Store("en", map[string]any{"ns": map[string]any{"c": "three"}}); err != nil {
		t.Fatalf("the second Store returned %v, want no error", err)
	}
	for key, want := range map[string]any{"a": "one", "ns.b": "two", "ns.c": "three"} {
		if value, ok := b.Lookup("en", key); !ok || value != want {
			t.Errorf("after merging, Lookup(en, %q) = %v, %v, want %v", key, value, ok, want)
		}
	}

	// A leaf already present is replaced rather than merged.
	if err := b.Store("en", map[string]any{"a": "replaced"}); err != nil {
		t.Fatalf("Store returned %v", err)
	}
	if value, _ := b.Lookup("en", "a"); value != "replaced" {
		t.Errorf("Lookup(en, a) = %v, want the replacement", value)
	}

	if err := b.Store("fr", map[string]any{"a": "un"}); err != nil {
		t.Fatalf("Store returned %v", err)
	}
	if got := b.Locales(); !reflect.DeepEqual(got, []string{"en", "fr"}) {
		t.Errorf("Locales = %v, want en and fr in order", got)
	}
}

func TestChainBackend(t *testing.T) {
	t.Parallel()
	first := &fixed{name: "the service", data: map[string]map[string]any{
		"en": {"a": "from the service"},
		"de": {"a": "vom Dienst"},
	}}
	second := NewSimple()
	if err := second.Store("en", map[string]any{"a": "from the file", "b": "only in the file"}); err != nil {
		t.Fatalf("Store returned %v", err)
	}

	chain := NewChain(first, second)
	if chain.Name() != "the service, then the loaded locales" {
		t.Errorf("Name = %q, want both backends in order", chain.Name())
	}

	// The first backend with an entry wins, and a key it lacks falls through.
	if value, _ := chain.Lookup("en", "a"); value != "from the service" {
		t.Errorf("Lookup(en, a) = %v, want the first backend to win", value)
	}
	if value, _ := chain.Lookup("en", "b"); value != "only in the file" {
		t.Errorf("Lookup(en, b) = %v, want the second backend to answer", value)
	}
	if _, ok := chain.Lookup("en", "nope"); ok {
		t.Error("Lookup answered for a key no backend holds")
	}

	locales := chain.Locales()
	if !reflect.DeepEqual(locales, []string{"de", "en"}) {
		t.Errorf("Locales = %v, want the union of both backends", locales)
	}
}

// TestStoreOverAPlainBackend proves a backend that does not compile its own
// entries still works, including for the things compiling is what makes fast:
// interpolation and plural forms.
func TestStoreOverAPlainBackend(t *testing.T) {
	t.Parallel()
	backend := &fixed{name: "the service", data: map[string]map[string]any{
		"en": {
			"greeting": "Hello, %{name}",
			"plain":    "no placeholders here",
			"inbox":    map[string]any{"one": "one message", "other": "%{count} messages"},
			"group":    map[string]any{"a": "one", "b": "two"},
			"list":     []any{"x", "y"},
		},
	}}
	// The built-in locale is left out so that this backend is the only one
	// answering, which is what makes the assertions below about it.
	store, err := New(StoreOptions{Backend: backend, WithoutBuiltin: true})
	if err != nil {
		t.Fatalf("New returned %v, want no error", err)
	}

	cases := []struct {
		key  string
		args []any
		want string
	}{
		{key: "plain", want: "no placeholders here"},
		{key: "greeting", args: []any{"name", "Ada"}, want: "Hello, Ada"},
		{key: "inbox", args: []any{"count", 1}, want: "one message"},
		{key: "inbox", args: []any{"count", 4}, want: "4 messages"},
	}
	for _, tc := range cases {
		if got := store.T("en", tc.key, tc.args...); got != tc.want {
			t.Errorf("T(en, %q, %v) = %q, want %q", tc.key, tc.args, got, tc.want)
		}
	}

	if group, ok := store.Namespace("en", "group"); !ok || group["a"] != "one" || group["b"] != "two" {
		t.Errorf("Namespace over a plain backend = %v, %v, want both leaves", group, ok)
	}
	if got := store.T("en", "nope"); got != "translation missing: en.nope" {
		t.Errorf("T of a key a plain backend does not hold = %q, want the marker", got)
	}
	if store.Backend() != backend {
		t.Error("Backend did not return the backend the store was built with")
	}

	// A key naming a list is not a translation, so it is missing rather than
	// rendered as whatever Go prints a slice as.
	if got := store.T("en", "list"); got != "translation missing: en.list" {
		t.Errorf("T of a key naming a list = %q, want the missing marker", got)
	}
}

// TestChainOverPlainAndCompilingBackends exercises the chain's own fast path,
// where one backend compiles its entries and the other does not.
func TestChainOverPlainAndCompilingBackends(t *testing.T) {
	t.Parallel()
	plain := &fixed{name: "the service", data: map[string]map[string]any{
		"en": {"fromService": "service says %{what}"},
	}}
	compiled := NewSimple()
	if err := compiled.Store("en", map[string]any{"fromFile": "file says %{what}"}); err != nil {
		t.Fatalf("Store returned %v", err)
	}

	store, err := New(StoreOptions{Backend: NewChain(plain, compiled)})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	if got := store.T("en", "fromService", "what", "hello"); got != "service says hello" {
		t.Errorf("T of the plain backend's key = %q", got)
	}
	if got := store.T("en", "fromFile", "what", "hello"); got != "file says hello" {
		t.Errorf("T of the compiling backend's key = %q", got)
	}
	if got := store.T("en", "nope"); got != "translation missing: en.nope" {
		t.Errorf("T of a key neither holds = %q, want the marker", got)
	}
}

func TestPluralFormsDetection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		node map[string]any
		want bool
	}{
		{name: "all categories", node: map[string]any{"one": "a", "other": "b"}, want: true},
		{name: "every category", node: map[string]any{
			"zero": "a", "one": "b", "two": "c", "few": "d", "many": "e", "other": "f"}, want: true},
		{name: "empty", node: map[string]any{}},
		{name: "a key that is not a category", node: map[string]any{"one": "a", "title": "b"}},
		{name: "a value that is not text", node: map[string]any{"one": map[string]any{"a": "b"}}},
	}
	for _, tc := range cases {
		if _, got := pluralForms(tc.node); got != tc.want {
			t.Errorf("%s: pluralForms reported %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBuiltinIsChainedByDefault is the behaviour an application depends on
// without knowing it.
//
// A locale file names the handful of strings a service adds and the handful of
// rules it wants worded differently. Everything else, from the wording of every
// validation rule to the order the parts of a date go in, comes from the locale
// the framework ships, chained beneath whatever the application wrote.
func TestBuiltinIsChainedByDefault(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	for key, want := range map[string]string{
		"errors.messages.blank": "is required",
		"date.formats.default":  "%Y-%m-%d",
		"muzak.http.404":        "The requested resource was not found.",
	} {
		if got := store.T("en", key); got != want {
			t.Errorf("a store with no files of its own has %q = %q, want %q", key, got, want)
		}
	}

	// An application's own wording wins over the framework's, key by key.
	if err := store.StoreTranslations("en", map[string]any{
		"errors": map[string]any{"messages": map[string]any{"blank": "we need this"}},
	}); err != nil {
		t.Fatalf("StoreTranslations returned %v", err)
	}
	if got := store.T("en", "errors.messages.blank"); got != "we need this" {
		t.Errorf("the application's wording gave %q, want it to win", got)
	}
	if got := store.T("en", "errors.messages.email"); got != "must be a valid email address" {
		t.Errorf("a rule the application did not touch gave %q, want the framework's wording", got)
	}

	// And it can be left out entirely, for an application that means to
	// translate everything itself.
	bare, err := New(StoreOptions{WithoutBuiltin: true})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	if got := bare.T("en", "errors.messages.blank"); got != "translation missing: en.errors.messages.blank" {
		t.Errorf("a store built without the built-in locale gave %q, want the marker", got)
	}
}
