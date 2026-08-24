package yaml

import (
	"errors"
	"strings"
	"testing"
)

// TestParseRefuses covers every construct this parser declines to read. Each
// case asserts the line the failure is reported on, because a located error is
// the whole point of refusing rather than guessing.
func TestParseRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		line     int
		mentions string
	}{
		{name: "tab indentation", in: "a:\n\tb: one\n", line: 2, mentions: "tab"},
		{name: "explicit tag on a value", in: "a: !!str one\n", line: 1, mentions: "tags"},
		{name: "explicit tag on a key", in: "!!str a: one\n", line: 1, mentions: "tags"},
		{name: "explicit key form", in: "? a\n: one\n", line: 1, mentions: "explicit key"},
		{name: "second document", in: "a: one\n---\nb: two\n", line: 2, mentions: "one document"},
		{name: "flow key", in: "[a]: one\n", line: 1, mentions: "scalar keys"},
		{name: "anchored key", in: "&a b: one\n", line: 1, mentions: "anchor a key"},
		{name: "recursive alias", in: "a: &x\n  b: *x\n", line: 2, mentions: "not been declared"},
		{name: "unknown alias", in: "a: *nope\n", line: 1, mentions: "not been declared"},
		{name: "unclosed single quote", in: "a: 'one\n", line: 1, mentions: "never closed"},
		{name: "unclosed double quote", in: "a: \"one\n", line: 1, mentions: "never closed"},
		{name: "unknown escape", in: "a: \"\\q\"\n", line: 1, mentions: "escape this parser does not know"},
		{name: "escape that is not hexadecimal", in: "a: \"\\uZZZZ\"\n", line: 1, mentions: "hexadecimal"},
		{name: "truncated escape", in: "a: \"\\u12\"\n", line: 1, mentions: "more digits"},
		{name: "unclosed flow sequence", in: "a: [one, two\n", line: 1, mentions: "never closed"},
		{name: "unclosed flow mapping", in: "a: {one: two\n", line: 1, mentions: "never closed"},
		{name: "missing comma in a flow sequence", in: "a: [\"one\" \"two\"]\n", line: 1, mentions: "comma"},
		{name: "missing comma in a flow mapping", in: "a: {b: 1 c: 2}\n", line: 1, mentions: "comma"},
		{name: "flow mapping without a colon", in: "a: {one two}\n", line: 1, mentions: "colon"},
		{name: "flow mapping without a key", in: "a: {: two}\n", line: 1, mentions: "without a key"},
		{name: "anchor inside a flow collection", in: "a: [&b one]\n", line: 1, mentions: "neither tags nor anchors"},
		{name: "text after a flow collection", in: "a: [one] two\n", line: 1, mentions: "follows the end"},
		{name: "text after a quoted value", in: "a: 'one' two\n", line: 1, mentions: "follows the end"},
		{name: "text after a quoted key", in: "'a' b: one\n", line: 1, mentions: "not part of it"},
		{name: "line that is not an entry", in: "a: one\njust text\n", line: 2, mentions: "not a mapping entry"},
		{name: "top level sequence", in: "- one\n- two\n", line: 1, mentions: "must be a mapping"},
		{name: "over-indented entry", in: "a: one\n    b: two\n", line: 2, mentions: "indented further"},
		{name: "unknown directive", in: "%TAG !e! tag\na: one\n", line: 1, mentions: "no directive other than"},
		{name: "content after the terminator", in: "a: one\n...\nb: two\n", line: 3, mentions: "follows the end of the document"},
		{name: "block scalar header", in: "a: |z\n  one\n", line: 1, mentions: "block scalar header"},
		{name: "block scalar chomped twice", in: "a: |-+\n  one\n", line: 1, mentions: "chomping twice"},
		{name: "block scalar indented twice", in: "a: |12\n  one\n", line: 1, mentions: "indentation twice"},
		{name: "anchor without a name", in: "a: &\n", line: 1, mentions: "anchor is written without a name"},
		{name: "alias without a name", in: "a: *\n", line: 1, mentions: "alias is written without a name"},
		{name: "merge from a scalar", in: "a: &b one\nc:\n  <<: *b\n", line: 3, mentions: "not a mapping"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.in))
			if err == nil {
				t.Fatalf("Parse(%q) returned no error, want one", tc.in)
			}
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("Parse(%q) error does not report ErrSyntax", tc.in)
			}
			var syntax *SyntaxError
			if !errors.As(err, &syntax) {
				t.Fatalf("Parse(%q) returned %T, want a *SyntaxError", tc.in, err)
			}
			if syntax.Line != tc.line {
				t.Errorf("Parse(%q) reported line %d, want %d (%v)", tc.in, syntax.Line, tc.line, err)
			}
			if syntax.Column < 1 {
				t.Errorf("Parse(%q) reported column %d, want at least 1", tc.in, syntax.Column)
			}
			if !strings.Contains(syntax.Message, tc.mentions) {
				t.Errorf("Parse(%q) said %q, want it to mention %q", tc.in, syntax.Message, tc.mentions)
			}
		})
	}
}

// TestParseRefusesDeepNesting proves the recursion bound holds, which is what
// keeps a hostile document from exhausting the stack.
func TestParseRefusesDeepNesting(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := range MaxDepth + 10 {
		b.WriteString(strings.Repeat(" ", i))
		b.WriteString("a:\n")
	}
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Fatal("Parse returned no error for a document nested past MaxDepth, want one")
	}
}

// TestParseRefusesDeepFlowNesting is the flow counterpart: brackets recurse
// through a different path than block indentation does.
func TestParseRefusesDeepFlowNesting(t *testing.T) {
	t.Parallel()
	in := "a: " + strings.Repeat("[", MaxDepth+10) + strings.Repeat("]", MaxDepth+10) + "\n"
	if _, err := Parse([]byte(in)); err == nil {
		t.Fatal("Parse returned no error for a flow collection nested past MaxDepth, want one")
	}
}

// TestSyntaxErrorNamesTheFile checks the two renderings of a located failure,
// since one of them is what an editor jumps to.
func TestSyntaxErrorNamesTheFile(t *testing.T) {
	t.Parallel()
	_, err := ParseFile("en.yml", []byte("a: one\n\tb: two\n"))
	if err == nil {
		t.Fatal("ParseFile returned no error, want one")
	}
	if got := err.Error(); !strings.Contains(got, "en.yml:2:") {
		t.Errorf("ParseFile error = %q, want it to name en.yml and the line", got)
	}

	_, err = Parse([]byte("a: one\n\tb: two\n"))
	if err == nil {
		t.Fatal("Parse returned no error, want one")
	}
	if got := err.Error(); !strings.Contains(got, "line 2, column") {
		t.Errorf("Parse error = %q, want it to give the position without a file", got)
	}
}
