package muzak

import (
	"bytes"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go/token"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"
)

// Empty is the sentinel input type for routes that bind nothing from the
// request. Declare a handler as func(ctx *Context, _ Empty) (Out, error) when
// the route takes no path, query, header or body parameters; binding is
// skipped entirely for it.
type Empty struct{}

// Struct tags recognised by the binder. A field carrying one of the first six
// is read from that part of the request; a field carrying none of them becomes
// part of the JSON body. A located field is never a member of the body,
// whatever its json tag says: a body naming one is refused like any member the
// body does not have, or ignored under [AllowUnknownFields]. A json:"-" tag
// keeps a field out of the body and its schema and nothing more, so a located
// field carrying one is still bound from its location.
const (
	tagPath     = "path"
	tagQuery    = "query"
	tagHeader   = "header"
	tagCookie   = "cookie"
	tagForm     = "form"
	tagFile     = "file"
	tagDoc      = "doc"
	tagDefault  = "default"
	tagRequired = "required"
	tagJSON     = "json"
)

// paramSource identifies which part of the request a parameter is read from.
type paramSource uint8

const (
	srcPath paramSource = iota
	srcQuery
	srcHeader
	srcCookie
	srcForm
	srcFile
)

// String returns the OpenAPI name for the parameter location, which is also
// what appears in the "location" member of a validation error. The form and
// file locations are not OpenAPI parameter locations, because both describe
// the body rather than a parameter; they only ever appear in error details.
func (s paramSource) String() string {
	switch s {
	case srcPath:
		return "path"
	case srcQuery:
		return "query"
	case srcHeader:
		return "header"
	case srcForm:
		return "form"
	case srcFile:
		return "file"
	default:
		return "cookie"
	}
}

var (
	durationType    = reflect.TypeFor[time.Duration]()
	timeType        = reflect.TypeFor[time.Time]()
	uuidType        = reflect.TypeFor[uuid.UUID]()
	emptyType       = reflect.TypeFor[Empty]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	textMarshaler   = reflect.TypeFor[encoding.TextMarshaler]()
)

// setter writes the textual values supplied for a parameter into a struct
// field. Setters are resolved once when the route is registered, so the
// per-request path performs no type switching beyond an indirect call.
type setter func(dst reflect.Value, raw []string) error

// paramBinder binds one struct field from one request parameter.
type paramBinder struct {
	index  []int
	source paramSource
	name   string
	// key is the canonical form of name for a header binder, resolved once
	// here so that a request pays no canonicalization.
	key      string
	typ      reflect.Type
	doc      string
	required bool
	defValue string
	hasDef   bool
	// isSlice is true when the field takes every value sent for it rather
	// than one, which decides whether a parameter sent twice is a list or a
	// mistake, and whether a header is split on its commas.
	isSlice bool
	set     setter
}

// takesList reports whether a parameter of type t takes every value sent for
// it, which is what its setter does for a slice, or a pointer to one, a byte
// slice included: each value is one byte, and the document says so. A type
// that reads itself from text, or a time.Duration, takes one piece of text
// however it is built.
func takesList(t reflect.Type) bool {
	for t != durationType && !reflect.PointerTo(t).Implements(textUnmarshaler) {
		switch t.Kind() {
		case reflect.Pointer:
			t = t.Elem()
			continue
		case reflect.Slice:
			return true
		}
		return false
	}
	return false
}

// bodyPlan describes how the JSON request body maps onto the input struct.
type bodyPlan struct {
	// direct is true when every field of the input struct belongs to the
	// body, so the body can be decoded straight into it without a scratch
	// value. This is the common case and the one that avoids all copying.
	direct bool
	// fields lists the index of each body-bound field, used to copy values
	// out of the scratch value when direct is false.
	fields [][]int
	// required is true when a request must carry a body.
	required bool
	// defaults are the members of the body a `default` tag fills in when the
	// client leaves them out.
	defaults []bodyDefault
	// shape is the type the body is decoded into: the input type itself when
	// direct is true, and otherwise a struct of the body members alone, which
	// copies moves into the input. See [bodyPlan.narrow].
	shape  reflect.Type
	copies []bodyCopy
}

// bodyDefault is a member of the JSON body that has a default.
type bodyDefault struct {
	// index locates the field in the input, at locates it in the value the
	// body is decoded into, name is what the body calls it, and raw is the
	// tag's text, which set writes into the field.
	index []int
	at    []int
	name  string
	typ   reflect.Type
	raw   string
	set   setter
}

// bindPlan is the precompiled recipe for turning a request into a value of the
// handler's input type. Exactly one plan is built per route when the route is
// registered; the per-request path only walks the plan, never the type.
type bindPlan struct {
	typ        reflect.Type
	params     []paramBinder
	body       *bodyPlan
	needsQuery bool
	// form and files describe a body sent as a form rather than as JSON. They
	// are populated from the "form" and "file" struct tags, and a plan that
	// has either of them has no JSON body at all.
	form  []paramBinder
	files []fileBinder
	// multipart is true when the plan reads a form-encoded body, which is what
	// tells the binder to parse one instead of decoding JSON.
	multipart bool
	// empty marks a plan with nothing to do at all, letting the router skip
	// binding for Empty inputs.
	empty bool
	// validation is non-nil when the input type declares rules, and holds the
	// field origins those rules are reported against. Resolving them here means
	// a request pays a map lookup per rule rather than a walk over the type.
	validation *validationPlan
	// deps lists the [Dep] fields of the input, which are filled from the
	// resolved dependencies rather than from the request. It is nil for an
	// input without one, which then pays a nil check for the feature.
	deps []depBinder
}

// bodyBufferPool recycles the buffers used to read request bodies. Buffers are
// returned only after decoding has finished and every string the decoder kept
// has been copied, so no request can observe another's bytes.
var bodyBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// maxPooledBodyBuffer bounds the size of a buffer kept for reuse. A single
// oversized request should not pin megabytes of memory in the pool for the
// lifetime of the process.
const maxPooledBodyBuffer = 64 << 10

// newBindPlan compiles the binding plan for the input type T of a route
// registered at the given path template.
//
// It returns an error when the type cannot be bound (a non-struct input, an
// unsupported field type, or a path parameter the template never declares), so
// that every such mistake surfaces at startup rather than on the first
// matching request.
func newBindPlan(t reflect.Type, method, path string) (*bindPlan, error) {
	plan := &bindPlan{typ: t}
	if t == emptyType {
		plan.empty = true
		return plan, nil
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("muzak: %s %s: input type %s must be a struct or muzak.Empty", method, path, t)
	}

	var bodyFields [][]int
	if err := collectFields(t, nil, plan, &bodyFields); err != nil {
		return nil, fmt.Errorf("muzak: %s %s: %w", method, path, err)
	}

	declared := templateParams(path)
	for i := range plan.params {
		p := &plan.params[i]
		if p.source == srcQuery {
			plan.needsQuery = true
		}
		if p.source == srcPath {
			if !declared[p.name] {
				return nil, fmt.Errorf("muzak: %s %s: field binds path parameter %q, which the route template does not declare", method, path, p.name)
			}
		}
	}

	if reflect.PointerTo(t).Implements(reflect.TypeFor[Validatable]()) {
		plan.validation = newValidationPlan(t, plan)
		if err := plan.checkRulesBindToFields(); err != nil {
			return nil, fmt.Errorf("muzak: %s %s: %w", method, path, err)
		}
	}

	if len(plan.form) > 0 || len(plan.files) > 0 {
		plan.multipart = true
		if len(bodyFields) > 0 {
			field := t.FieldByIndex(bodyFields[0]).Name
			return nil, fmt.Errorf("muzak: %s %s: field %s carries no location tag, so it would come from a JSON body, but this input already reads a form body; tag it with %q or move it to the path, query, header or cookie",
				method, path, field, tagForm)
		}
		return plan, nil
	}

	if len(bodyFields) > 0 {
		defaults, err := bodyDefaults(t, bodyFields)
		if err != nil {
			return nil, fmt.Errorf("muzak: %s %s: %w", method, path, err)
		}
		body := &bodyPlan{
			defaults: defaults,
			// Decoding straight into the input value is only safe when the
			// input has no located fields at all. Counting body fields against
			// the field count is not enough: an embedded struct holding both a
			// located parameter and a body member counts as one field on each
			// side, which would let a crafted body reach the located field. A
			// Dep is kept out of the body the same way.
			direct:   len(plan.params) == 0 && len(plan.deps) == 0 && len(bodyFields) == totalFields(t),
			fields:   bodyFields,
			required: true,
			shape:    t,
		}
		if !body.direct {
			body.narrow(t, bodyFields)
		}
		if err := checkBodyDecodes(body.shape, t); err != nil {
			return nil, fmt.Errorf("muzak: %s %s: %w", method, path, err)
		}
		plan.body = body
	}
	return plan, nil
}

// narrow gives a body plan whose input also has located fields the type its
// body is decoded into: a struct holding the input's body members and nothing
// else.
//
// The body used to be decoded into a scratch value of the input type itself,
// and only the body members copied out. That kept a crafted body from setting
// a located field, but not from naming one: the decoder still knew it as a
// member, so {"ID":999} beside a path parameter was accepted and silently
// dropped, and {"ID":"zz"} was a 422 about a member the documented schema does
// not list. Decoding into a struct with no such field makes a member naming a
// located field unknown, like any other member the body does not have: refused
// by default, and ignored, value unread, under [AllowUnknownFields].
//
// The struct is built once, when the route is compiled. Every body member keeps
// its own type and tag, so the decoder reads it exactly as it would have read
// it in the input. An embedded struct is rebuilt the same way and embedded
// again under the embed option, which is how encoding/json/v2 spells what
// embedding means to it; an unexported one is given an exported name, which
// is the only name reflect can build a field under, and its members are copied
// out one by one, since an unexported embedded struct cannot be set whole.
//
// An input that decodes itself, through UnmarshalJSON or the like, is left to
// do so into a scratch value of its own type, because rebuilding it would lose
// the method; what it writes into a located field is still not copied out.
func (b *bodyPlan) narrow(t reflect.Type, bodyFields [][]int) {
	if decodesItself(t) {
		for _, index := range bodyFields {
			b.copies = append(b.copies, bodyCopy{from: index, to: index})
		}
		return
	}
	b.shape, b.copies = bodyShape(t, nil)
	at := make(map[string][]int, len(b.copies))
	for _, c := range b.copies {
		at[fmt.Sprint(c.to)] = c.from
	}
	for i := range b.defaults {
		b.defaults[i].at = at[fmt.Sprint(b.defaults[i].index)]
	}
}

// bodyCopy pairs a field of the type a body is decoded into with the field of
// the input it is copied to, each located by its index path.
type bodyCopy struct {
	from, to []int
}

// bodyShape builds the struct holding the body members of t, which sits at the
// index path at within the input, and the copies that move each member from
// it into the input. See [bodyPlan.narrow].
func bodyShape(t reflect.Type, at []int) (reflect.Type, []bodyCopy) {
	var fields []reflect.StructField
	var copies []bodyCopy
	taken := make(map[string]bool, t.NumField())
	for i := range t.NumField() {
		taken[t.Field(i).Name] = true
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !usableField(f) {
			continue
		}
		if _, located := declaredLocation(f); located || isDepField(f) {
			continue
		}
		name, options, _ := strings.Cut(f.Tag.Get(tagJSON), ",")
		if name == "-" && options == "" {
			continue
		}
		index := append(slices.Clone(at), i)
		field := reflect.StructField{Name: exportedName(f.Name, taken), Type: f.Type, Tag: f.Tag}
		if f.Anonymous && name == "" {
			field.Tag = embedTag(options)
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			inner, innerCopies := bodyShape(f.Type, index)
			if inner.NumField() == 0 {
				continue
			}
			field.Type = inner
			for _, c := range innerCopies {
				copies = append(copies, bodyCopy{from: append([]int{len(fields)}, c.from...), to: c.to})
			}
		} else {
			copies = append(copies, bodyCopy{from: []int{len(fields)}, to: index})
		}
		fields = append(fields, field)
	}
	return reflect.StructOf(fields), copies
}

// exportedName returns a field name reflect can build a struct with, which an
// unexported embedded type's name is not, and one no other field of the
// struct already has.
func exportedName(name string, taken map[string]bool) string {
	if token.IsExported(name) {
		return name
	}
	candidate := "X" + name
	for taken[candidate] {
		candidate += "_"
	}
	taken[candidate] = true
	return candidate
}

// embedTag is the json tag that embeds a rebuilt struct under a field name of
// its own, keeping whatever options the original embedding carried so that
// the decoder refuses them, if it does, exactly as it would have.
func embedTag(options string) reflect.StructTag {
	parts := strings.Split(options, ",")
	if options == "" {
		parts = nil
	}
	if !slices.Contains(parts, "embed") && !slices.Contains(parts, "inline") {
		parts = append([]string{"embed"}, parts...)
	}
	return reflect.StructTag(`json:",` + strings.Join(parts, ",") + `"`)
}

// bodyDefaults finds the members of the JSON body that carry a `default` tag.
//
// Only the top level of the body is searched, with the members an embedded
// struct promotes to it. A default deeper than that would have to be written
// into an object the client may not have sent, or into each element of a
// collection, and the document says nothing about it either. A member whose
// type cannot be written from text, a struct or a map, is left without one
// rather than refused, since the tag used to be documentation and nothing
// else; a default that does not parse as its own type is an error, because it
// could never have been what the developer meant.
func bodyDefaults(t reflect.Type, bodyFields [][]int) ([]bodyDefault, error) {
	var out []bodyDefault
	var walk func(t reflect.Type, prefix []int, fields [][]int) error
	walk = func(t reflect.Type, prefix []int, fields [][]int) error {
		for _, tail := range fields {
			index := append(slices.Clone(prefix), tail...)
			f := t.FieldByIndex(tail)
			if f.Anonymous && f.Type.Kind() == reflect.Struct && f.Tag.Get(tagJSON) == "" {
				var promoted [][]int
				for i := range f.Type.NumField() {
					if usableField(f.Type.Field(i)) {
						promoted = append(promoted, []int{i})
					}
				}
				if err := walk(f.Type, index, promoted); err != nil {
					return err
				}
				continue
			}
			raw, has := f.Tag.Lookup(tagDefault)
			if !has {
				continue
			}
			name, _ := jsonFieldName(f)
			if name == "" {
				continue
			}
			set, err := setterFor(f.Type)
			if err != nil {
				continue
			}
			if err := set(reflect.New(f.Type).Elem(), []string{raw}); err != nil {
				return fmt.Errorf("field %s declares the default %q, which is not a valid %s", f.Name, raw, f.Type)
			}
			out = append(out, bodyDefault{index: index, at: index, name: name, typ: f.Type, raw: raw, set: set})
		}
		return nil
	}
	if err := walk(t, nil, bodyFields); err != nil {
		return nil, err
	}
	return out, nil
}

// checkBodyDecodes refuses a body type that encoding/json/v2 cannot decode
// into, whatever a request sends.
//
// Such a type used to build. Every request that reached the problem then
// failed to decode with an error that was the type's and not the request's,
// and it was sent to the client as a 422, usually against an empty field, and
// logged nowhere, so the one person who could fix it never heard of it.
//
// Two checks find them. A struct the decoder cannot make sense of, through a
// tag it rejects or two fields claiming one name, is found by asking the
// decoder to read an empty object into it: that is the first thing it does
// with any struct, so it fails here exactly when it would fail on every
// request. A field of a type JSON cannot carry, a channel, a function, a
// complex number or an interface the decoder cannot fill, and the `string`
// option on a field that is not a number, fail only when a request sends that
// member, so the fields are walked for them. A type that decodes itself is
// left to do so, and its fields are not walked.
//
// shape is the type the body is decoded into, and input the route's input
// type, which is what a failure of the shape itself is reported against.
func checkBodyDecodes(shape, input reflect.Type) error {
	if decodesItself(shape) {
		return nil
	}
	return checkStructDecodes(shape, input.String(), "", map[reflect.Type]bool{shape: true})
}

// checkDecodes is [checkBodyDecodes] for a type reached through the field at
// path. The seen set stops a recursive type from being walked forever.
func checkDecodes(t reflect.Type, path string, seen map[reflect.Type]bool) error {
	for !seen[t] && !decodesItself(t) {
		seen[t] = true
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		case reflect.Chan, reflect.Func, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
			return fmt.Errorf("field %s is of type %s, which a JSON body cannot carry; give it a type that JSON can, or tag it json:\"-\"", path, t)
		case reflect.Interface:
			if t.NumMethod() > 0 {
				return fmt.Errorf("field %s is of the interface type %s, which the decoder cannot fill because nothing says which type to decode into; give it a concrete type, or tag it json:\"-\"", path, t)
			}
		case reflect.Struct:
			return checkStructDecodes(t, t.String(), path, seen)
		}
		return nil
	}
	return nil
}

// checkStructDecodes is [checkDecodes] for a struct, which a failure names as
// name.
func checkStructDecodes(t reflect.Type, name, path string, seen map[reflect.Type]bool) error {
	if err := json.Unmarshal([]byte("{}"), reflect.New(t).Interface(), durationJSON); err != nil {
		cause := err
		var semantic *json.SemanticError
		if errors.As(err, &semantic) && semantic.Err != nil {
			cause = semantic.Err
		}
		where := ""
		if path != "" {
			where = ", held in field " + path + ","
		}
		return fmt.Errorf("type %s%s cannot be decoded from a JSON body, because encoding/json/v2 refuses it: %w", name, where, cause)
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		name, options, _ := strings.Cut(f.Tag.Get(tagJSON), ",")
		if name == "-" && options == "" {
			continue
		}
		at := f.Name
		if path != "" {
			at = path + "." + f.Name
		}
		if slices.Contains(strings.Split(options, ","), "string") && !isNumberBehind(f.Type) {
			return fmt.Errorf("field %s carries the `string` option in its json tag, which encoding/json/v2 accepts only on a number, and %s is not one; remove the option", at, f.Type)
		}
		if err := checkDecodes(f.Type, at, seen); err != nil {
			return err
		}
	}
	return nil
}

// isNumberBehind reports whether a type is a number once its pointers are
// followed, or reads itself and so decides for itself what the option means.
// time.Time has the methods but is read natively, and refuses the option.
func isNumberBehind(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return t != timeType && decodesItself(t)
}

// totalFields counts the exported fields of a struct, which tells newBindPlan
// whether the body covers the whole input type.
func totalFields(t reflect.Type) int {
	n := 0
	for i := range t.NumField() {
		if usableField(t.Field(i)) {
			n++
		}
	}
	return n
}

// templateParams returns the set of parameter names a route template declares,
// including a trailing wildcard.
func templateParams(path string) map[string]bool {
	out := map[string]bool{}
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			out[strings.TrimSuffix(seg[1:len(seg)-1], "...")] = true
		}
	}
	return out
}

// collectFields walks a struct type, appending a binder for every field that
// carries a location tag and recording the index of every field that belongs
// to the JSON body. It recurses into embedded structs so that shared parameter
// sets can be composed by embedding.
//
// Only the top level and structs embedded by value are searched for location
// tags. A tag anywhere else is a build error rather than something to skip,
// because skipping it is not neutral: the field it sits on becomes body
// content, and a request can then set the user ID the gateway was supposed to
// supply in a header by writing it into the JSON body instead.
func collectFields(t reflect.Type, prefix []int, plan *bindPlan, bodyFields *[][]int) error {
	for i := range t.NumField() {
		f := t.Field(i)
		// A Dep is filled from the dependencies, never from the request, so it
		// is neither a parameter nor body content; see [bindPlan.collectDep].
		if dep, err := plan.collectDep(f, prefix, i); dep || err != nil {
			if err != nil {
				return err
			}
			continue
		}
		if !usableField(f) {
			if location, declared := declaredLocation(f); declared {
				return fmt.Errorf("field %s declares a %s parameter but is unexported, so the binder cannot set it; export the field", f.Name, location)
			}
			if f.Anonymous {
				if err := checkUnreachable(f); err != nil {
					return err
				}
			}
			continue
		}
		index := append(append([]int(nil), prefix...), i)
		// A json:"-" tag keeps a field out of the body and out of the schema,
		// and that is all it does. It used to end the field's binding
		// altogether, which turned `header:"X-User-ID" json:"-"`, the natural
		// way to write "from the header and never from the body", into a field
		// bound from nowhere: the header, its default and its required check
		// all went silently with it.
		jsonName, _, _ := strings.Cut(f.Tag.Get(tagJSON), ",")
		inBody := jsonName != "-"

		if name, declared := f.Tag.Lookup(tagFile); declared {
			binder, err := newFileBinder(f, index, name)
			if err != nil {
				return err
			}
			plan.files = append(plan.files, binder)
			continue
		}

		source, name, ok := locationTag(f)
		if ok {
			binder, err := newParamBinder(f, index, source, name)
			if err != nil {
				return err
			}
			if source == srcForm {
				plan.form = append(plan.form, binder)
			} else {
				plan.params = append(plan.params, binder)
			}
			continue
		}

		// An embedded struct may itself declare located parameters. Recurse to
		// find them; if it declares none, the embedded value is body content
		// like any other field. One excluded from the body with json:"-" still
		// contributes its located parameters, and nothing else, since the
		// decoder never writes to it.
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			before := plan.located()
			var nestedBody [][]int
			if err := collectFields(f.Type, index, plan, &nestedBody); err != nil {
				return err
			}
			if !inBody {
				continue
			}
			if plan.located() != before {
				*bodyFields = append(*bodyFields, nestedBody...)
				continue
			}
		}
		// A field kept out of the body is checked as well: a location tag
		// inside it would be ignored just the same, and a developer who wrote
		// one expected it to bind.
		if err := checkUnreachable(f); err != nil {
			return err
		}
		if inBody {
			*bodyFields = append(*bodyFields, index)
		}
	}
	return nil
}

// declaredLocation reports the location a field's own tags bind it from, file
// included, which is what the reachability checks below look for.
func declaredLocation(f reflect.StructField) (string, bool) {
	if _, declared := f.Tag.Lookup(tagFile); declared {
		return tagFile, true
	}
	if source, _, declared := locationTag(f); declared {
		return source.String(), true
	}
	return "", false
}

// checkUnreachable refuses a field the binder treats as a single unit, body
// content or an embedded pointer, when a location tag sits somewhere inside
// its type.
//
// The binder does not follow such a field, so the tag inside would do nothing,
// and doing nothing is the dangerous outcome: the value is decoded from the
// body along with its parent, which hands the client a field the developer
// believed came from the path, a header or a cookie. Supporting the case
// instead would mean allocating embedded pointers and inventing a meaning for
// a query parameter inside element three of a slice; refusing it costs one
// "embed by value" and leaves nothing to guess.
func checkUnreachable(f reflect.StructField) error {
	path, location, found := locatedWithin(f.Type, map[reflect.Type]bool{})
	if !found {
		return nil
	}
	if f.Anonymous && f.Type.Kind() == reflect.Pointer {
		return fmt.Errorf("field %s.%s declares a %s parameter inside the embedded pointer %s, which the binder does not follow, so the value could be set by the request body instead; embed %s by value rather than by pointer",
			f.Name, path, location, f.Type, f.Type.Elem())
	}
	return fmt.Errorf("field %s.%s declares a %s parameter inside the field %s, but located parameters are read only at the top level of the input and in structs embedded by value, so the value could be set by the request body instead; move the field to the top level of the input or embed its struct by value",
		f.Name, path, location, f.Name)
}

// locatedWithin searches a type for a field carrying a location tag, looking
// through pointers, slices, arrays and map values, since JSON reaches a struct
// through any of them. It returns the dotted path to the first such field and
// the location it declares.
//
// Only fields encoding/json can reach are searched: exported ones and embedded
// ones. The seen set stops a recursive type, such as a tree of nodes, from
// being walked forever. It has to be consulted for the types looked through as
// well as for the structs, since `type Tree map[string]Tree` is a recursive
// type with no struct in it, and unwrapping it never reaches anything else.
func locatedWithin(t reflect.Type, seen map[reflect.Type]bool) (path, location string, found bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		if seen[t] {
			return "", "", false
		}
		seen[t] = true
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return "", "", false
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		if location, declared := declaredLocation(f); declared {
			return f.Name, location, true
		}
		if path, location, found := locatedWithin(f.Type, seen); found {
			return f.Name + "." + path, location, true
		}
	}
	return "", "", false
}

// located counts the binders compiled so far, which is what tells
// collectFields whether an embedded struct contributed anything of its own or
// is body content like any other field. A Dep counts, so that an embedded
// struct holding one is split into its body members rather than decoded whole,
// which would make the Dep a member a client could send.
func (p *bindPlan) located() int {
	return len(p.params) + len(p.form) + len(p.files) + len(p.deps)
}

// usableField reports whether a struct field takes part in binding.
//
// An embedded struct of unexported type is usable even though the field itself
// is unexported, because its promoted exported fields are settable through
// reflection and encoding/json treats them as members of the outer object.
func usableField(f reflect.StructField) bool {
	return f.IsExported() || (f.Anonymous && f.Type.Kind() == reflect.Struct)
}

// locationTag reports which part of the request a field is read from, if the
// field declares one.
func locationTag(f reflect.StructField) (paramSource, string, bool) {
	for _, candidate := range []struct {
		tag string
		src paramSource
	}{
		{tagPath, srcPath},
		{tagQuery, srcQuery},
		{tagHeader, srcHeader},
		{tagCookie, srcCookie},
		{tagForm, srcForm},
	} {
		if value, ok := f.Tag.Lookup(candidate.tag); ok {
			// Everything after the first comma is discarded, the way encoding/json
			// reads its own tag. Nothing here has ever meant anything by an
			// option - `default` and `required` are tags of their own - but a
			// struct tag that looks like a json tag will eventually be written
			// like one, and `query:"limit,omitzero"` binding a parameter
			// literally named `limit,omitzero` is a filter that silently does
			// nothing. It fails as a default rather than as an error, which is
			// the worst way for a parameter to fail: the endpoint answers, and
			// answers the unfiltered question.
			name, _, _ := strings.Cut(value, ",")
			return candidate.src, name, true
		}
	}
	return 0, "", false
}

// newParamBinder compiles the binder for a single located field, resolving its
// setter and its requiredness once.
func newParamBinder(f reflect.StructField, index []int, source paramSource, name string) (paramBinder, error) {
	if name == "" {
		return paramBinder{}, fmt.Errorf("field %s declares an empty %s parameter name", f.Name, source)
	}
	set, err := setterFor(f.Type)
	if err != nil {
		return paramBinder{}, fmt.Errorf("field %s (%s parameter %q): %w", f.Name, source, name, err)
	}
	b := paramBinder{
		index:   index,
		source:  source,
		name:    name,
		typ:     f.Type,
		doc:     f.Tag.Get(tagDoc),
		isSlice: takesList(f.Type),
		set:     set,
	}
	if source == srcHeader {
		b.key = http.CanonicalHeaderKey(name)
	}
	b.defValue, b.hasDef = f.Tag.Lookup(tagDefault)
	// A path parameter is always present when the route matched, so it is
	// required by definition. A form value is body content, so it is required
	// like the body is, unless the field carries a default or says otherwise.
	// Everything else is optional unless the field asks otherwise, which keeps
	// optional filters and cursors ergonomic.
	explicit, declared := f.Tag.Lookup(tagRequired)
	switch {
	case source == srcPath:
		b.required = true
	case declared:
		b.required = explicit == "true"
	case source == srcForm:
		b.required = !b.hasDef
	}
	if b.required && b.hasDef {
		return paramBinder{}, fmt.Errorf("field %s (%s parameter %q) is both required and given a default", f.Name, source, name)
	}
	return b, nil
}

// setterFor resolves the conversion used to write text into a field of type t.
// The returned setter is stored in the plan, so the reflection performed here
// happens once per route rather than once per request.
func setterFor(t reflect.Type) (setter, error) {
	return setterAt(t, 0)
}

// maxSetterDepth is how many pointers and slices deep a parameter type may be
// wrapped. Nothing a request parameter can carry goes past a slice of
// pointers, so what is deeper is a type that contains itself, such as
// `type List []List`, which would otherwise be followed until the stack ran
// out.
const maxSetterDepth = 8

// setterAt is [setterFor] for a type reached through depth levels of wrapper.
func setterAt(t reflect.Type, depth int) (setter, error) {
	if depth > maxSetterDepth {
		return nil, fmt.Errorf("type %s is wrapped in more than %d levels of pointers and slices, or contains itself, and cannot be bound from a request parameter", t, maxSetterDepth)
	}
	// time.Duration is an int64 underneath but developers expect "1500ms", so
	// it is handled before the integer kinds claim it.
	if t == durationType {
		return func(dst reflect.Value, raw []string) error {
			d, err := time.ParseDuration(raw[0])
			if err != nil {
				return errNotDuration
			}
			dst.SetInt(int64(d))
			return nil
		}, nil
	}
	if reflect.PointerTo(t).Implements(textUnmarshaler) {
		return func(dst reflect.Value, raw []string) error {
			return dst.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(raw[0]))
		}, nil
	}

	switch t.Kind() {
	case reflect.String:
		return func(dst reflect.Value, raw []string) error {
			dst.SetString(raw[0])
			return nil
		}, nil
	case reflect.Bool:
		return func(dst reflect.Value, raw []string) error {
			v, err := strconv.ParseBool(raw[0])
			if err != nil {
				return errNotBool
			}
			dst.SetBool(v)
			return nil
		}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		bits := t.Bits()
		outside := integerRange(t)
		return func(dst reflect.Value, raw []string) error {
			// ParseInt takes a leading "+" that a JSON number may not have.
			if !isDecimal(raw[0], false) {
				return errNotInt
			}
			v, err := strconv.ParseInt(raw[0], 10, bits)
			if err != nil {
				// The text is decimal, so the one way left to fail is to be
				// outside the range of the type.
				return outside
			}
			dst.SetInt(v)
			return nil
		}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		bits := t.Bits()
		outside := integerRange(t)
		return func(dst reflect.Value, raw []string) error {
			v, err := strconv.ParseUint(raw[0], 10, bits)
			if err != nil {
				if errors.Is(err, strconv.ErrRange) {
					return outside
				}
				return errNotUint
			}
			dst.SetUint(v)
			return nil
		}, nil
	case reflect.Float32, reflect.Float64:
		bits := t.Bits()
		return func(dst reflect.Value, raw []string) error {
			// ParseFloat reads Go's own literal syntax, not JSON's: "NaN",
			// "Inf", "0x1p-2", "1_000" and ".5" are all numbers to it. NaN
			// fails every comparison, so `if in.Amount > balance` waves it
			// through, and infinity passes any lower bound; the rest are
			// numbers a JSON body refuses and the document's type: number
			// does not describe, which a proxy or a firewall reading the
			// parameter by those rules sees differently from the handler. So
			// the text must be a decimal number first. An overflow such as
			// 1e400 then fails with a range error.
			if !isDecimal(raw[0], true) {
				return errNotNumber
			}
			v, err := strconv.ParseFloat(raw[0], bits)
			if err != nil {
				return errNotNumber
			}
			dst.SetFloat(v)
			return nil
		}, nil
	case reflect.Pointer:
		elem, err := setterAt(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return func(dst reflect.Value, raw []string) error {
			v := reflect.New(dst.Type().Elem())
			if err := elem(v.Elem(), raw); err != nil {
				return err
			}
			dst.Set(v)
			return nil
		}, nil
	case reflect.Slice:
		elem, err := setterAt(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return func(dst reflect.Value, raw []string) error {
			out := reflect.MakeSlice(dst.Type(), len(raw), len(raw))
			for i := range raw {
				if err := elem(out.Index(i), raw[i:i+1]); err != nil {
					return &entryError{index: i + 1, err: err}
				}
			}
			dst.Set(out)
			return nil
		}, nil
	default:
		return nil, fmt.Errorf("type %s cannot be bound from a request parameter; supported kinds are strings, booleans, numbers, time.Duration, slices of those, and any type implementing encoding.TextUnmarshaler", t)
	}
}

// isDecimal reports whether text is a number as a JSON body writes one: an
// optional minus sign, digits, and, when fraction is true, an optional
// fraction and exponent. Nothing else is: no plus sign, no hexadecimal, no
// digit separators, no "NaN" or "Inf", and no point without a digit on each
// side of it.
//
// Leading zeros are the one liberty taken beyond JSON. They are decimal and
// mean the same thing to every reader, and "007" or "09" is what people type.
func isDecimal(text string, fraction bool) bool {
	i := 0
	digits := func() bool {
		start := i
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
		}
		return i > start
	}
	if i < len(text) && text[i] == '-' {
		i++
	}
	if !digits() {
		return false
	}
	if !fraction {
		return i == len(text)
	}
	if i < len(text) && text[i] == '.' {
		i++
		if !digits() {
			return false
		}
	}
	if i < len(text) && (text[i] == 'e' || text[i] == 'E') {
		i++
		if i < len(text) && (text[i] == '+' || text[i] == '-') {
			i++
		}
		if !digits() {
			return false
		}
	}
	return i == len(text)
}

// fieldByIndex resolves a possibly nested field for writing, allocating any
// intermediate pointers along the way.
func fieldByIndex(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 {
			if v.Kind() == reflect.Pointer {
				if v.IsNil() {
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()
			}
		}
		v = v.Field(x)
	}
	return v
}

// bind materializes the input value for a request.
//
// It returns a *ValidationError describing every parameter that failed, so a
// client learns about all of its mistakes at once, or an *HTTPError for
// request-level problems such as an oversized or wrongly typed body.
func (p *bindPlan) bind(c *Context, dst reflect.Value, route *Route) error {
	if p.empty {
		return nil
	}
	// Filled first, so that a Validate method can read a dependency, and safe
	// to fill first because nothing below can write to a Dep field.
	if p.deps != nil {
		if err := p.fillDeps(c, dst); err != nil {
			return err
		}
	}
	// The model's name is the narrowest scope a translated message is looked up
	// under, so it travels with the failures rather than being rediscovered by
	// whatever renders them.
	verr := &ValidationError{Model: snakeCase(p.typ.Name())}

	var query url.Values
	if p.needsQuery {
		query = c.r.URL.Query()
	}

	bindParams(p.params, c, dst, query, verr)

	decoded := true
	switch {
	case p.multipart:
		if err := p.bindMultipart(c, dst, route, verr); err != nil {
			return err
		}
	case p.body != nil:
		var err error
		if decoded, err = p.bindBody(c, dst, route, verr); err != nil {
			return err
		}
	}

	// Validation runs even when binding found problems, so a client learns
	// about every field at once. Fields that already failed to bind are left
	// out, because a value that could not be parsed has nothing further to say.
	//
	// A body that failed to decode is different, and the model is not
	// validated at all. The decoder stops at the first member it cannot read,
	// so the model holds part of a body: neither what the client sent nor
	// anything it could have sent, and with a located parameter beside the body
	// nothing of it at all, since the scratch value the body went into is not
	// copied out. Rules run over that report members the client did send as
	// missing, and a Must check, or a rule on a parameter that depends on a
	// body member, judges a value nobody wrote. What failed to bind on its own,
	// a path, query, header or cookie parameter, is still reported, because it
	// does not depend on the body; the model's rules wait for a body that
	// decodes.
	if p.validation != nil && !route.skipValidation && decoded {
		failed := make(map[fieldKey]bool, len(verr.Details))
		for _, detail := range verr.Details {
			failed[fieldKey{detail.Location, detail.Field}] = true
		}
		verr.Details = append(verr.Details, p.runValidation(dst, failed)...)
	}

	if len(verr.Details) > 0 {
		return verr
	}
	return nil
}

// Errors the parameter setters report.
//
// They are package-level values because the same wording is produced on every
// failure, so a rejected request no longer allocates one, and because a
// sentinel is what lets [bindingKeyFor] name the translation of each without
// matching on text.
var (
	errNotDuration = errors.New("must be a valid duration, such as 1500ms")
	errNotBool     = errors.New("must be true or false")
	errNotInt      = errors.New("must be a valid integer")
	errNotUint     = errors.New("must be a valid non-negative integer")
	errNotNumber   = errors.New("must be a valid number")
	errNotText     = errors.New("is not in the expected format")
	errRepeated    = errors.New("must be given only once")
)

// bindingKeys maps each of those onto the key its translation is written under.
var bindingKeys = map[error]string{
	errNotDuration: "duration",
	errNotBool:     "boolean",
	errNotInt:      "integer",
	errNotUint:     "unsigned",
	errNotNumber:   "number",
	errNotText:     "format",
	errRepeated:    "repeated",
}

// rangeError reports a whole number that is well formed but does not fit the
// type it is meant for, naming the range that would have.
//
// "Must be a valid integer" is true of 300 for an int8 and tells the client
// nothing it can act on, since 300 is a perfectly valid integer. The range is a
// property of the type, so one value is built per setter when the route is
// compiled and returned for every failure.
type rangeError struct {
	low, high string
}

func (e *rangeError) Error() string { return "must be between " + e.low + " and " + e.high }

// integerRange returns the failure for a value outside an integer type.
func integerRange(t reflect.Type) *rangeError {
	shift := 64 - t.Bits()
	if t.Kind() >= reflect.Uint {
		return &rangeError{low: "0", high: strconv.FormatUint(math.MaxUint64>>shift, 10)}
	}
	return &rangeError{
		low:  strconv.FormatInt(int64(math.MinInt64)>>shift, 10),
		high: strconv.FormatInt(math.MaxInt64>>shift, 10),
	}
}

// entryError reports a failure inside a repeated parameter, naming the entry
// by its position.
//
// The position is all it names. The value is the client's own text, which a
// client already has, and quoting it back turned every rejected entry into an
// amplifier: a header of 450 KB of non-UTF-8 bytes came back quoted at four
// times the size. A position is enough for a client to find its mistake.
type entryError struct {
	index int
	err   error
}

func (e *entryError) Error() string { return fmt.Sprintf("entry %d %v", e.index, e.err) }

func (e *entryError) Unwrap() error { return e.err }

// paramIssue turns a setter's failure into the issue a client is sent, with
// the translation key and arguments that go with it.
//
// Every built-in kind already fails with one of the fixed phrases above. A type
// implementing encoding.TextUnmarshaler writes its own error, and the standard
// library's own such types write errors that are no fit for a client:
// time.Time names its layout string and quotes the whole input back, and
// netip.Addr names the function that failed. So the text of such an error is
// replaced with a fixed phrase too, and only an [*HTTPError] is passed
// through, since its Message is by contract written for the client. The same
// setters serve [LoadConfig], whose errors go to an operator rather than a
// client, which is why the replacement happens here rather than in the
// setter.
func paramIssue(err error) (issue, key string, args []any) {
	var entry *entryError
	if errors.As(err, &entry) {
		issue, key, args = paramIssue(entry.err)
		return fmt.Sprintf("entry %d %s", entry.index, issue), key, args
	}
	var outside *rangeError
	if errors.As(err, &outside) {
		return outside.Error(), "muzak.binding.range", []any{"min", outside.low, "max", outside.high}
	}
	if key := bindingKey(err); key != "" {
		return err.Error(), key, nil
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.Message != "" {
		return httpErr.Message, httpErr.MessageKey, httpErr.MessageArgs
	}
	return errNotText.Error(), bindingKey(errNotText), nil
}

// bindingKeyFor reports the rule a binding failure came from, or the empty
// string for one this package has no translation of.
//
// It walks the sentinels with errors.Is rather than comparing directly, because
// a failure inside a repeated parameter arrives wrapped in the entry it came
// from.
func bindingKeyFor(err error) string {
	for sentinel, key := range bindingKeys {
		if errors.Is(err, sentinel) {
			return key
		}
	}
	return ""
}

// bindParams writes each supplied parameter into its field, recording a
// failure for every one that is missing or malformed rather than stopping at
// the first, so a client learns about all of them at once.
func bindParams(binders []paramBinder, c *Context, dst reflect.Value, query url.Values, verr *ValidationError) {
	for i := range binders {
		b := &binders[i]
		raw, present := b.lookup(c, query)
		if !present {
			if b.required {
				verr.addKeyed(b.source.String(), b.name, "is required", "blank")
				continue
			}
			if !b.hasDef {
				continue
			}
			raw = []string{b.defValue}
		}
		if len(raw) > 1 && !b.isSlice {
			// A field that holds one value was sent several. Taking the first
			// is what this did, taking the last is what FastAPI and most
			// proxies do, and when the component that checks a request and
			// the one that serves it pick differently, a request passes the
			// check with one value and is served with another. Refusing it
			// leaves nothing to pick.
			verr.addKey(b.source.String(), b.name, errRepeated.Error(), bindingKey(errRepeated))
			continue
		}
		if err := b.set(fieldByIndex(dst, b.index), raw); err != nil {
			issue, key, args := paramIssue(err)
			verr.addKey(b.source.String(), b.name, issue, key, args...)
		}
	}
}

// lookup fetches the raw textual values supplied for a parameter and reports
// whether it was present at all, which is what distinguishes an empty value
// from a missing one.
func (b *paramBinder) lookup(c *Context, query url.Values) ([]string, bool) {
	switch b.source {
	case srcPath:
		v, ok := c.params.Get(b.name)
		if !ok {
			return nil, false
		}
		return []string{v}, true
	case srcQuery:
		v, ok := query[b.name]
		if !ok || len(v) == 0 {
			return nil, false
		}
		return v, true
	case srcHeader:
		// net/http lifts two headers out of the map and onto the request
		// itself, so a binder that read only the map would report them absent
		// on every request that carried them.
		switch b.key {
		case "Host":
			if c.r.Host == "" {
				return nil, false
			}
			return []string{c.r.Host}, true
		case "Content-Length":
			// A length of zero cannot be told from an absent header once
			// net/http has parsed the request, so it is reported as sent.
			if c.r.ContentLength < 0 {
				return nil, false
			}
			return []string{strconv.FormatInt(c.r.ContentLength, 10)}, true
		}
		v, ok := c.r.Header[b.key]
		if !ok || len(v) == 0 {
			return nil, false
		}
		if b.isSlice {
			return splitHeaderList(v), true
		}
		return v, true
	case srcForm:
		// The body has already been parsed by the time a form binder runs, and
		// net/http copies multipart values into PostForm too, so one lookup
		// serves both encodings.
		v, ok := c.r.PostForm[b.name]
		if !ok || len(v) == 0 {
			return nil, false
		}
		return v, true
	default:
		cookie, err := c.r.Cookie(b.name)
		if err != nil {
			return nil, false
		}
		return []string{cookie.Value}, true
	}
}

// splitHeaderList reads the lines of a header bound to a slice as the list
// RFC 9110 section 5.6.1 says they are.
//
// A sender may write a list as several lines, as one line of comma-separated
// elements, or as both, and the three mean the same thing, so each line is
// split on its commas: the spaces and tabs around an element are dropped and
// an empty element is skipped, as the RFC requires of a recipient. A comma
// inside a quoted string, such as an entity tag, belongs to the element, which
// keeps its quotes. A header holding nothing but separators is an empty list.
func splitHeaderList(lines []string) []string {
	out := make([]string, 0, len(lines))
	add := func(element string) {
		if element = strings.Trim(element, " \t"); element != "" {
			out = append(out, element)
		}
	}
	for _, line := range lines {
		start, quoted, escaped := 0, false, false
		for i := 0; i < len(line); i++ {
			switch c := line[i]; {
			case escaped:
				escaped = false
			case quoted && c == '\\':
				escaped = true
			case c == '"':
				quoted = !quoted
			case c == ',' && !quoted:
				add(line[start:i])
				start = i + 1
			}
		}
		add(line[start:])
	}
	return out
}

// declaredOverLimit reports whether a request declares a body longer than
// limit, which is known from its Content-Length before any of the body is read.
//
// Reading up to the limit and refusing then gives the same answer at the cost
// of the bytes, and worse, a request that sent Expect: 100-continue has been
// told to go ahead by the time the first read happens. A body that does not
// declare a length, as a chunked one does not, is still bounded as it is read.
// A limit that is not positive is no limit.
func declaredOverLimit(r *http.Request, limit int64) bool {
	return limit > 0 && r.ContentLength > limit
}

// bindBody reads, size-limits and decodes the JSON request body.
//
// It reports whether the body decoded, which is false only when the client
// sent one the decoder could not read into the input; an absent body is not a
// failure to decode, and is reported as missing when the route requires one.
func (p *bindPlan) bindBody(c *Context, dst reflect.Value, route *Route, verr *ValidationError) (bool, error) {
	labelled, err := checkContentType(c.r)
	if err != nil {
		return false, err
	}

	// A body that declares a length over the limit is refused before it is
	// read, so a client that asked for a 100 Continue is not told to send it.
	if declaredOverLimit(c.r, route.maxBodySize) {
		return false, NewHTTPErrorf(http.StatusRequestEntityTooLarge,
			"request body exceeds the %d byte limit for this route", route.maxBodySize)
	}

	buf := bodyBufferPool.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() <= maxPooledBodyBuffer {
			buf.Reset()
			bodyBufferPool.Put(buf)
		}
	}()
	buf.Reset()

	// A limit that is not positive is the documented way to remove it. It is
	// not passed on: MaxBytesReader clamps a negative limit to zero and
	// would refuse every body.
	var source io.Reader = c.r.Body
	if route.maxBodySize > 0 {
		source = http.MaxBytesReader(c.w, c.r.Body, route.maxBodySize)
	}
	if _, err := buf.ReadFrom(source); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return false, NewHTTPErrorf(http.StatusRequestEntityTooLarge,
				"request body exceeds the %d byte limit for this route", route.maxBodySize).Wrap(err)
		}
		return false, NewHTTPError(http.StatusBadRequest, "the request body could not be read").Wrap(err)
	}

	if buf.Len() == 0 {
		if p.body.required {
			verr.add("body", "", "is required")
		}
		return true, nil
	}
	if !labelled {
		return false, unlabelledBody()
	}

	target := dst
	if !p.body.direct {
		// Decoding into a scratch value that holds the body members alone and
		// copying them out means a crafted body can never reach a field that
		// is supposed to come from the path, a query parameter or a header,
		// nor even name one. See [bodyPlan.narrow].
		target = reflect.New(p.body.shape).Elem()
	}
	// The decoder leaves a member the body does not mention as it found it,
	// so a default written first is what a client that omits it gets, and one
	// that sends it overrides. The defaults were checked when the route was
	// registered, so there is no failure to report here.
	for i := range p.body.defaults {
		d := &p.body.defaults[i]
		_ = d.set(fieldByIndex(target, d.at), []string{d.raw})
	}
	if err := json.Unmarshal(buf.Bytes(), target.Addr().Interface(), route.jsonReadOptions()); err != nil {
		if typeFault(err) {
			// The type the route declared cannot hold what was sent, whatever
			// was sent, so the request did nothing wrong. It is answered as
			// the server's failure and logged where the developer will see
			// it, rather than reported to the client as its own mistake.
			return false, fmt.Errorf("muzak: %s %s: the request body could not be decoded into %s, through no fault of the request: %w",
				route.Method, route.Path, p.typ, err)
		}
		verr.Details = append(verr.Details, decodeIssue(err, target.Type()))
		return false, nil
	}
	for _, c := range p.body.copies {
		fieldByIndex(dst, c.to).Set(fieldByIndex(target, c.from))
	}
	return true, nil
}

// checkContentType rejects a body sent under a media type Muzak cannot
// decode, and reports whether the request declared a JSON one at all.
//
// A missing Content-Type is not refused here, only reported, because whether
// it matters depends on whether a body follows: a bodiless call to a route
// whose body is optional has nothing to label. [bindPlan.bindBody] refuses it
// once it has read a non-empty body.
func checkContentType(r *http.Request) (labelled bool, err error) {
	mediaType, err := requestMediaType(r)
	if err != nil {
		return false, err
	}
	if mediaType == "" {
		return false, nil
	}
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		return true, nil
	}
	return false, NewHTTPErrorf(http.StatusUnsupportedMediaType,
		"unsupported media type %q; this route accepts application/json", mediaType)
}

// unlabelledBody refuses a JSON route's body sent with no Content-Type.
//
// It used to be accepted on the grounds that the decoder rejects anything
// that is not JSON anyway, and that is true and beside the point. A browser
// sends a cross-site POST without asking first only when it looks like
// something a form could have sent, and a fetch whose body is a Blob with no
// type goes out with no Content-Type at all. Accepting it made every JSON
// route reachable from any page a signed-in user visited, cookies attached,
// without the CORS preflight that application/json triggers and the CORS
// policy is there to answer. Requiring the label puts every such request
// back behind that preflight.
//
// A fresh error is built each time rather than shared, because an error on its
// way to the client is annotated as it goes.
func unlabelledBody() error {
	return NewHTTPError(http.StatusUnsupportedMediaType,
		"the request carries a body but no Content-Type; this route accepts application/json")
}

// requestMediaType returns the media type of a request body, without its
// parameters, or the empty string when no Content-Type was sent.
func requestMediaType(r *http.Request) (string, error) {
	raw := r.Header.Get("Content-Type")
	if raw == "" {
		return "", nil
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", NewHTTPError(http.StatusUnsupportedMediaType, "the Content-Type header is malformed").Wrap(err)
	}
	return mediaType, nil
}

// jsonReadOptions returns the decoder options for the route. Duplicate object
// members and invalid UTF-8 are rejected by encoding/json/v2 by default;
// unknown members are rejected too unless the route opted out with
// [AllowUnknownFields].
func (rt *Route) jsonReadOptions() json.Options {
	return json.JoinOptions(json.RejectUnknownMembers(!rt.allowUnknownFields), durationJSON)
}

// durationJSON gives a time.Duration in a JSON body the representation the
// document describes, a string such as "1500ms" that time.ParseDuration reads
// and Duration.String writes.
//
// encoding/json/v2 has none of its own: it refuses to encode or decode a
// Duration that carries no format, and a format tag is not accepted for it
// either, so a body member of that type was a 422 for every value a client
// could send and a 500 for every response that held one, while the document
// called it a duration string and a request parameter of the same type read
// "1500ms" without trouble. A number is refused as the wrong type, and a null
// leaves the zero value, as it does for any other scalar.
var durationJSON = json.JoinOptions(
	json.WithMarshalers(json.MarshalToFunc(func(enc *jsontext.Encoder, d time.Duration) error {
		return enc.WriteToken(jsontext.String(d.String()))
	})),
	json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, d *time.Duration) error {
		switch kind := dec.PeekKind(); kind {
		case 'n':
			*d = 0
			return dec.SkipValue()
		case '"':
		default:
			return &json.SemanticError{GoType: reflect.TypeFor[time.Duration](), JSONKind: kind}
		}
		var text string
		if err := json.UnmarshalDecode(dec, &text); err != nil {
			return err
		}
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return errNotDuration
		}
		*d = parsed
		return nil
	})),
)

// discardBody drops what is left of a request body that nothing more will read,
// up to 4 KiB, so that a keep-alive connection can be reused instead of being
// torn down. It is for after a handler, never before one: it would take the
// front of a body the handler meant to read.
func discardBody(r *http.Request) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 4<<10))
	}
}

// captureRequestBody reads the body into the Context and puts an identical
// reader back, for a route that declared [CaptureBody].
//
// Reading it here rather than reusing the buffer the binder fills is
// deliberate. It makes the bytes available to a guard, which is where a
// signature check belongs -- an unsigned request should be refused before
// anything decodes it -- and it works the same for a route whose input is
// [Empty] and for one that binds a multipart form, neither of which reaches
// bindBody at all.
func captureRequestBody(c *Context, route *Route) error {
	if c.r.Body == nil {
		c.rawBodyCaptured = true
		return nil
	}

	// One byte past the limit, so a body exactly at it is not mistaken for one
	// over. MaxBytesReader is not used here because its error is reported
	// against the response writer, and this read happens before the binder's
	// own limit would apply.
	//
	// A limit that is not positive removes it, and one too large to add a byte
	// to is as good as none: the sum would overflow into a negative count, which
	// LimitReader reads as an empty body.
	limit := route.maxBodySize
	var source io.Reader = c.r.Body
	if limit > 0 && limit < math.MaxInt64 {
		source = io.LimitReader(c.r.Body, limit+1)
	}
	body, err := io.ReadAll(source)
	if err != nil {
		return NewHTTPError(http.StatusBadRequest, "the request body could not be read").Wrap(err)
	}

	if limit > 0 && int64(len(body)) > limit {
		// Refused here rather than truncated. A truncated body fails its
		// signature check, and a signature failure reads as an attack rather
		// than as the oversized request this is.
		return NewHTTPErrorf(http.StatusRequestEntityTooLarge,
			"request body exceeds the %d byte limit for this route", limit)
	}

	c.rawBody = body
	c.rawBodyCaptured = true
	c.r.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}
