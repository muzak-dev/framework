package muzak

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// minimalDocument is the smallest document ReadDocument accepts, with room for
// a schema named A.
func minimalDocument(schema string) string {
	doc := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{}`
	if schema != "" {
		doc += `,"components":{"schemas":{"A":` + schema + `}}`
	}
	return doc + "}"
}

// documentWithOperation is a document with one operation, written as given.
func documentWithOperation(operation string) string {
	return `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/items":{"get":` + operation + `}}}`
}

func TestReadDocumentAcceptsWhatItShould(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{
		"minimal":                  minimalDocument(""),
		"open and closed objects":  minimalDocument(`{"type":"object","properties":{"m":{"type":"object","additionalProperties":true}},"additionalProperties":false}`),
		"a map":                    minimalDocument(`{"type":"object","additionalProperties":{"type":["integer","null"]}}`),
		"a nullable reference":     minimalDocument(`{"anyOf":[{"$ref":"#/components/schemas/A"},{"type":"null"}],"allOf":[{"pattern":"a"}]}`),
		"no schema for a body":     documentWithOperation(`{"operationId":"x","requestBody":{"content":{"application/json":{"schema":null}}},"responses":{}}`),
		"every kind of status key": documentWithOperation(`{"operationId":"x","responses":{"200":{"description":""},"4XX":{"description":""},"default":{"description":""}}}`),
		"a parameter":              documentWithOperation(`{"operationId":"x","parameters":[{"name":"q","in":"query","schema":{"type":"string"}}],"responses":{}}`),
		"a version 3.1.1 document": strings.Replace(minimalDocument(""), "3.1.0", "3.1.1", 1),
	} {
		doc, err := ReadDocument(strings.NewReader(input))
		if err != nil || doc == nil {
			t.Errorf("%s: ReadDocument = %v", name, err)
		}
	}

	doc, err := ReadDocument(strings.NewReader(minimalDocument(`{"type":["string","null"],"enum":["a",null],"additionalProperties":{"type":"integer"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	a := doc.Components.Schemas["A"]
	if types, ok := a.Type.([]string); !ok || len(types) != 2 {
		t.Errorf("type = %#v, want a []string", a.Type)
	}
	if values, ok := a.AdditionalProperties.(*Schema); !ok || values.Type != "integer" {
		t.Errorf("additionalProperties = %#v, want a *Schema", a.AdditionalProperties)
	}
	closed, err := ReadDocument(strings.NewReader(minimalDocument(`{"additionalProperties":false}`)))
	if err != nil || closed.Components.Schemas["A"].AdditionalProperties != false {
		t.Errorf("additionalProperties false = %#v, %v", closed.Components.Schemas["A"].AdditionalProperties, err)
	}
}

func TestReadDocumentRefusesMalformedInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, input, want string
	}{
		{"empty", "", "malformed"},
		{"truncated", `{"openapi":`, "malformed"},
		{"trailing data", minimalDocument("") + ` {}`, "malformed"},
		{"not an object", `[]`, "malformed"},
		{"unknown member", strings.Replace(minimalDocument(""), `"paths"`, `"webhooks":{},"paths"`, 1), `unknown object member name "webhooks"`},
		{"unknown schema keyword", minimalDocument(`{"oneOf":[]}`), `unknown object member name "oneOf"`},
		{"map values refer nowhere", minimalDocument(`{"additionalProperties":{"$ref":"#/x"}}`), "/components/schemas/A/additionalProperties"},
		{"unknown keyword in a map's values", minimalDocument(`{"additionalProperties":{"const":1}}`), `unknown object member name "const"`},
		{"member names compared exactly", minimalDocument(`{"Type":"string"}`), `unknown object member name "Type"`},
		{"duplicate member", minimalDocument(`{"type":"string","type":"integer"}`), "duplicate"},
		{"invalid UTF-8", minimalDocument("{\"description\":\"\xff\"}"), "invalid UTF-8"},
		{"another version", strings.Replace(minimalDocument(""), "3.1.0", "3.0.3", 1), `OpenAPI "3.0.3"`},
		{"no version", `{"paths":{}}`, `OpenAPI ""`},
		{"null schema", minimalDocument(`{"properties":{"x":null}}`), "null schema at /components/schemas/A/properties/x"},
		{"null component", minimalDocument(`null`), "null schema at /components/schemas/A"},
		{"null alternative", minimalDocument(`{"anyOf":[null]}`), "null schema at /components/schemas/A/anyOf/0"},
		{"null conjunct", minimalDocument(`{"allOf":[{},null]}`), "null schema at /components/schemas/A/allOf/1"},
		{"dangling reference", minimalDocument(`{"items":{"$ref":"#/components/schemas/B"}}`), `refers to "#/components/schemas/B" at /components/schemas/A/items`},
		{"reference into another file", minimalDocument(`{"$ref":"other.json#/A"}`), "refers to"},
		{"reference into a schema", minimalDocument(`{"properties":{"x":{"$ref":"#/components/schemas/A/properties/x"}}}`), "refers to"},
		{"unknown type", minimalDocument(`{"type":"spaceship"}`), `"spaceship" is not a type`},
		{"empty type list", minimalDocument(`{"type":[]}`), "type list is empty"},
		{"type listed twice", minimalDocument(`{"type":["string","string"]}`), `"string" is listed twice`},
		{"type that is a number", minimalDocument(`{"type":5}`), "neither a name nor a list"},
		{"type that is null", minimalDocument(`{"type":null}`), "neither a name nor a list"},
		{"type list holding a number", minimalDocument(`{"type":["string",5]}`), "something other than a name"},
		{"type too large to read", minimalDocument(`{"type":[1e400]}`), "malformed"},
		{"additionalProperties that is a string", minimalDocument(`{"additionalProperties":"yes"}`), "neither a boolean nor a schema"},
		{"additionalProperties that is null", minimalDocument(`{"additionalProperties":null}`), "neither a boolean nor a schema"},
		{"additionalProperties too large to read", minimalDocument(`{"additionalProperties":1e400}`), "malformed"},
		{"bound too large to read", minimalDocument(`{"maximum":1e400}`), "malformed"},
		{"path without a slash", strings.Replace(minimalDocument(""), `"paths":{}`, `"paths":{"items":{}}`, 1), `"items", which does not start with a slash`},
		{"null path item", strings.Replace(minimalDocument(""), `"paths":{}`, `"paths":{"/items":null}`, 1), "null path item at /paths/~1items"},
		{"parameter read from the body", documentWithOperation(`{"operationId":"x","parameters":[{"name":"q","in":"body","schema":{}}],"responses":{}}`), `read from "body"`},
		{"parameter with no name", documentWithOperation(`{"operationId":"x","parameters":[{"name":"","in":"query","schema":{}}],"responses":{}}`), "with no name"},
		{"parameter schema refers nowhere", documentWithOperation(`{"operationId":"x","parameters":[{"name":"q","in":"query","schema":{"$ref":"#/x"}}],"responses":{}}`), "/paths/~1items/get/parameters/query/q/schema"},
		{"body schema refers nowhere", documentWithOperation(`{"operationId":"x","requestBody":{"content":{"application/json":{"schema":{"$ref":"#/x"}}}},"responses":{}}`), "/paths/~1items/get/requestBody/content/application~1json/schema"},
		{"response schema refers nowhere", documentWithOperation(`{"operationId":"x","responses":{"200":{"description":"","content":{"application/json":{"schema":{"$ref":"#/x"}}}}}}`), "/paths/~1items/get/responses/200/content/application~1json/schema"},
		{"status out of range", documentWithOperation(`{"operationId":"x","responses":{"600":{"description":""}}}`), `keyed "600"`},
		{"status that is a word", documentWithOperation(`{"operationId":"x","responses":{"ok":{"description":""}}}`), `keyed "ok"`},
		{"status range half written", documentWithOperation(`{"operationId":"x","responses":{"2X0":{"description":""}}}`), `keyed "2X0"`},
		{"null response", documentWithOperation(`{"operationId":"x","responses":{"200":null}}`), "null response at /paths/~1items/get/responses/200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc, err := ReadDocument(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("ReadDocument accepted %s", tt.input)
			}
			if doc != nil {
				t.Error("ReadDocument returned a document with its error")
			}
			if !strings.HasPrefix(err.Error(), "muzak: ") || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestReadDocumentSizeLimit drives the size bound at its edge: a document of
// exactly the limit is read, one byte more is refused before it is decoded.
func TestReadDocumentSizeLimit(t *testing.T) {
	t.Parallel()
	doc := minimalDocument("")
	padded := doc + strings.Repeat(" ", maxDocumentBytes-len(doc))
	if _, err := ReadDocument(strings.NewReader(padded)); err != nil {
		t.Fatalf("a document of exactly %d bytes was refused: %v", maxDocumentBytes, err)
	}
	_, err := ReadDocument(strings.NewReader(padded + " "))
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("a document one byte over the limit = %v", err)
	}
	// The reader is not drained beyond the limit, so an endless one is safe.
	if _, err := ReadDocument(endlessReader{}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an endless reader = %v", err)
	}
}

// endlessReader yields spaces for ever.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// TestReadDocumentDepthLimit drives the depth bound at its edge, in a schema
// and in a value a schema holds.
func TestReadDocumentDepthLimit(t *testing.T) {
	t.Parallel()
	// The schema A sits at the fourth level: the document, components,
	// schemas and A itself. Each items adds one.
	nested := func(levels int) string {
		return minimalDocument(strings.Repeat(`{"items":`, levels-4) + `{}` + strings.Repeat(`}`, levels-4))
	}
	if _, err := ReadDocument(strings.NewReader(nested(maxDocumentDepth))); err != nil {
		t.Fatalf("a document %d levels deep was refused: %v", maxDocumentDepth, err)
	}
	_, err := ReadDocument(strings.NewReader(nested(maxDocumentDepth + 1)))
	if err == nil || !strings.Contains(err.Error(), "nests deeper than 128 levels") {
		t.Fatalf("a document one level too deep = %v", err)
	}
	deepValue := minimalDocument(`{"enum":[` + strings.Repeat("[", maxDocumentDepth) + strings.Repeat("]", maxDocumentDepth) + `]}`)
	if _, err := ReadDocument(strings.NewReader(deepValue)); err == nil || !strings.Contains(err.Error(), "nests deeper") {
		t.Fatalf("a deeply nested enum value = %v", err)
	}
}

func TestReadDocumentReportsAFailingReader(t *testing.T) {
	t.Parallel()
	_, err := ReadDocument(io.MultiReader(strings.NewReader("{"), errorReader{}))
	if err == nil || !errors.Is(err, errWriteFailed) || !strings.HasPrefix(err.Error(), "muzak: the OpenAPI document could not be read") {
		t.Fatalf("ReadDocument = %v", err)
	}
}

// errorReader fails every read.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errWriteFailed }

// TestReadDocumentAgainstWhatItWasWrittenFrom reads a document back and
// compares it with the one it was written from, for a document whose values
// a Go program typed: they are the same document.
func TestReadDocumentAgainstWhatItWasWrittenFrom(t *testing.T) {
	t.Parallel()
	type state string
	original := compatReadDoc(&Schema{Type: "object", AdditionalProperties: false, Properties: map[string]*Schema{
		"state": {Type: []string{"string", "null"}, Enum: []any{state("on"), state("off"), nil}, Default: state("on")},
		"count": {Type: "integer", Format: "int32", Minimum: compatPtr(1.0), Default: int64(1)},
		"tags":  {Type: "object", AdditionalProperties: &Schema{Type: "string"}},
	}}, nil)
	data, err := original.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadDocument(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	assertAPIChanges(t, CompareDocuments(original, read))
}
