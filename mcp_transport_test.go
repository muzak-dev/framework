package muzak_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// TestMCPInitializeNegotiatesTheRevision opens a session under every legacy
// revision, each answered with itself, and under revisions the server does
// not open sessions for, each answered with the newest one it does.
func TestMCPInitializeNegotiatesTheRevision(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{Instructions: "Use get_item to read the catalogue."}))
	for requested, want := range map[string]string{
		"2025-03-26": "2025-03-26",
		"2025-06-18": "2025-06-18",
		"2025-11-25": "2025-11-25",
		"2024-11-05": "2025-11-25",
		"2026-07-28": "2025-11-25",
		"not a date": "2025-11-25",
	} {
		t.Run(requested, func(t *testing.T) {
			client := m.fork()
			client.t = t
			got := client.initialize(requested)
			if got.ProtocolVersion != want {
				t.Fatalf("protocolVersion = %q, want %q", got.ProtocolVersion, want)
			}
			if got.ServerInfo.Name != "Shop API" || got.ServerInfo.Version != "2.1.0" {
				t.Fatalf("serverInfo = %+v, want the OpenAPI title and version", got.ServerInfo)
			}
			if got.Capabilities.Tools.ListChanged == nil || *got.Capabilities.Tools.ListChanged {
				t.Fatal("capabilities.tools.listChanged must be false: the tools are fixed when the application is built")
			}
			if got.Instructions != "Use get_item to read the catalogue." {
				t.Fatalf("instructions = %q", got.Instructions)
			}
			if len(client.session) != 26 || strings.Trim(client.session, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
				t.Fatalf("session id %q is not 26 characters of base32", client.session)
			}
		})
	}
}

// TestMCPSessionIdentifiersAreUnique opens many sessions and finds no two
// identifiers alike.
func TestMCPSessionIdentifiersAreUnique(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	seen := map[string]bool{}
	for range 50 {
		client := m.fork()
		client.initialize("2025-06-18")
		if seen[client.session] {
			t.Fatalf("session id %q was issued twice", client.session)
		}
		seen[client.session] = true
	}
}

// TestMCPInitializeRefusesMalformedParams refuses initialize without each of
// the members the lifecycle requires.
func TestMCPInitializeRefusesMalformedParams(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	for name, params := range map[string]any{
		"no params":            nil,
		"no protocolVersion":   map[string]any{"capabilities": map[string]any{}, "clientInfo": map[string]any{}},
		"numeric version":      map[string]any{"protocolVersion": 2025, "capabilities": map[string]any{}, "clientInfo": map[string]any{}},
		"no capabilities":      map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{}},
		"capabilities a list":  map[string]any{"protocolVersion": "2025-06-18", "capabilities": []int{}, "clientInfo": map[string]any{}},
		"no clientInfo":        map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}},
		"clientInfo a string":  map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": "x"},
		"params a list":        []any{"2025-06-18"},
		"protocolVersion null": map[string]any{"protocolVersion": nil, "capabilities": map[string]any{}, "clientInfo": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			res := m.post(m.message("initialize", params))
			fault(t, decodeReply(t, res, http.StatusOK), -32602)
			if res.Header.Get(muzak.HeaderMCPSessionID) != "" {
				t.Fatal("a refused initialize opened a session")
			}
		})
	}
}

// TestMCPPingAndNotifications covers ping and the 202 every notification and
// client response gets, with no body.
func TestMCPPingAndNotifications(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-11-25")
	if got := m.send(http.StatusOK, "ping", nil); string(got.Result) != "{}" {
		t.Fatalf("ping = %s, want {}", got.Result)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"gone"}}`,
		`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
		`{"jsonrpc":"2.0","id":"x","error":{"code":-1,"message":"no"}}`,
	} {
		res := m.post(body)
		res.AssertStatus(http.StatusAccepted)
		if len(res.Body) != 0 {
			t.Fatalf("%s: a 202 carries a body: %s", body, res.Body)
		}
	}
}

// TestMCPUnknownMethods answers a method the server does not implement with
// -32601, in a 200 under a session and in a 404 under 2026-07-28, as each
// transport has it.
func TestMCPUnknownMethods(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	for _, method := range []string{"resources/list", "prompts/list", "server/discover", "initialized", "tools/List"} {
		fault(t, m.send(http.StatusOK, method, nil), -32601)
	}
	s := m.fork()
	s.stateless, s.version = true, "2026-07-28"
	for _, method := range []string{"ping", "resources/list", "initialize/x"} {
		fault(t, s.send(http.StatusNotFound, method, nil), -32601)
	}
}

// TestMCPOnlyPostAndDelete refuses GET, since no stream from the server is
// offered, and every other method, with 405 and the methods that are.
func TestMCPOnlyPostAndDelete(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch} {
		res := m.http.Do(method, "/mcp", testclient.Header("Accept", "text/event-stream"))
		res.AssertStatus(http.StatusMethodNotAllowed)
		if allow := res.Header.Get("Allow"); allow != "DELETE, OPTIONS, POST" {
			t.Fatalf("%s: Allow = %q", method, allow)
		}
	}
	m.http.Head("/mcp").AssertStatus(http.StatusMethodNotAllowed)
}

// TestMCPDeleteEndsTheSession ends a session, after which it is unknown, and
// refuses a DELETE that names none.
func TestMCPDeleteEndsTheSession(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	other := m.fork()
	other.initialize("2025-06-18")
	session := testclient.Header(muzak.HeaderMCPSessionID, m.session)
	m.http.Delete("/mcp", session).AssertStatus(http.StatusNoContent)
	m.post(m.message("ping", nil)).AssertStatus(http.StatusNotFound).AssertErrorCode(muzak.CodeNotFound)
	m.http.Delete("/mcp", session).AssertStatus(http.StatusNotFound)
	m.http.Delete("/mcp").AssertStatus(http.StatusBadRequest)
	// The other session is untouched.
	other.send(http.StatusOK, "ping", nil)
}

// TestMCPMediaTypeChecks holds every POST to the media types the transport
// specifies: a JSON body in UTF-8, and an Accept that admits JSON.
func TestMCPMediaTypeChecks(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	body := m.message("ping", nil)
	for _, tc := range []struct {
		contentType, accept string
		status              int
	}{
		{"application/json", mcpAccept, http.StatusBadRequest},
		{"application/json; charset=UTF-8", "application/json", http.StatusBadRequest},
		{"application/json", "*/*", http.StatusBadRequest},
		{"application/json", "application/*", http.StatusBadRequest},
		{"application/json", "text/event-stream;q=1, application/json;q=0.1", http.StatusBadRequest},
		{"", mcpAccept, http.StatusUnsupportedMediaType},
		{"text/plain", mcpAccept, http.StatusUnsupportedMediaType},
		{"application/x-www-form-urlencoded", mcpAccept, http.StatusUnsupportedMediaType},
		{"application/json; charset=utf-16", mcpAccept, http.StatusUnsupportedMediaType},
		{"application/json;;", mcpAccept, http.StatusUnsupportedMediaType},
		{"application/json", "", http.StatusNotAcceptable},
		{"application/json", "text/event-stream", http.StatusNotAcceptable},
		{"application/json", "application/json;q=0, */*", http.StatusNotAcceptable},
		{"application/json", "application/json;q=x", http.StatusNotAcceptable},
		{"application/json", "application/json;q=2", http.StatusNotAcceptable},
		{"application/json", "text/html, application/xml", http.StatusNotAcceptable},
	} {
		// Every message without a session is refused with 400 once the media
		// types pass, which is how a pass shows here.
		res := m.http.Post("/mcp", testclient.Body(tc.contentType, strings.NewReader(body)), testclient.Header("Accept", tc.accept))
		if res.Status != tc.status {
			t.Fatalf("Content-Type %q, Accept %q: status = %d, want %d\n%s", tc.contentType, tc.accept, res.Status, tc.status, res.Body)
		}
	}
}

// TestMCPProtocolVersionHeader accepts a session's messages with the header
// naming its revision or without one, and refuses one naming another revision
// or none at all with 400.
func TestMCPProtocolVersionHeader(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	ping := m.message("ping", nil)
	m.post(ping).AssertStatus(http.StatusOK)
	m.version = ""
	m.post(ping).AssertStatus(http.StatusOK)
	for _, header := range []string{"2025-03-26", "2025-11-25", "2026-07-28", "garbage", "2025-06-18x"} {
		m.post(ping, testclient.Header(muzak.HeaderMCPProtocolVersion, header)).AssertStatus(http.StatusBadRequest)
	}
	// A 2025-03-26 session predates the header and is served without it.
	old := m.fork()
	old.initialize("2025-03-26")
	old.post(old.message("ping", nil)).AssertStatus(http.StatusOK)
}

// TestMCPHeadersSentTwice refuses a message carrying the session or the
// revision twice, which two components could read differently.
func TestMCPHeadersSentTwice(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	for _, name := range []string{muzak.HeaderMCPSessionID, muzak.HeaderMCPProtocolVersion, "Origin"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, m.http.URL()+"/mcp", strings.NewReader(m.message("ping", nil)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", mcpAccept)
		req.Header.Set(muzak.HeaderMCPSessionID, m.session)
		req.Header.Set(muzak.HeaderMCPProtocolVersion, m.version)
		req.Header.Set("Origin", "http://"+strings.TrimPrefix(m.http.URL(), "http://"))
		req.Header.Add(name, req.Header.Get(name))
		res, err := m.http.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s twice: status = %d, want 400", name, res.StatusCode)
		}
	}
}

// TestMCPSessionRules refuses a message that needs a session and has none,
// one naming a session that does not exist, whatever shape the identifier
// has, and an initialize that names one.
func TestMCPSessionRules(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	ping := m.message("ping", nil)
	m.post(ping).AssertStatus(http.StatusBadRequest)
	m.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`).AssertStatus(http.StatusBadRequest)
	m.post(`{"jsonrpc":"2.0","id":1,"result":{}}`).AssertStatus(http.StatusBadRequest)
	m.post(ping, testclient.Header(muzak.HeaderMCPProtocolVersion, "2025-06-18")).AssertStatus(http.StatusBadRequest)
	for _, id := range []string{
		"AAAAAAAAAAAAAAAAAAAAAAAAAA",
		"aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"../../../etc/passwd",
		strings.Repeat("A", 4096),
		"A",
		"AAAAAAAAAAAAAAAAAAAAAAAAA1",
	} {
		m.post(ping, testclient.Header(muzak.HeaderMCPSessionID, id)).AssertStatus(http.StatusNotFound)
	}
	m.initialize("2025-06-18")
	res := m.post(m.message("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{},
	}))
	res.AssertStatus(http.StatusBadRequest)
}

// TestMCPSessionsAreBounded evicts the session idle longest once MaxSessions
// are open, whose client is then told 404, and keeps the rest.
func TestMCPSessionsAreBounded(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{MaxSessions: 3}))
	clients := make([]*mcpClient, 4)
	for i := range clients {
		clients[i] = m.fork()
		clients[i].initialize("2025-06-18")
		if i == 1 {
			// Used again, so the first is now the one idle longest.
			clients[0].send(http.StatusOK, "ping", nil)
		}
	}
	clients[1].post(clients[1].message("ping", nil)).AssertStatus(http.StatusNotFound)
	for _, i := range []int{0, 2, 3} {
		clients[i].send(http.StatusOK, "ping", nil)
	}
}

// TestMCPSessionsUnderConcurrency opens, uses and ends sessions from many
// goroutines at once, which the race detector checks.
func TestMCPSessionsUnderConcurrency(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{MaxSessions: 16}))
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			for range 5 {
				client := m.fork()
				res := client.post(client.message("initialize", map[string]any{
					"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{},
				}))
				if res.Status != http.StatusOK {
					t.Errorf("initialize: %d", res.Status)
					return
				}
				client.session, client.version = res.Header.Get(muzak.HeaderMCPSessionID), "2025-06-18"
				// Another goroutine may have evicted it already, which is
				// a 404 and not a failure.
				if status := client.post(client.message("tools/list", nil)).Status; status != http.StatusOK && status != http.StatusNotFound {
					t.Errorf("tools/list: %d", status)
				}
				client.http.Delete("/mcp", testclient.Header(muzak.HeaderMCPSessionID, client.session))
			}
		})
	}
	wg.Wait()
}

// TestMCPParseErrors answers a body that is not valid JSON with -32700 and a
// null id, in a 400.
func TestMCPParseErrors(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	for _, body := range []string{
		``,
		`{`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"a":1,"a":2}}`,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"p\xffng\"}",
		`{"jsonrpc":"2.0","id":1,"method":"\ud800"}`,
		`nul`,
	} {
		reply := decodeReply(t, m.post(body), http.StatusBadRequest)
		fault(t, reply, -32700)
		if string(reply.ID) != "null" {
			t.Fatalf("%q: id = %s, want null", body, reply.ID)
		}
	}
}

// TestMCPInvalidRequests answers what is JSON but not a JSON-RPC 2.0 message
// with -32600 and a null id, in a 400, a batch among them: batches were
// removed in 2025-06-18.
func TestMCPInvalidRequests(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	for _, body := range []string{
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`,
		`[]`,
		`"ping"`,
		`42`,
		`null`,
		`{}`,
		`{"id":1,"method":"ping"}`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":2.0,"id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":[1],"method":"ping"}`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":7}`,
		`{"jsonrpc":"2.0","id":1,"method":null}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","extra":1}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"x"}}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","result":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":"broken"}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"params":{}}`,
		`{"JSONRPC":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"` + strings.Repeat("x", 300) + `","method":"ping"}`,
	} {
		reply := decodeReply(t, m.post(body), http.StatusBadRequest)
		e := fault(t, reply, -32600)
		if string(reply.ID) != "null" {
			t.Fatalf("%.80s: id = %s, want null", body, reply.ID)
		}
		if strings.HasPrefix(body, "[") && !strings.Contains(e.Message, "batch") {
			t.Fatalf("a batch is refused with %q, which does not say why", e.Message)
		}
	}
	// A notification has no id to answer with, so params that are not an
	// object refuse it whole.
	reply := decodeReply(t, m.post(`{"jsonrpc":"2.0","method":"notifications/initialized","params":"text"}`), http.StatusBadRequest)
	fault(t, reply, -32602)
}

// TestMCPInvalidParams answers params of the wrong shape with -32602 and the
// request's own id, which is echoed exactly, string or number.
func TestMCPInvalidParams(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	for body, id := range map[string]string{
		`{"jsonrpc":"2.0","id":"abc","method":"ping","params":[]}`:                                   `"abc"`,
		`{"jsonrpc":"2.0","id":-12.5e3,"method":"tools/list","params":7}`:                            `-12.5e3`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{}}`:                                 `3`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":5}}`:                         `4`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"no_such_tool"}}`:            `5`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_item","arguments":[]}}`: `6`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_item","arguments":3}}`:  `7`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{"cursor":"bogus"}}`:                 `8`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{"cursor":42}}`:                      `9`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call"}`:                                            `10`,
	} {
		reply := decodeReply(t, m.post(body), http.StatusOK)
		fault(t, reply, -32602)
		if string(reply.ID) != id {
			t.Fatalf("%s: id = %s, want %s", body, reply.ID, id)
		}
	}
}

// TestMCPInternalErrorSaysNothing answers a failure inside the endpoint, here
// an error renderer that panics while a tool's failure is rendered, with
// -32603 and nothing of the panic.
func TestMCPInternalErrorSaysNothing(t *testing.T) {
	options := quietMCPOptions()
	options.ErrorRenderer = func(ctx *muzak.Context, err error) (int, any) {
		if ctx.Route() == nil {
			panic("secret renderer state at 0xdeadbeef")
		}
		return muzak.DefaultErrorRenderer(ctx, err)
	}
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/items/{id}", func(*muzak.Context, getItemIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	res := m.post(m.message("tools/call", map[string]any{"name": "get_items_by_id", "arguments": map[string]any{"bogus": 1}}))
	reply := decodeReply(t, res, http.StatusInternalServerError)
	e := fault(t, reply, -32603)
	if e.Message != "Internal error" || strings.Contains(res.String(), "secret") || strings.Contains(res.String(), "deadbeef") {
		t.Fatalf("the internal error says too much: %s", res.Body)
	}
	// The endpoint still answers afterwards.
	m.send(http.StatusOK, "ping", nil)
}

// TestMCPMessageBounds refuses a message over the endpoint's body limit with
// 413, whether its length is declared or not, and one nested deeper than the
// bound with -32600, while one at the bound is read.
func TestMCPMessageBounds(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}, muzak.MaxBodySize(512)))
	m.initialize("2025-06-18")
	padding := strings.Repeat(" ", 600)
	m.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + padding).AssertStatus(http.StatusRequestEntityTooLarge)
	chunked := testclient.Body("application/json", struct{ *strings.Reader }{strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + padding)})
	m.post("", chunked).AssertStatus(http.StatusRequestEntityTooLarge)

	deep := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	deep.initialize("2025-06-18")
	nested := func(depth int) string {
		return `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":` + strings.Repeat("[", depth-2) + strings.Repeat("]", depth-2) + `}}`
	}
	deep.post(nested(128)).AssertStatus(http.StatusOK)
	fault(t, decodeReply(t, deep.post(nested(129)), http.StatusBadRequest), -32600)
	// Brackets inside a string are text, not nesting.
	deep.post(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("[", 500) + `\"[{"}}`).AssertStatus(http.StatusOK)
	fault(t, decodeReply(t, deep.post(strings.Repeat("[", 10000)+strings.Repeat("]", 10000)), http.StatusBadRequest), -32600)
}

// TestMCPOriginBlocksDNSRebinding is the attack the transport's Origin check
// exists for: a page served from the attacker's name, made to resolve to this
// server, sends that name as Host and Origin alike. Without a Host the
// application vouches for, that is refused; the server's own origin under
// localhost or an address, and under AllowedHosts, is not.
func TestMCPOriginBlocksDNSRebinding(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	port := m.http.URL()[strings.LastIndex(m.http.URL(), ":"):]
	body := m.message("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{}})
	rebound := m.post(body, testclient.Header("Host", "attacker.example"+port), testclient.Header("Origin", "http://attacker.example"+port))
	rebound.AssertStatus(http.StatusForbidden).AssertErrorCode(muzak.CodeForbidden)
	if rebound.Header.Get(muzak.HeaderMCPSessionID) != "" {
		t.Fatal("a rebound page opened a session")
	}
	for _, origin := range []string{"http://localhost" + port, "http://evil.example", "null", "http://127.0.0.1" + port + "/", "http://user@127.0.0.1" + port} {
		m.post(body, testclient.Header("Origin", origin)).AssertStatus(http.StatusForbidden)
	}
	m.post(body, testclient.Header("Origin", "http://127.0.0.1"+port)).AssertStatus(http.StatusOK)
	m.post(body, testclient.Header("Host", "localhost"+port), testclient.Header("Origin", "http://localhost"+port)).AssertStatus(http.StatusOK)
	m.post(body).AssertStatus(http.StatusOK)

	// DELETE is held to the same check.
	m.initialize("2025-06-18")
	m.http.Delete("/mcp", testclient.Header(muzak.HeaderMCPSessionID, m.session), testclient.Header("Origin", "http://evil.example")).
		AssertStatus(http.StatusForbidden)
	m.send(http.StatusOK, "ping", nil)
}

// TestMCPOriginUnderAllowedHosts counts the server's own origin as allowed
// for a name AllowedHosts lists, whose Host the edge has already checked.
func TestMCPOriginUnderAllowedHosts(t *testing.T) {
	options := quietMCPOptions()
	options.AllowedHosts = []string{"api.example.com", "127.0.0.1"}
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	m := newMCPClient(t, app)
	body := m.message("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{}})
	m.post(body, testclient.Header("Host", "api.example.com"), testclient.Header("Origin", "https://api.example.com")).AssertStatus(http.StatusOK)
	m.post(body, testclient.Header("Host", "api.example.com"), testclient.Header("Origin", "https://other.example.com")).AssertStatus(http.StatusForbidden)
	m.post(body, testclient.Header("Host", "attacker.example"), testclient.Header("Origin", "http://attacker.example")).AssertStatus(http.StatusMisdirectedRequest)
}

// TestMCPAllowedOrigins lets the origins listed, and those the function
// allows, call the endpoint, and nobody else.
func TestMCPAllowedOrigins(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{
		AllowedOrigins:  []string{"https://inspector.example.com"},
		AllowOriginFunc: func(_ *http.Request, origin string) bool { return strings.HasSuffix(origin, ".trusted.example") },
	}))
	body := m.message("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{}})
	m.post(body, testclient.Header("Origin", "https://inspector.example.com")).AssertStatus(http.StatusOK)
	m.post(body, testclient.Header("Origin", "https://INSPECTOR.example.com")).AssertStatus(http.StatusOK)
	m.post(body, testclient.Header("Origin", "https://app.trusted.example")).AssertStatus(http.StatusOK)
	m.post(body, testclient.Header("Origin", "https://inspector.example.com.evil")).AssertStatus(http.StatusForbidden)
	m.post(body, testclient.Header("Origin", "https://trusted.example.evil")).AssertStatus(http.StatusForbidden)
}

// TestMCPOptionsAreChecked reports every option that cannot be served.
func TestMCPOptionsAreChecked(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{
		AllowedOrigins:     []string{"*", "null", "https://app.example.com/", "*.example.com"},
		ForwardHeaders:     []string{"X-API-Key", "Host", "bad header", "Content-Type", "Mcp-Session-Id", "Accept"},
		ForwardCookies:     []string{"session", "bad;cookie"},
		MaxResultSize:      -1,
		PageSize:           muzak.MaxMCPPageSize + 1,
		MaxSessions:        -1,
		SessionIdleTimeout: -1,
	})
	err := app.Build()
	if err == nil {
		t.Fatal("Build() succeeded")
	}
	mustContain(t, err.Error(),
		`AllowedOrigins holds "*"`, `"null" is refused`, `ends in a slash`, `is a pattern`,
		`ForwardHeaders holds Host`, `ForwardHeaders holds "bad header"`, `ForwardHeaders holds Content-Type`,
		`ForwardHeaders holds Mcp-Session-Id`, `ForwardHeaders holds Accept`, `ForwardCookies holds "bad;cookie"`,
		`MaxResultSize is -1`, `PageSize is 1001`, `MaxSessions is -1`, `SessionIdleTimeout is -1ns`)
	if strings.Contains(err.Error(), "X-API-Key") {
		t.Fatalf("a valid forwarded header was refused: %v", err)
	}
	big := muzak.New(quietMCPOptions())
	big.MCP("/mcp", muzak.MCPOptions{MaxResultSize: muzak.MaxMCPResultSize + 1, MaxSessions: muzak.MaxMCPSessions + 1})
	mustContain(t, fmt.Sprint(big.Build()), "MaxResultSize is", "MaxSessions is")
}

// TestMCPCalledTwice is a build error, and MCP after the build panics as any
// registration does.
func TestMCPCalledTwice(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.MCP("/other", muzak.MCPOptions{})
	mustContain(t, fmt.Sprint(app.Build()), "App.MCP was called more than once")

	built := muzak.New(quietMCPOptions())
	if err := built.Build(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MCP after Build did not panic")
		}
	}()
	built.MCP("/mcp", muzak.MCPOptions{})
}

// TestMCPEndpointIsNotDocumented leaves the endpoint out of the OpenAPI
// document: the tools it serves are described there already.
func TestMCPEndpointIsNotDocumented(t *testing.T) {
	app := shopApp(muzak.MCPOptions{})
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, documented := doc.Paths["/mcp"]; documented {
		t.Fatal("the MCP endpoint is in the OpenAPI document")
	}
	data, _ := json.Marshal(doc)
	if strings.Contains(string(data), "/mcp") {
		t.Fatal("the OpenAPI document mentions the MCP endpoint")
	}
}
