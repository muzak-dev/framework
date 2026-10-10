package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFile writes a file, creating the directories it is in.
func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// primed returns a watcher of dir that has recorded the tree.
func primed(t *testing.T, dir, extensions string, limit int) *watcher {
	t.Helper()
	w, err := newWatcher(dir, extensions, "", limit)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.prime(); err != nil {
		t.Fatal(err)
	}
	return w
}

// expectPoll fails unless the next poll reports want.
func expectPoll(t *testing.T, w *watcher, want bool, why string) {
	t.Helper()
	changed, err := w.poll()
	if err != nil {
		t.Fatalf("%s: %v", why, err)
	}
	if changed != want {
		t.Errorf("%s: poll reported a change: %v, want %v", why, changed, want)
	}
}

func TestWatcherSeesFilesCreatedChangedAndRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	main := filepath.Join(dir, "cmd", "server", "main.go")
	writeFile(t, main, "package main\n")
	w := primed(t, dir, defaultWatchExtensions, maxWatchedFiles)
	expectPoll(t, w, false, "nothing touched")

	writeFile(t, main, "package main\n\nfunc main() {}\n")
	expectPoll(t, w, true, "a file grew")
	expectPoll(t, w, false, "after the change was seen")

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(main, later, later); err != nil {
		t.Fatal(err)
	}
	expectPoll(t, w, true, "a file was touched")

	writeFile(t, filepath.Join(dir, "handlers", "new.go"), "package handlers\n")
	expectPoll(t, w, true, "a file was created in a new directory")

	if err := os.Remove(main); err != nil {
		t.Fatal(err)
	}
	expectPoll(t, w, true, "a file was removed")

	for _, name := range []string{"go.mod", "go.sum", "go.work", filepath.Join("core", "locales", "en.yaml"), "settings.YML", ".env"} {
		writeFile(t, filepath.Join(dir, name), "x")
		expectPoll(t, w, true, name+" was created")
	}
}

func TestWatcherIgnoresWhatIsNotBuiltFrom(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	skip := filepath.Join(dir, "build")
	w, err := newWatcher(dir, defaultWatchExtensions, skip, maxWatchedFiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.prime(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		filepath.Join(".git", "HEAD.go"),
		filepath.Join(".idea", "x.go"),
		filepath.Join("node_modules", "pkg", "x.go"),
		filepath.Join("vendor", "example.com", "x.go"),
		filepath.Join("testdata", "fixture.go"),
		filepath.Join("handlers", "testdata", "fixture.go"),
		filepath.Join("build", "app.go"),
		"app.db",
		"server.log",
		"upload.png",
		"server",
		"main.go~",
		filepath.Join("handlers", ".hidden", "x.go"),
	} {
		writeFile(t, filepath.Join(dir, name), "x")
		expectPoll(t, w, false, name+" was written")
	}
	if len(w.files) != 1 {
		t.Errorf("watching %d files, want main.go alone", len(w.files))
	}
}

func TestWatcherWatchesTheExtensionsItIsGiven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := primed(t, dir, " .HTML, tmpl ", maxWatchedFiles)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	expectPoll(t, w, false, "a Go file, which -ext left out")
	writeFile(t, filepath.Join(dir, "page.html"), "<p>")
	expectPoll(t, w, true, "an HTML file")
	writeFile(t, filepath.Join(dir, "mail.TMPL"), "{{.}}")
	expectPoll(t, w, true, "a template whose extension is in upper case")
}

func TestNewWatcherRefusesAnExtensionThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, list := range []string{"", "go,", "go,,mod", "tar.gz", "a/b", `a\b`, "g o", "."} {
		if _, err := newWatcher(t.TempDir(), list, "", 1); err == nil || !strings.Contains(err.Error(), "-ext lists") {
			t.Errorf("newWatcher(%q) = %v, want it refused", list, err)
		}
	}
}

func TestWatcherCapHoldsAtItsBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 5 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d.go", i)), "package p\n")
	}
	w := primed(t, dir, "go", 5)
	expectPoll(t, w, false, "exactly the limit")
	writeFile(t, filepath.Join(dir, "sixth.go"), "package p\n")
	_, err := w.poll()
	if err == nil || !strings.Contains(err.Error(), "holds more than 5 files dev would watch") || !strings.Contains(err.Error(), "point -watch at") {
		t.Errorf("poll past the limit = %v", err)
	}
	if _, err := newWatcher(dir, "go", "", 0); err != nil {
		t.Fatal(err)
	}
	none, _ := newWatcher(dir, "go", "", 0)
	if err := none.prime(); err == nil {
		t.Error("a limit of zero admitted a file")
	}
	empty := primed(t, t.TempDir(), "go", 0)
	expectPoll(t, empty, false, "an empty tree under a limit of zero")
}

// TestWatcherCapHoldsAtTheRealLimit drives the cap dev runs with: ten
// thousand files are polled, and one more is refused, in time linear in
// their number.
func TestWatcherCapHoldsAtTheRealLimit(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("writes ten thousand files")
	}
	dir := t.TempDir()
	for i := range maxWatchedFiles {
		name := filepath.Join(dir, fmt.Sprintf("d%03d", i/100), fmt.Sprintf("f%d.go", i))
		if i%100 == 0 {
			if err := os.Mkdir(filepath.Dir(name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w := primed(t, dir, "go", maxWatchedFiles)
	start := time.Now()
	expectPoll(t, w, false, "ten thousand files")
	elapsed := time.Since(start)
	writeFile(t, filepath.Join(dir, "one-more.go"), "")
	if _, err := w.poll(); err == nil {
		t.Errorf("%d files were watched", maxWatchedFiles+1)
	}
	t.Logf("one poll of %d files took %s", maxWatchedFiles, elapsed)
}

func TestWatcherRefusesARootItCannotRead(t *testing.T) {
	t.Parallel()
	w, err := newWatcher(filepath.Join(t.TempDir(), "missing"), "go", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.prime(); err == nil || !strings.Contains(err.Error(), "the directory dev watches cannot be read") {
		t.Errorf("prime = %v", err)
	}
}

func TestWatcherSkipsWhatItCannotRead(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions that bind the user running the test")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	locked := filepath.Join(dir, "locked")
	writeFile(t, filepath.Join(locked, "x.go"), "package x\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	w := primed(t, dir, "go", 10)
	if len(w.files) != 1 {
		t.Errorf("watching %d files, want the readable one", len(w.files))
	}
	// A link to nowhere is a file that cannot be read either.
	if err := os.Symlink(filepath.Join(dir, "gone.go"), filepath.Join(dir, "dangling.go")); err != nil {
		t.Fatal(err)
	}
	expectPoll(t, w, false, "a dangling link appeared")
}

func TestWatcherDoesNotFollowALinkIntoADirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "x.go"), "package a\n")
	if err := os.Symlink(dir, filepath.Join(dir, "a", "loop")); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "linked.go")); err != nil {
		t.Fatal(err)
	}
	w := primed(t, dir, "go", 10)
	if len(w.files) != 1 {
		t.Errorf("watching %v, want a/x.go alone", w.files)
	}
}

func TestWatcherFollowsALinkToAFile(t *testing.T) {
	t.Parallel()
	dir, elsewhere := t.TempDir(), t.TempDir()
	target := filepath.Join(elsewhere, "shared.go")
	writeFile(t, target, "package shared\n")
	if err := os.Symlink(target, filepath.Join(dir, "shared.go")); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	w := primed(t, dir, "go", 10)
	writeFile(t, target, "package shared\n\nconst X = 1\n")
	expectPoll(t, w, true, "the file a link points to changed")
}

// TestWatcherWatchesTheTreeALinkedRootNames covers a project reached through
// a symbolic link, as a shell whose working directory was entered through one
// reports it: the walk starts at the directory the link names, rather than
// at the link, which it would take for a file and watch nothing under.
func TestWatcherWatchesTheTreeALinkedRootNames(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	project := filepath.Join(base, "project")
	main := filepath.Join(project, "cmd", "server", "main.go")
	writeFile(t, main, "package main\n")
	link := filepath.Join(base, "shop")
	if err := os.Symlink(project, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	w := primed(t, link, defaultWatchExtensions, 10)
	if len(w.files) != 1 {
		t.Errorf("watching %v, want cmd/server/main.go", w.files)
	}
	writeFile(t, main, "package main\n\nfunc main() {}\n")
	expectPoll(t, w, true, "a file under the linked root grew")
}
