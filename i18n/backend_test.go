package i18n

import (
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"muzak.dev/framework/internal/yaml"
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

// TestStoreRefusesAKeyReachedTwice is the regression test for a locale file
// with a dotted key next to the namespace that spells the same path out. Both
// are the key "a.b"; the one a lookup returned depended on the order Go ranged
// the map in, so the same file answered differently after each restart, and
// one translator's entry could silently shadow another's.
func TestStoreRefusesAKeyReachedTwice(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"a dotted key and nested keys":  "en:\n  \"a.b\": flat\n  a:\n    b: nested\n",
		"a dotted namespace and a leaf": "en:\n  \"a.b\":\n    c: flat\n  a:\n    b:\n      c: nested\n",
		"a dotted key and a namespace":  "en:\n  \"a.b\": flat\n  a:\n    b:\n      c: nested\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Enough repetitions that the old, order-dependent behaviour would
			// have failed to fail by luck only once in 2^n.
			for range 50 {
				store, err := New(StoreOptions{})
				if err != nil {
					t.Fatal(err)
				}
				err = store.LoadFile("f.yml", []byte(doc))
				if err == nil || !strings.Contains(err.Error(), `"a.b"`) {
					t.Fatalf("LoadFile = %v, want an error naming the key a.b", err)
				}
			}
		})
	}
}

// A collision across two files is the same collision, and a refused file
// leaves the locale as it was.
func TestStoreRefusesACollisionAcrossFilesAndKeepsWhatWasLoaded(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LoadFile("a.yml", []byte("en:\n  a:\n    b: nested\n  keep: kept\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadFile("b.yml", []byte("en:\n  \"a.b\": flat\n  other: x\n")); err == nil {
		t.Fatal("LoadFile accepted a dotted key that collides with one loaded before")
	}
	if got := store.T("en", "a.b"); got != "nested" {
		t.Errorf("a.b = %q after a refused file, want the translation already loaded", got)
	}
	if got := store.T("en", "keep"); got != "kept" {
		t.Errorf("keep = %q, want kept", got)
	}
	// The locale is still writable: the refused file did not leave a half
	// merged tree behind that fails every later load.
	if err := store.LoadFile("c.yml", []byte("en:\n  extra: fine\n")); err != nil {
		t.Errorf("LoadFile after a refused file = %v", err)
	}
	if got := store.T("en", "extra"); got != "fine" {
		t.Errorf("extra = %q, want fine", got)
	}
}

// Dotted keys that collide with nothing keep working, which is how flat files
// exported by translation tools are written.
func TestStoreKeepsDottedKeysThatDoNotCollide(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LoadFile("f.yml", []byte("en:\n  \"a.b\": flat\n  \"a.c\": other\n  x:\n    y: nested\n")); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"a.b": "flat", "a.c": "other", "x.y": "nested"} {
		if got := store.T("en", key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestStoreRefusesATreeThatNestsTooDeep is the regression test for JSON locale
// files nesting to the decoder's own limit of ten thousand: every level kept a
// dotted path holding all the levels above it, so a 54 KB file was retained as
// 85 MB.
func TestStoreRefusesATreeThatNestsTooDeep(t *testing.T) {
	t.Parallel()
	nest := func(depth int) string {
		return `{"en":` + strings.Repeat(`{"a":`, depth) + `"x"` + strings.Repeat(`}`, depth) + `}`
	}
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = store.LoadFile("deep.json", []byte(nest(9000)))
	if err == nil || !strings.Contains(err.Error(), "nest") {
		t.Fatalf("LoadFile = %v, want a refusal of a file nested 9000 levels", err)
	}
	// One inside the bound loads, and a locale nests nothing like it.
	if err := store.LoadFile("ok.json", []byte(nest(maxTreeDepth-1))); err != nil {
		t.Errorf("LoadFile of a file nested %d levels = %v, want it loaded", maxTreeDepth-1, err)
	}
}

// aliasedUnderLongKeys writes a YAML locale that keeps well inside the parser's
// alias budget and still flattens to hundreds of megabytes: a 4 KB key above
// copies of an anchored tree, so that every value beneath it is stored under a
// dotted path that spells the long key out again.
func aliasedUnderLongKeys(locale string, copies int) string {
	var b strings.Builder
	key := strings.Repeat("k", 4096)
	b.WriteString(locale + ":\n")
	b.WriteString("  l0: &l0 {a: x, b: x, c: x, d: x, e: x, f: x, g: x, h: x, i: x, j: x}\n")
	b.WriteString("  l1: &l1 {a: *l0, b: *l0, c: *l0, d: *l0, e: *l0, f: *l0, g: *l0, h: *l0, i: *l0, j: *l0}\n")
	b.WriteString("  l2: &l2 {a: *l1, b: *l1, c: *l1, d: *l1, e: *l1, f: *l1, g: *l1, h: *l1, i: *l1, j: *l1}\n")
	b.WriteString("  l3: &l3 {a: *l2, b: *l2, c: *l2, d: *l2, e: *l2, f: *l2, g: *l2, h: *l2, i: *l2, j: *l2}\n")
	b.WriteString("  " + key + ":\n")
	for i := range copies {
		fmt.Fprintf(&b, "    k%d: *l3\n", i)
	}
	return b.String()
}

// TestStoreRefusesALongKeyAboveManyValues is the regression test for the YAML
// alias budget being bypassed by flattening. The parser bounds how many values
// aliases copy, but flattening then spelled the full dotted path out once per
// value, so a 4.5 KB file holding a 4 KB key above 78,000 aliased values was
// retained as about 394 MB, growing with the length of the key.
func TestStoreRefusesALongKeyAboveManyValues(t *testing.T) {
	// Not parallel: it measures what the load allocates, which a test running
	// beside it would add to.
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(aliasedUnderLongKeys("en", 7))
	if len(data) > 5000 {
		t.Fatalf("the file is %d bytes, want it to stay the size of the original report", len(data))
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	err = store.LoadFile("big.yml", data)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("LoadFile accepted a file that flattens to hundreds of megabytes, want it refused")
	}
	for _, mention := range []string{"big.yml", "MiB"} {
		if !strings.Contains(err.Error(), mention) {
			t.Errorf("the error does not mention %s: %v", mention, err)
		}
	}
	// It is refused before the paths are built, so the load costs what the
	// parse does rather than what the flattened locale would have.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > maxFlattenedBytes/4 {
		t.Errorf("the refused load allocated %d MB, want it refused before the paths were built", allocated>>20)
	}
	if store.Exists("en", "l0.a") {
		t.Error("a refused file left some of its translations behind")
	}

	// The same tree under a short key is an ordinary use of aliases, and loads.
	if err := store.LoadFile("small.yml", []byte(aliasedUnderLongKeys("en", 0))); err != nil {
		t.Errorf("LoadFile of the anchors alone = %v, want them loaded", err)
	}
}

// TestStoreRefusesAJSONKeyAboveManyValues covers the same multiplication
// without aliases. JSON has none, but a long key above many short values costs
// the key once in the file and once per value in the store, which is quadratic
// in the size of the file: a megabyte of JSON could ask for gigabytes.
func TestStoreRefusesAJSONKeyAboveManyValues(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString(`{"en": {"` + strings.Repeat("k", 8192) + `": {`)
	for i := range 20_000 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"v%d": "x"`, i)
	}
	b.WriteString("}}}")

	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LoadFile("wide.json", []byte(b.String())); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("LoadFile = %v, want a refusal of a file that flattens to over %d MiB", err, maxFlattenedBytes>>20)
	}
}

// TestStoreTranslationsRefusesAnAmplifiedTreeAndKeepsTheLocale covers the
// bound where it is enforced for every source, which is the backend: a tree
// handed over from Go is held to it as a file is, and refusing it leaves the
// locale as it was.
func TestStoreTranslationsRefusesAnAmplifiedTreeAndKeepsTheLocale(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreTranslations("en", map[string]any{"keep": "kept"}); err != nil {
		t.Fatal(err)
	}
	values := make(map[string]any, 20_000)
	for i := range 20_000 {
		values[fmt.Sprintf("v%d", i)] = "x"
	}
	err = store.StoreTranslations("en", map[string]any{strings.Repeat("k", 8192): values})
	if err == nil || !strings.Contains(err.Error(), `"en"`) {
		t.Fatalf("StoreTranslations = %v, want a refusal naming the locale", err)
	}
	if got := store.T("en", "keep"); got != "kept" {
		t.Errorf("keep = %q after a refused tree, want the translation already stored", got)
	}
}

// TestLoadFileBoundsItsLocalesTogether covers a file that spreads the same
// multiplication across several locales, each inside the bound on its own.
// Anchors are shared by the whole document, so one set of them can be aliased
// under a long key in as many locales as the alias budget pays for.
func TestLoadFileBoundsItsLocalesTogether(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	key := strings.Repeat("k", 4096)
	b.WriteString("xa:\n")
	b.WriteString("  l0: &l0 {a: x, b: x, c: x, d: x, e: x, f: x, g: x, h: x, i: x, j: x}\n")
	b.WriteString("  l1: &l1 {a: *l0, b: *l0, c: *l0, d: *l0, e: *l0, f: *l0, g: *l0, h: *l0, i: *l0, j: *l0}\n")
	b.WriteString("  l2: &l2 {a: *l1, b: *l1, c: *l1, d: *l1, e: *l1, f: *l1, g: *l1, h: *l1, i: *l1, j: *l1}\n")
	for _, locale := range []string{"xb", "xc", "xd"} {
		b.WriteString(locale + ":\n  " + key + ":\n")
		for i := range 5 {
			fmt.Fprintf(&b, "    k%d: *l2\n", i)
		}
	}

	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Each locale alone is inside the bound, which is what makes this the
	// case to cover.
	tree, err := yaml.Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	for _, locale := range []string{"xb", "xc", "xd"} {
		if size := flatSize(0, tree[locale].(map[string]any), 0, maxFlattenedBytes); size > maxFlattenedBytes {
			t.Fatalf("%s alone flattens to %d MB, want each locale inside the bound", locale, size>>20)
		}
	}

	if err := store.LoadFile("spread.yml", []byte(b.String())); err == nil || !strings.Contains(err.Error(), "spread.yml") {
		t.Fatalf("LoadFile = %v, want a refusal naming the file", err)
	}
	for _, locale := range []string{"xa", "xb", "xc", "xd"} {
		if store.Exists(locale, "l0.a") || slices.Contains(store.Backend().Locales(), locale) {
			t.Errorf("a refused file left %s behind", locale)
		}
	}
}
