package muzak

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// This file calls a tool: it reads the call's arguments into a value of the
// route's input type, writes that value as the request the binder reads back,
// with the encoder [Endpoint.Call] uses, and serves the request in-process
// through the whole application, recording the answer within a bound and
// turning it into a tool result.

// mcpTool is one route served as a tool.
type mcpTool struct {
	name  string
	route *Route
	// plan writes a value of the route's input as a request; see
	// endpoint_encode.go.
	plan *callPlan
	// groups are the members a call's arguments may have: the parts of the
	// request the route reads, by the name the input schema gives each.
	groups map[string]bool
	// accept is the Accept the request is sent with, versionHeader and
	// version the header that selects the route's version, when the
	// application chooses versions by header.
	accept, versionHeader, version string
	// bindsUserAgent and bindsLanguage record that the input reads one of the
	// headers a call otherwise copies from the MCP request.
	bindsUserAgent, bindsLanguage bool
	// listing is the tool's definition as each era's tools/list carries it,
	// and structured records whether that definition has an output schema, in
	// which case every successful result must carry structured content.
	listing    [mcpEras]jsontext.Value
	structured [mcpEras]bool
}

// compileTool compiles how a route is called as a tool. The input is held to
// what [Endpoint.Call] can write, since a tool call writes it the same way,
// and may not bind a header the call writes itself: the forwarded
// credentials, the headers a client address or scheme is read from, and the
// one that selects the route's version.
func (s *mcpServer) compileTool(rt *Route) (*mcpTool, []error) {
	p, errs := planCall(rt.plan, rt.outType, rt.Method, rt.Path)
	t := &mcpTool{name: mcpToolName(rt.OperationID), route: rt, plan: p, groups: map[string]bool{}}
	for i := range rt.plan.params {
		t.groups[rt.plan.params[i].source.String()] = true
		if b := &rt.plan.params[i]; b.source == srcCookie && slices.Contains(s.forwardCookies, b.name) {
			errs = append(errs, fmt.Errorf("muzak: %s %s: field %s binds the cookie %q, which MCPOptions.ForwardCookies forwards from the MCP request",
				rt.Method, rt.Path, rt.plan.typ.FieldByIndex(b.index).Name, b.name))
		}
	}
	if rt.plan.body != nil {
		t.groups["body"] = true
	}
	if rt.plan.multipart {
		t.groups["form"] = true
	}
	for _, name := range slices.Sorted(maps.Keys(p.headers)) {
		if name == "Authorization" || slices.Contains(s.forwardHeaders, name) || slices.Contains(s.forwarding, name) {
			errs = append(errs, fmt.Errorf("muzak: %s %s: the input binds the header %s, which a tool call forwards from the MCP request",
				rt.Method, rt.Path, name))
		}
	}
	errs = append(errs, t.selectVersion(s.app.opts.Versioning)...)
	if t.accept == "" && !p.headers["Accept"] && p.output == outputEncoded && !p.html && !p.empty {
		t.accept = "application/json"
	}
	t.bindsUserAgent, t.bindsLanguage = p.headers[userAgent], p.headers["Accept-Language"]
	for i, err := range errs {
		errs[i] = fmt.Errorf("%w; it is chosen as an MCP tool, and a tool call writes the input as a request as Endpoint.Call does, "+
			"so change the input or leave the route out of the MCP endpoint's tools", err)
	}
	return t, errs
}

// selectVersion arranges for a tool's request to declare the version its route
// answers, when the application chooses a route by a header rather than by its
// path: the version header, or a parameter of Accept. A route chosen by a
// custom extractor cannot be told what to read, and is refused.
func (t *mcpTool) selectVersion(v VersioningOptions) []error {
	rt := t.route
	if !v.enabled() || v.Type == VersioningURI || rt.isVersionNeutral() {
		return nil
	}
	version := string(rt.Versions[0])
	switch v.Type {
	case VersioningHeader:
		t.versionHeader, t.version = http.CanonicalHeaderKey(v.Header), version
		if t.plan.headers[t.versionHeader] {
			return []error{fmt.Errorf("muzak: %s %s: the input binds the header %s, which selects the route's version",
				rt.Method, rt.Path, t.versionHeader)}
		}
	case VersioningMediaType:
		key := strings.TrimSuffix(strings.TrimSpace(v.Key), "=")
		t.accept = mime.FormatMediaType("application/json", map[string]string{key: version})
		if t.accept == "" || t.plan.headers["Accept"] {
			return []error{fmt.Errorf("muzak: %s %s: the route's version is chosen by a parameter of Accept, which a tool call cannot write for it",
				rt.Method, rt.Path)}
		}
	default:
		return []error{fmt.Errorf("muzak: %s %s: the route's version is chosen by a custom extractor, which a tool call cannot satisfy",
			rt.Method, rt.Path)}
	}
	return nil
}

// mcpCallResult is a tools/call result.
type mcpCallResult struct {
	ResultType        string         `json:"resultType,omitzero"`
	Content           []any          `json:"content"`
	StructuredContent jsontext.Value `json:"structuredContent,omitzero"`
	IsError           bool           `json:"isError,omitzero"`
	Meta              *mcpResultMeta `json:"_meta,omitzero"`
}

// mcpTextContent is a text content item.
type mcpTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpMediaContent is an image or audio content item.
type mcpMediaContent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// newResult starts a result in the shape the era expects.
func (s *mcpServer) newResult(era mcpEra) *mcpCallResult {
	result := &mcpCallResult{Content: []any{}}
	if era == eraStateless {
		result.ResultType = "complete"
		result.Meta = &mcpResultMeta{ServerInfo: s.serverInfo()}
	}
	return result
}

// call calls a tool and returns its result. Nothing that goes wrong with the
// call is a protocol error: arguments that do not fit, a value no request can
// carry, a refusal by the route's security and a failure of its handler all
// come back as a result with isError set, carrying the error envelope the
// application renders, which is written for clients.
func (s *mcpServer) call(c *Context, t *mcpTool, args jsontext.Value, era mcpEra) *mcpCallResult {
	in, err := t.decode(args)
	if err != nil {
		return s.errorResult(c, era, err)
	}
	encoded, err := t.plan.encode(in, &callConfig{})
	if err != nil {
		return s.errorResult(c, era, mcpRefusal(t, err))
	}
	return s.serveTool(c, t, encoded, era)
}

// mcpRefusal turns the encoder's refusal of a value no request can carry into
// the 422 a tool's caller is answered with. The refusal names the field and
// the reason and never the value, so it is told, without the method and path
// it opens with, which name the route rather than the tool. It is the
// caller's mistake, and like any 422 is not logged.
func mcpRefusal(t *mcpTool, err error) error {
	prefix := fmt.Sprintf("muzak: %s %s: ", t.plan.method, t.plan.path)
	return NewHTTPError(http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), prefix))
}

// maxMCPArgumentIssues bounds the problems one call's arguments are answered
// with, since a client chooses how many members it sends.
const maxMCPArgumentIssues = 32

// decode reads a call's arguments into a value of the route's input type, by
// the binder's own rules for each part of the request: a parameter is turned
// into the text a request would carry and written by the binder's setter,
// with its default when it is left out and a failure when it is required, and
// the body is decoded under the route's own JSON options, its defaults
// written first. So the value is the one the binder would make of the
// request the arguments describe, and writing it as a request loses nothing.
//
// What the arguments get wrong is answered as the binder answers a request,
// with a *ValidationError, except that a member the input schema does not
// list is refused rather than ignored. The input's own rules are not run
// here: the route runs them once its guards have admitted the call.
func (t *mcpTool) decode(args jsontext.Value) (reflect.Value, error) {
	bind := t.plan.bind
	v := reflect.New(bind.typ).Elem()
	verr := &ValidationError{Model: snakeCase(bind.typ.Name())}
	groups := map[string]jsontext.Value{}
	if len(args) > 0 && args.Kind() != 'n' {
		// The message the arguments came in was valid JSON, so an object of
		// them always decodes.
		_ = json.Unmarshal(args, &groups)
	}
	located := map[string]map[string]jsontext.Value{}
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		raw := groups[name]
		switch {
		case !t.groups[name]:
			verr.add("arguments", truncateForMessage(name), "is not a part of the request this tool sends")
		case name == "body" || raw.Kind() == 'n':
		case raw.Kind() != '{':
			verr.add(name, "", "must be an object")
		default:
			members := map[string]jsontext.Value{}
			_ = json.Unmarshal(raw, &members)
			located[name] = members
		}
	}
	used := map[string]map[string]bool{}
	take := func(group, name string) (jsontext.Value, bool) {
		raw, present := located[group][name]
		if used[group] == nil {
			used[group] = map[string]bool{}
		}
		used[group][name] = true
		return raw, present && raw.Kind() != 'n'
	}
	for i := range bind.params {
		b := &bind.params[i]
		raw, present := take(b.source.String(), b.name)
		bindArgument(b, b.source.String(), raw, present, v, verr)
	}
	for i := range bind.form {
		b := &bind.form[i]
		raw, present := take("form", b.name)
		bindArgument(b, "form", raw, present, v, verr)
	}
	for i := range bind.files {
		f := &bind.files[i]
		raw, present := take("form", f.name)
		bindFileArgument(f, raw, present, v, verr)
	}
	for _, group := range slices.Sorted(maps.Keys(located)) {
		for _, name := range slices.Sorted(maps.Keys(located[group])) {
			if !used[group][name] {
				verr.add(group, truncateForMessage(name), "is not a parameter of this operation")
			}
		}
	}
	if err := t.decodeBody(groups["body"], v, verr); err != nil {
		return v, err
	}
	if len(verr.Details) > maxMCPArgumentIssues {
		verr.Details = verr.Details[:maxMCPArgumentIssues]
	}
	if len(verr.Details) > 0 {
		return v, verr
	}
	return v, nil
}

// decodeBody decodes the body argument as the binder decodes a body; see
// [bindPlan.bindBody]. A body left out, or null, is missing.
func (t *mcpTool) decodeBody(raw jsontext.Value, v reflect.Value, verr *ValidationError) error {
	body := t.plan.bind.body
	if body == nil {
		return nil
	}
	if raw == nil || raw.Kind() == 'n' {
		if body.required {
			verr.add("body", "", "is required")
		}
		return nil
	}
	target := v
	if !body.direct {
		target = reflect.New(body.shape).Elem()
	}
	for i := range body.defaults {
		d := &body.defaults[i]
		_ = d.set(fieldByIndex(target, d.at), []string{d.raw})
	}
	if err := json.Unmarshal(raw, target.Addr().Interface(), t.route.jsonReadOptions()); err != nil {
		if typeFault(err) {
			return fmt.Errorf("muzak: %s %s: the arguments of the MCP tool %q could not be decoded into %s, through no fault of the call: %w",
				t.route.Method, t.route.Path, t.name, t.plan.bind.typ, err)
		}
		verr.Details = append(verr.Details, decodeIssue(err, target.Type()))
		return nil
	}
	for _, c := range body.copies {
		fieldByIndex(v, c.to).Set(fieldByIndex(target, c.from))
	}
	return nil
}

// bindArgument writes one parameter's argument into its field, as
// [bindParams] writes a parameter a request carries.
func bindArgument(b *paramBinder, location string, raw jsontext.Value, present bool, v reflect.Value, verr *ValidationError) {
	var texts []string
	if present {
		var err error
		if texts, err = argumentTexts(raw, b.isSlice); err != nil {
			verr.add(location, b.name, err.Error())
			return
		}
	}
	if len(texts) == 0 {
		switch {
		case b.required:
			verr.addKeyed(location, b.name, "is required", "blank")
			return
		case !b.hasDef:
			return
		}
		texts = []string{b.defValue}
	}
	if err := b.set(fieldByIndex(v, b.index), texts); err != nil {
		issue, key, args := paramIssue(err)
		verr.addKey(location, b.name, issue, key, args...)
	}
}

// Why an argument is not a parameter's value.
var (
	errArgumentNotScalar = errors.New("must be a string, a number or a boolean")
	errArgumentNotOne    = errors.New("must be a single value, not a list")
)

// argumentTexts turns a parameter's argument into the texts a request would
// carry for it: a string as it is, a number as it is written, a boolean as
// true or false, and a list, for a parameter that takes one, entry by entry.
// An empty list is no value, as a request cannot send one.
func argumentTexts(raw jsontext.Value, list bool) ([]string, error) {
	if raw.Kind() != '[' {
		text, ok := scalarText(raw)
		if !ok {
			return nil, errArgumentNotScalar
		}
		return []string{text}, nil
	}
	if !list {
		return nil, errArgumentNotOne
	}
	var entries []jsontext.Value
	_ = json.Unmarshal(raw, &entries)
	texts := make([]string, 0, len(entries))
	for i, entry := range entries {
		text, ok := scalarText(entry)
		if !ok {
			return nil, &entryError{index: i + 1, err: errArgumentNotScalar}
		}
		texts = append(texts, text)
	}
	return texts, nil
}

// scalarText returns the text a scalar JSON value stands for.
func scalarText(raw jsontext.Value) (string, bool) {
	switch raw.Kind() {
	case '"':
		return rawString(raw)
	case '0':
		return string(raw), true
	case 't':
		return "true", true
	case 'f':
		return "false", true
	}
	return "", false
}

// bindFileArgument writes a file field's argument, standard base64 for one
// file, or a list of them for a field that takes several.
func bindFileArgument(f *fileBinder, raw jsontext.Value, present bool, v reflect.Value, verr *ValidationError) {
	if !present {
		if f.required {
			verr.addKeyed("form", f.name, "is required", "blank")
		}
		return
	}
	var encoded []jsontext.Value
	switch {
	case raw.Kind() == '"':
		encoded = []jsontext.Value{raw}
	case raw.Kind() == '[' && f.kind == fileBytesMany:
		_ = json.Unmarshal(raw, &encoded)
	default:
		verr.add("form", f.name, "must be a file's contents in standard base64")
		return
	}
	files := make([][]byte, 0, len(encoded))
	for _, entry := range encoded {
		text, ok := rawString(entry)
		data, err := base64.StdEncoding.DecodeString(text)
		if !ok || err != nil {
			verr.add("form", f.name, "must be a file's contents in standard base64")
			return
		}
		files = append(files, data)
	}
	field := fieldByIndex(v, f.index)
	if f.kind == fileBytes {
		field.SetBytes(files[0])
		return
	}
	field.Set(reflect.ValueOf(files))
}

// subrequestKey carries a tool call's [subrequest] in the context of the
// request it makes.
type subrequestKey struct{}

// subrequest is what a tool call's request carries to the application serving
// it: the identifier and span of the MCP request, which it is part of, and the
// one route it may reach.
type subrequest struct {
	requestID string
	parent    SpanContext
	route     *Route
	// reached is set when dispatch runs the route, and read once the
	// application has answered. Atomic, because a middleware may serve the
	// rest of the chain on a goroutine of its own, as http.TimeoutHandler
	// does, and return before it ends.
	reached atomic.Bool
}

// subrequestOf returns the subrequest a context carries, or nil.
func subrequestOf(ctx context.Context) *subrequest {
	sub, _ := ctx.Value(subrequestKey{}).(*subrequest)
	return sub
}

// inheritedRequestID is what [RequestID] gives a tool call's request instead
// of a fresh identifier: the MCP request's, so that the two requests' log
// lines, and the request_id of an error envelope the tool returns, are one.
// An application without an MCP endpoint pays a comparison.
func (a *App) inheritedRequestID(r *http.Request) string {
	if a.mcp == nil {
		return ""
	}
	if sub := subrequestOf(r.Context()); sub != nil {
		return sub.requestID
	}
	return ""
}

// subrequestParent is the parent a tool call's server span is started under:
// the span of the MCP request, whatever the [TraceParentPolicy] says, since
// the trace was not chosen by a client but continued by this server.
func (a *App) subrequestParent(r *http.Request) (SpanContext, bool) {
	sub := subrequestOf(r.Context())
	if sub == nil || !sub.parent.IsValid() {
		return SpanContext{}, false
	}
	return sub.parent, true
}

// dispatchSubrequest runs a tool call's request, which may reach its tool's
// route and nothing else. A path parameter can spell the path of a static
// route beside it, and a wildcard the path of any route, so the request is
// routed as any other and refused unless routing lands on the tool's own
// route, before anything of the other runs.
func (a *App) dispatchSubrequest(c *Context, sub *subrequest, entry *pathEntry, found bool) {
	if found {
		if candidates, ok := entry.methods[c.r.Method]; ok && a.matchVersion(c, candidates) == sub.route {
			sub.reached.Store(true)
			a.run(c, sub.route)
			return
		}
	}
	a.fail(c, errMCPStray())
}

// errMCPStray refuses a tool call whose arguments name another route.
func errMCPStray() *HTTPError {
	return NewHTTPError(http.StatusUnprocessableEntity,
		"the arguments name a path this tool's operation does not answer")
}

// detachedContext is the context of a tool call's request: the MCP request's
// cancellation and deadline, and none of its values. A value the MCP request
// gathered on its way in, from its route holder to whatever middleware put
// there, belongs to that request, and the call's request gathers its own.
type detachedContext struct{ parent context.Context }

func (d detachedContext) Deadline() (time.Time, bool) { return d.parent.Deadline() }
func (d detachedContext) Done() <-chan struct{}       { return d.parent.Done() }
func (d detachedContext) Err() error                  { return d.parent.Err() }
func (d detachedContext) Value(any) any               { return nil }

// AfterFunc lets context.WithCancel follow the parent's cancellation without
// a goroutine of its own.
func (d detachedContext) AfterFunc(f func()) func() bool { return context.AfterFunc(d.parent, f) }

// serveTool sends a tool call's request through the application and turns its
// answer into a result.
func (s *mcpServer) serveTool(c *Context, t *mcpTool, encoded *encodedRequest, era mcpEra) *mcpCallResult {
	ctx, cancel := context.WithCancel(detachedContext{parent: c.Context()})
	defer cancel()
	sub := &subrequest{requestID: c.RequestID(), route: t.route}
	if sc, traced := SpanContextFromContext(c.Context()); traced {
		sub.parent = sc
	}
	req, err := s.innerRequest(context.WithValue(ctx, subrequestKey{}, sub), c.r, t, encoded)
	if err != nil {
		// coverage: the method is the route's own and the target was written
		// by the encoder, which escapes every part of it, so there is nothing
		// left for net/http to refuse.
		return s.errorResult(c, era, err)
	}
	rec := &mcpRecorder{header: http.Header{}, limit: s.opts.MaxResultSize, cancel: cancel}
	aborted := s.serveInner(rec, req)
	return s.toolResult(c, t, rec, sub.reached.Load(), aborted, era)
}

// innerRequest builds a tool call's request: what the encoder wrote, and from
// the MCP request only what has to carry over.
//
// That is the client: its address, its connection's TLS state and protocol,
// its Host, and the headers a trusted proxy names the client and its scheme
// in, so that the call is attributed, rate limited and judged by the host
// allowlist and the HTTPS redirect exactly as the MCP request was, and a
// client can claim no more for one than for the other. It is the credentials:
// Authorization, and the headers and cookies [MCPOptions] forwards, unchanged.
// And it is the User-Agent and Accept-Language, when the input does not bind
// them itself. Nothing else is copied: no Origin, no session, no other cookie.
func (s *mcpServer) innerRequest(ctx context.Context, outer *http.Request, t *mcpTool, encoded *encodedRequest) (*http.Request, error) {
	target := encoded.path
	if encoded.query != "" {
		target += "?" + encoded.query
	}
	var body io.Reader
	if encoded.body != nil {
		body = bytes.NewReader(encoded.body)
	}
	req, err := http.NewRequestWithContext(ctx, t.route.Method, target, body)
	if err != nil {
		// coverage: see [mcpServer.serveTool], which reports it.
		return nil, err
	}
	if req.Body == nil {
		// A server's request always has a body to read, if an empty one.
		req.Body = http.NoBody
	}
	req.Header = encoded.header
	if encoded.contentType != "" {
		req.Header.Set("Content-Type", encoded.contentType)
	}
	if t.accept != "" {
		req.Header.Set("Accept", t.accept)
	}
	if t.versionHeader != "" {
		req.Header.Set(t.versionHeader, t.version)
	}
	copied := []string{"Authorization"}
	copied = append(copied, s.forwardHeaders...)
	copied = append(copied, s.forwarding...)
	if t.bindsUserAgent {
		if values := req.Header[userAgent]; len(values) == 1 && values[0] == "" {
			// The encoder marks a User-Agent left out for net/http's client,
			// which would write one of its own; this request is not sent.
			delete(req.Header, userAgent)
		}
	} else {
		copied = append(copied, userAgent)
	}
	if !t.bindsLanguage {
		copied = append(copied, "Accept-Language")
	}
	for _, name := range copied {
		if values := outer.Header.Values(name); len(values) > 0 {
			req.Header[name] = slices.Clone(values)
		}
	}
	var cookies []string
	for _, name := range s.forwardCookies {
		if cookie, err := outer.Cookie(name); err == nil {
			// The value is one net/http read from a Cookie header, so it holds
			// only bytes a cookie may; a space or a comma is quoted, as
			// net/http quotes one, so that neither is taken for a separator.
			value := cookie.Value
			if strings.ContainsAny(value, " ,") {
				value = `"` + value + `"`
			}
			cookies = append(cookies, cookie.Name+"="+value)
		}
	}
	if len(cookies) > 0 {
		if existing := req.Header.Get("Cookie"); existing != "" {
			cookies = append([]string{existing}, cookies...)
		}
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
	}
	req.Host = outer.Host
	req.RemoteAddr = outer.RemoteAddr
	req.TLS = outer.TLS
	req.Proto, req.ProtoMajor, req.ProtoMinor = outer.Proto, outer.ProtoMajor, outer.ProtoMinor
	req.RequestURI = target
	return req, nil
}

// serveInner serves a tool call's request through the application's whole
// handler, reporting whether its response was aborted.
//
// The application's recovery turns every panic below it into a 500 before it
// gets here. What does arrive is http.ErrAbortHandler, with which the
// framework ends a response that failed after it had started, and which
// would otherwise end the MCP request too.
func (s *mcpServer) serveInner(w http.ResponseWriter, r *http.Request) (aborted bool) {
	defer func() {
		if recover() != nil {
			aborted = true
		}
	}()
	s.app.handler.ServeHTTP(w, r)
	return false
}

// errMCPResultTooLarge is what a write past the result limit fails with.
var errMCPResultTooLarge = errors.New("muzak: the response is larger than an MCP tool result may be")

// mcpRecorder records a tool call's response within the result limit. The
// first write past it fails, and cancels the request's context, so a handler
// that writes a stream stops instead of producing what nobody will read.
type mcpRecorder struct {
	header http.Header
	status int
	// contentType and location are the header as it stood when the status
	// was written, which is what a client would have received.
	contentType, location string
	body                  bytes.Buffer
	limit                 int64
	overflow              bool
	cancel                context.CancelFunc
}

func (r *mcpRecorder) Header() http.Header { return r.header }

// WriteHeader records the status, ignoring an interim one and a repeat, as
// net/http does.
func (r *mcpRecorder) WriteHeader(status int) {
	if r.status != 0 || isInformational(status) {
		return
	}
	r.status = status
	r.contentType = r.header.Get("Content-Type")
	r.location = r.header.Get("Location")
}

// Write records the body, up to the limit.
func (r *mcpRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if r.overflow || int64(r.body.Len())+int64(len(p)) > r.limit {
		r.overflow = true
		r.cancel()
		return 0, errMCPResultTooLarge
	}
	return r.body.Write(p)
}

// FlushError commits the response as a flush would, and has nothing to send.
func (r *mcpRecorder) FlushError() error {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return nil
}

// toolResult turns a tool call's response into its result.
//
// A success is the body: JSON as text, and as structured content where the
// era allows it, which is an object before 2026-07-28 and any value after;
// text as text; an image or audio as itself, in base64. A redirect is the
// place it redirects to, which is not followed. A failure, any status from
// 400, is the error envelope the application answered with, as text, with
// isError set. A response that is too large, that was aborted, that has a
// media type a result cannot carry, or that a route other than the tool's
// produced, is an error result saying so.
func (s *mcpServer) toolResult(c *Context, t *mcpTool, rec *mcpRecorder, reached, aborted bool, era mcpEra) *mcpCallResult {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	body := rec.body.Bytes()
	contentType := rec.contentType
	if contentType == "" && len(body) > 0 {
		// What net/http would have labelled it with on the wire.
		contentType = http.DetectContentType(body)
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case rec.overflow:
		return s.errorResult(c, era, NewHTTPErrorf(http.StatusBadGateway,
			"the operation answered with more than the %d bytes a tool result may hold", s.opts.MaxResultSize))
	case aborted:
		return s.errorResult(c, era, NewHTTPError(http.StatusBadGateway,
			"the operation failed after its answer had started, so the answer is incomplete"))
	case !reached && status < http.StatusBadRequest:
		// Something answered before routing did, the documentation or a
		// health probe, for a path a wildcard argument spelled.
		return s.errorResult(c, era, errMCPStray())
	case status >= http.StatusBadRequest:
		return s.failureResult(c, rec, status, mediaType, era)
	case status >= http.StatusMultipleChoices:
		result := s.newResult(era)
		if rec.location != "" {
			result.Content = append(result.Content, mcpTextContent{Type: "text", Text: strings.ToValidUTF8(rec.location, "\uFFFD")})
		}
		return result
	}
	result := s.newResult(era)
	switch {
	case len(body) == 0:
	case isJSONMediaType(mediaType):
		value := jsontext.Value(body)
		if !value.IsValid() {
			return s.errorResult(c, era, NewHTTPError(http.StatusBadGateway,
				"the operation answered with a body labelled JSON that is not valid JSON"))
		}
		result.Content = append(result.Content, mcpTextContent{Type: "text", Text: string(body)})
		if era == eraStateless || (era == eraStructured && value.Kind() == '{') {
			result.StructuredContent = bytes.Clone(body)
		}
	case strings.HasPrefix(mediaType, "text/"):
		result.Content = append(result.Content, mcpTextContent{Type: "text", Text: strings.ToValidUTF8(string(body), "\uFFFD")})
	case strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"):
		kind, _, _ := strings.Cut(mediaType, "/")
		result.Content = append(result.Content, mcpMediaContent{Type: kind, Data: base64.StdEncoding.EncodeToString(body), MimeType: mediaType})
	default:
		return s.errorResult(c, era, NewHTTPErrorf(http.StatusBadGateway,
			"the operation answered with %s, which a tool result cannot carry", describeMediaType(mediaType)))
	}
	if t.structured[era] && result.StructuredContent == nil {
		s.app.logger.WarnContext(c.Context(), "muzak: an MCP tool answered with a body its output schema does not describe",
			slog.String("tool", t.name), slog.String(RequestIDKey, c.RequestID()), slog.Int("status", status))
		return s.errorResult(c, era, NewHTTPError(http.StatusBadGateway,
			"the operation answered with a body its output schema does not describe"))
	}
	return result
}

// failureResult is the result of a response that failed: its body, the
// application's error envelope, which is written for clients, when it is JSON
// or text, and otherwise an envelope of the status alone.
func (s *mcpServer) failureResult(c *Context, rec *mcpRecorder, status int, mediaType string, era mcpEra) *mcpCallResult {
	body := rec.body.Bytes()
	var text string
	switch {
	case isJSONMediaType(mediaType) && jsontext.Value(body).IsValid():
		text = string(body)
	case strings.HasPrefix(mediaType, "text/") && len(body) > 0:
		text = strings.ToValidUTF8(string(body), "\uFFFD")
	default:
		return s.errorResult(c, era, NewHTTPErrorf(clampStatus(status), "the operation failed with status %d", status))
	}
	result := s.newResult(era)
	result.IsError = true
	result.Content = append(result.Content, mcpTextContent{Type: "text", Text: text})
	return result
}

// errorResult is the result of a call that failed before or after its route
// answered, rendered by the application's error renderer as the route's own
// failure would be. A failure the client is not told the cause of is logged.
func (s *mcpServer) errorResult(c *Context, era mcpEra, err error) *mcpCallResult {
	if cause := logCause(err); cause != nil {
		s.app.logger.ErrorContext(c.Context(), "muzak: an MCP tool call failed",
			slog.String(RequestIDKey, c.RequestID()), slog.String("error", cause.Error()))
	}
	result := s.newResult(era)
	result.IsError = true
	result.Content = append(result.Content, mcpTextContent{Type: "text", Text: s.renderError(c, err)})
	return result
}

// renderError renders an error as the application's error renderer does,
// into the text of a result. The renderer is given a Context of its own,
// whose response nothing is written to, so that a header it sets, a problem's
// Content-Type say, does not reach the MCP response.
func (s *mcpServer) renderError(c *Context, err error) string {
	scratch := &Context{
		w:         asResponseWriter(&mcpRecorder{header: http.Header{}}),
		r:         c.r,
		app:       s.app,
		logger:    c.logger,
		status:    http.StatusOK,
		requestID: c.requestID,
		locale:    c.locale,
		i18n:      c.i18n,
	}
	status, body := s.app.renderError(scratch, err)
	if body != nil {
		if data, merr := json.Marshal(body, durationJSON); merr == nil {
			return string(data)
		}
	}
	return http.StatusText(clampStatus(status))
}

// isJSONMediaType reports whether a media type is JSON.
func isJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// describeMediaType names a media type in a message, bounded, since a handler
// writes it.
func describeMediaType(mediaType string) string {
	if mediaType == "" {
		return "a body of no known type"
	}
	return fmt.Sprintf("%q", wsShorten(mediaType))
}
