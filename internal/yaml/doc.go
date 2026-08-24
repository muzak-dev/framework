// Package yaml parses the subset of YAML that locale files are written in.
//
// Muzak has no third-party dependencies, and a locale file is the one document
// the framework has to read that it did not write. This package exists to keep
// both of those true at once: it reads the YAML that translation files actually
// contain, and nothing else.
//
// # Why a subset
//
// Full YAML is a large specification with several features that are actively
// hostile in a file loaded from disk at start-up: arbitrary type tags, multiple
// documents per stream, and recursive aliases among them. None of them appear
// in a translation file. The scope of this parser was therefore not chosen by
// judgement but by evidence: it is whatever the real rails-i18n locale corpus
// in testdata needs, and every construct outside that set is refused with a
// located error rather than guessed at.
//
// # Supported
//
//	block mappings          nested by space indentation
//	block sequences         "- item", including sequences of mappings
//	flow collections        "[a, b]" and "{a: 1}", on one line
//	plain scalars           resolved as YAML 1.1: null, bool, int, float
//	quoted scalars          'single' with '' escaping, "double" with the full escape set
//	block scalars           "|", "|-", "|+", ">", ">-", ">+", with indentation indicators
//	multi-line plain        a value continued on the following, more-indented lines
//	comments                whole-line and trailing, never inside quotes
//	anchors and aliases     "&name", "*name", and the merge key "<<: *name"
//	document markers        a leading "---", a trailing "...", a BOM, CRLF line endings
//
// # Keys are always strings
//
// A key is never type-resolved, which is a deliberate departure from YAML 1.1
// and the single most important rule here. Under the usual resolution "no:"
// becomes the boolean false, which silently destroys the Norwegian locale, and
// "400:" becomes an integer, which is not the key an HTTP status message was
// written under. Both are ordinary keys to this parser.
//
// # Refused
//
// Each of these produces a [*SyntaxError] naming the line, the column and the
// construct, so that a file which will not load says why:
//
//	tabs used for indentation
//	explicit tags, such as "!!str" or "!ruby/symbol"
//	explicit key syntax, "? key"
//	more than one document in a file
//	non-scalar keys
//	an anchor on a key
//	a recursive alias
//	a directive other than "%YAML"
//
// # Complexity
//
// Parsing is a single forward pass over the lines of the document. Nesting is
// bounded at [MaxDepth] so that a hostile file cannot exhaust the stack, which
// matters because the fuzz target in this package reaches the same entry point
// an application does.
package yaml
