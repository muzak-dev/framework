package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// printable escapes what a terminal would act on rather than print: control
// characters, which include the escape that starts an ANSI sequence, the tab,
// form feed and line feed that would break a table's columns and rows, the
// Unicode formatting characters that reorder or hide text, invalid UTF-8 and
// any other rune Unicode does not call printable. Each becomes the Go escape
// that names it, so what was there stays readable without being obeyed.
//
// It is linear in the length of s, and the result is at most four bytes for
// each byte of it: a control character or a byte that is not UTF-8 becomes
// four, as \x1b, and every longer escape stands for a longer rune.
func printable(s string) string {
	clean := true
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case !unicode.IsPrint(r):
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}
