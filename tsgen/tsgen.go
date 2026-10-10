package tsgen

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"muzak.dev/framework"
)

// Options configures [Generate].
type Options struct {
	// Client also writes a client for the API: a createClient function that
	// returns one method per operation, named after its operationId, each of
	// which sends a request with fetch and resolves to the operation's
	// Response type, and the ApiError class a method rejects with when the
	// status is not a success, carrying the status and the decoded body.
	//
	// Path parameters are escaped segment by segment, and one that is empty,
	// "." or "..", or that holds a "/", is refused with a TypeError rather
	// than sent to another route, as the Go client refuses it. A trailing
	// {name...} wildcard is held to one segment too, since the document
	// writes it as an ordinary parameter; send a value with "/" in it with a
	// request of your own. The path's own text is held to the same: a
	// character URL would read as the start of a query or a fragment, or as
	// a separator, is escaped, and a path holding a dot segment, which URL
	// resolves, out of the baseUrl if there are enough of them, makes
	// Generate return an error. Cookie parameters are typed but not sent,
	// since a browser sends its cookies itself and refuses a Cookie header
	// from a script. Leave it off to write the types alone and send requests
	// with a client of your own.
	Client bool
}

// Bounds on the work one document can ask for. A schema nested deeper than
// maxDepth is refused, which is also what stops a schema that contains itself
// by pointer rather than by reference. Every schema, member and enum value
// visited spends one of maxNodes, so a document whose schemas are shared in
// memory, which the JSON form of a document cannot express but a value built
// in Go can, cannot expand into a walk exponential in its size.
const (
	maxDepth = 64
	maxNodes = 1 << 20
)

// componentPrefix is where every reference Muzak writes points.
const componentPrefix = "#/components/schemas/"

// Generate writes the TypeScript declarations for doc; see the package
// documentation for what they are. It returns an error for a document it
// cannot translate faithfully: a reference to a schema the document does not
// define, or one outside it, and a document past the bounds above.
func Generate(doc *muzak.Document, opts Options) ([]byte, error) {
	if doc == nil {
		return nil, errors.New("tsgen: Generate was given no document")
	}
	g := &generator{doc: doc, opts: opts, names: newNamer(), budget: maxNodes}
	if err := g.run(); err != nil {
		return nil, err
	}
	return g.out.Bytes(), nil
}

// generator holds the state of one call to [Generate].
type generator struct {
	doc    *muzak.Document
	opts   Options
	out    bytes.Buffer
	names  *namer
	types  map[string]string
	ops    []operation
	budget int
	// depth is how many schemas deep the walk is, held to maxDepth.
	depth int
}

// operation is one operation of the document and the names its types are
// written under.
type operation struct {
	method, path string
	op           *muzak.Operation
	base         string
	// client is the name of the operation's method in the client.
	client string
}

// run writes the whole file.
func (g *generator) run() error {
	g.collect()
	g.header()
	if err := g.components(); err != nil {
		return err
	}
	for i := range g.ops {
		if err := g.operation(&g.ops[i]); err != nil {
			return err
		}
	}
	g.operationsMap()
	if g.opts.Client {
		g.client()
	}
	return nil
}

// collect names every component and every operation before anything is
// written, so that a reference can be written as a name whichever comes
// first, and so that the names are chosen in an order that does not depend
// on the order Go walks a map in: components sorted by key, then operations
// by path and by method in the order OpenAPI lists them.
func (g *generator) collect() {
	g.types = map[string]string{}
	if g.doc.Components != nil {
		for _, key := range sortedKeys(g.doc.Components.Schemas) {
			g.types[key] = g.names.claim(pascal(key, "Schema"))
		}
	}
	methods := []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch, http.MethodHead, http.MethodOptions}
	for _, path := range sortedKeys(g.doc.Paths) {
		item := g.doc.Paths[path]
		if item == nil {
			continue
		}
		for i, op := range []*muzak.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch, item.Head, item.Options} {
			if op == nil {
				continue
			}
			id := op.OperationID
			if id == "" {
				id = strings.ToLower(methods[i]) + " " + path
			}
			base := g.names.claimGroup(pascal(id, "Operation"), "Params", "Body", "Response", "Error")
			g.ops = append(g.ops, operation{method: methods[i], path: path, op: op, base: base})
		}
	}
	methodNames := newNamer()
	for i := range g.ops {
		g.ops[i].client = methodNames.claim(camel(g.ops[i].base))
	}
}

// header writes the comment that opens the file.
func (g *generator) header() {
	g.out.WriteString("// Code generated by muzak.dev/framework/tsgen. DO NOT EDIT.\n\n")
	info := strings.TrimSpace(g.doc.Info.Title + " " + g.doc.Info.Version)
	if info != "" || g.doc.Info.Description != "" {
		g.out.WriteString("/*\n")
		writeCommentLines(&g.out, "", info, g.doc.Info.Description)
		g.out.WriteString(" */\n\n")
	}
}

// components writes one declaration per component schema.
func (g *generator) components() error {
	if g.doc.Components == nil {
		return nil
	}
	for _, key := range sortedKeys(g.doc.Components.Schemas) {
		schema := g.doc.Components.Schemas[key]
		name := g.types[key]
		if err := g.declare(name, schema, false, describe(schema)); err != nil {
			return fmt.Errorf("tsgen: the schema %q: %w", clip(key), err)
		}
	}
	return nil
}

// describe returns the comment lines a schema documents itself with.
func describe(s *muzak.Schema) []string {
	if s == nil {
		return nil
	}
	var lines []string
	if s.Title != "" {
		lines = append(lines, s.Title)
	}
	if s.Description != "" {
		lines = append(lines, s.Description)
	}
	if s.Deprecated {
		lines = append(lines, "@deprecated")
	}
	return lines
}

// declare writes one named type: an interface for an object with members, an
// alias for anything else.
func (g *generator) declare(name string, s *muzak.Schema, blob bool, doc []string) error {
	writeComment(&g.out, "", doc...)
	if s != nil && s.Ref == "" && len(s.AnyOf) == 0 && len(s.AllOf) == 0 && s.Enum == nil && isObject(s) && len(s.Properties) > 0 {
		body, err := g.object(s, blob, 0)
		if err != nil {
			return err
		}
		g.out.WriteString("export interface " + name + " " + body + "\n\n")
		return nil
	}
	expr, err := g.typeOf(s, blob, 0)
	if err != nil {
		return err
	}
	g.out.WriteString("export type " + name + " = " + expr + ";\n\n")
	return nil
}

// isObject reports whether a schema describes an object: its type says so,
// or it has no type and describes members.
func isObject(s *muzak.Schema) bool {
	types := typeNames(s.Type)
	if len(types) == 0 {
		return s.Properties != nil || s.AdditionalProperties != nil
	}
	return len(types) == 1 && types[0] == "object"
}

// spend charges one unit of work against the document's budget.
func (g *generator) spend() error {
	g.budget--
	if g.budget < 0 {
		return fmt.Errorf("the document's schemas expand to more than %d schemas, members and values, which is more than any document written out as JSON holds; share structure by reference instead", maxNodes)
	}
	return nil
}

// reference writes a reference as the name of the component it points at.
func (g *generator) reference(ref string) (string, error) {
	key, local := strings.CutPrefix(ref, componentPrefix)
	if !local {
		return "", fmt.Errorf("the reference %q points outside the document's component schemas, which a declaration file cannot follow", clip(ref))
	}
	// A JSON pointer escapes "/" and "~" in a key.
	key = strings.NewReplacer("~1", "/", "~0", "~").Replace(key)
	name, defined := g.types[key]
	if !defined {
		return "", fmt.Errorf("the reference %q names a schema the document does not define", clip(ref))
	}
	return name, nil
}

// enum writes an enum as the union of its values, each a literal. A value
// with no literal type, an object or a number that is not finite, widens the
// union to what it can say.
func (g *generator) enum(values []any) (string, error) {
	var parts union
	for _, value := range values {
		if err := g.spend(); err != nil {
			return "", err
		}
		literal := literalOf(value)
		if literal == "unknown" {
			return "unknown", nil
		}
		parts.add(literal)
	}
	if len(parts.list) == 0 {
		// An enum with no values admits nothing.
		return "never", nil
	}
	return strings.Join(parts.list, " | "), nil
}

// union collects the distinct members of a union in the order they are first
// seen. A set rather than a search of the list, since a document can repeat a
// union's members by the hundred thousand, and a search per member is then a
// quadratic wait.
type union struct {
	list []string
	seen map[string]bool
}

// add adds expr unless it is there already.
func (u *union) add(expr string) {
	if u.seen[expr] {
		return
	}
	if u.seen == nil {
		u.seen = map[string]bool{}
	}
	u.seen[expr] = true
	u.list = append(u.list, expr)
}

// literalOf writes one enum value as a TypeScript literal type.
func literalOf(value any) string {
	if value == nil {
		return "null"
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.String:
		return quote(v.String())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "number"
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return "unknown"
}

// typeNames reads the type keyword, which Muzak writes as a string or a list
// of strings and a document built in Go may hold as a list of anything.
func typeNames(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		names := make([]string, 0, len(v))
		for _, entry := range v {
			name, _ := entry.(string)
			names = append(names, name)
		}
		return names
	}
	return nil
}

// arrayOf writes an array of item, with the short form only where it cannot
// be misread.
func arrayOf(item string) string {
	if isSimple(item) {
		return item + "[]"
	}
	return "Array<" + item + ">"
}

// group parenthesises a union or an intersection, so that it can sit inside
// another.
func group(expr string) string {
	if isSimple(expr) || strings.HasPrefix(expr, "{") || strings.HasPrefix(expr, "Array<") || strings.HasPrefix(expr, "Record<") {
		return expr
	}
	return "(" + expr + ")"
}

// isSimple reports whether a type expression is a name, a keyword, a literal
// or an array of one, which nothing around it can change the meaning of.
func isSimple(expr string) bool {
	if strings.HasPrefix(expr, `"`) {
		// One string literal from quote, when the quote that closes it is the
		// last byte. It is read as quote writes it, an escape at a time,
		// since counting quotes and escaped quotes takes the end of "C:\\"
		// for an escaped quote, and two such literals for one.
		i := 1
		for i < len(expr)-1 && expr[i] != '"' {
			if expr[i] == '\\' {
				i++
			}
			i++
		}
		return i == len(expr)-1
	}
	expr = strings.TrimSuffix(expr, "[]")
	return expr != "" && strings.IndexFunc(expr, func(r rune) bool {
		switch {
		case r == '_', r == '-', r == '.', r == '$', r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			return false
		}
		return true
	}) < 0
}

// operation writes the four types of one operation.
func (g *generator) operation(o *operation) error {
	where := fmt.Sprintf("tsgen: %s %q", o.method, clip(o.path))
	// The client fills every parameter of the path from params.path, so one
	// the operation does not describe would be a type that does not compile.
	described := map[string]bool{}
	for _, p := range o.op.Parameters {
		if p.In == "path" {
			described[p.Name] = true
		}
	}
	for _, name := range templateParams(o.path) {
		if !described[name] {
			return fmt.Errorf("%s: the path names the parameter %q, which the operation does not describe", where, clip(name))
		}
	}
	if g.opts.Client {
		if segment, found := dotSegment(o.path); found {
			return fmt.Errorf("%s: the path holds the dot segment %q, which URL resolves as a step through the path, "+
				"so the client would send the request somewhere else, outside its baseUrl if there are enough of them", where, clip(segment))
		}
	}
	doc := []string{}
	if o.op.Summary != "" {
		doc = append(doc, o.op.Summary)
	}
	if o.op.Description != "" {
		doc = append(doc, o.op.Description)
	}
	doc = append(doc, o.method+" "+o.path)
	if o.op.Deprecated {
		doc = append(doc, "@deprecated")
	}

	params, err := g.params(o.op.Parameters)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	writeComment(&g.out, "", append([]string{"The parameters of " + o.base + "."}, doc...)...)
	g.out.WriteString("export interface " + o.base + "Params " + params + "\n\n")

	body, err := g.requestBody(o.op.RequestBody)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	writeComment(&g.out, "", "The request body of "+o.base+".")
	g.out.WriteString("export type " + o.base + "Body = " + body + ";\n\n")

	success, failure, err := g.responses(o.op.Responses)
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	writeComment(&g.out, "", "What "+o.base+" answers with when it succeeds.")
	g.out.WriteString("export type " + o.base + "Response = " + success + ";\n\n")
	writeComment(&g.out, "", "What "+o.base+" answers with when it does not.")
	g.out.WriteString("export type " + o.base + "Error = " + failure + ";\n\n")
	return nil
}

// params writes the parameters of an operation, grouped by where they go. A
// group none of whose members is required is optional itself.
func (g *generator) params(parameters []muzak.Parameter) (string, error) {
	var b strings.Builder
	b.WriteString("{\n")
	for _, location := range []string{"path", "query", "header", "cookie"} {
		var members []muzak.Parameter
		required := false
		for _, p := range parameters {
			if p.In == location {
				members = append(members, p)
				required = required || p.Required || location == "path"
			}
		}
		if len(members) == 0 {
			continue
		}
		slices.SortStableFunc(members, func(a, b muzak.Parameter) int { return strings.Compare(a.Name, b.Name) })
		optional := "?"
		if required {
			optional = ""
		}
		b.WriteString("  " + location + optional + ": {\n")
		for i := 0; i < len(members); {
			// A name described more than once, as two fields that bind one
			// parameter describe it, is written as one member, since
			// TypeScript refuses a member declared twice. The one value sent
			// is read by each of them, so it is every type it is described
			// with, and required if any of them requires it.
			var types, docs union
			name, mark := members[i].Name, "?"
			for ; i < len(members) && members[i].Name == name; i++ {
				p := members[i]
				if err := g.spend(); err != nil {
					return "", err
				}
				expr, err := g.typeOf(p.Schema, false, 2)
				if err != nil {
					return "", err
				}
				types.add(expr)
				docs.add(p.Description)
				if p.Required || location == "path" {
					mark = ""
				}
			}
			expr := types.list[0]
			if len(types.list) > 1 {
				for j, part := range types.list {
					types.list[j] = group(part)
				}
				expr = strings.Join(types.list, " & ")
			}
			writeComment(&b, "    ", docs.list...)
			b.WriteString("    " + propertyKey(name) + mark + ": " + expr + ";\n")
		}
		b.WriteString("  };\n")
	}
	b.WriteString("}")
	return b.String(), nil
}

// requestBody writes the type of an operation's body: undefined when it takes
// none, and otherwise the type of the media type a client would send, JSON
// first, then a form, then anything else as a Blob. A body the operation does
// not require may also be left out.
func (g *generator) requestBody(body *muzak.RequestBody) (string, error) {
	if body == nil || len(body.Content) == 0 {
		return "undefined", nil
	}
	mediaType, kind := chooseRequestType(body.Content)
	var expr string
	switch kind {
	case bodyJSON, bodyForm, bodyURLEncoded:
		var err error
		if expr, err = g.typeOf(body.Content[mediaType].Schema, kind == bodyForm, 0); err != nil {
			return "", err
		}
	default:
		expr = "Blob"
	}
	if !body.Required {
		expr = group(expr) + " | undefined"
	}
	return expr, nil
}

// The kinds of body a client sends and receives.
const (
	bodyNone       = "none"
	bodyJSON       = "json"
	bodyForm       = "form"
	bodyURLEncoded = "urlencoded"
	bodyText       = "text"
	bodyBlob       = "blob"
)

// chooseRequestType picks the media type a client sends a body as, and what
// kind of body that is.
func chooseRequestType(content map[string]muzak.MediaType) (string, string) {
	types := sortedKeys(content)
	for _, t := range types {
		if isJSON(t) {
			return t, bodyJSON
		}
	}
	// Multipart before urlencoded, since only multipart carries a file.
	if _, offered := content["multipart/form-data"]; offered {
		return "multipart/form-data", bodyForm
	}
	if _, offered := content["application/x-www-form-urlencoded"]; offered {
		return "application/x-www-form-urlencoded", bodyURLEncoded
	}
	return types[0], bodyBlob
}

// isJSON reports whether a media type is JSON, or a structured syntax built on
// it such as application/problem+json.
func isJSON(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// responses writes the union of what an operation answers with when it
// succeeds, which is every 1xx, 2xx and 3xx response, and of what it answers
// with otherwise.
func (g *generator) responses(responses map[string]*muzak.Response) (string, string, error) {
	var success, failure union
	for _, status := range sortedKeys(responses) {
		expr, err := g.responseType(responses[status])
		if err != nil {
			return "", "", err
		}
		target := &failure
		if isSuccess(status) {
			target = &success
		}
		target.add(expr)
	}
	join := func(parts union) string {
		if len(parts.list) == 0 {
			return "never"
		}
		if parts.seen["unknown"] {
			return "unknown"
		}
		return strings.Join(parts.list, " | ")
	}
	return join(success), join(failure), nil
}

// isSuccess reports whether a response key is a status that is not a
// failure, 1xx, 2xx or 3xx, a range of them such as "2XX" included.
func isSuccess(status string) bool {
	return status != "" && status[0] >= '1' && status[0] <= '3'
}

// templateParams returns the names of the parameters a path template holds.
// OpenAPI writes a trailing wildcard as an ordinary parameter, so a name is
// taken as it is written, whatever it ends in.
func templateParams(template string) []string {
	var names []string
	for _, segment := range strings.Split(strings.TrimPrefix(template, "/"), "/") {
		if len(segment) > 2 && segment[0] == '{' && segment[len(segment)-1] == '}' {
			names = append(names, segment[1:len(segment)-1])
		}
	}
	return names
}

// responseType writes the type of one response's body.
func (g *generator) responseType(r *muzak.Response) (string, error) {
	if r == nil || len(r.Content) == 0 {
		return "undefined", nil
	}
	var parts union
	for _, mediaType := range sortedKeys(r.Content) {
		var expr string
		switch kind := responseKind(mediaType); kind {
		case bodyJSON:
			var err error
			if expr, err = g.typeOf(r.Content[mediaType].Schema, false, 0); err != nil {
				return "", err
			}
			expr = group(expr)
		case bodyText:
			expr = "string"
		default:
			expr = "Blob"
		}
		parts.add(expr)
	}
	if parts.seen["unknown"] {
		return "unknown", nil
	}
	return strings.Join(parts.list, " | "), nil
}

// responseKind says how a response of a media type is read.
func responseKind(mediaType string) string {
	switch {
	case isJSON(mediaType):
		return bodyJSON
	case strings.HasPrefix(mediaType, "text/") && mediaType != "text/event-stream":
		return bodyText
	}
	return bodyBlob
}

// operationsMap writes the interface that maps every operation to its
// method, path and types.
func (g *generator) operationsMap() {
	writeComment(&g.out, "", "Every operation of the API, by the name its types are written under.")
	g.out.WriteString("export interface Operations {\n")
	for _, o := range g.ops {
		g.out.WriteString("  " + o.base + ": {\n")
		g.out.WriteString("    method: " + quote(o.method) + ";\n")
		g.out.WriteString("    path: " + quote(o.path) + ";\n")
		for _, suffix := range []string{"Params", "Body", "Response", "Error"} {
			g.out.WriteString("    " + strings.ToLower(suffix) + ": " + o.base + suffix + ";\n")
		}
		g.out.WriteString("  };\n")
	}
	g.out.WriteString("}\n")
}

// sortedKeys returns the keys of a map in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// clip shortens a string from the document for an error message, quoting
// takes care of the rest.
func clip(s string) string {
	const most = 96
	if len(s) <= most {
		return s
	}
	return s[:most] + "..."
}
