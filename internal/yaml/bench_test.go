package yaml

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkParseLocale measures a whole locale file, which is the only thing
// this parser is ever asked to read. It runs once per locale at start-up, so
// the number that matters is milliseconds of boot rather than nanoseconds of
// request.
func BenchmarkParseLocale(b *testing.B) {
	data, err := os.ReadFile(filepath.Join("testdata", "de.yml"))
	if err != nil {
		b.Fatalf("reading the locale: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(data); err != nil {
			b.Fatalf("Parse returned %v", err)
		}
	}
}
