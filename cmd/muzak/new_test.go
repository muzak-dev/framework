package main

import (
	"bytes"
	"errors"
	"go/format"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// projectFiles are the files muzak new writes, in the order it lists them.
var projectFiles = []string{
	".gitignore",
	"README.md",
	"cmd/server/main.go",
	"go.mod",
	"handlers/hello.go",
	"handlers/hello_test.go",
	"routers/hello.go",
	"routers/routers.go",
	"schemas/hello.go",
}

// treeOf lists every file and directory under dir, slash-separated.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			rel += "/"
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// filesOf lists only the files under dir.
func filesOf(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	for _, name := range treeOf(t, dir) {
		if !strings.HasSuffix(name, "/") {
			files = append(files, name)
		}
	}
	slices.Sort(files)
	return files
}

func TestNewScaffoldsTheProject(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	r := runIn(t, work, "new", "shop", "-module", "example.com/shop")
	r.expect(t, exitOK)
	dir := filepath.Join(work, "shop")

	if got := filesOf(t, dir); !slices.Equal(got, projectFiles) {
		t.Fatalf("files %q, want %q", got, projectFiles)
	}
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "module example.com/shop\n\ngo 1.27.0\n\nrequire muzak.dev/framework v0.3.0\n"; string(gomod) != want {
		t.Errorf("go.mod is\n%s\nwant\n%s", gomod, want)
	}
	main, err := os.ReadFile(filepath.Join(dir, "cmd", "server", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"example.com/shop/routers"`, `Title:   "shop",`, "app.Include(routers.Hello())", "app.RunSignals()"} {
		if !bytes.Contains(main, []byte(want)) {
			t.Errorf("cmd/server/main.go does not contain %q", want)
		}
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(readme, []byte("# shop\n")) {
		t.Errorf("README.md starts %q", readme[:min(len(readme), 20)])
	}
	golden(t, "new.golden", r.stdout)
}

func TestNewWritesFilesReadableByAllAndWritableByTheOwner(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	work := t.TempDir()
	runIn(t, work, "new", "perms", "-module", "example.com/perms").expect(t, exitOK)
	dir := filepath.Join(work, "perms")
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		perm, want := info.Mode().Perm(), fs.FileMode(0o644)
		if entry.IsDir() {
			want = 0o755
		}
		// The umask may take bits away, never add them; the owner always
		// keeps what it needs.
		if perm&^want != 0 || perm&0o600 != 0o600 || (entry.IsDir() && perm&0o700 != 0o700) {
			t.Errorf("%s has permissions %v, want %v", name, perm, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewDefaultsTheModulePathToTheDirectoryName(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	runIn(t, work, "new", "inventory").expect(t, exitOK)
	gomod, err := os.ReadFile(filepath.Join(work, "inventory", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(gomod, []byte("module inventory\n")) {
		t.Errorf("go.mod starts %q", gomod)
	}
}

func TestNewWritesIntoAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	dir := filepath.Join(work, "empty")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runIn(t, dir, "new", ".", "-module", "example.com/empty").expect(t, exitOK)
	if got := filesOf(t, dir); !slices.Equal(got, projectFiles) {
		t.Errorf("files %q, want %q", got, projectFiles)
	}
}

func TestNewRefusesADirectoryThatIsNotEmpty(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	dir := filepath.Join(work, "busy")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "new", "busy").expect(t, exitFailure, `busy is not empty, and new writes a project only into an empty directory`)
	if got := treeOf(t, dir); !slices.Equal(got, []string{"go.mod"}) {
		t.Errorf("the directory now holds %q", got)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "go.mod")); string(data) != "module mine\n" {
		t.Errorf("go.mod was written over: %q", data)
	}
}

func TestNewRefusesADirectoryHoldingOnlyAHiddenFile(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	dir := filepath.Join(work, "repo")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "new", "repo").expect(t, exitFailure, `is not empty`)
}

func TestNewRefusesAFileInTheWay(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	path := filepath.Join(work, "taken")
	if err := os.WriteFile(path, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "new", "taken").expect(t, exitFailure, `taken already exists and is not a directory`)
	if data, _ := os.ReadFile(path); string(data) != "mine" {
		t.Errorf("the file was written over: %q", data)
	}
}

func TestNewRefusesASymbolicLinkAsTheTarget(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(work, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	runIn(t, work, "new", "link", "-module", "example.com/link").expect(t, exitFailure, `link is a symbolic link`)
	if got := treeOf(t, outside); len(got) != 0 {
		t.Errorf("the directory the link points to now holds %q", got)
	}
}

func TestNewNeedsTheParentToExist(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	runIn(t, work, "new", filepath.Join("missing", "shop")).expect(t, exitFailure, `the directory that would hold .*shop does not exist; create it first`)
	if got := treeOf(t, work); len(got) != 0 {
		t.Errorf("the working directory now holds %q", got)
	}
}

func TestNewReachesAboveTheWorkingDirectoryOnlyWhereTold(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// ".." is the parent itself, which holds the working directory, so it
	// is not empty and is refused rather than written into.
	runIn(t, work, "new", "..", "-module", "example.com/up").expect(t, exitFailure, `is not empty`)
	runIn(t, work, "new", filepath.Join("..", "work"), "-module", "example.com/up").expect(t, exitOK)
	if got := filesOf(t, work); !slices.Equal(got, projectFiles) {
		t.Errorf("files %q, want %q", got, projectFiles)
	}
	if got := treeOf(t, parent); len(got) != 1+len(treeOf(t, work)) {
		t.Errorf("something was written beside the project: %q", got)
	}
}

func TestNewArgumentErrors(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	runIn(t, work, "new").expect(t, exitUsage, `new needs the directory to create the project in, as in muzak new shop`, `muzak help new`)
	runIn(t, work, "new", "a", "b").expect(t, exitUsage, `new creates one project at a time, and was given 2 directories`)
	runIn(t, work, "new", "").expect(t, exitUsage, `was given an empty name`)
	runIn(t, work, "new", "My App").expect(t, exitUsage, `the name of the directory, "My App", is not a module path, so name one with -module`)
	if got := treeOf(t, work); len(got) != 0 {
		t.Errorf("a refused command line wrote %q", got)
	}
}

func TestNewRefusesAModulePathTheGoCommandWould(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		"example.com/../shop":         `has the element "..", which begins or ends with a dot`,
		"..":                          `has the element "..", which begins or ends with a dot`,
		"./shop":                      `has the element ".", which begins or ends with a dot`,
		"example.com/shop.":           `which begins or ends with a dot`,
		"example.com//shop":           `has an empty element`,
		"/shop":                       `has an empty element`,
		"shop/":                       `has an empty element`,
		"-shop":                       `begins with a dash, which the go command would read as a flag`,
		"my shop":                     `holds ' '`,
		`example.com\shop`:            `holds '\\\\'`,
		"example.com/shop\"x":         `holds '"'`,
		"example.com/shop\nrequire x": `holds '\\n'`,
		"example.com/caf\xc3\xa9":     `"example.com/caf\\u00e9" holds '\\u00e9'`,
		"example.com/con":             `"con", which Windows reserves for a device`,
		"example.com/LPT1.txt":        `which Windows reserves for a device`,
		"example.com/PROGRA~1":        `which Windows reads as a short file name`,
		strings.Repeat("a", 257):      `is 257 bytes long, and one is at most 256`,
	}
	for module, want := range refused {
		work := t.TempDir()
		runIn(t, work, "new", "shop", "-module", module).expect(t, exitUsage, `muzak: the module path`, want)
		if got := treeOf(t, work); len(got) != 0 {
			t.Errorf("%q: a refused module path wrote %q", module, got)
		}
	}
	for _, module := range []string{"shop", "example.com/shop", "github.com/Org/my-shop_v2.go/v2", "a~b", "x~", strings.Repeat("a", 256), "condo", "COM10"} {
		if err := checkModulePath(module); err != nil {
			t.Errorf("checkModulePath(%q) = %v, want nil", module, err)
		}
	}
	if err := checkModulePath(""); err == nil || err.Error() != "muzak: the module path is empty" {
		t.Errorf("checkModulePath(\"\") = %v", err)
	}
}

func TestRenderedGoFilesAreFormatted(t *testing.T) {
	t.Parallel()
	// The imports of the project sort on either side of muzak.dev, and the
	// templates must stay formatted either way.
	for _, module := range []string{"example.com/shop", "zoo.example/app", "a", "Z-Shop_v2~x"} {
		files := renderSkeleton(skeletonData{Module: module, Name: module[strings.LastIndexByte(module, '/')+1:], Version: "v0.3.0"})
		for _, file := range files {
			if !strings.HasSuffix(file.name, ".go") {
				continue
			}
			formatted, err := format.Source(file.content)
			if err != nil {
				t.Fatalf("%s for %s does not parse: %v", file.name, module, err)
			}
			if !bytes.Equal(formatted, file.content) {
				t.Errorf("%s for %s is not gofmt-formatted", file.name, module)
			}
		}
	}
}

func TestScaffoldRemovesTheDirectoryItCreatedWhenAWriteFails(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	dir := filepath.Join(work, "half")
	files := []scaffoldFile{
		{name: "go.mod", content: []byte("module half\n")},
		{name: "cmd/server/main.go", content: []byte("package main\n")},
		{name: "../escaped.txt", content: []byte("outside")},
	}
	err := scaffold(dir, files)
	if err == nil || !strings.Contains(err.Error(), "could not be created in") {
		t.Fatalf("scaffold = %v, want the escape refused", err)
	}
	if got := treeOf(t, work); len(got) != 0 {
		t.Errorf("after the failure the working directory holds %q", got)
	}
}

func TestScaffoldLeavesAnExistingDirectoryAsItFoundIt(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	dir := filepath.Join(work, "kept")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []scaffoldFile{
		{name: "go.mod", content: []byte("module kept\n")},
		{name: "handlers/hello.go", content: []byte("package handlers\n")},
		{name: "handlers/../../escaped.txt", content: []byte("outside")},
	}
	if err := scaffold(dir, files); err == nil {
		t.Fatal("a file outside the directory was written")
	}
	if got := treeOf(t, work); !slices.Equal(got, []string{"kept/"}) {
		t.Errorf("after the failure the working directory holds %q", got)
	}
}

func TestScaffoldDoesNotFollowALinkOutOfTheDirectory(t *testing.T) {
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	// A link placed inside the directory while the project is written, here
	// before the write that would follow it.
	if err := os.Symlink(outside, filepath.Join(dir, "cmd")); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var made []string
	// As if this run had made cmd and it were then replaced by the link.
	if err := makeParents(root, "cmd/server", map[string]bool{"cmd": true}, &made); err == nil {
		t.Error("a directory was created through a link that leaves the root")
	}
	if err := writeNew(root, scaffoldFile{name: "cmd/main.go", content: []byte("x")}, &made); err == nil {
		t.Error("a file was written through a link that leaves the root")
	}
	if got := treeOf(t, outside); len(got) != 0 {
		t.Errorf("the directory outside now holds %q", got)
	}
	if len(made) != 0 {
		t.Errorf("recorded %q as made", made)
	}
}

func TestScaffoldNeverWritesOverAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var made []string
	err = writeNew(root, scaffoldFile{name: "go.mod", content: []byte("theirs")}, &made)
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("writeNew = %v, want ErrExist", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "go.mod")); string(data) != "mine" {
		t.Errorf("the file was written over: %q", data)
	}
	if err := os.Mkdir(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := makeParents(root, "cmd/server", map[string]bool{}, &made); !errors.Is(err, fs.ErrExist) {
		t.Errorf("makeParents = %v, want a directory someone else made refused", err)
	}
	if len(made) != 0 {
		t.Errorf("recorded %q as made", made)
	}
}

func TestScaffoldRefusesADirectoryReplacedMeanwhile(t *testing.T) {
	t.Parallel()
	dir, other := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(other)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := checkSameDirectory(root, dir, true); err == nil || !strings.Contains(err.Error(), "was replaced while the project was being created") {
		t.Errorf("checkSameDirectory = %v", err)
	}
}

func TestScaffoldCleansUpAfterAFileItCannotWrite(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	dir := filepath.Join(work, "twice")
	files := []scaffoldFile{
		{name: "handlers/hello.go", content: []byte("package handlers\n")},
		{name: "handlers/hello.go", content: []byte("package handlers\n")},
	}
	err := scaffold(dir, files)
	if err == nil || !strings.Contains(err.Error(), "handlers/hello.go could not be written in") {
		t.Fatalf("scaffold = %v", err)
	}
	if got := treeOf(t, work); len(got) != 0 {
		t.Errorf("after the failure the working directory holds %q", got)
	}
}

// requireUnixPermissions skips a test that relies on permissions binding the
// user running it.
func requireUnixPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions that bind the user running the test")
	}
}

// chmod changes a mode for the rest of the test.
func chmod(t *testing.T, name string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(name, 0o755) })
}

func TestNewReportsADirectoryItIsNotAllowedToUse(t *testing.T) {
	t.Parallel()
	requireUnixPermissions(t)
	readOnly := t.TempDir()
	chmod(t, readOnly, 0o555)
	runIn(t, readOnly, "new", "shop").expect(t, exitFailure, `shop could not be created: .*permission denied`)

	unsearchable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unsearchable, "shop"), 0o755); err != nil {
		t.Fatal(err)
	}
	chmod(t, unsearchable, 0o000)
	runIn(t, unsearchable, "new", "shop").expect(t, exitFailure, `shop cannot be examined: .*permission denied`)

	work := t.TempDir()
	unreadable := filepath.Join(work, "shop")
	if err := os.Mkdir(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	chmod(t, unreadable, 0o333)
	runIn(t, work, "new", "shop").expect(t, exitFailure, `shop cannot be (opened|read): .*permission denied`)
}

// requireGo skips a test that runs the go command when there is none.
func requireGo(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the go command, which -short leaves out")
	}
	tool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not on PATH")
	}
	return tool
}

// repositoryRoot is the framework's own directory.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// goEnv is the environment a test runs the go command in: offline, outside
// any workspace, and with the module graph resolved from go.mod alone.
func goEnv() []string {
	return append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off", "GOTOOLCHAIN=local")
}

func TestScaffoldedProjectBuildsAndItsTestPasses(t *testing.T) {
	t.Parallel()
	goTool := requireGo(t)
	work := t.TempDir()
	runIn(t, work, "new", "my shop", "-module", "zoo.example/shop").expect(t, exitOK)
	dir := filepath.Join(work, "my shop")
	gomod := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, "\nreplace muzak.dev/framework => "+repositoryRoot(t)+"\n"...)
	if err := os.WriteFile(gomod, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"build", "./..."}, {"vet", "./..."}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command(goTool, args...)
		cmd.Dir, cmd.Env = dir, goEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s in the new project: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}
