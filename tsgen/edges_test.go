package tsgen

import (
	"strings"
	"testing"

	"muzak.dev/framework"
)

func TestGenerateReportsAMissingReferenceWhereverItSits(t *testing.T) {
	t.Parallel()
	missing := ref("Missing")
	for name, schema := range map[string]*muzak.Schema{
		"allOf member":          {AllOf: []*muzak.Schema{missing}},
		"anyOf member":          {AnyOf: []*muzak.Schema{{Type: "string"}, missing}},
		"array items":           {Type: "array", Items: missing},
		"interface member":      {Type: "object", Properties: map[string]*muzak.Schema{"a": missing}},
		"inline object member":  {Type: "array", Items: &muzak.Schema{Type: "object", Properties: map[string]*muzak.Schema{"a": missing}}},
		"additional properties": {Type: "object", AdditionalProperties: missing},
		"implied map":           {AdditionalProperties: missing},
	} {
		_, err := Generate(&muzak.Document{Components: &muzak.Components{Schemas: map[string]*muzak.Schema{"T": schema}}}, Options{})
		if err == nil || !strings.Contains(err.Error(), "does not define") {
			t.Errorf("%s: got %v", name, err)
		}
	}
	long := strings.Repeat("x", 200)
	_, err := Generate(&muzak.Document{Components: &muzak.Components{Schemas: map[string]*muzak.Schema{"T": ref(long)}}}, Options{})
	if err == nil || strings.Contains(err.Error(), long) || !strings.Contains(err.Error(), "...") {
		t.Errorf("a long name is not shortened in %v", err)
	}
}

func TestGenerateTranslatesTheRest(t *testing.T) {
	t.Parallel()
	other := map[string]*muzak.Schema{"Other": {Type: "string"}}
	for _, tc := range []struct {
		name   string
		schema *muzak.Schema
		want   string
	}{
		{"implied map", &muzak.Schema{AdditionalProperties: ref("Other")}, "Record<string, Other>"},
		{"union inside an intersection", &muzak.Schema{AllOf: []*muzak.Schema{{AnyOf: []*muzak.Schema{ref("Other"), {Type: "null"}}}, ref("Other")}}, "(Other | null) & Other"},
		{"array of a union of literals", &muzak.Schema{Type: "array", Items: &muzak.Schema{Enum: []any{"a", "b"}}}, `Array<"a" | "b">`},
		{"array of one literal", &muzak.Schema{Type: "array", Items: &muzak.Schema{Enum: []any{`a"b`}}}, `"a\"b"[]`},
		{"array of a literal ending in a backslash", &muzak.Schema{Type: "array", Items: &muzak.Schema{Enum: []any{`a\`}}}, `Array<"a\\">`},
		{"member with no schema", &muzak.Schema{Type: "object", Properties: map[string]*muzak.Schema{"a": nil}, AdditionalProperties: false},
			"{\n  a?: unknown;\n}"},
	} {
		if got := expr(t, tc.schema, other); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGenerateTypesEveryKindOfBodyAndAnswer(t *testing.T) {
	t.Parallel()
	ok := map[string]*muzak.Response{"200": {Content: map[string]muzak.MediaType{"application/json": {Schema: &muzak.Schema{}}}}}
	doc := &muzak.Document{
		Info: muzak.Info{Description: "Only a description."},
		Paths: map[string]*muzak.PathItem{
			"/nil": nil,
			"/upload": {Post: &muzak.Operation{OperationID: "1st upload",
				RequestBody: &muzak.RequestBody{Content: map[string]muzak.MediaType{"application/octet-stream": {}}},
				Responses:   ok,
			}},
			"/form": {Put: &muzak.Operation{OperationID: "form",
				RequestBody: &muzak.RequestBody{Required: true, Content: map[string]muzak.MediaType{"application/x-www-form-urlencoded": {
					Schema: &muzak.Schema{Type: "object", Properties: map[string]*muzak.Schema{"f": {Type: "string", Format: "binary"}}},
				}}},
				Responses: map[string]*muzak.Response{"204": {}, "2XX": {Content: map[string]muzak.MediaType{"text/csv": {}, "image/png": {}}}},
			}},
		},
	}
	out, err := Generate(doc, Options{Client: true})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		"/*\n * Only a description.\n */",
		"export type _1stUploadBody = Blob | undefined;",
		"export type _1stUploadResponse = unknown;",
		"_1stUpload(params: _1stUploadParams, body?: _1stUploadBody, init?: RequestInit)",
		`send(options, "POST", "/upload", params, "blob", body, "json", init)`,
		"export type FormBody = {\n  f?: string;\n};",
		"export type FormResponse = undefined | Blob | string;",
		`send(options, "PUT", "/form", params, "urlencoded", body, "text", init)`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	assertLexes(t, out)
	typeCheck(t, out)
}

func TestGenerateSpendsItsBudgetEverywhere(t *testing.T) {
	t.Parallel()
	for name, run := range map[string]func(g *generator) error{
		"enum": func(g *generator) error { _, err := g.enum([]any{1, 2, 3}); return err },
		"members": func(g *generator) error {
			_, err := g.object(&muzak.Schema{Properties: map[string]*muzak.Schema{"a": nil, "b": nil, "c": nil}}, false, 0)
			return err
		},
		"parameters": func(g *generator) error {
			_, err := g.params([]muzak.Parameter{{Name: "a", In: "query"}, {Name: "b", In: "query"}, {Name: "c", In: "query"}})
			return err
		},
	} {
		g := &generator{doc: &muzak.Document{}, names: newNamer(), budget: 2}
		if err := run(g); err == nil || !strings.Contains(err.Error(), "expand to more than") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestCommentSkipsBlankLines(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writeCommentLines(&b, "", "a\n   \nb", "", "c")
	if want := " * a\n * b\n *\n * c\n"; b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}
