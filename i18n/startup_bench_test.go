package i18n

import (
	"testing"
	"testing/fstest"
)

// BenchmarkBuiltinStore measures what an application pays at start-up for the
// locale the framework ships.
//
// It matters more than a per-request number on a platform that starts a fresh
// instance to serve one request. This runs once per process, before the socket
// opens, and every millisecond of it is added to a cold start.
func BenchmarkBuiltinStore(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if s := builtin(); s == nil {
			b.Fatal("no store")
		}
	}
}

// BenchmarkApplicationStore measures loading an application's own locales on
// top of the built-in one, which is what a real start-up does.
func BenchmarkApplicationStore(b *testing.B) {
	files := fstest.MapFS{
		"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  a: one\n  b: two\n  c:\n    d: three\n")},
		"locales/es.yml": &fstest.MapFile{Data: []byte("es:\n  a: uno\n  b: dos\n  c:\n    d: tres\n")},
		"locales/fr.yml": &fstest.MapFile{Data: []byte("fr:\n  a: un\n  b: deux\n  c:\n    d: trois\n")},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Load(files, "locales"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTranslatePlain is the request-time number, and the one that should
// not allocate: a message with nothing to interpolate is handed back as it
// stands.
func BenchmarkTranslatePlain(b *testing.B) {
	s := Builtin()
	b.ReportAllocs()
	for b.Loop() {
		if s.T("en", "errors.messages.blank") == "" {
			b.Fatal("empty")
		}
	}
}

// BenchmarkTranslateInterpolated is the same message with a value filled in,
// which is the path a rejected field takes.
func BenchmarkTranslateInterpolated(b *testing.B) {
	s := Builtin()
	b.ReportAllocs()
	for b.Loop() {
		if s.T("en", "errors.messages.too_short", "count", 12) == "" {
			b.Fatal("empty")
		}
	}
}
