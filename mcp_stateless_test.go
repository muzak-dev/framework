package muzak_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// statelessClient is a 2026-07-28 client of the shop.
func statelessClient(t *testing.T) *mcpClient {
	t.Helper()
	m := newMCPClient(t, shopApp(muzak.MCPOptions{Instructions: "Read the catalogue."}))
	m.stateless, m.version = true, "2026-07-28"
	return m
}

// TestMCPStatelessDiscoverAndCall serves a 2026-07-28 client with no
// handshake and no session: server/discover names the revisions, the
// capability and the server, and every result names its type and the server.
func TestMCPStatelessDiscoverAndCall(t *testing.T) {
	m := statelessClient(t)
	res := m.post(m.message("server/discover", nil), statelessHeaders("server/discover", "")...)
	if res.Header.Get(muzak.HeaderMCPSessionID) != "" {
		t.Fatal("a stateless answer issued a session")
	}
	jsonEqual(t, decodeReply(t, res, http.StatusOK).Result, `{
		"resultType":"complete",
		"supportedVersions":["2026-07-28","2025-11-25","2025-06-18","2025-03-26"],
		"capabilities":{"tools":{"listChanged":false}},
		"instructions":"Read the catalogue.",
		"ttlMs":300000,
		"cacheScope":"public",
		"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"Shop API","version":"2.1.0"}}
	}`)
	got := m.call("get_item", map[string]any{"path": map[string]any{"id": "9"}})
	jsonEqual(t, got.StructuredContent, `{"id":"9","name":"item verbose=false limit=10","price":5}`)
	jsonEqual(t, got.Meta, `{"io.modelcontextprotocol/serverInfo":{"name":"Shop API","version":"2.1.0"}}`)
	failed := m.call("get_item", map[string]any{})
	if !failed.IsError || failed.ResultType != "complete" {
		t.Fatalf("a failed call = %s", failed.raw)
	}
	// The tool name may travel in a header in base64, as a name a header
	// cannot carry would have to.
	body := m.message("tools/call", map[string]any{"name": "get_item", "arguments": map[string]any{"path": map[string]any{"id": "1"}}})
	encoded := m.post(body, testclient.Header("Mcp-Method", "tools/call"), testclient.Header("Mcp-Name", "=?base64?Z2V0X2l0ZW0=?="))
	if reply := decodeReply(t, encoded, http.StatusOK); reply.Error != nil {
		t.Fatalf("a base64 Mcp-Name was refused: %s", encoded.Body)
	}
}

// TestMCPStatelessHeadersMustMatchTheBody refuses, with 400 and -32020, a
// request whose headers are missing or say something its body does not, so
// that an intermediary routing on one and this server acting on the other
// cannot be told different things.
func TestMCPStatelessHeadersMustMatchTheBody(t *testing.T) {
	m := statelessClient(t)
	call := m.message("tools/call", map[string]any{"name": "get_item", "arguments": map[string]any{"path": map[string]any{"id": "1"}}})
	for _, headers := range map[string][]testclient.RequestOption{
		"no Mcp-Method":         {testclient.Header("Mcp-Name", "get_item")},
		"another Mcp-Method":    {testclient.Header("Mcp-Method", "tools/list"), testclient.Header("Mcp-Name", "get_item")},
		"no Mcp-Name":           {testclient.Header("Mcp-Method", "tools/call")},
		"another Mcp-Name":      {testclient.Header("Mcp-Method", "tools/call"), testclient.Header("Mcp-Name", "delete_item")},
		"Mcp-Name not base64":   {testclient.Header("Mcp-Method", "tools/call"), testclient.Header("Mcp-Name", "=?base64?***?=")},
		"Mcp-Name not UTF-8":    {testclient.Header("Mcp-Method", "tools/call"), testclient.Header("Mcp-Name", "=?base64?/w==?=")},
		"Mcp-Name half wrapped": {testclient.Header("Mcp-Method", "tools/call"), testclient.Header("Mcp-Name", "=?base64?Z2V0X2l0ZW0=")},
	} {
		fault(t, decodeReply(t, m.post(call, headers...), http.StatusBadRequest), -32020)
	}
	// The version is named in the header and in _meta, and they must agree.
	other := strings.Replace(call, `protocolVersion":"2026-07-28"`, `protocolVersion":"2025-11-25"`, 1)
	fault(t, decodeReply(t, m.post(other, statelessHeaders("tools/call", "get_item")...), http.StatusBadRequest), -32020)
	m.version = ""
	missing := decodeReply(t, m.post(call, statelessHeaders("tools/call", "get_item")...), http.StatusBadRequest)
	mustContain(t, fault(t, missing, -32020).Message, "MCP-Protocol-Version header is missing")
}

// TestMCPStatelessUnsupportedVersion answers a revision the server does not
// speak with -32022 and the revisions it does, which is how a 2026-07-28
// client recognises a modern server and retries.
func TestMCPStatelessUnsupportedVersion(t *testing.T) {
	m := statelessClient(t)
	m.version = "2099-01-01"
	reply := decodeReply(t, m.post(m.message("tools/list", nil), statelessHeaders("tools/list", "")...), http.StatusBadRequest)
	e := fault(t, reply, -32022)
	jsonEqual(t, e.Data, `{"supported":["2026-07-28","2025-11-25","2025-06-18","2025-03-26"],"requested":"2099-01-01"}`)
	// A revision that opens sessions is not spoken statelessly.
	m.version = "2025-11-25"
	fault(t, decodeReply(t, m.post(m.message("tools/list", nil), statelessHeaders("tools/list", "")...), http.StatusBadRequest), -32022)
}

// TestMCPStatelessMetadata refuses a request missing the protocol fields
// every 2026-07-28 request carries with 400 and -32602.
func TestMCPStatelessMetadata(t *testing.T) {
	m := statelessClient(t)
	headers := statelessHeaders("tools/list", "")
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":"x"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":[]}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`,
	} {
		reply := decodeReply(t, m.post(body, headers...), http.StatusBadRequest)
		fault(t, reply, -32602)
		if string(reply.ID) != "1" {
			t.Fatalf("%s: id = %s", body, reply.ID)
		}
	}
	// Invalid params of a well-formed request are a 400 here too.
	fault(t, m.send(http.StatusBadRequest, "tools/call", map[string]any{"name": "no_such_tool"}), -32602)
	fault(t, m.send(http.StatusBadRequest, "tools/list", map[string]any{"cursor": "forged"}), -32602)
	fault(t, m.send(http.StatusBadRequest, "tools/call", map[string]any{"name": "get_item", "arguments": "x"}), -32602)
}

// TestMCPStatelessMessagesThatAreNotRequests accepts a notification with 202
// and refuses a response, since the server sends a 2026-07-28 client no
// requests, and leaves out an id it could not read rather than send null.
func TestMCPStatelessMessagesThatAreNotRequests(t *testing.T) {
	m := statelessClient(t)
	m.post(`{"jsonrpc":"2.0","method":"notifications/progress"}`).AssertStatus(http.StatusAccepted)
	fault(t, decodeReply(t, m.post(`{"jsonrpc":"2.0","id":1,"result":{}}`), http.StatusBadRequest), -32600)
	res := m.post(`{"jsonrpc":`)
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(res.Body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, has := raw["id"]; has {
		t.Fatalf("a 2026-07-28 error with no id to echo carries one: %s", res.Body)
	}
	fault(t, decodeReply(t, res, http.StatusBadRequest), -32700)
}
