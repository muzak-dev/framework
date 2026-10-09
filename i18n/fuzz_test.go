package i18n

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// FuzzLoadFile follows a locale file past the parser and into the store, which
// is where the YAML reader's own fuzz target stops looking.
//
// That target bounds the tree a document parses to. Flattening then spells
// every key out in full, once per value beneath it, and a tree well inside the
// parser's bounds was once retained as hundreds of megabytes. So this asserts
// what a load owes the process whatever the file: it returns rather than
// crashes, and whatever it stores costs no more to hold than
// [maxFlattenedBytes], measured on the entries actually built rather than on
// the estimate that admitted them.
func FuzzLoadFile(f *testing.F) {
	names, _ := filepath.Glob(filepath.Join("..", "internal", "yaml", "testdata", "*.yml"))
	names = append(names, filepath.Join("locales", "en.yml"))
	for _, name := range names {
		if data, err := os.ReadFile(name); err == nil {
			f.Add(data, false)
		}
	}
	f.Add([]byte("en:\n  inbox:\n    one: one message\n    other: \"%{count} messages\"\n"), false)
	f.Add([]byte("en:\n  a: &x\n    b: \"%{v}%%\"\n  c: *x\n"), false)
	f.Add([]byte(aliasedUnderLongKeys("en", 0)), false)
	f.Add([]byte(aliasedUnderLongKeys("en", 7)), false)
	f.Add([]byte(`{"en": {"a": {"b": "%{x}"}, "c": ["d"]}}`), true)

	partSize := int(reflect.TypeFor[part]().Size())
	f.Fuzz(func(t *testing.T, data []byte, asJSON bool) {
		store, err := New(StoreOptions{WithoutBuiltin: true})
		if err != nil {
			t.Fatal(err)
		}
		name := "fuzz.yml"
		if asJSON {
			name = "fuzz.json"
		}
		_ = store.LoadFile(name, data)

		cost := func(path string, e *entry) int {
			return entryCost + len(path) + len(e.text) + partSize*len(e.parts)
		}
		total := 0
		for locale, flat := range store.writable.(*Simple).flat {
			size := 0
			for path, e := range flat {
				size += cost(path, e)
				for _, form := range e.plural {
					size += cost("", form)
				}
			}
			if size > maxFlattenedBytes {
				t.Errorf("%s holds %d MB once flattened, past the bound of %d MB", locale, size>>20, maxFlattenedBytes>>20)
			}
			total += size
		}
		if total > maxFlattenedBytes {
			t.Errorf("%d input bytes were stored as %d MB once flattened, past the bound of %d MB",
				len(data), total>>20, maxFlattenedBytes>>20)
		}
	})
}
