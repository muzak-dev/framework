package muzak

import (
	"bytes"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file writes a value of an endpoint's input type as the request the
// binder reads back into an equal value: the inverse of binding.go, compiled
// from the binder's own plan so that the two cannot disagree about which field
// comes from where. Where the request has no way to carry a value unchanged,
// the call is refused before anything is sent, with an error naming the field
// and the reason and never the value, which may be a credential.
//
// Everything here is linear in the size of the value being written.

// ErrCallRefused is wrapped by every error [Endpoint.Call] returns because a
// value of the input could not be written as a request the server would read
// back unchanged, such as a path parameter holding "/" or a header holding a
// line break. Nothing was sent. Test for it with [errors.Is]:
//
//	if errors.Is(err, muzak.ErrCallRefused) {
//		return Out{}, muzak.BadRequest("that name cannot be looked up")
//	}
//
// It is the caller's input that is at fault, which is why it is told apart
// from a failure to reach the service or an answer the service refused.
var ErrCallRefused = errors.New("muzak: the call was refused before it was sent")

// callRefusal is an error that wraps [ErrCallRefused] without repeating its
// text, so a message reads as one sentence.
type callRefusal struct{ message string }

func (e *callRefusal) Error() string        { return e.message }
func (e *callRefusal) Is(target error) bool { return target == ErrCallRefused }

// refuse builds a [callRefusal] for the endpoint at method and path.
func refuse(method, path, format string, args ...any) error {
	return &callRefusal{message: fmt.Sprintf("muzak: %s %s: ", method, path) + fmt.Sprintf(format, args...)}
}

// callPlan is how a value of one endpoint's input type is written as a
// request. It is compiled once per endpoint, on the first call, and only read
// afterwards.
type callPlan struct {
	method, path string
	// bind is the plan the server compiles for the same input, which is
	// where every decision below comes from.
	bind *bindPlan
	// segments is the path template in order, static text and parameters.
	segments []pathSegment
	// params are the query, header and cookie fields; form and files the
	// fields of a form body.
	params []callParam
	form   []callParam
	files  []fileBinder
	// headers holds the canonical name of every header the input binds,
	// which a [CallHeader] may not set as well.
	headers map[string]bool
	// output is the kind of the output type, and html and empty say whether
	// it is [HTML] or [Empty], which are not encoded as JSON.
	output outputKind
	html   bool
	empty  bool
	// validate is false when the endpoint was declared with
	// [SkipValidation], which [ValidateFirst] honours as the server does.
	validate bool
	// bodyShape is the type the JSON body is encoded from, and bodyCopies
	// move the body members of the input into it; see [callPlan.encodeBody].
	bodyShape  reflect.Type
	bodyCopies []bodyCopy
	// omitted are the body members with a default whose json tag may leave
	// them out of the body; see [callPlan.checkOmitted].
	omitted []bodyDefault
}

// pathSegment is one segment of a path template: static text, written as it
// is, or a parameter, written from the field that binds it.
type pathSegment struct {
	static string
	param  *callParam
	rest   bool
}

// callParam writes one located field.
type callParam struct {
	index    []int
	source   paramSource
	name     string
	required bool
	isSlice  bool
	encode   textEncoder
}

// compileCall builds the call plan for an endpoint whose input type is in and
// whose output type is out.
func compileCall(in, out reflect.Type, method, path string, opts []RouteOption) (*callPlan, error) {
	bind, err := newBindPlan(in, method, path)
	if err != nil {
		return nil, err
	}
	var cfg routeConfig
	for _, opt := range opts {
		opt.applyRoute(&cfg)
	}
	p := &callPlan{
		method:   method,
		path:     path,
		bind:     bind,
		headers:  map[string]bool{},
		output:   outputKindOf(out),
		html:     out == reflect.TypeFor[HTML](),
		empty:    out == emptyType,
		validate: !cfg.skipValidation,
	}
	if p.output == outputFile {
		return nil, fmt.Errorf("muzak: %s %s answers with a muzak.FileResponse, which names a file on the server and cannot be rebuilt from a response; "+
			"declare the endpoint's output as muzak.Stream, which the server sends the same way and a call hands back as a body to read", method, path)
	}
	if body := bind.body; body != nil {
		p.bodyShape, p.bodyCopies = body.shape, body.copies
		if !body.direct && body.shape == in && !encodesItself(in) {
			// The input decodes itself, so the binder reads the body into
			// the input whole. It is written from its body members alone,
			// which is what its own fields encode as once the located ones
			// and the Deps are left out; see encodeBody.
			p.bodyShape, p.bodyCopies = bodyShape(in, nil)
		}
		if !encodesItself(in) {
			for _, d := range body.defaults {
				if tag, _ := parseJSONTag(in.FieldByIndex(d.index)); tag.omitzero || tag.omitempty {
					p.omitted = append(p.omitted, d)
				}
			}
		}
	}
	errs := p.compileParams()
	errs = append(errs, p.compileForm()...)
	if err := p.compilePath(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

// headersNotBound are the headers an input may not bind if it is to be
// called, because net/http writes them itself, from the request's own fields,
// or because they say how the request is framed: a value set for one would be
// replaced, dropped or would change what the rest of the request means.
var headersNotBound = map[string]bool{
	"Host":              true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Keep-Alive":        true,
	"Proxy-Connection":  true,
	"Te":                true,
	"Trailer":           true,
	"Upgrade":           true,
	"Cookie":            true,
	// The transport asks for gzip unless one is set, and decompresses the
	// answer only when it asked itself, so a value of the input's would
	// either arrive when left out or hand the decoder compressed bytes.
	"Accept-Encoding": true,
}

// compileParams compiles the path, query, header and cookie fields.
func (p *callPlan) compileParams() []error {
	var errs []error
	seen := map[string]string{}
	for i := range p.bind.params {
		b := &p.bind.params[i]
		f := p.bind.typ.FieldByIndex(b.index)
		encode, err := textEncoderFor(b.typ)
		if err != nil {
			errs = append(errs, fmt.Errorf("muzak: %s %s: field %s (%s parameter %q): %w", p.method, p.path, f.Name, b.source, b.name, err))
			continue
		}
		name := b.name
		switch b.source {
		case srcHeader:
			name = b.key
			switch {
			case !isHTTPToken(b.name):
				err = fmt.Errorf("field %s binds the header %q, which is not a valid header name, so no request can carry it", f.Name, b.name)
			case headersNotBound[name]:
				err = fmt.Errorf("field %s binds the header %s, which net/http writes itself or which frames the request, so a call cannot send a value for it", f.Name, name)
			case name == "Content-Type" && (p.bind.body != nil || p.bind.multipart):
				err = fmt.Errorf("field %s binds the header Content-Type, which a call sets to describe the body this input also has", f.Name)
			}
			p.headers[name] = true
		case srcCookie:
			if !isHTTPToken(b.name) {
				err = fmt.Errorf("field %s binds the cookie %q, which is not a valid cookie name, so no request can carry it", f.Name, b.name)
			}
		}
		key := b.source.String() + "\x00" + name
		if other, taken := seen[key]; taken && err == nil {
			err = fmt.Errorf("fields %s and %s both bind the %s parameter %q, and a request can carry only one value for both", other, f.Name, b.source, b.name)
		}
		seen[key] = f.Name
		if err != nil {
			errs = append(errs, fmt.Errorf("muzak: %s %s: %w", p.method, p.path, err))
			continue
		}
		p.params = append(p.params, callParam{
			index: b.index, source: b.source, name: name,
			required: b.required, isSlice: b.isSlice, encode: encode,
		})
	}
	return errs
}

// compileForm compiles the fields of a form body, values and files.
func (p *callPlan) compileForm() []error {
	var errs []error
	seen := map[string]string{}
	add := func(field, name string) error {
		if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("field %s binds the form field %q, whose name holds a control character that would break the part header it is written into", field, name)
		}
		if other, taken := seen[name]; taken {
			return fmt.Errorf("fields %s and %s both bind the form field %q, and a request can carry only one value for both", other, field, name)
		}
		seen[name] = field
		return nil
	}
	for i := range p.bind.form {
		b := &p.bind.form[i]
		f := p.bind.typ.FieldByIndex(b.index)
		encode, err := textEncoderFor(b.typ)
		if err == nil {
			err = add(f.Name, b.name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("muzak: %s %s: field %s (form field %q): %w", p.method, p.path, f.Name, b.name, err))
			continue
		}
		p.form = append(p.form, callParam{
			index: b.index, source: srcForm, name: b.name,
			required: b.required, isSlice: b.isSlice, encode: encode,
		})
	}
	for _, b := range p.bind.files {
		f := p.bind.typ.FieldByIndex(b.index)
		var err error
		if b.kind == fileOne || b.kind == fileMany {
			// A File is the handle a handler is given on a part the parser
			// stored; it holds no content of its own a caller could fill.
			err = fmt.Errorf("field %s is a %s, which is what a handler receives and holds no content a caller can set; declare it []byte, or [][]byte for several files, to send it with Call",
				f.Name, f.Type)
		} else {
			err = add(f.Name, b.name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("muzak: %s %s: %w", p.method, p.path, err))
			continue
		}
		p.files = append(p.files, b)
	}
	return errs
}

// compilePath splits the template into its segments and finds the field that
// binds each parameter. A parameter no field binds is refused: the router
// accepts one, since it only ever reads the path, but a call has nothing to
// write in its place.
func (p *callPlan) compilePath() error {
	for seg := range strings.SplitSeq(p.path[1:], "/") {
		if len(seg) < 2 || seg[0] != '{' || seg[len(seg)-1] != '}' {
			p.segments = append(p.segments, pathSegment{static: seg})
			continue
		}
		name, rest := strings.CutSuffix(seg[1:len(seg)-1], "...")
		i := slices.IndexFunc(p.params, func(c callParam) bool { return c.source == srcPath && c.name == name })
		if i < 0 {
			if slices.ContainsFunc(p.bind.params, func(b paramBinder) bool { return b.source == srcPath && b.name == name }) {
				// Bound by a field compileParams refused, which it reported.
				continue
			}
			return fmt.Errorf("muzak: %s %s: the path parameter %q is bound by no field of the input, so a call has nothing to write in its place; bind it with a field tagged path:%q",
				p.method, p.path, name, name)
		}
		p.segments = append(p.segments, pathSegment{param: &p.params[i], rest: rest})
	}
	return nil
}

// textEncoder writes the value of a located field as the texts its setter
// reads back: none when the field is absent, one for a single value, and one
// per entry for a list.
type textEncoder func(v reflect.Value) ([]string, error)

// textEncoderFor resolves the encoder for a field of type t. It mirrors
// [setterFor] case for case and in the same order, which is what makes the
// two inverses: a time.Duration is written before the integer kinds could
// claim it, a type that reads itself from text writes itself as text, and a
// pointer or a slice is followed exactly as the setter follows it.
func textEncoderFor(t reflect.Type) (textEncoder, error) {
	return textEncoderAt(t, 0)
}

// textEncoderAt is [textEncoderFor] for a type reached through depth levels of
// pointers and slices.
func textEncoderAt(t reflect.Type, depth int) (textEncoder, error) {
	if depth > maxSetterDepth {
		// coverage: the binder's plan is compiled first and refuses the same
		// type with the same bound, so this only guards a future change to
		// one of the two.
		return nil, fmt.Errorf("type %s is wrapped in more than %d levels of pointers and slices", t, maxSetterDepth)
	}
	if t == durationType {
		return func(v reflect.Value) ([]string, error) {
			return []string{time.Duration(v.Int()).String()}, nil
		}, nil
	}
	if reflect.PointerTo(t).Implements(textUnmarshaler) {
		return textMarshalerFor(t)
	}
	switch t.Kind() {
	case reflect.String:
		return func(v reflect.Value) ([]string, error) { return []string{v.String()}, nil }, nil
	case reflect.Bool:
		return func(v reflect.Value) ([]string, error) { return []string{strconv.FormatBool(v.Bool())}, nil }, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return func(v reflect.Value) ([]string, error) { return []string{strconv.FormatInt(v.Int(), 10)}, nil }, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return func(v reflect.Value) ([]string, error) { return []string{strconv.FormatUint(v.Uint(), 10)}, nil }, nil
	case reflect.Float32, reflect.Float64:
		bits := t.Bits()
		return func(v reflect.Value) ([]string, error) {
			f := v.Float()
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, errNotFinite
			}
			// The shortest text that reads back as the same value of this
			// size. It is in the decimal grammar the setter accepts: an
			// optional minus, digits, an optional fraction and an optional
			// exponent, which is all 'g' ever writes for a finite number.
			return []string{strconv.FormatFloat(f, 'g', -1, bits)}, nil
		}, nil
	case reflect.Pointer:
		elem, err := textEncoderAt(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return func(v reflect.Value) ([]string, error) {
			if v.IsNil() {
				return nil, nil
			}
			return elem(v.Elem())
		}, nil
	case reflect.Slice:
		elem, err := textEncoderAt(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return func(v reflect.Value) ([]string, error) {
			out := make([]string, 0, v.Len())
			for i := range v.Len() {
				texts, err := elem(v.Index(i))
				if err != nil {
					return nil, &entryError{index: i + 1, err: err}
				}
				// The setter hands each entry exactly one text, so an entry
				// that is a nil pointer, or a list of other than one value,
				// has no way to arrive as it is.
				if len(texts) != 1 {
					return nil, &entryError{index: i + 1, err: errNotOneEntry}
				}
				out = append(out, texts[0])
			}
			return out, nil
		}, nil
	}
	// coverage: the binder's plan refuses every other kind first, with the
	// same wording, so this is only reached by a future change to one side.
	return nil, fmt.Errorf("type %s cannot be written as a request parameter", t)
}

// Errors the encoders report, which a call returns inside a [callRefusal].
var (
	errNotFinite   = errors.New("is not a finite number, and the server reads only decimal numbers")
	errNotOneEntry = errors.New("is a nil pointer, or a list of other than one value, which a request cannot carry as one entry")
)

// textMarshalerFor writes a type that reads itself with UnmarshalText through
// its MarshalText, with either receiver. A type with the one and not the
// other cannot be called with: the binder would read it, but nothing says how
// to write it.
func textMarshalerFor(t reflect.Type) (textEncoder, error) {
	marshal := func(m encoding.TextMarshaler) ([]string, error) {
		text, err := m.MarshalText()
		if err != nil {
			return nil, fmt.Errorf("could not be written as text: %w", err)
		}
		return []string{string(text)}, nil
	}
	switch {
	case t.Implements(textMarshaler):
		return func(v reflect.Value) ([]string, error) {
			return marshal(v.Interface().(encoding.TextMarshaler))
		}, nil
	case reflect.PointerTo(t).Implements(textMarshaler):
		return func(v reflect.Value) ([]string, error) {
			copied := reflect.New(t)
			copied.Elem().Set(v)
			return marshal(copied.Interface().(encoding.TextMarshaler))
		}, nil
	}
	return nil, fmt.Errorf("type %s reads itself from text with UnmarshalText but has no MarshalText to write itself with, so a call cannot send it; implement encoding.TextMarshaler on it", t)
}

// callConfig is what the [CallOption]s of one call declare.
type callConfig struct {
	headers  [][2]string
	validate bool
}

// encodedRequest is a value of the input written out, ready to become a
// request.
type encodedRequest struct {
	path        string
	query       string
	header      http.Header
	body        []byte
	contentType string
}

// fieldAt reads the field at index, which the binder's plan only ever places
// behind structs embedded by value, so there is no pointer to follow.
func fieldAt(v reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		v = v.Field(i)
	}
	return v
}

// encode writes v, an addressable value of the input type, as a request.
func (p *callPlan) encode(v reflect.Value, cfg *callConfig) (*encodedRequest, error) {
	if cfg.validate {
		if err := p.validateFirst(v); err != nil {
			return nil, err
		}
	}
	out := &encodedRequest{header: make(http.Header)}
	var err error
	if out.path, err = p.encodePath(v); err != nil {
		return nil, err
	}
	var query, cookies []string
	for i := range p.params {
		c := &p.params[i]
		if c.source == srcPath {
			continue
		}
		texts, err := c.encode(fieldAt(v, c.index))
		if err != nil {
			return nil, p.refuseParam(c, err)
		}
		if len(texts) == 0 {
			// Nothing is sent for a nil pointer or an empty list: a request
			// cannot spell an empty list, and the server reads both as
			// absent, as the default its tag declares if it has one.
			continue
		}
		switch c.source {
		case srcQuery:
			for _, text := range texts {
				query = append(query, url.QueryEscape(c.name)+"="+url.QueryEscape(text))
			}
		case srcHeader:
			value, err := p.headerValue(c, texts)
			if err != nil {
				return nil, err
			}
			out.header[c.name] = []string{value}
		default:
			// The binder reads the first cookie of a name, and a list from it
			// as a list of one, so a second entry would be lost.
			if len(texts) > 1 {
				return nil, refuse(p.method, p.path, "the cookie %q holds %d values, and a cookie carries one", c.name, len(texts))
			}
			cookie, err := p.cookieValue(c, texts[0])
			if err != nil {
				return nil, err
			}
			cookies = append(cookies, c.name+"="+cookie)
		}
	}
	out.query = strings.Join(query, "&")
	if _, set := out.header[userAgent]; p.headers[userAgent] && !set {
		// net/http writes a User-Agent of its own unless the header is
		// present and empty, which it then leaves out, so one the input left
		// out arrives absent, as it was sent.
		out.header[userAgent] = []string{""}
	}
	if len(cookies) > 0 {
		out.header.Set("Cookie", strings.Join(cookies, "; "))
	}
	if err := p.addHeaders(out.header, cfg.headers); err != nil {
		return nil, err
	}
	switch {
	case p.bind.multipart:
		out.body, out.contentType, err = p.encodeForm(v)
	case p.bind.body != nil:
		out.body, err = p.encodeBody(v)
		out.contentType = "application/json"
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// refuseParam reports a located field the encoder could not write.
func (p *callPlan) refuseParam(c *callParam, err error) error {
	var entry *entryError
	if errors.As(err, &entry) {
		return refuse(p.method, p.path, "entry %d of the %s parameter %q %v", entry.index, c.source, c.name, entry.err)
	}
	return refuse(p.method, p.path, "the %s parameter %q %v", c.source, c.name, err)
}

// encodePath writes the path, each parameter escaped so that the router
// reads back exactly the text written.
//
// A parameter that would change which route the path names is refused rather
// than escaped: an empty segment, which the router never matches and proxies
// merge, "." and "..", which clients and proxies resolve as steps through the
// path, and a "/" anywhere but in a trailing {name...} parameter. The router
// itself would keep an escaped "/" inside its segment, but proxies are not so
// careful: some decode it and route the request elsewhere, others refuse it,
// so a value that works against the application alone breaks behind one.
func (p *callPlan) encodePath(v reflect.Value) (string, error) {
	var b strings.Builder
	for _, seg := range p.segments {
		b.WriteByte('/')
		if seg.param == nil {
			b.WriteString(seg.static)
			continue
		}
		c := seg.param
		texts, err := c.encode(fieldAt(v, c.index))
		if err != nil {
			return "", p.refuseParam(c, err)
		}
		if len(texts) != 1 {
			return "", refuse(p.method, p.path, "the path parameter %q holds %d values, and a path segment carries exactly one: a nil pointer or an empty list has nothing to put there", c.name, len(texts))
		}
		text := texts[0]
		if !seg.rest {
			if why := pathSegmentProblem(text, false); why != "" {
				return "", refuse(p.method, p.path, "the path parameter %q %s", c.name, why)
			}
			b.WriteString(url.PathEscape(text))
			continue
		}
		if text == "" {
			continue
		}
		for i, part := range strings.Split(text, "/") {
			if why := pathSegmentProblem(part, true); why != "" {
				return "", refuse(p.method, p.path, "segment %d of the path parameter %q %s", i+1, c.name, why)
			}
			if i > 0 {
				b.WriteByte('/')
			}
			b.WriteString(url.PathEscape(part))
		}
	}
	return b.String(), nil
}

// pathSegmentProblem says why text cannot be one segment of a path, or returns
// the empty string when it can.
func pathSegmentProblem(text string, inRest bool) string {
	switch {
	case text == "" && inRest:
		return "is empty, from a leading, trailing or doubled \"/\", which proxies merge or drop"
	case text == "":
		return "is empty, and the router never matches an empty segment"
	case text == "." || text == "..":
		return "is a dot segment, which clients and proxies resolve as a step through the path"
	case strings.Contains(text, "/") && !inRest:
		return "holds a \"/\", which only a trailing {name...} parameter may hold: an escaped one is decoded or refused by some proxies"
	}
	return ""
}

// headerValueProblem says why text cannot be sent as a header value and read
// back unchanged, or returns the empty string when it can.
//
// A control character other than a tab is refused by net/http on both sides,
// and a line break would end the header and begin another. Spaces and tabs at
// either end are dropped by every HTTP parser, so a value carrying them would
// arrive shorter than it left. Bytes above ASCII arrive as they were sent.
func headerValueProblem(text string) string {
	for i := range len(text) {
		if c := text[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return "holds a control character such as a line break or a NUL, which could end the header and write another"
		}
	}
	if text != strings.Trim(text, " \t") {
		return "begins or ends with a space or a tab, which HTTP drops from a header value"
	}
	return ""
}

// userAgent is the header net/http writes a value of its own into.
const userAgent = "User-Agent"

// headerValue writes the texts of a header field as the one line its binder
// reads back.
//
// A list is written as one line, its entries separated by ", ", and checked
// by splitting it again with the binder's own [splitHeaderList], so that an
// entry holding a comma, an unbalanced quote or a space at either end, any of
// which would arrive as some other list, is refused rather than altered. One
// line rather than one line per entry, because a proxy may join repeated
// lines into one, and a list that survives being one line survives that too.
func (p *callPlan) headerValue(c *callParam, texts []string) (string, error) {
	for i, text := range texts {
		if why := headerValueProblem(text); why != "" {
			if c.isSlice {
				return "", refuse(p.method, p.path, "entry %d of the header %s %s", i+1, c.name, why)
			}
			return "", refuse(p.method, p.path, "the header %s %s", c.name, why)
		}
	}
	if c.name == userAgent && texts[0] == "" {
		return "", refuse(p.method, p.path, "the header User-Agent is empty, and net/http leaves an empty one out, so it would arrive absent")
	}
	if !c.isSlice {
		return texts[0], nil
	}
	line := strings.Join(texts, ", ")
	if got := splitHeaderList([]string{line}); !slices.Equal(got, texts) {
		return "", refuse(p.method, p.path, "the header %s is a list whose entries would not arrive as they are: "+
			"an entry is empty, or holds a comma or an unbalanced double quote, which the server reads as the edge of another entry", c.name)
	}
	return line, nil
}

// cookieValue writes the text of a cookie field as the server's cookie parser
// reads it back.
//
// The parser admits printable ASCII but for the double quote, the semicolon
// and the backslash, and strips one pair of double quotes from around a
// value. A value holding a space or a comma is quoted, as net/http quotes it,
// so that a space at either end survives the trimming of the cookie header.
// Anything else the parser would drop, and net/http would drop it on the way
// out with nothing but a log line, so it is refused.
func (p *callPlan) cookieValue(c *callParam, text string) (string, error) {
	for i := range len(text) {
		if b := text[i]; b < 0x20 || b >= 0x7f || b == '"' || b == ';' || b == '\\' {
			return "", refuse(p.method, p.path, "the cookie %q holds a character a cookie cannot carry: a control character, a double quote, a semicolon, a backslash or a byte outside ASCII", c.name)
		}
	}
	if strings.ContainsAny(text, " ,") {
		return `"` + text + `"`, nil
	}
	return text, nil
}

// addHeaders sets the headers a call added with [CallHeader], refusing one
// the input binds itself or a call has to write, and a value that would not
// arrive as it is.
func (p *callPlan) addHeaders(h http.Header, extra [][2]string) error {
	for _, pair := range extra {
		name := http.CanonicalHeaderKey(pair[0])
		switch {
		case !isHTTPToken(pair[0]):
			return refuse(p.method, p.path, "CallHeader was given %q, which is not a valid header name", clientShorten(pair[0]))
		case p.headers[name]:
			return refuse(p.method, p.path, "CallHeader was given %s, which the input binds itself; set the field instead", name)
		case name != "Cookie" && (headersNotBound[name] || name == "Content-Type"):
			return refuse(p.method, p.path, "CallHeader was given %s, which a call writes itself", name)
		}
		if why := headerValueProblem(pair[1]); why != "" {
			return refuse(p.method, p.path, "the value CallHeader was given for %s %s", name, why)
		}
		if name == "Cookie" && h.Get("Cookie") != "" {
			// After the input's own cookies, so that a cookie of the same
			// name is the second one the server sees and not the one it
			// reads, which is the first.
			h.Set("Cookie", h.Get("Cookie")+"; "+pair[1])
			continue
		}
		h.Add(name, pair[1])
	}
	return nil
}

// encodeBody writes the JSON body: the body members of v and nothing else.
//
// The binder decodes a body into a struct holding its members alone, built
// once from the input type, and copies them into the input; see
// [bodyPlan.narrow]. Writing goes the other way through the same struct, so a
// located field and a Dep are never sent, whatever their json tags say, and a
// member is named and encoded exactly as it is decoded. An input whose every
// field is a body member is written whole, as the binder reads it whole.
//
// So is an input that decodes itself, which the binder hands the body whole.
// Writing it whole put its located fields in the body beside the headers and
// cookies they were sent in, the credential in an Authorization field among
// them, where a body is logged and kept, and a Dep in it failed every call,
// since a Dep has no exported field to encode. One with no encoder of its own
// is written from the same struct as any other input, which its own fields
// encode as once those are left out. One that encodes itself is handed a copy
// with its located fields and Deps zeroed.
func (p *callPlan) encodeBody(v reflect.Value) ([]byte, error) {
	source := v
	switch {
	case p.bodyShape != v.Type():
		source = reflect.New(p.bodyShape).Elem()
		for _, c := range p.bodyCopies {
			fieldAt(source, c.from).Set(fieldAt(v, c.to))
		}
	case !p.bind.body.direct:
		source = reflect.New(v.Type()).Elem()
		source.Set(v)
		for i := range p.params {
			fieldAt(source, p.params[i].index).SetZero()
		}
		for _, dep := range p.bind.deps {
			fieldAt(source, dep.index).SetZero()
		}
	}
	data, err := json.Marshal(source.Addr().Interface(), durationJSON, json.Deterministic(true))
	if err != nil {
		return nil, refuse(p.method, p.path, "the body could not be encoded as JSON: %v", err)
	}
	if len(p.omitted) > 0 {
		if err := p.checkOmitted(v, data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// checkOmitted refuses a body whose json tags left out a member that has a
// default, which the server then fills in: a zero value the tag omits would
// arrive as the default. A nil pointer and an empty list are absent wherever
// they are in a request, and absent is what a default is for, so those are
// left out as they are. The body is the call's own encoding of a struct, an
// object, whose member names are read in one pass.
func (p *callPlan) checkOmitted(v reflect.Value, body []byte) error {
	present := map[string]bool{}
	dec := jsontext.NewDecoder(bytes.NewReader(body))
	if _, err := dec.ReadToken(); err == nil {
		for dec.PeekKind() == '"' {
			name, _ := dec.ReadToken()
			present[name.String()] = true
			_ = dec.SkipValue()
		}
	}
	for _, d := range p.omitted {
		if present[d.name] {
			continue
		}
		switch field := fieldAt(v, d.index); field.Kind() {
		case reflect.Pointer:
			if field.IsNil() {
				continue
			}
		case reflect.Slice, reflect.Map:
			if field.Len() == 0 {
				continue
			}
		}
		return refuse(p.method, p.path, "the body member %q would be left out by the omitzero or omitempty option of its json tag, and the server would read its default in its place; "+
			"send a value the tag keeps, or a nil pointer for the default", d.name)
	}
	return nil
}

// encodeForm writes a form body, always as multipart/form-data.
//
// The binder also reads a urlencoded body for a route without files, but
// net/http parses one only for POST, PUT and PATCH, while a multipart body is
// read whatever the method, so multipart is the one encoding every endpoint
// can be called with. A value is a part of its own and arrives byte for byte.
// A []byte file is sent as a part named after its field, with the field's
// name as its file name, and a nil one is not sent at all.
func (p *callPlan) encodeForm(v reflect.Value) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for i := range p.form {
		c := &p.form[i]
		texts, err := c.encode(fieldAt(v, c.index))
		if err != nil {
			return nil, "", p.refuseParam(c, err)
		}
		for _, text := range texts {
			// coverage: writing to a bytes.Buffer cannot fail.
			if err := w.WriteField(c.name, text); err != nil {
				return nil, "", err
			}
		}
	}
	for _, b := range p.files {
		field := fieldAt(v, b.index)
		var contents [][]byte
		if b.kind == fileBytes {
			if !field.IsNil() {
				contents = [][]byte{field.Bytes()}
			}
		} else {
			contents = field.Interface().([][]byte)
		}
		for _, content := range contents {
			part, err := w.CreateFormFile(b.name, b.name)
			if err == nil {
				_, err = part.Write(content)
			}
			// coverage: writing to a bytes.Buffer cannot fail.
			if err != nil {
				return nil, "", err
			}
		}
	}
	// coverage: closing a writer over a bytes.Buffer cannot fail.
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// validateFirst runs, before anything is sent, the checks the server runs
// after binding: a required parameter or file that has no value, and the
// rules of an input that implements [Validatable]. See [ValidateFirst].
func (p *callPlan) validateFirst(v reflect.Value) error {
	verr := &ValidationError{Model: snakeCase(p.bind.typ.Name())}
	failed := map[fieldKey]bool{}
	missing := func(location, name string) {
		verr.addKeyed(location, name, "is required", "blank")
		failed[fieldKey{location, name}] = true
	}
	for _, params := range [][]callParam{p.params, p.form} {
		for i := range params {
			c := &params[i]
			if !c.required || c.source == srcPath {
				continue
			}
			// A field that cannot be encoded is reported by the encoder,
			// with the reason, once validation has passed.
			if texts, err := c.encode(fieldAt(v, c.index)); err == nil && len(texts) == 0 {
				missing(c.source.String(), c.name)
			}
		}
	}
	for _, b := range p.files {
		// A nil []byte is not sent, and neither is a [][]byte with no file in
		// it; an empty file that is not nil is sent, as a file with nothing in
		// it.
		field := fieldAt(v, b.index)
		if b.required && ((b.kind == fileBytes && field.IsNil()) || (b.kind == fileBytesMany && field.Len() == 0)) {
			missing(srcFile.String(), b.name)
		}
	}
	if p.validate && p.bind.validation != nil {
		verr.Details = append(verr.Details, p.bind.runValidation(v, failed)...)
	}
	if len(verr.Details) > 0 {
		return verr
	}
	return nil
}
