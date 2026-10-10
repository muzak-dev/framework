package muzak

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file is the Streamable HTTP transport and the JSON-RPC methods behind
// it. A POST carries one message; what kind of client sent it decides how it
// is answered:
//
//   - initialize opens a session under 2025-03-26, 2025-06-18 or 2025-11-25,
//     whose identifier every later message of that client carries in
//     Mcp-Session-Id;
//   - a message carrying a session is answered under the revision its session
//     negotiated;
//   - a message carrying 2026-07-28's per-request metadata, or naming a
//     revision in MCP-Protocol-Version that no session is needed for or that
//     is not supported, is answered statelessly, as 2026-07-28 has it;
//   - anything else needed a session and is refused with 400.

// servePost answers one POSTed message.
//
// The transport is checked before the message is read: the Origin first,
// since a refused page should learn nothing more, then the media types. The
// message is read under the endpoint's [MaxBodySize].
func (s *mcpServer) servePost(c *Context, _ Empty) (Empty, error) {
	if err := s.checkOrigin(c.r); err != nil {
		return Empty{}, err
	}
	if err := checkMCPContentType(c.r); err != nil {
		return Empty{}, err
	}
	if !acceptsJSON(c.r.Header.Values("Accept")) {
		return Empty{}, NewHTTPError(http.StatusNotAcceptable,
			"an MCP client must accept application/json, which is how this endpoint answers")
	}
	sessionID, err := singleHeader(c.r.Header, HeaderMCPSessionID)
	if err != nil {
		return Empty{}, err
	}
	version, err := singleHeader(c.r.Header, HeaderMCPProtocolVersion)
	if err != nil {
		return Empty{}, err
	}
	body, err := readMCPMessage(c)
	if err != nil {
		return Empty{}, err
	}
	x := &mcpExchange{s: s, c: c, sessionID: sessionID, version: version}
	return Empty{}, x.serve(body)
}

// serveDelete ends the session the request names, as a client that is done
// with it is asked to.
func (s *mcpServer) serveDelete(c *Context, _ Empty) (Empty, error) {
	if err := s.checkOrigin(c.r); err != nil {
		return Empty{}, err
	}
	id, err := singleHeader(c.r.Header, HeaderMCPSessionID)
	if err != nil {
		return Empty{}, err
	}
	if id == "" {
		return Empty{}, errMCPNoSession()
	}
	session, ok := s.sessions.lookup(id)
	if !ok || !session.ownedBy(mcpOwner(c)) {
		return Empty{}, errMCPUnknownSession()
	}
	s.sessions.remove(id)
	if err := c.settle(nil); err != nil {
		return Empty{}, err
	}
	c.w.WriteHeader(http.StatusNoContent)
	return Empty{}, nil
}

// errMCPNoSession refuses a legacy message that carries no session.
func errMCPNoSession() *HTTPError {
	return NewHTTPError(http.StatusBadRequest,
		"this request needs the Mcp-Session-Id header the initialize response issued; send initialize first")
}

// errMCPUnknownSession refuses a session that does not exist, which is what
// the protocol tells a client to answer by starting another.
func errMCPUnknownSession() *HTTPError {
	return NewHTTPError(http.StatusNotFound,
		"the MCP session does not exist or has expired; start a new one with initialize")
}

// checkOrigin applies the endpoint's Origin policy; see
// [MCPOptions.AllowedOrigins].
func (s *mcpServer) checkOrigin(r *http.Request) error {
	values := r.Header.Values("Origin")
	if len(values) > 1 {
		return NewHTTPError(http.StatusBadRequest, "the Origin header was sent more than once")
	}
	if len(values) == 0 || values[0] == "" {
		// Only a browser sends an Origin, and only a page in a browser is the
		// DNS rebinding attack the check exists to stop.
		return nil
	}
	origin := values[0]
	for _, allowed := range s.opts.AllowedOrigins {
		if strings.EqualFold(allowed, origin) {
			return nil
		}
	}
	if host, ok := wsOriginHost(origin); ok && strings.EqualFold(host, r.Host) &&
		(len(s.app.opts.AllowedHosts) > 0 || localHost(r.Host)) {
		return nil
	}
	if s.opts.AllowOriginFunc != nil && s.opts.AllowOriginFunc(r, origin) {
		return nil
	}
	return NewHTTPErrorf(http.StatusForbidden, "the origin %q may not use this MCP endpoint", wsShorten(origin))
}

// checkMCPContentType refuses a message that is not labelled JSON in UTF-8,
// the only encoding JSON-RPC messages of MCP may have.
func checkMCPContentType(r *http.Request) error {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return NewHTTPError(http.StatusUnsupportedMediaType, "an MCP message is sent as application/json")
	}
	if charset, named := params["charset"]; named && !strings.EqualFold(charset, "utf-8") {
		return NewHTTPError(http.StatusUnsupportedMediaType, "an MCP message is encoded as UTF-8")
	}
	return nil
}

// acceptsJSON reports whether an Accept header admits application/json, by
// the most specific range that covers it, as RFC 9110 section 12.5.1 reads
// one: "application/json;q=0" refuses it even beside "*/*". A header that is
// absent admits nothing here, since the transport requires one.
func acceptsJSON(lines []string) bool {
	best, quality := -1, 0.0
	for _, line := range lines {
		for part := range strings.SplitSeq(line, ",") {
			mediaType, params, err := mime.ParseMediaType(part)
			if err != nil {
				continue
			}
			specificity := slices.Index([]string{"*/*", "application/*", "application/json"}, mediaType)
			if specificity < 0 || specificity < best {
				continue
			}
			q := 1.0
			if raw, given := params["q"]; given {
				if q, err = strconv.ParseFloat(raw, 64); err != nil || q < 0 || q > 1 {
					continue
				}
			}
			if specificity > best || q > quality {
				best, quality = specificity, q
			}
		}
	}
	return quality > 0
}

// singleHeader returns a header that may be sent at most once.
func singleHeader(h http.Header, name string) (string, error) {
	values := h.Values(name)
	if len(values) > 1 {
		return "", NewHTTPErrorf(http.StatusBadRequest, "the %s header was sent more than once", name)
	}
	if len(values) == 0 {
		return "", nil
	}
	return values[0], nil
}

// readMCPMessage reads the body of a POST under the endpoint's limit. An
// application that removed the limit still has one here, since every message
// is held in memory whole: [DefaultMaxBodySize].
func readMCPMessage(c *Context) ([]byte, error) {
	limit := mcpMessageLimit(c.route)
	tooLarge := func() *HTTPError {
		return NewHTTPErrorf(http.StatusRequestEntityTooLarge, "the MCP message exceeds the %d byte limit of this endpoint", limit)
	}
	if declaredOverLimit(c.r, limit) {
		return nil, tooLarge()
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, limit))
	if err != nil {
		var over *http.MaxBytesError
		if errors.As(err, &over) {
			return nil, tooLarge().Wrap(err)
		}
		return nil, NewHTTPError(http.StatusBadRequest, "the MCP message could not be read").Wrap(err)
	}
	return data, nil
}

// mcpMessageLimit is the largest message the endpoint served by rt reads.
func mcpMessageLimit(rt *Route) int64 {
	if rt.maxBodySize > 0 {
		return rt.maxBodySize
	}
	return DefaultMaxBodySize
}

// mcpExchange is one message being answered.
type mcpExchange struct {
	s         *mcpServer
	c         *Context
	sessionID string
	version   string
	msg       *rpcMessage
	era       mcpEra
	// stateless is set for a message answered under 2026-07-28, whose errors
	// carry their own HTTP statuses and leave out an id that is not known.
	stateless bool
}

// mcpMethod answers one request.
type mcpMethod func(x *mcpExchange) error

// The methods each kind of client may call; anything else is -32601.
var (
	mcpSessionMethods = map[string]mcpMethod{
		"ping":       (*mcpExchange).ping,
		"tools/list": (*mcpExchange).toolsList,
		"tools/call": (*mcpExchange).toolsCall,
	}
	mcpStatelessMethods = map[string]mcpMethod{
		"server/discover": (*mcpExchange).discover,
		"tools/list":      (*mcpExchange).toolsList,
		"tools/call":      (*mcpExchange).toolsCall,
	}
)

// serve reads the message and answers it under the right revision.
//
// A panic while it is answered, in the error renderer a tool's failure is
// rendered by say, is answered with -32603 and nothing of the panic, which is
// logged, so that a client is told in its own protocol that the server
// failed. The answer is the last thing an exchange writes, so a panic always
// comes before it; and a tool call's own request, where http.ErrAbortHandler
// could come from, recovers on its own; see [mcpServer.serveInner].
func (x *mcpExchange) serve(body []byte) (err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		x.c.logger.ErrorContext(x.c.Context(), "muzak: recovered from a panic while answering an MCP message",
			slog.String("panic", panicValue(recovered)),
			slog.String(RequestIDKey, x.c.RequestID()),
			slog.String("stack", string(debug.Stack())))
		if !x.c.w.written {
			err = x.rpcFail(http.StatusInternalServerError, &rpcError{Code: rpcInternalError, Message: "Internal error"})
		}
	}()
	x.stateless = isStatelessVersion(x.version)
	msg, rerr := parseRPCMessage(body)
	x.msg = msg
	if rerr != nil && (msg == nil || msg.kind != rpcRequest) {
		// A message that cannot be read, or a notification whose params are
		// not an object, has no request to answer, and is refused as a whole.
		return x.rpcFail(http.StatusBadRequest, rerr)
	}
	switch {
	case msg.kind == rpcRequest && msg.method == "initialize":
		if x.sessionID != "" {
			return NewHTTPError(http.StatusBadRequest,
				"an initialize request opens a session, so it cannot carry an Mcp-Session-Id header")
		}
		x.stateless = false
		return x.initialize(rerr)
	case x.sessionID != "":
		x.stateless = false
		return x.serveSession(rerr)
	case x.stateless || hasStatelessMeta(msg.params) || (x.version != "" && !slices.Contains(mcpVersions, x.version)):
		x.stateless = true
		return x.serveStateless(rerr)
	}
	return errMCPNoSession()
}

// isStatelessVersion reports whether a revision is one without sessions.
func isStatelessVersion(version string) bool { return version == mcpVersion20260728 }

// isLegacyVersion reports whether a revision opens a session with initialize.
func isLegacyVersion(version string) bool {
	return slices.Contains(mcpVersions, version) && !isStatelessVersion(version)
}

// serveSession answers a message of a legacy client's session.
//
// The session must exist and belong to whoever presents it. The
// MCP-Protocol-Version header, which 2025-03-26 did not have, may be left out,
// since the session says which revision is spoken; one that names another is
// refused with 400, as the transport requires for a version that is not the
// one in use. So may Mcp-Method and Mcp-Name, which these revisions do not
// have either; but a request that carries them is held to them as a
// 2026-07-28 one is, since a gateway that admits tools by those headers would
// otherwise be told of one tool while another is called.
func (x *mcpExchange) serveSession(rerr *rpcError) error {
	session, ok := x.s.sessions.lookup(x.sessionID)
	if !ok || !session.ownedBy(mcpOwner(x.c)) {
		return errMCPUnknownSession()
	}
	if x.version != "" && x.version != session.version {
		return NewHTTPError(http.StatusBadRequest,
			"the MCP-Protocol-Version header names a revision other than the one this session negotiated")
	}
	x.era = eraOf(session.version)
	if x.msg.kind != rpcRequest {
		// The client's notifications and its answers to requests this server
		// never sends need nothing from it.
		return x.accepted()
	}
	if x.c.r.Header.Values(HeaderMCPMethod) != nil {
		if method, err := singleHeader(x.c.r.Header, HeaderMCPMethod); err != nil || method != x.msg.method {
			return x.headerMismatch("the Mcp-Method header does not name the request's method")
		}
	}
	method, ok := mcpSessionMethods[x.msg.method]
	switch {
	case !ok:
		return x.rpcFail(http.StatusOK, &rpcError{Code: rpcMethodNotFound, Message: "Method not found"})
	case rerr != nil:
		return x.rpcFail(http.StatusOK, rerr)
	}
	return method(x)
}

// serveStateless answers a 2026-07-28 message. The revision is named twice,
// in the MCP-Protocol-Version header and in the request's _meta, and so are
// the method and a called tool's name, in Mcp-Method and Mcp-Name; every pair
// must agree, so that an intermediary routing on the headers and this server
// acting on the body cannot be told different things.
func (x *mcpExchange) serveStateless(rerr *rpcError) error {
	x.era = eraStateless
	switch x.msg.kind {
	case rpcResponse:
		return x.rpcFail(http.StatusBadRequest, &rpcError{Code: rpcInvalidRequest,
			Message: "Invalid Request: a client does not answer this server, which sends it no requests"})
	case rpcNotification:
		return x.accepted()
	}
	if x.version == "" {
		return x.headerMismatch("the MCP-Protocol-Version header is missing")
	}
	if !isStatelessVersion(x.version) {
		return x.rpcFail(http.StatusBadRequest, &rpcError{
			Code:    rpcUnsupportedVersion,
			Message: "Unsupported protocol version",
			Data:    mcpUnsupportedVersion{Supported: mcpVersions, Requested: clientShorten(x.version)},
		})
	}
	if method, err := singleHeader(x.c.r.Header, HeaderMCPMethod); err != nil || method != x.msg.method {
		return x.headerMismatch("the Mcp-Method header is missing or does not name the request's method")
	}
	method, ok := mcpStatelessMethods[x.msg.method]
	if !ok {
		return x.rpcFail(http.StatusNotFound, &rpcError{Code: rpcMethodNotFound, Message: "Method not found"})
	}
	if rerr != nil {
		return x.rpcFail(http.StatusBadRequest, rerr)
	}
	var params struct {
		Meta *mcpRequestMeta `json:"_meta"`
	}
	if x.msg.params == nil || json.Unmarshal(x.msg.params, &params) != nil || params.Meta == nil {
		return x.rpcFail(http.StatusBadRequest, &rpcError{Code: rpcInvalidParams,
			Message: "Invalid params: a request carries its protocol version and client capabilities in _meta"})
	}
	if version, ok := rawString(params.Meta.ProtocolVersion); !ok || version != x.version {
		return x.headerMismatch("the MCP-Protocol-Version header does not match the protocol version in _meta")
	}
	if params.Meta.ClientCapabilities.Kind() != '{' {
		return x.rpcFail(http.StatusBadRequest, &rpcError{Code: rpcInvalidParams,
			Message: "Invalid params: _meta carries no client capabilities"})
	}
	return method(x)
}

// mcpRequestMeta is the part of a 2026-07-28 request's _meta this server
// reads.
type mcpRequestMeta struct {
	ProtocolVersion    jsontext.Value `json:"io.modelcontextprotocol/protocolVersion"`
	ClientCapabilities jsontext.Value `json:"io.modelcontextprotocol/clientCapabilities"`
}

// mcpUnsupportedVersion is the data of an UnsupportedProtocolVersionError.
type mcpUnsupportedVersion struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

// hasStatelessMeta reports whether params carry the protocol version a
// 2026-07-28 request names in its _meta.
func hasStatelessMeta(params jsontext.Value) bool {
	var p struct {
		Meta *mcpRequestMeta `json:"_meta"`
	}
	return params != nil && json.Unmarshal(params, &p) == nil && p.Meta != nil && p.Meta.ProtocolVersion != nil
}

// headerMismatch refuses a stateless request whose headers and body disagree.
func (x *mcpExchange) headerMismatch(why string) error {
	return x.rpcFail(http.StatusBadRequest, &rpcError{Code: rpcHeaderMismatch, Message: "Header mismatch: " + why})
}

// mcpInitializeParams is what an initialize request carries.
type mcpInitializeParams struct {
	ProtocolVersion jsontext.Value `json:"protocolVersion"`
	Capabilities    jsontext.Value `json:"capabilities"`
	ClientInfo      jsontext.Value `json:"clientInfo"`
}

// mcpImplementation names this server.
type mcpImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// mcpToolsCapability is the only capability the endpoint has: tools, whose
// list is fixed when the application is built and so never changes.
type mcpToolsCapability struct {
	Tools struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools"`
}

// mcpInitializeResult answers initialize.
type mcpInitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    mcpToolsCapability `json:"capabilities"`
	ServerInfo      mcpImplementation  `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitzero"`
}

// initialize opens a session. The revision is the one the client asked for
// when it is one this server speaks with sessions, and otherwise the newest
// that is, which is how the lifecycle has a server answer a version it does
// not support; a client that cannot speak it disconnects. The session belongs
// to the principal the endpoint's security verified, when it verified one.
func (x *mcpExchange) initialize(rerr *rpcError) error {
	var p mcpInitializeParams
	readParams(x.msg.params, &p)
	requested, ok := rawString(p.ProtocolVersion)
	if rerr == nil && (!ok || p.Capabilities.Kind() != '{' || p.ClientInfo.Kind() != '{') {
		rerr = &rpcError{Code: rpcInvalidParams,
			Message: "Invalid params: initialize carries a protocolVersion, capabilities and clientInfo"}
	}
	if rerr != nil {
		return x.rpcFail(http.StatusOK, rerr)
	}
	version := mcpVersion20251125
	if isLegacyVersion(requested) {
		version = requested
	}
	x.era = eraOf(version)
	x.c.w.Header().Set(HeaderMCPSessionID, x.s.sessions.create(version, mcpOwner(x.c)))
	return x.result(mcpInitializeResult{
		ProtocolVersion: version,
		ServerInfo:      x.s.serverInfo(),
		Instructions:    x.s.opts.Instructions,
	})
}

// readParams reads a request's params into a struct whose every member is raw
// JSON. The params are an object the message parser has validated, which such
// a struct always takes, and an absent member is left nil, so the reading
// cannot fail: what each member holds is for the method to check.
func readParams(params jsontext.Value, into any) {
	if params != nil {
		_ = json.Unmarshal(params, into)
	}
}

// serverInfo names the server after the API the OpenAPI document describes.
func (s *mcpServer) serverInfo() mcpImplementation {
	return mcpImplementation{
		Name:    orDefault(s.app.opts.Title, "Muzak API"),
		Version: orDefault(s.app.opts.Version, "0.1.0"),
	}
}

// ping answers that the server is there.
func (x *mcpExchange) ping() error { return x.result(struct{}{}) }

// mcpResultMeta is the _meta a 2026-07-28 result names its server in.
type mcpResultMeta struct {
	ServerInfo mcpImplementation `json:"io.modelcontextprotocol/serverInfo"`
}

// mcpDiscoverResult answers server/discover.
type mcpDiscoverResult struct {
	ResultType        string             `json:"resultType"`
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      mcpToolsCapability `json:"capabilities"`
	Instructions      string             `json:"instructions,omitzero"`
	TTLMs             int64              `json:"ttlMs"`
	CacheScope        string             `json:"cacheScope"`
	Meta              mcpResultMeta      `json:"_meta"`
}

// discover answers server/discover, which a 2026-07-28 client may ask before
// anything else.
func (x *mcpExchange) discover() error {
	return x.result(mcpDiscoverResult{
		ResultType:        "complete",
		SupportedVersions: mcpVersions,
		Instructions:      x.s.opts.Instructions,
		TTLMs:             x.s.ttlMs,
		CacheScope:        x.s.cacheScope,
		Meta:              mcpResultMeta{ServerInfo: x.s.serverInfo()},
	})
}

// mcpToolsListResult answers tools/list.
type mcpToolsListResult struct {
	ResultType string           `json:"resultType,omitzero"`
	Tools      []jsontext.Value `json:"tools"`
	NextCursor string           `json:"nextCursor,omitzero"`
	TTLMs      *int64           `json:"ttlMs,omitzero"`
	CacheScope string           `json:"cacheScope,omitzero"`
	Meta       *mcpResultMeta   `json:"_meta,omitzero"`
}

// toolsList answers one page of tools/list, in the order of the tools' names,
// which is the same on every call.
func (x *mcpExchange) toolsList() error {
	var p struct {
		Cursor jsontext.Value `json:"cursor"`
	}
	readParams(x.msg.params, &p)
	start := 0
	if p.Cursor != nil && p.Cursor.Kind() != 'n' {
		cursor, _ := rawString(p.Cursor)
		var ok bool
		if start, ok = x.s.pageStart(cursor); !ok {
			return x.rpcFail(x.invalidParamsStatus(), &rpcError{Code: rpcInvalidParams,
				Message: "Invalid params: the cursor is not one this server issued"})
		}
	}
	end := min(start+x.s.opts.PageSize, len(x.s.tools))
	result := mcpToolsListResult{Tools: make([]jsontext.Value, 0, end-start)}
	for _, tool := range x.s.tools[start:end] {
		result.Tools = append(result.Tools, tool.listing[x.era])
	}
	if end < len(x.s.tools) {
		result.NextCursor = x.s.cursor(end)
	}
	if x.era == eraStateless {
		result.ResultType = "complete"
		result.TTLMs = &x.s.ttlMs
		result.CacheScope = x.s.cacheScope
		result.Meta = &mcpResultMeta{ServerInfo: x.s.serverInfo()}
	}
	return x.result(result)
}

// invalidParamsStatus is the status a -32602 is sent with: 200 under a
// session, where JSON-RPC errors are carried in successful responses, and
// 400 under 2026-07-28, which sends them as Bad Request.
func (x *mcpExchange) invalidParamsStatus() int {
	if x.stateless {
		return http.StatusBadRequest
	}
	return http.StatusOK
}

// The cursors tools/list hands out are the index a page starts at,
// authenticated with HMAC-SHA256 under a key drawn when the application is
// built, so that a client can neither forge nor alter one: a cursor is
// opaque, and one that was not issued by this process is refused. A cursor is
// 4 bytes of index and 16 of tag, in unpadded base64url.
const (
	mcpCursorTagLength = 16
	mcpCursorLength    = 27
)

// cursor returns the cursor of the page starting at index.
func (s *mcpServer) cursor(index int) string {
	var raw [4 + mcpCursorTagLength]byte
	binary.BigEndian.PutUint32(raw[:4], uint32(index)) //nolint:gosec // index is the position of a tool, far below 2^32
	copy(raw[4:], s.cursorTag(raw[:4]))
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// pageStart reads a cursor back into the index of the page it starts.
func (s *mcpServer) pageStart(cursor string) (int, bool) {
	if len(cursor) != mcpCursorLength {
		return 0, false
	}
	// Strict, so that a cursor has one spelling: the bits left over at its
	// end must be zero rather than ignored.
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || len(raw) != 4+mcpCursorTagLength || !hmac.Equal(raw[4:], s.cursorTag(raw[:4])) {
		return 0, false
	}
	index := int(binary.BigEndian.Uint32(raw[:4]))
	return index, index > 0 && index < len(s.tools)
}

// cursorTag authenticates the index of a cursor.
func (s *mcpServer) cursorTag(index []byte) []byte {
	mac := hmac.New(sha256.New, s.cursorKey[:])
	mac.Write([]byte("muzak mcp tools/list cursor\x00"))
	mac.Write(index)
	return mac.Sum(nil)[:mcpCursorTagLength]
}

// toolsCall calls a tool. A name that is not a tool's and arguments that are
// not an object are protocol errors; everything about the call itself,
// arguments that do not fit the tool's schema included, is the tool's result,
// so that a model reads it and can try again.
func (x *mcpExchange) toolsCall() error {
	var p struct {
		Name      jsontext.Value `json:"name"`
		Arguments jsontext.Value `json:"arguments"`
	}
	readParams(x.msg.params, &p)
	name, ok := rawString(p.Name)
	if !ok {
		return x.rpcFail(x.invalidParamsStatus(), &rpcError{Code: rpcInvalidParams,
			Message: "Invalid params: tools/call names the tool to call in name"})
	}
	if x.stateless || x.c.r.Header.Values(HeaderMCPName) != nil {
		// Required under 2026-07-28, and held to the body wherever it is sent;
		// see [mcpExchange.serveSession].
		if header, err := singleHeader(x.c.r.Header, HeaderMCPName); err != nil || decodeMCPHeaderValue(header) != name {
			return x.headerMismatch("the Mcp-Name header is missing or does not name the tool called")
		}
	}
	tool := x.s.byName[name]
	if tool == nil {
		return x.rpcFail(x.invalidParamsStatus(), &rpcError{Code: rpcInvalidParams, Message: "Invalid params: no tool has that name"})
	}
	if p.Arguments != nil && p.Arguments.Kind() != '{' && p.Arguments.Kind() != 'n' {
		return x.rpcFail(x.invalidParamsStatus(), &rpcError{Code: rpcInvalidParams,
			Message: "Invalid params: the arguments of a tool call are an object"})
	}
	return x.result(x.s.call(x.c, tool, p.Arguments, x.era))
}

// decodeMCPHeaderValue reads a header value 2026-07-28 may write in the
// "=?base64?...?=" form, for a value a header cannot carry as it is. A value
// in that form that does not decode to UTF-8 reads as nothing, which matches
// no name.
func decodeMCPHeaderValue(value string) string {
	encoded, wrapped := strings.CutPrefix(value, "=?base64?")
	if !wrapped {
		return value
	}
	encoded, wrapped = strings.CutSuffix(encoded, "?=")
	if !wrapped {
		return value
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !utf8.Valid(decoded) {
		return ""
	}
	return string(decoded)
}

// result answers the request with a result.
func (x *mcpExchange) result(v any) error {
	data, err := encodeRPCResult(x.msg.id, v)
	if err != nil {
		// coverage: every result is built from strings, numbers and JSON
		// that was validated before it was kept, all of which encode.
		x.c.logger.ErrorContext(x.c.Context(), "muzak: an MCP result could not be encoded",
			slog.String(RequestIDKey, x.c.RequestID()), slog.String("error", err.Error()))
		return x.rpcFail(http.StatusInternalServerError, &rpcError{Code: rpcInternalError, Message: "Internal error"})
	}
	return x.reply(http.StatusOK, data)
}

// rpcFail answers with a JSON-RPC error.
func (x *mcpExchange) rpcFail(status int, e *rpcError) error {
	var id jsontext.Value
	if x.msg != nil {
		id = x.msg.id
	}
	return x.reply(status, encodeRPCError(id, e, x.stateless))
}

// accepted answers a notification or a response, which get no body.
func (x *mcpExchange) accepted() error {
	if err := x.c.settle(nil); err != nil {
		return err
	}
	x.c.w.WriteHeader(http.StatusAccepted)
	return nil
}

// reply writes a JSON-RPC answer. The request's releases are settled first,
// as they are before any response the framework writes.
func (x *mcpExchange) reply(status int, data []byte) error {
	if err := x.c.settle(nil); err != nil {
		return err
	}
	header := x.c.w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(data)))
	x.c.w.WriteHeader(status)
	_, _ = x.c.w.Write(data)
	return nil
}

// mcpOwner identifies the principal the endpoint's security verified for a
// request: the issuer and subject of a bearer token, or the identifier of an
// API key, with the scheme that verified it. It is zero when nothing was
// verified, and a session opened that way belongs to nobody in particular.
//
// Binding a session to its principal is the specification's advice against
// session hijacking. A session grants nothing here, but a client presenting
// another principal's session is told it does not exist, as though it had
// guessed one.
func mcpOwner(c *Context) [sha256.Size]byte {
	var parts []string
	if claims, ok := TryFrom[*Claims](c); ok && claims != nil {
		parts = append(parts, "jwt", claims.Scheme, claims.Issuer, claims.Subject)
	}
	if key, ok := TryFrom[*APIKeyPrincipal](c); ok && key != nil {
		parts = append(parts, "key", key.Scheme, key.ID)
	}
	if len(parts) == 0 {
		return [sha256.Size]byte{}
	}
	digest := sha256.New()
	for _, part := range parts {
		// Each part is prefixed with its length, so that no two lists of
		// parts write the same bytes.
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		digest.Write(size[:])
		digest.Write([]byte(part))
	}
	return [sha256.Size]byte(digest.Sum(nil))
}

// ownedBy reports whether a request presenting owner may use the session.
func (session mcpSession) ownedBy(owner [sha256.Size]byte) bool {
	return session.owner == [sha256.Size]byte{} || session.owner == owner
}
