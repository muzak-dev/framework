package badele

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"badele/validate"
)

// OpenAPIVersion is the specification version Badele emits.
const OpenAPIVersion = "3.1.0"

// componentPrefix is the JSON pointer prefix under which named schemas live.
const componentPrefix = "#/components/schemas/"

// OpenAPIOptions describes the API in the generated document.
//
// It is embedded in [AppOptions], so its fields can be set inline:
//
//	badele.AppOptions{Title: "Bigger Applications Example", Version: "1.0.0"}
type OpenAPIOptions struct {
	// Title names the API. It defaults to "Badele API".
	Title string
	// Version is the API's own version, not the OpenAPI version. It defaults
	// to "0.1.0".
	Version string
	// Description is a long-form description of the API, rendered as
	// CommonMark by documentation tools.
	Description string
	// TermsOfService is a URL to the terms the API is offered under.
	TermsOfService string
	// Contact identifies who to approach about the API.
	Contact *Contact
	// License states the licence the API is offered under.
	License *License
	// Servers lists the base URLs the API is served from. When empty,
	// documentation tools treat the document's own origin as the server.
	Servers []Server
}

// Contact identifies the people responsible for an API.
type Contact struct {
	// Name is the person or team to contact.
	Name string `json:"name,omitzero"`
	// URL points at a contact page.
	URL string `json:"url,omitzero"`
	// Email is a contact address.
	Email string `json:"email,omitzero"`
}

// License states the licence an API is offered under.
type License struct {
	// Name is the licence name, such as "Apache 2.0".
	Name string `json:"name"`
	// Identifier is the SPDX identifier, such as "Apache-2.0". It is mutually
	// exclusive with URL in OpenAPI 3.1.
	Identifier string `json:"identifier,omitzero"`
	// URL points at the licence text.
	URL string `json:"url,omitzero"`
}

// Server describes one base URL the API is served from.
type Server struct {
	// URL is the base URL, which may be relative to the document.
	URL string `json:"url"`
	// Description explains what this server is, such as "production".
	Description string `json:"description,omitzero"`
}

// Tag groups operations in the generated documentation.
type Tag struct {
	// Name is the tag as it appears on operations.
	Name string `json:"name"`
	// Description explains what the group covers.
	Description string `json:"description,omitzero"`
}

// Document is a complete OpenAPI 3.1 description of an application. It is
// produced once when the application is built and served at
// [AppOptions.OpenAPIPath].
type Document struct {
	// OpenAPI is the specification version, always [OpenAPIVersion].
	OpenAPI string `json:"openapi"`
	// Info carries the title, version and other API metadata.
	Info Info `json:"info"`
	// Servers lists the base URLs the API is served from.
	Servers []Server `json:"servers,omitzero"`
	// Paths maps each path template to the operations available on it.
	Paths map[string]*PathItem `json:"paths"`
	// Components holds the reusable schemas referenced from operations.
	Components *Components `json:"components,omitzero"`
	// Tags lists the groups operations are sorted into.
	Tags []Tag `json:"tags,omitzero"`
}

// Info carries the metadata describing an API.
type Info struct {
	// Title names the API.
	Title string `json:"title"`
	// Version is the API's version.
	Version string `json:"version"`
	// Description is a long-form description of the API.
	Description string `json:"description,omitzero"`
	// TermsOfService is a URL to the terms of service.
	TermsOfService string `json:"termsOfService,omitzero"`
	// Contact identifies who to approach about the API.
	Contact *Contact `json:"contact,omitzero"`
	// License states the licence the API is offered under.
	License *License `json:"license,omitzero"`
}

// PathItem lists the operations available at one path template.
type PathItem struct {
	// Get through Options are the operations registered for each method.
	Get     *Operation `json:"get,omitzero"`
	Put     *Operation `json:"put,omitzero"`
	Post    *Operation `json:"post,omitzero"`
	Delete  *Operation `json:"delete,omitzero"`
	Patch   *Operation `json:"patch,omitzero"`
	Head    *Operation `json:"head,omitzero"`
	Options *Operation `json:"options,omitzero"`
}

// set stores op under the slot for an HTTP method, reporting whether the
// method has a slot in OpenAPI at all.
func (p *PathItem) set(method string, op *Operation) bool {
	switch method {
	case http.MethodGet:
		p.Get = op
	case http.MethodPut:
		p.Put = op
	case http.MethodPost:
		p.Post = op
	case http.MethodDelete:
		p.Delete = op
	case http.MethodPatch:
		p.Patch = op
	case http.MethodHead:
		p.Head = op
	case http.MethodOptions:
		p.Options = op
	default:
		return false
	}
	return true
}

// Operation describes a single method at a single path.
type Operation struct {
	// Tags group the operation in the documentation.
	Tags []string `json:"tags,omitzero"`
	// Summary is the one-line description.
	Summary string `json:"summary,omitzero"`
	// Description is the long-form description.
	Description string `json:"description,omitzero"`
	// OperationID uniquely identifies the operation.
	OperationID string `json:"operationId"`
	// Parameters describes the path, query, header and cookie parameters.
	Parameters []Parameter `json:"parameters,omitzero"`
	// RequestBody describes the JSON body, when the operation takes one.
	RequestBody *RequestBody `json:"requestBody,omitzero"`
	// Responses maps status codes to the responses they carry.
	Responses map[string]*Response `json:"responses"`
	// Deprecated marks the operation as no longer recommended.
	Deprecated bool `json:"deprecated,omitzero"`
}

// Parameter describes one path, query, header or cookie parameter.
type Parameter struct {
	// Name is the parameter name as it appears in the request.
	Name string `json:"name"`
	// In is where the parameter is read from: "path", "query", "header" or
	// "cookie".
	In string `json:"in"`
	// Description explains the parameter, taken from its doc struct tag.
	Description string `json:"description,omitzero"`
	// Required reports whether the request must supply the parameter.
	Required bool `json:"required,omitzero"`
	// Schema describes the accepted values.
	Schema *Schema `json:"schema"`
}

// RequestBody describes the body an operation accepts.
type RequestBody struct {
	// Description explains the body.
	Description string `json:"description,omitzero"`
	// Required reports whether the body must be present.
	Required bool `json:"required,omitzero"`
	// Content maps media types to their schemas.
	Content map[string]MediaType `json:"content"`
}

// MediaType pairs a media type with the schema of its payload.
type MediaType struct {
	// Schema describes the payload.
	Schema *Schema `json:"schema"`
}

// Response describes one outcome of an operation.
type Response struct {
	// Description explains when this response occurs. OpenAPI requires it.
	Description string `json:"description"`
	// Content maps media types to their schemas, and is absent for responses
	// with no body.
	Content map[string]MediaType `json:"content,omitzero"`
}

// Components holds the reusable schemas an operation refers to by name.
type Components struct {
	// Schemas maps a schema name to its definition.
	Schemas map[string]*Schema `json:"schemas,omitzero"`
}

// Schema is a JSON Schema 2020-12 description of a value, which is the schema
// dialect OpenAPI 3.1 uses.
type Schema struct {
	// Ref points at a named schema in the components section. When set, every
	// other field is empty.
	Ref string `json:"$ref,omitzero"`
	// Type is the JSON type, or a list of types when the value is nullable.
	Type any `json:"type,omitzero"`
	// Format refines the type, as "date-time" or "uuid" do for strings.
	Format string `json:"format,omitzero"`
	// Title names the schema in generated documentation.
	Title string `json:"title,omitzero"`
	// Description explains the value, taken from its doc struct tag.
	Description string `json:"description,omitzero"`
	// Properties describes each member of an object.
	Properties map[string]*Schema `json:"properties,omitzero"`
	// Required lists the members an object must carry.
	Required []string `json:"required,omitzero"`
	// Items describes the elements of an array.
	Items *Schema `json:"items,omitzero"`
	// AnyOf lists alternative schemas, used to widen a reference so that null
	// is also permitted.
	AnyOf []*Schema `json:"anyOf,omitzero"`
	// AdditionalProperties describes values of a map, or is false for an
	// object that accepts no extra members.
	AdditionalProperties any `json:"additionalProperties,omitzero"`
	// Default is the value used when the input omits this one.
	Default any `json:"default,omitzero"`
	// Enum lists the permitted values.
	Enum []any `json:"enum,omitzero"`
	// Pattern is a regular expression a string must match.
	Pattern string `json:"pattern,omitzero"`
	// MinLength and MaxLength bound a string's length.
	MinLength *int `json:"minLength,omitzero"`
	MaxLength *int `json:"maxLength,omitzero"`
	// Minimum and Maximum bound a number's value.
	Minimum *float64 `json:"minimum,omitzero"`
	Maximum *float64 `json:"maximum,omitzero"`
	// MultipleOf requires a number to divide evenly by this value.
	MultipleOf *float64 `json:"multipleOf,omitzero"`
	// MinItems and MaxItems bound an array's length.
	MinItems *int `json:"minItems,omitzero"`
	MaxItems *int `json:"maxItems,omitzero"`
	// UniqueItems requires an array's elements to differ.
	UniqueItems bool `json:"uniqueItems,omitzero"`
	// Deprecated marks the value as no longer recommended.
	Deprecated bool `json:"deprecated,omitzero"`
}

// Marshal renders the document as indented JSON. Map keys are emitted in
// sorted order so that the output is byte-for-byte reproducible, which is what
// lets a generated document be committed and diffed.
func (d *Document) Marshal() ([]byte, error) {
	return json.Marshal(d, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("  "))
}

// Document returns the generated OpenAPI description of the application,
// building it if necessary. It returns nil when documentation is disabled with
// [AppOptions.DisableDocs], and an error when the application does not build.
func (a *App) Document() (*Document, error) {
	if err := a.Build(); err != nil {
		return nil, err
	}
	return a.spec, nil
}

// buildDocument generates the OpenAPI description from the resolved routes.
// All of the reflection it performs happens here, once, so that nothing on the
// request path ever inspects a type.
func (a *App) buildDocument() *Document {
	doc := &Document{
		OpenAPI: OpenAPIVersion,
		Info: Info{
			Title:          orDefault(a.opts.Title, "Badele API"),
			Version:        orDefault(a.opts.Version, "0.1.0"),
			Description:    a.opts.Description,
			TermsOfService: a.opts.TermsOfService,
			Contact:        a.opts.Contact,
			License:        a.opts.License,
		},
		Servers: a.opts.Servers,
		Paths:   make(map[string]*PathItem),
	}
	builder := newSchemaBuilder()
	var tags []string

	for _, rt := range a.routes {
		if rt.Hidden {
			continue
		}
		item, ok := doc.Paths[rt.docPath()]
		if !ok {
			item = &PathItem{}
			doc.Paths[rt.docPath()] = item
		}
		if !item.set(rt.Method, a.operationFor(rt, builder)) {
			// A method OpenAPI has no slot for is routable but cannot be
			// described, so it is simply left out of the document.
			continue
		}
		tags = append(tags, rt.Tags...)
	}

	for _, name := range dedupeStrings(tags) {
		doc.Tags = append(doc.Tags, Tag{Name: name})
	}
	if len(builder.schemas) > 0 {
		doc.Components = &Components{Schemas: builder.schemas}
	}
	return doc
}

// docPath converts a route template into the form OpenAPI expects, where a
// wildcard is written as an ordinary parameter.
func (rt *Route) docPath() string {
	if !strings.Contains(rt.Path, "...") {
		return rt.Path
	}
	return strings.ReplaceAll(rt.Path, "...}", "}")
}

// orDefault returns value unless it is empty, in which case it returns
// fallback.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// operationFor describes a single route.
func (a *App) operationFor(rt *Route, builder *schemaBuilder) *Operation {
	op := &Operation{
		Tags:        rt.Tags,
		Summary:     rt.Summary,
		Description: rt.Description,
		OperationID: rt.OperationID,
		Deprecated:  rt.Deprecated,
		Responses:   make(map[string]*Response, 2+len(rt.responses)),
	}

	// Validation rules describe themselves, so the document carries the limits
	// the code actually enforces rather than a prose restatement of them.
	constraints := rt.constraintsForDocs()
	elements := rt.elementConstraintsForDocs()

	for i := range rt.plan.params {
		parameter := builder.parameterFor(&rt.plan.params[i])
		if c, described := constraints[parameter.Name]; described {
			applyConstraints(parameter.Schema, c)
			if c.Required {
				parameter.Required = true
			}
		}
		if c, described := elements[parameter.Name]; described && parameter.Schema.Items != nil {
			applyConstraints(parameter.Schema.Items, c)
		}
		op.Parameters = append(op.Parameters, parameter)
	}
	switch {
	case rt.plan.multipart:
		body := builder.multipartSchema(rt.plan)
		builder.applyBodyConstraints(body, constraints, elements)
		op.RequestBody = &RequestBody{
			Required: len(body.Required) > 0,
			Content:  multipartContent(rt.plan, body),
		}
	case rt.plan.body != nil:
		// The schema is built once and then annotated. Building it twice would
		// leave the constraints on a throwaway for a mixed input, whose body is
		// described inline rather than by reference.
		body := builder.bodySchema(rt.plan)
		builder.applyBodyConstraints(body, constraints, elements)
		op.RequestBody = &RequestBody{
			Required: rt.plan.body.required,
			Content:  map[string]MediaType{"application/json": {Schema: body}},
		}
	}

	op.Responses[strconv.Itoa(rt.Status)] = &Response{
		Description: orDefault(http.StatusText(rt.Status), "Success"),
		Content:     builder.responseContent(rt.outType),
	}
	if len(rt.plan.params) > 0 || rt.plan.body != nil || rt.plan.multipart {
		op.Responses[strconv.Itoa(http.StatusUnprocessableEntity)] = builder.errorResponse("The request could not be validated.")
	}
	for _, doc := range rt.responses {
		op.Responses[strconv.Itoa(doc.code)] = builder.errorResponse(doc.description)
	}
	if _, described := op.Responses["default"]; !described {
		op.Responses["default"] = builder.errorResponse("An unexpected error occurred.")
	}
	return op
}

// applyBodyConstraints writes a model's validation rules onto the schema its
// body was described with.
//
// The schema may be a reference into components, in which case the constraints
// land on the shared definition. That is correct: the rules belong to the type,
// so every operation that accepts it enforces them.

func (b *schemaBuilder) applyBodyConstraints(body *Schema, constraints, elements map[string]validate.Constraints) {
	if len(constraints) == 0 && len(elements) == 0 {
		return
	}
	schema := b.resolve(body)
	if schema == nil || schema.Properties == nil {
		return
	}
	for name, c := range constraints {
		property, described := schema.Properties[name]
		if !described {
			continue
		}
		applyConstraints(property, c)

		// A field the rules speak for is required exactly when they say so.
		// Without this the document would fall back to the Go type's shape,
		// which calls every non-pointer field required and would contradict a
		// model that deliberately left one optional.
		schema.Required = setRequired(schema.Required, name, c.Required)
	}
	for name, c := range elements {
		if property, described := schema.Properties[name]; described && property.Items != nil {
			applyConstraints(property.Items, c)
		}
	}
}

// resolve follows a reference back to the schema it names, so that constraints
// can be written onto the definition rather than onto the pointer to it.
func (b *schemaBuilder) resolve(schema *Schema) *Schema {
	if schema == nil || schema.Ref == "" {
		return schema
	}
	return b.schemas[strings.TrimPrefix(schema.Ref, componentPrefix)]
}

// setRequired adds or removes a name from a schema's required list, keeping it
// sorted so the generated document stays reproducible.
func setRequired(required []string, name string, want bool) []string {
	present := slices.Contains(required, name)
	switch {
	case want && !present:
		required = append(required, name)
		slices.Sort(required)
	case !want && present:
		required = slices.DeleteFunc(required, func(candidate string) bool {
			return candidate == name
		})
	}
	return required
}

// applyConstraints writes what a rule set demands onto a schema.
//
// A constraint the rules do not mention is left alone, so a format already
// derived from the Go type, such as date-time for a time.Time, survives a rule
// set that says nothing about it.
func applyConstraints(schema *Schema, c validate.Constraints) {
	if schema == nil {
		return
	}
	if c.Format != "" {
		schema.Format = c.Format
	}
	if c.Pattern != "" {
		schema.Pattern = c.Pattern
	}
	if c.MinLength != nil {
		schema.MinLength = c.MinLength
	}
	if c.MaxLength != nil {
		schema.MaxLength = c.MaxLength
	}
	if c.Minimum != nil {
		schema.Minimum = c.Minimum
	}
	if c.Maximum != nil {
		schema.Maximum = c.Maximum
	}
	if c.MultipleOf != nil {
		schema.MultipleOf = c.MultipleOf
	}
	if c.MinItems != nil {
		schema.MinItems = c.MinItems
	}
	if c.MaxItems != nil {
		schema.MaxItems = c.MaxItems
	}
	if c.UniqueItems {
		schema.UniqueItems = true
	}
	if len(c.Enum) > 0 {
		schema.Enum = c.Enum
	}
}

// schemaBuilder turns Go types into JSON schemas, hoisting every named struct
// into the components section so that a type used by several operations is
// described once and referenced thereafter.
type schemaBuilder struct {
	schemas map[string]*Schema
	byType  map[reflect.Type]*Schema
	names   map[string]reflect.Type
}

// newSchemaBuilder returns an empty builder.
func newSchemaBuilder() *schemaBuilder {
	return &schemaBuilder{
		schemas: map[string]*Schema{},
		byType:  map[reflect.Type]*Schema{},
		names:   map[string]reflect.Type{},
	}
}

// parameterFor describes one bound request parameter.
func (b *schemaBuilder) parameterFor(p *paramBinder) Parameter {
	schema := b.inline(p.typ)
	if p.hasDef {
		schema.Default = p.defValue
	}
	return Parameter{
		Name:        p.name,
		In:          p.source.String(),
		Description: p.doc,
		Required:    p.required,
		Schema:      schema,
	}
}

// bodySchema describes the JSON body a route accepts. When the whole input
// type is the body the named schema is referenced directly; when the input
// mixes located parameters with body members, an object is described inline
// from just the members that come from the body.
func (b *schemaBuilder) bodySchema(plan *bindPlan) *Schema {
	if plan.body.direct {
		return b.schemaFor(plan.typ)
	}
	schema := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for _, index := range plan.body.fields {
		field := plan.typ.FieldByIndex(index)
		name, optional := jsonFieldName(field)
		if name == "" {
			// coverage: body field indices come from the binding plan, which
			// already excludes located fields and those tagged "-", so
			// jsonFieldName cannot return an empty name here. The guard keeps a
			// future change to that plan from emitting a nameless property.
			continue
		}
		schema.Properties[name] = b.describeField(field)
		if !optional {
			schema.Required = append(schema.Required, name)
		}
	}
	slices.Sort(schema.Required)
	return schema
}

// multipartSchema describes the form body a route accepts, with one property
// per form value and one per file field. Files are described as the binary
// strings OpenAPI uses for them, so the documentation UI offers a file picker
// rather than a text box.
func (b *schemaBuilder) multipartSchema(plan *bindPlan) *Schema {
	schema := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for i := range plan.files {
		f := &plan.files[i]
		property := &Schema{Type: "string", Format: "binary"}
		if f.multi() {
			property = &Schema{Type: "array", Items: property}
		}
		property.Description = f.doc
		schema.Properties[f.name] = property
		if f.required {
			schema.Required = append(schema.Required, f.name)
		}
	}
	for i := range plan.form {
		p := &plan.form[i]
		property := b.inline(p.typ)
		property.Description = p.doc
		if p.hasDef {
			property.Default = p.defValue
		}
		schema.Properties[p.name] = property
		if p.required {
			schema.Required = append(schema.Required, p.name)
		}
	}
	slices.Sort(schema.Required)
	return schema
}

// multipartContent pairs the form schema with the media types the route
// accepts it under. A route that binds a file accepts only multipart, since
// that is the only encoding that can carry one; a route that binds form values
// alone also accepts what a plain HTML form posts.
func multipartContent(plan *bindPlan, schema *Schema) map[string]MediaType {
	content := map[string]MediaType{"multipart/form-data": {Schema: schema}}
	if len(plan.files) == 0 {
		content["application/x-www-form-urlencoded"] = MediaType{Schema: schema}
	}
	return content
}

// responseContent describes the body a successful response carries, or nothing
// at all for a route whose output type is [Empty].
func (b *schemaBuilder) responseContent(t reflect.Type) map[string]MediaType {
	if t == emptyType {
		return nil
	}
	if t == htmlType {
		return map[string]MediaType{"text/html": {Schema: &Schema{Type: "string"}}}
	}
	return map[string]MediaType{"application/json": {Schema: b.schemaFor(t)}}
}

// errorResponse describes a failure carrying the standard error envelope.
func (b *schemaBuilder) errorResponse(description string) *Response {
	return &Response{
		Description: description,
		Content: map[string]MediaType{
			"application/json": {Schema: b.schemaFor(reflect.TypeFor[ErrorResponse]())},
		},
	}
}

// schemaFor returns a schema for t, referencing a named component when t is a
// named struct and describing it inline otherwise.
func (b *schemaBuilder) schemaFor(t reflect.Type) *Schema {
	for t.Kind() == reflect.Pointer {
		// A pointer only affects whether a member is required, which the
		// enclosing object records; the schema itself describes the pointee.
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.Name() == "" || isWellKnown(t) {
		return b.inline(t)
	}
	if existing, ok := b.byType[t]; ok {
		return existing
	}
	name := b.nameFor(t)
	ref := &Schema{Ref: componentPrefix + name}
	// The reference is registered before the body is described so that a type
	// containing itself terminates instead of recursing forever.
	b.byType[t] = ref
	b.schemas[name] = &Schema{Type: "object"}
	b.schemas[name] = b.describeStruct(t)
	return ref
}

// nameFor picks a unique component name for a named type, qualifying it with
// its package when two packages export the same type name.
func (b *schemaBuilder) nameFor(t reflect.Type) string {
	name := t.Name()
	if existing, taken := b.names[name]; taken && existing != t {
		name = sanitizeSchemaName(shortPackage(t.PkgPath()) + "." + t.Name())
	}
	b.names[name] = t
	return name
}

// shortPackage returns the last element of an import path.
func shortPackage(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// sanitizeSchemaName reduces a name to the characters permitted in a component
// key.
func sanitizeSchemaName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// describeStruct builds the object schema for a struct type.
func (b *schemaBuilder) describeStruct(t reflect.Type) *Schema {
	schema := &Schema{Type: "object", Properties: map[string]*Schema{}}
	b.collectProperties(t, schema)
	slices.Sort(schema.Required)
	if len(schema.Properties) == 0 {
		schema.Properties = nil
	}
	return schema
}

// collectProperties adds a struct's fields to a schema, promoting the fields
// of an embedded struct the way JSON encoding does.
func (b *schemaBuilder) collectProperties(t reflect.Type, schema *Schema) {
	for i := range t.NumField() {
		field := t.Field(i)
		if !usableField(field) {
			continue
		}
		name, optional := jsonFieldName(field)
		if name == "" {
			continue
		}
		if field.Anonymous && field.Type.Kind() == reflect.Struct && field.Tag.Get(tagJSON) == "" {
			b.collectProperties(field.Type, schema)
			continue
		}
		schema.Properties[name] = b.describeField(field)
		if !optional {
			schema.Required = append(schema.Required, name)
		}
	}
}

// describeField builds the schema for one struct field, applying its doc and
// default tags.
func (b *schemaBuilder) describeField(field reflect.StructField) *Schema {
	schema := b.schemaFor(field.Type)
	if field.Type.Kind() == reflect.Pointer {
		// schemaFor looks through a pointer so that T and *T share one
		// component. At field level the pointer still means something, though:
		// the member may be null, and the schema has to say so.
		schema = nullable(schema)
	}
	doc := field.Tag.Get(tagDoc)
	def, hasDef := field.Tag.Lookup(tagDefault)
	if doc == "" && !hasDef {
		return schema
	}
	// The schema may be a shared *Schema or a reference into components, so
	// annotations go on a copy rather than mutating what other fields point
	// at. JSON Schema 2020-12, which OpenAPI 3.1 uses, allows a $ref to carry
	// sibling keywords, so a copied reference keeps its description.
	annotated := *schema
	annotated.Description = doc
	if hasDef {
		annotated.Default = def
	}
	return &annotated
}

// jsonFieldName returns the name a field is encoded under and whether it may
// be omitted. A field tagged "-" returns an empty name and is skipped.
func jsonFieldName(field reflect.StructField) (name string, optional bool) {
	tag := field.Tag.Get(tagJSON)
	name, rest, _ := strings.Cut(tag, ",")
	if name == "-" && rest == "" {
		return "", false
	}
	if name == "" {
		name = field.Name
	}
	optional = field.Type.Kind() == reflect.Pointer ||
		strings.Contains(rest, "omitempty") ||
		strings.Contains(rest, "omitzero") ||
		field.Tag.Get(tagRequired) == "false"
	if _, hasDefault := field.Tag.Lookup(tagDefault); hasDefault {
		optional = true
	}
	// A field read from somewhere other than the body is never a body member.
	if _, _, located := locationTag(field); located {
		return "", false
	}
	return name, optional
}

// isWellKnown reports whether a struct type has a natural JSON representation
// that should not be described field by field.
func isWellKnown(t reflect.Type) bool {
	return t == timeType || t == uuidType || isTextCoded(t)
}

// isTextCoded reports whether a type converts to and from text rather than to
// a JSON object, which is what a type implementing both text interfaces means.
// Such a type is a string everywhere it appears: in a parameter, where the
// binder parses it with UnmarshalText, and in a body, where encoding/json
// writes it with MarshalText. Describing it by walking its fields would
// document the Go struct rather than the value on the wire.
func isTextCoded(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(textUnmarshaler) && t.Implements(textMarshaler)
}

// inline describes a type without hoisting it into the components section.
func (b *schemaBuilder) inline(t reflect.Type) *Schema {
	switch t {
	case timeType:
		return &Schema{Type: "string", Format: "date-time"}
	case uuidType:
		return &Schema{Type: "string", Format: "uuid"}
	case durationType:
		return &Schema{Type: "string", Format: "duration", Description: "A Go duration such as 1500ms or 2h45m."}
	}
	if t.Kind() != reflect.String && isTextCoded(t) {
		return &Schema{Type: "string"}
	}

	switch t.Kind() {
	case reflect.Pointer:
		return nullable(b.schemaFor(t.Elem()))
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer", Format: intFormat(t)}
	case reflect.Float32:
		return &Schema{Type: "number", Format: "float"}
	case reflect.Float64:
		return &Schema{Type: "number", Format: "double"}
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			// A byte slice is encoded as a base64 string by encoding/json.
			return &Schema{Type: "string", Format: "byte"}
		}
		return &Schema{Type: "array", Items: b.schemaFor(t.Elem())}
	case reflect.Array:
		return &Schema{Type: "array", Items: b.schemaFor(t.Elem())}
	case reflect.Map:
		return &Schema{Type: "object", AdditionalProperties: b.schemaFor(t.Elem())}
	case reflect.Struct:
		return b.describeStruct(t)
	case reflect.Interface:
		// An empty schema accepts any JSON value, which is the honest
		// description of an interface-typed field.
		return &Schema{}
	default:
		return &Schema{Description: fmt.Sprintf("Values of Go type %s have no JSON representation.", t)}
	}
}

// intFormat reports the OpenAPI format for an integer type, which tells a
// client generator how wide the value can be.
func intFormat(t reflect.Type) string {
	switch t.Bits() {
	case 8, 16, 32:
		return "int32"
	default:
		return "int64"
	}
}

// nullable widens a schema so that null is permitted, which is how OpenAPI 3.1
// spells an optional pointer.
func nullable(schema *Schema) *Schema {
	if schema.Ref != "" {
		// A reference carries no type of its own to widen, so the alternative
		// is expressed as a choice between the reference and null.
		return &Schema{AnyOf: []*Schema{schema, {Type: "null"}}}
	}
	widened := *schema
	if text, ok := widened.Type.(string); ok {
		widened.Type = []string{text, "null"}
	}
	return &widened
}
