package muzak

import (
	"bytes"
	"encoding"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"reflect"
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
// part of the JSON body. A json:"-" tag keeps a field out of the body and its
// schema and nothing more, so a located field carrying one is still bound from
// its location.
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
	isSlice  bool
	set      setter
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
		plan.body = &bodyPlan{
			// Decoding straight into the input value is only safe when the
			// input has no located fields at all. Counting body fields against
			// the field count is not enough: an embedded struct holding both a
			// located parameter and a body member counts as one field on each
			// side, which would let a crafted body reach the located field.
			direct:   len(plan.params) == 0 && len(bodyFields) == totalFields(t),
			fields:   bodyFields,
			required: true,
		}
	}
	return plan, nil
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
// being walked forever.
func locatedWithin(t reflect.Type, seen map[reflect.Type]bool) (path, location string, found bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
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
// is body content like any other field.
func (p *bindPlan) located() int {
	return len(p.params) + len(p.form) + len(p.files)
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
		isSlice: f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() != reflect.Uint8,
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
		return func(dst reflect.Value, raw []string) error {
			v, err := strconv.ParseInt(raw[0], 10, bits)
			if err != nil {
				return errNotInt
			}
			dst.SetInt(v)
			return nil
		}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		bits := t.Bits()
		return func(dst reflect.Value, raw []string) error {
			v, err := strconv.ParseUint(raw[0], 10, bits)
			if err != nil {
				return errNotUint
			}
			dst.SetUint(v)
			return nil
		}, nil
	case reflect.Float32, reflect.Float64:
		bits := t.Bits()
		return func(dst reflect.Value, raw []string) error {
			// ParseFloat reads "NaN", "Inf" and "Infinity" as numbers, and
			// neither belongs in a parameter. NaN fails every comparison, so
			// `if in.Amount > balance` waves it through; infinity passes any
			// lower bound. A JSON body cannot carry either, so refusing them
			// here makes a query string no more permissive than a body. An
			// overflow such as 1e400 already fails with a range error.
			v, err := strconv.ParseFloat(raw[0], bits)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return errNotNumber
			}
			dst.SetFloat(v)
			return nil
		}, nil
	case reflect.Pointer:
		elem, err := setterFor(t.Elem())
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
		elem, err := setterFor(t.Elem())
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
	// The model's name is the narrowest scope a translated message is looked up
	// under, so it travels with the failures rather than being rediscovered by
	// whatever renders them.
	verr := &ValidationError{Model: snakeCase(p.typ.Name())}

	var query url.Values
	if p.needsQuery {
		query = c.r.URL.Query()
	}

	bindParams(p.params, c, dst, query, verr)

	switch {
	case p.multipart:
		if err := p.bindMultipart(c, dst, route, verr); err != nil {
			return err
		}
	case p.body != nil:
		if err := p.bindBody(c, dst, route, verr); err != nil {
			return err
		}
	}

	// Validation runs even when binding found problems, so a client learns
	// about every field at once. Fields that already failed to bind are left
	// out, because a value that could not be parsed has nothing further to say.
	if p.validation != nil && !route.skipValidation {
		failed := make(map[string]bool, len(verr.Details))
		for _, detail := range verr.Details {
			failed[detail.Field] = true
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
)

// bindingKeys maps each of those onto the key its translation is written under.
var bindingKeys = map[error]string{
	errNotDuration: "duration",
	errNotBool:     "boolean",
	errNotInt:      "integer",
	errNotUint:     "unsigned",
	errNotNumber:   "number",
	errNotText:     "format",
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

// bindBody reads, size-limits and decodes the JSON request body.
func (p *bindPlan) bindBody(c *Context, dst reflect.Value, route *Route, verr *ValidationError) error {
	labelled, err := checkContentType(c.r)
	if err != nil {
		return err
	}

	buf := bodyBufferPool.Get().(*bytes.Buffer)
	defer func() {
		if buf.Cap() <= maxPooledBodyBuffer {
			buf.Reset()
			bodyBufferPool.Put(buf)
		}
	}()
	buf.Reset()

	limited := http.MaxBytesReader(c.w, c.r.Body, route.maxBodySize)
	if _, err := buf.ReadFrom(limited); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return NewHTTPErrorf(http.StatusRequestEntityTooLarge,
				"request body exceeds the %d byte limit for this route", route.maxBodySize).Wrap(err)
		}
		return NewHTTPError(http.StatusBadRequest, "the request body could not be read").Wrap(err)
	}

	if buf.Len() == 0 {
		if p.body.required {
			verr.add("body", "", "is required")
		}
		return nil
	}
	if !labelled {
		return unlabelledBody()
	}

	target := dst
	if !p.body.direct {
		// Decoding into a scratch value of the same type and copying only the
		// body-bound fields out means a crafted body can never reach a field
		// that is supposed to come from the path, a query parameter or a
		// header.
		target = reflect.New(p.typ).Elem()
	}
	if err := json.Unmarshal(buf.Bytes(), target.Addr().Interface(), route.jsonReadOptions()); err != nil {
		field, issue := decodeIssue(err)
		verr.add("body", field, issue)
		return nil
	}
	if !p.body.direct {
		for _, index := range p.body.fields {
			fieldByIndex(dst, index).Set(fieldByIndex(target, index))
		}
	}
	return nil
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
	return json.RejectUnknownMembers(!rt.allowUnknownFields)
}

// discardBody drains and closes a request body that no handler will read, so
// that keep-alive connections can be reused instead of being torn down.
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
	limit := route.maxBodySize
	body, err := io.ReadAll(io.LimitReader(c.r.Body, limit+1))
	if err != nil {
		return NewHTTPError(http.StatusBadRequest, "the request body could not be read").Wrap(err)
	}

	if int64(len(body)) > limit {
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
