package i18n

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

// reservedNames are the words the argument list spends on options rather than
// on values, and which a translation therefore cannot interpolate.
//
// They are reserved because a translation that could name them would be able to
// change how it was looked up.
var reservedNames = map[string]bool{"scope": true, "default": true}

// part is one piece of a compiled translation.
//
// A translation is scanned into parts once, when it is loaded, so that filling
// one in at request time is an append loop rather than a parse. Almost every
// message the framework ships holds no placeholder at all, and those compile to
// nothing: rendering them copies no bytes and allocates nothing.
type part struct {
	// literal is text emitted as written. A part is a literal when name is
	// empty.
	literal string
	// name is the value to interpolate.
	name string
	// verb is the format directive for the "%<name>d" form, including its
	// leading percent. It is empty for the plain "%{name}" form.
	verb string
}

// compile scans a translation into parts, returning nil when it holds nothing
// to interpolate.
//
// Nil is the point of the function. It is what tells the renderer that the text
// can be handed back untouched, which is the path almost every lookup takes.
func compile(text string) []part {
	if !needsCompiling(text) {
		return nil
	}

	var parts []part
	literal := 0
	flush := func(upto int) {
		if upto > literal {
			parts = append(parts, part{literal: text[literal:upto]})
		}
	}

	for i := 0; i < len(text); i++ {
		if text[i] != '%' || i+1 >= len(text) {
			continue
		}
		switch text[i+1] {
		case '%':
			flush(i)
			parts = append(parts, part{literal: "%"})
			i++
			literal = i + 1
		case '{':
			end := indexByte(text, '}', i+2)
			if end < 0 {
				continue
			}
			flush(i)
			parts = append(parts, part{name: text[i+2 : end]})
			i = end
			literal = i + 1
		case '<':
			end := indexByte(text, '>', i+2)
			if end < 0 {
				continue
			}
			verbEnd := endOfVerb(text, end+1)
			if verbEnd < 0 || !boundedVerb(text[end+1:verbEnd]) {
				continue
			}
			flush(i)
			parts = append(parts, part{name: text[i+2 : end], verb: "%" + text[end+1:verbEnd]})
			i = verbEnd - 1
			literal = i + 1
		}
	}
	flush(len(text))
	return parts
}

// needsCompiling reports whether a translation holds anything a renderer would
// have to act on. A strftime pattern such as "%Y-%m-%d" does not: every one of
// its directives is literal text as far as interpolation is concerned.
func needsCompiling(text string) bool {
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '%' {
			continue
		}
		switch text[i+1] {
		case '%', '{', '<':
			return true
		}
	}
	return false
}

// indexByte finds c at or after start, or reports -1.
func indexByte(s string, c byte, start int) int {
	for i := start; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// endOfVerb finds the end of a format directive, which is its first letter.
func endOfVerb(s string, start int) int {
	for i := start; i < len(s); i++ {
		if (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') {
			return i + 1
		}
	}
	return -1
}

// maxVerbWidth is the widest field or precision a "%<name>d" directive may ask
// for.
//
// The directive is handed to fmt as written, and fmt pads to whatever width it
// is given: "%<n>1000000d" is twelve bytes and renders a megabyte, so a
// translation of a hundred of them renders a hundred megabytes, and the pooled
// buffer that built it then keeps that capacity. Nothing a message says needs
// more than a column or two.
const maxVerbWidth = 64

// boundedVerb reports whether the directive between a placeholder's name and
// its end is one that renders a bounded amount of text: flags, then a width,
// then a precision, each within [maxVerbWidth], then the verb letter. A "*" is
// not accepted where a width goes, because it takes its width from an argument
// the placeholder does not supply. A directive that is not accepted leaves the
// whole placeholder as literal text, the way an unterminated one is left.
func boundedVerb(spec string) bool {
	if spec == "" {
		return false
	}
	// Whatever precedes the verb letter is flags, a width, and a precision.
	width, precision, dotted := strings.Cut(strings.TrimLeft(spec[:len(spec)-1], "+-# 0"), ".")
	return boundedNumber(width) && (!dotted || boundedNumber(precision))
}

// boundedNumber reports whether a width or precision is empty, or digits that
// total no more than [maxVerbWidth].
func boundedNumber(digits string) bool {
	n := 0
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
		if n = n*10 + int(digits[i]-'0'); n > maxVerbWidth {
			return false
		}
	}
	return true
}

// buffers recycles the byte slices a rendered translation is built in.
//
// A message that interpolates anything is built rather than returned, and the
// framework renders one per rejected field on a failed request. Recycling the
// buffer keeps that from being a per-field allocation.
var buffers = sync.Pool{New: func() any { b := make([]byte, 0, 256); return &b }}

// Interpolate fills the placeholders in a translation from a list of
// alternating names and values.
//
//	i18n.Interpolate("must be at least %{count} characters", "count", 12)
//
// It is exported because a message assembled outside a locale file still has to
// be filled in the same way, and because it is the unit worth testing directly.
func Interpolate(text string, args ...any) (string, error) {
	vars, err := pairs(text, args)
	if err != nil {
		return "", err
	}
	return render(compile(text), text, vars, "", "")
}

// render fills a compiled translation.
//
// Nil parts mean the text holds nothing to interpolate, which is returned as it
// stands: no copy, no allocation, no work.
func render(parts []part, text string, vars map[string]any, locale, key string) (string, error) {
	if parts == nil {
		return text, nil
	}

	buf := buffers.Get().(*[]byte)
	defer func() {
		*buf = (*buf)[:0]
		buffers.Put(buf)
	}()
	b := (*buf)[:0]

	for _, p := range parts {
		if p.name == "" {
			b = append(b, p.literal...)
			continue
		}
		if reservedNames[p.name] {
			return "", &ReservedInterpolationKeyError{Locale: locale, Key: key, Placeholder: p.name}
		}
		value, given := vars[p.name]
		if !given {
			return "", &MissingInterpolationArgumentError{
				Locale: locale, Key: key, Placeholder: p.name, Text: text,
			}
		}
		b = appendValue(b, value, p.verb)
	}

	*buf = b
	return string(b), nil
}

// appendValue writes one interpolated value.
//
// The types a translation actually interpolates are written out rather than
// handed to fmt, because reflection on a count is the difference between a
// rejected field costing one allocation and costing three. Anything unusual,
// and anything with an explicit directive, still goes through fmt.
func appendValue(b []byte, value any, verb string) []byte {
	if verb != "" {
		return fmt.Appendf(b, verb, value)
	}
	switch v := value.(type) {
	case string:
		return append(b, v...)
	case int:
		return strconv.AppendInt(b, int64(v), 10)
	case int64:
		return strconv.AppendInt(b, v, 10)
	case int32:
		return strconv.AppendInt(b, int64(v), 10)
	case uint:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint64:
		return strconv.AppendUint(b, v, 10)
	case float64:
		// A bound of ten reads as "10" rather than as "1e+01" or "10.000000".
		// The threshold is where a float64 stops representing every integer, so
		// beyond it the exponent form is the honest one.
		if v == math.Trunc(v) && math.Abs(v) < 1e15 {
			return strconv.AppendInt(b, int64(v), 10)
		}
		return strconv.AppendFloat(b, v, 'g', -1, 64)
	case bool:
		return strconv.AppendBool(b, v)
	default:
		return fmt.Appendf(b, "%v", value)
	}
}
