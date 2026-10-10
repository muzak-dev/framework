package muzak

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// writeServedTree writes a small build output under a fresh directory and
// returns the directory.
func writeServedTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dist")
	for name, body := range map[string]string{
		"index.html":    "<h1>hi</h1>",
		"assets/app.js": "x",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestServedDirHoldsNothingBetweenCalls reads a directory through servedDir,
// whose every call opens a root of its own and closes it before returning,
// and then removes the directory: an open directory cannot be removed on
// Windows, which is what a deployment replacing it, or a test cleaning up,
// does while the application still serves.
func TestServedDirHoldsNothingBetweenCalls(t *testing.T) {
	dir := writeServedTree(t)
	files := servedDir(dir)

	info, err := fs.Stat(files, "assets/app.js")
	if err != nil || info.IsDir() || info.Size() != 1 {
		t.Fatalf("Stat = %v, %v", info, err)
	}
	// The file is read after the root it was reached through has closed.
	file, err := files.Open("index.html")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, err := io.ReadAll(file)
	if err != nil || string(body) != "<h1>hi</h1>" {
		t.Fatalf("read %q, %v", body, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := files.Open("../outside"); err == nil {
		t.Error("Open left the directory")
	}
	if _, err := files.Open("missing.html"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open of a missing file = %v, want fs.ErrNotExist", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("the directory could not be removed between calls: %v", err)
	}
	// Gone, it is reported gone rather than served from a handle kept on it.
	if _, err := files.Open("index.html"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open after removal = %v, want fs.ErrNotExist", err)
	}
	if _, err := files.Stat("index.html"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat after removal = %v, want fs.ErrNotExist", err)
	}
}

// TestServedDirectoryMissingIsReported refuses a directory that is not there,
// whichever way this platform holds the one it serves.
func TestServedDirectoryMissingIsReported(t *testing.T) {
	if _, err := openServedDir(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("openServedDir = %v, want fs.ErrNotExist", err)
	}
}

// TestFrontendLetsItsDirectoryBeReplaced serves a build from disk and then
// removes it, as a deployment does before writing the next one. On Unix the
// open root keeps nothing from doing so, and elsewhere nothing is held open
// between requests.
func TestFrontendLetsItsDirectoryBeReplaced(t *testing.T) {
	dir := writeServedTree(t)
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: dir})
	mustBuild(t, app)
	rec := do(t, app, http.MethodGet, "/")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != "<h1>hi</h1>" {
		t.Fatalf("body = %q", got)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("the served directory could not be removed: %v", err)
	}
}
