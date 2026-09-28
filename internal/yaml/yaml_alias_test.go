package yaml

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// nodeCount and nodeDepth measure a parsed tree the way the parser bounds it.
func nodeCount(v any) int {
	n, _ := sizeOf(v)
	return n
}

func nodeDepth(v any) int {
	_, h := sizeOf(v)
	return h
}

// aliasBomb builds a document whose every anchor aliases the one above it ten
// times, so each level multiplies what the file expands to by ten while
// adding a few dozen bytes to it.
func aliasBomb(levels int) string {
	var b strings.Builder
	b.WriteString("a0: &a0 [x,x,x,x,x,x,x,x,x,x]\n")
	for i := 1; i < levels; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [", i, i)
		for j := range 10 {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "*a%d", i-1)
		}
		b.WriteString("]\n")
	}
	return b.String()
}

// TestAliasBombIsRefused is the regression test for aliases that were copied
// without limit: a 330-byte file of seven levels allocated 216 MB, and each
// further level cost ten times as much for 45 more bytes.
func TestAliasBombIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(aliasBomb(7)))
	var syntax *SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("Parse = %v, want a *SyntaxError for an alias bomb", err)
	}
	if !strings.Contains(syntax.Message, "expand") {
		t.Errorf("message = %q, want it to say the aliases expand too far", syntax.Message)
	}
	if syntax.Line < 2 {
		t.Errorf("reported on line %d, want the line of the alias that crossed the bound", syntax.Line)
	}
}

// TestAliasChainCannotOutnestMaxDepth covers the other way to spend an alias:
// each anchor names the one above it inside one more level of nesting, which
// parsed a depth of 5000 under a limit of 100 and copied the whole chain at
// every step.
func TestAliasChainCannotOutnestMaxDepth(t *testing.T) {
	t.Parallel()
	for name, form := range map[string]string{
		"flow":  "a%d: &a%d [*a%d]\n",
		"block": "a%d: &a%d\n  - *a%d\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			b.WriteString("a0: &a0 [x]\n")
			for i := 1; i < 5000; i++ {
				fmt.Fprintf(&b, form, i, i, i-1)
			}
			tree, err := Parse([]byte(b.String()))
			var syntax *SyntaxError
			if !errors.As(err, &syntax) {
				depth := 0
				for _, v := range tree {
					depth = max(depth, nodeDepth(v))
				}
				t.Fatalf("Parse = %v (depth %d), want a *SyntaxError once the chain outnests MaxDepth", err, depth)
			}
			if syntax.Line > 2*MaxDepth+2 {
				t.Errorf("refused on line %d, want it refused within about MaxDepth levels", syntax.Line)
			}
		})
	}
}

// TestSharingDefaultsStillWorks pins what the bound must not break: a block of
// defaults aliased and merged into every locale of a file, and a value that
// nests a little through aliases.
func TestSharingDefaultsStillWorks(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("defaults: &defaults\n  a: 1\n  b: [x, y, {c: z}]\n  d:\n    e: f\n")
	for i := range 200 {
		fmt.Fprintf(&b, "l%d:\n  <<: *defaults\n  own: %d\n", i, i)
	}
	b.WriteString("deep: &deep [[[[[[[[[[x]]]]]]]]]]\nuse: [*deep, *deep]\n")
	tree, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatalf("Parse = %v, want a document that shares defaults to load", err)
	}
	if got := len(tree["l199"].(map[string]any)); got != 4 {
		t.Errorf("l199 has %d keys, want 4 (three merged, one of its own)", got)
	}
	if got := nodeDepth(tree["use"]); got != 11 {
		t.Errorf("use nests %d levels, want 11", got)
	}
}

// TestAliasExpansionIsBoundedByInputSize is the property the two tests above
// are instances of, checked against the shapes a fuzzer would start from.
func TestAliasExpansionIsBoundedByInputSize(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		aliasBomb(5),
		aliasBomb(12),
		"a: &a [x,x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: [*b,*b,*b,*b,*b,*b,*b,*b,*b,*b]\n",
		"a: &a\n  k: v\nb:\n  <<: *a\n",
	} {
		tree, err := Parse([]byte(doc))
		if err != nil {
			continue
		}
		if got, limit := nodeCount(tree), 20*len(doc)+1000; got > limit {
			t.Errorf("%d input bytes produced %d values, want at most %d", len(doc), got, limit)
		}
		if got := nodeDepth(tree); got > MaxDepth+1 {
			t.Errorf("depth %d exceeds MaxDepth %d", got, MaxDepth)
		}
	}
}
