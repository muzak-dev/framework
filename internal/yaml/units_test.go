package yaml

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLooksNumeric(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"1", "-1", "+1", ".5", "0x1"} {
		if !looksNumeric(s) {
			t.Errorf("looksNumeric(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "inf", "NaN", "one", "e5"} {
		if looksNumeric(s) {
			t.Errorf("looksNumeric(%q) = true, want false", s)
		}
	}
}

func TestParseInt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{in: "42", want: 42, ok: true},
		{in: "-42", want: -42, ok: true},
		{in: "+42", want: 42, ok: true},
		{in: "0x1F", want: 31, ok: true},
		{in: "0X1f", want: 31, ok: true},
		{in: "0o17", want: 15, ok: true},
		{in: "0O17", want: 15, ok: true},
		{in: "0b101", want: 5, ok: true},
		{in: "0B101", want: 5, ok: true},
		{in: "-0x10", want: -16, ok: true},
		{in: "", ok: false},
		{in: "0x", ok: false},
		{in: "-", ok: false},
		{in: "1.5", ok: false},
		{in: "99999999999999999999", ok: false},
	}
	for _, tc := range cases {
		got, ok := parseInt(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseInt(%q) = %d, %v, want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestUnquoteDoubleEscapes(t *testing.T) {
	t.Parallel()
	// One case per escape the table defines, so that a sequence cannot be
	// dropped from it without a test noticing.
	cases := map[string]string{
		`"\0"`:         "\x00",
		`"\a"`:         "\a",
		`"\b"`:         "\b",
		`"\t"`:         "\t",
		`"\n"`:         "\n",
		`"\v"`:         "\v",
		`"\f"`:         "\f",
		`"\r"`:         "\r",
		`"\e"`:         "\x1b",
		`"\ "`:         " ",
		`"\""`:         "\"",
		`"\/"`:         "/",
		`"\\"`:         "\\",
		`"\N"`:         string(rune(0x0085)),
		`"\_"`:         string(rune(0x00A0)),
		`"\L"`:         string(rune(0x2028)),
		`"\P"`:         string(rune(0x2029)),
		`"\x41"`:       "A",
		`"\u00e9"`:     "\u00e9",
		`"\U0001F600"`: "\U0001F600",
	}
	for in, want := range cases {
		got, used, err := unquoteDouble(in, "", line{num: 1}, 1)
		if err != nil {
			t.Errorf("unquoteDouble(%s) returned %v, want no error", in, err)
			continue
		}
		if got != want {
			t.Errorf("unquoteDouble(%s) = %q, want %q", in, got, want)
		}
		if used != len(in) {
			t.Errorf("unquoteDouble(%s) consumed %d bytes, want %d", in, used, len(in))
		}
	}
}

func TestUnquoteDoubleRefusesBadRune(t *testing.T) {
	t.Parallel()
	// A surrogate half is a valid hexadecimal number and not a character, which
	// is the one case the digits alone cannot rule out.
	if _, _, err := unquoteDouble(`"\uD800"`, "", line{num: 1}, 1); err == nil {
		t.Error("unquoteDouble returned no error for a surrogate half, want one")
	}
	// Above the last code point entirely, which is the case that has to be
	// caught before the value is narrowed rather than after.
	if _, _, err := unquoteDouble(`"\U0011FFFF"`, "", line{num: 1}, 1); err == nil {
		t.Error("unquoteDouble returned no error for a value above the last code point, want one")
	}
	if _, _, err := unquoteDouble(`"\`, "", line{num: 1}, 1); err == nil {
		t.Error("unquoteDouble returned no error for a value ending in a backslash, want one")
	}
}

func TestUnquoteSingle(t *testing.T) {
	t.Parallel()
	got, used, err := unquoteSingle(`'a''b' rest`, "", line{num: 1}, 1)
	if err != nil || got != "a'b" || used != 6 {
		t.Errorf("unquoteSingle = %q, %d, %v, want \"a'b\", 6, no error", got, used, err)
	}
}

func TestSplitKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		key    string
		rest   string
		quoted bool
		ok     bool
	}{
		{in: "a: one", key: "a", rest: "one", ok: true},
		{in: "a:", key: "a", ok: true},
		{in: `"a b": one`, key: `"a b"`, rest: "one", quoted: true, ok: true},
		{in: "a: [x: 1]", key: "a", rest: "[x: 1]", ok: true},
		{in: "url: http://x", key: "url", rest: "http://x", ok: true},
		{in: "a:one"},
		{in: "just text"},
		{in: `"unclosed: one`},
		{in: "{a: 1}"},
	}
	for _, tc := range cases {
		key, rest, quoted, ok := splitKey(tc.in)
		if ok != tc.ok {
			t.Errorf("splitKey(%q) reported ok=%v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if key != tc.key || rest != tc.rest || quoted != tc.quoted {
			t.Errorf("splitKey(%q) = %q, %q, %v, want %q, %q, %v",
				tc.in, key, rest, quoted, tc.key, tc.rest, tc.quoted)
		}
	}
}

func TestSkipQuoted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{in: `'a'`, want: 3},
		{in: `'a''b'`, want: 6},
		{in: `"a\"b"`, want: 6},
		{in: `'a`, want: -1},
		{in: `"a`, want: -1},
	}
	for _, tc := range cases {
		if got := skipQuoted(tc.in, 0); got != tc.want {
			t.Errorf("skipQuoted(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStripComment(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"a # b":     "a",
		"# all":     "",
		"a#b":       "a#b",
		"'a # b'":   "'a # b'",
		`"a # b"`:   `"a # b"`,
		`"a\" # b"`: `"a\" # b"`,
		"a":         "a",
	}
	for in, want := range cases {
		if got := stripComment(in); got != want {
			t.Errorf("stripComment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAssembleChomping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body []string
		fold bool
		chop byte
		want string
	}{
		{name: "clip", body: []string{"a", ""}, want: "a\n"},
		{name: "strip", body: []string{"a", ""}, chop: '-', want: "a"},
		{name: "keep", body: []string{"a", "", ""}, chop: '+', want: "a\n\n\n"},
		{name: "empty clip", body: nil, want: ""},
		{name: "empty keep", body: []string{""}, chop: '+', want: "\n"},
		{name: "folded indented line keeps its break", body: []string{"a", "  b", "c"}, fold: true, want: "a\n  b\nc\n"},
		{name: "folded paragraphs", body: []string{"a", "", "b"}, fold: true, want: "a\nb\n"},
		{name: "folded blank run", body: []string{"a", "", "", "b"}, fold: true, want: "a\n\nb\n"},
	}
	for _, tc := range cases {
		if got := assemble(tc.body, tc.fold, tc.chop); got != tc.want {
			t.Errorf("%s: assemble = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCopyNode(t *testing.T) {
	t.Parallel()
	original := map[string]any{"list": []any{map[string]any{"k": "v"}}, "scalar": int64(1)}
	copied := copyNode(original).(map[string]any)
	if !reflect.DeepEqual(copied, original) {
		t.Fatalf("copyNode = %#v, want an equal tree", copied)
	}
	copied["list"].([]any)[0].(map[string]any)["k"] = "changed"
	if original["list"].([]any)[0].(map[string]any)["k"] != "v" {
		t.Error("copyNode shared a nested mapping with the original, want a copy")
	}
}

func TestErrAtClampsTheColumn(t *testing.T) {
	t.Parallel()
	// A column is 1-based wherever it is reported, so an arithmetic slip that
	// produces zero must not travel out to an editor as a position.
	var syntax *SyntaxError
	err := errAt("", line{num: 3}, 0, "something")
	if !errors.As(err, &syntax) || syntax.Column != 1 {
		t.Errorf("errAt with column 0 reported %v, want column 1", err)
	}
}

// TestParseMoreShapes covers the block and flow paths the locale corpus does
// not happen to exercise, so that the parser is tested for what it claims to
// read rather than only for what testdata contains.
func TestParseMoreShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "sequence entry holding a nested block",
			in:   "a:\n  -\n    b: one\n  -\n    b: two\n",
			want: map[string]any{"a": []any{
				map[string]any{"b": "one"}, map[string]any{"b": "two"}}},
		},
		{
			name: "sequence ends at a sibling key",
			in:   "a:\n  - one\nb: two\n",
			want: map[string]any{"a": []any{"one"}, "b": "two"},
		},
		{
			name: "alias to a sequence is copied",
			in:   "a: &s [1, 2]\nb: *s\n",
			want: map[string]any{"a": []any{int64(1), int64(2)}, "b": []any{int64(1), int64(2)}},
		},
		{
			name: "anchor on a scalar",
			in:   "a: &s one\nb: *s\n",
			want: map[string]any{"a": "one", "b": "one"},
		},
		{
			name: "quoted flow key",
			in:   "a: {'k 1': one, \"k 2\": two}\n",
			want: map[string]any{"a": map[string]any{"k 1": "one", "k 2": "two"}},
		},
		{
			name: "empty flow collections",
			in:   "a: []\nb: {}\n",
			want: map[string]any{"a": []any{}, "b": map[string]any{}},
		},
		{
			name: "block scalar keeping its trailing breaks",
			in:   "a: |+\n  one\n\n\nb: two\n",
			want: map[string]any{"a": "one\n\n\n", "b": "two"},
		},
		{
			name: "block scalar with a comment on its header",
			in:   "a: | # a note\n  one\n",
			want: map[string]any{"a": "one\n"},
		},
		{
			name: "blank line inside a literal block",
			in:   "a: |\n  one\n\n  two\n",
			want: map[string]any{"a": "one\n\ntwo\n"},
		},
		{
			name: "comment between entries",
			in:   "a: one\n# between\nb: two\n",
			want: map[string]any{"a": "one", "b": "two"},
		},
		{
			name: "dash that is not a sequence entry",
			in:   "a: -3\nb: --see below\n",
			want: map[string]any{"a": int64(-3), "b": "--see below"},
		},
		{
			name: "sequence of sequences of mappings",
			in:   "a:\n  - - k: v\n",
			want: map[string]any{"a": []any{[]any{map[string]any{"k": "v"}}}},
		},
		{
			name: "trailing document terminator only",
			in:   "a: one\n...",
			want: map[string]any{"a": "one"},
		},
		{
			name: "sequence flush with its own key",
			in:   "a:\n- one\n- two\nb: three\n",
			want: map[string]any{"a": []any{"one", "two"}, "b": "three"},
		},
		{
			name: "plain scalar stopped by a blank line",
			in:   "a: one\n  two\n\nb: three\n",
			want: map[string]any{"a": "one two", "b": "three"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse(%q) returned %v, want no error", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse(%q) =\n  %#v\nwant\n  %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseRefusesMoreShapes covers the remaining refusals, which are mostly
// reached through a flow collection rather than through block structure.
func TestParseRefusesMoreShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{name: "tab indenting a sequence entry", in: "a:\n\t- one\n"},
		{name: "over-indented sequence entry", in: "a:\n  - one\n      - two\n"},
		{name: "unclosed quote inside a flow collection", in: "a: [\"one]\n"},
		{name: "alias without a name inside a flow collection", in: "a: [*]\n"},
		{name: "unknown alias inside a flow collection", in: "a: [*nope]\n"},
		{name: "flow mapping ending after a colon", in: "a: {b:\n"},
		{name: "tag inside a flow collection", in: "a: [!!str one]\n"},
		{name: "nested tag", in: "a:\n  b: !!str one\n"},
		{name: "sequence under a key that is over-indented", in: "a:\n    b: one\n  c: two\n"},
		{name: "block scalar line less indented than the block", in: "a: |\n    one\n  two\n"},
		{name: "empty flow sequence that is never closed", in: "a: [\n"},
		{name: "empty flow mapping that is never closed", in: "a: {\n"},
		{name: "unclosed quoted key inside a flow mapping", in: "a: {\"unclosed: 1}\n"},
		{name: "bad escape in a quoted key", in: "\"a\\q\": one\n"},
		{name: "tab indenting a later sequence entry", in: "a:\n  - one\n  \t- two\n"},
		{name: "tag inside a nested sequence block", in: "a:\n  -\n    b: !!str one\n"},
		{name: "tag in a mapping opened by a dash", in: "a:\n  - b: !!str one\n"},
		{name: "unknown alias as a sequence entry", in: "a:\n  - *nope\n"},
		{name: "unknown alias in a merge key", in: "a:\n  <<: *nope\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(tc.in)); err == nil {
				t.Fatalf("Parse(%q) returned no error, want one", tc.in)
			} else if !strings.HasPrefix(err.Error(), "yaml: ") {
				t.Errorf("Parse(%q) returned %q, want a located yaml error", tc.in, err)
			}
		})
	}
}
