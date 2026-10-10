package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/tsgen"
)

// generated is what tsgen writes for the routes fixture.
func generated(t *testing.T, client bool) string {
	t.Helper()
	doc, err := muzak.ReadDocument(bytes.NewReader(documentOf(t, routesApp())))
	if err != nil {
		t.Fatal(err)
	}
	out, err := tsgen.Generate(doc, tsgen.Options{Client: client})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestTSWritesTheDeclarations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "openapi.json", documentOf(t, routesApp()))
	r := runIn(t, dir, "ts", "-file", "openapi.json")
	r.expect(t, exitOK)
	if r.stdout != generated(t, false) {
		t.Errorf("stdout is not what tsgen generates:\n%s", r.stdout)
	}
	withClient := runIn(t, dir, "ts", "-file", "openapi.json", "-client", "-o", "-")
	withClient.expect(t, exitOK)
	if withClient.stdout != generated(t, true) || !strings.Contains(withClient.stdout, "createClient") {
		t.Errorf("-client did not write the client:\n%s", withClient.stdout)
	}
}

func TestTSFetchesTheDocument(t *testing.T) {
	t.Parallel()
	data := documentOf(t, routesApp())
	url := serveDocument(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	r := runIn(t, t.TempDir(), "ts", "-url", url)
	r.expect(t, exitOK)
	if r.stdout != generated(t, false) {
		t.Error("the declarations of a fetched document differ")
	}
}

func TestTSWritesAFileWhole(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "openapi.json", documentOf(t, routesApp()))
	out := filepath.Join("web", "api.ts")
	if err := os.Mkdir(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runIn(t, dir, "ts", "-file", "openapi.json", "-o", out)
	r.expect(t, exitOK)
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
	data, err := os.ReadFile(filepath.Join(dir, out))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != generated(t, false) {
		t.Error("the file is not what tsgen generates")
	}
	info, err := os.Stat(filepath.Join(dir, out))
	if err != nil {
		t.Fatal(err)
	}
	// A new file is created 0644 less what the umask takes away, as a file
	// the test creates the same way is.
	reference := filepath.Join(t.TempDir(), "reference")
	if err := os.WriteFile(reference, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if want, _ := os.Stat(reference); runtime.GOOS != "windows" && info.Mode().Perm() != want.Mode().Perm() {
		t.Errorf("a new file has permissions %v, want %v", info.Mode().Perm(), want.Mode().Perm())
	}

	// Writing again replaces the file, and keeps the permissions it was
	// given since.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(dir, out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runIn(t, dir, "ts", "-file", "openapi.json", "-o", out, "-client").expect(t, exitOK)
	data, err = os.ReadFile(filepath.Join(dir, out))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != generated(t, true) {
		t.Error("the file was not replaced")
	}
	if info, _ := os.Stat(filepath.Join(dir, out)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the replaced file has permissions %v, want 0600", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "web")); len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want the file alone", len(entries))
	}
}

func TestTSReplacesALinkRatherThanWritingThroughIt(t *testing.T) {
	t.Parallel()
	dir, elsewhere := t.TempDir(), t.TempDir()
	writeDocument(t, dir, "openapi.json", documentOf(t, routesApp()))
	target := filepath.Join(elsewhere, "target.ts")
	if err := os.WriteFile(target, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "api.ts")); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	runIn(t, dir, "ts", "-file", "openapi.json", "-o", "api.ts").expect(t, exitOK)
	if data, _ := os.ReadFile(target); string(data) != "theirs" {
		t.Error("the file the link pointed to was written")
	}
	if info, err := os.Lstat(filepath.Join(dir, "api.ts")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("api.ts is not a file of its own: %v", err)
	}
}

func TestTSLeavesNothingBehindWhenItCannotWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "openapi.json", documentOf(t, routesApp()))
	if err := os.Mkdir(filepath.Join(dir, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "taken", "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, dir, "ts", "-file", "openapi.json", "-o", "taken").expect(t, exitFailure, `muzak: taken could not be written`)
	runIn(t, dir, "ts", "-file", "openapi.json", "-o", filepath.Join("missing", "api.ts")).expect(t, exitFailure, `could not be written`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the directory holds %d entries, want the document and the directory in the way", len(entries))
	}
}

func TestTSCommandLineErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runIn(t, dir, "ts").expect(t, exitUsage, `ts needs a document, from -url or -file`)
	runIn(t, dir, "ts", "-file", "a.json", "out.ts").expect(t, exitUsage, `ts takes no arguments, and was given "out.ts"`)
	runIn(t, dir, "ts", "-url", "file:///etc/passwd").expect(t, exitUsage, `-url is "file:///etc/passwd"`)
	runIn(t, dir, "ts", "-file", "missing.json").expect(t, exitFailure, `the document cannot be opened`)
}

func TestTSReportsADocumentItCannotTranslate(t *testing.T) {
	t.Parallel()
	// A path whose parameter no operation describes reads as a document,
	// and has no faithful declaration.
	dir := t.TempDir()
	writeDocument(t, dir, "orphan.json", []byte(`{"openapi": "3.1.0", "info": {"title": "x", "version": "1"},
		"paths": {"/items/{\u001bid}": {"get": {"operationId": "read", "responses": {"200": {"description": "ok"}}}}}}`))
	r := runIn(t, dir, "ts", "-file", "orphan.json")
	r.expect(t, exitFailure, `muzak: the TypeScript could not be generated: .*the path names the parameter`)
	checkPrintable(t, r.stderr, true)
}
