package i18n

import (
	"embed"
	"sync"
)

// builtinLocales holds the locale this package ships.
//
// Only English is here. The framework translates its own strings into the
// language it was written in, and the other hundred locales are a corpus an
// application brings. Shipping more would mean carrying translations of every
// message in every binary, and re-translating them before every release.
//
//go:embed locales/en.yml
var builtinLocales embed.FS

// builtin builds the store the package-level functions answer from before an
// application installs one of its own.
//
// A failure here is a bug in the file this package embeds rather than in
// anything a caller did, and it is caught by the test that loads it, so there
// is no error for a caller to handle and nothing sensible to return instead.
func builtin() *Store {
	store, err := New(StoreOptions{FS: builtinLocales, Dir: "locales", WithoutBuiltin: true})
	if err != nil {
		// coverage: the embedded locale is parsed by TestBuiltinLocaleLoads on
		// every run, so a file that would panic here fails the build first.
		panic(err)
	}
	return store
}

// Builtin returns a store holding only the locale this package ships.
//
// It is what an application chains its own translations in front of when it
// wants the framework's messages in a language of its own but has no wish to
// restate the English ones:
//
//	app := i18n.MustLoad(files, "locales")
//	store, err := i18n.New(i18n.StoreOptions{
//		Backend: i18n.NewChain(app.Backend(), i18n.Builtin().Backend()),
//	})
func Builtin() *Store { return builtin() }

// builtinOnce guards the shared built-in backend.
//
// It is a function with a guard rather than a variable with an initializer,
// because the initializer would refer to New, which refers back to this: an
// initialization cycle Go refuses to compile.
var (
	builtinOnce   sync.Once
	builtinShared Backend
)

// builtinBackend returns the shared backend holding the locale this package
// ships, which every store chains beneath its own translations.
//
// It is loaded once because it never changes: the file is compiled into the
// binary, and nothing writes to it after it has been read.
func builtinBackend() Backend {
	builtinOnce.Do(func() { builtinShared = builtin().Backend() })
	return builtinShared
}
