package muzak_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// TestMCPNothingIsAToolByDefault serves no tool until one is chosen, however
// many routes the application has.
func TestMCPNothingIsAToolByDefault(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/items/{id}", func(*muzak.Context, getItemIn) (shopItem, error) { return shopItem{}, nil }, muzak.WithTags("agent"))
	app.Post("/items", func(*muzak.Context, createItemIn) (createdItem, error) { return createdItem{}, nil })
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	if tools := m.listAll(); len(tools) != 0 {
		t.Fatalf("tools = %+v, want none", tools)
	}
	fault(t, m.send(http.StatusOK, "tools/call", map[string]any{"name": "get_items_by_id"}), -32602)
}

// TestMCPToolSelection chooses a route by MCPTool, by a tag and by Include,
// which adds up, and asks Include only about routes that can be tools.
func TestMCPToolSelection(t *testing.T) {
	var asked []string
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{
		Tags: []string{"agent"},
		Include: func(rt *muzak.Route) bool {
			asked = append(asked, rt.OperationID)
			return strings.HasPrefix(rt.Path, "/reports")
		},
	})
	empty := func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil }
	app.Get("/explicit", empty, muzak.MCPTool())
	app.Get("/tagged", empty, muzak.WithTags("other", "agent"))
	app.Get("/reports/daily", empty)
	app.Get("/plain", empty, muzak.WithTags("other"))
	app.Get("/hidden", empty, muzak.Hidden())
	app.WS("/socket", func(*muzak.Context, muzak.Empty, *muzak.WSConn) error { return nil })
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	var names []string
	for _, tool := range m.listAll() {
		names = append(names, tool.Name)
	}
	if want := []string{"get_explicit", "get_reports_daily", "get_tagged"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	slices.Sort(asked)
	if want := []string{"get_plain", "get_reports_daily"}; !slices.Equal(asked, want) {
		t.Fatalf("Include was asked about %v, want only the routes that can be tools and were not chosen already: %v", asked, want)
	}
}

// TestMCPExcludedKindsAreNeverTools lists none of what can never be a tool,
// however eagerly the options choose: a WebSocket or event stream route, a
// hidden one, a mount, static files, a frontend, the documentation, the
// health endpoints, the RFC 9728 metadata route, a method OpenAPI has no
// place for, and the endpoint itself.
func TestMCPExcludedKindsAreNeverTools(t *testing.T) {
	options := quietMCPOptions()
	options.Health = muzak.HealthOptions{Enabled: true}
	options.SecuritySchemes = map[string]muzak.SecurityScheme{"bearer": muzak.JWTBearer(muzak.JWTOptions{
		Issuers:          []string{"https://issuer.example.com"},
		Audience:         "https://api.example.com/mcp",
		Algorithms:       []string{"HS256"},
		Keys:             []muzak.JWTKey{{Secret: []byte(strings.Repeat("k", 32))}},
		ResourceMetadata: &muzak.ResourceMetadata{Resource: "https://api.example.com/mcp"},
	})}
	app := muzak.New(options, muzak.WithTags("agent"))
	app.MCP("/mcp", muzak.MCPOptions{Tags: []string{"agent"}, Include: func(*muzak.Route) bool { return true }})
	empty := func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil }
	app.Get("/visible", empty)
	app.Get("/hidden", empty, muzak.Hidden())
	app.Handle("PROPFIND", "/dav", empty)
	app.WS("/socket", func(*muzak.Context, muzak.Empty, *muzak.WSConn) error { return nil })
	app.SSE("/events", func(*muzak.Context, muzak.Empty, *muzak.SSEStream[shopItem]) error { return nil })
	app.Mount("/legacy", http.NotFoundHandler())
	app.Static("/assets", muzak.StaticOptions{FS: fstest.MapFS{"a.txt": {Data: []byte("a")}}})
	app.Frontend("/app", muzak.FrontendOptions{FS: fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}})
	m := newMCPClient(t, app)
	m.initialize("2025-11-25")
	tools := m.listAll()
	if len(tools) != 1 || tools[0].Name != "get_visible" {
		t.Fatalf("tools = %+v, want get_visible alone", tools)
	}
}

// TestMCPToolDeclaredOnWhatCannotBeOne is a build error naming the route and
// why.
func TestMCPToolDeclaredOnWhatCannotBeOne(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{}, muzak.MCPTool())
	empty := func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil }
	app.Get("/hidden", empty, muzak.Hidden(), muzak.MCPTool())
	app.Handle("PROPFIND", "/dav", empty, muzak.MCPTool())
	app.WS("/socket", func(*muzak.Context, muzak.Empty, *muzak.WSConn) error { return nil }, muzak.MCPTool())
	app.SSE("/events", func(*muzak.Context, muzak.Empty, *muzak.SSEStream[shopItem]) error { return nil }, muzak.MCPTool())
	mustContain(t, fmt.Sprint(app.Build()),
		"POST /mcp is declared with MCPTool, but it is the MCP endpoint itself",
		"GET /hidden is declared with MCPTool, but it is hidden",
		"PROPFIND /dav is declared with MCPTool, but its method has no place in the OpenAPI document",
		"GET /socket is declared with MCPTool, but it is a WebSocket route",
		"GET /events is declared with MCPTool, but it is an event stream")
}

// TestMCPToolNames names a tool after its operation id, cut to what a tool
// name may hold, and refuses two tools that end up with one name.
func TestMCPToolNames(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	empty := func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil }
	app.Get("/a", empty, muzak.MCPTool(), muzak.OperationID("orders.list v2/all\u00e9"))
	app.Get("/b", empty, muzak.MCPTool(), muzak.OperationID(strings.Repeat("x", 100)))
	app.Get("/c", empty, muzak.MCPTool(), muzak.OperationID("Mixed-Case_9"))
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	var names []string
	for _, tool := range m.listAll() {
		names = append(names, tool.Name)
	}
	if want := []string{"Mixed-Case_9", "orders_list_v2_all_", strings.Repeat("x", 64)}; !slices.Equal(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}

	clash := muzak.New(quietMCPOptions())
	clash.MCP("/mcp", muzak.MCPOptions{})
	clash.Get("/a", empty, muzak.MCPTool(), muzak.OperationID("list.items"))
	clash.Get("/b", empty, muzak.MCPTool(), muzak.OperationID("list items"))
	mustContain(t, fmt.Sprint(clash.Build()), `GET /a and GET /b are both MCP tools named "list_items"`)
}

// fileIn has a file field a tool call cannot fill.
type fileIn struct {
	Doc muzak.File `file:"doc"`
}

// authHeaderIn binds the Authorization header, which a tool call forwards.
type authHeaderIn struct {
	Auth string `header:"Authorization"`
}

// forwardedIn binds a header the client address is read from.
type forwardedIn struct {
	For string `header:"X-Forwarded-For"`
}

// apiKeyIn binds a header the options forward.
type apiKeyIn struct {
	Key string `header:"X-API-Key"`
}

// sessionCookieIn binds a cookie the options forward.
type sessionCookieIn struct {
	Session string `cookie:"session"`
}

// TestMCPUncallableInputs is a build error for each route whose input a tool
// call cannot write, or that binds what the call forwards.
func TestMCPUncallableInputs(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{ForwardHeaders: []string{"x-api-key"}, ForwardCookies: []string{"session"}})
	app.Post("/upload", func(*muzak.Context, fileIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	app.Get("/auth", func(*muzak.Context, authHeaderIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	app.Get("/forwarded", func(*muzak.Context, forwardedIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	app.Get("/key", func(*muzak.Context, apiKeyIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	app.Get("/cookie", func(*muzak.Context, sessionCookieIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	mustContain(t, fmt.Sprint(app.Build()),
		"POST /upload: field Doc is a muzak.File",
		"GET /auth: the input binds the header Authorization, which a tool call forwards",
		"GET /forwarded: the input binds the header X-Forwarded-For, which a tool call forwards",
		"GET /key: the input binds the header X-Api-Key, which a tool call forwards",
		`GET /cookie: field Session binds the cookie "session", which MCPOptions.ForwardCookies forwards`,
		"it is chosen as an MCP tool")
}

// queryKeyIn binds a query parameter an API key scheme reads its key from.
type queryKeyIn struct {
	Key string `query:"api_key"`
}

// browserSessionIn binds the cookie the application's sessions are kept in.
type browserSessionIn struct {
	Session string `cookie:"__Host-session"`
}

// TestMCPInputCannotBindACredential is a build error for a route whose input
// binds a header, query parameter or cookie a security scheme or the session
// reads a credential from, even one the endpoint does not forward: a model
// would choose the credential the call is made with, and present a key it
// read somewhere as its own. A route reading none of them is a tool.
func TestMCPInputCannotBindACredential(t *testing.T) {
	options := quietMCPOptions()
	options.Sessions = &muzak.SessionOptions{Secrets: []string{"0123456789abcdef0123456789abcdef"}}
	options.CrossOriginProtection = &muzak.CrossOriginOptions{}
	options.SecuritySchemes = map[string]muzak.SecurityScheme{
		"header": muzak.APIKeyVerifier(muzak.APIKeyOptions{Name: "x-api-key", Keys: []muzak.APIKey{{ID: "alice", Key: "alice-key-0123456789"}}}),
		"query":  muzak.APIKeyVerifier(muzak.APIKeyOptions{In: "query", Name: "api_key", Keys: []muzak.APIKey{{ID: "alice", Key: "alice-key-0123456789"}}}),
		"cookie": muzak.APIKeyCookie("sid"),
	}
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/header", func(*muzak.Context, apiKeyIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	app.Get("/query", func(*muzak.Context, queryKeyIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	app.Get("/cookie", func(*muzak.Context, struct {
		ID string `cookie:"sid"`
	}) (shopItem, error) {
		return shopItem{}, nil
	}, muzak.MCPTool())
	app.Get("/session", func(*muzak.Context, browserSessionIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	app.Get("/other", func(*muzak.Context, sessionCookieIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	err := fmt.Sprint(app.Build())
	mustContain(t, err,
		`GET /header: field Key binds the header X-Api-Key, which the security scheme "header" reads a credential from`,
		`GET /query: field Key binds the query parameter "api_key", which the security scheme "query" reads a credential from`,
		`GET /cookie: field ID binds the cookie "sid", which the security scheme "cookie" reads a credential from`,
		`GET /session: field Session binds the cookie "__Host-session", which AppOptions.Sessions reads a credential from`,
		"a tool call's credentials come from the client's request and never from a model")
	if strings.Contains(err, "GET /other") {
		t.Fatalf("a cookie no credential is read from was refused:\n%s", err)
	}
}

// TestMCPToolsAreDescribedAsTheDocumentDescribesThem lists a tool's
// description, title and annotations from the route, and its input and output
// schemas from the OpenAPI document's, every reference resolved.
func TestMCPToolsAreDescribedAsTheDocumentDescribesThem(t *testing.T) {
	app := shopApp(muzak.MCPOptions{})
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	tools := m.listAll()

	get := toolNamed(t, tools, "get_item")
	if get.Title != "Get Item" || get.Description != "Fetch an item\n\nReads one item by its id." {
		t.Fatalf("title %q, description %q", get.Title, get.Description)
	}
	if get.Annotations == nil || get.Annotations.ReadOnlyHint == nil || !*get.Annotations.ReadOnlyHint {
		t.Fatalf("GET is not read-only: %+v", get.Annotations)
	}
	operation := doc.Paths["/items/{id}"].Get
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Properties map[string]jsontext.Value `json:"properties"`
			Required   []string                  `json:"required"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(get.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.AdditionalProperties || !slices.Equal(schema.Required, []string{"path"}) {
		t.Fatalf("input schema root = %s", get.InputSchema)
	}
	for _, parameter := range operation.Parameters {
		described := *parameter.Schema
		if described.Description == "" {
			described.Description = parameter.Description
		}
		jsonEqual(t, schema.Properties[parameter.In].Properties[parameter.Name], described)
		if parameter.Required != slices.Contains(schema.Properties[parameter.In].Required, parameter.Name) {
			t.Fatalf("%s %s: required differs from the document", parameter.In, parameter.Name)
		}
	}
	jsonEqual(t, get.OutputSchema, resolveRefs(t, doc, operation.Responses["200"].Content["application/json"].Schema))

	create := toolNamed(t, tools, "create_item")
	created := doc.Paths["/items"].Post
	var body struct {
		Properties map[string]jsontext.Value `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(create.InputSchema, &body); err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body.Properties["body"], resolveRefs(t, doc, created.RequestBody.Content["application/json"].Schema))
	if !slices.Equal(body.Required, []string{"body", "header"}) {
		t.Fatalf("required = %v, want body and header", body.Required)
	}
	jsonEqual(t, create.OutputSchema, resolveRefs(t, doc, created.Responses["201"].Content["application/json"].Schema))
	if create.Annotations != nil {
		t.Fatalf("POST promises nothing, but is annotated %+v", create.Annotations)
	}

	remove := toolNamed(t, tools, "delete_item")
	if remove.Annotations == nil || !*remove.Annotations.DestructiveHint || !*remove.Annotations.IdempotentHint || remove.Annotations.ReadOnlyHint != nil {
		t.Fatalf("DELETE annotations = %+v", remove.Annotations)
	}
	if remove.OutputSchema != nil {
		t.Fatalf("a route answering 204 has an output schema: %s", remove.OutputSchema)
	}
}

// resolveRefs is an independent reading of a document schema with every
// reference replaced by the component it names, for comparison with a tool's.
func resolveRefs(t *testing.T, doc *muzak.Document, schema *muzak.Schema) any {
	t.Helper()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	components, _ := json.Marshal(doc.Components.Schemas)
	var byName map[string]any
	_ = json.Unmarshal(components, &byName)
	var value any
	_ = json.Unmarshal(data, &value)
	var walk func(v any) any
	walk = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for key, item := range v {
				out[key] = walk(item)
			}
			ref, isRef := out["$ref"].(string)
			if !isRef {
				return out
			}
			delete(out, "$ref")
			target := walk(byName[strings.TrimPrefix(ref, "#/components/schemas/")]).(map[string]any)
			if len(out) == 0 {
				return target
			}
			if _, annotationOnly := out["description"]; annotationOnly && len(out) == 1 {
				target["description"] = out["description"]
				return target
			}
			allOf, _ := out["allOf"].([]any)
			out["allOf"] = append([]any{target}, allOf...)
			return out
		case []any:
			for i := range v {
				v[i] = walk(v[i])
			}
		}
		return v
	}
	return walk(value)
}

// treeNode contains itself.
type treeNode struct {
	Name     string     `json:"name"`
	Children []treeNode `json:"children,omitzero"`
}

// TestMCPRecursiveSchemas writes a type that contains itself once under $defs
// and refers to it from within, since it cannot be inlined, and resolves every
// reference within the tool's own schema.
func TestMCPRecursiveSchemas(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Post("/trees", func(_ *muzak.Context, in treeNode) (treeNode, error) { return in, nil }, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	tool := toolNamed(t, m.listAll(), "post_trees")
	for _, schema := range []jsontext.Value{tool.InputSchema, tool.OutputSchema} {
		var parsed struct {
			Defs map[string]jsontext.Value `json:"$defs"`
		}
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed.Defs) != 1 {
			t.Fatalf("$defs = %v, want the tree alone: %s", parsed.Defs, schema)
		}
		for _, ref := range refsIn(schema) {
			name, local := strings.CutPrefix(ref, "#/$defs/")
			if _, defined := parsed.Defs[name]; !local || !defined {
				t.Fatalf("reference %q does not resolve within the schema %s", ref, schema)
			}
		}
	}
	got := m.call("post_trees", map[string]any{"body": map[string]any{"name": "root", "children": []any{map[string]any{"name": "leaf"}}}})
	jsonEqual(t, got.StructuredContent, `{"name":"root","children":[{"name":"leaf"}]}`)
}

// refsIn lists every $ref in a schema.
func refsIn(schema jsontext.Value) []string {
	var refs []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				refs = append(refs, ref)
			}
			for _, item := range v {
				walk(item)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	var value any
	_ = json.Unmarshal(schema, &value)
	walk(value)
	return refs
}

// uploadIn is a form with files.
type uploadIn struct {
	Title string   `form:"title" doc:"What the upload is"`
	Doc   []byte   `file:"doc"`
	Extra [][]byte `file:"extra"`
}

// TestMCPFormsAndFiles describes a form's files as base64 strings and calls
// the route with them as a multipart body.
func TestMCPFormsAndFiles(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Put("/uploads", func(_ *muzak.Context, in uploadIn) (map[string]any, error) {
		return map[string]any{"title": in.Title, "doc": string(in.Doc), "extra": len(in.Extra)}, nil
	}, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	tool := toolNamed(t, m.listAll(), "put_uploads")
	var schema struct {
		Properties map[string]struct {
			Properties map[string]jsontext.Value `json:"properties"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	form := schema.Properties["form"].Properties
	jsonEqual(t, form["doc"], `{"type":"string","contentEncoding":"base64"}`)
	jsonEqual(t, form["extra"], `{"type":"array","items":{"type":"string","contentEncoding":"base64"}}`)
	jsonEqual(t, form["title"], `{"type":"string","description":"What the upload is"}`)
	if !slices.Contains(schema.Required, "form") {
		t.Fatalf("the form holds a required field, but is not required: %v", schema.Required)
	}
	if tool.Annotations == nil || !*tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint != nil {
		t.Fatalf("PUT annotations = %+v", tool.Annotations)
	}
	got := m.call("put_uploads", map[string]any{"form": map[string]any{
		"title": "report", "doc": "aGVsbG8=", "extra": []string{"YQ==", "Yg=="},
	}})
	jsonEqual(t, got.StructuredContent, `{"title":"report","doc":"hello","extra":2}`)
	got = m.call("put_uploads", map[string]any{"form": map[string]any{"title": "report", "doc": "%%%"}})
	mustContain(t, got.text(t), "must be a file's contents in standard base64")
}

// TestMCPEraShapes lists and answers in each revision's shape: 2025-03-26 has
// no title, output schema or structured content, 2025-06-18 has them for
// objects, and 2026-07-28 for any JSON, with the type of each result, the
// server's identity and the caching hints a list carries.
func TestMCPEraShapes(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{ListTTL: -1})
	app.Get("/item", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{ID: "1"}, nil },
		muzak.MCPTool(), muzak.Title("One Item"))
	app.Get("/list", func(*muzak.Context, muzak.Empty) ([]shopItem, error) { return []shopItem{{ID: "1"}}, nil }, muzak.MCPTool())
	m := newMCPClient(t, app)

	old := m.fork()
	old.initialize("2025-03-26")
	for _, tool := range old.listAll() {
		if tool.Title != "" || tool.OutputSchema != nil {
			t.Fatalf("2025-03-26 lists %s with a title or an output schema", tool.Name)
		}
	}
	if got := old.call("get_item", nil); got.StructuredContent != nil || got.text(t) != `{"id":"1","name":"","price":0}` {
		t.Fatalf("2025-03-26 result = %s", got.raw)
	}

	current := m.fork()
	current.initialize("2025-11-25")
	tools := current.listAll()
	if item := toolNamed(t, tools, "get_item"); item.Title != "One Item" || item.OutputSchema == nil {
		t.Fatalf("2025-11-25 lists get_item as %+v", item)
	}
	if list := toolNamed(t, tools, "get_list"); list.OutputSchema != nil {
		t.Fatalf("2025-11-25 lists an array's output schema, which must be an object: %s", list.OutputSchema)
	}
	if got := current.call("get_list", nil); got.StructuredContent != nil || got.ResultType != "" || got.Meta != nil {
		t.Fatalf("2025-11-25 result for an array = %s", got.raw)
	}

	modern := m.fork()
	modern.stateless, modern.version = true, "2026-07-28"
	page := result[toolsPage](t, modern.send(http.StatusOK, "tools/list", nil))
	if page.ResultType != "complete" || page.TTLMs == nil || *page.TTLMs != 0 || page.CacheScope != "public" || page.Meta == nil {
		t.Fatalf("2026-07-28 list = %+v", page)
	}
	if list := toolNamed(t, page.Tools, "get_list"); list.OutputSchema == nil {
		t.Fatal("2026-07-28 lists no output schema for an array")
	}
	got := modern.call("get_list", nil)
	jsonEqual(t, got.StructuredContent, `[{"id":"1","name":"","price":0}]`)
	if got.ResultType != "complete" || got.Meta["io.modelcontextprotocol/serverInfo"] == nil {
		t.Fatalf("2026-07-28 result = %s", got.raw)
	}
}

// TestMCPPagination pages tools/list by PageSize, in the order of the tools'
// names, and refuses a cursor that was altered, forged or issued by another
// server.
func TestMCPPagination(t *testing.T) {
	build := func() *muzak.App {
		app := muzak.New(quietMCPOptions())
		app.MCP("/mcp", muzak.MCPOptions{PageSize: 2})
		empty := func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil }
		for _, name := range []string{"e", "a", "d", "c", "b"} {
			app.Get("/"+name, empty, muzak.MCPTool())
		}
		return app
	}
	m := newMCPClient(t, build())
	m.initialize("2025-06-18")
	var names []string
	var cursors []string
	params := map[string]any{}
	for {
		page := result[toolsPage](t, m.send(http.StatusOK, "tools/list", params))
		if len(page.Tools) > 2 {
			t.Fatalf("a page holds %d tools, over the page size", len(page.Tools))
		}
		for _, tool := range page.Tools {
			names = append(names, tool.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursors = append(cursors, page.NextCursor)
		params = map[string]any{"cursor": page.NextCursor}
	}
	if want := []string{"get_a", "get_b", "get_c", "get_d", "get_e"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if len(cursors) != 2 {
		t.Fatalf("cursors = %v, want two", cursors)
	}
	tampered := []string{"", "x", strings.Repeat("A", 27), cursors[0] + "A", cursors[0][1:]}
	for i := range cursors[0] {
		flipped := []byte(cursors[0])
		if flipped[i] == 'A' {
			flipped[i] = 'B'
		} else {
			flipped[i] = 'A'
		}
		tampered = append(tampered, string(flipped))
	}
	for _, cursor := range tampered {
		fault(t, m.send(http.StatusOK, "tools/list", map[string]any{"cursor": cursor}), -32602)
	}
	// A cursor from another server, or the same one after a restart, is not
	// one this server issued.
	other := newMCPClient(t, build())
	other.initialize("2025-06-18")
	fault(t, other.send(http.StatusOK, "tools/list", map[string]any{"cursor": cursors[0]}), -32602)
	// A null cursor is the first page.
	page := result[toolsPage](t, m.send(http.StatusOK, "tools/list", map[string]any{"cursor": nil}))
	if page.Tools[0].Name != "get_a" {
		t.Fatalf("a null cursor started at %s", page.Tools[0].Name)
	}
}

// TestMCPVersionedRoutes calls the variant of a route a header chooses by
// sending its version, lists URI versions as tools of their own, and refuses
// a route chosen by a custom extractor.
func TestMCPVersionedRoutes(t *testing.T) {
	options := quietMCPOptions()
	options.Versioning = muzak.VersioningOptions{Type: muzak.VersioningHeader, Header: "X-API-Version"}
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/things", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{ID: "v1"}, nil },
		muzak.WithVersion("1"), muzak.MCPTool())
	app.Get("/things", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{ID: "v2"}, nil },
		muzak.WithVersion("2"), muzak.MCPTool())
	app.Get("/plain", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{ID: "neutral"}, nil },
		muzak.WithVersion(muzak.VersionNeutral), muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	for name, want := range map[string]string{"get_things_1": "v1", "get_things_2": "v2", "get_plain": "neutral"} {
		got := m.call(name, nil)
		var item shopItem
		_ = json.Unmarshal(got.StructuredContent, &item)
		if item.ID != want {
			t.Fatalf("%s reached %q, want %q", name, item.ID, want)
		}
	}

	media := quietMCPOptions()
	media.Versioning = muzak.VersioningOptions{Type: muzak.VersioningMediaType, Key: "v="}
	mediaApp := muzak.New(media)
	mediaApp.MCP("/mcp", muzak.MCPOptions{})
	mediaApp.Get("/things", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{ID: "m3"}, nil },
		muzak.WithVersion("3"), muzak.MCPTool())
	mm := newMCPClient(t, mediaApp)
	mm.initialize("2025-06-18")
	jsonEqual(t, mm.call("get_things_3", nil).StructuredContent, `{"id":"m3","name":"","price":0}`)

	uri := quietMCPOptions()
	uri.Versioning = muzak.VersioningOptions{Type: muzak.VersioningURI}
	uriApp := muzak.New(uri)
	uriApp.MCP("/mcp", muzak.MCPOptions{})
	uriApp.Get("/things", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{ID: "uri"}, nil },
		muzak.WithVersion("1", "2"), muzak.MCPTool())
	um := newMCPClient(t, uriApp)
	um.initialize("2025-06-18")
	var names []string
	for _, tool := range um.listAll() {
		names = append(names, tool.Name)
	}
	if want := []string{"get_v1_things", "get_v2_things"}; !slices.Equal(names, want) {
		t.Fatalf("URI versions are tools %v, want %v", names, want)
	}
	jsonEqual(t, um.call("get_v2_things", nil).StructuredContent, `{"id":"uri","name":"","price":0}`)

	custom := quietMCPOptions()
	custom.Versioning = muzak.VersioningOptions{Type: muzak.VersioningCustom, Extractor: func(*http.Request) []string { return nil }}
	customApp := muzak.New(custom)
	customApp.MCP("/mcp", muzak.MCPOptions{})
	customApp.Get("/things", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil },
		muzak.WithVersion("1"), muzak.MCPTool())
	mustContain(t, fmt.Sprint(customApp.Build()), "the route's version is chosen by a custom extractor")

	bound := quietMCPOptions()
	bound.Versioning = muzak.VersioningOptions{Type: muzak.VersioningHeader, Header: "X-API-Version"}
	boundApp := muzak.New(bound)
	boundApp.MCP("/mcp", muzak.MCPOptions{})
	boundApp.Get("/things", func(*muzak.Context, versionIn) (shopItem, error) { return shopItem{}, nil },
		muzak.WithVersion("1"), muzak.MCPTool())
	mediaBound := quietMCPOptions()
	mediaBound.Versioning = muzak.VersioningOptions{Type: muzak.VersioningMediaType, Key: "v"}
	mediaBoundApp := muzak.New(mediaBound)
	mediaBoundApp.MCP("/mcp", muzak.MCPOptions{})
	mediaBoundApp.Get("/things", func(*muzak.Context, acceptIn) (shopItem, error) { return shopItem{}, nil },
		muzak.WithVersion("1"), muzak.MCPTool())
	mustContain(t, fmt.Sprint(boundApp.Build()), "the input binds the header X-Api-Version, which selects the route's version")
	mustContain(t, fmt.Sprint(mediaBoundApp.Build()), "chosen by a parameter of Accept")
}

// versionIn binds the version header.
type versionIn struct {
	Version string `header:"X-API-Version"`
}

// acceptIn binds Accept.
type acceptIn struct {
	Accept string `header:"Accept"`
}

// TestMCPGuardedEndpointListsPrivately marks a 2026-07-28 listing private
// when the endpoint is guarded, so a shared cache does not hand it to a client
// the guard refuses.
func TestMCPGuardedEndpointListsPrivately(t *testing.T) {
	guard := func(ctx *muzak.Context) error {
		if ctx.Header("Authorization") != "Bearer letmein" {
			return muzak.Unauthorized("")
		}
		return nil
	}
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}, muzak.WithDependencies(guard)), testclient.WithHeader("Authorization", "Bearer letmein"))
	m.stateless, m.version = true, "2026-07-28"
	page := result[toolsPage](t, m.send(http.StatusOK, "tools/list", nil))
	if page.CacheScope != "private" || page.TTLMs == nil || *page.TTLMs != 300000 {
		t.Fatalf("a guarded listing is %q for %v ms", page.CacheScope, page.TTLMs)
	}
}
