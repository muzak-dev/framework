package muzak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Translator renders a message in a locale.
//
// It is the only thing the framework needs from a translation engine, and it is
// an interface rather than a concrete type for one reason: an application that
// does not translate should not carry one. Leave [I18nOptions.Store] nil and
// nothing in muzak.dev/framework/i18n is reachable from the root package, so a
// binary that never mentions it links neither the engine, nor the YAML parser,
// nor the table of plural rules.
//
// The Store type in muzak.dev/framework/i18n implements it:
//
//	//go:embed locales
//	var locales embed.FS
//
//	app := muzak.New(muzak.AppOptions{
//		I18n: muzak.I18nOptions{Store: i18n.MustLoad(locales, "locales")},
//	})
type Translator interface {
	// Translate returns the text a key names in a locale, with the arguments
	// interpolated into it. Arguments are alternating names and values.
	Translate(locale, key string, args ...any) string
	// Localize renders a value, such as a time or a number, the way the locale
	// writes it.
	Localize(locale string, value any, args ...any) string
	// Exists reports whether a key can be translated for a locale. The
	// framework asks before it renders, so that a message nobody has translated
	// falls back to the English the framework would have produced anyway rather
	// than to a "translation missing" marker in a response body.
	Exists(locale, key string) bool
	// AvailableLocales lists the locales that can be answered in, which is what
	// an inbound Accept-Language header is negotiated against.
	AvailableLocales() []string
	// DefaultLocale names the locale to answer in when a request asks for none
	// that can be served.
	DefaultLocale() string
}

// LocaleSource is one place a request's locale can be read from.
//
// The choice is usually written by hand in a filter, picking between a domain,
// a path segment, a parameter, a user's stored preference and the
// Accept-Language header. Here it is declared instead, so that it is visible in
// the application's options rather than buried in code that runs per request.
type LocaleSource uint8

const (
	// LocaleFromPath reads a segment of the request path, as "/pt/books".
	// [I18nOptions.PathIndex] says which segment.
	LocaleFromPath LocaleSource = iota + 1
	// LocaleFromQuery reads a query parameter, as "?locale=pt".
	LocaleFromQuery
	// LocaleFromHeader reads a request header named by [I18nOptions.Header].
	LocaleFromHeader
	// LocaleFromCookie reads a cookie named by [I18nOptions.Cookie].
	//
	// A locale in a cookie is invisible in the URL, so two people opening the
	// same link see different pages. Prefer a source that travels with the
	// address unless the application has a reason not to.
	LocaleFromCookie
	// LocaleFromAcceptLanguage negotiates the Accept-Language header against
	// the locales the application answers in, honouring the quality values the
	// client sent. It is the default, and the only source that needs nothing
	// configured and nothing added to a URL.
	LocaleFromAcceptLanguage
	// LocaleFromCustom reads the locale with [I18nOptions.Extractor], for
	// anything the sources above do not cover, such as a stored preference on
	// the authenticated user.
	LocaleFromCustom
)

// String names the source, for error messages.
func (s LocaleSource) String() string {
	switch s {
	case LocaleFromPath:
		return "the request path"
	case LocaleFromQuery:
		return "a query parameter"
	case LocaleFromHeader:
		return "a request header"
	case LocaleFromCookie:
		return "a cookie"
	case LocaleFromAcceptLanguage:
		return "the Accept-Language header"
	case LocaleFromCustom:
		return "a custom extractor"
	default:
		return "an unknown source"
	}
}

// LocaleExtractor pulls the locale or locales a request declares, for
// [LocaleFromCustom].
//
// Return them from most to least preferred. Returning nil reports a request
// that declares none, which moves on to the next source rather than failing.
type LocaleExtractor func(r *http.Request) []string

// HeaderContentLanguage names the response header that reports the locale a
// response was rendered in.
const HeaderContentLanguage = "Content-Language"

// LocaleKey is the log attribute the resolved locale is recorded under.
const LocaleKey = "locale"

// I18nOptions configures how a request's locale is decided and where the
// translations come from.
//
// The zero value leaves internationalization off entirely: no middleware is
// installed, no locale is resolved, and every message the framework produces
// reads exactly as it does without this feature existing.
type I18nOptions struct {
	// Store renders the messages. It is nil by default, which is
	// internationalization turned off.
	Store Translator

	// Sources lists where a locale is read from, in order. The first source
	// that yields a locale the application actually answers in wins. Left empty
	// with a Store set, it is [LocaleFromAcceptLanguage] alone.
	Sources []LocaleSource

	// Query names the query parameter carrying the locale, for
	// [LocaleFromQuery]. It defaults to "locale".
	Query string

	// Header names the request header carrying the locale, for
	// [LocaleFromHeader]. It defaults to "X-Locale".
	Header string

	// Cookie names the cookie carrying the locale, for [LocaleFromCookie]. It
	// defaults to "locale".
	Cookie string

	// PathIndex is the segment of the path holding the locale, counting from
	// zero, for [LocaleFromPath]. With "/pt/books" it is 0.
	PathIndex int

	// Extractor reads the locale for [LocaleFromCustom]. It is required for
	// that source.
	Extractor LocaleExtractor

	// DefaultLocale is answered in when a request asks for nothing that can be
	// served. Left unset, it is whatever the Store defaults to.
	DefaultLocale string

	// AvailableLocales lists the locales requests may be answered in. Left
	// empty, it is whatever the Store turns out to hold.
	//
	// Nothing a request says is used unless it matches this list, which is what
	// keeps a locale arriving from a client out of a filesystem path and out of
	// a response header.
	AvailableLocales []string

	// derived records that the locale settings above were filled in from the
	// store rather than written by the application. A disagreement between two
	// settings the application made itself is a mistake worth reporting; one
	// between a setting and a list the framework derived is not.
	derived bool

	// DisableVary stops a Vary header being added for the request inputs that
	// took part in choosing the locale.
	//
	// By default the response names every input the resolver actually read
	// before it settled: "Accept-Language" for [LocaleFromAcceptLanguage], the
	// configured header name for [LocaleFromHeader] and "Cookie" for
	// [LocaleFromCookie]. A source listed after the one that decided is not
	// read and so not named, and the path and query need nothing because they
	// are already part of the address a cache keys on. It is added because a
	// cache in front of the service that does not see it will serve one
	// language to a client that asked for another, and with a header or cookie
	// source the client choosing the language can be whoever reached the cache
	// first. Turn it off only when something else already varies the cache
	// key.
	//
	// The inputs of a [LocaleFromCustom] extractor are not known to the
	// framework, so an extractor that reads a header or a cookie should name it
	// in Vary itself.
	DisableVary bool
}

// enabled reports whether internationalization is configured at all.
func (o I18nOptions) enabled() bool { return o.Store != nil }

// withDefaults fills in the fields a source needs but an application did not
// bother to name.
func (o I18nOptions) withDefaults() I18nOptions {
	if !o.enabled() {
		return o
	}
	if len(o.Sources) == 0 {
		o.Sources = []LocaleSource{LocaleFromAcceptLanguage}
	}
	if o.Query == "" {
		o.Query = "locale"
	}
	if o.Header == "" {
		o.Header = "X-Locale"
	}
	if o.Cookie == "" {
		o.Cookie = "locale"
	}
	if o.DefaultLocale == "" {
		o.DefaultLocale = o.Store.DefaultLocale()
	}
	if len(o.AvailableLocales) == 0 {
		o.AvailableLocales = o.Store.AvailableLocales()
		o.derived = true
	}
	if o.derived && !slices.Contains(o.AvailableLocales, o.DefaultLocale) {
		// The default is answerable by definition: it is what a request that
		// matches nothing is served in. An application whose only locale file
		// is Spanish still answers in English, because the framework's own
		// messages fall back to the wording they are written in.
		o.AvailableLocales = append(slices.Clone(o.AvailableLocales), o.DefaultLocale)
	}
	return o
}

// validate reports an [I18nOptions] that cannot mean anything: a source missing
// the field it reads, or a locale named that the application does not answer in.
func (o I18nOptions) validate() error {
	if !o.enabled() {
		return nil
	}
	filled := o.withDefaults()

	var problems []error
	for _, source := range filled.Sources {
		switch source {
		case LocaleFromPath:
			if o.PathIndex < 0 {
				problems = append(problems, fmt.Errorf(
					"muzak: I18nOptions.PathIndex is %d; a path segment is counted from zero", o.PathIndex))
			}
		case LocaleFromCustom:
			if o.Extractor == nil {
				problems = append(problems, errors.New(
					"muzak: I18nOptions.Sources names LocaleFromCustom but I18nOptions.Extractor is not set"))
			}
		case LocaleFromQuery, LocaleFromHeader, LocaleFromCookie, LocaleFromAcceptLanguage:
		default:
			problems = append(problems, fmt.Errorf(
				"muzak: I18nOptions.Sources names %d, which is not a locale source", source))
		}
	}

	// Only a disagreement between two settings the application made itself is a
	// mistake. A default missing from a list the framework derived is not: the
	// list is what the locale files happened to hold, and the default is added
	// to it above.
	if !filled.derived && !slices.Contains(filled.AvailableLocales, filled.DefaultLocale) {
		problems = append(problems, fmt.Errorf(
			"muzak: I18nOptions.DefaultLocale is %q, which is not among the available locales %s",
			filled.DefaultLocale, strings.Join(o.AvailableLocales, ", ")))
	}

	return errors.Join(problems...)
}

// localeContextKey is the unexported type the resolved locale is stored under,
// so that nothing outside this package can collide with it.
type localeContextKey struct{}

// LocaleFromContext returns the locale resolved for the request carried by ctx,
// and reports whether one was resolved.
//
// Use it where there is a context.Context but no Muzak [Context]: a repository,
// a client wrapper, or a goroutine started from a handler. Inside a handler,
// [Context.Locale] is the same value without the type assertion.
func LocaleFromContext(ctx context.Context) (string, bool) {
	locale, ok := ctx.Value(localeContextKey{}).(string)
	return locale, ok
}

// contextWithLocale attaches a resolved locale to a request context.
func contextWithLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, localeContextKey{}, locale)
}

// Locale resolves the locale for every request, records it in the request
// context, and reports it in the Content-Language response header.
//
// It is installed automatically when [AppOptions.I18n] names a store, between
// panic recovery and the access log. That position is not arbitrary: a custom
// extractor is application code and can panic, so this has to run inside
// Recovery; and the access log can only record a locale that was resolved
// before it, because middleware further out holds the request as it arrived.
func Locale(opts I18nOptions) Middleware {
	opts = opts.withDefaults()
	// The available locales and what the response varies on are settled once,
	// when the chain is built, rather than per request.
	available := opts.AvailableLocales
	var vary []localeVary
	if !opts.DisableVary {
		vary = localeVaryPrefixes(opts)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			locale, consulted := resolveLocale(r, opts, available)
			if consulted > 0 && len(vary) > 0 {
				// Declared to the writer instead of added to the header, so
				// that it composes with the Vary the compression middleware
				// writes for Accept-Encoding and survives a handler that sets
				// a Vary of its own; see [responseWriter].
				rw := asResponseWriter(w)
				defer rw.commitVary()
				w = rw
				rw.varyOn(vary[consulted-1].fields...)
			}
			if locale != "" {
				w.Header().Set(HeaderContentLanguage, locale)
			}
			next.ServeHTTP(w, r.WithContext(contextWithLocale(r.Context(), locale)))
		})
	}
}

// resolveLocale walks the configured sources and returns the first locale the
// application actually answers in, or the default when none of them yields one.
//
// Nothing a request carries is used unless it matches the available list. That
// single rule is what stops "?locale=../../etc/passwd" reaching a filesystem
// path and a locale carrying a line break reaching a response header: the value
// returned here is always one the application chose, never one a client sent.
//
// It also returns how many sources it read, counting the one that decided,
// because that is exactly the set of request inputs the answer depends on and
// so the set a shared cache has to key on.
func resolveLocale(r *http.Request, opts I18nOptions, available []string) (string, int) {
	for i, source := range opts.Sources {
		var candidates []string
		switch source {
		case LocaleFromPath:
			candidates = pathLocale(r.URL.Path, opts.PathIndex)
		case LocaleFromQuery:
			candidates = presentValue(r.URL.Query().Get(opts.Query))
		case LocaleFromHeader:
			candidates = presentValue(r.Header.Get(opts.Header))
		case LocaleFromCookie:
			if cookie, err := r.Cookie(opts.Cookie); err == nil {
				candidates = presentValue(cookie.Value)
			}
		case LocaleFromAcceptLanguage:
			if matched := negotiateLanguage(r.Header.Get("Accept-Language"), available); matched != "" {
				return matched, i + 1
			}
		case LocaleFromCustom:
			candidates = opts.Extractor(r)
		}
		for _, candidate := range candidates {
			if matched := matchLocale(candidate, available); matched != "" {
				return matched, i + 1
			}
		}
	}
	return opts.DefaultLocale, len(opts.Sources)
}

// localeVary is the Vary a response carries when the locale resolver read a
// given number of sources: the request headers those sources depend on.
type localeVary struct {
	fields []string
}

// localeVaryPrefixes computes, for each count of sources read, the Vary that
// count implies. Entry i covers the first i+1 sources. Doing this once when
// the middleware is built keeps the per-request cost to one header write.
// Names are deduplicated case-insensitively, so a header source configured as
// "Accept-Language" next to [LocaleFromAcceptLanguage], or two sources that
// both depend on cookies, name the field once.
func localeVaryPrefixes(opts I18nOptions) []localeVary {
	out := make([]localeVary, len(opts.Sources))
	var fields []string
	for i, source := range opts.Sources {
		var field string
		switch source {
		case LocaleFromHeader:
			field = http.CanonicalHeaderKey(opts.Header)
		case LocaleFromCookie:
			field = "Cookie"
		case LocaleFromAcceptLanguage:
			field = "Accept-Language"
		}
		if field != "" && !slices.ContainsFunc(fields, func(f string) bool { return strings.EqualFold(f, field) }) {
			fields = append(fields, field)
		}
		// Clip so that a later append never writes into an earlier entry's
		// backing array.
		out[i] = localeVary{fields: slices.Clip(fields)}
	}
	return out
}

// varyNames reports whether a Vary header already covers a field, either by
// naming it or with "*", which varies on everything.
func varyNames(values []string, field string) bool {
	for _, value := range values {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.TrimSpace(name)
			if name == "*" || strings.EqualFold(name, field) {
				return true
			}
		}
	}
	return false
}

// presentValue wraps a single value, dropping it when it is empty.
func presentValue(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

// pathLocale returns the requested segment of a path, if there is one.
func pathLocale(path string, index int) []string {
	for segment := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if index == 0 {
			return presentValue(segment)
		}
		index--
	}
	return nil
}

// matchLocale returns the available locale a candidate names, or the empty
// string when it names none.
//
// Matching is case-insensitive, and a candidate naming a region matches the
// plain language when that is what the application has, so a client asking for
// "pt-BR" is served "pt" rather than the default.
func matchLocale(candidate string, available []string) string {
	if candidate == "" {
		return ""
	}
	lower := strings.ToLower(candidate)
	for _, locale := range available {
		if strings.EqualFold(locale, lower) {
			return locale
		}
	}
	if language, _, regional := strings.Cut(lower, "-"); regional {
		for _, locale := range available {
			if strings.EqualFold(locale, language) {
				return locale
			}
		}
	}
	return ""
}

// The bounds on an Accept-Language header. A real one names a handful of
// languages; anything beyond this is a client wasting the server's time, and
// the scan stops rather than obliging it.
const (
	maxAcceptLanguageBytes  = 8 << 10
	maxAcceptLanguageRanges = 64
)

// languageRange is one entry of an Accept-Language header: the language it
// names and how much the client wants it.
type languageRange struct {
	tag     string
	quality float64
}

// negotiateLanguage picks the best locale for an Accept-Language header among
// the ones the application answers in, or the empty string when the client
// accepts none of them.
//
// The client's own quality values order the candidates, and a range matches a
// locale by its language as well as in full, so a client asking for "pt-BR" is
// served "pt" when that is what the application has. A range with a quality of
// zero refuses that language outright, so "en;q=0" is not answered in English
// even when English is all there is.
func negotiateLanguage(header string, available []string) string {
	if header == "" || len(available) == 0 {
		return ""
	}
	if len(header) > maxAcceptLanguageBytes {
		header = header[:maxAcceptLanguageBytes]
	}

	var ranges []languageRange
	refused := map[string]bool{}
	for entry := range strings.SplitSeq(header, ",") {
		if len(ranges) >= maxAcceptLanguageRanges {
			break
		}
		tag, parameters, _ := strings.Cut(strings.TrimSpace(entry), ";")
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		q := quality(parameters)
		if q <= 0 {
			refused[tag] = true
		}
		ranges = append(ranges, languageRange{tag: tag, quality: q})
	}

	// Stable, so that two languages the client wants equally are tried in the
	// order the client listed them.
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].quality > ranges[j].quality })

	for _, want := range ranges {
		if want.quality <= 0 {
			// Sorted by quality, so everything from here down is refused.
			break
		}
		for _, locale := range available {
			if languageMatches(want.tag, locale) && !languageRefused(locale, refused) {
				return locale
			}
		}
	}
	return ""
}

// languageMatches reports whether a range from the header names a locale.
func languageMatches(tag, locale string) bool {
	if tag == "*" {
		return true
	}
	lower := strings.ToLower(locale)
	if tag == lower {
		return true
	}
	if language, _, regional := strings.Cut(lower, "-"); regional && tag == language {
		return true
	}
	if language, _, regional := strings.Cut(tag, "-"); regional && language == lower {
		return true
	}
	return false
}

// languageRefused reports whether the client explicitly rejected a locale, by
// name or by its language.
func languageRefused(locale string, refused map[string]bool) bool {
	lower := strings.ToLower(locale)
	if refused[lower] {
		return true
	}
	language, _, regional := strings.Cut(lower, "-")
	return regional && refused[language]
}

// quality returns the q-value of an Accept header parameter list, defaulting to
// one when none is given and when the one given cannot be read.
//
// It is the graded form of [refused], which only has to know whether a value is
// zero. Both live here so that the compression middleware and locale
// negotiation parse the same header syntax the same way.
func quality(parameters string) float64 {
	for parameter := range strings.SplitSeq(parameters, ";") {
		key, value, found := strings.Cut(parameter, "=")
		if !found || strings.ToLower(strings.TrimSpace(key)) != "q" {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 1
		}
		return q
	}
	return 1
}
