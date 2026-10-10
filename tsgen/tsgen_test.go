package tsgen

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"muzak.dev/framework"
)

// The petstore is a representative application: every location a parameter
// is read from, a form with a file, every kind of output, an enum, nullable
// members, maps, byte strings, a recursive type and a deprecated route.

type Owner struct {
	Name  string `json:"name"`
	Email string `json:"email,omitzero"`
}

type Pet struct {
	ID       uuid.UUID         `json:"id"`
	Name     string            `json:"name" doc:"What the pet answers to."`
	Kind     string            `json:"kind"`
	Tags     []string          `json:"tags,omitzero"`
	Owner    *Owner            `json:"owner"`
	Born     time.Time         `json:"born"`
	Attrs    map[string]string `json:"attrs"`
	Photo    []byte            `json:"photo,omitzero"`
	Weight   *float64          `json:"weight"`
	Children []Pet             `json:"children"`
	Age      time.Duration     `json:"age"`
}

type CreatePetIn struct {
	Tenant string   `header:"X-Tenant" doc:"The tenant the pet belongs to."`
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Tags   []string `json:"tags"`
	Score  int      `json:"score,omitzero"`
}

func (in *CreatePetIn) Validate(v *muzak.Validation) {
	v.String(&in.Kind).OneOf("cat", "dog", "bird")
	v.Number(&in.Score).OneOf(1, 2, 3)
}

type GetPetIn struct {
	ID      uuid.UUID `path:"id"`
	Expand  []string  `query:"expand"`
	Session string    `cookie:"session"`
}

type ListPetsIn struct {
	Limit  int     `query:"limit" default:"20"`
	Cursor *string `query:"cursor"`
	Kind   string  `query:"kind" required:"true"`
}

type Page struct {
	Items []Pet   `json:"items"`
	Next  *string `json:"next"`
}

type UploadIn struct {
	ID      uuid.UUID `path:"id"`
	Caption string    `form:"caption" required:"false"`
	Photo   []byte    `file:"photo"`
}

type FileIn struct {
	Path string `path:"path"`
}

var (
	listPets   = muzak.NewEndpoint[ListPetsIn, Page](http.MethodGet, "/pets", muzak.Summary("List pets"), muzak.OperationID("listPets"))
	createPet  = muzak.NewEndpoint[CreatePetIn, Pet](http.MethodPost, "/pets", muzak.Status(http.StatusCreated))
	getPet     = muzak.NewEndpoint[GetPetIn, Pet](http.MethodGet, "/pets/{id}", muzak.Description("Fetch one pet.\nIt says */ and <b>.\n"))
	deletePet  = muzak.NewEndpoint[GetPetIn, muzak.Empty](http.MethodDelete, "/pets/{id}", muzak.Status(http.StatusNoContent), muzak.Deprecated())
	uploadPet  = muzak.NewEndpoint[UploadIn, muzak.Empty](http.MethodPut, "/pets/{id}/photo")
	photo      = muzak.NewEndpoint[GetPetIn, muzak.Bytes](http.MethodGet, "/pets/{id}/photo", muzak.Produces("image/png"))
	file       = muzak.NewEndpoint[FileIn, muzak.Stream](http.MethodGet, "/files/{path...}")
	page       = muzak.NewEndpoint[muzak.Empty, muzak.HTML](http.MethodGet, "/page")
	moved      = muzak.NewEndpoint[muzak.Empty, muzak.Redirect](http.MethodGet, "/old")
	healthPing = muzak.NewEndpoint[muzak.Empty, map[string]bool](http.MethodGet, "/ping")
)

func petstore(t testing.TB) *muzak.Document {
	t.Helper()
	app := muzak.New(muzak.AppOptions{Title: "Petstore", Version: "1.2.3", Description: "Pets, and the people they own.",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone}})
	app.Implement(listPets, func(*muzak.Context, ListPetsIn) (Page, error) { return Page{}, nil })
	app.Implement(createPet, func(*muzak.Context, CreatePetIn) (Pet, error) { return Pet{}, nil })
	app.Implement(getPet, func(*muzak.Context, GetPetIn) (Pet, error) { return Pet{}, nil },
		muzak.WithResponseDoc(http.StatusNotFound, "No such pet"))
	app.Implement(deletePet, func(*muzak.Context, GetPetIn) (muzak.Empty, error) { return muzak.Empty{}, nil })
	app.Implement(uploadPet, func(*muzak.Context, UploadIn) (muzak.Empty, error) { return muzak.Empty{}, nil })
	app.Implement(photo, func(*muzak.Context, GetPetIn) (muzak.Bytes, error) { return muzak.Bytes{}, nil })
	app.Implement(file, func(*muzak.Context, FileIn) (muzak.Stream, error) { return muzak.Stream{}, nil })
	app.Implement(page, func(*muzak.Context, muzak.Empty) (muzak.HTML, error) { return "", nil })
	app.Implement(moved, func(*muzak.Context, muzak.Empty) (muzak.Redirect, error) { return muzak.Redirect{To: "/"}, nil })
	app.Implement(healthPing, func(*muzak.Context, muzak.Empty) (map[string]bool, error) { return nil, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// checkGolden compares got with the file at path, rewriting it instead when
// MUZAK_UPDATE_GOLDEN=1.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("MUZAK_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run with MUZAK_UPDATE_GOLDEN=1 to write it", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the output differs from %s; run with MUZAK_UPDATE_GOLDEN=1 to accept it:\n%s", path, got)
	}
}

func TestGenerateGolden(t *testing.T) {
	t.Parallel()
	doc := petstore(t)
	for _, tc := range []struct {
		file   string
		client bool
	}{
		{"petstore.ts", false},
		{"petstore_client.ts", true},
	} {
		out, err := Generate(doc, Options{Client: tc.client})
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, filepath.Join("testdata", tc.file), out)
		assertLexes(t, out)
		typeCheck(t, out)
	}
}

func TestGenerateReadsADocumentReadBackFromJSON(t *testing.T) {
	t.Parallel()
	doc := petstore(t)
	direct, err := Generate(doc, Options{Client: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	read, err := muzak.ReadDocument(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Generate(read, Options{Client: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(direct, again) {
		t.Fatalf("a document read back from its JSON generates different declarations:\n%s\n----\n%s", direct, again)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	t.Parallel()
	first, err := Generate(petstore(t), Options{Client: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		// A fresh document each time, so that every map is walked in a
		// fresh order.
		again, err := Generate(petstore(t), Options{Client: true})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("two runs over the same application differ")
		}
	}
}

func TestGenerateRefusesNoDocument(t *testing.T) {
	t.Parallel()
	if _, err := Generate(nil, Options{}); err == nil || !strings.HasPrefix(err.Error(), "tsgen: ") {
		t.Fatalf("got %v", err)
	}
}

// expr generates a document holding one component, named T, and returns what
// it was declared as.
func expr(t *testing.T, schema *muzak.Schema, extra map[string]*muzak.Schema) string {
	t.Helper()
	schemas := map[string]*muzak.Schema{"T": schema}
	for name, s := range extra {
		schemas[name] = s
	}
	out, err := Generate(&muzak.Document{Components: &muzak.Components{Schemas: schemas}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	start := strings.Index(text, "export type T = ")
	if start < 0 {
		start = strings.Index(text, "export interface T ")
		if start < 0 {
			t.Fatalf("no declaration of T in:\n%s", text)
		}
		end := strings.Index(text[start:], "\n}\n")
		return text[start+len("export interface T ") : start+end+2]
	}
	end := strings.Index(text[start:], ";\n\n")
	return text[start+len("export type T = ") : start+end]
}

func ref(name string) *muzak.Schema { return &muzak.Schema{Ref: componentPrefix + name} }

// pointerRef refers to a component by a name of any spelling, escaped as a
// JSON pointer escapes one.
func pointerRef(name string) *muzak.Schema {
	return ref(strings.NewReplacer("~", "~0", "/", "~1").Replace(name))
}

func TestGenerateTranslatesEachKeyword(t *testing.T) {
	t.Parallel()
	one, two := 1, 2
	other := map[string]*muzak.Schema{"Other": {Type: "string"}}
	for _, tc := range []struct {
		name   string
		schema *muzak.Schema
		want   string
	}{
		{"string", &muzak.Schema{Type: "string"}, "string"},
		{"date-time", &muzak.Schema{Type: "string", Format: "date-time"}, "string"},
		{"binary outside a form is text", &muzak.Schema{Type: "string", Format: "binary"}, "string"},
		{"integer", &muzak.Schema{Type: "integer", Format: "int64"}, "number"},
		{"number", &muzak.Schema{Type: "number"}, "number"},
		{"boolean", &muzak.Schema{Type: "boolean"}, "boolean"},
		{"null", &muzak.Schema{Type: "null"}, "null"},
		{"nullable", &muzak.Schema{Type: []string{"string", "null"}}, "string | null"},
		{"nullable read from JSON", &muzak.Schema{Type: []any{"integer", "null"}}, "number | null"},
		{"unknown type", &muzak.Schema{Type: "galaxy"}, "unknown"},
		{"any", &muzak.Schema{}, "unknown"},
		{"array", &muzak.Schema{Type: "array", Items: &muzak.Schema{Type: "string"}}, "string[]"},
		{"array of nullable", &muzak.Schema{Type: "array", Items: &muzak.Schema{Type: []string{"string", "null"}}}, "Array<string | null>"},
		{"array of anything", &muzak.Schema{Type: "array"}, "unknown[]"},
		{"implied array", &muzak.Schema{Items: ref("Other")}, "Other[]"},
		{"nested array", &muzak.Schema{Type: "array", Items: &muzak.Schema{Type: "array", Items: &muzak.Schema{Type: "number"}}}, "number[][]"},
		{"reference", ref("Other"), "Other"},
		{"nullable reference", &muzak.Schema{AnyOf: []*muzak.Schema{ref("Other"), {Type: "null"}}}, "Other | null"},
		{"union with anything", &muzak.Schema{AnyOf: []*muzak.Schema{ref("Other"), {}}}, "unknown"},
		{"allOf", &muzak.Schema{AllOf: []*muzak.Schema{ref("Other"), {Type: "object", Properties: map[string]*muzak.Schema{"x": {Type: "number"}}}}},
			"Other & {\n  x?: number;\n}"},
		{"allOf of rules", &muzak.Schema{Type: "string", AllOf: []*muzak.Schema{{Pattern: "a"}, {Pattern: "b"}}}, "string"},
		{"enum", &muzak.Schema{Type: "string", Enum: []any{"a", "b", "a"}}, `"a" | "b"`},
		{"mixed enum", &muzak.Schema{Enum: []any{"x", 1.5, -2, uint8(3), true, nil, float32(0.25)}}, `"x" | 1.5 | -2 | 3 | true | null | 0.25`},
		{"enum of an object", &muzak.Schema{Enum: []any{"x", map[string]any{}}}, "unknown"},
		{"enum of a named string", &muzak.Schema{Enum: []any{kind("cat")}}, `"cat"`},
		{"enum that is not finite", &muzak.Schema{Enum: []any{math.Inf(1), 1}}, "number | 1"},
		{"empty enum", &muzak.Schema{Enum: []any{}}, "never"},
		{"map", &muzak.Schema{Type: "object", AdditionalProperties: &muzak.Schema{Type: "integer"}}, "Record<string, number>"},
		{"map read as a value", &muzak.Schema{Type: "object", AdditionalProperties: muzak.Schema{Type: "boolean"}}, "Record<string, boolean>"},
		{"open object", &muzak.Schema{Type: "object", AdditionalProperties: true}, "Record<string, unknown>"},
		{"object", &muzak.Schema{Type: "object"}, "Record<string, unknown>"},
		{"closed empty object", &muzak.Schema{Type: "object", AdditionalProperties: false}, "Record<string, never>"},
		{"members and extra", &muzak.Schema{Type: "object", Required: []string{"a"}, Properties: map[string]*muzak.Schema{"a": {Type: "string"}}, AdditionalProperties: &muzak.Schema{Type: "integer"}},
			"{\n  a: string;\n  [key: string]: unknown;\n}"},
		{"odd additionalProperties", &muzak.Schema{Type: "object", AdditionalProperties: 7}, "Record<string, unknown>"},
		{"string or number", &muzak.Schema{Type: []string{"string", "number", "string"}}, "string | number"},
		{"lengths are not types", &muzak.Schema{Type: "string", MinLength: &one, MaxLength: &two}, "string"},
	} {
		if got := expr(t, tc.schema, other); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

type kind string

func TestGenerateWritesInterfacesWithOptionalAndDocumentedMembers(t *testing.T) {
	t.Parallel()
	got := expr(t, &muzak.Schema{
		Type:        "object",
		Description: "A thing.",
		Required:    []string{"id", "class"},
		Properties: map[string]*muzak.Schema{
			"id":       {Type: "string", Description: "The id."},
			"class":    {Type: "string"},
			"x-y":      {Type: []string{"number", "null"}, Deprecated: true},
			"9lives":   {Type: "boolean"},
			"$ref":     {Type: "string"},
			"nested":   {Type: "object", Properties: map[string]*muzak.Schema{"deep": {Type: "string"}}, Required: []string{"deep"}},
			"withNull": {AnyOf: []*muzak.Schema{{Type: "object", Properties: map[string]*muzak.Schema{"a": {Type: "string"}}}, {Type: "null"}}},
		},
	}, nil)
	want := "{\n" +
		"  $ref?: string;\n" +
		"  \"9lives\"?: boolean;\n" +
		"  class: string;\n" +
		"  /**\n   * The id.\n   */\n" +
		"  id: string;\n" +
		"  nested?: {\n    deep: string;\n  };\n" +
		"  withNull?: {\n    a?: string;\n  } | null;\n" +
		"  /**\n   * @deprecated\n   */\n" +
		"  \"x-y\"?: number | null;\n" +
		"}"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestGenerateNamesSafely(t *testing.T) {
	t.Parallel()
	doc := &muzak.Document{Components: &muzak.Components{Schemas: map[string]*muzak.Schema{
		"a-b": {Type: "string"}, "a_b": {Type: "string"}, "AB": {Type: "string"},
		"Record": {Type: "string"}, "string": {Type: "string"}, "class": {Type: "string"},
		"1st": {Type: "string"}, "\xc3\xbc": {Type: "string"}, "": {Type: "string"},
		"models.Item": {Type: "string"}, "Page_app.Item": {Type: "string"},
		"GetPetsParams": {Type: "string"}, "x/y~z": {Type: "string"},
		"Uses": {Type: "array", Items: ref("x~1y~0z")},
		// The names the client's runtime declares for itself.
		"Fields": {Type: "object", Properties: map[string]*muzak.Schema{"a": {Type: "string"}}},
		"Sent":   {Type: "object", Properties: map[string]*muzak.Schema{"path": {Type: "string"}}},
		"Wants":  {Type: "array", Items: ref("Fields")},
	}}, Paths: map[string]*muzak.PathItem{
		"/pets": {Get: &muzak.Operation{OperationID: "get_pets", Responses: map[string]*muzak.Response{"200": {Description: "ok"}}}},
		"/a":    {Get: &muzak.Operation{OperationID: "get-pets", Responses: map[string]*muzak.Response{"200": {Description: "ok"}}}},
		"/b":    {Post: &muzak.Operation{Responses: map[string]*muzak.Response{"200": {Description: "ok"}}}},
	}}
	out, err := Generate(doc, Options{Client: true})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		"export type AB = string;", "export type AB2 = string;", "export type AB3 = string;",
		"export type Record2 = string;", "export type String2 = string;", "export type Class = string;",
		"export type _1st = string;", "export type Schema = string;", "export type Schema2 = string;",
		"export type ModelsItem = string;", "export type PageAppItem = string;",
		"export type GetPetsParams = string;", "export type XYZ = string;", "export type Uses = XYZ[];",
		"export interface Fields2 {", "export interface Sent2 {", "export type Wants = Fields2[];",
		// Operations are named after their ids, apart from what the
		// components took, and so are their methods.
		"export interface GetPets2Params", "export interface GetPets3Params", "export interface PostBParams",
		"getPets2(params?: GetPets2Params", "getPets3(params?: GetPets3Params", "postB(params?: PostBParams",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	assertLexes(t, out)
	typeCheck(t, out)
}

func TestGenerateRefusesWhatItCannotTranslate(t *testing.T) {
	t.Parallel()
	deep := &muzak.Schema{Type: "string"}
	for range maxDepth + 1 {
		deep = &muzak.Schema{Type: "array", Items: deep}
	}
	cyclic := &muzak.Schema{Type: "array"}
	cyclic.Items = cyclic
	// Each level points at the one below twice, which is 2^40 walks of a
	// document a few dozen schemas long.
	shared := &muzak.Schema{Type: "string"}
	for range 40 {
		shared = &muzak.Schema{AnyOf: []*muzak.Schema{shared, shared}}
	}
	for _, tc := range []struct {
		name     string
		schema   *muzak.Schema
		fragment string
	}{
		{"dangling reference", ref("Missing"), `"#/components/schemas/Missing" names a schema the document does not define`},
		{"external reference", &muzak.Schema{Ref: "other.json#/components/schemas/T"}, "points outside the document"},
		{"too deep", deep, "nested more than 64 levels deep"},
		{"cyclic in memory", cyclic, "nested more than 64 levels deep"},
		{"exponential in memory", shared, "expand to more than 1048576"},
	} {
		start := time.Now()
		_, err := Generate(&muzak.Document{Components: &muzak.Components{Schemas: map[string]*muzak.Schema{"T": tc.schema}}}, Options{})
		if err == nil || !strings.Contains(err.Error(), tc.fragment) || !strings.HasPrefix(err.Error(), `tsgen: the schema "T": `) {
			t.Errorf("%s: got %v", tc.name, err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("%s: took %v", tc.name, elapsed)
		}
	}
	// Just inside the depth bound is fine.
	fine := &muzak.Schema{Type: "string"}
	for range maxDepth - 1 {
		fine = &muzak.Schema{Type: "array", Items: fine}
	}
	if _, err := Generate(&muzak.Document{Components: &muzak.Components{Schemas: map[string]*muzak.Schema{"T": fine}}}, Options{}); err != nil {
		t.Fatalf("a schema %d levels deep: %v", maxDepth, err)
	}

	// An operation whose path names a parameter it does not describe.
	doc := &muzak.Document{Paths: map[string]*muzak.PathItem{
		"/x/{id}": {Get: &muzak.Operation{OperationID: "x", Responses: map[string]*muzak.Response{}}},
	}}
	if _, err := Generate(doc, Options{}); err == nil || !strings.Contains(err.Error(), `tsgen: GET "/x/{id}": the path names the parameter "id"`) {
		t.Fatalf("got %v", err)
	}
	doc = &muzak.Document{Paths: map[string]*muzak.PathItem{
		"/x": {Get: &muzak.Operation{OperationID: "x", Parameters: []muzak.Parameter{{Name: "q", In: "query", Schema: ref("Nope")}}}},
	}}
	if _, err := Generate(doc, Options{}); err == nil || !strings.Contains(err.Error(), "does not define") {
		t.Fatalf("got %v", err)
	}
	for _, op := range []*muzak.Operation{
		{OperationID: "x", RequestBody: &muzak.RequestBody{Content: map[string]muzak.MediaType{"application/json": {Schema: ref("Nope")}}}},
		{OperationID: "x", Responses: map[string]*muzak.Response{"200": {Content: map[string]muzak.MediaType{"application/json": {Schema: ref("Nope")}}}}},
	} {
		doc := &muzak.Document{Paths: map[string]*muzak.PathItem{"/x": {Post: op}}}
		if _, err := Generate(doc, Options{}); err == nil || !strings.Contains(err.Error(), "does not define") {
			t.Fatalf("got %v", err)
		}
	}
}

func TestGenerateWritesRecursiveTypesByName(t *testing.T) {
	t.Parallel()
	doc := &muzak.Document{Components: &muzak.Components{Schemas: map[string]*muzak.Schema{
		"Node": {Type: "object", Required: []string{"children"}, Properties: map[string]*muzak.Schema{
			"children": {Type: "array", Items: ref("Node")},
			"parent":   {AnyOf: []*muzak.Schema{ref("Node"), {Type: "null"}}},
			"index":    {Type: "object", AdditionalProperties: ref("Node")},
		}},
		"Even": {Type: "object", Properties: map[string]*muzak.Schema{"next": ref("Odd")}},
		"Odd":  {Type: "object", Properties: map[string]*muzak.Schema{"next": ref("Even")}},
	}}}
	out, err := Generate(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"children: Node[];", "parent?: Node | null;", "index?: Record<string, Node>;", "next?: Odd;", "next?: Even;"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	typeCheck(t, out)
}

func TestGenerateTypesOperations(t *testing.T) {
	t.Parallel()
	text := string(must(Generate(petstore(t), Options{})))
	for _, want := range []string{
		// The parameters, grouped, optional where nothing in a group is
		// required.
		"export interface ListPetsParams {\n  query: {\n    cursor?: string | null;\n    kind: string;\n    limit?: number;\n  };\n}",
		"export interface PostPetsParams {\n  header?: {\n    /**\n     * The tenant the pet belongs to.\n     */\n    \"X-Tenant\"?: string;\n  };\n}",
		"cookie?: {\n    session?: string;\n  };",
		// A form body, with its file as a Blob.
		"export type PutPetsByIdPhotoBody = {\n  caption?: string;\n  photo: Blob;\n};",
		"export type PostPetsBody = {\n  kind?: \"cat\" | \"dog\" | \"bird\";",
		"score?: 1 | 2 | 3;",
		// Each kind of output.
		"export type PostPetsResponse = Pet;",
		"export type GetPetsByIdPhotoResponse = Blob;",
		"export type GetFilesByPathResponse = Blob;",
		"export type GetPageResponse = string;",
		"export type GetOldResponse = undefined;",
		"export type DeletePetsByIdResponse = undefined;",
		"export type GetPingResponse = Record<string, boolean>;",
		"export type GetPetsByIdError = ErrorResponse;",
		"export type ListPetsBody = undefined;",
		" * @deprecated",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func must(out []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return out
}

// hostile holds every string a document could use to break out of where it
// is written. Each carries the marker INJECTED, which must only ever appear
// inside a comment or a string literal.
var hostile = []string{
	"*/ @INJECTED@ /*",
	"@INJECTED@ */@export@const@pwned@=@1;@/*",
	"line\n@INJECTED@",
	"line\r@INJECTED@",
	"line\xe2\x80\xa8@INJECTED@",
	"line\xe2\x80\xa9@INJECTED@",
	"line\xc2\x85@INJECTED@",
	"\"; @INJECTED@; \"",
	"\\\"; @INJECTED@; //",
	"`${@INJECTED@}`",
	"'@INJECTED@'",
	"@INJECTED@\x00nul",
	"@INJECTED@\xffbad",
	"@INJECTED@\xe2\x80\xaereversed",
	"*\xe2\x80\xa8/@INJECTED@",
	"</script>@INJECTED@",
	"@INJECTED@*",
	"/@INJECTED@",
}

// hostileDocument puts each hostile string everywhere a document holds text.
func hostileDocument() *muzak.Document {
	schemas := map[string]*muzak.Schema{}
	properties := map[string]*muzak.Schema{}
	var enum []any
	var params []muzak.Parameter
	for i, s := range hostile {
		schemas[s] = &muzak.Schema{Type: "string", Description: s, Title: s}
		properties[s] = &muzak.Schema{Type: "string", Description: s}
		enum = append(enum, s)
		for _, in := range []string{"query", "header", "cookie"} {
			params = append(params, muzak.Parameter{Name: s + fmt.Sprint(i), In: in, Description: s, Schema: &muzak.Schema{Type: "string", Enum: []any{s}}})
		}
	}
	schemas["Model"] = &muzak.Schema{Type: "object", Description: strings.Join(hostile, " "), Properties: properties, Required: hostile[:3]}
	schemas["Choice"] = &muzak.Schema{Enum: enum}
	paths := map[string]*muzak.PathItem{}
	for i, s := range hostile {
		path := "/" + s + "/{p" + fmt.Sprint(i) + "}"
		paths[path] = &muzak.PathItem{Post: &muzak.Operation{
			OperationID: s, Summary: s, Description: s,
			Parameters:  append([]muzak.Parameter{{Name: "p" + fmt.Sprint(i), In: "path", Required: true, Schema: &muzak.Schema{Type: "string"}}}, params...),
			RequestBody: &muzak.RequestBody{Required: true, Content: map[string]muzak.MediaType{"application/json": {Schema: ref("Model")}}},
			Responses: map[string]*muzak.Response{
				"200":     {Description: s, Content: map[string]muzak.MediaType{"application/json": {Schema: ref("Choice")}}},
				"default": {Description: s, Content: map[string]muzak.MediaType{"text/plain": {Schema: &muzak.Schema{Type: "string"}}}},
			},
		}}
	}
	return &muzak.Document{
		Info:       muzak.Info{Title: hostile[0], Version: hostile[1], Description: strings.Join(hostile, "\n")},
		Components: &muzak.Components{Schemas: schemas},
		Paths:      paths,
	}
}

func TestGenerateCannotBeBrokenOutOf(t *testing.T) {
	t.Parallel()
	out, err := Generate(hostileDocument(), Options{Client: true})
	if err != nil {
		t.Fatal(err)
	}
	assertLexes(t, out)
	typeCheck(t, out)
}

// tsSpan is a stretch of TypeScript source that is a comment or a string
// literal.
type tsSpan struct {
	start, end int
	kind       byte
}

// lexTS reads TypeScript source as far as this package writes it, which is
// code, line comments, block comments and double-quoted strings: no template
// literal, no regular expression and no single-quoted string is ever written.
// It returns the comments and strings it found, and fails on one left open,
// on a string holding a raw line terminator, and on code that is not ASCII.
func lexTS(src []byte) ([]tsSpan, error) {
	var spans []tsSpan
	for i := 0; i < len(src); {
		switch {
		case bytes.HasPrefix(src[i:], []byte("//")):
			end := bytes.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			spans = append(spans, tsSpan{i, i + end, '/'})
			i += end
		case bytes.HasPrefix(src[i:], []byte("/*")):
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return nil, fmt.Errorf("a comment at %d is never closed", i)
			}
			spans = append(spans, tsSpan{i, i + 2 + end + 2, '*'})
			i += 2 + end + 2
		case src[i] == '"':
			j := i + 1
			for ; j < len(src) && src[j] != '"'; j++ {
				if src[j] == '\\' {
					j++
					continue
				}
				if src[j] == '\n' || src[j] == '\r' || src[j] >= 0x80 || src[j] < 0x20 {
					return nil, fmt.Errorf("a string at %d holds %q, which is not printable ASCII", i, src[j])
				}
			}
			if j >= len(src) {
				return nil, fmt.Errorf("a string at %d is never closed", i)
			}
			spans = append(spans, tsSpan{i, j + 1, '"'})
			i = j + 1
		case src[i] == '`' || src[i] == '\'':
			return nil, fmt.Errorf("code at %d holds %q, which the generator never writes", i, src[i])
		case src[i] >= 0x80 || (src[i] < 0x20 && src[i] != '\n' && src[i] != '\t'):
			return nil, fmt.Errorf("code at %d holds the byte %#x", i, src[i])
		default:
			i++
		}
	}
	return spans, nil
}

// assertLexes fails unless out lexes and every INJECTED marker in it sits
// inside a comment or a string literal, and no comment holds a line that does
// not begin as a comment line does.
func assertLexes(t *testing.T, out []byte) {
	t.Helper()
	spans, err := lexTS(out)
	if err != nil {
		t.Fatalf("%v in:\n%s", err, out)
	}
	inside := func(at int) bool {
		for _, s := range spans {
			if at >= s.start && at < s.end {
				return true
			}
		}
		return false
	}
	// Every hostile string carries "@" beside each of its parts, and nothing
	// the generator writes as code holds one: a name loses it on the way to
	// an identifier, so an "@" outside a comment or a string is text from the
	// document that escaped where it was written.
	for at := 0; ; {
		i := bytes.IndexByte(out[at:], '@')
		if i < 0 {
			break
		}
		if !inside(at + i) {
			t.Fatalf("text from the document escaped into code at %d:\n%s", at+i, numbered(out))
		}
		at += i + 1
	}
	for _, s := range spans {
		if s.kind != '*' {
			continue
		}
		lines := strings.Split(string(out[s.start:s.end]), "\n")
		for _, line := range lines[1:] {
			trimmed := strings.TrimLeft(line, " ")
			if !strings.HasPrefix(trimmed, "*") {
				t.Fatalf("a comment line does not begin with *: %q", line)
			}
			for _, terminator := range []string{"\r", "\xe2\x80\xa8", "\xe2\x80\xa9", "\xc2\x85"} {
				if strings.Contains(line, terminator) {
					t.Fatalf("a comment line holds a line terminator: %q", line)
				}
			}
		}
	}
}

// typeCheck compiles out with the TypeScript compiler, when one is installed:
// tsc itself, or the one deno carries. Neither is a dependency of the module;
// without one the check is skipped and the lexer above stands in for it.
func typeCheck(t *testing.T, out []byte) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "api.ts")
	if err := os.WriteFile(file, out, 0o600); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	if tsc, err := exec.LookPath("tsc"); err == nil {
		cmd = exec.Command(tsc, "--strict", "--noEmit", "--noUnusedLocals", "--target", "es2022", "--lib", "es2022,dom", "--skipLibCheck", file)
	} else if deno, err := exec.LookPath("deno"); err == nil {
		cmd = exec.Command(deno, "check", "--quiet", "--no-config", file)
		cmd.Env = append(os.Environ(), "NO_COLOR=1", "DENO_NO_UPDATE_CHECK=1")
	} else {
		t.Log("no TypeScript compiler is installed, so the output is lexed but not compiled")
		return
	}
	cmd.Dir = dir
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("the TypeScript compiler refused the output: %v\n%s\n%s", err, output.String(), numbered(out))
	}
}

// numbered returns source with line numbers, for a compiler's report.
func numbered(src []byte) string {
	var b strings.Builder
	for i, line := range strings.Split(string(src), "\n") {
		fmt.Fprintf(&b, "%4d %s\n", i+1, line)
	}
	return b.String()
}

func TestQuote(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                 `""`,
		"plain":            `"plain"`,
		`a"b\c`:            `"a\"b\\c"`,
		"\n\r\t\x00\x7f":   `"\u000a\u000d\u0009\u0000\u007f"`,
		"\xe2\x80\xa8":     `"\u2028"`,
		"\xf0\x9f\x98\x80": `"\ud83d\ude00"`,
		"\xff":             `"\ufffd"`,
		"${x}`":            "\"${x}`\"",
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCommentText(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	writeComment(&b, "", "ends */ here", "", "  ", "x\xe2\x80\xaey\xff\x01 \xc3\xbcber\ttab\xc2\xad")
	want := "/**\n * ends *\\/ here\n *\n * x\\u202ey\\ufffd\\u0001 \xc3\xbcber\ttab\\u00ad\n */\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
	b.Reset()
	writeComment(&b, "", "", " ")
	if b.Len() != 0 {
		t.Fatalf("an empty comment was written: %q", b.String())
	}
}

func FuzzGenerate(f *testing.F) {
	for _, s := range hostile {
		f.Add(s, s, s)
	}
	f.Add("", "", "")
	// A name ending in "...", whose dots a template once lost.
	f.Add("...", "0", "0")
	f.Fuzz(func(t *testing.T, name, text, value string) {
		doc := &muzak.Document{
			Info: muzak.Info{Title: text, Description: text},
			Components: &muzak.Components{Schemas: map[string]*muzak.Schema{
				name: {Type: "object", Description: text, Title: value, Required: []string{value},
					Properties: map[string]*muzak.Schema{value: {Type: "string", Description: text, Enum: []any{value, text}}, name: pointerRef(name)}},
			}},
			Paths: map[string]*muzak.PathItem{
				"/" + value + "/{" + name + "}": {Get: &muzak.Operation{
					OperationID: name, Summary: text,
					Parameters: []muzak.Parameter{
						{Name: name, In: "path", Required: true, Schema: &muzak.Schema{Type: "string"}},
						{Name: value, In: "query", Description: text, Schema: &muzak.Schema{Enum: []any{value}}},
					},
					Responses: map[string]*muzak.Response{"200": {Description: text, Content: map[string]muzak.MediaType{"application/json": {Schema: pointerRef(name)}}}},
				}},
			},
		}
		out, err := Generate(doc, Options{Client: true})
		if err != nil {
			// Only a name with a "{" or "}" in it can make the path's
			// parameter one the operation does not describe.
			if !strings.ContainsAny(name+value, "{}/") {
				t.Fatalf("Generate: %v", err)
			}
			return
		}
		if _, err := lexTS(out); err != nil {
			t.Fatalf("%v in:\n%s", err, out)
		}
		again, _ := Generate(doc, Options{Client: true})
		if !bytes.Equal(out, again) {
			t.Fatal("two runs differ")
		}
		if len(out) > 64*(len(name)+len(text)+len(value))+16<<10 {
			t.Fatalf("%d bytes from %d bytes of text", len(out), len(name)+len(text)+len(value))
		}
	})
}
