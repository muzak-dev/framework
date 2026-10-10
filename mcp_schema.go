package muzak

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// This file describes a tool from the operation the OpenAPI document holds for
// its route, so that a tool's schemas say exactly what the document says.
//
// The input is grouped by where the route reads it: an object with a member
// for each part of the request the route reads, "path", "query", "header" and
// "cookie" each holding the parameters read from there by the names a request
// uses, "body" holding the JSON body as the document describes it, and "form"
// the fields of a form body, files among them as base64. Grouping is what
// keeps a path parameter and a body member of the same name, which a route
// may well have, from colliding, and it is the shape the request takes.

// mcpToolDefinition is a tool as tools/list lists it.
type mcpToolDefinition struct {
	Name         string              `json:"name"`
	Title        string              `json:"title,omitzero"`
	Description  string              `json:"description,omitzero"`
	InputSchema  jsontext.Value      `json:"inputSchema"`
	OutputSchema jsontext.Value      `json:"outputSchema,omitzero"`
	Annotations  *mcpToolAnnotations `json:"annotations,omitzero"`
}

// mcpToolAnnotations are the hints a tool's HTTP method gives about it. A
// client treats them as hints from the server, which is what they are.
type mcpToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitzero"`
	DestructiveHint *bool `json:"destructiveHint,omitzero"`
	IdempotentHint  *bool `json:"idempotentHint,omitzero"`
}

// mcpAnnotations says what an HTTP method promises: GET, HEAD and OPTIONS
// change nothing, DELETE removes something and doing it twice is doing it
// once, and so is a PUT. POST and PATCH promise nothing, and the protocol's
// defaults for a tool that says nothing, that it may change and destroy and
// is not idempotent, are the safe reading of them.
func mcpAnnotations(method string) *mcpToolAnnotations {
	yes := true
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return &mcpToolAnnotations{ReadOnlyHint: &yes}
	case http.MethodDelete:
		return &mcpToolAnnotations{DestructiveHint: &yes, IdempotentHint: &yes}
	case http.MethodPut:
		return &mcpToolAnnotations{IdempotentHint: &yes}
	}
	return nil
}

// describe writes the tool's definition as each era lists it.
func (t *mcpTool) describe(op *Operation, components map[string]*Schema) error {
	input, err := renderToolSchema(components, func(in *schemaInliner) *Schema { return t.inputSchema(op, in) })
	if err != nil {
		return t.describeError(err)
	}
	output, object, err := t.outputSchema(op, components)
	if err != nil {
		return t.describeError(err)
	}
	def := mcpToolDefinition{
		Name:        t.name,
		Description: strings.TrimSpace(op.Summary + "\n\n" + op.Description),
		InputSchema: input,
		Annotations: mcpAnnotations(t.route.Method),
	}
	// 2025-03-26 has neither a title nor an output schema.
	if t.listing[eraBasic], err = json.Marshal(def, json.Deterministic(true)); err != nil {
		return t.describeError(err)
	}
	def.Title = t.route.Title
	if object {
		def.OutputSchema = output
		t.structured[eraStructured] = true
	}
	if t.listing[eraStructured], err = json.Marshal(def, json.Deterministic(true)); err != nil {
		// coverage: the definition differs from the one just encoded by a
		// title that passed checkLabel and a schema already encoded.
		return t.describeError(err)
	}
	def.OutputSchema = output
	t.structured[eraStateless] = output != nil
	if t.listing[eraStateless], err = json.Marshal(def, json.Deterministic(true)); err != nil {
		// coverage: as above, the definition differs only by a schema already encoded.
		return t.describeError(err)
	}
	return nil
}

// describeError reports a tool that cannot be listed, which is text a struct
// tag or a route option wrote that is not valid UTF-8.
func (t *mcpTool) describeError(err error) error {
	return fmt.Errorf("muzak: %s %s: the MCP tool %q cannot be encoded as JSON: %w; "+
		"every doc, summary, description and example must be valid UTF-8", t.route.Method, t.route.Path, t.name, err)
}

// inputSchema describes the arguments of a call: an object with a member for
// each part of the request the route reads, required when the part holds
// something required, and nothing else.
func (t *mcpTool) inputSchema(op *Operation, in *schemaInliner) *Schema {
	root := &Schema{Type: "object", Properties: map[string]*Schema{}, AdditionalProperties: false}
	groups := map[string]*Schema{}
	for _, p := range op.Parameters {
		group := groups[p.In]
		if group == nil {
			group = &Schema{Type: "object", Properties: map[string]*Schema{}, AdditionalProperties: false}
			groups[p.In] = group
		}
		property := in.node(p.Schema)
		if property.Description == "" {
			property.Description = p.Description
		}
		group.Properties[p.Name] = property
		if p.Required {
			group.Required = append(group.Required, p.Name)
		}
	}
	for name, group := range groups {
		slices.Sort(group.Required)
		root.Properties[name] = group
	}
	if body := op.RequestBody; body != nil {
		if media, ok := body.Content["application/json"]; ok {
			root.Properties["body"] = in.node(media.Schema)
			if body.Required {
				root.Required = append(root.Required, "body")
			}
		} else {
			form := in.node(body.Content["multipart/form-data"].Schema)
			for _, f := range t.plan.bind.files {
				form.Properties[f.name] = base64Files(form.Properties[f.name])
			}
			form.AdditionalProperties = false
			root.Properties["form"] = form
		}
	}
	for name, group := range root.Properties {
		if name != "body" && len(group.Required) > 0 {
			root.Required = append(root.Required, name)
		}
	}
	slices.Sort(root.Required)
	if len(root.Properties) == 0 {
		root.Properties = nil
	}
	return root
}

// base64Files describes a file field as an argument carries it: the document
// says a binary string, which a form part is, and an argument is JSON, so the
// file is a string of standard base64.
func base64Files(property *Schema) *Schema {
	out := *property
	if out.Items != nil {
		out.Items = base64Files(out.Items)
		return &out
	}
	out.Format = ""
	out.ContentEncoding = "base64"
	return &out
}

// outputSchema describes the JSON a route answers with when it succeeds, as
// the document does, and reports whether it describes an object: a revision
// before 2026-07-28 has an output schema only for an object, while 2026-07-28
// takes any. A route whose success is not JSON has none.
func (t *mcpTool) outputSchema(op *Operation, components map[string]*Schema) (jsontext.Value, bool, error) {
	response := op.Responses[strconv.Itoa(t.route.Status)]
	if response == nil {
		// coverage: the document describes the status every route that is not
		// a WebSocket or an event stream succeeds with, and those are never
		// tools; this keeps a hand-edited document from crashing the build.
		return nil, false, nil
	}
	media, ok := response.Content["application/json"]
	if !ok || media.Schema == nil {
		return nil, false, nil
	}
	object := false
	output, err := renderToolSchema(components, func(in *schemaInliner) *Schema {
		root := in.root(media.Schema)
		object = root.Type == "object"
		return root
	})
	return output, object, err
}

// mcpSchemaNodeBudget bounds how many schema nodes inlining one tool's schema
// may write. The document's components may share one another many times
// over, so inlining every reference could write exponentially more than the
// document holds; past the budget, every component is written once under
// $defs and referred to, which is linear.
const mcpSchemaNodeBudget = 4096

// schemaInliner writes the document's schemas into a tool's: a reference to a
// component is replaced by the component, so that a client needs no document
// to read the schema, except where a component refers back to itself, which
// can only be written once under $defs and referred to from there.
type schemaInliner struct {
	components map[string]*Schema
	// defsOnly writes every component under $defs, for a schema past the
	// budget.
	defsOnly bool
	// active holds the components being inlined, so that one reached again
	// inside itself is referred to instead.
	active map[string]bool
	defs   map[string]*Schema
	wanted map[string]bool
	queue  []string
	nodes  int
	// overflow records that the budget ran out.
	overflow bool
}

// renderToolSchema builds a schema with build and encodes it, every reference
// resolved within it: inline, or within its own $defs past the budget.
func renderToolSchema(components map[string]*Schema, build func(*schemaInliner) *Schema) (jsontext.Value, error) {
	in := newSchemaInliner(components, false)
	root := in.complete(build(in))
	if in.overflow {
		in = newSchemaInliner(components, true)
		root = in.complete(build(in))
	}
	return in.encode(root)
}

// newSchemaInliner returns an inliner over the document's components.
func newSchemaInliner(components map[string]*Schema, defsOnly bool) *schemaInliner {
	return &schemaInliner{
		components: components,
		defsOnly:   defsOnly,
		active:     map[string]bool{},
		defs:       map[string]*Schema{},
		wanted:     map[string]bool{},
	}
}

// complete writes every component referred to under $defs, each once, and
// returns root.
func (in *schemaInliner) complete(root *Schema) *Schema {
	for len(in.queue) > 0 {
		name := in.queue[0]
		in.queue = in.queue[1:]
		in.active[name] = true
		in.defs[name] = in.node(in.component(name))
		delete(in.active, name)
	}
	return root
}

// node copies a schema with its references resolved.
func (in *schemaInliner) node(s *Schema) *Schema {
	if s == nil {
		return nil
	}
	if in.overflow {
		return &Schema{}
	}
	if in.nodes++; !in.defsOnly && in.nodes > mcpSchemaNodeBudget {
		in.overflow = true
		return &Schema{}
	}
	out := *s
	if s.Properties != nil {
		out.Properties = make(map[string]*Schema, len(s.Properties))
		for name, property := range s.Properties {
			out.Properties[name] = in.node(property)
		}
	}
	out.Items = in.node(s.Items)
	if values, ok := s.AdditionalProperties.(*Schema); ok {
		out.AdditionalProperties = in.node(values)
	}
	out.AnyOf = in.nodeList(s.AnyOf)
	out.AllOf = in.nodeList(s.AllOf)
	if s.Ref == "" {
		return &out
	}
	name := strings.TrimPrefix(s.Ref, componentPrefix)
	if in.defsOnly || in.active[name] {
		out.Ref = "#/$defs/" + name
		if !in.wanted[name] {
			in.wanted[name] = true
			in.queue = append(in.queue, name)
		}
		return &out
	}
	return in.inline(name, &out)
}

// root is [schemaInliner.node] for the root of a schema, which is never a
// reference, so that its type can be read.
func (in *schemaInliner) root(s *Schema) *Schema {
	if s.Ref == "" || !in.defsOnly {
		return in.node(s)
	}
	siblings := *s
	siblings.Ref = ""
	return in.inline(strings.TrimPrefix(s.Ref, componentPrefix), in.node(&siblings))
}

// inline replaces a reference by the component it names. What was written
// beside the reference applies together with it, as JSON Schema 2020-12 reads
// one: a description or a default is the use's own and overrides the
// component's, and anything else, such as the rules a model declared for one
// member of the component, is a schema the value must satisfy as well, under
// allOf.
func (in *schemaInliner) inline(name string, siblings *Schema) *Schema {
	in.active[name] = true
	target := in.node(in.component(name))
	delete(in.active, name)
	siblings.Ref = ""
	rest := *siblings
	rest.Description, rest.Title, rest.Deprecated, rest.Default = "", "", false, nil
	if !reflect.ValueOf(rest).IsZero() {
		siblings.AllOf = append([]*Schema{target}, siblings.AllOf...)
		return siblings
	}
	merged := *target
	if siblings.Description != "" {
		merged.Description = siblings.Description
	}
	if siblings.Title != "" {
		merged.Title = siblings.Title
	}
	merged.Deprecated = merged.Deprecated || siblings.Deprecated
	if siblings.Default != nil {
		merged.Default = siblings.Default
	}
	return &merged
}

// nodeList copies a list of schemas.
func (in *schemaInliner) nodeList(list []*Schema) []*Schema {
	if list == nil {
		return nil
	}
	out := make([]*Schema, len(list))
	for i, s := range list {
		out[i] = in.node(s)
	}
	return out
}

// component returns the component a reference names.
func (in *schemaInliner) component(name string) *Schema {
	if schema := in.components[name]; schema != nil {
		return schema
	}
	// coverage: the document writes a reference only to a component it
	// holds, so this is an empty schema rather than a crash for a document
	// edited by hand.
	return &Schema{}
}

// encode writes a schema and its $defs as one JSON object.
func (in *schemaInliner) encode(root *Schema) (jsontext.Value, error) {
	data, err := json.Marshal(root, json.Deterministic(true))
	if err != nil || len(in.defs) == 0 {
		return data, err
	}
	defs, err := json.Marshal(in.defs, json.Deterministic(true))
	if err != nil {
		// coverage: below the node budget the root inlines the first use of
		// every component it refers to, so text that cannot be encoded fails
		// there first; this is reached past the budget alone.
		return nil, err
	}
	// The root is an object; the definitions are added as its last member.
	out := append([]byte(nil), data[:len(data)-1]...)
	if len(data) > 2 {
		out = append(out, ',')
	}
	out = append(out, `"$defs":`...)
	out = append(out, defs...)
	return append(out, '}'), nil
}
