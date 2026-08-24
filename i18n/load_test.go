package i18n

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadFromFS(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"locales/en.yml":         &fstest.MapFile{Data: []byte("en:\n  a: from yaml\n")},
		"locales/en.json":        &fstest.MapFile{Data: []byte(`{"en": {"b": "from json"}}`)},
		"locales/nested/en.yaml": &fstest.MapFile{Data: []byte("en:\n  c: from a subdirectory\n")},
		"locales/README.md":      &fstest.MapFile{Data: []byte("not a locale file")},
		"elsewhere/en.yml":       &fstest.MapFile{Data: []byte("en:\n  d: outside the directory\n")},
	}

	store, err := Load(files, "locales")
	if err != nil {
		t.Fatalf("Load returned %v, want no error", err)
	}

	// Every locale file under the directory is read, whatever its extension and
	// however deep it sits, and files that are not locale files are left alone.
	for key, want := range map[string]string{"a": "from yaml", "b": "from json", "c": "from a subdirectory"} {
		if got := store.T("en", key); got != want {
			t.Errorf("T(en, %q) = %q, want %q", key, got, want)
		}
	}
	if store.Exists("en", "d") {
		t.Error("a file outside the named directory was loaded")
	}
}

func TestLoadFromDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "en.yml"), []byte("en:\n  a: from disk\n"), 0o600); err != nil {
		t.Fatalf("writing the locale: %v", err)
	}

	store, err := New(StoreOptions{Dir: dir})
	if err != nil {
		t.Fatalf("New returned %v, want no error", err)
	}
	if got := store.T("en", "a"); got != "from disk" {
		t.Errorf("T(en, a) = %q, want the translation read from disk", got)
	}

	// A second directory can be added afterwards, which is what a load path is.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "en.yml"), []byte("en:\n  b: also from disk\n"), 0o600); err != nil {
		t.Fatalf("writing the second locale: %v", err)
	}
	if err := store.LoadPath(other); err != nil {
		t.Fatalf("LoadPath returned %v, want no error", err)
	}
	if got := store.T("en", "b"); got != "also from disk" {
		t.Errorf("T(en, b) = %q, want the translation from the second directory", got)
	}
}

func TestLoadReportsEveryProblemTogether(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"locales/broken.yml": &fstest.MapFile{Data: []byte("en:\n\tbad: tabbed\n")},
		"locales/wrong.json": &fstest.MapFile{Data: []byte("{not json")},
		"locales/flat.yml":   &fstest.MapFile{Data: []byte("en: just a string\n")},
		"locales/good.yml":   &fstest.MapFile{Data: []byte("en:\n  a: fine\n")},
	}

	_, err := Load(files, "locales")
	if err == nil {
		t.Fatal("Load returned no error for a directory of broken files, want one")
	}
	// A first run should list everything wrong with a set of locale files, not
	// stop at the first.
	message := err.Error()
	for _, mention := range []string{"broken.yml", "wrong.json", "flat.yml"} {
		if !strings.Contains(message, mention) {
			t.Errorf("the error does not mention %s: %v", mention, err)
		}
	}
}

func TestLoadRefusesUnknownFileType(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	err = store.LoadFile("en.toml", []byte("a = 1"))
	var unknown *UnknownFileTypeError
	if !errors.As(err, &unknown) || !errors.Is(err, ErrUnknownFileType) {
		t.Fatalf("LoadFile of a .toml returned %v, want an UnknownFileTypeError", err)
	}
	if unknown.Ext != ".toml" {
		t.Errorf("the error names the extension %q, want .toml", unknown.Ext)
	}
	if !strings.Contains(unknown.Error(), ".yml") {
		t.Error("the error does not say which extensions would have worked")
	}
}

func TestLoadRefusesAMissingDirectory(t *testing.T) {
	t.Parallel()
	if _, err := New(StoreOptions{Dir: filepath.Join(t.TempDir(), "nowhere")}); err == nil {
		t.Fatal("New returned no error for a directory that is not there, want one")
	}
}

func TestLoadRefusesAnUnwritableBackend(t *testing.T) {
	t.Parallel()
	// A backend that only reads cannot have a file loaded into it, and saying so
	// is better than loading nothing and looking as though it worked.
	store, err := New(StoreOptions{Backend: &fixed{name: "the service"}})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	if err := store.LoadFile("en.yml", []byte("en:\n  a: one\n")); err == nil {
		t.Fatal("LoadFile into a read-only backend returned no error, want one")
	}
	if err := store.StoreTranslations("en", map[string]any{"a": "one"}); err == nil {
		t.Fatal("StoreTranslations into a read-only backend returned no error, want one")
	}
}

func TestStoreTranslations(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	if err := store.StoreTranslations("en", map[string]any{
		"inbox": map[string]any{"one": "one message", "other": "%{count} messages"},
	}); err != nil {
		t.Fatalf("StoreTranslations returned %v, want no error", err)
	}
	if got := store.T("en", "inbox", "count", 3); got != "3 messages" {
		t.Errorf("T(en, inbox, count=3) = %q, want the stored plural form", got)
	}
}

func TestMustLoad(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  a: one\n")}}
	if got := MustLoad(files, "locales").T("en", "a"); got != "one" {
		t.Errorf("MustLoad gave %q, want the translation", got)
	}

	defer func() {
		if recover() == nil {
			t.Error("MustLoad of a broken locale did not panic, want it to stop start-up")
		}
	}()
	MustLoad(fstest.MapFS{"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n\tbad: tabbed\n")}}, "locales")
}

func TestNewRefusesADefaultOutsideTheAvailableLocales(t *testing.T) {
	t.Parallel()
	_, err := New(StoreOptions{DefaultLocale: "de", AvailableLocales: []string{"en", "fr"}})
	if err == nil {
		t.Fatal("New returned no error for a default outside the available locales, want one")
	}
	if !strings.Contains(err.Error(), "de") {
		t.Errorf("the error does not name the locale: %v", err)
	}
}

func TestLoadIntoAnExistingStore(t *testing.T) {
	t.Parallel()
	store, err := New(StoreOptions{})
	if err != nil {
		t.Fatalf("New returned %v", err)
	}
	files := fstest.MapFS{"en.yml": &fstest.MapFile{Data: []byte("en:\n  a: one\n")}}
	if err := store.Load(files, ""); err != nil {
		t.Fatalf("Load returned %v, want no error", err)
	}
	if got := store.T("en", "a"); got != "one" {
		t.Errorf("T(en, a) = %q, want the loaded translation", got)
	}

	if err := store.LoadPath(filepath.Join(t.TempDir(), "nowhere")); err == nil {
		t.Fatal("LoadPath of a directory that is not there returned no error, want one")
	}
}

func TestSetDefaultStore(t *testing.T) {
	// Not parallel: it replaces process-wide state, and restores it afterwards
	// so that the tests running beside it still see the built-in locale.
	previous := Default()
	t.Cleanup(func() { SetDefault(previous) })

	store := MustLoad(fstest.MapFS{
		"locales/en.yml": &fstest.MapFile{Data: []byte("en:\n  custom: installed\n")},
	}, "locales")
	SetDefault(store)

	if got := Translate("en", "custom"); got != "installed" {
		t.Errorf("Translate after SetDefault = %q, want the installed store to answer", got)
	}
	SetDefault(nil)
	if got := T("en", "custom"); got != "installed" {
		t.Errorf("SetDefault(nil) replaced the store, want it left alone")
	}
}
