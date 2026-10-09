package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"unicode/utf8"
)

// skeleton holds the project muzak new writes, one template per file. A
// template is named after the file it becomes with ".tmpl" added, so that
// neither the go command nor a linter mistakes it for code of this module,
// and gitignore.tmpl becomes .gitignore, since embed leaves out a file whose
// name starts with a dot.
//
//go:embed skeleton
var skeleton embed.FS

// newCmd creates a project.
var newCmd = &command{
	name:    "new",
	args:    "<dir> [-module path]",
	summary: "create a project laid out the way the framework's example is",
	about: `New creates a project in dir: a go.mod requiring this version of the
framework, a server in cmd/server, a route in routers, its handler in handlers,
its models in schemas, a test that serves the application with testclient, a
.gitignore and a README.

The module path defaults to the name of dir. The directory is created, or may
already exist if it is empty; anything else in the way is refused rather than
written over. Nothing is written outside dir, even through a symbolic link
placed inside it while it is being written, and if writing fails part way,
everything new wrote is removed again.

Afterwards, run "go mod tidy" in dir to download the framework and record its
checksum, and "muzak dev" to run the server.`,
	make: func() runner { return &newRunner{} },
}

type newRunner struct {
	module string
}

func (n *newRunner) flags(fs *flag.FlagSet) {
	fs.StringVar(&n.module, "module", "", "the module `path`, such as example.com/shop (default: the name of dir)")
}

func (n *newRunner) run(_ context.Context, c *console, args, passthrough []string) error {
	args = append(args, passthrough...)
	switch len(args) {
	case 0:
		return usagef(newCmd, "new needs the directory to create the project in, as in muzak new shop")
	case 1:
	default:
		return usagef(newCmd, "new creates one project at a time, and was given %d directories", len(args))
	}
	if args[0] == "" {
		return usagef(newCmd, "new needs the directory to create the project in, and was given an empty name")
	}
	dir, err := filepath.Abs(c.path(args[0]))
	if err != nil {
		// coverage: Abs fails only when the working directory has been
		// removed, which a test cannot arrange without pulling it from under
		// every other test of the package.
		return fmt.Errorf("muzak: the directory %s cannot be resolved: %w", printable(args[0]), err)
	}
	module := n.module
	if module == "" {
		module = filepath.Base(dir)
		if err := checkModulePath(module); err != nil {
			return usagef(newCmd, "the name of the directory, %q, is not a module path, so name one with -module: %s",
				module, strings.TrimPrefix(err.Error(), "muzak: "))
		}
	} else if err := checkModulePath(module); err != nil {
		return &usageError{cmd: newCmd, msg: err.Error()}
	}
	files := renderSkeleton(skeletonData{Module: module, Name: path.Base(module), Version: c.version})
	if err := scaffold(dir, files); err != nil {
		return err
	}
	return reportScaffold(c.stdout, args[0], module, files)
}

// skeletonData is what the templates are rendered with. Module has passed
// checkModulePath, so it holds nothing that a Go string, an import path or a
// line of go.mod would read as anything but a name, and Name is its last
// element.
type skeletonData struct {
	Module  string
	Name    string
	Version string
}

// scaffoldFile is one file of a new project.
type scaffoldFile struct {
	// name is the slash-separated path inside the project.
	name    string
	content []byte
}

// renderSkeleton renders every template, in memory, before anything is
// written.
func renderSkeleton(data skeletonData) []scaffoldFile {
	var files []scaffoldFile
	mustRender(fs.WalkDir(skeleton, "skeleton", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		tmpl, err := template.New(path.Base(name)).Option("missingkey=error").ParseFS(skeleton, name)
		mustRender(err)
		var out bytes.Buffer
		mustRender(tmpl.Execute(&out, data))
		target := strings.TrimSuffix(strings.TrimPrefix(name, "skeleton/"), ".tmpl")
		if target == "gitignore" {
			target = ".gitignore"
		}
		files = append(files, scaffoldFile{name: target, content: out.Bytes()})
		return nil
	}))
	slices.SortFunc(files, func(a, b scaffoldFile) int { return strings.Compare(a.name, b.name) })
	return files
}

// mustRender panics on an error from reading, parsing or executing a
// template. The templates are compiled into this binary, the data they are
// given is checked first, and a test renders every one of them, so a failure
// is a mistake in this source that no command line can cause.
func mustRender(err error) {
	if err != nil {
		// coverage: unreachable for the reason above; a test that rendered a
		// broken template would fail first.
		panic("muzak: the project template could not be rendered: " + err.Error())
	}
}

// scaffold writes files into dir, creating dir when it does not exist.
//
// dir is refused when it is a file, a symbolic link or a directory that is not
// empty. Every file is written through an os.Root opened on dir, so no name,
// and no symbolic link someone places inside dir while it is being written,
// can lead a write outside it, and every file is created exclusively, so
// nothing that appears there meanwhile is written over. When a write fails,
// what scaffold created is removed, newest first, and dir too when scaffold
// created it, which leaves the file system as it found it.
func scaffold(dir string, files []scaffoldFile) (err error) {
	created, err := prepareTarget(dir)
	if err != nil {
		return err
	}
	var root *os.Root
	var made []string
	defer func() {
		if root != nil {
			if err != nil {
				for _, name := range slices.Backward(made) {
					_ = root.Remove(name)
				}
			}
			_ = root.Close()
		}
		if err != nil && created {
			// Remove rather than RemoveAll: if anything is left, someone else
			// put it there, and it is not this command's to delete.
			_ = os.Remove(dir)
		}
	}()
	root, err = os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("muzak: %s cannot be opened: %w", printable(dir), err)
	}
	if err := checkSameDirectory(root, dir, created); err != nil {
		return err
	}
	dirs := map[string]bool{}
	for _, file := range files {
		if err := makeParents(root, path.Dir(file.name), dirs, &made); err != nil {
			return fmt.Errorf("muzak: the directory for %s could not be created in %s: %w", file.name, printable(dir), err)
		}
		if err := writeNew(root, file, &made); err != nil {
			return fmt.Errorf("muzak: %s could not be written in %s: %w", file.name, printable(dir), err)
		}
	}
	return nil
}

// prepareTarget creates dir, or checks that the directory already there may
// be written into, and reports whether it created it.
func prepareTarget(dir string) (created bool, err error) {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // a project directory is meant to be read by its collaborators' tools
			if errors.Is(err, fs.ErrNotExist) {
				return false, fmt.Errorf("muzak: the directory that would hold %s does not exist; create it first", printable(dir))
			}
			return false, fmt.Errorf("muzak: %s could not be created: %w", printable(dir), err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("muzak: %s cannot be examined: %w", printable(dir), err)
	case info.Mode()&fs.ModeSymlink != 0:
		return false, fmt.Errorf("muzak: %s is a symbolic link; name the directory it points to, if that is where the project belongs", printable(dir))
	case !info.IsDir():
		return false, fmt.Errorf("muzak: %s already exists and is not a directory, so there is nowhere to create the project", printable(dir))
	}
	return false, nil
}

// checkSameDirectory checks that the root is the directory prepareTarget
// examined, and, when it was there already, that it is empty. A directory
// replaced by a symbolic link in between is refused, since the root would
// have followed the link.
func checkSameDirectory(root *os.Root, dir string, created bool) error {
	opened, openedErr := root.Stat(".")
	named, namedErr := os.Lstat(dir)
	if openedErr != nil || namedErr != nil || !os.SameFile(opened, named) {
		return fmt.Errorf("muzak: %s was replaced while the project was being created in it", printable(dir))
	}
	if created {
		return nil
	}
	// One name is enough to know, so a directory of any size costs one read.
	f, err := root.Open(".")
	if err == nil {
		defer func() { _ = f.Close() }()
		var names []string
		names, err = f.Readdirnames(1)
		if len(names) > 0 {
			return fmt.Errorf("muzak: %s is not empty, and new writes a project only into an empty directory", printable(dir))
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		// coverage: the root was opened on this directory a moment ago,
		// which needs the same permission reading it does, so only a change
		// made in between or a failing disk reaches this.
		return fmt.Errorf("muzak: %s cannot be read: %w", printable(dir), err)
	}
	return nil
}

// makeParents creates dir and each directory above it inside the root that
// this run has not created yet, recording each in made. A directory that is
// there already was put there by someone else, since the target was empty, so
// it is refused rather than written into.
func makeParents(root *os.Root, dir string, done map[string]bool, made *[]string) error {
	if dir == "." || done[dir] {
		return nil
	}
	if err := makeParents(root, path.Dir(dir), done, made); err != nil {
		return err
	}
	if err := root.Mkdir(dir, 0o755); err != nil { //nolint:gosec // a source directory is meant to be read by its collaborators' tools
		return err
	}
	done[dir] = true
	*made = append(*made, dir)
	return nil
}

// writeNew creates one file, refusing to open one that exists.
func writeNew(root *os.Root, file scaffoldFile, made *[]string) error {
	f, err := root.OpenFile(file.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // source files are meant to be read by their collaborators' tools
	if err != nil {
		return err
	}
	*made = append(*made, file.name)
	_, err = f.Write(file.content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// reportScaffold says what was created and what to do next.
func reportScaffold(w io.Writer, dir, module string, files []scaffoldFile) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Created %s, module %s:\n\n", printable(dir), module)
	for _, file := range files {
		fmt.Fprintf(&b, "  %s\n", file.name)
	}
	fmt.Fprintf(&b, "\nNext:\n\n  cd %s\n  go mod tidy\n  go test ./...\n  muzak dev\n", printable(dir))
	_, err := io.WriteString(w, b.String())
	return err
}

// maxModulePath bounds a module path, far above any real one, so that a path
// is something a person can read in an error message.
const maxModulePath = 256

// checkModulePath refuses a module path the go command would refuse, under
// the rules golang.org/x/mod/module applies to an import path: elements of
// ASCII letters, digits and "-._~", none empty, none starting or ending with a
// dot, none a name Windows reserves for a device, none ending in a tilde and
// digits, which Windows reads as a short file name. A path that passes holds
// nothing a Go string, an import path or a line of go.mod could read as
// anything but a name, which is what lets the templates write it as it is.
func checkModulePath(module string) error {
	switch {
	case module == "":
		return errors.New("muzak: the module path is empty")
	case len(module) > maxModulePath:
		return fmt.Errorf("muzak: the module path is %d bytes long, and one is at most %d", len(module), maxModulePath)
	case module[0] == '-':
		return fmt.Errorf("muzak: the module path %q begins with a dash, which the go command would read as a flag", module)
	}
	for _, elem := range strings.Split(module, "/") {
		if err := checkModuleElem(module, elem); err != nil {
			return err
		}
	}
	return nil
}

// checkModuleElem checks one element of a module path.
func checkModuleElem(module, elem string) error {
	if elem == "" {
		return fmt.Errorf("muzak: the module path %q has an empty element; it starts or ends with a slash, or has two together", module)
	}
	for i := range len(elem) {
		if !isModulePathByte(elem[i]) {
			r, _ := utf8.DecodeRuneInString(elem[i:])
			return fmt.Errorf("muzak: the module path %+q holds %+q, and a module path is made of ASCII letters, digits, slashes and - . _ ~",
				module, r)
		}
	}
	if elem[0] == '.' || elem[len(elem)-1] == '.' {
		return fmt.Errorf("muzak: the module path %q has the element %q, which begins or ends with a dot", module, elem)
	}
	if isWindowsDeviceName(elem) {
		return fmt.Errorf("muzak: the module path %q has the element %q, which Windows reserves for a device, so the module could not be checked out there", module, elem)
	}
	if i := strings.LastIndexByte(elem, '~'); i >= 0 && i < len(elem)-1 && strings.Trim(elem[i+1:], "0123456789") == "" {
		return fmt.Errorf("muzak: the module path %q has the element %q, which Windows reads as a short file name", module, elem)
	}
	return nil
}

// isModulePathByte reports whether b may appear in a module path element.
func isModulePathByte(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' ||
		b == '-' || b == '.' || b == '_' || b == '~'
}

// isWindowsDeviceName reports whether elem is a name Windows reserves, alone
// or before an extension, in any case.
func isWindowsDeviceName(elem string) bool {
	stem, _, _ := strings.Cut(elem, ".")
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}
