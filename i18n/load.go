package i18n

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"muzak.dev/framework/internal/yaml"
)

// Load builds a store from a directory of locale files.
//
// It is the short form of [New] for the common case, where the only thing an
// application configures is where its translations are:
//
//	//go:embed locales
//	var files embed.FS
//
//	store, err := i18n.Load(files, "locales")
func Load(files fs.FS, dir string) (*Store, error) {
	return New(StoreOptions{FS: files, Dir: dir})
}

// MustLoad is [Load] for a store built during start-up, where a locale file
// that will not parse should stop the program rather than be handled.
//
// It is written at the call that builds the application, so the panic happens
// before the server listens:
//
//	app := muzak.New(muzak.AppOptions{
//		I18n: muzak.I18nOptions{Store: i18n.MustLoad(files, "locales")},
//	})
func MustLoad(files fs.FS, dir string) *Store {
	store, err := Load(files, dir)
	if err != nil {
		panic(err)
	}
	return store
}

// Load reads every locale file under a directory into the store.
//
// Files are read in name order and merged key by key, so a locale spread over
// several files adds up rather than the last one read winning. Every problem
// found is reported together, because a first run should list everything wrong
// with a set of locale files rather than the first thing.
//
// The plural rules and fallback chains are brought up to date even when some
// files failed, because the ones that loaded are in the backend by then, and a
// locale whose translations are in place but whose rule is not would count by
// the wrong one.
func (s *Store) Load(files fs.FS, dir string) error {
	err := s.load(files, dir)
	s.refreshRules()
	s.refreshChains()
	return err
}

// LoadPath reads locale files from directories on disk.
//
// It is for translations deployed alongside a binary rather than built into
// it.
func (s *Store) LoadPath(paths ...string) error {
	var problems []error
	for _, dir := range paths {
		if err := s.load(nil, dir); err != nil {
			problems = append(problems, err)
		}
	}
	s.refreshRules()
	s.refreshChains()
	return errors.Join(problems...)
}

// StoreTranslations merges a tree of translations into a locale.
//
// It is the programmatic form of a locale file, for translations that come from
// somewhere other than one: a test that declares two keys, or an application
// that reads them from a service at start-up.
func (s *Store) StoreTranslations(locale string, tree map[string]any) error {
	if s.writable == nil {
		return fmt.Errorf("i18n: %s cannot be written to", s.backend.Name())
	}
	if err := s.writable.Store(locale, tree); err != nil {
		return err
	}
	s.refreshRules()
	s.refreshChains()
	return nil
}

// load reads locale files from a filesystem, or from disk when none is given.
func (s *Store) load(files fs.FS, dir string) error {
	if files == nil {
		if dir == "" {
			return nil
		}
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("i18n: the locale directory %q could not be read: %w", dir, err)
		}
		files, dir = os.DirFS(dir), "."
	}
	if dir == "" {
		dir = "."
	}

	names, err := localeFiles(files, dir)
	if err != nil {
		return err
	}

	var problems []error
	for _, name := range names {
		data, err := fs.ReadFile(files, name)
		if err != nil {
			problems = append(problems, fmt.Errorf("i18n: %s could not be read: %w", name, err))
			continue
		}
		if err := s.loadFile(name, data); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// localeFiles lists the locale files under a directory, in name order so that
// merging is the same on every machine.
func localeFiles(files fs.FS, dir string) ([]string, error) {
	var names []string
	err := fs.WalkDir(files, dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if _, known := loaders[strings.ToLower(path.Ext(name))]; known {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("i18n: the locale directory %q could not be walked: %w", dir, err)
	}
	sort.Strings(names)
	return names, nil
}

// loaders maps a file extension onto the decoder that reads it.
//
// YAML is what the locale corpus of the wider world is written in, and JSON is
// what a translation service tends to export; both describe the same tree, so
// the two decoders differ only in how they get to it.
var loaders = map[string]func(name string, data []byte) (map[string]any, error){
	".yml":  yaml.ParseFile,
	".yaml": yaml.ParseFile,
	".json": parseJSON,
}

// parseJSON reads a locale file written as JSON.
func parseJSON(name string, data []byte) (map[string]any, error) {
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("i18n: %s is not valid JSON: %w", name, err)
	}
	return tree, nil
}

// LoadFile reads one locale file that has already been read into memory.
//
// The decoder is chosen by the file's extension, which is also what makes a
// file with any other extension an error rather than a silent omission: a
// locale nobody notices is missing is worse than one that will not load.
//
// A file whose locales would take more than 64 MiB between them to hold once
// every key is spelled out in full is refused whole, before any of it is
// stored. No real locale comes near that; a file that does is one built to
// multiply a long key across many values.
//
// Like [Store.Load], it brings the plural rules and fallback chains up to date
// afterwards, whether or not every locale in the file could be stored, so a
// rule the file names at "i18n.plural.rule" counts from the next lookup.
func (s *Store) LoadFile(name string, data []byte) error {
	err := s.loadFile(name, data)
	s.refreshRules()
	s.refreshChains()
	return err
}

// loadFile is [Store.LoadFile] without the refresh, for a directory load that
// refreshes once when every file is in rather than once per file.
func (s *Store) loadFile(name string, data []byte) error {
	decode, known := loaders[strings.ToLower(path.Ext(name))]
	if !known {
		return &UnknownFileTypeError{Name: name, Ext: path.Ext(name)}
	}
	tree, err := decode(name, data)
	if err != nil {
		return err
	}

	if s.writable == nil {
		return fmt.Errorf("i18n: %s cannot be written to, so %s cannot be loaded into it", s.backend.Name(), name)
	}

	// The locales of one file are bounded together as well as one by one.
	// Anchors are shared by the whole document, so a file can alias one tree
	// under a long key in as many locales as the alias budget pays for, each
	// inside the bound on its own. Checking here also covers a backend of
	// someone else's, which receives the same trees.
	size := 0
	for _, node := range tree {
		if translations, isTree := node.(map[string]any); isTree {
			size += flatSize(0, translations, 0, maxFlattenedBytes-size)
			if size > maxFlattenedBytes {
				return tooLargeError(name)
			}
		}
	}

	var problems []error
	for locale, node := range tree {
		translations, isTree := node.(map[string]any)
		if !isTree {
			problems = append(problems, fmt.Errorf(
				"i18n: %s holds %q at its top level, where a locale naming a tree of translations was expected", name, locale))
			continue
		}
		if err := s.writable.Store(locale, translations); err != nil {
			problems = append(problems, fmt.Errorf("i18n: %s could not be stored: %w", name, err))
		}
	}
	return errors.Join(problems...)
}
