package yaml

import (
	"math"
	"reflect"
	"testing"
)

func TestParseAccepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "empty document",
			in:   "",
			want: map[string]any{},
		},
		{
			name: "comments only",
			in:   "# nothing here\n\n  # nor here\n",
			want: map[string]any{},
		},
		{
			name: "flat mapping",
			in:   "en:\n  hello: Hello world\n",
			want: map[string]any{"en": map[string]any{"hello": "Hello world"}},
		},
		{
			name: "nested mapping",
			in:   "en:\n  errors:\n    messages:\n      blank: is required\n",
			want: map[string]any{"en": map[string]any{"errors": map[string]any{
				"messages": map[string]any{"blank": "is required"}}}},
		},
		{
			name: "keys are never resolved",
			in:   "no:\n  yes: maybe\n  400: gone\n  on: off\n",
			want: map[string]any{"no": map[string]any{
				"yes": "maybe", "400": "gone", "on": false}},
		},
		{
			name: "scalar resolution",
			in:   "a: ~\nb: null\nc: true\nd: no\ne: 42\nf: -7\ng: 3.5\nh: 0x1f\ni: 1_000\nj: text\n",
			want: map[string]any{
				"a": nil, "b": nil, "c": true, "d": false, "e": int64(42),
				"f": int64(-7), "g": 3.5, "h": int64(31), "i": int64(1000), "j": "text",
			},
		},
		{
			name: "leading zero is decimal",
			in:   "a: 0644\n",
			want: map[string]any{"a": int64(644)},
		},
		{
			name: "quoted scalars",
			in:   "a: 'it''s here'\nb: \"line\\nbreak\"\nc: \"\\u00e9\"\n'd e': f\n",
			want: map[string]any{"a": "it's here", "b": "line\nbreak", "c": "\u00e9", "d e": "f"},
		},
		{
			name: "colon inside a plain value",
			in:   "url: http://muzak.dev/docs\n",
			want: map[string]any{"url": "http://muzak.dev/docs"},
		},
		{
			name: "trailing comment",
			in:   "a: one # a comment\nb: 'two # not a comment'\nc: colour#1\n",
			want: map[string]any{"a": "one", "b": "two # not a comment", "c": "colour#1"},
		},
		{
			name: "block sequence",
			in:   "order:\n  - year\n  - month\n  - day\n",
			want: map[string]any{"order": []any{"year", "month", "day"}},
		},
		{
			name: "sequence with a null entry",
			in:   "month_names:\n  - ~\n  - January\n",
			want: map[string]any{"month_names": []any{nil, "January"}},
		},
		{
			name: "sequence of mappings",
			in:   "people:\n  - name: ada\n    age: 36\n  - name: alan\n    age: 41\n",
			want: map[string]any{"people": []any{
				map[string]any{"name": "ada", "age": int64(36)},
				map[string]any{"name": "alan", "age": int64(41)},
			}},
		},
		{
			name: "nested sequence",
			in:   "grid:\n  - - a\n    - b\n  - - c\n",
			want: map[string]any{"grid": []any{[]any{"a", "b"}, []any{"c"}}},
		},
		{
			name: "empty value is a nested block",
			in:   "en:\n  a:\n  b: two\n",
			want: map[string]any{"en": map[string]any{"a": nil, "b": "two"}},
		},
		{
			name: "flow sequence",
			in:   "order: [year, month, day]\n",
			want: map[string]any{"order": []any{"year", "month", "day"}},
		},
		{
			name: "flow mapping",
			in:   "counts: {one: one item, other: many items}\n",
			want: map[string]any{"counts": map[string]any{"one": "one item", "other": "many items"}},
		},
		{
			name: "nested and trailing comma flow",
			in:   "a: [1, [2, 3], {k: v}, ]\n",
			want: map[string]any{"a": []any{int64(1), []any{int64(2), int64(3)},
				map[string]any{"k": "v"}}},
		},
		{
			name: "literal block scalar",
			in:   "text: |\n  one\n  two\n",
			want: map[string]any{"text": "one\ntwo\n"},
		},
		{
			name: "literal block scalar stripped",
			in:   "text: |-\n  one\n  two\n",
			want: map[string]any{"text": "one\ntwo"},
		},
		{
			name: "folded block scalar",
			in:   "text: >\n  one\n  two\n\n  three\n",
			want: map[string]any{"text": "one two\nthree\n"},
		},
		{
			name: "block scalar with an indentation indicator",
			in:   "text: |2\n    indented\n",
			want: map[string]any{"text": "  indented\n"},
		},
		{
			name: "multi-line plain scalar",
			in:   "note: this is a long\n  sentence that wraps\nnext: here\n",
			want: map[string]any{"note": "this is a long sentence that wraps", "next": "here"},
		},
		{
			name: "anchor and alias",
			in:   "base: &b\n  one: 1\ncopy: *b\n",
			want: map[string]any{
				"base": map[string]any{"one": int64(1)},
				"copy": map[string]any{"one": int64(1)},
			},
		},
		{
			name: "merge key",
			in:   "base: &b\n  one: 1\n  two: 2\nover:\n  <<: *b\n  two: replaced\n",
			want: map[string]any{
				"base": map[string]any{"one": int64(1), "two": int64(2)},
				"over": map[string]any{"one": int64(1), "two": "replaced"},
			},
		},
		{
			name: "merge from several anchors",
			in:   "a: &a\n  one: 1\nb: &b\n  two: 2\nc:\n  <<: [*a, *b]\n",
			want: map[string]any{
				"a": map[string]any{"one": int64(1)},
				"b": map[string]any{"two": int64(2)},
				"c": map[string]any{"one": int64(1), "two": int64(2)},
			},
		},
		{
			name: "document markers",
			in:   "---\na: one\n...\n",
			want: map[string]any{"a": "one"},
		},
		{
			name: "yaml directive",
			in:   "%YAML 1.2\n---\na: one\n",
			want: map[string]any{"a": "one"},
		},
		{
			name: "byte order mark and crlf",
			in:   "\ufeffa: one\r\nb: two\r\n",
			want: map[string]any{"a": "one", "b": "two"},
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

func TestParseSpecialFloats(t *testing.T) {
	t.Parallel()
	got, err := Parse([]byte("a: .inf\nb: -.inf\nc: .nan\n"))
	if err != nil {
		t.Fatalf("Parse returned %v, want no error", err)
	}
	if a := got["a"].(float64); !math.IsInf(a, 1) {
		t.Errorf("a = %v, want +Inf", a)
	}
	if b := got["b"].(float64); !math.IsInf(b, -1) {
		t.Errorf("b = %v, want -Inf", b)
	}
	if c := got["c"].(float64); !math.IsNaN(c) {
		t.Errorf("c = %v, want NaN", c)
	}
}
