package tsgen

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"muzak.dev/framework"
)

// linearGuard is how long one of the documents below may take. Each is a few
// megabytes and is generated in a second or two, under the race detector on
// a loaded machine included; written with a step that costs a pass over, or a
// copy of, everything repeated before it, the same document takes several
// minutes.
const linearGuard = time.Minute

// TestGenerateIsLinearInWhatADocumentRepeats generates documents that repeat
// one thing many times over: names that reduce to the same identifier,
// distinct enum values, required members, union members, responses, type
// names, path parameters and path segments. Each used to cost a pass over
// everything before it, or a copy of it, per repetition.
//
// The documents are generated one at a time, and before the package's other
// tests rather than beside them, since those time themselves too: each
// document is large, and the guard allows for the race detector on a busy
// machine, not for nine of them at once beside everything else.
func TestGenerateIsLinearInWhatADocumentRepeats(t *testing.T) {
	const many = 300_000
	components := func(schemas map[string]*muzak.Schema) *muzak.Document {
		return &muzak.Document{Components: &muzak.Components{Schemas: schemas}}
	}
	// literal is a schema of one distinct literal, which is cheap to write
	// and differs from every other.
	literal := func(i int) *muzak.Schema { return &muzak.Schema{Enum: []any{i}} }
	operation := func(path string, op *muzak.Operation) *muzak.Document {
		if op.Responses == nil {
			op.Responses = map[string]*muzak.Response{"204": {}}
		}
		return &muzak.Document{Paths: map[string]*muzak.PathItem{path: {Get: op}}}
	}
	for name, build := range map[string]func() (*muzak.Document, Options){
		// Every key reduces to "A", so each is numbered after all the others.
		"colliding component names": func() (*muzak.Document, Options) {
			schemas := map[string]*muzak.Schema{}
			for i := range 40_000 {
				schemas["a"+separators(i)] = &muzak.Schema{Type: "string"}
			}
			return components(schemas), Options{}
		},
		"colliding operation ids": func() (*muzak.Document, Options) {
			paths := map[string]*muzak.PathItem{}
			for i := range 40_000 {
				paths["/p"+strconv.Itoa(i)] = &muzak.PathItem{Get: &muzak.Operation{OperationID: "op" + separators(i),
					Responses: map[string]*muzak.Response{"204": {}}}}
			}
			return &muzak.Document{Paths: paths}, Options{}
		},
		"enum values": func() (*muzak.Document, Options) {
			enum := make([]any, many)
			for i := range enum {
				enum[i] = i
			}
			return components(map[string]*muzak.Schema{"E": {Enum: enum}}), Options{}
		},
		"required members": func() (*muzak.Document, Options) {
			properties := map[string]*muzak.Schema{}
			required := make([]string, many)
			for i := range many {
				required[i] = "r" + strconv.Itoa(i)
			}
			for i := range 100_000 {
				properties["p"+strconv.Itoa(i)] = nil
			}
			return components(map[string]*muzak.Schema{"O": {Type: "object", Properties: properties, Required: required}}), Options{}
		},
		"union members": func() (*muzak.Document, Options) {
			members := make([]*muzak.Schema, many)
			for i := range members {
				members[i] = literal(i)
			}
			return components(map[string]*muzak.Schema{"U": {AnyOf: members}}), Options{}
		},
		"responses": func() (*muzak.Document, Options) {
			responses := map[string]*muzak.Response{}
			for i := range many {
				responses["4"+strconv.Itoa(i)] = &muzak.Response{Content: map[string]muzak.MediaType{"application/json": {Schema: literal(i)}}}
			}
			return operation("/r", &muzak.Operation{OperationID: "r", Responses: responses}), Options{}
		},
		// The type keyword names "object" over and over, and the object's
		// one member carries a long description.
		"type names": func() (*muzak.Document, Options) {
			types := make([]any, 40_000)
			for i := range types {
				types[i] = "object"
			}
			member := &muzak.Schema{Type: "string", Description: strings.Repeat("d", 512<<10)}
			return components(map[string]*muzak.Schema{"T": {Type: types, Properties: map[string]*muzak.Schema{"m": member}}}), Options{}
		},
		"path parameters": func() (*muzak.Document, Options) {
			const n = 400_000
			var path strings.Builder
			parameters := make([]muzak.Parameter, n)
			for i := range n {
				name := "p" + strconv.Itoa(i)
				path.WriteString("/{" + name + "}")
				parameters[i] = muzak.Parameter{Name: name, In: "path"}
			}
			return operation(path.String(), &muzak.Operation{OperationID: "p", Parameters: parameters}), Options{}
		},
		"path segments": func() (*muzak.Document, Options) {
			return operation(strings.Repeat("/s", 300_000), &muzak.Operation{OperationID: "s"}), Options{Client: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc, opts := build()
			start := time.Now()
			out, err := Generate(doc, opts)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if elapsed > linearGuard {
				t.Errorf("took %v, more than %v, for %d bytes of output", elapsed, linearGuard, len(out))
			}
		})
	}
}

// separators writes i in base four with characters that are not letters or
// digits, so that every name built from it is distinct and all of them reduce
// to the same identifier.
func separators(i int) string {
	var b strings.Builder
	for {
		b.WriteByte("-_.~"[i%4])
		if i /= 4; i == 0 {
			return b.String()
		}
	}
}
