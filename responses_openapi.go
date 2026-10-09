package muzak

import (
	"net/http"
	"reflect"
	"strings"
)

// ResponseHeader describes one header a response carries, as an OpenAPI
// Header Object does. The document lists the Location of a [Redirect] this
// way.
type ResponseHeader struct {
	// Description explains the header.
	Description string `json:"description,omitzero"`
	// Required reports whether every such response carries the header.
	Required bool `json:"required,omitzero"`
	// Schema describes the header's value.
	Schema *Schema `json:"schema"`
}

// successResponse describes what a route answers with when its handler
// succeeds, which its output type decides.
func (b *schemaBuilder) successResponse(rt *Route) *Response {
	response := &Response{Description: orDefault(http.StatusText(rt.Status), "Success")}
	switch kind := rt.output.kind; {
	case kind == outputRedirect:
		response.Headers = redirectHeaders()
	case kind.binary():
		response.Content = binaryContent(rt.output.produces)
	default:
		response.Content = b.responseContent(rt.outType)
	}
	return response
}

// typedContent describes the body of one of the output types that are not
// encoded, reporting false for every other type: a body of bytes as binary
// content of an unnamed type, and a [Redirect] as no body at all.
func typedContent(t reflect.Type) (map[string]MediaType, bool) {
	switch kind := outputKindOf(t); {
	case kind.binary():
		return binaryContent(nil), true
	case kind == outputRedirect:
		return nil, true
	}
	return nil, false
}

// typedHeaders describes the headers an output type always answers with,
// which is the Location of a [Redirect] and nothing for any other type.
func typedHeaders(t reflect.Type) map[string]*ResponseHeader {
	if t == redirectOutType {
		return redirectHeaders()
	}
	return nil
}

// binaryContent describes a body of bytes under each media type a route
// declared with [Produces], or under application/octet-stream when it
// declared none.
//
// Each is a string of format binary, which is what client generators read as
// "a file, not text to parse", together with OpenAPI 3.1's contentMediaType,
// which says the same thing in JSON Schema's own terms. A range such as
// "image/*" names no single type, so it carries no contentMediaType.
func binaryContent(contentTypes []string) map[string]MediaType {
	if len(contentTypes) == 0 {
		contentTypes = []string{"application/octet-stream"}
	}
	content := make(map[string]MediaType, len(contentTypes))
	for _, contentType := range contentTypes {
		schema := &Schema{Type: "string", Format: "binary"}
		if !strings.Contains(contentType, "*") {
			schema.ContentMediaType = contentType
		}
		content[contentType] = MediaType{Schema: schema}
	}
	return content
}

// redirectHeaders describes the Location every [Redirect] is sent with.
func redirectHeaders() map[string]*ResponseHeader {
	return map[string]*ResponseHeader{
		"Location": {
			Description: "Where the client is sent: a path on this origin, or an absolute URL to a host the route allows.",
			Required:    true,
			Schema:      &Schema{Type: "string", Format: "uri-reference"},
		},
	}
}
