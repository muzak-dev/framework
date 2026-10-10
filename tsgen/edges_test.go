package tsgen

import (
	"net/http"
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
		{"array of a literal ending in a backslash", &muzak.Schema{Type: "array", Items: &muzak.Schema{Enum: []any{`a\`}}}, `"a\\"[]`},
		// Two literals ending in a backslash hold as many escaped quotes as
		// unescaped ones beyond the first two, which once passed them off as
		// one literal: "C:\\" | "D:\\"[] is a drive or an array of drives.
		{"array of a union of literals ending in a backslash", &muzak.Schema{Type: "array", Items: &muzak.Schema{Enum: []any{`C:\`, `D:\`}}},
			`Array<"C:\\" | "D:\\">`},
		{"union of literals ending in a backslash inside an intersection",
			&muzak.Schema{AllOf: []*muzak.Schema{{Enum: []any{`C:\`, `D:\`}}, ref("Other")}}, `("C:\\" | "D:\\") & Other`},
		{"array of one literal of escapes", &muzak.Schema{Type: "array", Items: &muzak.Schema{Enum: []any{`\"\`}}}, `"\\\"\\"[]`},
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

// twiceIn binds one query parameter from two fields, which an application
// builds and documents as two parameters of the same name.
type twiceIn struct {
	Text string `query:"q"`
	Num  *int   `query:"q"`
}

func TestGenerateWritesAParameterDescribedTwiceOnce(t *testing.T) {
	t.Parallel()
	doc := &muzak.Document{Paths: map[string]*muzak.PathItem{"/x/{id}": {Get: &muzak.Operation{OperationID: "x",
		Parameters: []muzak.Parameter{
			{Name: "id", In: "path", Required: true, Schema: &muzak.Schema{Type: "string"}},
			{Name: "q", In: "query", Description: "As text.", Schema: &muzak.Schema{Type: "string"}},
			{Name: "id", In: "path", Required: true, Schema: &muzak.Schema{Type: "string"}},
			{Name: "q", In: "query", Required: true, Description: "As a number.", Schema: &muzak.Schema{Type: "integer"}},
			{Name: "q", In: "header", Schema: &muzak.Schema{Type: "boolean"}},
		},
		Responses: map[string]*muzak.Response{"204": {}}}}}}
	out := must(Generate(doc, Options{Client: true}))
	text := string(out)
	// One member per name and location, whose value must be every type it
	// is described with, as the one value sent is read by each.
	for want, count := range map[string]int{
		"    id: string;\n": 1, "    q: string & number;\n": 1, "    q?: boolean;\n": 1, "As text.": 1, "As a number.": 1,
	} {
		if got := strings.Count(text, want); got != count {
			t.Errorf("%q appears %d times, want %d, in:\n%s", want, got, count, text)
		}
	}
	typeCheck(t, out)

	app := muzak.New(muzak.AppOptions{LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone}})
	app.Implement(muzak.NewEndpoint[twiceIn, muzak.Empty](http.MethodGet, "/twice"),
		func(*muzak.Context, twiceIn) (muzak.Empty, error) { return muzak.Empty{}, nil })
	built, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	out = must(Generate(built, Options{Client: true}))
	if got := strings.Count(string(out), "    q?: "); got != 1 {
		t.Errorf("the parameter q is written %d times in:\n%s", got, out)
	}
	typeCheck(t, out)
}

func TestGenerateKeepsTheClientWithinItsBaseURL(t *testing.T) {
	t.Parallel()
	doc := func(path string) *muzak.Document {
		var parameters []muzak.Parameter
		for _, name := range templateParams(path) {
			parameters = append(parameters, muzak.Parameter{Name: name, In: "path", Required: true, Schema: &muzak.Schema{Type: "string"}})
		}
		return &muzak.Document{Paths: map[string]*muzak.PathItem{path: {Get: &muzak.Operation{OperationID: "x",
			Parameters: parameters, Responses: map[string]*muzak.Response{"204": {}}}}}}
	}
	// URL resolves a dot segment however it is spelled, and enough of them
	// climb out of the base the client was given, to another service on the
	// same host, with the headers given for this one.
	for _, path := range []string{"/../../admin", "/v1/./x", "/a/%2e%2E/b", "/a/.%2e", "/%2E/{id}", "/x/.."} {
		if _, err := Generate(doc(path), Options{Client: true}); err == nil || !strings.Contains(err.Error(), "dot segment") ||
			!strings.HasPrefix(err.Error(), "tsgen: GET ") {
			t.Errorf("%q: got %v", path, err)
		}
		// The types alone hold the path as a string, which nothing resolves.
		if _, err := Generate(doc(path), Options{}); err != nil {
			t.Errorf("%q without the client: %v", path, err)
		}
	}
	// "?" and "#" would begin a query and a fragment, URL reads "\" as "/" and
	// drops a tab or a line break, so each is written escaped, as a Muzak
	// router decodes it before it compares the segment.
	out := must(Generate(doc("/a?b=1#c\\d\te\n/.../{id}/x?"), Options{Client: true}))
	if want := `"/a%3Fb=1%23c%5Cd%09e%0A/.../" + pathSegment("id", params.path["id"]) + "/x%3F"`; !strings.Contains(string(out), want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
	assertLexes(t, out)
	typeCheck(t, out)
}

func TestCommentSkipsBlankLines(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writeCommentLines(&b, "", "a\n   \nb", "", "c")
	if want := " * a\n * b\n *\n * c\n"; b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}
