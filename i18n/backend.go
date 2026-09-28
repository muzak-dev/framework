package i18n

import (
	"fmt"
	"sort"
	"strings"

	"muzak.dev/framework/internal/yaml"
)

// Backend supplies translations for a locale.
//
// Backends are consulted in the order a [Chain] holds them and the first one
// with an entry wins, which is the contract muzak.ConfigSource follows for
// configuration values. Implement it to read translations from somewhere other
// than a file: a table, a service, or a cache in front of either.
//
// A key reaches Lookup as a dotted path, whatever separator the caller wrote it
// with. A backend is read from concurrently and must be safe for that; it is
// never written to while it is being read.
type Backend interface {
	// Name identifies the backend in errors and logs, as "the built-in
	// locales" or the path of a directory.
	Name() string
	// Lookup returns the entry under a dotted key in a locale and reports
	// whether it was present. The value is a string for a leaf, a
	// map[string]any for a namespace or a set of plural forms, or a []any for
	// a list such as the names of the months.
	Lookup(locale, key string) (any, bool)
	// Locales lists the locales this backend holds entries for.
	Locales() []string
}

// Storer is implemented by a backend that can be written to as well as read.
//
// [Store.Load] needs it, and so does any code that adds translations after
// start-up. A backend that reads from somewhere it cannot write to implements
// only [Backend].
type Storer interface {
	Backend
	// Store merges a tree into a locale, key by key: an entry already present
	// is replaced, and a namespace already present is merged into rather than
	// overwritten, so that one file may add to what another declared.
	Store(locale string, tree map[string]any) error
}

// entry is one loaded translation.
//
// Text is scanned into parts when it is loaded rather than when it is used,
// which is what makes rendering a message with no placeholders free: parts is
// nil, and the text is handed back as it stands.
type entry struct {
	// text is a leaf translation as written.
	text string
	// parts is the scan of text, or nil when there is nothing to interpolate.
	parts []part
	// plural holds the forms of a pluralized translation, and is nil for
	// everything else.
	plural map[PluralCategory]*entry
	// value is the node as it was loaded, for a caller that wants it whole: a
	// namespace of formats, or the list of month names.
	value any
}

// entryLookup is the faster path a backend may offer.
//
// [Backend] traffics in plain Go values so that a backend of someone else's can
// be written without knowing anything about this package's internals. A backend
// that does know can hand over its compiled entry instead, which is what keeps
// a translation with no placeholders from being rescanned on every request.
type entryLookup interface {
	lookupEntry(locale, key string) (*entry, bool)
}

// pluralCategories maps the names a locale file writes its plural forms under
// onto the categories a rule chooses between.
var pluralCategories = map[string]PluralCategory{
	"zero": Zero, "one": One, "two": Two, "few": Few, "many": Many, "other": Other,
}

// Simple holds translations in memory, which is what a locale file loaded at
// start-up becomes.
//
// It is the backend a [Store] builds when it is given none, and it is named for
// what it does: the simplest thing that works, swapped out rather than extended
// when that is not enough.
//
// Entries are flattened to dotted keys as they are stored, so a lookup is one
// map access rather than a walk of one map per dot.
type Simple struct {
	name  string
	trees map[string]map[string]any
	flat  map[string]map[string]*entry
}

// NewSimple returns an empty in-memory backend.
func NewSimple() *Simple {
	return &Simple{
		name:  "the loaded locales",
		trees: map[string]map[string]any{},
		flat:  map[string]map[string]*entry{},
	}
}

// Name identifies the backend in errors.
func (b *Simple) Name() string { return b.name }

// Locales lists the locales this backend holds entries for, in order.
func (b *Simple) Locales() []string {
	out := make([]string, 0, len(b.trees))
	for locale := range b.trees {
		out = append(out, locale)
	}
	sort.Strings(out)
	return out
}

// Lookup returns the value under a dotted key.
func (b *Simple) Lookup(locale, key string) (any, bool) {
	found, ok := b.lookupEntry(locale, key)
	if !ok {
		return nil, false
	}
	return found.value, true
}

// lookupEntry returns the compiled entry under a dotted key.
func (b *Simple) lookupEntry(locale, key string) (*entry, bool) {
	keys, known := b.flat[locale]
	if !known {
		return nil, false
	}
	found, ok := keys[key]
	return found, ok
}

// Store merges a tree into a locale.
//
// The merge is by key rather than by file: a locale written across several
// files, or an application's own translations laid over the framework's, add up
// rather than replace one another. Only a leaf already present is overwritten.
//
// A tree that cannot be stored as one is refused, and the locale is left as it
// was: see [flatten] for what that covers.
func (b *Simple) Store(locale string, tree map[string]any) error {
	merged := map[string]any{}
	if existing, known := b.trees[locale]; known {
		merged = cloneTree(existing)
	}
	mergeTree(merged, tree)

	flat := map[string]*entry{}
	if err := flatten(locale, "", merged, flat, 0); err != nil {
		return err
	}
	b.trees[locale] = merged
	b.flat[locale] = flat
	return nil
}

// cloneTree copies the namespaces of a tree, which is what a merge writes into.
// Leaves and lists are shared, because nothing ever writes into one.
func cloneTree(tree map[string]any) map[string]any {
	out := make(map[string]any, len(tree))
	for key, value := range tree {
		if child, isMap := value.(map[string]any); isMap {
			out[key] = cloneTree(child)
			continue
		}
		out[key] = value
	}
	return out
}

// mergeTree folds one tree into another, joining namespaces rather than
// replacing them.
func mergeTree(into, from map[string]any) {
	for key, value := range from {
		child, isMap := value.(map[string]any)
		if !isMap {
			into[key] = value
			continue
		}
		target, present := into[key].(map[string]any)
		if !present {
			target = map[string]any{}
			into[key] = target
		}
		mergeTree(target, child)
	}
}

// maxTreeDepth bounds how deeply a locale's translations may nest, which is the
// bound the YAML reader already keeps to. A locale nests perhaps six levels.
// JSON has no such bound of its own short of ten thousand, and every level
// costs a dotted path holding every key above it, so a file of a few dozen
// kilobytes that nests that deep is retained as many megabytes.
const maxTreeDepth = yaml.MaxDepth

// flatten records every node of a tree under its dotted path.
//
// Intermediate nodes are recorded as well as leaves, because a bulk lookup asks
// for a namespace by name and a date format asks for a list of month names. The
// cost is one map entry per node of a file that is read once.
//
// It refuses two trees, because neither has one meaning. One nests deeper than
// [maxTreeDepth]. The other reaches a dotted path twice, as a key that itself
// contains a dot ("a.b: one") next to a namespace that spells the same path
// out (a: {b: two}): both are the key "a.b", a lookup can return only one, and
// which one it was used to be decided by the order Go happened to range the map
// in, so the same file answered differently after each restart.
func flatten(locale, prefix string, node map[string]any, into map[string]*entry, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("i18n: the translations of %q nest more than %d levels deep, at %q", locale, maxTreeDepth, prefix)
	}
	if prefix != "" {
		into[prefix] = &entry{value: node}
		if forms, isPlural := pluralForms(node); isPlural {
			into[prefix].plural = forms
		}
	}
	// Sorted, so that when there is more than one collision the one reported
	// is the same on every run.
	keys := make([]string, 0, len(node))
	for key := range node {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := node[key]
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if _, taken := into[path]; taken {
			return fmt.Errorf("i18n: the translations of %q reach the key %q twice, as a key that contains a dot and "+
				"as nested keys; write it one way", locale, path)
		}
		switch child := value.(type) {
		case map[string]any:
			if err := flatten(locale, path, child, into, depth+1); err != nil {
				return err
			}
		case string:
			into[path] = &entry{text: child, parts: compile(child), value: child}
		default:
			into[path] = &entry{value: value}
		}
	}
	return nil
}

// pluralForms reports whether a namespace is a set of plural forms, and returns
// them when it is.
//
// The test is exact rather than approximate: every key must name a category and
// every value must be text. A namespace that merely happens to hold a key
// called "one" alongside anything else is an ordinary namespace.
func pluralForms(node map[string]any) (map[PluralCategory]*entry, bool) {
	if len(node) == 0 {
		return nil, false
	}
	forms := make(map[PluralCategory]*entry, len(node))
	for key, value := range node {
		category, named := pluralCategories[key]
		if !named {
			return nil, false
		}
		text, isText := value.(string)
		if !isText {
			return nil, false
		}
		forms[category] = &entry{text: text, parts: compile(text), value: text}
	}
	return forms, true
}

// Chain consults several backends in order and answers from the first that has
// an entry.
//
// It is how an application keeps its own translations somewhere of its own
// while still inheriting the ones this package ships: put the application's
// backend first and the built-in locales last, and a key the application has
// not translated falls through to the one that has.
type Chain struct {
	backends []Backend
}

// NewChain returns a backend that consults the given backends in order.
func NewChain(backends ...Backend) *Chain {
	return &Chain{backends: backends}
}

// Name identifies the chain by the backends in it.
func (c *Chain) Name() string {
	names := make([]string, len(c.backends))
	for i, b := range c.backends {
		names[i] = b.Name()
	}
	return strings.Join(names, ", then ")
}

// Lookup returns the first entry any backend in the chain has.
func (c *Chain) Lookup(locale, key string) (any, bool) {
	for _, b := range c.backends {
		if value, found := b.Lookup(locale, key); found {
			return value, true
		}
	}
	return nil, false
}

// lookupEntry returns the first compiled entry any backend in the chain has,
// falling back to compiling one for a backend that does not offer them.
func (c *Chain) lookupEntry(locale, key string) (*entry, bool) {
	for _, b := range c.backends {
		if compiling, offers := b.(entryLookup); offers {
			if found, ok := compiling.lookupEntry(locale, key); ok {
				return found, true
			}
			continue
		}
		if value, found := b.Lookup(locale, key); found {
			return entryFor(value), true
		}
	}
	return nil, false
}

// entryFor compiles a value a backend returned that does not compile its own.
func entryFor(value any) *entry {
	switch node := value.(type) {
	case string:
		return &entry{text: node, parts: compile(node), value: node}
	case map[string]any:
		found := &entry{value: node}
		if forms, isPlural := pluralForms(node); isPlural {
			found.plural = forms
		}
		return found
	default:
		return &entry{value: value}
	}
}

// Locales lists every locale any backend in the chain answers in.
func (c *Chain) Locales() []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range c.backends {
		for _, locale := range b.Locales() {
			if !seen[locale] {
				seen[locale] = true
				out = append(out, locale)
			}
		}
	}
	sort.Strings(out)
	return out
}
