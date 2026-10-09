package i18n

import (
	"errors"
	"fmt"
	"strings"
)

// The failures a lookup can produce. Each is a sentinel so that a caller which
// only needs to know what kind of thing went wrong can ask with errors.Is,
// while the typed error beneath carries the locale, the key and whatever else
// is needed to fix it.
var (
	// ErrMissingTranslation reports that no translation was found for a key,
	// in the locale asked for or in any locale behind it.
	ErrMissingTranslation = errors.New("i18n: no translation for the key")
	// ErrInvalidLocale reports a locale the store does not answer in.
	ErrInvalidLocale = errors.New("i18n: the locale is not one this store answers in")
	// ErrInvalidPluralizationData reports an entry that cannot be pluralized
	// the way the call asked for.
	ErrInvalidPluralizationData = errors.New("i18n: the entry is not a usable set of plural forms")
	// ErrMissingInterpolationArgument reports a translation that names a value
	// the call did not supply.
	ErrMissingInterpolationArgument = errors.New("i18n: the translation expects a value that was not given")
	// ErrReservedInterpolationKey reports a translation that names one of the
	// words the argument list uses for something else.
	ErrReservedInterpolationKey = errors.New("i18n: the translation names a reserved interpolation key")
	// ErrUnknownFileType reports a locale file this package has no loader for.
	ErrUnknownFileType = errors.New("i18n: the file type has no loader")
	// ErrMalformedArguments reports an argument list that is not alternating
	// names and values.
	ErrMalformedArguments = errors.New("i18n: the argument list is not alternating names and values")
)

// MissingTranslationError reports a key nothing translated.
//
// It lists every locale that was tried, because the usual cause is not that the
// key is absent but that the locale chain did not reach the file holding it.
type MissingTranslationError struct {
	// Locale is the locale the lookup asked for.
	Locale string
	// Key is the full key, with any scope already joined onto it.
	Key string
	// Tried lists the locales consulted, in the order they were consulted.
	Tried []string
}

// Error renders the failure with the locale chain that came up empty.
func (e *MissingTranslationError) Error() string {
	return fmt.Sprintf("i18n: no translation for %q in %s", e.Key, strings.Join(e.Tried, ", "))
}

// Unwrap reports [ErrMissingTranslation].
func (e *MissingTranslationError) Unwrap() error { return ErrMissingTranslation }

// Marker is the text a missing translation renders as by default.
//
// It is deliberately conspicuous rather than empty: a blank space in a page
// looks like a design decision, where "translation missing: fr.store.title"
// names the key that has to be added and the file to add it to.
func (e *MissingTranslationError) Marker() string {
	return "translation missing: " + e.Locale + "." + e.Key
}

// InvalidLocaleError reports a locale the store was told not to answer in.
type InvalidLocaleError struct {
	// Locale is the locale that was asked for.
	Locale string
	// Available lists the locales the store does answer in.
	Available []string
}

// Error renders the failure with the locales that would have worked.
func (e *InvalidLocaleError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("i18n: %q is not a locale this store answers in", e.Locale)
	}
	return fmt.Sprintf("i18n: %q is not a locale this store answers in; it answers in %s",
		e.Locale, strings.Join(e.Available, ", "))
}

// Unwrap reports [ErrInvalidLocale].
func (e *InvalidLocaleError) Unwrap() error { return ErrInvalidLocale }

// InvalidPluralizationDataError reports an entry that a count cannot select a
// form from.
//
// It happens two ways: a count was given for an entry that is a single string
// broken into no forms at all, or the locale's rule chose a category the entry
// does not define and there is no "other" to fall back to.
type InvalidPluralizationDataError struct {
	// Locale and Key name the entry.
	Locale, Key string
	// Count is the number that was being pluralized, as the call gave it: an
	// int when it came from [Lookup.Count], and whichever integer or
	// floating-point type was passed to [Store.Translate] otherwise.
	Count any
	// Category is the form the locale's rule selected.
	Category PluralCategory
	// Have lists the forms the entry actually defines.
	Have []PluralCategory
}

// Error renders the failure with the form that was wanted and the ones present.
func (e *InvalidPluralizationDataError) Error() string {
	if len(e.Have) == 0 {
		return fmt.Sprintf("i18n: %q in %s is one string rather than a set of plural forms, so a count of %v cannot choose between them",
			e.Key, e.Locale, e.Count)
	}
	have := make([]string, len(e.Have))
	for i, category := range e.Have {
		have[i] = string(category)
	}
	return fmt.Sprintf("i18n: %q in %s has no %q form for a count of %v; it defines %s",
		e.Key, e.Locale, e.Category, e.Count, strings.Join(have, ", "))
}

// Unwrap reports [ErrInvalidPluralizationData].
func (e *InvalidPluralizationDataError) Unwrap() error { return ErrInvalidPluralizationData }

// MissingInterpolationArgumentError reports a translation that names a value
// the call did not supply.
type MissingInterpolationArgumentError struct {
	// Locale and Key name the translation.
	Locale, Key string
	// Placeholder is the name the translation used.
	Placeholder string
	// Text is the translation as written, so that the placeholder can be seen
	// in the sentence it belongs to.
	Text string
}

// Error renders the failure with the value that was wanted.
func (e *MissingInterpolationArgumentError) Error() string {
	return fmt.Sprintf("i18n: %q in %s expects %q, which was not given: %q",
		e.Key, e.Locale, e.Placeholder, e.Text)
}

// Unwrap reports [ErrMissingInterpolationArgument].
func (e *MissingInterpolationArgumentError) Unwrap() error {
	return ErrMissingInterpolationArgument
}

// ReservedInterpolationKeyError reports a translation naming one of the words
// the argument list uses for something other than a value.
type ReservedInterpolationKeyError struct {
	// Locale and Key name the translation.
	Locale, Key string
	// Placeholder is the reserved name the translation used.
	Placeholder string
}

// Error renders the failure with the name that cannot be interpolated.
func (e *ReservedInterpolationKeyError) Error() string {
	return fmt.Sprintf("i18n: %q in %s names %q, which the argument list reserves for an option rather than a value",
		e.Key, e.Locale, e.Placeholder)
}

// Unwrap reports [ErrReservedInterpolationKey].
func (e *ReservedInterpolationKeyError) Unwrap() error { return ErrReservedInterpolationKey }

// UnknownFileTypeError reports a file in the load path that no loader reads.
type UnknownFileTypeError struct {
	// Name is the file that could not be loaded.
	Name string
	// Ext is its extension, which is what the loader is chosen by.
	Ext string
}

// Error renders the failure with the extensions that would have worked.
func (e *UnknownFileTypeError) Error() string {
	return fmt.Sprintf("i18n: %s has the extension %q; locale files are .yml, .yaml or .json", e.Name, e.Ext)
}

// Unwrap reports [ErrUnknownFileType].
func (e *UnknownFileTypeError) Unwrap() error { return ErrUnknownFileType }

// ArgumentError reports an argument list that is not alternating names and
// values.
//
// The list is read the way slog reads one, so the mistakes are the same two:
// an odd number of arguments, or a name that is not a string.
type ArgumentError struct {
	// Key names the translation the arguments were given to.
	Key string
	// Index is the position in the argument list the problem was found at, or
	// -1 when the problem is the value given as the count.
	Index int
	// Value is what was found where a name was expected, or nil when the list
	// simply ran out. When Index is -1 it is the count that was refused.
	Value any
}

// Error renders the failure with the position that is wrong.
func (e *ArgumentError) Error() string {
	if e.Index < 0 {
		return fmt.Sprintf("i18n: the count given to %q is %v, which is not a number a plural form can be chosen by; "+
			"give an integer or a finite float below 2^63 in magnitude", e.Key, e.Value)
	}
	if e.Value == nil {
		return fmt.Sprintf("i18n: the arguments to %q end with a name that has no value", e.Key)
	}
	return fmt.Sprintf("i18n: argument %d to %q is %v, where a name was expected; arguments alternate name and value",
		e.Index, e.Key, e.Value)
}

// Unwrap reports [ErrMalformedArguments].
func (e *ArgumentError) Unwrap() error { return ErrMalformedArguments }

// ExceptionHandler decides what a failed lookup produces.
//
// It receives the failure, the locale and the key, and returns the text to use
// in place of the translation together with the error to report to the caller.
// Returning a nil error swallows the failure, which is what
// [DefaultExceptionHandler] does for a missing translation: one key nobody has
// translated yet should leave a mark in the page, not take the response down.
//
// Install a different one through [StoreOptions.ExceptionHandler].
type ExceptionHandler func(err error, locale, key string) (string, error)

// DefaultExceptionHandler renders a missing translation as a visible marker and
// reports every other failure.
//
// The asymmetry is deliberate. A missing translation is a gap in the content,
// which the person filling it needs to see; a reserved key or an absent
// interpolation value is a mistake in the code, which the person who wrote it
// needs told about.
func DefaultExceptionHandler(err error, _, _ string) (string, error) {
	var missing *MissingTranslationError
	if errors.As(err, &missing) {
		return missing.Marker(), nil
	}
	return "", err
}

// StrictExceptionHandler reports every failure, including a missing
// translation.
//
// Install it in a test suite: a key nobody translated then fails a test rather
// than reaching a client as a marker.
func StrictExceptionHandler(err error, _, _ string) (string, error) {
	return "", err
}
