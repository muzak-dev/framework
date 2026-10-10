package muzak_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// TestMCPCallSucceeds calls tools whose arguments fill every part of a
// request, defaults included, and answers with the route's JSON as text and
// as structured content.
func TestMCPCallSucceeds(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	got := m.call("get_item", map[string]any{"path": map[string]any{"id": "42"}, "query": map[string]any{"verbose": true}})
	if got.IsError {
		t.Fatalf("the call failed: %s", got.raw)
	}
	jsonEqual(t, got.StructuredContent, `{"id":"42","name":"item verbose=true limit=10","price":5}`)
	jsonEqual(t, got.text(t), got.StructuredContent)

	got = m.call("get_item", map[string]any{"path": map[string]any{"id": "a b%2F?#\u00e9"}, "query": map[string]any{"limit": 25, "verbose": "false"}})
	jsonEqual(t, got.StructuredContent, `{"id":"a b%2F?#\u00e9","name":"item verbose=false limit=25","price":5}`)

	got = m.call("get_item", map[string]any{"path": map[string]any{"id": "1"}, "query": map[string]any{"limit": nil}})
	jsonEqual(t, got.StructuredContent, `{"id":"1","name":"item verbose=false limit=10","price":5}`)

	got = m.call("create_item", map[string]any{"header": map[string]any{"X-Tenant": "acme"}, "body": map[string]any{"name": "pen", "price": 3}})
	jsonEqual(t, got.StructuredContent, `{"item":{"id":"new","name":"pen","price":3},"tenant":"acme","color":"red"}`)

	got = m.call("delete_item", map[string]any{"path": map[string]any{"id": "1"}})
	if got.IsError || len(got.Content) != 0 || got.StructuredContent != nil {
		t.Fatalf("a 204 is a result with nothing in it, got %s", got.raw)
	}
}

// TestMCPCallArgumentProblems answers arguments that do not fit the tool's
// schema with an error result a model can read and correct, in the shape of
// the binder's 422.
func TestMCPCallArgumentProblems(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	id := map[string]any{"id": "1"}
	for _, tc := range []struct {
		tool    string
		args    any
		details []string
	}{
		{"get_item", map[string]any{}, []string{"path id is required"}},
		{"get_item", nil, []string{"path id is required"}},
		{"get_item", map[string]any{"path": map[string]any{"id": nil}}, []string{"path id is required"}},
		{"get_item", map[string]any{"bogus": 1, "path": id}, []string{"arguments bogus is not a part of the request this tool sends"}},
		{"get_item", map[string]any{"path": "1"}, []string{"path  must be an object", "path id is required"}},
		{"get_item", map[string]any{"path": map[string]any{"id": "1", "extra": 2}}, []string{"path extra is not a parameter of this operation"}},
		{"get_item", map[string]any{"path": map[string]any{"id": map[string]any{"a": 1}}}, []string{"path id must be a string, a number or a boolean"}},
		{"get_item", map[string]any{"path": id, "query": map[string]any{"limit": "ten"}}, []string{"query limit must be a valid integer"}},
		{"get_item", map[string]any{"path": id, "query": map[string]any{"limit": 2.5}}, []string{"query limit must be a valid integer"}},
		{"get_item", map[string]any{"path": id, "query": map[string]any{"limit": []int{1, 2}}}, []string{"query limit must be a single value, not a list"}},
		{"get_item", map[string]any{"path": id, "query": map[string]any{"verbose": "maybe"}}, []string{"query verbose must be true or false"}},
		{"get_item", map[string]any{"path": id, "body": map[string]any{}}, []string{"arguments body is not a part of the request this tool sends"}},
		{"create_item", map[string]any{"header": map[string]any{"X-Tenant": "a"}}, []string{"body  is required"}},
		{"create_item", map[string]any{"header": map[string]any{"X-Tenant": "a"}, "body": nil}, []string{"body  is required"}},
		{"create_item", map[string]any{"body": map[string]any{"name": "x", "price": 1}}, []string{"header X-Tenant is required"}},
		{"create_item", map[string]any{"header": map[string]any{"X-Tenant": "a"}, "body": map[string]any{"name": "x", "unknown": 1}}, []string{"body unknown"}},
		{"create_item", map[string]any{"header": map[string]any{"X-Tenant": "a"}, "body": map[string]any{"name": 5}}, []string{"body name"}},
		{"create_item", map[string]any{"header": map[string]any{"x-tenant": "a"}, "body": map[string]any{"name": "x", "price": 1}}, []string{"header x-tenant is not a parameter of this operation"}},
	} {
		got := m.call(tc.tool, tc.args)
		envelope := errorEnvelope(t, got)
		if envelope.Error.Status != http.StatusUnprocessableEntity || envelope.Error.Code != muzak.CodeValidationError {
			t.Fatalf("%v: envelope = %+v", tc.args, envelope)
		}
		var details []string
		for _, d := range envelope.Error.Details {
			details = append(details, strings.TrimSpace(d.Location+" "+d.Field+" "+d.Issue))
		}
		joined := strings.Join(details, "\n")
		for _, want := range tc.details {
			mustContain(t, joined, strings.TrimSpace(want))
		}
	}
	// The problems a client can send are bounded.
	many := map[string]any{"path": id}
	for i := range 100 {
		many[fmt.Sprintf("group%03d", i)] = i
	}
	if details := errorEnvelope(t, m.call("get_item", many)).Error.Details; len(details) != 32 {
		t.Fatalf("%d details, want the bound of 32", len(details))
	}
}

// tagsIn reads a list from the query.
type tagsIn struct {
	Tags   []string `query:"tag"`
	IDs    []int    `header:"X-Ids"`
	Labels []string `header:"X-Labels"`
}

// TestMCPCallLists sends a list argument entry by entry, an empty list as
// nothing, and refuses an entry that is not a scalar.
func TestMCPCallLists(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/tags", func(_ *muzak.Context, in tagsIn) (map[string]any, error) {
		return map[string]any{"tags": in.Tags, "ids": in.IDs, "nil": in.Tags == nil}, nil
	}, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	jsonEqual(t, m.call("get_tags", map[string]any{"query": map[string]any{"tag": []any{"a", 1, true}}, "header": map[string]any{"X-Ids": []int{4, 5}}}).StructuredContent,
		`{"tags":["a","1","true"],"ids":[4,5],"nil":false}`)
	jsonEqual(t, m.call("get_tags", map[string]any{"query": map[string]any{"tag": []any{}}}).StructuredContent, `{"tags":[],"ids":[],"nil":true}`)
	jsonEqual(t, m.call("get_tags", map[string]any{"query": map[string]any{"tag": "solo"}}).StructuredContent, `{"tags":["solo"],"ids":[],"nil":false}`)
	mustContain(t, m.call("get_tags", map[string]any{"query": map[string]any{"tag": []any{"a", map[string]any{}}}}).text(t),
		"entry 2 must be a string, a number or a boolean")
	mustContain(t, m.call("get_tags", map[string]any{"query": map[string]any{"tag": []any{nil}}}).text(t),
		"entry 1 must be a string, a number or a boolean")
	mustContain(t, m.call("get_tags", map[string]any{"header": map[string]any{"X-Labels": []string{"a,b"}}}).text(t),
		"is a list whose entries would not arrive as they are")
}

// TestMCPCallRefusesValuesNoRequestCarries answers a value the encoder will
// not write, because it would name another path or break a header, with an
// error result that names the field and never the value.
func TestMCPCallRefusesValuesNoRequestCarries(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	for args, why := range map[string]string{
		`{"path":{"id":"secret/value"}}`: `holds a "/"`,
		`{"path":{"id":".."}}`:           "dot segment",
		`{"path":{"id":""}}`:             "is empty",
		`{"header":{"X-Tenant":"secret\r\nX-Admin: 1"},"body":{"name":"x"}}`: "control character",
		`{"header":{"X-Tenant":" secretpadded "},"body":{"name":"x"}}`:       "begins or ends with a space",
	} {
		tool := "get_item"
		if strings.Contains(args, "header") {
			tool = "create_item"
		}
		var arguments map[string]any
		_ = json.Unmarshal([]byte(args), &arguments)
		got := m.call(tool, arguments)
		envelope := errorEnvelope(t, got)
		if envelope.Error.Status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d", args, envelope.Error.Status)
		}
		mustContain(t, envelope.Error.Message, why)
		if strings.Contains(got.text(t), "secret") || strings.Contains(envelope.Error.Message, "muzak:") {
			t.Fatalf("%s: the refusal quotes the value or the route: %s", args, got.text(t))
		}
	}
}

// TestMCPCallValidationFailureIsAResult answers the route's 422 as an error
// result carrying the envelope, whose request_id is the MCP request's.
func TestMCPCallValidationFailureIsAResult(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	res := m.post(m.message("tools/call", map[string]any{"name": "create_item", "arguments": map[string]any{
		"header": map[string]any{"X-Tenant": "acme"}, "body": map[string]any{"name": "", "price": 5000},
	}}))
	got := result[toolResult](t, decodeReply(t, res, http.StatusOK))
	envelope := errorEnvelope(t, got)
	if envelope.Error.Code != muzak.CodeValidationError || len(envelope.Error.Details) != 2 {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.RequestID == "" || envelope.RequestID != res.RequestID() {
		t.Fatalf("the tool's request_id %q is not the MCP request's %q", envelope.RequestID, res.RequestID())
	}
	if got.StructuredContent != nil {
		t.Fatal("an error result carries structured content, which a client would validate against the output schema")
	}
}

// wildcardIn reads a trailing wildcard.
type wildcardIn struct {
	Rest string `path:"rest"`
}

// TestMCPCallReachesItsOwnRouteAlone refuses a call whose arguments spell the
// path of another route, a static sibling of a parameter or anything under a
// wildcard, before the other route runs anything, and one that something
// above routing answers, here a middleware serving metrics.
func TestMCPCallReachesItsOwnRouteAlone(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	var purged, admin, metrics atomic.Int32
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/metrics") {
				metrics.Add(1)
				_, _ = w.Write([]byte("requests_total 7"))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Post("/things/{id}", func(_ *muzak.Context, in getItemIn) (shopItem, error) { return shopItem{ID: in.ID}, nil }, muzak.MCPTool())
	app.Post("/things/purge", func(*muzak.Context, muzak.Empty) (shopItem, error) { purged.Add(1); return shopItem{}, nil })
	app.Get("/files/{rest...}", func(_ *muzak.Context, in wildcardIn) (shopItem, error) { return shopItem{ID: in.Rest}, nil }, muzak.MCPTool())
	app.Get("/files/admin", func(*muzak.Context, muzak.Empty) (shopItem, error) { admin.Add(1); return shopItem{}, nil })
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")

	jsonEqual(t, m.call("post_things_by_id", map[string]any{"path": map[string]any{"id": "7"}}).StructuredContent, `{"id":"7","name":"","price":0}`)
	jsonEqual(t, m.call("get_files_by_rest", map[string]any{"path": map[string]any{"rest": "a/b"}}).StructuredContent, `{"id":"a/b","name":"","price":0}`)
	for tool, args := range map[string]map[string]any{
		"post_things_by_id": {"id": "purge"},
		"get_files_by_rest": {"rest": "admin"},
	} {
		mustContain(t, m.call(tool, map[string]any{"path": args}).text(t), "the arguments name a path this tool's operation does not answer")
	}
	got := m.call("get_files_by_rest", map[string]any{"path": map[string]any{"rest": "x/metrics"}})
	mustContain(t, got.text(t), "the arguments name a path this tool's operation does not answer")
	if strings.Contains(string(got.raw), "requests_total") {
		t.Fatalf("what the middleware answered leaked into a result: %s", got.raw)
	}
	if purged.Load() != 0 || admin.Load() != 0 || metrics.Load() != 1 {
		t.Fatalf("purge ran %d times, admin %d, metrics %d", purged.Load(), admin.Load(), metrics.Load())
	}
}

// TestMCPWildcardToolAndTheDocumentation refuses a call whose wildcard
// argument spells the documentation's path, which the documentation answers
// ahead of routing, so that a tool returns its own route's answers alone. A
// probe's path cannot be spelled at all: a route answering it is a build
// error.
func TestMCPWildcardToolAndTheDocumentation(t *testing.T) {
	options := quietMCPOptions()
	options.DocsUI = fstestDocs()
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/{rest...}", func(_ *muzak.Context, in wildcardIn) (shopItem, error) { return shopItem{ID: in.Rest}, nil }, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	for _, rest := range []string{"openapi.json", "docs"} {
		got := m.call("get_by_rest", map[string]any{"path": map[string]any{"rest": rest}})
		mustContain(t, got.text(t), "the arguments name a path this tool's operation does not answer")
		if strings.Contains(string(got.raw), "openapi\\\"") || strings.Contains(string(got.raw), "html") {
			t.Fatalf("the documentation leaked into a result: %s", got.raw)
		}
	}
	jsonEqual(t, m.call("get_by_rest", map[string]any{"path": map[string]any{"rest": "x"}}).StructuredContent, `{"id":"x","name":"","price":0}`)

	probes := quietMCPOptions()
	probes.Health = muzak.HealthOptions{Enabled: true}
	probed := muzak.New(probes)
	probed.MCP("/mcp", muzak.MCPOptions{})
	probed.Get("/{rest...}", func(*muzak.Context, wildcardIn) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	mustContain(t, fmt.Sprint(probed.Build()), "LivenessPath")
}

// hs256 signs claims as a JWT under secret.
func hs256(t *testing.T, secret []byte, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	data, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// jwtSecret signs the tokens of the authentication tests.
var jwtSecret = []byte("0123456789abcdef0123456789abcdef")

// tokenFor issues a token for subject.
func tokenFor(t *testing.T, subject string) string {
	return hs256(t, jwtSecret, map[string]any{
		"iss": "https://issuer.example.com", "aud": "https://api.example.com/mcp", "sub": subject,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
}

// jwtShop is an application whose tool route verifies bearer tokens, served
// by an endpoint that requires one when protect is set.
func jwtShop(logger *slog.Logger, protect bool) *muzak.App {
	options := quietMCPOptions()
	options.Logger = logger
	options.SecuritySchemes = map[string]muzak.SecurityScheme{"bearer": muzak.JWTBearer(muzak.JWTOptions{
		Issuers:          []string{"https://issuer.example.com"},
		Audience:         "https://api.example.com/mcp",
		Algorithms:       []string{"HS256"},
		Keys:             []muzak.JWTKey{{Secret: jwtSecret}},
		ResourceMetadata: &muzak.ResourceMetadata{Resource: "https://api.example.com/mcp"},
	})}
	app := muzak.New(options)
	var routeOpts []muzak.RouteOption
	if protect {
		routeOpts = append(routeOpts, muzak.WithSecurity(muzak.Require("bearer")))
	}
	app.MCP("/mcp", muzak.MCPOptions{}, routeOpts...)
	app.Get("/me", func(_ *muzak.Context, in meIn) (map[string]string, error) {
		return map[string]string{"subject": in.Claims.Get().Subject}, nil
	}, muzak.WithSecurity(muzak.Require("bearer")), muzak.MCPTool())
	return app
}

// meIn reads the verified claims.
type meIn struct {
	Claims muzak.Dep[*muzak.Claims]
}

// TestMCPCallCarriesTheBearerToken calls a route that requires a token: the
// call is refused with the route's 401 as an error result without one, and
// succeeds with the MCP request's own, which is never logged or echoed.
func TestMCPCallCarriesTheBearerToken(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(lockedWriter{&mu, &logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := newMCPClient(t, jwtShop(logger, false))
	m.initialize("2025-06-18")
	refused := errorEnvelope(t, m.call("get_me", nil))
	if refused.Error.Status != http.StatusUnauthorized || refused.Error.Code != muzak.CodeUnauthorized {
		t.Fatalf("without a token: %+v", refused)
	}

	token := tokenFor(t, "alice")
	authed := m.fork()
	authed.http = testclient.New(t, jwtShop(logger, false), testclient.WithHeader("Authorization", "Bearer "+token))
	authed.initialize("2025-06-18")
	got := authed.call("get_me", nil)
	jsonEqual(t, got.StructuredContent, `{"subject":"alice"}`)

	forged := m.fork()
	forged.http = testclient.New(t, jwtShop(logger, false), testclient.WithHeader("Authorization", "Bearer "+token+"x"))
	forged.initialize("2025-06-18")
	if envelope := errorEnvelope(t, forged.call("get_me", nil)); envelope.Error.Status != http.StatusUnauthorized {
		t.Fatalf("a forged token: %+v", envelope)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), token[:40]) {
		t.Fatalf("the token was logged:\n%s", logs.String())
	}
	if strings.Contains(string(got.raw), token[:40]) {
		t.Fatal("the token was echoed in a result")
	}
}

// lockedWriter serializes writes to a buffer shared by goroutines.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// TestMCPEndpointChallengesForAToken answers an unauthenticated request to a
// protected endpoint with the 401 whose challenge names the RFC 9728 metadata
// an MCP client discovers the authorization server by, and binds a session to
// the subject that opened it.
func TestMCPEndpointChallengesForAToken(t *testing.T) {
	app := jwtShop(slog.New(slog.DiscardHandler), true)
	m := newMCPClient(t, app)
	res := m.post(m.message("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{}}))
	res.AssertStatus(http.StatusUnauthorized)
	mustContain(t, res.Header.Get("WWW-Authenticate"), `Bearer`, `resource_metadata="https://api.example.com/.well-known/oauth-protected-resource/mcp"`)
	m.http.Get("/.well-known/oauth-protected-resource/mcp").AssertStatus(http.StatusOK)

	alice := tokenFor(t, "alice")
	m.http = testclient.New(t, jwtShop(slog.New(slog.DiscardHandler), true))
	bearer := func(token string) testclient.RequestOption {
		return testclient.Header("Authorization", "Bearer "+token)
	}
	init := m.message("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{}})
	opened := m.post(init, bearer(alice))
	opened.AssertStatus(http.StatusOK)
	m.session, m.version = opened.Header.Get(muzak.HeaderMCPSessionID), "2025-06-18"
	m.post(m.message("ping", nil), bearer(tokenFor(t, "alice"))).AssertStatus(http.StatusOK)
	m.post(m.message("ping", nil), bearer(tokenFor(t, "mallory"))).AssertStatus(http.StatusNotFound)
	m.http.Delete("/mcp", testclient.Header(muzak.HeaderMCPSessionID, m.session), bearer(tokenFor(t, "mallory"))).AssertStatus(http.StatusNotFound)
	got := result[toolResult](t, decodeReply(t, m.post(m.message("tools/call", map[string]any{"name": "get_me"}), bearer(alice)), http.StatusOK))
	jsonEqual(t, got.StructuredContent, `{"subject":"alice"}`)
	m.http.Delete("/mcp", testclient.Header(muzak.HeaderMCPSessionID, m.session), bearer(alice)).AssertStatus(http.StatusNoContent)
}

// TestMCPCallIsCountedAgainstTheRealClient counts a tool call against the
// route's rate limit as the client's own request, under the client's address,
// so calls through the endpoint and requests made directly share one budget.
func TestMCPCallIsCountedAgainstTheRealClient(t *testing.T) {
	options := quietMCPOptions()
	options.ClientIP = muzak.ClientIPOptions{TrustedProxies: []string{"127.0.0.1"}}
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/whoami", func(ctx *muzak.Context, _ muzak.Empty) (map[string]string, error) {
		return map[string]string{"ip": ctx.ClientIP()}, nil
	}, muzak.MCPTool(), muzak.RateLimit(muzak.Quota{Name: "whoami", Window: time.Minute, Limit: 2}))
	m := newMCPClient(t, app, testclient.WithHeader("X-Forwarded-For", "203.0.113.9"))
	m.initialize("2025-06-18")
	jsonEqual(t, m.call("get_whoami", nil).StructuredContent, `{"ip":"203.0.113.9"}`)
	m.http.Get("/whoami").AssertStatus(http.StatusOK)
	envelope := errorEnvelope(t, m.call("get_whoami", nil))
	if envelope.Error.Status != http.StatusTooManyRequests {
		t.Fatalf("the third request of the budget: %+v", envelope)
	}
	m.http.Get("/whoami").AssertStatus(http.StatusTooManyRequests)
}

// TestMCPCallRunsTheRouteWhole runs the route's guards and providers for a
// tool call, and its releases with the call's outcome.
func TestMCPCallRunsTheRouteWhole(t *testing.T) {
	var guarded atomic.Int32
	var mu sync.Mutex
	var released []string
	guard := func(ctx *muzak.Context) error {
		guarded.Add(1)
		if ctx.Header("Authorization") != "Bearer staff" {
			return muzak.Forbidden("staff only")
		}
		return nil
	}
	acquire := muzak.Acquire(func(*muzak.Context) (*strings.Builder, muzak.Release, error) {
		return &strings.Builder{}, func(failure error) error {
			mu.Lock()
			defer mu.Unlock()
			released = append(released, fmt.Sprint(failure))
			return nil
		}, nil
	})
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/staff/{id}", func(_ *muzak.Context, in getItemIn) (shopItem, error) {
		if in.ID == "fail" {
			return shopItem{}, errors.New("database is down")
		}
		return shopItem{ID: in.ID}, nil
	}, muzak.MCPTool(), muzak.WithDependencies(guard), acquire)
	anonymous := newMCPClient(t, app)
	anonymous.initialize("2025-06-18")
	if envelope := errorEnvelope(t, anonymous.call("get_staff_by_id", map[string]any{"path": map[string]any{"id": "1"}})); envelope.Error.Status != http.StatusForbidden {
		t.Fatalf("without the credential: %+v", envelope)
	}
	staff := anonymous.fork()
	staff.extra = []testclient.RequestOption{testclient.Header("Authorization", "Bearer staff")}
	staff.initialize("2025-06-18")
	staff.call("get_staff_by_id", map[string]any{"path": map[string]any{"id": "1"}})
	failed := errorEnvelope(t, staff.call("get_staff_by_id", map[string]any{"path": map[string]any{"id": "fail"}}))
	if failed.Error.Status != http.StatusInternalServerError || strings.Contains(failed.Error.Message, "database") {
		t.Fatalf("a failing handler: %+v", failed)
	}
	if guarded.Load() != 3 {
		t.Fatalf("the guard ran %d times, want 3", guarded.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(released) != 2 || released[0] != "<nil>" || !strings.Contains(released[1], "database is down") {
		t.Fatalf("releases = %q, want a success and the failure", released)
	}
}

// TestMCPCallPanicIsAnErrorResult answers a handler's panic with the
// application's opaque 500 as an error result, the session still usable.
func TestMCPCallPanicIsAnErrorResult(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/boom", func(*muzak.Context, muzak.Empty) (shopItem, error) { panic("password=hunter2 at 0xc000123") }, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	got := m.call("get_boom", nil)
	envelope := errorEnvelope(t, got)
	if envelope.Error.Code != muzak.CodeInternalError || strings.Contains(string(got.raw), "hunter2") || strings.Contains(string(got.raw), "0xc0") {
		t.Fatalf("the panic leaked: %s", got.raw)
	}
	m.send(http.StatusOK, "ping", nil)
}

// TestMCPCallIsCancelledWithItsRequest cancels a tool call's request when the
// MCP request it is part of goes away.
func TestMCPCallIsCancelledWithItsRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/slow", func(ctx *muzak.Context, _ muzak.Empty) (shopItem, error) {
		close(started)
		select {
		case <-ctx.Context().Done():
			cancelled <- ctx.Context().Err()
		case <-time.After(10 * time.Second):
			cancelled <- errors.New("never cancelled")
		}
		return shopItem{}, ctx.Context().Err()
	}, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.http.URL()+"/mcp",
		strings.NewReader(m.message("tools/call", map[string]any{"name": "get_slow"})))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	req.Header.Set(muzak.HeaderMCPSessionID, m.session)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := m.http.HTTPClient().Do(req)
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the tool call ended with %v, want its cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the MCP request did not cancel the tool call")
	}
	<-done
}

// endless is a body that never ends, counting what was read and whether it
// was closed.
type endless struct {
	read   atomic.Int64
	closed atomic.Bool
}

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.read.Add(int64(len(p)))
	return len(p), nil
}

func (e *endless) Close() error { e.closed.Store(true); return nil }

// TestMCPCallResultIsBounded ends a call whose answer is over MaxResultSize
// with an error result, and stops a stream that would never end.
func TestMCPCallResultIsBounded(t *testing.T) {
	body := &endless{}
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{MaxResultSize: 1024})
	app.Get("/big", func(*muzak.Context, muzak.Empty) (muzak.Bytes, error) {
		return muzak.Bytes{ContentType: "text/plain", Data: bytes.Repeat([]byte("y"), 1025)}, nil
	}, muzak.MCPTool())
	app.Get("/exact", func(*muzak.Context, muzak.Empty) (muzak.Bytes, error) {
		return muzak.Bytes{ContentType: "text/plain", Data: bytes.Repeat([]byte("y"), 1024)}, nil
	}, muzak.MCPTool())
	app.Get("/stream", func(*muzak.Context, muzak.Empty) (muzak.Stream, error) {
		return muzak.Stream{ContentType: "text/plain", Body: body}, nil
	}, muzak.MCPTool())
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")
	if got := m.call("get_exact", nil); got.IsError || len(got.text(t)) != 1024 {
		t.Fatalf("a result at the limit: %d bytes, error %t", len(got.text(t)), got.IsError)
	}
	for _, tool := range []string{"get_big", "get_stream"} {
		envelope := errorEnvelope(t, m.call(tool, nil))
		if envelope.Error.Status != http.StatusBadGateway {
			t.Fatalf("%s: %+v", tool, envelope)
		}
		mustContain(t, envelope.Error.Message, "more than the 1024 bytes a tool result may hold")
	}
	if !body.closed.Load() || body.read.Load() > 1<<20 {
		t.Fatalf("the stream read %d bytes and closed %t", body.read.Load(), body.closed.Load())
	}
}

// TestMCPCallResponseKinds answers each kind of body as a result may carry
// it, and refuses what a result cannot.
func TestMCPCallResponseKinds(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	var redirected atomic.Int32
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{})
	tool := muzak.MCPTool()
	app.Get("/image", func(*muzak.Context, muzak.Empty) (muzak.Bytes, error) {
		return muzak.Bytes{ContentType: "image/png", Data: png}, nil
	}, tool)
	app.Get("/audio", func(*muzak.Context, muzak.Empty) (muzak.Bytes, error) {
		return muzak.Bytes{ContentType: "audio/wav", Data: []byte("RIFF")}, nil
	}, tool)
	app.Get("/text", func(*muzak.Context, muzak.Empty) (muzak.Bytes, error) {
		return muzak.Bytes{ContentType: "text/csv; charset=utf-8", Data: []byte("a,b\n1,\xff")}, nil
	}, tool)
	app.Get("/pdf", func(*muzak.Context, muzak.Empty) (muzak.Bytes, error) {
		return muzak.Bytes{ContentType: "application/pdf", Data: []byte("%PDF")}, nil
	}, tool)
	app.Get("/page", func(*muzak.Context, muzak.Empty) (muzak.HTML, error) { return "<p>hi</p>", nil }, tool)
	app.Get("/moved", func(*muzak.Context, muzak.Empty) (muzak.Redirect, error) {
		return muzak.Redirect{To: "/target", Status: http.StatusFound}, nil
	}, tool)
	app.Get("/target", func(*muzak.Context, muzak.Empty) (shopItem, error) { redirected.Add(1); return shopItem{}, nil })
	app.Get("/raw", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		_, err := ctx.ResponseWriter().Write([]byte("plain words"))
		return muzak.Empty{}, err
	}, tool)
	app.Get("/broken", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		ctx.ResponseWriter().Header().Set("Content-Type", "application/json")
		_, err := ctx.ResponseWriter().Write([]byte(`{"broken":`))
		return muzak.Empty{}, err
	}, tool)
	app.Get("/mismatch", func(ctx *muzak.Context, _ muzak.Empty) (shopItem, error) {
		ctx.ResponseWriter().Header().Set("Content-Type", "application/json")
		_, err := ctx.ResponseWriter().Write([]byte(`[1,2]`))
		return shopItem{}, err
	}, tool)
	app.Get("/down", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		ctx.ResponseWriter().Header().Set("Content-Type", "text/plain")
		ctx.ResponseWriter().WriteHeader(http.StatusServiceUnavailable)
		_, err := ctx.ResponseWriter().Write([]byte("down for maintenance"))
		return muzak.Empty{}, err
	}, tool)
	app.Get("/opaque", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		ctx.ResponseWriter().Header().Set("Content-Type", "image/png")
		ctx.ResponseWriter().WriteHeader(http.StatusTeapot)
		_, err := ctx.ResponseWriter().Write(png)
		return muzak.Empty{}, err
	}, tool)
	app.Get("/partial", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		ctx.ResponseWriter().Header().Set("Content-Type", "text/plain")
		_, _ = ctx.ResponseWriter().Write([]byte("rows 1 to 10"))
		return muzak.Empty{}, errors.New("the database went away at row 11")
	}, tool)
	m := newMCPClient(t, app)
	m.initialize("2025-06-18")

	image := m.call("get_image", nil)
	if len(image.Content) != 1 || image.Content[0].Type != "image" || image.Content[0].MimeType != "image/png" ||
		image.Content[0].Data != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("image = %s", image.raw)
	}
	if audio := m.call("get_audio", nil); audio.Content[0].Type != "audio" || audio.Content[0].MimeType != "audio/wav" {
		t.Fatalf("audio = %s", audio.raw)
	}
	if text := m.call("get_text", nil).text(t); text != "a,b\n1,\uFFFD" {
		t.Fatalf("text = %q", text)
	}
	if page := m.call("get_page", nil).text(t); page != "<p>hi</p>" {
		t.Fatalf("html = %q", page)
	}
	moved := m.call("get_moved", nil)
	if moved.IsError || moved.text(t) != "/target" || redirected.Load() != 0 {
		t.Fatalf("a redirect is its Location, not followed: %s, target ran %d times", moved.raw, redirected.Load())
	}
	if raw := m.call("get_raw", nil).text(t); raw != "plain words" {
		t.Fatalf("an unlabelled body = %q", raw)
	}
	for tool, message := range map[string]string{
		"get_pdf":      `the operation answered with \"application/pdf\", which a tool result cannot carry`,
		"get_broken":   "labelled JSON that is not valid JSON",
		"get_mismatch": "a body its output schema does not describe",
		"get_opaque":   "the operation failed with status 418",
		"get_partial":  "the operation failed after its answer had started",
	} {
		mustContain(t, m.call(tool, nil).text(t), message)
	}
	down := m.call("get_down", nil)
	if !down.IsError || down.text(t) != "down for maintenance" {
		t.Fatalf("a text failure = %s", down.raw)
	}
}

// TestMCPCallForwardsOnlyWhatItShould sends a tool call's request the
// credentials and client details it needs, and nothing else the MCP request
// carried.
func TestMCPCallForwardsOnlyWhatItShould(t *testing.T) {
	app := muzak.New(quietMCPOptions())
	app.MCP("/mcp", muzak.MCPOptions{ForwardHeaders: []string{"X-API-Key"}, ForwardCookies: []string{"session"}})
	app.Get("/echo", func(ctx *muzak.Context, _ muzak.Empty) (map[string]string, error) {
		out := map[string]string{}
		for _, name := range []string{"Authorization", "X-Api-Key", "X-Other", "Cookie", "Origin", muzak.HeaderMCPSessionID,
			muzak.HeaderMCPProtocolVersion, "User-Agent", "Accept-Language", "Accept", "Content-Type", "Mcp-Method"} {
			out[name] = strings.Join(ctx.Request().Header.Values(name), "|")
		}
		out["request_id"] = ctx.RequestID()
		out["remote"] = strings.Split(ctx.Request().RemoteAddr, ":")[0]
		return out, nil
	}, muzak.MCPTool())
	m := newMCPClient(t, app,
		testclient.WithHeader("Authorization", "Bearer abc"),
		testclient.WithHeader("X-API-Key", "key-1"),
		testclient.WithHeader("X-Other", "nope"),
		testclient.WithHeader("Cookie", "session=s1; tracking=t2"),
		testclient.WithHeader("User-Agent", "agent/1.0"),
		testclient.WithHeader("Accept-Language", "de"),
		testclient.WithoutCookies())
	m.initialize("2025-06-18")
	res := m.post(m.message("tools/call", map[string]any{"name": "get_echo"}), testclient.Header("Origin", "http://"+strings.TrimPrefix(m.http.URL(), "http://")))
	got := result[toolResult](t, decodeReply(t, res, http.StatusOK))
	var echoed map[string]string
	_ = json.Unmarshal(got.StructuredContent, &echoed)
	want := map[string]string{
		"Authorization": "Bearer abc", "X-Api-Key": "key-1", "X-Other": "", "Cookie": "session=s1", "Origin": "",
		muzak.HeaderMCPSessionID: "", muzak.HeaderMCPProtocolVersion: "", "User-Agent": "agent/1.0", "Accept-Language": "de",
		"Accept": "application/json", "Content-Type": "", "Mcp-Method": "", "request_id": res.RequestID(), "remote": "127.0.0.1",
	}
	jsonEqual(t, echoed, want)
}

// TestMCPCallBehindAProxy judges a tool call by the scheme and address a
// trusted proxy named for the MCP request, so a call is not redirected to
// https when the MCP request came over it, nor refused for its host.
func TestMCPCallBehindAProxy(t *testing.T) {
	options := quietMCPOptions()
	options.AllowedHosts = []string{"api.example.com"}
	options.RedirectHTTPS = &muzak.RedirectHTTPSOptions{}
	options.ClientIP = muzak.ClientIPOptions{TrustedProxies: []string{"127.0.0.1"}}
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/secure", func(ctx *muzak.Context, _ muzak.Empty) (map[string]string, error) {
		return map[string]string{"host": ctx.Request().Host, "ip": ctx.ClientIP()}, nil
	}, muzak.MCPTool())
	m := newMCPClient(t, app,
		testclient.WithHeader("Host", "api.example.com"),
		testclient.WithHeader("X-Forwarded-Proto", "https"),
		testclient.WithHeader("X-Forwarded-For", "192.0.2.4"))
	m.initialize("2025-06-18")
	jsonEqual(t, m.call("get_secure", nil).StructuredContent, `{"host":"api.example.com","ip":"192.0.2.4"}`)
}

// recordingTracer records the spans it is asked to start.
type recordingTracer struct {
	mu     sync.Mutex
	starts []muzak.SpanStart
}

func (r *recordingTracer) StartSpan(_ context.Context, start muzak.SpanStart) muzak.Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, start)
	return noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetName(string)                         {}
func (noopSpan) SetAttributes(...slog.Attr)             {}
func (noopSpan) AddEvent(string, ...slog.Attr)          {}
func (noopSpan) SetStatus(muzak.SpanStatusCode, string) {}
func (noopSpan) End()                                   {}

// TestMCPCallSpanIsAChildOfTheRequest starts a tool call's server span under
// the MCP request's, whatever the policy says of traceparents clients send.
func TestMCPCallSpanIsAChildOfTheRequest(t *testing.T) {
	tracer := &recordingTracer{}
	options := quietMCPOptions()
	options.Tracing = muzak.TracingOptions{Tracer: tracer, Parent: muzak.TraceParentIgnore}
	app := muzak.New(options)
	app.MCP("/mcp", muzak.MCPOptions{})
	app.Get("/traced", func(*muzak.Context, muzak.Empty) (shopItem, error) { return shopItem{}, nil }, muzak.MCPTool())
	m := newMCPClient(t, app, testclient.WithHeader("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"))
	m.initialize("2025-06-18")
	tracer.mu.Lock()
	tracer.starts = nil
	tracer.mu.Unlock()
	m.call("get_traced", nil)
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	if len(tracer.starts) != 2 {
		t.Fatalf("%d spans, want the MCP request's and the call's", len(tracer.starts))
	}
	inner, outer := tracer.starts[1], tracer.starts[0]
	if inner.Parent.SpanID != outer.SpanContext.SpanID || inner.SpanContext.TraceID != outer.SpanContext.TraceID {
		t.Fatalf("the call's span is not a child of the request's: %+v under %+v", inner, outer)
	}
	if outer.SpanContext.TraceID.String() == "0af7651916cd43dd8448eb211c80319c" {
		t.Fatal("the client's traceparent was continued despite TraceParentIgnore")
	}
}

// TestMCPConcurrentCalls calls tools from many goroutines in one session,
// which the race detector checks.
func TestMCPConcurrentCalls(t *testing.T) {
	m := newMCPClient(t, shopApp(muzak.MCPOptions{}))
	m.initialize("2025-06-18")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 5 {
				id := fmt.Sprintf("%d-%d", i, j)
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"get_item","arguments":{"path":{"id":%q}}}}`, i*100+j, id)
				res := m.http.Post("/mcp", append([]testclient.RequestOption{testclient.RawJSON(body)}, m.headers()...)...)
				if !strings.Contains(res.String(), id) {
					t.Errorf("call %s answered %s", id, res.Body)
				}
			}
		})
	}
	wg.Wait()
}

// fstestDocs is a documentation UI for the tests that serve one.
func fstestDocs() fstest.MapFS {
	return fstest.MapFS{"index.html": {Data: []byte(`<html><script>window.x="/__muzak_spec__"</script></html>`)}}
}

// io is used by the endless body's interface checks.
var _ io.ReadCloser = (*endless)(nil)
