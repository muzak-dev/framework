package yaml

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParse asserts the one property a parser reading files from disk owes its
// caller: whatever it is handed, it returns rather than crashes.
//
// This package is the only foreign-format reader in the module, and a locale
// file is the only document the framework did not write itself, so it is the
// one place a fuzz target clearly earns the time it costs. The seed corpus is
// the real locale files, so the generated inputs start from something shaped
// like the thing being read.
func FuzzParse(f *testing.F) {
	names, _ := filepath.Glob(filepath.Join("testdata", "*.yml"))
	for _, name := range names {
		if data, err := os.ReadFile(name); err == nil {
			f.Add(data)
		}
	}
	f.Add([]byte("a: one\nb:\n  - two\n"))
	f.Add([]byte("a: &x\n  b: 1\nc: *x\n"))
	f.Add([]byte("a: |\n  text\n"))
	f.Add([]byte("a: [1, {b: 2}]\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		tree, err := Parse(data)
		if err != nil {
			if tree != nil {
				t.Errorf("Parse returned a tree alongside %v, want nothing", err)
			}
			return
		}
		if tree == nil {
			t.Error("Parse returned neither a tree nor an error")
		}
	})
}
