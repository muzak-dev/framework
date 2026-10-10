package muzak_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// mcpAccept is the Accept header every MCP client sends.
const mcpAccept = "application/json, text/event-stream"

// mcpClient speaks the MCP transport to an application served by testclient,
// as a client of one revision would.
type mcpClient struct {
	t       *testing.T
	http    *testclient.Client
	path    string
	session string
	// version is the negotiated revision a session client sends in
	// MCP-Protocol-Version, or 2026-07-28 for a stateless client.
	version   string
	stateless bool
	next      int
	// extra are options sent with every message, a credential say.
	extra []testclient.RequestOption
}

// newMCPClient serves app and returns a client of its endpoint at /mcp.
func newMCPClient(t *testing.T, app *muzak.App, opts ...testclient.Option) *mcpClient {
	t.Helper()
	return &mcpClient{t: t, http: testclient.New(t, app, opts...), path: "/mcp"}
}

// fork returns a second client of the same server, with no session.
func (m *mcpClient) fork() *mcpClient {
	return &mcpClient{t: m.t, http: m.http, path: m.path}
}

// rpcReply is a JSON-RPC answer.
type rpcReply struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  jsontext.Value `json:"result"`
	Error   *rpcFault      `json:"error"`
}

// rpcFault is a JSON-RPC error object.
type rpcFault struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    jsontext.Value `json:"data"`
}

// headers returns the transport headers the client sends with every message.
func (m *mcpClient) headers() []testclient.RequestOption {
	opts := []testclient.RequestOption{testclient.Header("Accept", mcpAccept)}
	if m.session != "" {
		opts = append(opts, testclient.Header(muzak.HeaderMCPSessionID, m.session))
	}
	if m.version != "" && m.version != "2025-03-26" {
		opts = append(opts, testclient.Header(muzak.HeaderMCPProtocolVersion, m.version))
	}
	return append(opts, m.extra...)
}

// post sends a raw message with the client's transport headers and extra
// options after them.
func (m *mcpClient) post(body string, extra ...testclient.RequestOption) *testclient.Response {
	m.t.Helper()
	opts := append([]testclient.RequestOption{testclient.RawJSON(body)}, m.headers()...)
	return m.http.Post(m.path, append(opts, extra...)...)
}

// message encodes a request with the next id.
func (m *mcpClient) message(method string, params any) string {
	m.t.Helper()
	m.next++
	envelope := map[string]any{"jsonrpc": "2.0", "id": m.next, "method": method}
	if m.stateless {
		p, _ := params.(map[string]any)
		if p == nil {
			p = map[string]any{}
		}
		p["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test", "version": "1"},
		}
		params = p
	}
	if params != nil {
		envelope["params"] = params
	}
	data, err := json.Marshal(envelope, json.Deterministic(true))
	if err != nil {
		m.t.Fatalf("encoding %s: %v", method, err)
	}
	return string(data)
}

// statelessHeaders are the headers 2026-07-28 mirrors from the body.
func statelessHeaders(method, name string) []testclient.RequestOption {
	opts := []testclient.RequestOption{testclient.Header("Mcp-Method", method)}
	if name != "" {
		opts = append(opts, testclient.Header("Mcp-Name", name))
	}
	return opts
}

// send sends a request and decodes the answer, failing unless it is a
// JSON-RPC answer with the expected status.
func (m *mcpClient) send(status int, method string, params any) rpcReply {
	m.t.Helper()
	var extra []testclient.RequestOption
	if m.stateless {
		name := ""
		if p, ok := params.(map[string]any); ok {
			name, _ = p["name"].(string)
		}
		extra = statelessHeaders(method, name)
	}
	res := m.post(m.message(method, params), extra...)
	return decodeReply(m.t, res, status)
}

// decodeReply decodes a JSON-RPC answer.
func decodeReply(t *testing.T, res *testclient.Response, status int) rpcReply {
	t.Helper()
	res.AssertStatus(status)
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json; body %s", got, res.Body)
	}
	var reply rpcReply
	if err := json.Unmarshal(res.Body, &reply, json.RejectUnknownMembers(true)); err != nil {
		t.Fatalf("the answer is not a JSON-RPC response: %v\n%s", err, res.Body)
	}
	if reply.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q", reply.JSONRPC)
	}
	return reply
}

// result decodes a successful answer's result into T.
func result[T any](t *testing.T, reply rpcReply) T {
	t.Helper()
	if reply.Error != nil {
		t.Fatalf("the request failed: %d %s", reply.Error.Code, reply.Error.Message)
	}
	var out T
	if err := json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatalf("result %s: %v", reply.Result, err)
	}
	return out
}

// fault returns the error of an answer that must have failed with code.
func fault(t *testing.T, reply rpcReply, code int) *rpcFault {
	t.Helper()
	if reply.Error == nil {
		t.Fatalf("the request succeeded with %s, want error %d", reply.Result, code)
	}
	if reply.Error.Code != code {
		t.Fatalf("error code = %d (%s), want %d", reply.Error.Code, reply.Error.Message, code)
	}
	if reply.Result != nil {
		t.Fatalf("an error answer carries a result: %s", reply.Result)
	}
	return reply.Error
}

// initialize opens a session under version and returns the result.
func (m *mcpClient) initialize(version string) initializeResult {
	m.t.Helper()
	res := m.post(m.message("initialize", map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	}))
	out := result[initializeResult](m.t, decodeReply(m.t, res, http.StatusOK))
	m.session = res.Header.Get(muzak.HeaderMCPSessionID)
	if m.session == "" {
		m.t.Fatal("initialize answered without an Mcp-Session-Id")
	}
	m.version = out.ProtocolVersion
	notified := m.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	notified.AssertStatus(http.StatusAccepted)
	return out
}

// initializeResult is what initialize answers.
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Tools struct {
			ListChanged *bool `json:"listChanged"`
		} `json:"tools"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// toolResult is what tools/call answers.
type toolResult struct {
	ResultType        string         `json:"resultType"`
	Content           []contentItem  `json:"content"`
	StructuredContent jsontext.Value `json:"structuredContent"`
	IsError           bool           `json:"isError"`
	Meta              map[string]any `json:"_meta"`
	Unknown           jsontext.Value `json:",unknown"`
	raw               jsontext.Value
}

// contentItem is one content item of a result.
type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// text returns the only text item of a result.
func (r toolResult) text(t *testing.T) string {
	t.Helper()
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want one text item", r.Content)
	}
	return r.Content[0].Text
}

// call calls a tool and returns its result.
func (m *mcpClient) call(name string, args any) toolResult {
	m.t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	reply := m.send(http.StatusOK, "tools/call", params)
	out := result[toolResult](m.t, reply)
	out.raw = reply.Result
	return out
}

// errorEnvelope decodes the error envelope an error result carries.
func errorEnvelope(t *testing.T, r toolResult) muzak.ErrorResponse {
	t.Helper()
	if !r.IsError {
		t.Fatalf("the result is not an error: %+v", r)
	}
	var out muzak.ErrorResponse
	if err := json.Unmarshal([]byte(r.text(t)), &out); err != nil {
		t.Fatalf("the error result is not an error envelope: %v\n%s", err, r.text(t))
	}
	return out
}

// toolDef is a tool as tools/list lists it.
type toolDef struct {
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	InputSchema  jsontext.Value `json:"inputSchema"`
	OutputSchema jsontext.Value `json:"outputSchema"`
	Annotations  *struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
	} `json:"annotations"`
}

// toolsPage is one page of tools/list.
type toolsPage struct {
	ResultType string         `json:"resultType"`
	Tools      []toolDef      `json:"tools"`
	NextCursor string         `json:"nextCursor"`
	TTLMs      *int64         `json:"ttlMs"`
	CacheScope string         `json:"cacheScope"`
	Meta       map[string]any `json:"_meta"`
}

// listAll lists every tool, following cursors.
func (m *mcpClient) listAll() []toolDef {
	m.t.Helper()
	var all []toolDef
	var params any
	for range 1000 {
		page := result[toolsPage](m.t, m.send(http.StatusOK, "tools/list", params))
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			return all
		}
		params = map[string]any{"cursor": page.NextCursor}
	}
	m.t.Fatal("tools/list never ended")
	return nil
}

// toolNamed finds a tool in a listing.
func toolNamed(t *testing.T, tools []toolDef, name string) toolDef {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	t.Fatalf("no tool %q among %s", name, strings.Join(names, ", "))
	return toolDef{}
}

// jsonEqual compares two JSON documents by value.
func jsonEqual(t *testing.T, got, want any) {
	t.Helper()
	normal := func(v any) string {
		var data []byte
		switch v := v.(type) {
		case string:
			data = []byte(v)
		case []byte:
			data = v
		case jsontext.Value:
			data = v
		default:
			var err error
			if data, err = json.Marshal(v); err != nil {
				t.Fatalf("encoding %T: %v", v, err)
			}
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, data)
		}
		out, _ := json.Marshal(value, json.Deterministic(true))
		return string(out)
	}
	if g, w := normal(got), normal(want); g != w {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", g, w)
	}
}

// mustContain fails unless text holds every fragment.
func mustContain(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("%q is missing from:\n%s", fragment, text)
		}
	}
}

// quietMCPOptions are application options that keep tests silent.
func quietMCPOptions() muzak.AppOptions {
	return muzak.AppOptions{
		Title:         "Shop API",
		Version:       "2.1.0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	}
}

// Shop is the application most tests serve: a catalogue whose routes cover
// each part of a request.

type shopItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
}

type getItemIn struct {
	ID      string `path:"id" doc:"The item's identifier"`
	Verbose bool   `query:"verbose" doc:"Include everything"`
	Limit   int    `query:"limit" default:"10"`
}

type createItemIn struct {
	Tenant string `header:"X-Tenant" required:"true"`
	Name   string `json:"name" doc:"What the item is called"`
	Price  int    `json:"price"`
	Color  string `json:"color" default:"red"`
}

func (in *createItemIn) Validate(v *muzak.Validation) {
	v.String(&in.Name).Required().MaxLen(20)
	v.Number(&in.Price).Between(1, 1000)
}

type createdItem struct {
	Item   shopItem `json:"item"`
	Tenant string   `json:"tenant"`
	Color  string   `json:"color"`
}

// shopApp builds the catalogue with an MCP endpoint choosing the "agent" tag.
func shopApp(opts muzak.MCPOptions, routeOpts ...muzak.RouteOption) *muzak.App {
	app := muzak.New(quietMCPOptions())
	if opts.Tags == nil && opts.Include == nil {
		opts.Tags = []string{"agent"}
	}
	app.MCP("/mcp", opts, routeOpts...)
	r := muzak.NewRouter(muzak.WithTags("agent"))
	r.Get("/items/{id}", func(_ *muzak.Context, in getItemIn) (shopItem, error) {
		if in.ID == "missing" {
			return shopItem{}, muzak.NotFound("no such item")
		}
		return shopItem{ID: in.ID, Name: fmt.Sprintf("item verbose=%t limit=%d", in.Verbose, in.Limit), Price: 5}, nil
	}, muzak.Summary("Fetch an item"), muzak.Description("Reads one item by its id."), muzak.OperationID("get_item"), muzak.Title("Get Item"))
	r.Post("/items", func(_ *muzak.Context, in createItemIn) (createdItem, error) {
		return createdItem{Item: shopItem{ID: "new", Name: in.Name, Price: in.Price}, Tenant: in.Tenant, Color: in.Color}, nil
	}, muzak.Status(http.StatusCreated), muzak.OperationID("create_item"), muzak.Summary("Create an item"))
	r.Delete("/items/{id}", func(_ *muzak.Context, _ getItemIn) (muzak.Empty, error) {
		return muzak.Empty{}, nil
	}, muzak.Status(http.StatusNoContent), muzak.OperationID("delete_item"))
	app.Include(r)
	app.Get("/internal", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil })
	return app
}
