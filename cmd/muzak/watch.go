package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// maxWatchedFiles bounds how many files dev polls. Every poll stats each of
// them, so a poll costs time linear in their number, and a tree past this is
// refused with a message rather than polled ever more slowly: a project with
// ten thousand Go files in one tree is one where dev is walking something it
// should not, such as a build output or another checkout.
const maxWatchedFiles = 10_000

// defaultWatchExtensions are the files whose change rebuilds the application
// unless -ext names others: the Go sources and the files the go command reads
// to build them, YAML, which is what the framework's locale files are and
// which an application usually embeds, and .env, which configuration is read
// from at start-up. Anything else an application writes into its own tree
// while it runs, such as a database, a log or an upload, would otherwise
// restart it every time it wrote.
const defaultWatchExtensions = "go,mod,sum,work,yaml,yml,env"

// fileState is what a poll compares a file by. A change that keeps the size
// and the modification time to the nanosecond is not seen, which no editor
// and no build tool produces.
type fileState struct {
	size    int64
	modTime int64
	mode    fs.FileMode
}

// watcher polls a tree for changes to the files dev rebuilds on, by walking
// it and comparing what it finds with what the previous walk found. Polling
// needs nothing from the operating system beyond stat, works the same on
// every platform and on every file system, network mounts and containers'
// bind mounts included, and costs a bounded amount: one walk of at most limit
// files per poll.
type watcher struct {
	root string
	// extensions are the extensions, with their dot and in lower case, of
	// the files that are watched.
	extensions map[string]bool
	// skip is a directory never walked, which is where dev writes its builds,
	// so that a build is not itself a change.
	skip  string
	limit int
	files map[string]fileState
}

// errTooManyFiles is wrapped by the error a walk past the limit returns.
var errTooManyFiles = errors.New("too many files to watch")

// newWatcher returns a watcher of the files under root with one of the given
// extensions, which have no dot and are separated by commas.
func newWatcher(root, extensions, skip string, limit int) (*watcher, error) {
	w := &watcher{root: root, skip: skip, limit: limit, extensions: map[string]bool{}}
	for _, ext := range strings.Split(extensions, ",") {
		ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
		if ext == "" || strings.ContainsAny(ext, `/\.`+" ") {
			return nil, fmt.Errorf("-ext lists %q, and an extension is a name such as go or yaml, without a dot", ext)
		}
		w.extensions["."+ext] = true
	}
	return w, nil
}

// prime records the tree as it is, which is what the first poll compares with.
func (w *watcher) prime() error {
	files, err := w.scan()
	if err != nil {
		return err
	}
	w.files = files
	return nil
}

// poll reports whether any watched file was created, removed or changed since
// the previous poll.
func (w *watcher) poll() (bool, error) {
	files, err := w.scan()
	if err != nil {
		return false, err
	}
	changed := !maps.Equal(w.files, files)
	w.files = files
	return changed, nil
}

// scan walks the tree once and records every watched file.
//
// It does not descend into a directory whose name starts with a dot, which
// is where version control, editors and tools keep their state, nor into
// node_modules, vendor or testdata, none of which holds a source the
// application is built from that its author edits, nor into the directory
// dev builds into. A symbolic link is not followed into a directory, so a
// link cannot make the walk loop, and a file that disappears or cannot be
// read mid-walk is left out rather than failing it.
func (w *watcher) scan() (map[string]fileState, error) {
	files := map[string]fileState{}
	err := filepath.WalkDir(w.root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if name == w.root {
				return err
			}
			// WalkDir reports an error below the root only for a directory
			// it could not read, which is passed over.
			return filepath.SkipDir
		}
		if entry.IsDir() {
			if name != w.root && (skippedDirectory(entry.Name()) || name == w.skip) {
				return filepath.SkipDir
			}
			return nil
		}
		if !w.extensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return nil
		}
		// Stat rather than the entry's own information, so a link to a file
		// is watched by what it points to. A file that vanished, or a link to
		// nothing, is passed over.
		if info, statErr := os.Stat(name); statErr == nil && !info.IsDir() {
			if len(files) == w.limit {
				return fmt.Errorf("%w: more than %d", errTooManyFiles, w.limit)
			}
			files[name] = fileState{size: info.Size(), modTime: info.ModTime().UnixNano(), mode: info.Mode()}
		}
		return nil
	})
	if errors.Is(err, errTooManyFiles) {
		return nil, fmt.Errorf("muzak: %s holds more than %d files dev would watch, which is more than it polls; point -watch at the directory the sources are in, or narrow -ext",
			printable(w.root), w.limit)
	}
	if err != nil {
		return nil, fmt.Errorf("muzak: the directory dev watches cannot be read: %w", err)
	}
	return files, nil
}

// skippedDirectory reports whether a directory is never watched.
func skippedDirectory(name string) bool {
	switch name {
	case "node_modules", "vendor", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".")
}
