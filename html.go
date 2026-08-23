package muzak

import (
	"io"
	"reflect"
	"strconv"
)

// htmlType is what the document generator matches an output type against to
// describe a response as text/html.
var htmlType = reflect.TypeFor[HTML]()

// HTML is a response body written as an HTML document instead of as JSON.
//
// A handler that returns it bypasses JSON encoding entirely: the string is
// written verbatim under a text/html content type, and the generated document
// describes the response as text/html rather than as a JSON schema. Everything
// else about the route is unchanged, so its status, headers and errors work
// exactly as they do for a JSON route.
//
//	r.Get("/", func(ctx *muzak.Context, _ muzak.Empty) (muzak.HTML, error) {
//		return muzak.HTML("<h1>Hello</h1>"), nil
//	})
//
// The value is written as given. Muzak does not escape it, because it cannot
// tell markup the handler meant from text it did not: interpolating anything a
// client supplied is the handler's job to escape, with html/template or
// html.EscapeString.
type HTML string

// writeHTML writes an HTML document as the response body.
func (c *Context) writeHTML(status int, document HTML) error {
	header := c.w.Header()
	setIfAbsent(header, "Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(document)))
	c.w.WriteHeader(status)
	_, err := io.WriteString(c.w, string(document))
	return err
}
