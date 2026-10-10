package testclient_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// recordingTB stands in for the testing.TB AssertCompatible reports through,
// so that a test can check that it fails, and how, without failing itself.
// Fatalf records and returns, which is why AssertCompatible returns after
// every call to it.
type recordingTB struct {
	testing.TB
	errors, fatals, logs []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

// only fails unless the recorder holds exactly the wanted number of each kind
// of report.
func (r *recordingTB) only(t *testing.T, errors, fatals, logs int) {
	t.Helper()
	if len(r.errors) != errors || len(r.fatals) != fatals || len(r.logs) != logs {
		t.Fatalf("errors %q, fatals %q, logs %q; want %d, %d and %d", r.errors, r.fatals, r.logs, errors, fatals, logs)
	}
}

type catalogueItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type catalogueID struct {
	ID string `path:"id"`
}

// catalogueApp builds an application that reads and writes items. Each option
// changes it the way a later version of it might.
func catalogueApp(opts ...string) *muzak.App {
	has := func(opt string) bool {
		for _, o := range opts {
			if o == opt {
				return true
			}
		}
		return false
	}
	app := muzak.New(muzak.AppOptions{
		Title:         "Catalogue",
		Version:       "1.0.0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
		DisableDocs:   has("no docs"),
	})
	var read []muzak.RouteOption
	if has("rename operation") {
		read = append(read, muzak.OperationID("fetchItem"))
	}
	app.Get("/items/{id}", func(*muzak.Context, catalogueID) (catalogueItem, error) { return catalogueItem{}, nil }, read...)
	if !has("drop write") {
		app.Post("/items", func(*muzak.Context, catalogueItem) (catalogueItem, error) { return catalogueItem{}, nil },
			muzak.Status(http.StatusCreated))
	}
	if has("add health") {
		app.Get("/health", func(*muzak.Context, muzak.Empty) (muzak.Empty, error) { return muzak.Empty{}, nil })
	}
	if has("duplicate") {
		app.Get("/health", func(*muzak.Context, muzak.Empty) (muzak.Empty, error) { return muzak.Empty{}, nil })
		app.Get("/health", func(*muzak.Context, muzak.Empty) (muzak.Empty, error) { return muzak.Empty{}, nil })
	}
	return app
}

// record writes the baseline of an application, as a run with the variable
// set does.
func record(t *testing.T, app *muzak.App, path string) {
	t.Helper()
	t.Setenv("MUZAK_UPDATE_OPENAPI", "1")
	rec := &recordingTB{}
	testclient.AssertCompatible(rec, app, path)
	rec.only(t, 0, 0, 1)
	t.Setenv("MUZAK_UPDATE_OPENAPI", "")
}

func TestAssertCompatibleWithoutABaseline(t *testing.T) {
	t.Setenv("MUZAK_UPDATE_OPENAPI", "")
	path := filepath.Join(t.TempDir(), "openapi.json")
	rec := &recordingTB{}
	testclient.AssertCompatible(rec, catalogueApp(), path)
	rec.only(t, 0, 1, 0)
	if !strings.Contains(rec.fatals[0], "MUZAK_UPDATE_OPENAPI=1") || !strings.Contains(rec.fatals[0], path) {
		t.Errorf("fatal = %q, want instructions naming the variable and the path", rec.fatals[0])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a baseline was created without being asked for: %v", err)
	}
}

func TestAssertCompatibleRecordsTheBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api", "v1", "openapi.json")
	app := catalogueApp()
	record(t, app, path)

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	want, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, append(want, '\n')) {
		t.Error("the baseline is not the application's document")
	}
	// The baseline is created 0644, as a file the test creates the same way
	// is, which on Windows is a file that is not read-only.
	reference := filepath.Join(t.TempDir(), "reference")
	if err := os.WriteFile(reference, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Errorf("the baseline cannot be examined: %v", err)
	} else if want, _ := os.Stat(reference); info.Mode().Perm() != want.Mode().Perm() {
		t.Errorf("the baseline's mode = %v; want %v", info.Mode().Perm(), want.Mode().Perm())
	}
	assertOnlyEntry(t, filepath.Dir(path), "openapi.json")
	assertOnlyEntry(t, dir, "api")

	rec := &recordingTB{}
	testclient.AssertCompatible(rec, catalogueApp(), path)
	rec.only(t, 0, 0, 0)

	// A checkout with CRLF line endings holds the same document.
	if err := os.WriteFile(path, bytes.ReplaceAll(written, []byte("\n"), []byte("\r\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	crlf := &recordingTB{}
	testclient.AssertCompatible(crlf, catalogueApp(), path)
	crlf.only(t, 0, 0, 0)
}

// assertOnlyEntry fails unless a directory holds exactly one entry, which is
// how a test sees that no temporary file was left behind.
func assertOnlyEntry(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %v, want only %s", dir, names, name)
	}
}

func TestAssertCompatibleJudgesChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openapi.json")
	record(t, catalogueApp(), path)

	t.Run("compatible", func(t *testing.T) {
		rec := &recordingTB{}
		testclient.AssertCompatible(rec, catalogueApp("add health"), path)
		rec.only(t, 0, 0, 1)
		if !strings.Contains(rec.logs[0], "no longer matches") {
			t.Errorf("log = %q, want a note that the baseline is out of date", rec.logs[0])
		}
	})
	t.Run("possibly breaking", func(t *testing.T) {
		rec := &recordingTB{}
		testclient.AssertCompatible(rec, catalogueApp("rename operation"), path)
		rec.only(t, 0, 0, 2)
		if !strings.Contains(rec.logs[0], "may break") || !strings.Contains(rec.logs[0], "[operation-id-changed]") {
			t.Errorf("log = %q, want the possibly breaking change listed", rec.logs[0])
		}
	})
	t.Run("breaking", func(t *testing.T) {
		rec := &recordingTB{}
		testclient.AssertCompatible(rec, catalogueApp("drop write"), path)
		rec.only(t, 1, 0, 0)
		for _, want := range []string{"Breaking (1):", "/paths/~1items", "[path-removed]", "MUZAK_UPDATE_OPENAPI=1"} {
			if !strings.Contains(rec.errors[0], want) {
				t.Errorf("error = %q, want it to mention %q", rec.errors[0], want)
			}
		}
	})
	t.Run("a value other than 1 does not update", func(t *testing.T) {
		t.Setenv("MUZAK_UPDATE_OPENAPI", "true")
		rec := &recordingTB{}
		testclient.AssertCompatible(rec, catalogueApp("drop write"), path)
		rec.only(t, 1, 0, 0)
	})
	t.Run("accepted by recording", func(t *testing.T) {
		t.Setenv("MUZAK_UPDATE_OPENAPI", "1")
		rec := &recordingTB{}
		testclient.AssertCompatible(rec, catalogueApp("drop write"), path)
		rec.only(t, 0, 0, 2)
		if !strings.Contains(rec.logs[0], "accepts these changes") || !strings.Contains(rec.logs[0], "[path-removed]") {
			t.Errorf("log = %q, want the accepted change listed", rec.logs[0])
		}
		t.Setenv("MUZAK_UPDATE_OPENAPI", "")
		again := &recordingTB{}
		testclient.AssertCompatible(again, catalogueApp("drop write"), path)
		again.only(t, 0, 0, 0)
	})
}

func TestAssertCompatibleRefusesWhatItCannotCompare(t *testing.T) {
	t.Setenv("MUZAK_UPDATE_OPENAPI", "")
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{"openapi":"3.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge.json")
	if err := os.WriteFile(huge, bytes.Repeat([]byte(" "), 16<<20+2), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		app  *muzak.App
		path string
		want string
	}{
		{"no path", catalogueApp(), "", "needs the path"},
		{"no document", catalogueApp("no docs"), filepath.Join(dir, "x.json"), "DisableDocs"},
		{"an application that does not build", catalogueApp("duplicate"), filepath.Join(dir, "x.json"), "could not be built"},
		{"a malformed baseline", catalogueApp(), malformed, "is not a document it can compare with"},
		{"a baseline too large to read", catalogueApp(), huge, "larger than"},
		{"a directory where the baseline should be", catalogueApp(), dir, "could not be read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingTB{}
			testclient.AssertCompatible(rec, tt.app, tt.path)
			rec.only(t, 0, 1, 0)
			if !strings.Contains(rec.fatals[0], tt.want) {
				t.Errorf("fatal = %q, want it to mention %q", rec.fatals[0], tt.want)
			}
		})
	}
}

func TestAssertCompatibleRecordingFailures(t *testing.T) {
	t.Setenv("MUZAK_UPDATE_OPENAPI", "1")
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(dir, "occupied")
	if err := os.Mkdir(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"beneath a file":        filepath.Join(file, "openapi.json"),
		"over a full directory": occupied,
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recordingTB{}
			testclient.AssertCompatible(rec, catalogueApp(), path)
			rec.only(t, 0, 1, 0)
			if !strings.Contains(rec.fatals[0], "could not be written") {
				t.Errorf("fatal = %q", rec.fatals[0])
			}
		})
	}
	// Nothing was left behind beside the directory the rename failed onto.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the directory holds %d entries, want the two the test made", len(entries))
	}

	// A malformed baseline is replaced, since recording is how it is fixed.
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recordingTB{}
	testclient.AssertCompatible(rec, catalogueApp(), malformed)
	rec.only(t, 0, 0, 1)
}

// TestAssertCompatibleInAReadOnlyDirectory covers a directory the baseline
// cannot be written into, refused by permission bits on Unix and by the access
// control list on Windows: the run fails, and nothing is left there. Where
// the refusal does not bind the user, as it does not the superuser, the
// baseline is written.
func TestAssertCompatibleInAReadOnlyDirectory(t *testing.T) {
	t.Setenv("MUZAK_UPDATE_OPENAPI", "1")
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	denyCreate(t, dir)
	rec := &recordingTB{}
	testclient.AssertCompatible(rec, catalogueApp(), filepath.Join(dir, "openapi.json"))
	entries, err := os.ReadDir(dir)
	if !restrictionsBind {
		rec.only(t, 0, 0, 1)
		if err != nil || len(entries) != 1 {
			t.Errorf("the directory holds %v, %v; want the baseline", entries, err)
		}
		return
	}
	rec.only(t, 0, 1, 0)
	if err != nil || len(entries) != 0 {
		t.Errorf("the directory holds %v, %v", entries, err)
	}
}

// TestAssertCompatibleReplacesALink checks that a link at the baseline's path
// is replaced rather than written through, so nothing outside the path is
// touched.
func TestAssertCompatibleReplacesALink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "openapi.json")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	record(t, catalogueApp(), path)
	if got, err := os.ReadFile(outside); err != nil || string(got) != "untouched" {
		t.Errorf("the link's target = %q, %v; want it untouched", got, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the baseline is still a link: %v, %v", info, err)
	}
}

// untypable has a description json/v2 refuses to write, which is the one way
// an application's document can fail to be written.
type untypable struct {
	Name string `json:"name" doc:"\xff"`
}

func TestAssertCompatibleWithADocumentThatCannotBeWritten(t *testing.T) {
	t.Setenv("MUZAK_UPDATE_OPENAPI", "1")
	app := muzak.New(muzak.AppOptions{LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone}})
	app.Get("/x", func(*muzak.Context, muzak.Empty) (untypable, error) { return untypable{}, nil })
	path := filepath.Join(t.TempDir(), "openapi.json")
	rec := &recordingTB{}
	testclient.AssertCompatible(rec, app, path)
	rec.only(t, 0, 1, 0)
	// The application refuses to build, since a document it cannot encode is
	// a build error, and nothing is recorded.
	if !strings.Contains(rec.fatals[0], "cannot be encoded as JSON") {
		t.Errorf("fatal = %q", rec.fatals[0])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a baseline was written for a document that could not be: %v", err)
	}
}
