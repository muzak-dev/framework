package muzak

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests drive the bounds of a comparison: what a document built to be
// expensive costs, and what the comparison says when it stops early.

// chainDocument builds a document whose components form a chain depth long,
// each referring to the next from fanout members, so that the paths through
// it number fanout to the power depth. The head is read by one operation and
// answered by another, and a third reads a union of width references to it.
// extra adds a required member to the last link.
func chainDocument(depth, fanout, width int, extra bool) *Document {
	schemas := map[string]*Schema{}
	link := func(i int) string { return "C" + strconv.Itoa(i) }
	for i := range depth - 1 {
		properties := map[string]*Schema{}
		for j := range fanout {
			properties["p"+strconv.Itoa(j)] = &Schema{Ref: componentPrefix + link(i+1), Description: "Next."}
		}
		schemas[link(i)] = &Schema{Type: "object", Properties: properties}
	}
	last := compatObj(map[string]*Schema{"leaf": compatStr()})
	if extra {
		last.Properties["extra"] = compatStr()
		last.Required = []string{"extra"}
	}
	schemas[link(depth-1)] = last
	union := &Schema{}
	for range width {
		union.AnyOf = append(union.AnyOf, &Schema{Ref: componentPrefix + link(0)})
	}
	schemas["U"] = union
	head := func(name string) map[string]MediaType {
		return map[string]MediaType{"application/json": {Schema: &Schema{Ref: componentPrefix + name}}}
	}
	return compatDoc(map[string]*PathItem{
		"/chain": {
			Post: &Operation{OperationID: "write", RequestBody: &RequestBody{Content: head(link(0))}, Responses: map[string]*Response{"204": {}}},
			Get:  &Operation{OperationID: "read", Responses: map[string]*Response{"200": {Content: head(link(0))}}},
			Put:  &Operation{OperationID: "union", RequestBody: &RequestBody{Content: head("U")}, Responses: map[string]*Response{"204": {}}},
		},
	}, schemas)
}

// stepsFor compares two documents and returns the changes and the steps the
// comparison took.
func stepsFor(old, cur *Document) ([]APIChange, int, bool) {
	c := newDocComparer(old, cur)
	start := c.budget
	c.run()
	return c.result(), start - c.budget, c.incomplete
}

// hangGuard is how long a bounded comparison may take before a test calls it
// a hang. The steps a comparison takes are what these tests measure, since
// they do not depend on the machine; the clock only catches a comparison
// that never ends, with room for the race detector on a loaded CI machine.
const hangGuard = time.Minute

// TestPathologicalDocumentIsComparedInLinearTime compares documents with
// fanout^depth paths through shared references (16^1000 at the largest),
// which a comparison that followed every path would never finish, and checks
// that the change at the end of the chain is found once per direction, that
// the steps stay within a small constant per node of the two documents, and
// that doubling the depth no more than doubles them.
func TestPathologicalDocumentIsComparedInLinearTime(t *testing.T) {
	t.Parallel()
	const fanout, width = 16, 1000
	var steps []int
	for _, depth := range []int{250, 500, 1000} {
		old, cur := chainDocument(depth, fanout, width, false), chainDocument(depth, fanout, width, true)
		start := time.Now()
		got, used, incomplete := stepsFor(old, cur)
		elapsed := time.Since(start)
		at := fmt.Sprintf("/components/schemas/C%d/properties/extra", depth-1)
		assertAPIChanges(t, got,
			wantChange{Breaking, "request-property-added", at},
			wantChange{Compatible, "response-property-added", at},
		)
		nodes := countDocumentNodes(old) + countDocumentNodes(cur)
		if incomplete || used > 8*nodes {
			t.Errorf("depth %d: %d steps for %d nodes (incomplete %v), want at most %d", depth, used, nodes, incomplete, 8*nodes)
		}
		if elapsed > hangGuard {
			t.Errorf("depth %d took %v", depth, elapsed)
		}
		t.Logf("depth %d: %d nodes, %d steps, %v", depth, nodes, used, elapsed)
		steps = append(steps, used)
	}
	for i := 1; i < len(steps); i++ {
		if ratio := float64(steps[i]) / float64(steps[i-1]); ratio > 2.2 {
			t.Errorf("doubling the depth multiplied the steps by %.2f: %v", ratio, steps)
		}
	}
}

// TestExpensiveDocumentStopsAtItsBudget builds the two documents a comparison
// costs most for: one large component compared against a great many partners
// that are small themselves but reach a copy of it through allOf, so that
// every pair walks every member and finds no change. That is quadratic in the
// documents, and the comparison stops at its bound and says so instead.
func TestExpensiveDocumentStopsAtItsBudget(t *testing.T) {
	t.Parallel()
	const members, positions = 5000, 1000
	big := func() *Schema {
		s := &Schema{Type: "object", Properties: map[string]*Schema{}}
		for i := range members {
			s.Properties["m"+strconv.Itoa(i)] = compatStr()
		}
		return s
	}
	oldBody := &Schema{Type: "object", Properties: map[string]*Schema{}}
	curBody := &Schema{Type: "object", Properties: map[string]*Schema{}}
	curSchemas := map[string]*Schema{"Big": big()}
	for i := range positions {
		name := "f" + strconv.Itoa(i)
		oldBody.Properties[name] = &Schema{Ref: componentPrefix + "Big"}
		curBody.Properties[name] = &Schema{Ref: componentPrefix + "Alias" + strconv.Itoa(i)}
		curSchemas["Alias"+strconv.Itoa(i)] = &Schema{AllOf: []*Schema{{Ref: componentPrefix + "Big"}}}
	}
	old := compatAnswerDoc(oldBody, map[string]*Schema{"Big": big()})
	cur := compatAnswerDoc(curBody, curSchemas)
	start := time.Now()
	got, used, incomplete := stepsFor(old, cur)
	if !incomplete || got[0].Kind != "comparison-incomplete" || got[0].Severity != Breaking {
		t.Fatalf("the comparison did not say it stopped: incomplete %v, first change %+v", incomplete, got[0])
	}
	if len(got) > positions+1 {
		t.Errorf("%d changes; only the renames were expected before the comparison stopped", len(got))
	}
	nodes := countDocumentNodes(old) + countDocumentNodes(cur)
	if budget := compareBaseBudget + compareBudgetPerNode*nodes; used > budget+members {
		t.Errorf("used %d steps, more than the budget of %d", used, budget)
	}
	if elapsed := time.Since(start); elapsed > hangGuard {
		t.Errorf("an expensive comparison took %v", elapsed)
	}
	t.Logf("%d nodes, stopped after %d steps where the full comparison needs about %d", nodes, used, 4*members*positions)
}

// TestLargeSchemaReachedEverywhereStopsAtItsBudget builds documents in which
// one component holds something large, a long enum, a long required list, a
// long pattern, a large default or a long member name, and thousands of
// positions reach it beside a rule of their own, so that each position reads
// all of it again. That is quadratic in the documents however few steps the
// positions themselves take, and took minutes for documents of a few hundred
// kilobytes, so reading a schema is charged for what it holds, and the
// comparison stops at its bound and says so.
func TestLargeSchemaReachedEverywhereStopsAtItsBudget(t *testing.T) {
	t.Parallel()
	const positions = 2000
	large := map[string]func() *Schema{
		"enum": func() *Schema {
			s := &Schema{Type: "string"}
			for i := range 2000 {
				s.Enum = append(s.Enum, "v"+strconv.Itoa(i))
			}
			return s
		},
		"required": func() *Schema {
			return &Schema{Type: "object", Required: slices.Repeat([]string{"a"}, 2000)}
		},
		"pattern": func() *Schema { return &Schema{Type: "string", Pattern: strings.Repeat("a", 1<<20)} },
		"default": func() *Schema {
			list := make([]any, 20000)
			for i := range list {
				list[i] = float64(i)
			}
			return &Schema{Type: "array", Default: list}
		},
		"member name": func() *Schema {
			return &Schema{Type: "object", Properties: map[string]*Schema{strings.Repeat("m", 1<<20): compatStr()}}
		},
	}
	for kind, build := range large {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			doc := func() *Document {
				body := &Schema{Type: "object", Properties: map[string]*Schema{}}
				for i := range positions {
					body.Properties["f"+strconv.Itoa(i)] = &Schema{Ref: componentPrefix + "Large", Description: "Rule beside it.", MinLength: compatPtr(0)}
				}
				return compatAnswerDoc(body, map[string]*Schema{"Large": build()})
			}
			old, cur := doc(), doc()
			start := time.Now()
			got, used, incomplete := stepsFor(old, cur)
			if !incomplete || got[0].Kind != "comparison-incomplete" {
				t.Errorf("a comparison that reads the large schema %d times finished in %d steps", 2*positions, used)
			}
			if elapsed := time.Since(start); elapsed > hangGuard {
				t.Errorf("the comparison took %v", elapsed)
			}
		})
	}
}

// TestLongLocationsAreChargedAndTheReportIsBounded covers the text a
// comparison builds rather than reads: every change inside an object is
// located under the name of the member holding it, and every parameter under
// its path, so a long name above many positions is copied once per position.
// That copying is charged, and what the list of changes holds is bounded in
// bytes as well as in changes, so a megabyte name above a few hundred changes
// cannot make it hundreds of megabytes.
func TestLongLocationsAreChargedAndTheReportIsBounded(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("n", 1<<20)
	t.Run("changes under a long member name", func(t *testing.T) {
		t.Parallel()
		doc := func(kind string) *Document {
			inner := &Schema{Type: "object", Properties: map[string]*Schema{}}
			for i := range 500 {
				inner.Properties["m"+strconv.Itoa(i)] = &Schema{Type: kind}
			}
			return compatAnswerDoc(&Schema{Type: "object", Properties: map[string]*Schema{long: inner}}, nil)
		}
		got, _, incomplete := stepsFor(doc("string"), doc("integer"))
		size := 0
		for _, change := range got {
			size += len(change.Location) + len(change.Message) + len(change.Kind)
		}
		if !incomplete || size > compareMaxChangeBytes {
			t.Errorf("the changes hold %d bytes (incomplete %v), want at most %d", size, incomplete, compareMaxChangeBytes)
		}
	})
	t.Run("parameters under a long path", func(t *testing.T) {
		t.Parallel()
		op := compatOp("op")
		for i := range 5000 {
			op.Parameters = append(op.Parameters, Parameter{Name: "q" + strconv.Itoa(i), In: "query", Schema: compatStr()})
		}
		doc := compatDoc(map[string]*PathItem{"/" + long: {Get: op}}, nil)
		if _, used, incomplete := stepsFor(doc, compatDoc(doc.Paths, nil)); !incomplete {
			t.Errorf("comparing 5000 parameters located under a path of %d bytes finished in %d steps", len(long), used)
		}
	})
}

// TestManySecurityAlternativesStopAtTheBudget compares an operation that
// lists a hundred requirements of two thousand scopes each with one that
// lists them again plus a scope, so that every requirement of one side is
// checked against every requirement of the other, scope by scope. That costs
// the product of the two lists times the scopes, and the comparison charges
// it rather than one step per pair.
func TestManySecurityAlternativesStopAtTheBudget(t *testing.T) {
	t.Parallel()
	scopes := make([]string, 2000)
	for i := range scopes {
		scopes[i] = "s" + strconv.Itoa(i)
	}
	doc := func(extra bool) *Document {
		op := compatOp("op")
		for range 100 {
			requirement := scopes
			if extra {
				requirement = append(slices.Clone(scopes), "x")
			}
			op.Security = append(op.Security, SecurityRequirement{"oauth": requirement})
		}
		if extra {
			op.Security[len(op.Security)-1] = SecurityRequirement{"oauth": scopes}
		}
		return compatDoc(map[string]*PathItem{"/a": {Get: op}}, nil)
	}
	start := time.Now()
	got, used, incomplete := stepsFor(doc(false), doc(true))
	if !incomplete || got[0].Kind != "comparison-incomplete" {
		t.Errorf("checking 100 by 100 requirements of 2000 scopes finished in %d steps", used)
	}
	if elapsed := time.Since(start); elapsed > hangGuard {
		t.Errorf("the comparison took %v", elapsed)
	}
}

// TestChangeListIsBounded checks that two documents differing everywhere list
// at most compareMaxChanges changes, and say the list is incomplete.
func TestChangeListIsBounded(t *testing.T) {
	t.Parallel()
	paths := map[string]*PathItem{}
	for i := range compareMaxChanges + 10 {
		paths["/p"+strconv.Itoa(i)] = &PathItem{Get: compatOp("p" + strconv.Itoa(i))}
	}
	got := CompareDocuments(compatDoc(paths, nil), compatDoc(map[string]*PathItem{}, nil))
	if len(got) != compareMaxChanges+1 || got[0].Kind != "comparison-incomplete" {
		t.Fatalf("got %d changes, the first %+v", len(got), got[0])
	}
}

// TestDeepConjunctionStopsAtTheDepthBound covers an allOf nested deeper than a
// read document can be, which only a Document built in Go holds.
func TestDeepConjunctionStopsAtTheDepthBound(t *testing.T) {
	t.Parallel()
	deep := func() *Schema {
		s := &Schema{Type: "string"}
		for range compareMaxDepth + 10 {
			s = &Schema{AllOf: []*Schema{s}}
		}
		return s
	}
	assertAPIChanges(t, CompareDocuments(compatReadDoc(deep(), nil), compatReadDoc(deep(), nil)),
		wantChange{Breaking, "comparison-incomplete", ""})
}

// kitchenSink returns two documents that differ in every way a comparison
// looks at, for the tests that walk all of it.
func kitchenSink() (*Document, *Document) {
	schemes := map[string]SecurityScheme{"bearer": BearerAuth("JWT"), "key": APIKeyHeader("X-Key")}
	build := func(variant bool) *Document {
		pick := func(a, b *Schema) *Schema {
			if variant {
				return b
			}
			return a
		}
		item := compatObj(map[string]*Schema{
			"a":    pick(compatStr(), &Schema{Type: "integer"}),
			"b":    {Type: "number", MultipleOf: compatPtr(2.0), Minimum: compatPtr(0.0)},
			"c":    {AnyOf: []*Schema{compatStr(), pick(&Schema{Type: "integer"}, &Schema{Type: "boolean"})}},
			"d":    {Type: "array", Items: pick(compatStr(), &Schema{Type: "string", MaxLength: compatPtr(2)})},
			"e":    {Type: "object", AdditionalProperties: pick(&Schema{}, compatStr())},
			"f":    {Ref: componentPrefix + "Leaf"},
			"g":    pick(&Schema{Enum: []any{"x"}}, &Schema{Enum: []any{"y"}}),
			"gone": compatStr(),
		}, "a")
		if variant {
			delete(item.Properties, "gone")
			item.Properties["new"] = compatStr()
			item.Properties["b"].MultipleOf = compatPtr(3.0)
		}
		o := compatOp("op")
		o.Parameters = []Parameter{{Name: "q", In: "query", Schema: pick(compatStr(), &Schema{Type: "integer"})}, {Name: "h", In: "header", Schema: compatStr()}}
		o.RequestBody = &RequestBody{Content: map[string]MediaType{"application/json": {Schema: &Schema{Ref: componentPrefix + "Item"}}}}
		o.Responses = map[string]*Response{"200": {Content: map[string]MediaType{"application/json": {Schema: &Schema{Ref: componentPrefix + "Item"}}}}}
		o.Responses["200"].Headers = map[string]*ResponseHeader{"X-Kept": {Required: true, Schema: compatStr()}, "X-Gone": {Schema: compatStr()}}
		o.Security = []SecurityRequirement{Require("bearer")}
		if variant {
			o.Responses["200"].Headers = map[string]*ResponseHeader{"x-kept": {Schema: &Schema{Type: "integer"}}, "X-New": {Schema: compatStr()}}
			o.Responses["404"] = &Response{Description: "Not Found"}
			o.Security = []SecurityRequirement{Require("key")}
			o.Parameters = o.Parameters[:1]
		}
		doc := compatDoc(map[string]*PathItem{"/a": {Post: o}, "/b": {Get: compatOp("b")}}, map[string]*Schema{
			"Item": item,
			"Leaf": pick(compatStr(), &Schema{Type: "string", Pattern: "x"}),
		})
		doc.Components.SecuritySchemes = schemes
		return doc
	}
	return build(false), build(true)
}

// TestComparisonStopsCleanlyAtAnyStep exhausts the budget at every step of a
// comparison that looks at everything, so that every place it can stop is
// shown to stop cleanly and to say so.
func TestComparisonStopsCleanlyAtAnyStep(t *testing.T) {
	t.Parallel()
	pairs := [][2]*Document{}
	a, b := kitchenSink()
	pairs = append(pairs, [2]*Document{a, b}, [2]*Document{b, a})
	pairs = append(pairs, [2]*Document{newIntegrationDocument(t), richDocument(t, true)})
	for _, pair := range pairs {
		full, used, incomplete := stepsFor(pair[0], pair[1])
		if incomplete {
			t.Fatal("the full comparison did not complete")
		}
		// An honest comparison spends a small constant per node, far below
		// the budget per node that only an expensive document reaches.
		if nodes := countDocumentNodes(pair[0]) + countDocumentNodes(pair[1]); used > 8*nodes {
			t.Errorf("an honest comparison took %d steps for %d nodes", used, nodes)
		}
		for budget := range used {
			c := newDocComparer(pair[0], pair[1])
			c.budget = budget
			c.run()
			got := c.result()
			if !slices.ContainsFunc(got, func(change APIChange) bool { return change.Kind == "comparison-incomplete" }) {
				t.Fatalf("with a budget of %d of %d steps, the comparison did not say it stopped", budget, used)
			}
			for _, change := range got {
				if change.Kind != "comparison-incomplete" && !slices.Contains(full, change) {
					t.Fatalf("with a budget of %d of %d steps, the comparison listed %+v, which the full one does not", budget, used, change)
				}
			}
		}
	}
}

// TestKitchenSinkChanges pins what the kitchen sink documents differ in, so
// that the budget test above is known to walk all of it.
func TestKitchenSinkChanges(t *testing.T) {
	t.Parallel()
	a, b := kitchenSink()
	item := "/components/schemas/Item/properties/"
	parameters := "/paths/~1a/post/parameters/"
	headers := "/paths/~1a/post/responses/200/headers/"
	assertAPIChanges(t, CompareDocuments(a, b),
		wantChange{Breaking, "parameter-removed", parameters + "header/h"},
		wantChange{Breaking, "request-type-narrowed", parameters + "query/q/schema/type"},
		wantChange{Breaking, "security-tightened", "/paths/~1a/post/security"},
		wantChange{Compatible, "security-loosened", "/paths/~1a/post/security"},
		wantChange{Breaking, "request-type-changed", item + "a/type"},
		wantChange{Breaking, "response-type-changed", item + "a/type"},
		wantChange{Breaking, "request-multiple-of-tightened", item + "b/multipleOf"},
		wantChange{Compatible, "request-multiple-of-relaxed", item + "b/multipleOf"},
		wantChange{Compatible, "response-multiple-of-tightened", item + "b/multipleOf"},
		wantChange{PossiblyBreaking, "response-multiple-of-relaxed", item + "b/multipleOf"},
		wantChange{Breaking, "request-type-changed", item + "c/anyOf/1/type"},
		wantChange{Breaking, "response-type-changed", item + "c/anyOf/1/type"},
		wantChange{Breaking, "request-max-length-tightened", item + "d/items/maxLength"},
		wantChange{Compatible, "response-max-length-tightened", item + "d/items/maxLength"},
		wantChange{Breaking, "request-type-narrowed", item + "e/additionalProperties/type"},
		wantChange{Breaking, "request-nullable-removed", item + "e/additionalProperties"},
		wantChange{Compatible, "response-type-narrowed", item + "e/additionalProperties/type"},
		wantChange{Compatible, "response-nullable-removed", item + "e/additionalProperties"},
		wantChange{Breaking, "request-enum-values-removed", item + "g/enum"},
		wantChange{Compatible, "request-enum-values-added", item + "g/enum"},
		wantChange{PossiblyBreaking, "response-enum-values-added", item + "g/enum"},
		wantChange{Compatible, "response-enum-values-removed", item + "g/enum"},
		wantChange{PossiblyBreaking, "request-property-removed", item + "gone"},
		wantChange{PossiblyBreaking, "response-property-removed", item + "gone"},
		wantChange{Compatible, "request-property-added", item + "new"},
		wantChange{Compatible, "response-property-added", item + "new"},
		wantChange{Breaking, "request-pattern-added", "/components/schemas/Leaf/pattern"},
		wantChange{Compatible, "response-pattern-added", "/components/schemas/Leaf/pattern"},
		wantChange{Compatible, "response-status-added", "/paths/~1a/post/responses/404"},
		wantChange{PossiblyBreaking, "response-header-removed", headers + "X-Gone"},
		wantChange{Compatible, "response-header-added", headers + "X-New"},
		wantChange{Breaking, "response-header-became-optional", headers + "X-Kept"},
		wantChange{Compatible, "response-type-narrowed", headers + "X-Kept/schema/type"},
	)
}
