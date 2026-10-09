package muzak

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// The bounds on a document [ReadDocument] accepts. Both are far above what an
// application Muzak generates a document for reaches: sixteen mebibytes of
// indented JSON is tens of thousands of operations, and a document nests about
// ten levels before its first schema, and two more for each object a schema
// holds inline, which only an anonymous struct or a collection adds.
const (
	maxDocumentBytes = 16 << 20
	maxDocumentDepth = 128
)

// ReadDocument reads an OpenAPI document, such as a baseline a test committed
// with [Document.Marshal], so that it can be compared with [CompareDocuments].
//
// It reads what Muzak writes and refuses everything else with an error that
// says what and where, rather than comparing a document it only half
// understood: a document that is not OpenAPI 3.1, a member [Document] has no
// field for (an extension or a keyword such as oneOf that Muzak never writes),
// a duplicate member, invalid UTF-8, a type JSON Schema does not define, a
// schema, response or header that is null where one is needed, and a
// reference to anything but a schema in the document's own components, which
// is all a comparison can follow.
//
// The document is untrusted input as far as reading it goes: it may be at
// most 16 MiB, and nest at most 128 levels, and anything larger or deeper is
// refused before it is decoded, so a hostile file costs a bounded amount to
// read and to compare. The names an error quotes from it have every character
// a terminal would act on escaped, as [WriteChanges] escapes them, since the
// error is printed by a failing test.
func ReadDocument(r io.Reader) (*Document, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("muzak: the OpenAPI document could not be read: %w", err)
	}
	if len(data) > maxDocumentBytes {
		return nil, fmt.Errorf("muzak: the OpenAPI document is larger than %d bytes, which is more than a document is read at", maxDocumentBytes)
	}
	if err := checkDocumentDepth(data); err != nil {
		return nil, err
	}
	doc := new(Document)
	if err := json.Unmarshal(data, doc, documentReadOptions); err != nil {
		return nil, fmt.Errorf("muzak: the OpenAPI document is malformed: %w", err)
	}
	if err := checkDocument(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// checkDocumentDepth refuses a document that nests deeper than a document is
// read at, before anything is decoded: reading every token once is linear in
// the input, and the decoding that follows recurses as deep as the input
// nests. A syntax error found here is reported as the decoder would.
func checkDocumentDepth(data []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	for {
		if _, err := dec.ReadToken(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("muzak: the OpenAPI document is malformed: %w", err)
		}
		if dec.StackDepth() > maxDocumentDepth {
			return fmt.Errorf("muzak: the OpenAPI document nests deeper than %d levels at %s, which is more than a document is read at",
				maxDocumentDepth, printableReport(string(dec.StackPointer())))
		}
	}
}

// documentReadOptions decode a document strictly: a member [Document] has no
// field for is refused, as json/v2 already refuses a duplicate member and
// invalid UTF-8, and every schema is decoded by [decodeSchema].
var documentReadOptions = json.JoinOptions(
	json.RejectUnknownMembers(true),
	json.WithUnmarshalers(json.UnmarshalFromFunc(decodeSchema)),
)

// schemaFields is [Schema] without its methods, so that decoding one does not
// call back into [decodeSchema].
type schemaFields Schema

// schemaWire is how a schema is decoded. The two keywords [Schema] holds as
// any are decoded by types of their own, which shadow them, so that each
// arrives as what a Document Muzak built holds: a type as a string or a list
// of strings, and additionalProperties as false, true or a *Schema, rather
// than as the []any and map[string]any json/v2 decodes any into.
type schemaWire struct {
	schemaFields
	Type                 wireType       `json:"type,omitzero"`
	AdditionalProperties wireAdditional `json:"additionalProperties,omitzero"`
}

// decodeSchema decodes one schema.
func decodeSchema(dec *jsontext.Decoder, s *Schema) error {
	var wire schemaWire
	if err := json.UnmarshalDecode(dec, &wire); err != nil {
		return err
	}
	*s = Schema(wire.schemaFields)
	s.Type = wire.Type.value
	s.AdditionalProperties = wire.AdditionalProperties.value
	return nil
}

// jsonSchemaTypes are the types JSON Schema defines.
var jsonSchemaTypes = []string{"array", "boolean", "integer", "null", "number", "object", "string"}

// wireType decodes the type keyword.
type wireType struct{ value any }

// UnmarshalJSONFrom reads one type name, or a list of distinct ones.
func (w *wireType) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var raw any
	if err := json.UnmarshalDecode(dec, &raw); err != nil {
		return err
	}
	var names []string
	switch v := raw.(type) {
	case string:
		names, w.value = []string{v}, v
	case []any:
		if len(v) == 0 {
			return errors.New("a type list is empty, which admits no value at all")
		}
		for _, entry := range v {
			name, ok := entry.(string)
			if !ok {
				return errors.New("a type list holds something other than a name")
			}
			names = append(names, name)
		}
		w.value = names
	default:
		return errors.New("a type is neither a name nor a list of names")
	}
	for i, name := range names {
		if !slices.Contains(jsonSchemaTypes, name) {
			return fmt.Errorf("%q is not a type JSON Schema defines", name)
		}
		if slices.Contains(names[:i], name) {
			return fmt.Errorf("the type %q is listed twice", name)
		}
	}
	return nil
}

// wireAdditional decodes the additionalProperties keyword.
type wireAdditional struct{ value any }

// UnmarshalJSONFrom reads a boolean or a schema.
func (w *wireAdditional) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if dec.PeekKind() == '{' {
		values := new(Schema)
		if err := json.UnmarshalDecode(dec, values); err != nil {
			return err
		}
		w.value = values
		return nil
	}
	var raw any
	if err := json.UnmarshalDecode(dec, &raw); err != nil {
		return err
	}
	allowed, ok := raw.(bool)
	if !ok {
		return errors.New("additionalProperties is neither a boolean nor a schema")
	}
	w.value = allowed
	return nil
}

// checkDocument checks what decoding cannot: the version, and that every
// schema, path, operation, response and header is where the document says one
// is.
// The first problem, in a fixed order, is reported.
func checkDocument(d *Document) error {
	if !strings.HasPrefix(d.OpenAPI, "3.1.") {
		return fmt.Errorf("muzak: the document says it is OpenAPI %q, and only an OpenAPI 3.1 document can be read", d.OpenAPI)
	}
	check := documentCheck{}
	if d.Components != nil {
		check.schemas = d.Components.Schemas
	}
	for _, name := range slices.Sorted(maps.Keys(check.schemas)) {
		if err := check.schema("/components/schemas/"+pointerToken(name), check.schemas[name]); err != nil {
			return err
		}
	}
	for _, path := range slices.Sorted(maps.Keys(d.Paths)) {
		location := "/paths/" + pointerToken(path)
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("muzak: the OpenAPI document has the path %q, which does not start with a slash", path)
		}
		if d.Paths[path] == nil {
			return fmt.Errorf("muzak: the OpenAPI document has a null path item at %s", printableReport(location))
		}
		for _, op := range pathOperations(d.Paths[path]) {
			if op.op == nil {
				continue
			}
			if err := check.operation(location+"/"+op.method, op.op); err != nil {
				return err
			}
		}
	}
	return nil
}

// documentCheck checks the parts of one document against its components.
type documentCheck struct {
	schemas map[string]*Schema
}

// parameterLocations are where OpenAPI reads a parameter from.
var parameterLocations = []string{"cookie", "header", "path", "query"}

// operation checks one operation.
func (d documentCheck) operation(location string, op *Operation) error {
	for _, p := range op.Parameters {
		if !slices.Contains(parameterLocations, p.In) {
			return fmt.Errorf("muzak: the OpenAPI document has a parameter %q at %s read from %q, which is not where OpenAPI reads one from", p.Name, printableReport(location), p.In)
		}
		if p.Name == "" {
			return fmt.Errorf("muzak: the OpenAPI document has a %s parameter with no name at %s", p.In, printableReport(location))
		}
		if err := d.optionalSchema(location+"/parameters/"+p.In+"/"+pointerToken(p.Name)+"/schema", p.Schema); err != nil {
			return err
		}
	}
	if op.RequestBody != nil {
		if err := d.content(location+"/requestBody/content", op.RequestBody.Content); err != nil {
			return err
		}
	}
	for _, status := range slices.Sorted(maps.Keys(op.Responses)) {
		at := location + "/responses/" + pointerToken(status)
		if !isStatusKey(status) {
			return fmt.Errorf("muzak: the OpenAPI document has a response keyed %q at %s, which is neither a status code, a range such as 4XX, nor default", status, printableReport(location))
		}
		if op.Responses[status] == nil {
			return fmt.Errorf("muzak: the OpenAPI document has a null response at %s", printableReport(at))
		}
		if err := d.content(at+"/content", op.Responses[status].Content); err != nil {
			return err
		}
		headers := op.Responses[status].Headers
		for _, name := range slices.Sorted(maps.Keys(headers)) {
			header := at + "/headers/" + pointerToken(name)
			if headers[name] == nil {
				return fmt.Errorf("muzak: the OpenAPI document has a null header at %s", printableReport(header))
			}
			if err := d.optionalSchema(header+"/schema", headers[name].Schema); err != nil {
				return err
			}
		}
	}
	return nil
}

// isStatusKey reports whether a responses key is one OpenAPI defines.
func isStatusKey(key string) bool {
	if key == "default" {
		return true
	}
	if len(key) != 3 || key[0] < '1' || key[0] > '5' {
		return false
	}
	if key[1:] == "XX" {
		return true
	}
	_, err := strconv.Atoi(key)
	return err == nil
}

// content checks the schemas of a map of media types.
func (d documentCheck) content(location string, content map[string]MediaType) error {
	for _, media := range slices.Sorted(maps.Keys(content)) {
		if err := d.optionalSchema(location+"/"+pointerToken(media)+"/schema", content[media].Schema); err != nil {
			return err
		}
	}
	return nil
}

// optionalSchema checks a schema a position may leave out.
func (d documentCheck) optionalSchema(location string, s *Schema) error {
	if s == nil {
		return nil
	}
	return d.schema(location, s)
}

// schema checks a schema and everything it holds inline. The depth was bounded
// before decoding, so the recursion is too.
func (d documentCheck) schema(location string, s *Schema) error {
	if s == nil {
		return fmt.Errorf("muzak: the OpenAPI document has a null schema at %s", printableReport(location))
	}
	if s.Ref != "" {
		name, target := refTarget(d.schemas, s.Ref)
		if target == nil || strings.Contains(name, "/") {
			return fmt.Errorf("muzak: the OpenAPI document refers to %q at %s, which is not a schema in its own components", s.Ref, printableReport(location))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		if err := d.schema(location+"/properties/"+pointerToken(name), s.Properties[name]); err != nil {
			return err
		}
	}
	if err := d.optionalSchema(location+"/items", s.Items); err != nil {
		return err
	}
	if err := d.optionalSchema(location+"/additionalProperties", additionalSchema(s.AdditionalProperties)); err != nil {
		return err
	}
	for i, entry := range s.AnyOf {
		if err := d.schema(location+"/anyOf/"+strconv.Itoa(i), entry); err != nil {
			return err
		}
	}
	for i, entry := range s.AllOf {
		if err := d.schema(location+"/allOf/"+strconv.Itoa(i), entry); err != nil {
			return err
		}
	}
	return nil
}
