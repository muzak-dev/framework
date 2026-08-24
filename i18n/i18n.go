package i18n

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
)

// DefaultLocale is the locale a store answers in when it is given none and
// configured with none.
const DefaultLocale = "en"

// Store holds the loaded translations and the settings a lookup resolves
// against.
//
// It holds no current locale of its own, which is the one deliberate departure
// from how this is usually done. Go has no per-goroutine storage, and a
// package-level locale shared by every goroutine would leak one request's
// language into another. Every lookup names the locale it wants instead, and
// the framework resolves that once per request and carries it on the request's
// context.
//
// A Store is safe for concurrent lookup once loading has returned. It does not
// support loading concurrently with lookup: an application loads its locales at
// start-up and only reads them afterwards, which is the same contract the
// router's tree follows.
type Store struct {
	// backend is what a lookup reads through, which is usually a chain ending
	// at the locale this package ships.
	backend Backend
	// writable is what a load writes into, which is the application's own
	// backend rather than the chain in front of it.
	writable  Storer
	entries   entryLookup
	def       string
	available []string
	enforce   bool
	fallbacks map[string][]string
	handler   ExceptionHandler
	rules     map[string]PluralRule
	supplied  map[string]PluralRule
	separator string
}

// StoreOptions configures a [Store].
//
// The zero value is usable: it builds an empty in-memory store answering in
// English, which is what [New] with no options returns.
type StoreOptions struct {
	// Backend supplies the translations. It defaults to an empty [Simple],
	// which [Store.Load] then fills.
	Backend Backend

	// FS holds locale files to load, which is how they are built into a binary:
	//
	//	//go:embed locales
	//	var files embed.FS
	//
	//	store, err := i18n.New(i18n.StoreOptions{FS: files, Dir: "locales"})
	FS fs.FS

	// Dir names a directory of locale files: within FS when one is given, and
	// on disk when one is not. Loading from disk keeps translations editable
	// without a rebuild, at the cost of a deployment that has to carry them.
	Dir string

	// DefaultLocale is the locale a lookup falls back to last. It defaults to
	// [DefaultLocale].
	DefaultLocale string

	// AvailableLocales lists the locales the application answers in. Left
	// empty, it is whatever the backend turns out to hold.
	AvailableLocales []string

	// EnforceAvailable makes a lookup in a locale outside AvailableLocales an
	// error rather than a fallback. Leave it off unless a locale arriving from
	// somewhere unexpected should be loud rather than quietly answered in the
	// default language.
	EnforceAvailable bool

	// Fallbacks maps a locale onto the locales tried after it and before the
	// default. A regional locale falls back to its own language with no entry
	// here, so "pt-BR" reaches "pt" already.
	Fallbacks map[string][]string

	// ExceptionHandler decides what a failed lookup produces. It defaults to
	// [DefaultExceptionHandler].
	ExceptionHandler ExceptionHandler

	// PluralRules supplies or overrides the rule for a locale, for a language
	// this package does not know or one an application counts differently.
	PluralRules map[string]PluralRule

	// Separator is what divides the parts of a key. It defaults to a dot, and
	// is worth changing only when a key itself has to contain one.
	Separator string

	// WithoutBuiltin leaves the locale this package ships out of the store.
	//
	// It is chained beneath the application's own translations by default, so
	// that a key the application has not translated still reaches the wording
	// the framework would otherwise have produced: the rules, the binder's
	// messages, the HTTP status sentences, and the date and number formats.
	// Turn it on only to prove that an application has translated everything
	// itself.
	WithoutBuiltin bool
}

// New builds a store from options.
//
// It loads whatever [StoreOptions.FS] and [StoreOptions.Dir] name, so a store
// that returns without error is ready to answer: a locale file that will not
// parse is reported here, at start-up, rather than on the first request that
// needs it.
func New(opts StoreOptions) (*Store, error) {
	s := &Store{
		backend:   opts.Backend,
		def:       opts.DefaultLocale,
		available: opts.AvailableLocales,
		enforce:   opts.EnforceAvailable,
		fallbacks: opts.Fallbacks,
		handler:   opts.ExceptionHandler,
		supplied:  opts.PluralRules,
		separator: opts.Separator,
	}
	if s.backend == nil {
		s.backend = NewSimple()
	}
	// Loads go into the backend the application supplied, never into the chain
	// that may be wrapped around it below.
	s.writable, _ = s.backend.(Storer)
	if s.def == "" {
		s.def = DefaultLocale
	}
	if s.handler == nil {
		s.handler = DefaultExceptionHandler
	}
	if s.separator == "" {
		s.separator = "."
	}
	s.entries, _ = s.backend.(entryLookup)

	if opts.FS != nil || opts.Dir != "" {
		if err := s.load(opts.FS, opts.Dir); err != nil {
			return nil, err
		}
	}
	if !opts.WithoutBuiltin {
		s.backend = NewChain(s.backend, builtinBackend())
	}
	s.entries, _ = s.backend.(entryLookup)
	s.refreshRules()

	if len(s.available) > 0 && !contains(s.available, s.def) {
		return nil, fmt.Errorf("i18n: the default locale %q is not among the available locales %s",
			s.def, strings.Join(s.available, ", "))
	}
	return s, nil
}

// Backend returns the backend the store reads from, so that a chain can be
// built in front of one already loaded.
func (s *Store) Backend() Backend { return s.backend }

// DefaultLocale returns the locale a lookup falls back to last.
func (s *Store) DefaultLocale() string { return s.def }

// AvailableLocales lists the locales the store answers in.
func (s *Store) AvailableLocales() []string {
	if len(s.available) > 0 {
		return s.available
	}
	return s.backend.Locales()
}

// Translate returns the translation of a key in a locale.
//
// Arguments are alternating names and values, the way slog reads them. Most are
// interpolated into the result; a handful say how the lookup is performed:
//
//	count    selects a plural form, and is interpolated as well
//	scope    is prepended to the key, as a string or a list of segments
//	default  is what to fall back to: text, an [i18n.Key], or a list tried in order
//	locale   overrides the locale passed as the first argument
//	raise    reports a missing translation rather than rendering a marker
//
// A failure is passed to the store's exception handler, which by default
// renders a missing translation as a visible marker and returns everything else
// as the empty string. Use [Store.Get] when the error itself is wanted.
func (s *Store) Translate(locale, key string, args ...any) string {
	l, override, err := parseArgs(key, args)
	if err != nil {
		text, _ := s.handler(err, locale, key)
		return text
	}
	if override != "" {
		locale = override
	}
	text, err := s.Get(locale, l)
	if err != nil {
		text, _ = s.handler(err, locale, key)
	}
	return text
}

// T is [Store.Translate].
func (s *Store) T(locale, key string, args ...any) string {
	return s.Translate(locale, key, args...)
}

// Get returns the translation for a lookup, together with whatever went wrong.
//
// It is the form that reports rather than renders, where [Store.Translate]
// renders and swallows. A test suite wants this one.
func (s *Store) Get(locale string, l Lookup) (string, error) {
	if locale == "" {
		locale = s.def
	}
	if s.enforce && !contains(s.AvailableLocales(), locale) {
		return "", &InvalidLocaleError{Locale: locale, Available: s.AvailableLocales()}
	}

	key := l.full(s.separator)
	tried := s.chain(locale)
	for _, candidate := range tried {
		found, ok := s.find(candidate, key)
		if !ok {
			continue
		}
		text, err := s.renderEntry(found, candidate, key, l)
		if err == nil {
			return text, nil
		}
		var missing *MissingTranslationError
		if !errors.As(err, &missing) {
			return "", err
		}
	}

	if text, ok, err := s.applyDefaults(locale, l); ok || err != nil {
		return text, err
	}
	return "", &MissingTranslationError{Locale: locale, Key: key, Tried: tried}
}

// Exists reports whether a key can be translated for a locale, without
// rendering it.
//
// It walks the same chain a lookup does, so a key present only in the default
// locale still exists for a request in another: that is what a caller deciding
// between a translation and a fallback needs to know.
func (s *Store) Exists(locale, key string) bool {
	if locale == "" {
		locale = s.def
	}
	path := normalizeKey(key, s.separator)
	for _, candidate := range s.chain(locale) {
		if found, ok := s.find(candidate, path); ok && (found.text != "" || found.plural != nil) {
			return true
		}
	}
	return false
}

// Namespace returns every translation under a key, each under its path relative
// to that key.
//
// It is the bulk lookup a key names when it points at a namespace rather than a
// leaf: one call for a whole group of related strings, such as the error
// messages, rather than one call per string.
func (s *Store) Namespace(locale, key string) (Group, bool) {
	if locale == "" {
		locale = s.def
	}
	path := normalizeKey(key, s.separator)
	for _, candidate := range s.chain(locale) {
		found, ok := s.find(candidate, path)
		if !ok {
			continue
		}
		node, isNamespace := found.value.(map[string]any)
		if !isNamespace {
			continue
		}
		group := Group{}
		collect("", node, group)
		return group, true
	}
	return nil, false
}

// collect walks a namespace into a flat group of its text leaves.
func collect(prefix string, node map[string]any, into Group) {
	for key, value := range node {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch child := value.(type) {
		case string:
			into[path] = child
		case map[string]any:
			collect(path, child, into)
		}
	}
}

// FallbacksFor lists the locales tried for a locale, in order, ending at the
// default. It is what a lookup walks, exposed so that a configuration can be
// checked rather than guessed at.
func (s *Store) FallbacksFor(locale string) []string { return s.chain(locale) }

// chain builds the locales a lookup walks.
//
// The order is the locale itself, whatever was declared to follow it, its own
// language when it names a region, and the default last. A regional locale
// reaching its language without being told to is the rule that makes "pt-BR"
// useful with only a "pt" file present.
func (s *Store) chain(locale string) []string {
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(candidate string) {
		if candidate != "" && !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}

	add(locale)
	for _, fallback := range s.fallbacks[locale] {
		add(fallback)
	}
	if language, _, regional := strings.Cut(locale, "-"); regional {
		add(language)
		for _, fallback := range s.fallbacks[language] {
			add(fallback)
		}
	}
	add(s.def)
	return out
}

// find returns the entry a locale holds under a key.
func (s *Store) find(locale, key string) (*entry, bool) {
	if s.entries != nil {
		return s.entries.lookupEntry(locale, key)
	}
	value, ok := s.backend.Lookup(locale, key)
	if !ok {
		return nil, false
	}
	return entryFor(value), true
}

// renderEntry turns an entry into the text a lookup asked for, choosing a
// plural form when there is a count and filling in whatever it interpolates.
func (s *Store) renderEntry(found *entry, locale, key string, l Lookup) (string, error) {
	if l.Count != nil {
		form, err := s.pluralize(found, locale, key, *l.Count)
		if err != nil {
			return "", err
		}
		found = form
	} else if found.plural != nil {
		return "", &InvalidPluralizationDataError{
			Locale: locale, Key: key, Category: Other, Have: categoriesOf(found.plural),
		}
	}

	if found.text == "" && found.parts == nil {
		if _, isText := found.value.(string); !isText {
			// A namespace or a list is not something a translation can be, so
			// the key names the wrong kind of node rather than a missing one.
			return "", &MissingTranslationError{Locale: locale, Key: key, Tried: []string{locale}}
		}
	}
	return render(found.parts, found.text, l.Vars, locale, key)
}

// pluralize chooses the form a count selects.
//
// A zero form wins for a count of nothing whenever one is written, which is
// a departure from CLDR: English has no zero category, but "no messages" reads
// better than "0 messages", and a translator who wrote one meant it to be
// used.
func (s *Store) pluralize(found *entry, locale, key string, count int) (*entry, error) {
	if found.plural == nil {
		if found.text != "" || found.parts != nil {
			// A single string with a count is not an error: the count is simply
			// interpolated, which is what a language with one form needs.
			return found, nil
		}
		return nil, &InvalidPluralizationDataError{Locale: locale, Key: key, Count: count}
	}

	if count == 0 {
		if form, written := found.plural[Zero]; written {
			return form, nil
		}
	}
	category := s.ruleFor(locale)(count)
	if form, written := found.plural[category]; written {
		return form, nil
	}
	if form, written := found.plural[Other]; written {
		return form, nil
	}
	return nil, &InvalidPluralizationDataError{
		Locale: locale, Key: key, Count: count,
		Category: category, Have: categoriesOf(found.plural),
	}
}

// categoriesOf lists the forms an entry defines, in CLDR's order, for an error
// that has to say what was there instead.
func categoriesOf(forms map[PluralCategory]*entry) []PluralCategory {
	var out []PluralCategory
	for _, category := range []PluralCategory{Zero, One, Two, Few, Many, Other} {
		if _, written := forms[category]; written {
			out = append(out, category)
		}
	}
	return out
}

// ruleFor returns the plural rule a locale counts by.
func (s *Store) ruleFor(locale string) PluralRule {
	if rule, set := s.supplied[locale]; set {
		return rule
	}
	if rule, resolved := s.rules[locale]; resolved {
		return rule
	}
	return PluralRuleFor(locale)
}

// refreshRules resolves the rule each loaded locale names at "i18n.plural.rule".
//
// It runs after loading rather than on every lookup, because reading the rule
// out of the backend on the way to every plural would cost a second lookup per
// message. The contract that a store is loaded before it is read is what makes
// caching them here safe without a lock.
func (s *Store) refreshRules() {
	rules := make(map[string]PluralRule)
	for _, locale := range s.backend.Locales() {
		named, ok := s.backend.Lookup(locale, "i18n.plural.rule")
		if !ok {
			continue
		}
		name, isName := named.(string)
		if !isName {
			continue
		}
		if rule, known := PluralRuleNamed(name); known {
			rules[locale] = rule
		}
	}
	s.rules = rules
}

// applyDefaults tries what a lookup said to fall back to, in order.
//
// A [Key] is looked up as another translation in the same scope; anything else
// is used as it stands, after being interpolated like a translation of its own.
//
// The lookup a Key performs carries no defaults of its own, which is what keeps
// this from recursing: a chain is one level deep however the locale files are
// written.
func (s *Store) applyDefaults(locale string, l Lookup) (string, bool, error) {
	for _, fallback := range l.Default {
		switch value := fallback.(type) {
		case Key:
			sub := Lookup{Key: string(value), Scope: l.Scope, Count: l.Count, Vars: l.Vars}
			text, err := s.Get(locale, sub)
			if err == nil {
				return text, true, nil
			}
			var missing *MissingTranslationError
			if !errors.As(err, &missing) {
				return "", false, err
			}
		case string:
			text, err := render(compile(value), value, l.Vars, locale, l.Key)
			if err != nil {
				return "", false, err
			}
			return text, true, nil
		case nil:
		default:
			return fmt.Sprint(value), true, nil
		}
	}
	return "", false, nil
}

// contains reports whether a list holds a value.
func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// defaultStore is the process-wide store the package-level functions use.
//
// It is an atomic pointer rather than a plain variable because a store swapped
// while requests are in flight would otherwise be a data race, and because that
// is how the framework holds its running server.
var defaultStore atomic.Pointer[Store]

// Default returns the process-wide store, which is what the package-level [T]
// and [L] use.
//
// It holds the built-in English locale until an application replaces it, which
// is what building an application with an i18n store configured does.
func Default() *Store {
	if s := defaultStore.Load(); s != nil {
		return s
	}
	s := builtin()
	if defaultStore.CompareAndSwap(nil, s) {
		return s
	}
	// coverage: reached only when two goroutines build the default store at the
	// same instant, in which case the one that lost the swap uses the winner's.
	return defaultStore.Load()
}

// SetDefault installs the process-wide store.
//
// Call it once, during start-up, before anything translates. It is safe to call
// concurrently, but a store replaced while a response is being rendered would
// translate two halves of it from two corpora.
func SetDefault(s *Store) {
	if s != nil {
		defaultStore.Store(s)
	}
}

// T translates a key using the process-wide store, for code that runs outside a
// request and so has no Muzak context to translate through.
func T(locale, key string, args ...any) string {
	return Default().Translate(locale, key, args...)
}

// Translate is [T].
func Translate(locale, key string, args ...any) string {
	return Default().Translate(locale, key, args...)
}

// Exists reports whether the process-wide store has a translation for a key.
func Exists(locale, key string) bool { return Default().Exists(locale, key) }

// Namespace returns every translation under a key from the process-wide store.
func Namespace(locale, key string) (Group, bool) { return Default().Namespace(locale, key) }

// AvailableLocales lists the locales the process-wide store answers in.
func AvailableLocales() []string { return Default().AvailableLocales() }
