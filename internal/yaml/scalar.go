package yaml

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// resolvePlain gives a plain scalar the type YAML 1.1 says it has.
//
// Only values reach this function. A key is always kept as a string, which is
// what keeps "no:" the Norwegian locale rather than the boolean false, and
// "400:" the key an HTTP status message was written under rather than an
// integer nothing looks up.
func resolvePlain(s string) any {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return nil
	case "true", "True", "TRUE", "yes", "Yes", "YES", "on", "On", "ON":
		return true
	case "false", "False", "FALSE", "no", "No", "NO", "off", "Off", "OFF":
		return false
	case ".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF":
		return math.Inf(1)
	case "-.inf", "-.Inf", "-.INF":
		return math.Inf(-1)
	case ".nan", ".NaN", ".NAN":
		return math.NaN()
	}
	if !looksNumeric(s) {
		return s
	}
	digits := strings.ReplaceAll(s, "_", "")
	if n, ok := parseInt(digits); ok {
		return n
	}
	if f, err := strconv.ParseFloat(digits, 64); err == nil {
		return f
	}
	return s
}

// looksNumeric reports whether a scalar could be a number at all, so that a
// word is never handed to a numeric parser that might accept it. Go's
// ParseFloat reads "inf" and "NaN" as numbers, and a translation may well say
// either.
func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case '+', '-', '.', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	default:
		return false
	}
}

// parseInt reads the integer forms YAML writes, including the base prefixes.
//
// A leading zero is deliberately not read as octal. YAML 1.1 says it is, YAML
// 1.2 says it is not, and a locale file that writes "0644" means six hundred
// and forty four every time.
func parseInt(s string) (int64, bool) {
	base := 10
	body := s
	sign := int64(1)
	if body != "" && (body[0] == '+' || body[0] == '-') {
		if body[0] == '-' {
			sign = -1
		}
		body = body[1:]
	}
	switch {
	case strings.HasPrefix(body, "0x"), strings.HasPrefix(body, "0X"):
		base, body = 16, body[2:]
	case strings.HasPrefix(body, "0o"), strings.HasPrefix(body, "0O"):
		base, body = 8, body[2:]
	case strings.HasPrefix(body, "0b"), strings.HasPrefix(body, "0B"):
		base, body = 2, body[2:]
	}
	if body == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(body, base, 64)
	if err != nil {
		return 0, false
	}
	return sign * n, true
}

// unquoteSingle reads a single-quoted scalar, in which the only escape is a
// doubled quote. It returns the text and the number of bytes it consumed.
func unquoteSingle(s, file string, at line, col int) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return b.String(), i + 1, nil
	}
	return "", 0, errAt(file, at, col, "a single-quoted value is never closed")
}

// escapes are the one-character escape sequences a double-quoted scalar may
// use. They are a table rather than a switch because the set is data: every
// entry is one sequence YAML defines, and nothing is decided about any of them.
//
// The last four are written as code points rather than as characters because
// every source file in this module is plain ASCII, and a next line or a
// non-breaking space typed literally is exactly the invisible character that
// rule exists to keep out.
var escapes = map[byte]string{
	'0': "\x00", 'a': "\a", 'b': "\b", 't': "\t", '\t': "\t",
	'n': "\n", 'v': "\v", 'f': "\f", 'r': "\r", 'e': "\x1b",
	' ': " ", '"': "\"", '/': "/", '\\': "\\",
	'N': string(rune(0x0085)), '_': string(rune(0x00A0)),
	'L': string(rune(0x2028)), 'P': string(rune(0x2029)),
}

// escapeWidths are the escapes that introduce a fixed run of hexadecimal
// digits, and how many digits each takes.
var escapeWidths = map[byte]int{'x': 2, 'u': 4, 'U': 8}

// unquoteDouble reads a double-quoted scalar and its escape sequences.
//
// The numeric escapes matter beyond correctness: they are what lets a test
// fixture in this repository produce non-ASCII output from source that is
// itself plain ASCII.
func unquoteDouble(s, file string, at line, col int) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return b.String(), i + 1, nil
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		if text, known := escapes[s[i]]; known {
			b.WriteString(text)
			continue
		}
		width, numeric := escapeWidths[s[i]]
		if !numeric {
			return "", 0, errAt(file, at, col+i, "a double-quoted value uses an escape this parser does not know")
		}
		if i+width >= len(s) {
			return "", 0, errAt(file, at, col+i, "an escape names more digits than the value holds")
		}
		n, err := strconv.ParseUint(s[i+1:i+1+width], 16, 32)
		if err != nil {
			return "", 0, errAt(file, at, col+i, "an escape names something that is not a hexadecimal number")
		}
		// The bound is checked before the conversion rather than after it,
		// because a value above the last code point does not survive being
		// narrowed to a rune intact and so cannot be judged afterwards.
		if n > utf8.MaxRune {
			return "", 0, errAt(file, at, col+i, "an escape names a value that is not a character")
		}
		r := rune(n)
		if !utf8.ValidRune(r) {
			return "", 0, errAt(file, at, col+i, "an escape names a value that is not a character")
		}
		b.WriteRune(r)
		i += width
	}
	return "", 0, errAt(file, at, col, "a double-quoted value is never closed")
}

// stripComment removes a trailing comment from a line of content.
//
// A '#' opens a comment only when it begins the line or follows a space, and
// never inside quotes. A hash joined to the text before it, as in "colour#1",
// is part of the value, which is the rule YAML states and the one a locale
// file relies on when it writes a fragment identifier.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case quote == '"':
			switch c {
			case '\\':
				i++
			case '"':
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}

// splitKey finds the ":" that separates a mapping key from its value.
//
// The separator is a colon followed by a space or ending the line, which is
// what lets a plain value hold a colon of its own, as "http://muzak.dev" does.
// A key written in quotes is reported so that the caller reads it as a quoted
// scalar rather than as text.
func splitKey(s string) (key, rest string, quoted, ok bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"':
			end := skipQuoted(s, i)
			if end < 0 {
				return "", "", false, false
			}
			quoted = i == 0
			i = end - 1
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case ':':
			if depth > 0 {
				continue
			}
			if i+1 == len(s) {
				return s[:i], "", quoted, true
			}
			if s[i+1] == ' ' || s[i+1] == '\t' {
				return s[:i], strings.TrimLeft(s[i+1:], " \t"), quoted, true
			}
		}
	}
	return "", "", false, false
}

// skipQuoted returns the index just past a quoted scalar starting at i, or -1
// when it is never closed.
func skipQuoted(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch {
		case q == '"' && s[j] == '\\':
			j++
		case s[j] != q:
		case q == '\'' && j+1 < len(s) && s[j+1] == '\'':
			j++
		default:
			return j + 1
		}
	}
	return -1
}
