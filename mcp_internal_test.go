package muzak

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMCPSessionStoreExpires drops a session idle past the timeout, and only
// that one, keeping one that was used meanwhile.
func TestMCPSessionStoreExpires(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	store := newMCPSessions(10, time.Minute)
	store.now = clock.Now
	a := store.create(mcpVersion20250618, [sha256.Size]byte{})
	b := store.create(mcpVersion20251125, [sha256.Size]byte{1})
	clock.Advance(30 * time.Second)
	if session, ok := store.lookup(a); !ok || session.version != mcpVersion20250618 {
		t.Fatalf("a = %+v, %t", session, ok)
	}
	clock.Advance(45 * time.Second)
	if _, ok := store.lookup(b); ok {
		t.Fatal("a session idle for 75s outlived a one minute timeout")
	}
	if _, ok := store.lookup(a); !ok {
		t.Fatal("a session used 45s ago was dropped")
	}
	if store.len() != 1 {
		t.Fatalf("len = %d, want 1", store.len())
	}
	clock.Advance(2 * time.Minute)
	if store.remove(a) {
		t.Fatal("an expired session was removed as though it were open")
	}
	if store.len() != 0 {
		t.Fatalf("len = %d, want 0", store.len())
	}
}

// TestMCPSessionStoreIsBounded holds at most its limit however many sessions
// are opened, evicting the one idle longest each time, in time linear in the
// sessions opened.
func TestMCPSessionStoreIsBounded(t *testing.T) {
	const limit = 10_000
	store := newMCPSessions(limit, time.Hour)
	ids := make([]string, 0, 2*limit)
	start := time.Now()
	for range 2 * limit {
		ids = append(ids, store.create(mcpVersion20250618, [sha256.Size]byte{}))
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("opening %d sessions took %v", 2*limit, elapsed)
	}
	if store.len() != limit {
		t.Fatalf("len = %d, want the limit %d", store.len(), limit)
	}
	if _, ok := store.lookup(ids[0]); ok {
		t.Fatal("the session idle longest survived past the limit")
	}
	if _, ok := store.lookup(ids[len(ids)-1]); !ok {
		t.Fatal("the newest session was evicted")
	}
	if len(store.index) != limit {
		t.Fatalf("the index holds %d entries, want %d", len(store.index), limit)
	}
}

// TestMCPSessionStoreRefusesMalformedIdentifiers never looks up a value that
// is not an identifier the store could have issued.
func TestMCPSessionStoreRefusesMalformedIdentifiers(t *testing.T) {
	store := newMCPSessions(4, time.Hour)
	id := store.create(mcpVersion20250618, [sha256.Size]byte{})
	for _, bad := range []string{"", strings.ToLower(id), id + "A", id[1:], strings.Repeat("A", 25) + "1", strings.Repeat("A", 25) + "8"} {
		if _, ok := store.lookup(bad); ok {
			t.Fatalf("%q was found", bad)
		}
		if store.remove(bad) {
			t.Fatalf("%q was removed", bad)
		}
	}
	if !wellFormedSessionID(id) {
		t.Fatalf("the store issued %q, which it would not accept", id)
	}
}

// TestMCPSessionStoreConcurrency opens, finds and ends sessions from many
// goroutines, which the race detector checks, and keeps the bound throughout.
func TestMCPSessionStoreConcurrency(t *testing.T) {
	store := newMCPSessions(64, time.Hour)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 200 {
				id := store.create(mcpVersion20250618, [sha256.Size]byte{})
				store.lookup(id)
				if store.len() > 64 {
					t.Error("the bound was exceeded")
				}
				store.remove(id)
			}
		})
	}
	wg.Wait()
}

// mcpTestApp is an application with an MCP endpoint and one tool, built.
func mcpTestApp(t *testing.T, opts MCPOptions) *App {
	t.Helper()
	app := New(quietOptions())
	app.MCP("/mcp", opts)
	app.Get("/items/{id}", func(_ *Context, in struct {
		ID string `path:"id"`
	}) (map[string]string, error) {
		return map[string]string{"id": in.ID}, nil
	}, MCPTool())
	return mustBuild(t, app)
}

// mcpPost sends one message to an application's endpoint in-process.
func mcpPost(t *testing.T, app *App, session, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set(HeaderMCPSessionID, session)
	}
	return doRequest(t, app, req)
}

// TestMCPExpiredSessionIs404 answers a session that expired with 404, which
// tells the client to open another.
func TestMCPExpiredSessionIs404(t *testing.T) {
	app := mcpTestApp(t, MCPOptions{SessionIdleTimeout: time.Minute})
	clock := &fakeClock{now: time.Now()}
	app.mcp.sessions.now = clock.Now
	rec := mcpPost(t, app, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{}}}`)
	assertStatus(t, rec, http.StatusOK)
	session := rec.Header().Get(HeaderMCPSessionID)
	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	assertStatus(t, mcpPost(t, app, session, ping), http.StatusOK)
	clock.Advance(59 * time.Second)
	assertStatus(t, mcpPost(t, app, session, ping), http.StatusOK)
	clock.Advance(61 * time.Second)
	assertStatus(t, mcpPost(t, app, session, ping), http.StatusNotFound)
}

// TestMCPSessionIDIsNeverLogged opens, uses and ends a session with every log
// level on, and finds the identifier nowhere in the logs.
func TestMCPSessionIDIsNeverLogged(t *testing.T) {
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.MCP("/mcp", MCPOptions{})
	app.Get("/fail", func(*Context, Empty) (Empty, error) { return Empty{}, errors.New("broken") }, MCPTool())
	mustBuild(t, app)
	rec := mcpPost(t, app, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{}}}`)
	session := rec.Header().Get(HeaderMCPSessionID)
	mcpPost(t, app, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_fail"}}`)
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(HeaderMCPSessionID, session)
	assertStatus(t, doRequest(t, app, req), http.StatusNoContent)
	if !strings.Contains(logs.String(), "broken") {
		t.Fatal("the failing tool was not logged, so the logs were not captured")
	}
	if strings.Contains(logs.String(), session) {
		t.Fatalf("the session identifier was logged:\n%s", logs.String())
	}
}

// TestMCPOwnerIdentifiesThePrincipal tells principals apart by scheme, issuer
// and subject, or by key, and gives a request with none the zero owner.
func TestMCPOwnerIdentifiesThePrincipal(t *testing.T) {
	owner := func(values ...any) [sha256.Size]byte {
		c := &Context{}
		for _, v := range values {
			switch v := v.(type) {
			case *Claims:
				c.deps = append(c.deps, depValue{typ: claimsType, val: v})
			case *APIKeyPrincipal:
				c.deps = append(c.deps, depValue{typ: apiKeyPrincipalType, val: v})
			}
		}
		return mcpOwner(c)
	}
	none := owner()
	alice := owner(&Claims{Scheme: "oidc", Issuer: "https://a", Subject: "alice"})
	bob := owner(&Claims{Scheme: "oidc", Issuer: "https://a", Subject: "bob"})
	shifted := owner(&Claims{Scheme: "oidc", Issuer: "https://aalice", Subject: ""})
	key := owner(&APIKeyPrincipal{Scheme: "key", ID: "alice"})
	both := owner(&Claims{Scheme: "oidc", Issuer: "https://a", Subject: "alice"}, &APIKeyPrincipal{Scheme: "key", ID: "k"})
	if none != ([sha256.Size]byte{}) {
		t.Fatal("no principal is not the zero owner")
	}
	for i, a := range [][sha256.Size]byte{alice, bob, shifted, key, both} {
		for j, b := range [][sha256.Size]byte{alice, bob, shifted, key, both} {
			if i != j && a == b {
				t.Fatalf("owners %d and %d collide", i, j)
			}
		}
	}
	if alice != owner(&Claims{Scheme: "oidc", Issuer: "https://a", Subject: "alice", ID: "another token"}) {
		t.Fatal("a new token for the same subject is another owner")
	}
	session := mcpSession{owner: alice}
	if !session.ownedBy(alice) || session.ownedBy(bob) || session.ownedBy(none) {
		t.Fatal("ownedBy")
	}
	if !(mcpSession{}).ownedBy(bob) {
		t.Fatal("a session opened by nobody refused a principal")
	}
}

// TestMCPHeaderHelpers covers the transport's small parsers at their edges.
func TestMCPHeaderHelpers(t *testing.T) {
	for header, want := range map[string]bool{
		"application/json":                             true,
		"Application/JSON":                             true,
		"*/*;q=0.001":                                  true,
		"text/html, application/*;q=0.5":               true,
		"application/json;q=0, application/*":          false,
		"application/*;q=0, application/json;q=0.2":    true,
		"application/json;q=0.5, application/json;q=0": true,
		"":                      false,
		",,,":                   false,
		"text/plain":            false,
		"application/jsonx":     false,
		"application/json;q=-1": false,
	} {
		if got := acceptsJSON([]string{header}); got != want {
			t.Errorf("acceptsJSON(%q) = %t, want %t", header, got, want)
		}
	}
	if !acceptsJSON([]string{"text/html", "application/json"}) {
		t.Error("an Accept sent as two lines was not read as one list")
	}
	for value, want := range map[string]string{
		"plain":           "plain",
		"=?base64?aGk=?=": "hi",
		"=?base64?aGk=":   "=?base64?aGk=",
		"=?base64?!!?=":   "",
		"=?base64?/w==?=": "",
		"=?BASE64?aGk=?=": "=?BASE64?aGk=?=",
		"=?base64??=":     "",
	} {
		if got := decodeMCPHeaderValue(value); got != want {
			t.Errorf("decodeMCPHeaderValue(%q) = %q, want %q", value, got, want)
		}
	}
	for data, want := range map[string]bool{
		`{"a":"[[[["}`:     false,
		`{"a":"\"[[[["}`:   false,
		`{"a":"\\"}`:       false,
		`[[[[1]]]]`:        true,
		`[[[1]]]`:          false,
		`{"a":[{"b":[]}]}`: true,
		`{"a":[{"b":1}]}`:  false,
	} {
		if got := jsonDepthExceeds([]byte(data), 3); got != want {
			t.Errorf("jsonDepthExceeds(%s, 3) = %t, want %t", data, got, want)
		}
	}
}

// TestMCPCursors refuses every cursor the server did not issue, the first
// page's included, and one past the tools.
func TestMCPCursors(t *testing.T) {
	s := &mcpServer{tools: make([]*mcpTool, 5)}
	s.cursorKey = sha256.Sum256([]byte("key"))
	for index := 1; index < 5; index++ {
		cursor := s.cursor(index)
		if len(cursor) != mcpCursorLength {
			t.Fatalf("cursor %q is %d long", cursor, len(cursor))
		}
		if got, ok := s.pageStart(cursor); !ok || got != index {
			t.Fatalf("pageStart(cursor(%d)) = %d, %t", index, got, ok)
		}
	}
	for _, index := range []int{0, 5, 6} {
		if _, ok := s.pageStart(s.cursor(index)); ok {
			t.Fatalf("a cursor for index %d was accepted", index)
		}
	}
	other := &mcpServer{tools: s.tools}
	other.cursorKey = sha256.Sum256([]byte("other key"))
	if _, ok := other.pageStart(s.cursor(2)); ok {
		t.Fatal("another server's cursor was accepted")
	}
	if _, ok := s.pageStart(strings.Repeat("!", mcpCursorLength)); ok {
		t.Fatal("a cursor that is not base64 was accepted")
	}
}

// TestMCPSchemaInlinerBudget writes components that share one another many
// times over once each under $defs, rather than inline them into a schema
// exponentially larger than the document.
func TestMCPSchemaInlinerBudget(t *testing.T) {
	components := map[string]*Schema{}
	const levels = 30
	for i := range levels {
		name := fmt.Sprintf("L%02d", i)
		next := &Schema{Type: "string"}
		if i+1 < levels {
			next = &Schema{Ref: componentPrefix + fmt.Sprintf("L%02d", i+1)}
		}
		components[name] = &Schema{Type: "object", Properties: map[string]*Schema{"left": next, "right": next}}
	}
	start := time.Now()
	out, err := renderToolSchema(components, func(in *schemaInliner) *Schema {
		return in.root(&Schema{Ref: componentPrefix + "L00"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second || len(out) > 64<<10 {
		t.Fatalf("the schema took %v and is %d bytes", time.Since(start), len(out))
	}
	var parsed struct {
		Type string                    `json:"type"`
		Defs map[string]jsontext.Value `json:"$defs"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Type != "object" || len(parsed.Defs) != levels-1 {
		t.Fatalf("type %q with %d definitions: %s", parsed.Type, len(parsed.Defs), out)
	}
	// A small schema is inlined whole.
	small, err := renderToolSchema(components, func(in *schemaInliner) *Schema {
		return in.node(&Schema{Ref: componentPrefix + "L28"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(small), "$ref") || strings.Contains(string(small), "$defs") {
		t.Fatalf("a small schema was not inlined: %s", small)
	}
}

// TestMCPSchemaInlinerSiblings applies what is written beside a reference as
// JSON Schema 2020-12 does: a description replaces the component's, and a rule
// is a schema the value satisfies as well.
func TestMCPSchemaInlinerSiblings(t *testing.T) {
	components := map[string]*Schema{"Money": {Type: "object", Description: "An amount", Properties: map[string]*Schema{"amount": {Type: "integer"}}}}
	low := 1.0
	for name, tc := range map[string]struct {
		in   *Schema
		want string
	}{
		"described": {&Schema{Ref: componentPrefix + "Money", Description: "The price", Title: "Price", Deprecated: true, Default: map[string]any{}}, `{"type":"object","title":"Price","description":"The price","properties":{"amount":{"type":"integer"}},"default":{},"deprecated":true}`},
		"narrowed":  {&Schema{Ref: componentPrefix + "Money", Properties: map[string]*Schema{"amount": {Minimum: &low}}}, `{"properties":{"amount":{"minimum":1}},"allOf":[{"type":"object","description":"An amount","properties":{"amount":{"type":"integer"}}}]}`},
		"nullable":  {&Schema{AnyOf: []*Schema{{Ref: componentPrefix + "Money"}, {Type: "null"}}}, `{"anyOf":[{"type":"object","description":"An amount","properties":{"amount":{"type":"integer"}}},{"type":"null"}]}`},
		"map":       {&Schema{Type: "object", AdditionalProperties: &Schema{Ref: componentPrefix + "Money"}}, `{"type":"object","additionalProperties":{"type":"object","description":"An amount","properties":{"amount":{"type":"integer"}}}}`},
		"unknown":   {&Schema{Ref: componentPrefix + "Missing"}, `{}`},
	} {
		out, err := renderToolSchema(components, func(in *schemaInliner) *Schema { return in.node(tc.in) })
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		_ = json.Unmarshal(out, &got)
		_ = json.Unmarshal([]byte(tc.want), &want)
		g, _ := json.Marshal(got, json.Deterministic(true))
		w, _ := json.Marshal(want, json.Deterministic(true))
		if string(g) != string(w) {
			t.Errorf("%s:\n got %s\nwant %s", name, g, w)
		}
	}
	// A root that refers to itself is written under $defs and inlined once
	// at the root, so its type can be read; an empty root gets its $defs too.
	self := map[string]*Schema{"Node": {Type: "object", Properties: map[string]*Schema{"next": {Ref: componentPrefix + "Node"}}}}
	in := newSchemaInliner(self, true)
	root := in.complete(in.root(&Schema{Ref: componentPrefix + "Node", Description: "the head"}))
	out, err := in.encode(root)
	if err != nil || !strings.Contains(string(out), `"description":"the head"`) || !strings.Contains(string(out), `"$defs":{"Node"`) {
		t.Fatalf("%s, %v", out, err)
	}
	in = newSchemaInliner(self, true)
	empty := in.complete(in.node(&Schema{Items: &Schema{Ref: componentPrefix + "Node"}}))
	empty.Items = nil
	if out, err := in.encode(&Schema{}); err != nil || string(out) != `{"$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}` {
		t.Fatalf("%s, %v", out, err)
	}
}

// TestMCPDescribeErrorsAreBuildErrors reports a tool whose description is not
// valid UTF-8, which cannot be listed, even with the documentation disabled.
func TestMCPDescribeErrorsAreBuildErrors(t *testing.T) {
	options := quietOptions()
	options.DisableDocs = true
	app := New(options)
	app.MCP("/mcp", MCPOptions{})
	app.Get("/bad", func(*Context, Empty) (Empty, error) { return Empty{}, nil }, MCPTool(), Summary("broken \xff text"))
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), `the MCP tool "get_bad" cannot be encoded as JSON`) {
		t.Fatalf("Build() = %v", err)
	}
	type badDoc struct {
		Name string `json:"name" doc:"broken \xff"`
	}
	body := New(options)
	body.MCP("/mcp", MCPOptions{})
	body.Post("/bad", func(*Context, badDoc) (Empty, error) { return Empty{}, nil }, MCPTool())
	if err := body.Build(); err == nil || !strings.Contains(err.Error(), "cannot be encoded as JSON") {
		t.Fatalf("Build() = %v", err)
	}
	type badOut struct {
		Name string `json:"name" doc:"broken \xff"`
	}
	output := New(options)
	output.MCP("/mcp", MCPOptions{})
	output.Get("/bad", func(*Context, Empty) (badOut, error) { return badOut{}, nil }, MCPTool())
	if err := output.Build(); err == nil || !strings.Contains(err.Error(), "cannot be encoded as JSON") {
		t.Fatalf("Build() = %v", err)
	}
}

// TestMCPCostsNothingWhenOff serves a request with no more allocations in an
// application with an MCP endpoint than in one without, for a route that is
// not a tool call: the hooks the endpoint adds are comparisons.
func TestMCPCostsNothingWhenOff(t *testing.T) {
	build := func(withMCP bool) *App {
		options := quietOptions()
		options.DisableAccessLog = true
		app := New(options)
		if withMCP {
			app.MCP("/mcp", MCPOptions{})
		}
		app.Get("/items/{id}", func(_ *Context, in struct {
			ID string `path:"id"`
		}) (map[string]string, error) {
			return map[string]string{"id": in.ID}, nil
		})
		return mustBuild(t, app)
	}
	measure := func(app *App) float64 {
		req := httptest.NewRequest(http.MethodGet, "/items/42", nil)
		return testing.AllocsPerRun(200, func() {
			app.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
	without, with := measure(build(false)), measure(build(true))
	if with > without {
		t.Fatalf("a request allocates %v times with an MCP endpoint and %v without", with, without)
	}
}

// TestMCPNoGoroutineLeaks drives tool calls of every outcome, a cancelled one
// and one over the result limit among them, and finds nothing left running.
func TestMCPNoGoroutineLeaks(t *testing.T) {
	app := New(quietOptions())
	app.MCP("/mcp", MCPOptions{MaxResultSize: 64})
	app.Get("/ok", func(*Context, Empty) (map[string]int, error) { return map[string]int{"a": 1}, nil }, MCPTool())
	app.Get("/big", func(*Context, Empty) (Bytes, error) {
		return Bytes{ContentType: "text/plain", Data: []byte(strings.Repeat("x", 100))}, nil
	}, MCPTool())
	app.Get("/slow", func(ctx *Context, _ Empty) (Empty, error) {
		<-ctx.Context().Done()
		return Empty{}, ctx.Context().Err()
	}, MCPTool())
	app.Get("/panic", func(*Context, Empty) (Empty, error) { panic("boom") }, MCPTool())
	srv := httptest.NewServer(mustBuild(t, app))
	post := func(ctx context.Context, session, body string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if session != "" {
			req.Header.Set(HeaderMCPSessionID, session)
		}
		return srv.Client().Do(req)
	}
	res, err := post(t.Context(), "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{}}}`)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	session := res.Header.Get(HeaderMCPSessionID)
	for range 20 {
		for _, tool := range []string{"get_ok", "get_big", "get_panic"} {
			res, err := post(t.Context(), session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+tool+`"}}`)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
		}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		if res, err := post(ctx, session, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_slow"}}`); err == nil {
			_ = res.Body.Close()
		}
		cancel()
	}
	srv.Close()
	assertNoGoroutineLeaks(t)
}

// TestMCPDetachedContext follows the MCP request's cancellation and deadline
// and carries none of its values.
func TestMCPDetachedContext(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithTimeout(context.WithValue(t.Context(), key{}, "secret"), time.Hour)
	ctx, stop := context.WithCancel(detachedContext{parent: parent})
	defer stop()
	if ctx.Value(key{}) != nil {
		t.Fatal("a value of the MCP request reached the call")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 59*time.Minute {
		t.Fatalf("deadline = %v, %t", deadline, ok)
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the MCP request did not cancel the call")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err() = %v", ctx.Err())
	}
}

// TestMCPRecorder ignores an interim status and a repeated one, as net/http
// does, and commits the response on a flush.
func TestMCPRecorder(t *testing.T) {
	rec := &mcpRecorder{header: http.Header{}, limit: 10, cancel: func() {}}
	rec.WriteHeader(http.StatusEarlyHints)
	rec.header.Set("Content-Type", "text/plain")
	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusTeapot)
	if rec.status != http.StatusCreated || rec.contentType != "text/plain" {
		t.Fatalf("status %d, type %q", rec.status, rec.contentType)
	}
	flushed := &mcpRecorder{header: http.Header{}}
	if err := flushed.FlushError(); err != nil || flushed.status != http.StatusOK {
		t.Fatalf("a flush left status %d, %v", flushed.status, err)
	}
	if err := flushed.FlushError(); err != nil {
		t.Fatal(err)
	}
}

// TestMCPInheritedRequestID adopts the MCP request's identifier only for a
// request made inside the application.
func TestMCPInheritedRequestID(t *testing.T) {
	app := mcpTestApp(t, MCPOptions{})
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	if id := app.inheritedRequestID(plain); id != "" {
		t.Fatalf("a client's request inherited %q", id)
	}
	inner := plain.WithContext(context.WithValue(plain.Context(), subrequestKey{}, &subrequest{requestID: "abc"}))
	if id := app.inheritedRequestID(inner); id != "abc" {
		t.Fatalf("inherited %q", id)
	}
	off := New(quietOptions())
	if id := off.inheritedRequestID(inner); id != "" {
		t.Fatalf("an application without an endpoint inherited %q", id)
	}
	if _, ok := app.subrequestParent(plain); ok {
		t.Fatal("a client's request has an in-process parent")
	}
}

// limitedBodyIn is an input with a JSON body for TestCallBodyLimit.
type limitedBodyIn struct {
	ID   string   `path:"id"`
	Name string   `json:"name"`
	Rows []string `json:"rows"`
}

// TestCallBodyLimit writes a form and a JSON body under every limit short of
// their length, each of which fails with errCallBodyTooLarge at whichever
// write crosses it, a form field, a file or the closing boundary, and under
// their length and no limit at all, which write the same body.
func TestCallBodyLimit(t *testing.T) {
	three := 3
	form := epFormIn{ID: "f1", Name: "name", Count: &three, Tags: []string{"a", ""}, Avatar: []byte("avatar"), Extra: [][]byte{[]byte("x"), {}}}
	body := limitedBodyIn{ID: "b1", Name: "name", Rows: []string{"a", "", "c"}}
	for _, c := range []struct {
		in    any
		route string
	}{{&form, "/forms/{id}"}, {&body, "/bodies/{id}"}} {
		value := reflect.ValueOf(c.in).Elem()
		plan, err := compileCall(value.Type(), reflect.TypeFor[Empty](), http.MethodPost, c.route, nil)
		if err != nil {
			t.Fatal(err)
		}
		whole, err := plan.encode(value, &callConfig{})
		if err != nil {
			t.Fatal(err)
		}
		exact, err := plan.encode(value, &callConfig{maxBody: int64(len(whole.body))})
		if err != nil || len(exact.body) != len(whole.body) {
			t.Fatalf("%s at its own length: %d bytes, %v", c.route, len(exact.body), err)
		}
		for limit := 1; limit < len(whole.body); limit++ {
			if _, err := plan.encode(value, &callConfig{maxBody: int64(limit)}); !errors.Is(err, errCallBodyTooLarge) {
				t.Fatalf("%s under %d of its %d bytes: %v", c.route, limit, len(whole.body), err)
			}
		}
	}
}

// limitedFormIn and limitedRowsIn are inputs a short value of which is written
// as a long body: an empty string is a form part with a boundary and headers,
// and an empty row every member of a struct.
type limitedFormIn struct {
	Tags []string `form:"tags"`
}

type limitedRow struct {
	A, B, C, D, E, F, G, H, I, J string
	K, L, M, N, O, P, Q, R, S, T int
}

type limitedRowsIn struct {
	Rows []limitedRow `json:"rows"`
}

// allocatedBy reports what the whole process allocates while work runs.
func allocatedBy(work func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	work()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestCallBodyLimitBoundsTheWork shows that a body past its limit costs about
// the limit to refuse, where written out whole a list of empty form values or
// empty rows costs a hundred times the bytes a client sent for it, and that a
// tool call's body argument past the limit is refused before it is decoded,
// which is where a list of empty objects becomes a list of structs. Each is
// measured against the work it saves, built the same way, so that what the
// race detector adds to both does not count.
//
// It is not parallel: it measures what the whole process allocates.
func TestCallBodyLimitBoundsTheWork(t *testing.T) {
	const entries, limit = 80_000, 64 << 10
	for _, in := range []any{
		&limitedFormIn{Tags: make([]string, entries)},
		&limitedRowsIn{Rows: make([]limitedRow, entries)},
	} {
		value := reflect.ValueOf(in).Elem()
		plan, err := compileCall(value.Type(), reflect.TypeFor[Empty](), http.MethodPost, "/limited", nil)
		if err != nil {
			t.Fatal(err)
		}
		bounded := allocatedBy(func() { _, err = plan.encode(value, &callConfig{maxBody: limit}) })
		if !errors.Is(err, errCallBodyTooLarge) {
			t.Fatalf("%s: %v", value.Type(), err)
		}
		whole := allocatedBy(func() { _, err = plan.encode(value, &callConfig{}) })
		if err != nil {
			t.Fatal(err)
		}
		if bounded*10 > whole {
			t.Errorf("%s: refusing at the limit allocated %d bytes, and writing it whole %d", value.Type(), bounded, whole)
		}
	}

	app := New(quietOptions())
	app.MCP("/mcp", MCPOptions{})
	app.Post("/rows", func(*Context, limitedRowsIn) (Empty, error) { return Empty{}, nil }, MaxBodySize(limit), MCPTool())
	tool := mustBuild(t, app).mcp.byName["post_rows"]
	args := jsontext.Value(`{"body":{"rows":[{}` + strings.Repeat(`,{}`, entries-1) + `]}}`)
	var err error
	bounded := allocatedBy(func() { _, err = tool.decode(args) })
	if !errors.Is(err, errCallBodyTooLarge) {
		t.Fatalf("a body argument past the limit: %v", err)
	}
	tool.maxBody = 1 << 30
	whole := allocatedBy(func() { _, err = tool.decode(args) })
	if err != nil {
		t.Fatal(err)
	}
	if bounded*10 > whole {
		t.Errorf("refusing the body argument allocated %d bytes, and decoding it %d", bounded, whole)
	}
}
