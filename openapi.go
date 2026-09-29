package muzak

import (
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
// pointer that a Required rule speaks for is required and not nullable. The
// response side keeps the shape, since a member that is not a pointer and not
// omitempty is always written; a type used both ways therefore has a component
// for the response and a copy for the request, named for it with Input after.
// Members of a form are required as the binder decides.
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
	}
	builder.sealResponses()
	for _, d := range operations {
		a.describeRequestBody(d.route, d.op, builder)
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
			if c.Required {
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

// describeRequestBody attaches the body a route accepts to its operation.
//
// It runs after every route's parameters and responses have been described,
// which is what tells it whether a type the body is made of was already
// described for a response.
func (a *App) describeRequestBody(rt *Route, op *Operation, builder *schemaBuilder) {
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
		for _, location := range locations {
			for name, property := range builder.resolve(body).Properties {
				if constraints[fieldKey{location, name}].Required && property != nil {
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
		body = builder.requireOnlyWhatIsEnforced(body, constraints, true)
		for _, nested := range rt.nestedModelsForDocs() {
			// A nested model's rules land on the component of its type, which
			// is where a reference to it leads. One that was described inline,
			// as an anonymous struct is, has no component to carry them.
			if ref, described := builder.byType[nested.typ]; described {
				builder.applyBodyConstraints(ref, nested.constraints, nested.elements, "body")
				builder.requireOnlyWhatIsEnforced(ref, nested.constraints, false)
			}
		}
		op.RequestBody = &RequestBody{
			Required: rt.plan.body.required,
			Content:  map[string]MediaType{"application/json": {Schema: body}},
		}
	}
}

// applyBodyConstraints writes a model's validation rules onto the schema its
// body was described with, for the fields read from any of the given
// locations.
//
// The schema may be a reference into components, in which case the constraints
// land on the shared definition. That is correct: the rules belong to the type,
// so every operation that accepts it enforces them.
//
// Which members are required is not decided here; see
// [schemaBuilder.requireOnlyWhatIsEnforced] for a JSON body, and the caller for
// a form.
func (b *schemaBuilder) applyBodyConstraints(body *Schema, constraints, elements map[fieldKey]validate.Constraints, locations ...string) {
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
			if c, described := constraints[fieldKey{location, name}]; described {
				applyConstraints(property, c)
			}
			if c, described := elements[fieldKey{location, name}]; described && property.Items != nil {
				applyConstraints(property.Items, c)
			}
		}
	}
}

// requireOnlyWhatIsEnforced lists a JSON body's required members as the ones
// the runtime refuses to be without, and returns the schema to describe the body
// with.
//
// The decoder accepts a body that leaves out any member: what it does not find
// it leaves as the zero value, and only a Required rule turns that into a
// failure, so only a member with one is required. Absence is what Required
// refuses in a pointer too, and null reads as absence there, so a required
// pointer member is no longer described as nullable. Listing the rest, as the Go type's shape suggests, would tell a client, or a gateway
// validating requests against the document, that a request the server accepts
// is malformed.
//
// A component is also described for the responses that carry it, and there the
// shape is the truth: a member that is not a pointer and not omitempty is always
// present. Rewriting it in place for the request would take that away from
// every client of the response, so a body type a response already described
// gets a component of its own for the request, named for the type with Input
// after it. A type only requests use keeps its name and is rewritten in place.
// The types nested within a body are held to the same rule where the rules
// speak for them, but are never split, since telling a parent to point at the
// copy would mean describing it again; one a response shares keeps the shape.
func (b *schemaBuilder) requireOnlyWhatIsEnforced(body *Schema, constraints map[fieldKey]validate.Constraints, split bool) *Schema {
	schema := b.resolve(body)
	if schema == nil || schema.Properties == nil {
		return body
	}
	var required []string
	for name := range schema.Properties {
		if constraints[fieldKey{"body", name}].Required {
			required = append(required, name)
		}
	}
	slices.Sort(required)

	if body.Ref == "" {
		// A schema described inline belongs to this operation alone.
		schema.Required = required
		refuseNull(schema, required)
		return body
	}
	name := strings.TrimPrefix(body.Ref, componentPrefix)
	signature := strings.Join(required, "\x00")
	if claimed, isClaimed := b.claimed[name]; isClaimed {
		// Another route already decided this component, and described it for
		// the same members, or this one takes a copy like any other.
		if claimed == signature {
			return body
		}
	} else if !b.responded[name] {
		b.claimed[name] = signature
		schema.Required = required
		refuseNull(schema, required)
		return body
	}
	if !split {
		return body
	}
	if slices.Equal(schema.Required, required) && !anyNullable(schema, required) {
		// The shape a response has is already what the request is.
		return body
	}
	return b.inputVariant(body, required)
}

// anyNullable reports whether any of the named members is described as
// accepting null.
func anyNullable(schema *Schema, names []string) bool {
	for _, name := range names {
		if property := schema.Properties[name]; property != nil && notNullable(property) != property {
			return true
		}
	}
	return false
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

// inputVariant returns a reference to a copy of a component whose required
// members are the ones given, creating it the first time it is asked for.
func (b *schemaBuilder) inputVariant(ref *Schema, required []string) *Schema {
	name := strings.TrimPrefix(ref.Ref, componentPrefix)
	key := name + "\x00" + strings.Join(required, "\x00")
	if variant, ok := b.variants[key]; ok {
		return variant
	}
	copied := *b.schemas[name]
	copied.Required = required
	// The properties are shared with the component the responses use, so the
	// ones narrowed for the request are set on a map of the copy's own.
	copied.Properties = maps.Clone(copied.Properties)
	refuseNull(&copied, required)
	variantName := name + "Input"
	for n := 2; ; n++ {
		if _, taken := b.names[variantName]; !taken {
			break
		}
		variantName = fmt.Sprintf("%sInput%d", name, n)
	}
	b.names[variantName] = reflect.TypeFor[inputVariantName]()
	b.schemas[variantName] = &copied
	variant := &Schema{Ref: componentPrefix + variantName}
	b.variants[key] = variant
	return variant
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
	// walking holds the named collection types being described, so that one
	// containing itself is described once rather than for ever.
	walking map[reflect.Type]bool
	// responded holds the components that existed once every response was
	// described, and claimed those a request body has since decided the
	// required members of. variants holds the copies made for a request where a
	// component could not be rewritten.
	responded map[string]bool
	claimed   map[string]string
	variants  map[string]*Schema
}

// newSchemaBuilder returns an empty builder.
func newSchemaBuilder() *schemaBuilder {
	return &schemaBuilder{
		schemas: map[string]*Schema{},
		byType:  map[reflect.Type]*Schema{},
		names:   map[string]reflect.Type{},
		walking: map[reflect.Type]bool{},

		responded: map[string]bool{},
		claimed:   map[string]string{},
		variants:  map[string]*Schema{},
	}
}

// parameterFor describes one bound request parameter.
func (b *schemaBuilder) parameterFor(p *paramBinder) Parameter {
	schema := b.inline(p.typ)
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
	name := componentName(t.Name())
	if existing, taken := b.names[name]; taken && existing != t {
		name = sanitizeSchemaName(shortPackage(t.PkgPath()) + "." + t.Name())
	}
	b.names[name] = t
	return name
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
		if field.Anonymous && field.Type.Kind() == reflect.Pointer && field.Type.Elem().Kind() == reflect.Struct &&
			field.Tag.Get(tagJSON) == "" && !isWellKnown(field.Type.Elem()) {
			// An embedded pointer is inlined like an embedded value, and its
			// members are where the client puts them, at the top level, not
			// under a member named for the type. Each is optional, since the
			// pointer stays nil when none of them is sent.
			promoted := &Schema{Properties: map[string]*Schema{}}
			b.collectProperties(field.Type.Elem(), promoted)
			for name, property := range promoted.Properties {
				schema.Properties[name] = property
			}
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

// notNullable is the inverse of [nullable]: it narrows a schema so that null is
// no longer permitted. A schema that never permitted it is returned as it is.
func notNullable(schema *Schema) *Schema {
	if len(schema.AnyOf) == 2 && schema.AnyOf[1].Type == "null" {
		// What a nullable reference was widened to: the reference is the schema,
		// and what the wrapper was annotated with goes with it.
		narrowed := *schema.AnyOf[0]
		if schema.Description != "" {
			narrowed.Description = schema.Description
		}
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
	return &narrowed
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
