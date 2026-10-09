package tsgen

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// reserved are the names a generated identifier may not take: the words
// JavaScript and TypeScript reserve, or give a meaning to in a type position,
// and the globals the output itself refers to, which a declaration of the same
// name would shadow for the whole file.
var reserved = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`
		break case catch class const continue debugger default delete do else enum export extends
		false finally for function if import in instanceof new null return super switch this throw
		true try typeof var void while with implements interface let package private protected
		public static yield await abstract any as asserts async bigint boolean constructor declare
		get global infer is keyof module namespace never number object of out readonly require set
		string symbol type undefined unique unknown satisfies accessor from arguments eval
		Array ArrayBuffer Blob BodyInit Boolean Date Error File FormData Function Headers JSON Map
		Math Number Object Partial Promise Readonly ReadonlyArray Record Request RequestInit
		Response Set String Symbol TypeError URL URLSearchParams
		ApiError ClientOptions Operations createClient fetch toString valueOf hasOwnProperty
		isPrototypeOf propertyIsEnumerable toLocaleString prototype __proto__`) {
		reserved[word] = true
	}
}

// namer hands out identifiers no two of which are the same.
type namer struct {
	taken map[string]bool
	// next is, for each name already numbered, the number to try first the
	// next time it is asked for. Every number below it was taken when it was
	// passed, and a name once taken stays taken, so starting there finds the
	// same name as counting from 2 would. Counting from 2 every time made the
	// n-th of many names that reduce to one identifier cost n steps, which a
	// document of a few megabytes turned into minutes.
	next map[string]int
}

func newNamer() *namer { return &namer{taken: map[string]bool{}, next: map[string]int{}} }

// claim returns name, or name followed by the first number that makes it
// free, and takes it. A reserved word is never free.
func (n *namer) claim(name string) string {
	candidate := name
	i := max(n.next[name], 2)
	for ; n.taken[candidate] || reserved[candidate]; i++ {
		candidate = name + strconv.Itoa(i)
	}
	n.next[name] = i
	n.taken[candidate] = true
	return candidate
}

// claimGroup claims a base name every one of whose suffixed forms is free,
// and takes them all, so that an operation's four types share one base.
func (n *namer) claimGroup(base string, suffixes ...string) string {
	free := func(candidate string) bool {
		if n.taken[candidate] || reserved[candidate] {
			return false
		}
		for _, suffix := range suffixes {
			if n.taken[candidate+suffix] || reserved[candidate+suffix] {
				return false
			}
		}
		return true
	}
	// Numbered apart from claim's, since a number a group passed over may
	// still be free for a name of its own.
	key := base + "\x00" + strings.Join(suffixes, "\x00")
	candidate := base
	i := max(n.next[key], 2)
	for ; !free(candidate); i++ {
		candidate = base + strconv.Itoa(i)
	}
	n.next[key] = i
	n.taken[candidate] = true
	for _, suffix := range suffixes {
		n.taken[candidate+suffix] = true
	}
	return candidate
}

// pascal reduces a name from the document to an identifier in PascalCase:
// ASCII letters and digits are kept, and everything else separates words,
// each of which begins with a capital. A name with nothing left is fallback,
// and one that would begin with a digit is given an underscore in front.
func pascal(name, fallback string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			if upper {
				r -= 'a' - 'A'
			}
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			upper = true
			continue
		}
		b.WriteRune(r)
		upper = false
	}
	out := b.String()
	if out == "" {
		out = fallback
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

// camel is pascal with a lower-case first letter, for a method name.
func camel(identifier string) string {
	if identifier == "" || identifier[0] < 'A' || identifier[0] > 'Z' {
		return identifier
	}
	return string(identifier[0]+('a'-'A')) + identifier[1:]
}

// isIdentifierName reports whether a property name can be written bare, which
// is a name of ASCII letters, digits, "_" and "$" that does not begin with a
// digit. A reserved word is a valid property name, and stays bare.
func isIdentifierName(name string) bool {
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '$':
		default:
			return false
		}
	}
	return true
}

// propertyKey writes a member's name, bare where it can be and as a string
// literal otherwise.
func propertyKey(name string) string {
	if isIdentifierName(name) {
		return name
	}
	return quote(name)
}

// quote writes s as a TypeScript string literal in double quotes, holding
// only printable ASCII: a quote and a backslash are escaped, and every other
// character outside printable ASCII is written as a \u escape, a character
// beyond the basic plane as its surrogate pair. Nothing in s can end the
// literal or the line, U+2028 and U+2029 included, or reorder the text around
// it, and a byte that is not UTF-8 is written as U+FFFD.
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			// A byte that is not UTF-8 arrives here as utf8.RuneError, and
			// is written as one.
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// writeComment writes a documentation comment holding each non-empty entry
// of lines as a paragraph, at indent, or nothing when there is nothing to say.
func writeComment(b interface{ WriteString(string) (int, error) }, indent string, lines ...string) {
	var text []string
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			text = append(text, line)
		}
	}
	if len(text) == 0 {
		return
	}
	_, _ = b.WriteString(indent + "/**\n")
	writeCommentLines(b, indent, text...)
	_, _ = b.WriteString(indent + " */\n")
}

// writeCommentLines writes the inside of a block comment, one " * " line per
// line of text, with a blank one between paragraphs.
//
// Text from the document is made safe to sit inside the comment. It is split
// at every line terminator JavaScript knows, CR, LF, U+2028 and U+2029, and at
// NEL, so no line of it is ever written without the " * " in front. "*/",
// which would end the comment, is written "*\/". A control character, a
// Unicode control that reorders text or hides it, and a byte that is not
// UTF-8 are written as \u escapes, which a reader sees and an editor cannot
// draw as something else; any other character is written as itself.
func writeCommentLines(b interface{ WriteString(string) (int, error) }, indent string, paragraphs ...string) {
	first := true
	for _, paragraph := range paragraphs {
		if strings.TrimSpace(paragraph) == "" {
			continue
		}
		if !first {
			_, _ = b.WriteString(indent + " *\n")
		}
		first = false
		for line := range strings.FieldsFuncSeq(paragraph, isLineTerminator) {
			line = strings.TrimRight(safeComment(line), " \t")
			if line == "" {
				continue
			}
			_, _ = b.WriteString(indent + " * " + line + "\n")
		}
	}
}

// replacementEscape is how a byte that is not UTF-8 is written in a comment:
// the escape of U+FFFD, the replacement character, spelled out so that the
// source of this file stays ASCII.
const replacementEscape = `\` + "ufffd"

// isLineTerminator reports whether r ends a line of a comment.
func isLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 || r == 0x85
}

// safeComment escapes what may not appear in a comment as itself.
func safeComment(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteString(replacementEscape)
		case r == '*' && strings.HasPrefix(line[i+1:], "/"):
			b.WriteString(`*\`)
		case r == '\t' || r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r < 0xa0 || hidden(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// hidden reports whether a character changes how the text around it is
// drawn, or is not drawn at all: the bidirectional controls a "Trojan Source"
// comment hides code with, the zero-width characters, and the byte order mark.
func hidden(r rune) bool {
	switch {
	case r == 0x061c, r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e,
		r >= 0x2060 && r <= 0x2069, r == 0xfeff, r == 0xad:
		return true
	}
	return false
}
