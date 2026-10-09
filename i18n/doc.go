// Package i18n translates and localizes the strings an application shows.
//
// The shape is the one the wider ecosystem settled on: a public API over a
// swappable backend, translations kept in YAML files, and a key hierarchy an
// application extends by writing the same keys under another locale. Following
// it means the locale files published for that ecosystem load here unchanged.
//
// One thing it assumes and Go does not provide is a per-goroutine current
// locale, so the one difference here is deliberate and runs through everything:
// a locale is passed, never set.
//
// # Translating
//
//	store := i18n.MustLoad(files, "locales")
//	store.T("es", "store.title")
//	store.T("es", "inbox", "count", 3)
//	store.T("es", "record_invalid", "scope", "errors.messages")
//
// Arguments are alternating names and values, the way slog reads them. Most are
// interpolated into the result; [Store.Translate] lists the handful that say
// how the lookup is performed instead.
//
// # Where the locale comes from
//
// Nothing in this package decides that. In a Muzak application the framework
// resolves it once per request, from the Accept-Language header or wherever
// else it is configured to look, and a handler translates through its context:
//
//	func handler(ctx *muzak.Context, in Params) (Out, error) {
//		return Out{Title: ctx.T("store.title")}, nil
//	}
//
// Outside a request there is no context to carry one, so the package-level [T]
// names the locale itself and answers from the store [SetDefault] installed.
//
// # Locale files
//
// A locale file is YAML or JSON whose top level is the locale:
//
//	en:
//	  store:
//	    title: "Muzak"
//	  inbox:
//	    zero: "no messages"
//	    one: "one message"
//	    other: "%{count} messages"
//
// Files are merged key by key rather than file by file, so a locale may be
// spread across as many files as suits it, and an application's own English
// overrides the framework's one key at a time rather than wholesale.
//
// Muzak has no third-party dependencies, so the YAML is read by a parser of its
// own in internal/yaml. It reads the subset locale files are written in, which
// is deliberately the subset the published locale corpora use, so a file taken
// from one loads here unchanged, formats and plural forms included.
//
// A locale file is input the framework did not write, so what one can cost is
// bounded. A tree nested more than 100 levels deep is refused, and so is a
// locale, or a file, that would take more than 64 MiB to hold once every key
// is spelled out as the full dotted path a lookup asks for. A long key above
// many values is paid for once per value, which is how a few kilobytes could
// otherwise ask for hundreds of megabytes.
//
// # Pluralization
//
// A count both selects a plural form and is interpolated into it. Which form it
// selects is the locale's own business: English has two, Russian four, Arabic
// six, and Japanese one. This package knows the CLDR arithmetic for about
// ninety languages, so a locale file has only to supply the words.
//
// A count may have a fraction, and the form it selects is the one CLDR gives
// its language for that number as it is printed: 1.5 kilometres is plural in
// English, singular in French, and a form of its own in Czech. That is why a
// rule reads a count as [PluralOperands], the integer and fraction digits CLDR
// states its rules over, rather than as an int.
//
// A bespoke rule is often written as a function inside the locale data, which a
// YAML file in Go cannot hold. A locale file here names a rule instead:
//
//	ru:
//	  i18n:
//	    plural:
//	      rule: slavic
//
// and a rule that is genuinely new is supplied as a [PluralRule] through
// [StoreOptions.PluralRules].
//
// # Fallbacks
//
// A lookup walks the locale asked for, then whatever [StoreOptions.Fallbacks]
// declares after it, then the language of a regional locale, then the default.
// So "pt-BR" reaches a "pt" file with nothing configured, and a key no locale
// translates reaches the default one.
//
// # The framework's own strings
//
// This package ships an English locale covering every string Muzak itself
// produces: the wording of each validation rule, what the binder says about a
// value it could not read, and the sentence behind each HTTP status. They are
// keyed under errors.messages, which is where the published locale files put
// them, so one of those already translates most of them.
package i18n
