package muzak

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FuzzMCPMessage reads arbitrary bytes as a JSON-RPC message and holds the
// reader to its promises: no panic, a message only of a known kind, an id
// only a short string or number, params only an object, and an answer that is
// always valid JSON whatever was read.
func FuzzMCPMessage(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"a","arguments":{"path":{"id":"1"}}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"x"}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":[1]}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":[[[[[["]"]]]]]}}`,
		`{"jsonrpc":"2.0","id":1e400,"method":"ping"}`,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"\\ud800\"}",
		``, `null`, `"x"`, `{`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, rerr := parseRPCMessage(data)
		if rerr != nil {
			if rerr.Code > rpcParseError+100 || rerr.Code < rpcParseError || rerr.Message == "" {
				t.Fatalf("error %+v", rerr)
			}
			if msg != nil && (rerr != errRPCParamsObject || msg.kind == rpcResponse) {
				t.Fatalf("a message %+v came back beside %+v", msg, rerr)
			}
		}
		var id jsontext.Value
		if msg != nil {
			switch msg.kind {
			case rpcRequest, rpcNotification:
				if !utf8String(msg.method) {
					t.Fatalf("method %q", msg.method)
				}
			case rpcResponse:
			default:
				t.Fatalf("kind %d", msg.kind)
			}
			if msg.id != nil {
				if kind := msg.id.Kind(); (kind != '"' && kind != '0') || len(msg.id) > maxRPCIDLength {
					t.Fatalf("id %s", msg.id)
				}
			}
			if (msg.kind == rpcNotification) == (msg.id != nil) || (msg.kind == rpcResponse && (msg.id == nil || msg.method != "")) {
				t.Fatalf("the kind does not match the members: %+v", msg)
			}
			if msg.params != nil && msg.params.Kind() != '{' {
				t.Fatalf("params %s", msg.params)
			}
			id = msg.id
		}
		if rerr == nil {
			rerr = &rpcError{Code: rpcInternalError, Message: "x"}
		}
		for _, omit := range []bool{false, true} {
			if answer := encodeRPCError(id, rerr, omit); !jsontext.Value(answer).IsValid() {
				t.Fatalf("the answer is not valid JSON: %s", answer)
			}
		}
	})
}

// utf8String reports whether s came from a JSON string, which is valid UTF-8.
func utf8String(s string) bool { return strings.ToValidUTF8(s, "\x00") == s }

// mcpFuzzIn is an input with a field in every part of a request, and defaults.
type mcpFuzzIn struct {
	ID     string   `path:"id" json:"id"`
	Limit  int      `query:"limit" default:"5" json:"limit"`
	Tags   []string `query:"tag" json:"tags"`
	Flag   *bool    `query:"flag" json:"flag"`
	Ratio  float64  `query:"ratio" json:"ratio"`
	Tenant string   `header:"X-Tenant" json:"tenant"`
	Labels []string `header:"X-Labels" json:"labels"`
	Theme  string   `cookie:"theme" json:"theme"`
	Name   string   `json:"name"`
	Count  int      `json:"count" default:"2"`
	Wait   time.Duration
	Nested struct {
		A []int `json:"a"`
	} `json:"nested"`
}

// mcpFuzzOut echoes everything the route bound.
type mcpFuzzOut struct {
	ID     string        `json:"id"`
	Limit  int           `json:"limit"`
	Tags   []string      `json:"tags"`
	Flag   *bool         `json:"flag"`
	Ratio  float64       `json:"ratio"`
	Tenant string        `json:"tenant"`
	Labels []string      `json:"labels"`
	Theme  string        `json:"theme"`
	Name   string        `json:"name"`
	Count  int           `json:"count"`
	Wait   time.Duration `json:"wait"`
	A      []int         `json:"a"`
}

// echo turns a bound input into what the route answers.
func (in mcpFuzzIn) echo() mcpFuzzOut {
	return mcpFuzzOut{ID: in.ID, Limit: in.Limit, Tags: in.Tags, Flag: in.Flag, Ratio: in.Ratio, Tenant: in.Tenant,
		Labels: in.Labels, Theme: in.Theme, Name: in.Name, Count: in.Count, Wait: in.Wait, A: in.Nested.A}
}

// FuzzMCPArguments decodes arbitrary arguments for a tool whose input has a
// field in every part of a request. Whatever comes in, decoding does not
// panic and fails with a bounded validation error; and arguments that decode
// and that the encoder writes reach the route as exactly the value they
// decoded to, through the whole application.
func FuzzMCPArguments(f *testing.F) {
	app := New(quietOptions())
	app.MCP("/mcp", MCPOptions{})
	app.Post("/things/{id}", func(_ *Context, in mcpFuzzIn) (mcpFuzzOut, error) { return in.echo(), nil }, MCPTool())
	if err := app.Build(); err != nil {
		f.Fatal(err)
	}
	tool := app.mcp.byName["post_things_by_id"]
	for _, seed := range []string{
		`{}`,
		`{"path":{"id":"a/b"},"query":{"limit":3,"tag":["x","y"],"flag":true,"ratio":1.5e3},"header":{"X-Tenant":"t","X-Labels":["a","b"]},"cookie":{"theme":"dark mode"},"body":{"name":"n","Wait":"1s","nested":{"a":[1,2]}}}`,
		`{"path":{"id":"x"},"body":{"count":null}}`,
		`{"path":{"id":"x"},"query":{"tag":[]},"body":{}}`,
		`{"path":{"id":"\u00e9 %2F?#"},"cookie":{"theme":"a,b"},"body":{"name":"\u0000"}}`,
		`{"path":{"id":"x"},"header":{"X-Labels":["a,b"]},"body":{}}`,
		`{"path":5,"bogus":1}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		args := jsontext.Value(data)
		if !args.IsValid() || jsonDepthExceeds(data, maxRPCDepth) || (args.Kind() != '{' && args.Kind() != 'n') {
			// The transport refuses all of these before a tool is called.
			return
		}
		v, err := tool.decode(args)
		var verr *ValidationError
		switch {
		case err == nil:
		case errors.As(err, &verr):
			if len(verr.Details) == 0 || len(verr.Details) > maxMCPArgumentIssues {
				t.Fatalf("%d details", len(verr.Details))
			}
			return
		default:
			t.Fatalf("decoding failed with %v", err)
		}
		encoded, err := tool.plan.encode(v, &callConfig{})
		if err != nil {
			if !errors.Is(err, ErrCallRefused) {
				t.Fatalf("encoding failed with %v", err)
			}
			return
		}
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		w := asResponseWriter(httptest.NewRecorder())
		c := app.acquire(w, req)
		defer app.release(c)
		result := app.mcp.serveTool(c, tool, encoded, eraStateless)
		if result.IsError {
			// The binder may refuse what the arguments decoded to only for a
			// reason the route reports, which is a result with its envelope.
			if !strings.Contains(result.Content[0].(mcpTextContent).Text, `"status":422`) {
				t.Fatalf("the route refused decoded arguments: %+v", result.Content)
			}
			return
		}
		want, _ := json.Marshal(v.Interface().(mcpFuzzIn).echo(), durationJSON)
		var got, expected any
		_ = json.Unmarshal(result.StructuredContent, &got)
		_ = json.Unmarshal(want, &expected)
		g, _ := json.Marshal(got, json.Deterministic(true))
		e, _ := json.Marshal(expected, json.Deterministic(true))
		if !bytes.Equal(g, e) {
			t.Fatalf("the route bound another value\n got: %s\nwant: %s", g, e)
		}
	})
}

// FuzzMCPTransportHeaders reads arbitrary header values the way the transport
// does: an Accept list, a header value in base64, a session identifier and a
// cursor, none of which may panic, and none of which forges a cursor.
func FuzzMCPTransportHeaders(f *testing.F) {
	s := &mcpServer{tools: make([]*mcpTool, 10)}
	s.cursorKey[0] = 1
	sessions := newMCPSessions(4, time.Hour)
	issued := sessions.create(mcpVersion20250618, [32]byte{})
	for _, seed := range []string{"application/json;q=0.5", "*/*", "=?base64?aGk=?=", issued, s.cursor(3), "", "\x00\xff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		acceptsJSON([]string{value})
		if decoded := decodeMCPHeaderValue(value); strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") && len(value) >= 11 &&
			decoded != "" && !utf8String(decoded) {
			t.Fatalf("decoded %q to text that is not UTF-8", value)
		}
		if _, found := sessions.lookup(value); found != (value == issued) {
			t.Fatalf("lookup(%q) = %t", value, found)
		}
		if index, ok := s.pageStart(value); ok {
			if value != s.cursor(index) {
				t.Fatalf("pageStart accepted %q, which is not the cursor it issues for %d", value, index)
			}
		}
	})
}
