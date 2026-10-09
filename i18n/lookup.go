package i18n

import (
	"math"
	"strings"
)

// Key marks a value in a [Lookup.Default] chain as another translation key
// rather than as literal text.
//
// A default chain mixes keys with literal text, and the two are told apart by
// type rather than by convention:
//
//	Default: []any{i18n.Key("errors.messages.blank"), "is required"}
type Key string

// Group is a namespace of translations: every leaf under one key, each under
// its path relative to that key.
//
// A flat map is returned rather than a nested one because that is what Go code
// does something with: splitting a dotted path is a line, and rebuilding a tree
// from one is a walk.
type Group map[string]string

// Lookup is one translation request written out, for the calls that need more
// than a list of values.
//
// Every field has a zero value meaning "not given", so a Lookup that names only
// a key behaves exactly as the same key passed to [Store.T] would.
type Lookup struct {
	// Key is the translation to find.
	Key string
	// Scope is prepended to Key, so that a group of related lookups can name
	// the part they share once.
	Scope []string
	// Count selects a plural form and is interpolated as %{count}. It holds a
	// whole number; a count with a fraction, such as 1.5 kilometres, is given
	// to [Store.Translate] as its count argument, which takes any integer or
	// floating-point type and chooses the form by its [PluralOperands].
	Count *int
	// Vars are the values the translation interpolates.
	Vars map[string]any
	// Default holds what to fall back to when Key is missing, tried in order. A
	// string is used as it stands; a [Key] is looked up as another translation.
	Default []any
	// Deep asks for interpolation to reach inside a bulk lookup, which it does
	// not do by default because a namespace is usually wanted whole.
	Deep bool
	// Raise asks for a missing translation to be reported rather than rendered
	// as a marker, whatever the store's exception handler would otherwise do.
	Raise bool

	// count is the count a plural form is chosen by: the one [Store.Translate]
	// read from its arguments, which may have a fraction, or the one Count
	// holds, which [Store.Get] reads into it.
	count pluralCount
}

// pluralCount is a count as a lookup carries it.
//
// It is held by value rather than through a pointer, so that a count costs a
// lookup nothing to carry. Carrying one behind a pointer, as Count does, cost
// an allocation on every translated message that had a count.
type pluralCount struct {
	// operands are what a plural rule chooses by.
	operands PluralOperands
	// given is the count as the arguments held it, for an error to quote. It
	// is nil when the count came from Count, which an error quotes instead.
	given any
	// set reports whether there is a count at all.
	set bool
}

// countGiven returns the count as the caller gave it, for an error to quote.
func (l Lookup) countGiven() any {
	if l.Count != nil {
		return *l.Count
	}
	return l.count.given
}

// full joins the scope and the key into the dotted path a backend is asked for.
func (l Lookup) full(separator string) string {
	if len(l.Scope) == 0 {
		return normalizeKey(l.Key, separator)
	}
	parts := make([]string, 0, len(l.Scope)+1)
	for _, scope := range l.Scope {
		if scope != "" {
			parts = append(parts, normalizeKey(scope, separator))
		}
	}
	if l.Key != "" {
		parts = append(parts, normalizeKey(l.Key, separator))
	}
	return strings.Join(parts, ".")
}

// normalizeKey rewrites a caller's separator as the dot a backend is asked in.
//
// Backends receive dotted keys whatever the store's separator is, so that a
// backend of someone else's has one form to parse rather than a setting to read.
func normalizeKey(key, separator string) string {
	if separator == "" || separator == "." {
		return key
	}
	return strings.ReplaceAll(key, separator, ".")
}

// optionNames are the words the argument list spends on how a lookup is
// performed rather than on what it interpolates.
//
// Count is the interesting one: it is both an option and a value, because a
// plural form both selects on the number and prints it.
var optionNames = map[string]bool{
	"scope": true, "default": true, "count": true,
	"locale": true, "deep": true, "raise": true,
}

// pairs reads an alternating name and value list into a map, the way slog reads
// one.
//
// Go has no keyword arguments, and a translation takes an open set of values
// under names the translator chose. A pair list is the only shape that stays
// the same across translate, localize and every number helper; a typed option
// per function would need a parallel vocabulary for each.
func pairs(key string, args []any) (map[string]any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	vars := make(map[string]any, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		name, named := args[i].(string)
		if !named {
			return nil, &ArgumentError{Key: key, Index: i, Value: args[i]}
		}
		if i+1 >= len(args) {
			return nil, &ArgumentError{Key: key, Index: i}
		}
		vars[name] = args[i+1]
	}
	return vars, nil
}

// parseArgs reads an argument list into a Lookup, separating the words that
// say how to look up from the values that fill the result in.
func parseArgs(key string, args []any) (Lookup, string, error) {
	l := Lookup{Key: key}
	var locale string

	given, err := pairs(key, args)
	if err != nil {
		return l, "", err
	}

	for name, value := range given {
		if !optionNames[name] {
			if l.Vars == nil {
				l.Vars = make(map[string]any, len(given))
			}
			l.Vars[name] = value
			continue
		}
		switch name {
		case "scope":
			l.Scope = toScope(value)
		case "default":
			l.Default = toDefaults(value)
		case "locale":
			locale, _ = value.(string)
		case "deep":
			l.Deep, _ = value.(bool)
		case "raise":
			l.Raise, _ = value.(bool)
		case "count":
			operands, ok := PluralOperandsOf(value)
			if !ok {
				return l, "", &ArgumentError{Key: key, Index: -1, Value: value}
			}
			l.count = pluralCount{operands: operands, given: value, set: true}
			// A count is a value as well as an option: a plural form selects on
			// the number and then prints it.
			if l.Vars == nil {
				l.Vars = make(map[string]any, len(given))
			}
			l.Vars["count"] = value
		}
	}
	return l, locale, nil
}

// toScope reads the scope option, which may be written as one dotted string or
// as a list of segments.
func toScope(value any) []string {
	switch scope := value.(type) {
	case string:
		return []string{scope}
	case []string:
		return scope
	case []any:
		out := make([]string, 0, len(scope))
		for _, part := range scope {
			if text, isText := part.(string); isText {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

// toDefaults reads the default option, which may be one fallback or several
// tried in order.
func toDefaults(value any) []any {
	switch fallback := value.(type) {
	case []any:
		return fallback
	case []string:
		out := make([]any, len(fallback))
		for i, text := range fallback {
			out[i] = text
		}
		return out
	default:
		return []any{value}
	}
}

// toInt reads a whole number written as any of the numeric types a caller
// might have one in, such as the precision a number is formatted to. A
// fraction is dropped, which is why a count is read by [PluralOperandsOf]
// instead.
func toInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case uint:
		if n > math.MaxInt {
			return 0, false
		}
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		return int(n), true
	case uint64:
		// A count this large is not a number of anything a sentence counts, and
		// narrowing it would turn it into a different number entirely.
		if n > math.MaxInt {
			return 0, false
		}
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
