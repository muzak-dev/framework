package main

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// slashed turns each "%" of s into a backslash, which keeps the escapes the
// expected output is made of readable in the source without them being
// escapes of the source.
func slashed(s string) string { return strings.ReplaceAll(s, "%", `\`) }

func TestPrintableEscapesWhatATerminalObeys(t *testing.T) {
	t.Parallel()
	// Non-ASCII input is written as the bytes of its UTF-8 encoding, so the
	// source stays ASCII.
	cases := map[string]string{
		"":                                     "",
		"plain text / {id}":                    "plain text / {id}",
		"caf\xc3\xa9 \xe6\x97\xa5\xe6\x9c\xac": "caf\xc3\xa9 \xe6\x97\xa5\xe6\x9c\xac",
		"\x1b[31mred\x1b[0m":                   `\x1b[31mred\x1b[0m`,
		"\x1b]0;title\x07":                     `\x1b]0;title\a`,
		"tab\there":                            `tab\there`,
		"line\nbreak\r":                        `line\nbreak\r`,
		"form\ffeed\vtab":                      `form\ffeed\vtab`,
		"nul\x00del\x7f":                       `nul\x00del\x7f`,
		"c1\xc2\x9bcsi":                        slashed("c1%u009bcsi"),
		"bidi\xe2\x80\xaeevil\xe2\x81\xa6":     slashed("bidi%u202eevil%u2066"),
		"zero\xe2\x80\x8bwidth\xef\xbb\xbf":    slashed("zero%u200bwidth%ufeff"),
		"line\xe2\x80\xa8sep\xe2\x80\xa9":      slashed("line%u2028sep%u2029"),
		"tag\xf3\xa0\x81\x81":                  `tag\U000e0041`,
		"bad\xffutf8\xc3":                      `bad\xffutf8\xc3`,
		"\xed\xa0\x80 surrogate":               `\xed\xa0\x80 surrogate`,
		"replacement \xef\xbf\xbd kept":        "replacement \xef\xbf\xbd kept",
		`a\x1b stays as it was`:                `a\x1b stays as it was`,
		"\xc2\xa0 non-breaking":                slashed("%u00a0 non-breaking"),
		"\x1b\x1b\x1b":                         `\x1b\x1b\x1b`,
		"ok\xcc\x81 combining mark":            "ok\xcc\x81 combining mark",
	}
	for in, want := range cases {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
}

// checkPrintable fails unless s holds nothing a terminal would act on.
func checkPrintable(t *testing.T, s string, allowNewline bool) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("output is not UTF-8: %q", s)
	}
	for _, r := range s {
		if r == '\n' && allowNewline {
			continue
		}
		if !unicode.IsPrint(r) {
			t.Fatalf("output holds %U, which a terminal would act on: %q", r, s)
		}
	}
}

func FuzzPrintable(f *testing.F) {
	for _, seed := range []string{"", "plain", "\x1b[2J\x1b[H", "\xe2\x80\xae\xe2\x81\xa6", "\xff\xfe", "\t\n\r\f\v\x00\x7f", "\U0010ffff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := printable(s)
		checkPrintable(t, out, false)
		if len(out) > 4*len(s) {
			t.Fatalf("%d bytes became %d, more than four for each", len(s), len(out))
		}
		if printable(out) != out {
			t.Fatalf("escaping is not idempotent: %q", out)
		}
		if utf8.ValidString(s) {
			clean := true
			for _, r := range s {
				clean = clean && unicode.IsPrint(r)
			}
			if clean && out != s {
				t.Fatalf("printable text was changed: %q became %q", s, out)
			}
		}
	})
}
