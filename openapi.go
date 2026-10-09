package muzak

import (
	"cmp"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"muzak.dev/framework/validate"
)

// OpenAPIVersion is the specification version Muzak emits.
const OpenAPIVersion = "3.1.0"

// componentPrefix is the JSON pointer prefix under which named schemas live.
const componentPrefix = "#/components/schemas/"

// OpenAPIOptions describes the API in the generated document.
//
// It is embedded in [AppOptions], so its fields can be set inline:
//
//	muzak.AppOptions{Title: "Bigger Applications Example", Version: "1.0.0"}
type OpenAPIOptions struct {
	// Title names the API. It defaults to "Muzak API".
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
	// Tags describes the groups operations are sorted into, and decides the
	// order they are presented in. A route joins a group by naming it with
	// [WithTags]; describing it here is what gives the group a sentence of
	// explanation and a place in the running order:
	//
	//	Tags: []muzak.Tag{
	//		{Name: "items", Description: "Everything the catalogue holds."},
	//		{Name: "admin", Description: "Operations that need a staff token."},
	//	}
	//
	// A tag no route carries is left out of the document, since it would
	// document an empty group, and a tag some route carries but nothing here
	// describes still appears, after the described ones, in the order the
	// routes named it.
	Tags []Tag
	// SecuritySchemes declares the ways a client can authenticate, by the name
	// a route refers to them with in [WithSecurity]. It is documentation: the
	// schemes are written into the document so that a tool can offer the means
	// to authenticate, and nothing is enforced by declaring one. What refuses
	// a request is a [Guard], which Muzak can run but cannot read, so the
	// document says only what the application says about its guards here. An
	// application that declares none emits none.
	//
	//	SecuritySchemes: map[string]muzak.SecurityScheme{
	//		"bearer": muzak.BearerAuth("JWT"),
	//		"key":    muzak.APIKeyHeader("X-API-Key"),
	//	}
	//
	// A scheme is checked when the application is built: its type has to be
	// one OpenAPI defines, and carry what that type needs.
	SecuritySchemes map[string]SecurityScheme
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
	// Categories lists every distinct category an operation in the document
	// carries, in the order the routes were first registered, as the
	// "x-categories" vendor extension. It is what a documentation tool orders
	// its categories by, since the paths are listed in sorted order. It is
	// absent when no operation has a category. See [WithCategory].
	Categories []string `json:"x-categories,omitzero"`
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
	// Security lists the alternatives a client satisfies one of, as declared
	// with [WithSecurity]. It is absent for a route that declared nothing, and
	// an empty list, which is emitted, says the route needs no credentials.
	Security []SecurityRequirement `json:"security,omitzero"`
	// Category is the heading a documentation tool lists the operation under,
	// as set with [WithCategory]. It is written as the "x-category" vendor
	// extension, and only when the operation has one. A category is not a tag.
	Category string `json:"x-category,omitzero"`
	// Title is the human name a documentation tool lists the operation by in
	// place of its path, as set with [Title]. It is written as the "x-title"
	// vendor extension, and only when the operation has one.
	Title string `json:"x-title,omitzero"`
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
	// SecuritySchemes maps a name to the way of authenticating it stands for,
	// as declared in [OpenAPIOptions.SecuritySchemes].
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitzero"`
}

// Schema is a JSON Schema 2020-12 description of a value, which is the schema
// dialect OpenAPI 3.1 uses.
type Schema struct {
	// Ref points at a named schema in the components section. What is set
	// beside it applies together with it, as JSON Schema 2020-12 reads a $ref:
	// a description of the member that holds it, or the rules a model declares
	// for the members of that one use of the type.
	Ref string `json:"$ref,omitzero"`
	// Type is the JSON type, or a list of types when the value is nullable.
	Type any `json:"type,omitzero"`
	// Format refines the type, as "date-time" or "uuid" do for strings.
	Format string `json:"format,omitzero"`
	// ContentEncoding names the encoding a string carries binary data in,
	// which is "base64" for a byte slice or array in a JSON body.
	ContentEncoding string `json:"contentEncoding,omitzero"`
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
	// AllOf lists schemas a value must satisfy every one of, used where the
	// rules hold a value to more patterns, formats or multiples than one
	// keyword can say.
	AllOf []*Schema `json:"allOf,omitzero"`
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
	// Minimum and Maximum bound a number's value inclusively.
	Minimum *float64 `json:"minimum,omitzero"`
	Maximum *float64 `json:"maximum,omitzero"`
	// ExclusiveMinimum and ExclusiveMaximum bound it exclusively, for a rule
	// that rejects the bound itself rather than admitting it.
	ExclusiveMinimum *float64 `json:"exclusiveMinimum,omitzero"`
	ExclusiveMaximum *float64 `json:"exclusiveMaximum,omitzero"`
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
//
// Because it builds the application, call it only once every route, guard
// and middleware is in place: configuring the application afterwards panics,
// as it does after [App.Build].
//
// The document describes what the binder and the validation rules enforce. A
// member of a JSON request body is listed as required only when the runtime
// refuses a body without it, which is when a Required rule is declared for it:
// the decoder itself accepts a body that leaves any member out, whatever the Go
// type's shape suggests, so a member with no such rule is optional and a
// pointer that a Required rule speaks for is required and not nullable. That
// holds at every depth: an object the body nests requires only what the rules
// of a model nested with [Validation.Nested] require of it, and nothing when no
// rule speaks for it. The response side keeps the shape, since a member that is
// not a pointer and not omitempty is always written; a type used both ways
// therefore has a component for the response and a copy for the request, named
// for it with Input after, and the request's copy of an object refers to the
// request's copies of the objects it nests. Every object a request is read into
// says additionalProperties: false, since the decoder refuses a member it does
// not know at any depth, unless the route allows them with
// [AllowUnknownFields]; a response is left open, so that a member added to it
// later breaks no client. Members of a form are required as the binder decides.
//
// A field named by several rule sets is held to all of them, and so is its
// schema: the tightest of each bound, only the values every list allows, and,
// where they ask for more than one pattern, format or multiple, each of them
// under allOf.
//
// It describes authentication only as the application declares it. A guard is a
// function that Muzak can run but not read, so it cannot say whether it wants a
// bearer token, an API key or a session cookie; the schemes of
// [OpenAPIOptions.SecuritySchemes] and the requirements of [WithSecurity] are
// what the application says about its guards, and are emitted as it said them,
// as components.securitySchemes and a security list on each operation that
// declared one. Nothing is inferred and nothing is verified against the guards,
// and an application that declares none emits none.
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
			Title:          orDefault(a.opts.Title, "Muzak API"),
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

	// The request bodies are described after everything else. What a body
	// member is required to be is decided by what the binder refuses, which
	// is less than what the Go type's shape says a response always carries, so
	// a type the responses already described has to be told apart from one
	// only a request uses, and that is known once the responses are done.
	type documented struct {
		route *Route
		op    *Operation
	}
	var operations []documented
	// Categories are gathered with the position their route was registered at,
	// because the paths are listed in sorted order and say nothing of it.
	type categorized struct {
		rank     int
		category string
	}
	var categories []categorized
	ranks := a.registrationOrder()
	for _, rt := range a.routes {
		if rt.Hidden {
			continue
		}
		item, ok := doc.Paths[rt.docPath()]
		if !ok {
			item = &PathItem{}
			doc.Paths[rt.docPath()] = item
		}
		op := a.operationFor(rt, builder)
		operations = append(operations, documented{rt, op})
		if !item.set(rt.Method, op) {
			// A method OpenAPI has no slot for is routable but cannot be
			// described, so it is simply left out of the document.
			continue
		}
		tags = append(tags, rt.Tags...)
		if rt.Category != "" {
			categories = append(categories, categorized{ranks[rt.seq], rt.Category})
		}
	}
	slices.SortStableFunc(categories, func(x, y categorized) int { return cmp.Compare(x.rank, y.rank) })
	for _, c := range categories {
		if !slices.Contains(doc.Categories, c.category) {
			doc.Categories = append(doc.Categories, c.category)
		}
	}
	builder.sealResponses()
	// Every body is described, and the rules of its types written onto them,
	// before any is read as a request, so that what a request's copy of a
	// component is made from does not depend on which route reads it first.
	bodies := make([]*Schema, len(operations))
	for i, d := range operations {
		bodies[i] = a.describeRequestBody(d.route, d.op, builder)
	}
	for i, d := range operations {
		if bodies[i] != nil {
			a.readRequestBody(d.route, d.op, bodies[i], builder)
		}
	}

	doc.Tags = a.tagList(tags)
	schemes := a.opts.securitySchemesForDocs()
	if len(builder.schemas) > 0 || len(schemes) > 0 {
		doc.Components = &Components{Schemas: builder.schemas, SecuritySchemes: schemes}
	}
	return doc
}

// tagList describes and orders the groups the routes are sorted into.
//
// A tag described in [OpenAPIOptions.Tags] keeps the position it was declared
// at and the description it was given, which is how an application decides
// what its documentation leads with; a tag only a route names follows, in the
// order the routes named it. A described tag no route carries is dropped,
// because a documentation tool would otherwise render an empty group for it.
func (a *App) tagList(used []string) []Tag {
	names := dedupeStrings(used)
	if len(names) == 0 {
		return nil
	}
	carried := make(map[string]bool, len(names))
	for _, name := range names {
		carried[name] = true
	}

	tags := make([]Tag, 0, len(names))
	described := make(map[string]bool, len(a.opts.Tags))
	for _, tag := range a.opts.Tags {
		if !carried[tag.Name] || described[tag.Name] {
			continue
		}
		described[tag.Name] = true
		tags = append(tags, tag)
	}
	for _, name := range names {
		if !described[name] {
			tags = append(tags, Tag{Name: name})
		}
	}
	return tags
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
		Security:    rt.securityForDocs(),
		Category:    rt.Category,
		Title:       rt.Title,
		Responses:   make(map[string]*Response, 2+len(rt.responses)),
	}

	// Validation rules describe themselves, so the document carries the limits
	// the code actually enforces rather than a prose restatement of them. A
	// rule belongs to the parameter it was declared for and to no other field
	// of the same name, so each is looked up by where it is read from too.
	constraints := rt.constraintsForDocs()
	elements := rt.elementConstraintsForDocs()

	for i := range rt.plan.params {
		parameter := builder.parameterFor(&rt.plan.params[i])
		key := fieldKey{parameter.In, parameter.Name}
		if c, described := constraints[key]; described {
			applyConstraints(parameter.Schema, c)
			// A default is written before any rule runs, so a parameter that
			// has one is not refused for being left out, whatever Required says.
			if c.required() && !rt.plan.params[i].hasDef {
				parameter.Required = true
			}
		}
		if c, described := elements[key]; described && parameter.Schema.Items != nil {
			applyConstraints(parameter.Schema.Items, c)
		}
		op.Parameters = append(op.Parameters, parameter)
	}
	switch {
	case rt.websocket != nil:
		// A WebSocket route has no response body to describe. What it has is a
		// handshake, and what OpenAPI can say about one is that it answers 101
		// and the conversation continues off the document.
		op.Responses[strconv.Itoa(http.StatusSwitchingProtocols)] = &Response{
			Description: "Switching Protocols. The connection is upgraded and the conversation continues over WebSocket.",
		}
	case rt.sse != nil:
		// An event stream answers 200 like any other route, and then keeps
		// answering. The schema describes one event's data rather than the
		// whole body, because the body has no end to describe.
		op.Responses[strconv.Itoa(http.StatusOK)] = &Response{
			Description: "An event stream. The schema describes the data field of a single event.",
			Content:     builder.eventStreamContent(rt.outType),
		}
	default:
		op.Responses[strconv.Itoa(rt.Status)] = &Response{
			Description: orDefault(http.StatusText(rt.Status), "Success"),
			Content:     builder.responseContent(rt.outType),
		}
	}
	if len(rt.plan.params) > 0 || rt.plan.body != nil || rt.plan.multipart {
		op.Responses[strconv.Itoa(http.StatusUnprocessableEntity)] = builder.errorResponse("The request could not be validated.")
	}
	for _, doc := range rt.responses {
		op.Responses[strconv.Itoa(doc.code)] = builder.declaredResponse(doc)
	}
	if _, described := op.Responses["default"]; !described {
		op.Responses["default"] = builder.errorResponse("An unexpected error occurred.")
	}
	return op
}

// describeRequestBody describes the body a route accepts and writes the rules
// of the types it is made of onto their schemas. A form is attached to the
// operation there and then; a JSON body is returned, for [App.readRequestBody]
// to attach once every route's body has been described.
//
// It runs after every route's parameters and responses have been described,
// which is what tells the reading whether a type the body is made of was
// already described for a response.
func (a *App) describeRequestBody(rt *Route, op *Operation, builder *schemaBuilder) *Schema {
	constraints := rt.constraintsForDocs()
	elements := rt.elementConstraintsForDocs()

	switch {
	case rt.plan.multipart:
		body := builder.multipartSchema(rt.plan)
		// The binder already decided which form values are required, and it
		// enforces that whatever the rules say, so the rules may only add to
		// the list here. A rule that named itself with As() and is bound to
		// nothing in the model is reported under "body", so it is looked for
		// there too.
		locations := []string{srcForm.String(), srcFile.String(), "body"}
		builder.applyBodyConstraints(body, constraints, elements, locations...)
		defaulted := make(map[string]bool, len(rt.plan.form))
		for i := range rt.plan.form {
			defaulted[rt.plan.form[i].name] = rt.plan.form[i].hasDef
		}
		for _, location := range locations {
			for name, property := range builder.resolve(body).Properties {
				if constraints[fieldKey{location, name}].required() && property != nil && !defaulted[name] {
					body.Required = setRequired(body.Required, name, true)
				}
			}
		}
		op.RequestBody = &RequestBody{
			Required: len(body.Required) > 0,
			Content:  multipartContent(rt.plan, body),
		}
	case rt.plan.body != nil:
		// The schema is built once and then annotated. Building it twice would
		// leave the constraints on a throwaway for a mixed input, whose body is
		// described inline rather than by reference.
		body := builder.bodySchema(rt.plan)
		builder.applyBodyDefaults(body, rt.plan.body.defaults)
		builder.applyBodyConstraints(body, constraints, elements, "body")
		for _, nested := range rt.nestedModelsForDocs() {
			// A nested model's rules land on the component of its type, which
			// is where a reference to it leads. One that was described inline,
			// as an anonymous struct is, has no component to carry them.
			if ref, described := builder.byType[nested.typ]; described {
				builder.applyBodyConstraints(ref, nested.constraints, nested.elements, "body")
			}
		}
		return body
	}
	return nil
}

// readRequestBody attaches a JSON body to its operation, described as the route
// reads it; see [requestReading].
func (a *App) readRequestBody(rt *Route, op *Operation, body *Schema, builder *schemaBuilder) {
	reading := builder.newRequestReading(rt, body)
	op.RequestBody = &RequestBody{
		Required: rt.plan.body.required,
		Content:  map[string]MediaType{"application/json": {Schema: reading.body(body)}},
	}
}

// applyBodyConstraints writes a model's validation rules onto the schema its
// body was described with, for the fields read from any of the given
// locations.
//
// The schema may be a reference into components, in which case the constraints
// land on the shared definition. That is correct: the rules belong to the type,
// so every operation that accepts it enforces them. Each member is narrowed in
// a copy, though, since what it holds may be a schema another member shares.
// A rule on a member of a struct the type holds is not written here; see
// [requestReading.object].
//
// Which members are required is not decided here; see [requestReading] for a
// JSON body, and the caller for a form.
func (b *schemaBuilder) applyBodyConstraints(body *Schema, constraints, elements map[fieldKey]fieldConstraints, locations ...string) {
	if len(constraints) == 0 && len(elements) == 0 {
		return
	}
	schema := b.resolve(body)
	if schema == nil || schema.Properties == nil {
		return
	}
	// Locations are tried in the order given, so that when a form is described
	// the value a rule declared for a form field wins over one that only
	// borrowed its name.
	for _, location := range locations {
		for name, property := range schema.Properties {
			c, described := constraints[fieldKey{location, name}]
			element, describedElement := elements[fieldKey{location, name}]
			if !described && !describedElement {
				continue
			}
			narrowed := *property
			applyConstraints(&narrowed, c)
			if describedElement && narrowed.Items != nil {
				items := *narrowed.Items
				applyConstraints(&items, element)
				narrowed.Items = &items
			}
			schema.Properties[name] = &narrowed
		}
	}
}

// requestReading is how one route reads its JSON body, and every object the
// body nests, at whatever depth.
//
// The decoder accepts a body that leaves out any member: what it does not find
// it leaves as the zero value, and only a Required rule turns that into a
// failure, so a member is required only when one is declared for it. That holds
// for an object nested in the body as much as for the body itself, where the
// rules of a model the body nests speak for its members and nothing else does.
// Absence is what Required refuses in a pointer too, and null reads as absence
// there, so a required pointer member is no longer described as nullable.
// Listing more, as the Go type's shape suggests, tells a client, or a gateway
// validating requests against the document, that a request the server accepts
// is malformed. A member of the body with a default is not required either,
// since the default is written before the rules run.
//
// The decoder also refuses a member it does not know, at every depth, unless
// the route allows them with [AllowUnknownFields], so every object read from a
// struct says additionalProperties: false; saying nothing let a client, or a
// gateway, pass what the server answers with a 422. An object that collects
// unknown members in an embedded map already says what it takes instead, and
// a type that decodes itself is not closed, since what it accepts is its own
// business. A response is never closed: a client has to be free to read one
// that gains a member.
//
// A component is also described for the responses that carry it, and there the
// shape is the truth: a member that is not a pointer and not omitempty is always
// present. So a component a response describes is never rewritten. Where the
// request reads one differently, the request gets a copy of its own, named for
// the type with Input after it, and every object on the way to it gets a copy
// too, so that the copies refer to each other and the responses' components
// stay as they were. A type only requests use keeps its name and is rewritten in
// place the first time a route reads it; a route that reads it differently
// later gets a copy, and one that reads it the same shares it.
type requestReading struct {
	b *schemaBuilder
	// closed is set when the route refuses unknown members.
	closed bool
	// rules holds what the rules the route enforces say of the members of each
	// type the body is made of, by the name of the component describing the
	// type, and of the body itself under root, which is empty for a body
	// described inline; elements holds what they say of the elements of its
	// collections. defaulted holds the members of the body a default fills in.
	rules     map[string]map[fieldKey]fieldConstraints
	elements  map[string]map[fieldKey]fieldConstraints
	root      string
	defaulted map[string]bool
	// placed holds the component the request reads in place of each one the
	// body reaches, and pending the name of a copy decided on but not yet made.
	placed  map[string]string
	pending map[string]string
}

// newRequestReading collects what a route's rules say of the types its body is
// made of.
func (b *schemaBuilder) newRequestReading(rt *Route, body *Schema) *requestReading {
	r := &requestReading{
		b:         b,
		closed:    !rt.allowUnknownFields,
		rules:     map[string]map[fieldKey]fieldConstraints{},
		elements:  map[string]map[fieldKey]fieldConstraints{},
		root:      strings.TrimPrefix(body.Ref, componentPrefix),
		defaulted: map[string]bool{},
		placed:    map[string]string{},
		pending:   map[string]string{},
	}
	r.rules[r.root], r.elements[r.root] = rt.constraintsForDocs(), rt.elementConstraintsForDocs()
	for _, d := range rt.plan.body.defaults {
		r.defaulted[d.name] = true
	}
	for _, nested := range rt.nestedModelsForDocs() {
		if ref, described := b.byType[nested.typ]; described {
			name := strings.TrimPrefix(ref.Ref, componentPrefix)
			r.rules[name], r.elements[name] = nested.constraints, nested.elements
		}
	}
	return r
}

// body returns the schema the route's body is described with.
//
// Which component a request reads in place of another depends on what the ones
// it refers to became, and a type may refer to itself, so the choice is made
// again for every component until none changes; each is then written out
// referring to what the others became.
func (r *requestReading) body(body *Schema) *Schema {
	reach := r.b.reach(body)
	for _, name := range reach {
		r.placed[name] = name
	}
	for range len(reach) + 1 {
		changed := false
		for _, name := range reach {
			if target := r.choose(name); target != r.placed[name] {
				r.placed[name] = target
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	for _, name := range reach {
		r.commit(name)
	}
	if body.Ref != "" {
		return &Schema{Ref: componentPrefix + r.placed[r.root]}
	}
	// A body described inline belongs to this operation alone.
	return r.object("", body)
}

// choose decides which component the request reads in place of one it
// reaches: the component itself when it already says what the request needs,
// or when no response and no other route has a claim on it; a copy made
// earlier that says the same; or a new copy.
func (r *requestReading) choose(name string) string {
	if reflect.DeepEqual(r.readAs(name, name), r.b.schemas[name]) || (!r.b.responded[name] && !r.b.claimed[name]) {
		return name
	}
	for _, variant := range r.b.variants[name] {
		if reflect.DeepEqual(r.readAs(name, variant), r.b.schemas[variant]) {
			return variant
		}
	}
	if pending, decided := r.pending[name]; decided {
		return pending
	}
	variant := name + "Input"
	for n := 2; r.nameTaken(variant); n++ {
		variant = fmt.Sprintf("%sInput%d", name, n)
	}
	r.pending[name] = variant
	return variant
}

// nameTaken reports whether a component name is in use, or about to be.
func (r *requestReading) nameTaken(name string) bool {
	_, taken := r.b.names[name]
	return taken || slices.Contains(slices.Collect(maps.Values(r.pending)), name)
}

// commit writes out the reading of a component where it was placed.
func (r *requestReading) commit(name string) {
	target := r.placed[name]
	reading := r.readAs(name, target)
	switch {
	case target == name:
		if r.b.responded[name] {
			// The reading is the response's component as it stands.
			return
		}
		if _, saved := r.b.originals[name]; !saved {
			r.b.originals[name] = r.b.schemas[name]
		}
		r.b.schemas[name] = reading
		r.b.claimed[name] = true
	case r.pending[name] == target:
		r.b.names[target] = reflect.TypeFor[inputVariantName]()
		r.b.schemas[target] = reading
		r.b.variants[name] = append(r.b.variants[name], target)
	}
}

// readAs describes a component as the request reads it, were it placed under
// the given name, which is what a reference to itself then leads to.
func (r *requestReading) readAs(name, as string) *Schema {
	placed := r.placed[name]
	r.placed[name] = as
	defer func() { r.placed[name] = placed }()
	return r.object(name, r.b.original(name))
}

// object describes an object the route's rules may speak for, under the name
// of its component, or the body described inline under the empty name.
//
// A rule on a member of a struct the object holds, as v.Number(&in.Price.Amount)
// is, belongs to this use of the struct and to no other, so it narrows the
// member that holds the struct rather than the struct's own component; see
// [requestReading.overlay]. The member holding it is then required as well
// when the rule is Required, since a body without it fails that rule.
func (r *requestReading) object(name string, schema *Schema) *Schema {
	out := r.node(schema)
	if name != "" && decodesItself(r.b.names[name]) {
		out.AdditionalProperties = schema.AdditionalProperties
	}
	out.Required = nil
	rules := r.rules[name]
	for _, key := range slices.SortedFunc(maps.Keys(rules), compareFieldKeys) {
		c := rules[key]
		member, path, deeper := strings.Cut(key.name, ".")
		if key.location != "body" || schema.Properties[member] == nil {
			continue
		}
		if deeper {
			out.Properties[member] = r.overlay(out.Properties[member], schema.Properties[member], path, c, r.elements[name][key])
		}
		if c.required() && (deeper || name != r.root || !r.defaulted[member]) {
			out.Required = append(out.Required, member)
		}
	}
	slices.Sort(out.Required)
	out.Required = slices.Compact(out.Required)
	refuseNull(out, out.Required)
	return out
}

// overlay narrows the member at path within an object a body holds, for a rule
// declared on it by the model holding the object, and returns the narrowed
// copy of read, the schema the request reads the object with. described is the
// object as it was described, which says what the member is.
//
// Nothing shared is written to. An object described inline is this use's own,
// so its member is narrowed in a copy of it. A reference leads to a component
// every use of the type shares, and the rule was written on that shared
// reference once, so that refund and every response carried the order's rule
// on its price; the narrowing is written beside the reference instead, which
// JSON Schema 2020-12 applies together with it, for this use alone.
func (r *requestReading) overlay(read, described *Schema, path string, c, element fieldConstraints) *Schema {
	name, rest, deeper := strings.Cut(path, ".")
	if described.Ref != "" {
		described = r.b.original(strings.TrimPrefix(described.Ref, componentPrefix))
	}
	original := described.Properties[name]
	if original == nil {
		// A path walked from the members json/v2 resolves names a property at
		// every step. One a rule chose with As() may not, and then there is
		// nothing for it to narrow.
		return read
	}
	out := *read
	out.Properties = maps.Clone(read.Properties)
	if out.Properties == nil {
		out.Properties = map[string]*Schema{}
	}
	member := &Schema{}
	if existing := out.Properties[name]; existing != nil {
		copied := *existing
		member = &copied
	}
	required := c.required()
	if deeper {
		member = r.overlay(member, original, rest, c, element)
	} else {
		constrain(member, c, admitsNull(original) && !required)
		if len(element) > 0 {
			items := &Schema{}
			if member.Items != nil {
				copied := *member.Items
				items = &copied
			}
			constrain(items, element, original.Items != nil && admitsNull(original.Items))
			member.Items = items
		}
		if required && admitsNull(original) {
			if member.Type == nil && member.AnyOf == nil {
				// Only the narrowing is written here, beside a reference, so
				// the type that refuses null is said again without it.
				member.Type = notNullable(original).Type
			} else {
				member = notNullable(member)
			}
		}
	}
	if required {
		out.Required = setRequired(out.Required, name, true)
	}
	out.Properties[name] = member
	return &out
}

// compareFieldKeys orders fields by where they are read from and then by name.
func compareFieldKeys(x, y fieldKey) int {
	return cmp.Or(strings.Compare(x.location, y.location), strings.Compare(x.name, y.name))
}

// node copies a schema as the request reads it: every reference leads where
// the component it names was placed, an object read from a struct is closed
// when the route refuses unknown members, and an object described inline, as
// an anonymous struct is, requires nothing, since no rule can name its members.
func (r *requestReading) node(schema *Schema) *Schema {
	if schema == nil {
		return nil
	}
	out := *schema
	if schema.Ref != "" {
		out.Ref = componentPrefix + r.placed[strings.TrimPrefix(schema.Ref, componentPrefix)]
	}
	if r.closed && isStructObject(schema) {
		out.AdditionalProperties = false
	}
	if schema.Properties != nil {
		out.Properties = make(map[string]*Schema, len(schema.Properties))
		for member, property := range schema.Properties {
			out.Properties[member] = r.node(property)
		}
		out.Required = nil
	}
	out.Items = r.node(schema.Items)
	if values, ok := schema.AdditionalProperties.(*Schema); ok {
		out.AdditionalProperties = r.node(values)
	}
	if schema.AnyOf != nil {
		out.AnyOf = make([]*Schema, len(schema.AnyOf))
		for i, alternative := range schema.AnyOf {
			out.AnyOf[i] = r.node(alternative)
		}
	}
	return &out
}

// isStructObject reports whether a schema is an object read from a struct with
// no embedded map to collect the members it does not name. A map has its
// values as additional properties, and a reference has no type of its own.
func isStructObject(schema *Schema) bool {
	if schema.Ref != "" || schema.AdditionalProperties != nil {
		return false
	}
	if types, ok := schema.Type.([]string); ok {
		return slices.Contains(types, "object")
	}
	return schema.Type == "object"
}

// reach lists, in order, every component a schema leads to, at any depth.
func (b *schemaBuilder) reach(schema *Schema) []string {
	seen := map[string]bool{}
	var walk func(*Schema)
	walk = func(schema *Schema) {
		if schema == nil {
			return
		}
		if name := strings.TrimPrefix(schema.Ref, componentPrefix); schema.Ref != "" && !seen[name] {
			seen[name] = true
			walk(b.original(name))
		}
		for _, property := range schema.Properties {
			walk(property)
		}
		walk(schema.Items)
		if values, ok := schema.AdditionalProperties.(*Schema); ok {
			walk(values)
		}
		for _, alternative := range schema.AnyOf {
			walk(alternative)
		}
	}
	walk(schema)
	return slices.Sorted(maps.Keys(seen))
}

// original returns a component as it was described, before any request
// rewrote it in place.
func (b *schemaBuilder) original(name string) *Schema {
	if schema, rewritten := b.originals[name]; rewritten {
		return schema
	}
	return b.schemas[name]
}

// refuseNull stops describing the named members as nullable. It is for the
// members a Required rule refuses to be without, which reads a null as nothing
// having been sent.
func refuseNull(schema *Schema, names []string) {
	for _, name := range names {
		if property := schema.Properties[name]; property != nil {
			schema.Properties[name] = notNullable(property)
		}
	}
}

// inputVariantName stands in the table of component names for the ones given to
// the copy of a type made for a request, so that a type which is really called
// that is not given the same name.
type inputVariantName struct{}

// sealResponses records which components the responses, and the parameters, have
// described, which is every one that exists when it is called.
func (b *schemaBuilder) sealResponses() {
	for name := range b.schemas {
		b.responded[name] = true
	}
}

// applyBodyDefaults writes the defaults of a request body's members onto the
// schema it was described with, as the values the binder gives a member the
// client leaves out.
//
// The property is copied first. A property of a struct type is a reference
// shared by every member of that type, and the default belongs to this member
// alone.
func (b *schemaBuilder) applyBodyDefaults(body *Schema, defaults []bodyDefault) {
	schema := b.resolve(body)
	if schema == nil || schema.Properties == nil {
		return
	}
	for _, d := range defaults {
		property, described := schema.Properties[d.name]
		if !described {
			continue
		}
		annotated := *property
		annotated.Default = typedDefault(d.typ, d.raw)
		schema.Properties[d.name] = &annotated
	}
}

// typedDefault returns a default as the JSON type its schema describes, so an
// integer member shows 10 rather than "10". A type whose schema is a string
// keeps the text, and so does a default that does not parse, which is the
// parameter's own error to report.
func typedDefault(t reflect.Type, raw string) any {
	if t == durationType || isTextCoded(t) {
		return raw
	}
	switch t.Kind() {
	case reflect.Pointer:
		return typedDefault(t.Elem(), raw)
	case reflect.Slice:
		// The binder reads a default as the one value the list is given.
		return []any{typedDefault(t.Elem(), raw)}
	case reflect.Bool:
		if v, err := strconv.ParseBool(raw); err == nil {
			return v
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v, err := strconv.ParseInt(raw, 10, t.Bits()); err == nil {
			return v
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v, err := strconv.ParseUint(raw, 10, t.Bits()); err == nil {
			return v
		}
	case reflect.Float32, reflect.Float64:
		if v, err := strconv.ParseFloat(raw, t.Bits()); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v
		}
	}
	return raw
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

// applyConstraints writes what the rule sets declared for a field demand onto a
// schema.
//
// A constraint the rules do not mention is left alone, so a format already
// derived from the Go type, such as date-time for a time.Time, survives a rule
// set that says nothing about it.
func applyConstraints(schema *Schema, f fieldConstraints) {
	if schema == nil {
		return
	}
	constrain(schema, f, admitsNull(schema))
}

// constrain is [applyConstraints] for a schema that may say less than the value
// it constrains, as one written beside a reference does, so whether the value
// admits null is given rather than read off the schema.
func constrain(schema *Schema, f fieldConstraints, nullable bool) {
	c, also := f.merged()
	if len(also) > 0 {
		schema.AllOf = also
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
	if c.ExclusiveMinimum != nil {
		schema.ExclusiveMinimum = c.ExclusiveMinimum
	}
	if c.ExclusiveMaximum != nil {
		schema.ExclusiveMaximum = c.ExclusiveMaximum
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
	if c.Enum != nil {
		schema.Enum = slices.Clone(c.Enum)
		// A nil pointer skips its rules, so a member whose type admits null is
		// still accepted as null whatever the list says. JSON Schema reads the
		// two keywords together, and a list without null next to a type with it
		// refused a request the server takes. A Required rule refuses the null,
		// and notNullable takes it out of the list again.
		if nullable && !slices.Contains(schema.Enum, nil) {
			schema.Enum = append(schema.Enum, nil)
		}
	}
}

// merged combines the rule sets declared for one field into what they demand
// together, since each is enforced: Required if any says it, the tightest of
// each bound, and only the values every list allows, which may be none.
//
// A pattern, a format and a multiple each take one keyword. Where the rule sets
// name more than one of a kind, the keyword is left unset and every one of them
// is returned instead, as a schema the value has to satisfy as well, which is
// what allOf says; describing only one let through what the others refuse.
func (f fieldConstraints) merged() (validate.Constraints, []*Schema) {
	var c validate.Constraints
	var patterns, formats []string
	var multiples []float64
	for _, d := range f {
		c.Required = c.Required || d.Required
		c.UniqueItems = c.UniqueItems || d.UniqueItems
		patterns = appendDistinct(patterns, d.Pattern)
		formats = appendDistinct(formats, d.Format)
		if d.MultipleOf != nil {
			multiples = appendDistinct(multiples, *d.MultipleOf)
		}
		c.MinLength = tighter(c.MinLength, d.MinLength, true)
		c.MaxLength = tighter(c.MaxLength, d.MaxLength, false)
		c.Minimum = tighter(c.Minimum, d.Minimum, true)
		c.Maximum = tighter(c.Maximum, d.Maximum, false)
		c.ExclusiveMinimum = tighter(c.ExclusiveMinimum, d.ExclusiveMinimum, true)
		c.ExclusiveMaximum = tighter(c.ExclusiveMaximum, d.ExclusiveMaximum, false)
		c.MinItems = tighter(c.MinItems, d.MinItems, true)
		c.MaxItems = tighter(c.MaxItems, d.MaxItems, false)
		switch {
		case len(d.Enum) == 0:
		case c.Enum == nil:
			c.Enum = slices.Clone(d.Enum)
		default:
			c.Enum = slices.DeleteFunc(c.Enum, func(value any) bool {
				return !slices.ContainsFunc(d.Enum, func(other any) bool { return reflect.DeepEqual(value, other) })
			})
		}
	}
	var also []*Schema
	if len(patterns) == 1 {
		c.Pattern = patterns[0]
	} else {
		for _, pattern := range patterns {
			also = append(also, &Schema{Pattern: pattern})
		}
	}
	if len(formats) == 1 {
		c.Format = formats[0]
	} else {
		for _, format := range formats {
			also = append(also, &Schema{Format: format})
		}
	}
	if len(multiples) == 1 {
		c.MultipleOf = &multiples[0]
	} else {
		for i := range multiples {
			also = append(also, &Schema{MultipleOf: &multiples[i]})
		}
	}
	return c, also
}

// appendDistinct appends a value to a list unless it is the zero value or the
// list already holds it.
func appendDistinct[T comparable](list []T, value T) []T {
	var zero T
	if value == zero || slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

// tighter returns the stricter of two bounds, the larger of two lower bounds or
// the smaller of two upper ones, or whichever of them is set.
func tighter[T cmp.Ordered](current, next *T, lower bool) *T {
	switch {
	case next == nil:
		return current
	case current == nil:
		return next
	}
	if (*next > *current) == lower {
		return next
	}
	return current
}

// admitsNull reports whether a schema accepts null, the way [nullable] spells
// it.
func admitsNull(schema *Schema) bool {
	if types, ok := schema.Type.([]string); ok {
		return slices.Contains(types, "null")
	}
	return len(schema.AnyOf) == 2 && schema.AnyOf[1].Type == "null"
}

// schemaBuilder turns Go types into JSON schemas, hoisting every named struct
// into the components section so that a type used by several operations is
// described once and referenced thereafter.
type schemaBuilder struct {
	schemas map[string]*Schema
	byType  map[reflect.Type]*Schema
	names   map[string]reflect.Type
	// walking holds the named collection types being described, so that one
	// containing itself is described once rather than for ever.
	walking map[reflect.Type]bool
	// responded holds the components that existed once every response was
	// described, and claimed those a request has since read in place, which
	// another request may share but not rewrite. originals keeps a component a
	// request rewrote as it was described, and variants the copies made of
	// each for requests that read it differently. See [requestReading].
	responded map[string]bool
	claimed   map[string]bool
	originals map[string]*Schema
	variants  map[string][]string
}

// newSchemaBuilder returns an empty builder.
func newSchemaBuilder() *schemaBuilder {
	return &schemaBuilder{
		schemas: map[string]*Schema{},
		byType:  map[reflect.Type]*Schema{},
		names:   map[string]reflect.Type{},
		walking: map[reflect.Type]bool{},

		responded: map[string]bool{},
		claimed:   map[string]bool{},
		originals: map[string]*Schema{},
		variants:  map[string][]string{},
	}
}

// parameterFor describes one bound request parameter.
func (b *schemaBuilder) parameterFor(p *paramBinder) Parameter {
	schema := b.textSchema(p.typ)
	if p.hasDef {
		schema.Default = typedDefault(p.typ, p.defValue)
	}
	return Parameter{
		Name:        p.name,
		In:          p.source.String(),
		Description: p.doc,
		Required:    p.required,
		Schema:      schema,
	}
}

// textSchema describes a type as a request parameter or a form value carries
// it, which is as text the binder converts, not as JSON.
//
// It differs from [schemaBuilder.inline] for a byte slice. A JSON body carries
// one as base64, but the binder reads each value of a parameter as one element,
// so `?data=65` is []byte{65} and a base64 string is refused. It is described
// as the list of numbers it is.
func (b *schemaBuilder) textSchema(t reflect.Type) *Schema {
	if isTextCoded(t) {
		return b.inline(t)
	}
	switch t.Kind() {
	case reflect.Pointer:
		return nullable(b.textSchema(t.Elem()))
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 && !isTextCoded(t.Elem()) {
			zero, top := 0.0, float64(math.MaxUint8)
			return &Schema{Type: "array", Items: &Schema{Type: "integer", Format: "int32", Minimum: &zero, Maximum: &top}}
		}
		return &Schema{Type: "array", Items: b.textSchema(t.Elem())}
	}
	return b.inline(t)
}

// bodySchema describes the JSON body a route accepts. When the whole input
// type is the body the named schema is referenced directly; when the input
// mixes located parameters with body members, an object is described inline
// from just the members that come from the body.
//
// The decoder reads a mixed body into the whole input type, so its members are
// the ones json/v2 resolves over that whole type, and the fields read from the
// path, the query, a header or a cookie are then left out: none of them is a
// member a client may send.
func (b *schemaBuilder) bodySchema(plan *bindPlan) *Schema {
	if plan.body.direct {
		return b.schemaFor(plan.typ)
	}
	return b.describeObject(jsonMembers(plan.typ), func(member jsonMember) bool {
		_, located := declaredLocation(member.field)
		// A Dep is filled by a provider and never sent, so it is no member.
		return located || isDepField(member.field)
	})
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
		property := b.textSchema(p.typ)
		property.Description = p.doc
		if p.hasDef {
			property.Default = typedDefault(p.typ, p.defValue)
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

// eventStreamContent describes the events a server-sent events route streams,
// or nothing at all for one whose output type is [Empty], which is how a
// stream of text that is not JSON declares itself.
//
// OpenAPI 3.1 has no way to say "many of these, one after another", so the
// schema given for text/event-stream is the one a single event's data field
// carries. That is what the tools which understand event streams read it as,
// and it is the only part a client has to be able to decode.
func (b *schemaBuilder) eventStreamContent(t reflect.Type) map[string]MediaType {
	if t == emptyType {
		return nil
	}
	return map[string]MediaType{"text/event-stream": {Schema: b.schemaFor(t)}}
}

// declaredResponse describes an outcome declared at registration with
// [WithResponseDoc] or [WithResponseModel].
//
// A declaration that named no model is the error envelope, because an outcome
// a handler reports by returning an error is answered with one; a declaration
// that named a model is described exactly as a handler's own return type would
// be, so that an operation can carry a different schema per status code. A
// description left empty falls back to the status code's standard reason
// phrase, which OpenAPI requires a response to carry and which is what the
// declaration would have said anyway.
func (b *schemaBuilder) declaredResponse(doc responseDoc) *Response {
	description := orDefault(doc.description, orDefault(http.StatusText(doc.code), "Response"))
	if doc.model == nil {
		return b.errorResponse(description)
	}
	return &Response{
		Description: description,
		Content:     b.responseContent(doc.model),
	}
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
		switch t.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			// A collection type can only contain itself by being named, as in
			// `type Tree map[string]Tree`, and is described inline, so the
			// second visit is the recursion. It is left open, which is what a
			// tree of any depth is in a schema with no component to point at.
			if t.Name() != "" {
				if b.walking[t] {
					return &Schema{}
				}
				b.walking[t] = true
				defer delete(b.walking, t)
			}
		}
		return b.inline(t)
	}
	// Every use of the type is given a reference of its own. Handing out the
	// one kept here made every member of the type one schema, so a rule
	// written onto one member's reference was written onto all of them.
	if existing, ok := b.byType[t]; ok {
		ref := *existing
		return &ref
	}
	name := b.nameFor(t)
	// The reference is registered before the body is described so that a type
	// containing itself terminates instead of recursing forever.
	b.byType[t] = &Schema{Ref: componentPrefix + name}
	b.schemas[name] = &Schema{Type: "object"}
	b.schemas[name] = b.describeStruct(t)
	return &Schema{Ref: componentPrefix + name}
}

// nameFor picks a unique component name for a named type.
//
// A type is named for itself while that name is free. One that shares its name
// with a type already described is qualified by its import path, an element at
// a time from the end, so that Item in v1/models, v2/models and v3/models is
// Item, models.Item and v3.models.Item. A qualifier only tells packages apart,
// so once a candidate is held by a type of the same package, as it is for
// three types declared inside three functions, a number is appended instead.
//
// Every candidate is checked before it is taken. The qualified name used to be
// handed to a third type without asking, which pointed the second type's
// references at the third type's schema. The types are met in the order the
// routes were registered, so the names come out the same on every run.
func (b *schemaBuilder) nameFor(t reflect.Type) string {
	claim := func(name string) bool {
		if holder, taken := b.names[name]; taken && holder != t {
			return false
		}
		b.names[name] = t
		return true
	}
	base := componentName(t.Name())
	if claim(base) {
		return base
	}
	// Only a named struct is given a component, and a named type always
	// belongs to a package, so the path is never empty.
	path := t.PkgPath()
	segments := strings.Split(path, "/")
	for i := len(segments) - 1; i >= 0; i-- {
		candidate := sanitizeSchemaName(strings.Join(segments[i:], ".") + "." + base)
		if claim(candidate) {
			return candidate
		}
		if b.names[candidate].PkgPath() == path {
			// The holder lives in this package too, so no more of the path can
			// tell the two apart.
			break
		}
	}
	qualified := sanitizeSchemaName(shortPackage(path) + "." + base)
	for n := 2; ; n++ {
		if candidate := fmt.Sprintf("%s%d", qualified, n); claim(candidate) {
			return candidate
		}
	}
}

// componentName turns the name reflect gives a type into one that is a valid
// component key. A generic type is named with its arguments, including the
// import path of each, as in Page[example.com/app.Item], and a reference to a
// component whose key holds a slash or a bracket does not resolve: the slash
// splits the JSON pointer, and the bracket is not a character a key may hold.
// Each argument keeps the package and name it is known by and loses the
// import path, so Page[example.com/app.Item] is Page_app.Item.
func componentName(name string) string {
	if !strings.ContainsAny(name, "[/") {
		return name
	}
	var b strings.Builder
	var word strings.Builder
	flush := func() {
		text := word.String()
		if i := strings.LastIndexByte(text, '/'); i >= 0 {
			text = text[i+1:]
		}
		b.WriteString(text)
		word.Reset()
	}
	for _, r := range name {
		switch r {
		case '[', ']', ',', ' ', '*':
			flush()
			b.WriteByte('_')
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return strings.Trim(sanitizeSchemaName(b.String()), "_")
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
	return b.describeObject(jsonMembers(t), nil)
}

// describeObject builds the schema of the object a struct encodes as, from the
// members json/v2 gives it, leaving out any that skip reports.
//
// The members are json/v2's own, so the fields of an embedded struct, or of a
// field tagged with the embed option, are where a client puts them, at the top
// level and not under a member named for the type; a member an outer field of
// the same name shadows is not described, nor one two fields at the same depth
// both claim, since json/v2 neither writes nor reads it; and the members an
// embedded map collects are described as the object's additional properties.
// Each member is listed once, so required names none twice.
func (b *schemaBuilder) describeObject(object jsonObject, skip func(jsonMember) bool) *Schema {
	schema := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for _, member := range object.members {
		if skip != nil && skip(member) {
			continue
		}
		schema.Properties[member.name] = b.describeMember(member)
		if !member.mayBeAbsent() {
			schema.Required = append(schema.Required, member.name)
		}
	}
	slices.Sort(schema.Required)
	if len(schema.Properties) == 0 {
		schema.Properties = nil
	}
	if object.fallback != nil {
		// Every member the struct does not name is read into the map, so any
		// is accepted, as a value of the map's element type.
		schema.AdditionalProperties = &Schema{}
		if target := embeddedType(object.fallback.field.Type); target != jsontextValueType {
			schema.AdditionalProperties = b.schemaFor(target.Elem())
		}
	}
	return schema
}

// describeMember builds the schema for one member, applying its doc tag and
// what its json tag says about how it is written.
func (b *schemaBuilder) describeMember(member jsonMember) *Schema {
	field := member.field
	value := field.Type
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	var schema *Schema
	if member.stringify && isPlainNumber(value) {
		// The string option writes the number inside a string and reads it
		// only from one, so a bare number is refused.
		schema = quotedNumber(value)
	} else {
		schema = b.schemaFor(field.Type)
	}
	if field.Type.Kind() == reflect.Pointer {
		// schemaFor looks through a pointer so that T and *T share one
		// component. At field level the pointer still means something, though:
		// the member may be null, and the schema has to say so.
		schema = nullable(schema)
	}
	doc := field.Tag.Get(tagDoc)
	if doc == "" {
		return schema
	}
	// A default is not written here. The tag is honoured for the members of a
	// request body, at its top level, and describing the type says nothing
	// about which of those a request is: the same struct may be a response, or
	// sit inside a collection, where nothing fills the default in. It is
	// written by [schemaBuilder.applyBodyDefaults] instead.
	//
	// The schema may be a shared *Schema or a reference into components, so
	// annotations go on a copy rather than mutating what other fields point
	// at. JSON Schema 2020-12, which OpenAPI 3.1 uses, allows a $ref to carry
	// sibling keywords, so a copied reference keeps its description.
	annotated := *schema
	annotated.Description = doc
	return &annotated
}

// jsonFieldName returns the name a field is encoded under and whether it may
// be omitted, read from its tag the way json/v2 reads it. A field tagged "-"
// returns an empty name and is skipped, and so does one read from somewhere
// other than the body, which is never a body member.
func jsonFieldName(field reflect.StructField) (name string, optional bool) {
	tag, ignored := parseJSONTag(field)
	if _, _, located := locationTag(field); ignored || located {
		return "", false
	}
	return tag.name, jsonMember{jsonTag: tag, field: field}.mayBeAbsent()
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
		// Not format "duration": that is ISO 8601, such as PT1.5S, and a client
		// that checks formats would refuse the "1500ms" the binder and the JSON
		// codec read. The text is described by what it looks like instead.
		return &Schema{Type: "string", Pattern: durationPattern, Description: "A Go duration such as 1500ms or 2h45m."}
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
		if isRawByte(t.Elem()) {
			// A byte slice is encoded as a base64 string by encoding/json.
			return &Schema{Type: "string", Format: "byte", ContentEncoding: "base64"}
		}
		return &Schema{Type: "array", Items: b.schemaFor(t.Elem())}
	case reflect.Array:
		if isRawByte(t.Elem()) {
			// So is a byte array, and json/v2 reads it back only from padded
			// base64 of exactly as many bytes, so the length is known too.
			low, high := base64.StdEncoding.EncodedLen(t.Len()), base64.StdEncoding.EncodedLen(t.Len())
			return &Schema{Type: "string", Format: "byte", ContentEncoding: "base64", MinLength: &low, MaxLength: &high}
		}
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

// durationPattern matches the text time.ParseDuration reads: a sign, then
// either a bare zero or one or more numbers each followed by a unit.
//
// The two micro signs, U+00B5 and U+03BC, are written as escapes so that the
// pattern holds the characters themselves, which is what every regular
// expression dialect reads alike.
const durationPattern = `^[-+]?(0|((\d+(\.\d*)?|\.\d+)(ns|us|` + "\u00b5s|\u03bcs" + `|ms|s|m|h))+)$`

// The text json/v2 reads a number from under the string option: a JSON number
// and nothing else, with no sign but a minus, no leading zero and no space, and
// for an integer no fraction or exponent either.
const (
	quotedIntegerPattern  = `^-?(0|[1-9][0-9]*)$`
	quotedUnsignedPattern = `^(0|[1-9][0-9]*)$`
	quotedNumberPattern   = `^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`
)

// isPlainNumber reports whether json/v2 writes a type as a JSON number of its
// own accord, which is what the string option quotes. A type that writes
// itself, and a duration, which the binder writes as text, are not.
func isPlainNumber(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return t != durationType && !encodesItself(t)
	}
	return false
}

// quotedNumber describes a number written inside a string, by the text json/v2
// reads it back from.
func quotedNumber(t reflect.Type) *Schema {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &Schema{Type: "string", Pattern: quotedIntegerPattern}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "string", Pattern: quotedNumberPattern}
	default:
		return &Schema{Type: "string", Pattern: quotedUnsignedPattern}
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

// notNullable is the inverse of [nullable]: it narrows a schema so that null is
// no longer permitted, in its type and in any list of values it is held to. A
// schema that never permitted it is returned as it is.
func notNullable(schema *Schema) *Schema {
	if len(schema.AnyOf) == 2 && schema.AnyOf[1].Type == "null" {
		// What a nullable reference was widened to: the reference is the schema,
		// and what the wrapper was annotated with goes with it.
		narrowed := *schema
		narrowed.AnyOf = nil
		narrowed.Ref = schema.AnyOf[0].Ref
		narrowed.Enum = withoutNull(schema.Enum)
		return &narrowed
	}
	types, ok := schema.Type.([]string)
	if !ok {
		return schema
	}
	i := slices.Index(types, "null")
	if i < 0 || len(types) != 2 {
		return schema
	}
	narrowed := *schema
	narrowed.Type = types[1-i]
	narrowed.Enum = withoutNull(schema.Enum)
	return &narrowed
}

// withoutNull returns a list of values with null taken out, leaving the list
// itself alone since it may be shared.
func withoutNull(values []any) []any {
	if !slices.Contains(values, nil) {
		return values
	}
	return slices.DeleteFunc(slices.Clone(values), func(value any) bool { return value == nil })
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
