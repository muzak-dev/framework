package muzak

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// openMCPSession opens a session on an application in-process and returns its
// identifier.
func openMCPSession(t *testing.T, app *App) string {
	t.Helper()
	rec := mcpPost(t, app, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{}}}`)
	assertStatus(t, rec, http.StatusOK)
	return rec.Header().Get(HeaderMCPSessionID)
}

// mcpCall calls a tool in-process and returns its result.
func mcpCall(t *testing.T, app *App, session, tool, args string) mcpCallResultForTest {
	t.Helper()
	rec := mcpPost(t, app, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
	assertStatus(t, rec, http.StatusOK)
	var reply struct {
		Result mcpCallResultForTest `json:"result"`
		Error  *rpcError            `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error != nil {
		t.Fatalf("tools/call: %v %s", err, rec.Body.String())
	}
	return reply.Result
}

// mcpCallResultForTest is a decoded tools/call result.
type mcpCallResultForTest struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
	IsError           bool           `json:"isError"`
}

// text returns a result's only text.
func (r mcpCallResultForTest) text(t *testing.T) string {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("content = %+v", r.Content)
	}
	return r.Content[0].Text
}

// TestMCPEndpointPathIsChecked reports an endpoint path the router refuses,
// with the tools still chosen and checked beside it.
func TestMCPEndpointPathIsChecked(t *testing.T) {
	app := New(quietOptions())
	app.MCP("mcp", MCPOptions{})
	app.Get("/hidden", func(*Context, Empty) (Empty, error) { return Empty{}, nil }, Hidden(), MCPTool())
	err := app.Build()
	if err == nil || !strings.Contains(err.Error(), `POST mcp: path must begin with "/"`) || !strings.Contains(err.Error(), "GET /hidden is declared with MCPTool") {
		t.Fatalf("Build() = %v", err)
	}
}

// TestMCPPathParameterBoundByNothing is a build error: a tool call has nothing
// to write in the parameter's place.
func TestMCPPathParameterBoundByNothing(t *testing.T) {
	app := New(quietOptions())
	app.MCP("/mcp", MCPOptions{})
	app.Get("/x/{id}", func(*Context, Empty) (Empty, error) { return Empty{}, nil }, MCPTool())
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), `the path parameter "id" is bound by no field`) {
		t.Fatalf("Build() = %v", err)
	}
}

// TestMCPArgumentsTheTypeCannotHold answers arguments that fail because of
// the route's type rather than the call as the opaque 500 the route would
// answer, and logs the cause for the developer.
func TestMCPArgumentsTheTypeCannotHold(t *testing.T) {
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.MCP("/mcp", MCPOptions{})
	app.Post("/h", func(*Context, codecHiddenIn) (Empty, error) { return Empty{}, nil }, MCPTool())
	mustBuild(t, app)
	session := openMCPSession(t, app)
	got := mcpCall(t, app, session, "post_h", `{"body":{"x":1}}`)
	if !got.IsError || !strings.Contains(got.text(t), `"code":"internal_error"`) || strings.Contains(got.text(t), "codecHidden") {
		t.Fatalf("result = %+v", got)
	}
	if !strings.Contains(logs.String(), "an MCP tool call failed") || !strings.Contains(logs.String(), "cannot set embedded pointer") {
		t.Fatalf("the cause was not logged:\n%s", logs.String())
	}
	if got := mcpCall(t, app, session, "post_h", `{"body":{"y":1}}`); got.IsError {
		t.Fatalf("a body that does not reach the fault failed: %+v", got)
	}
}

// singleFileIn takes one file and binds the User-Agent and a cookie.
type singleFileIn struct {
	Agent *string `header:"User-Agent"`
	Theme string  `cookie:"theme"`
	Off   bool    `query:"off"`
	Doc   []byte  `file:"doc"`
}

// TestMCPCallEdges covers the corners of writing a call's request: a list
// given for one file, a User-Agent the input binds, sent and left out, a
// forwarded cookie beside the input's own, and false written as itself.
func TestMCPCallEdges(t *testing.T) {
	app := New(quietOptions())
	app.MCP("/mcp", MCPOptions{ForwardCookies: []string{"session"}})
	app.Post("/f", func(ctx *Context, in singleFileIn) (map[string]any, error) {
		_, hasAgent := ctx.Request().Header["User-Agent"]
		return map[string]any{"agent": in.Agent, "has_agent": hasAgent, "cookie": ctx.Request().Header.Get("Cookie"),
			"off": in.Off, "doc": string(in.Doc)}, nil
	}, MCPTool())
	mustBuild(t, app)
	session := openMCPSession(t, app)
	call := func(args string, cookie string) mcpCallResultForTest {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"post_f","arguments":`+args+`}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set(HeaderMCPSessionID, session)
		req.Header.Set("User-Agent", "outer/1")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		rec := doRequest(t, app, req)
		var reply struct {
			Result mcpCallResultForTest `json:"result"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &reply)
		return reply.Result
	}
	doc := base64.StdEncoding.EncodeToString([]byte("pdf"))
	if got := call(`{"form":{"doc":["`+doc+`"]}}`, ""); !got.IsError || !strings.Contains(got.text(t), "standard base64") {
		t.Fatalf("a list for one file: %+v", got)
	}
	got := call(`{"form":{"doc":"`+doc+`"},"header":{"User-Agent":"inner/2"},"cookie":{"theme":"dark"},"query":{"off":false}}`, "session=s1; other=o")
	want := map[string]any{"agent": "inner/2", "has_agent": true, "cookie": "theme=dark; session=s1", "off": false, "doc": "pdf"}
	g, _ := json.Marshal(got.StructuredContent, json.Deterministic(true))
	w, _ := json.Marshal(want, json.Deterministic(true))
	if string(g) != string(w) {
		t.Fatalf("got %s, want %s", g, w)
	}
	got = call(`{"form":{"doc":"`+doc+`"}}`, "")
	if got.StructuredContent["has_agent"] != false || got.StructuredContent["cookie"] != "theme=" {
		t.Fatalf("a User-Agent the input binds and the call left out arrived: %+v", got)
	}
}

// TestMCPCallAnsweredByNothing refuses a call that something above routing
// answered by writing nothing at all, which net/http would send as an empty
// 200.
func TestMCPCallAnsweredByNothing(t *testing.T) {
	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/void") {
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	app.MCP("/mcp", MCPOptions{})
	app.Get("/files/{rest...}", func(*Context, struct {
		Rest string `path:"rest"`
	}) (Empty, error) {
		return Empty{}, nil
	}, MCPTool())
	mustBuild(t, app)
	session := openMCPSession(t, app)
	got := mcpCall(t, app, session, "get_files_by_rest", `{"path":{"rest":"a/void"}}`)
	if !got.IsError || !strings.Contains(got.text(t), "does not answer") {
		t.Fatalf("result = %+v", got)
	}
}

// oddMediaType writes a body under a Content-Type that does not parse.
func oddMediaType(ctx *Context, _ Empty) (Empty, error) {
	ctx.ResponseWriter().Header().Set("Content-Type", "not a type;;")
	_, err := ctx.ResponseWriter().Write([]byte("?"))
	return Empty{}, err
}

// TestMCPErrorRendererWithoutBody renders an error result as the status text
// when the application's renderer answers errors with no body, and refuses a
// body whose media type does not parse.
func TestMCPErrorRendererWithoutBody(t *testing.T) {
	options := quietOptions()
	options.ErrorRenderer = func(_ *Context, err error) (int, any) {
		var coder StatusCoder
		if errors.As(err, &coder) {
			return coder.HTTPStatus(), nil
		}
		return http.StatusInternalServerError, nil
	}
	app := New(options)
	app.MCP("/mcp", MCPOptions{})
	app.Get("/odd", oddMediaType, MCPTool())
	mustBuild(t, app)
	session := openMCPSession(t, app)
	if got := mcpCall(t, app, session, "get_odd", `{"bogus":1}`); got.text(t) != "Unprocessable Entity" {
		t.Fatalf("result = %+v", got)
	}
	if got := mcpCall(t, app, session, "get_odd", `{}`); got.text(t) != "Bad Gateway" {
		t.Fatalf("result = %+v", got)
	}
	plain := New(quietOptions())
	plain.MCP("/mcp", MCPOptions{})
	plain.Get("/odd", oddMediaType, MCPTool())
	mustBuild(t, plain)
	if got := mcpCall(t, plain, openMCPSession(t, plain), "get_odd", `{}`); !strings.Contains(got.text(t), "a body of no known type") {
		t.Fatalf("result = %+v", got)
	}
}

// badNode contains itself and documents a member with text that is not UTF-8.
type badNode struct {
	Name string    `json:"name" doc:"broken \xff"`
	Next []badNode `json:"next"`
}

// TestMCPRecursiveSchemaThatCannotBeEncoded reports a tool whose $defs hold
// text that is not UTF-8.
func TestMCPRecursiveSchemaThatCannotBeEncoded(t *testing.T) {
	options := quietOptions()
	options.DisableDocs = true
	app := New(options)
	app.MCP("/mcp", MCPOptions{})
	app.Post("/n", func(*Context, Empty) (badNode, error) { return badNode{}, nil }, MCPTool())
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "cannot be encoded as JSON") {
		t.Fatalf("Build() = %v", err)
	}
}

// TestMCPReleasesThatFail renders the failure of the endpoint's own release
// in place of the answer it was about to send, for a request, a notification
// and a DELETE alike.
func TestMCPReleasesThatFail(t *testing.T) {
	failing := Acquire(func(*Context) (*strings.Builder, Release, error) {
		return &strings.Builder{}, func(failure error) error {
			if failure == nil {
				return errors.New("the commit failed")
			}
			return nil
		}, nil
	})
	app := New(quietOptions())
	app.MCP("/mcp", MCPOptions{}, failing)
	mustBuild(t, app)
	assertStatus(t, mcpPost(t, app, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{}}}`),
		http.StatusInternalServerError)
	session := app.mcp.sessions.create(mcpVersion20250618, [32]byte{})
	assertStatus(t, mcpPost(t, app, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`), http.StatusInternalServerError)
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(HeaderMCPSessionID, session)
	assertStatus(t, doRequest(t, app, req), http.StatusInternalServerError)
	twice := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	twice.Header.Add(HeaderMCPSessionID, session)
	twice.Header.Add(HeaderMCPSessionID, session)
	assertStatus(t, doRequest(t, app, twice), http.StatusBadRequest)
}

// failingBody is a request body that breaks after some bytes.
type failingBody struct{ sent bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `{"jsonrpc":`), nil
	}
	return 0, errors.New("connection reset")
}

// TestMCPMessageReading bounds a message by the default body limit when the
// application removed its own, and refuses one whose body cannot be read.
func TestMCPMessageReading(t *testing.T) {
	options := quietOptions()
	options.MaxBodySize = -1
	app := New(options)
	app.MCP("/mcp", MCPOptions{})
	mustBuild(t, app)
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("x", int(DefaultMaxBodySize)) + `"}}`
	assertStatus(t, mcpPost(t, app, "", big), http.StatusRequestEntityTooLarge)
	req := httptest.NewRequest(http.MethodPost, "/mcp", io.NopCloser(&failingBody{}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	assertStatus(t, doRequest(t, app, req), http.StatusBadRequest)
}
