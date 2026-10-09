package muzak

import (
	"slices"
	"strings"
	"testing"
)

// These tests cover the rules that judge everything around a schema: paths,
// operations, parameters, bodies, statuses, media types and security.

func compatOp(id string) *Operation {
	return &Operation{OperationID: id, Responses: map[string]*Response{"204": {Description: "No Content"}}}
}

func TestPathsAndOperations(t *testing.T) {
	t.Parallel()
	deprecated := compatOp("c")
	deprecated.Deprecated = true
	old := compatDoc(map[string]*PathItem{
		"/a":     {Get: compatOp("a"), Post: compatOp("a2")},
		"/b":     {Get: compatOp("b")},
		"/c":     {Get: deprecated},
		"/empty": {},
	}, nil)
	cur := compatDoc(map[string]*PathItem{
		"/a": {Get: compatOp("a"), Put: compatOp("a3")},
		"/d": {Delete: compatOp("d")},
	}, nil)
	got := CompareDocuments(old, cur)
	assertAPIChanges(t, got,
		wantChange{Breaking, "operation-removed", "/paths/~1a/post"},
		wantChange{Compatible, "operation-added", "/paths/~1a/put"},
		wantChange{Breaking, "path-removed", "/paths/~1b"},
		wantChange{PossiblyBreaking, "path-removed", "/paths/~1c"},
		wantChange{Compatible, "path-added", "/paths/~1d"},
	)
	if got[0].Severity != Breaking || got[len(got)-1].Severity != Compatible {
		t.Errorf("changes are not sorted from the most serious: %v", got)
	}
}

func TestRemovedDeprecatedOperationIsPossiblyBreaking(t *testing.T) {
	t.Parallel()
	deprecated := compatOp("a2")
	deprecated.Deprecated = true
	old := compatDoc(map[string]*PathItem{"/a": {Get: compatOp("a"), Post: deprecated}}, nil)
	cur := compatDoc(map[string]*PathItem{"/a": {Get: compatOp("a")}}, nil)
	got := CompareDocuments(old, cur)
	assertAPIChanges(t, got, wantChange{PossiblyBreaking, "operation-removed", "/paths/~1a/post"})
	if !strings.Contains(got[0].Message, "deprecated") {
		t.Errorf("message = %q, want it to say the operation was deprecated", got[0].Message)
	}
}

// TestPathParameterRenamed checks that a path differing only in the names of
// its parameters is the same path, and that its parameters are paired by
// position.
func TestPathParameterRenamed(t *testing.T) {
	t.Parallel()
	read := func(name string, schema *Schema) *Operation {
		o := compatOp("read")
		o.Parameters = []Parameter{{Name: name, In: "path", Required: true, Schema: schema}}
		return o
	}
	old := compatDoc(map[string]*PathItem{"/users/{id}/{$}": {Get: read("id", compatStr())}}, nil)
	cur := compatDoc(map[string]*PathItem{"/users/{userID}/{$}": {Get: read("userID", &Schema{Type: "string", MaxLength: compatPtr(8)})}}, nil)
	assertAPIChanges(t, CompareDocuments(old, cur),
		wantChange{Compatible, "path-renamed", "/paths/~1users~1{id}~1{$}"},
		wantChange{Breaking, "request-max-length-tightened", "/paths/~1users~1{id}~1{$}/get/parameters/path/id/schema/maxLength"},
	)

	// When two paths of one shape are left over, which one a path became
	// cannot be told, so neither is paired.
	ambiguous := compatDoc(map[string]*PathItem{
		"/users/{a}/{$}": {Get: read("a", compatStr())},
		"/users/{b}/{$}": {Get: read("b", compatStr())},
	}, nil)
	assertAPIChanges(t, CompareDocuments(old, ambiguous),
		wantChange{Breaking, "path-removed", "/paths/~1users~1{id}~1{$}"},
		wantChange{Compatible, "path-added", "/paths/~1users~1{a}~1{$}"},
		wantChange{Compatible, "path-added", "/paths/~1users~1{b}~1{$}"},
	)
}

// TestRemovedVersionNamesTheOneStillServed checks the hint a removed versioned
// path carries.
func TestRemovedVersionNamesTheOneStillServed(t *testing.T) {
	t.Parallel()
	old := compatDoc(map[string]*PathItem{"/v1/cats": {Get: compatOp("v1")}, "/v2/cats": {Get: compatOp("v2")}, "/v3/dogs": {Get: compatOp("v3")}}, nil)
	cur := compatDoc(map[string]*PathItem{"/v2/cats": {Get: compatOp("v2")}, "/items/{id}": {Get: compatOp("i")}}, nil)
	got := CompareDocuments(old, cur)
	assertAPIChanges(t, got,
		wantChange{Breaking, "path-removed", "/paths/~1v1~1cats"},
		wantChange{Breaking, "path-removed", "/paths/~1v3~1dogs"},
		wantChange{Compatible, "path-added", "/paths/~1items~1{id}"},
	)
	for _, c := range got {
		switch c.Location {
		case "/paths/~1v1~1cats":
			if !strings.Contains(c.Message, `another version of it is served at "/v2/cats"`) {
				t.Errorf("message = %q, want it to name /v2/cats", c.Message)
			}
		case "/paths/~1v3~1dogs":
			if strings.Contains(c.Message, "another version") {
				t.Errorf("message = %q names a version that does not exist", c.Message)
			}
		}
	}
}

// TestVersionedApplication drives the versioning rules through real
// applications: adding a version is compatible, dropping the old one is not.
func TestVersionedApplication(t *testing.T) {
	t.Parallel()
	build := func(versions ...Version) *Document {
		opts := quietOptions()
		opts.Versioning = VersioningOptions{Type: VersioningURI}
		app := New(opts)
		app.Get("/cats", func(*Context, Empty) (compatCreated, error) { return compatCreated{}, nil }, WithVersion(versions...))
		doc, err := app.Document()
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	v1, both, v2 := build("1"), build("1", "2"), build("2")
	assertAPIChanges(t, CompareDocuments(v1, both), wantChange{Compatible, "path-added", "/paths/~1v2~1cats"})
	got := CompareDocuments(both, v2)
	assertAPIChanges(t, got, wantChange{Breaking, "path-removed", "/paths/~1v1~1cats"})
	if !strings.Contains(got[0].Message, `"/v2/cats"`) {
		t.Errorf("message = %q, want it to name the version still served", got[0].Message)
	}
}

func TestOperationMetadata(t *testing.T) {
	t.Parallel()
	before := compatOp("listItems")
	before.Tags = []string{"items", "legacy"}
	before.Summary = "List"
	after := compatOp("list_items")
	after.Tags = []string{"items", "catalogue"}
	after.Deprecated = true
	after.Summary = "List every item"
	after.Description = "Prose is not compared."
	old := compatDoc(map[string]*PathItem{"/items": {Get: before}}, nil)
	cur := compatDoc(map[string]*PathItem{"/items": {Get: after}}, nil)
	assertAPIChanges(t, CompareDocuments(old, cur),
		wantChange{PossiblyBreaking, "operation-id-changed", "/paths/~1items/get"},
		wantChange{Compatible, "operation-deprecated", "/paths/~1items/get"},
		wantChange{Compatible, "operation-tag-removed", "/paths/~1items/get/tags"},
		wantChange{Compatible, "operation-tag-added", "/paths/~1items/get/tags"},
	)
	assertAPIChanges(t, CompareDocuments(cur, old),
		wantChange{PossiblyBreaking, "operation-id-changed", "/paths/~1items/get"},
		wantChange{Compatible, "operation-undeprecated", "/paths/~1items/get"},
		wantChange{Compatible, "operation-tag-removed", "/paths/~1items/get/tags"},
		wantChange{Compatible, "operation-tag-added", "/paths/~1items/get/tags"},
	)
}

func TestServersAndTags(t *testing.T) {
	t.Parallel()
	old := compatDoc(map[string]*PathItem{}, nil)
	old.Servers = []Server{{URL: "https://a.example"}, {URL: "https://b.example"}}
	old.Tags = []Tag{{Name: "items"}, {Name: "legacy"}}
	cur := compatDoc(map[string]*PathItem{}, nil)
	cur.Servers = []Server{{URL: "https://b.example", Description: "renamed"}, {URL: "https://c.example"}}
	cur.Tags = []Tag{{Name: "items", Description: "Prose is not compared."}, {Name: "catalogue"}}
	assertAPIChanges(t, CompareDocuments(old, cur),
		wantChange{PossiblyBreaking, "server-removed", "/servers"},
		wantChange{Compatible, "server-added", "/servers"},
		wantChange{Compatible, "tag-removed", "/tags"},
		wantChange{Compatible, "tag-added", "/tags"},
	)
}

func compatParamDoc(parameters ...Parameter) *Document {
	o := compatOp("list")
	o.Parameters = parameters
	return compatDoc(map[string]*PathItem{"/items": {Get: o}}, nil)
}

func TestParameters(t *testing.T) {
	t.Parallel()
	at := "/paths/~1items/get/parameters/"
	integer := &Schema{Type: "integer"}
	tests := []struct {
		name     string
		old, cur []Parameter
		want     []wantChange
	}{
		{"removed", []Parameter{{Name: "limit", In: "query", Schema: integer}}, nil,
			[]wantChange{{Breaking, "parameter-removed", at + "query/limit"}}},
		{"added optional", nil, []Parameter{{Name: "limit", In: "query", Schema: integer}},
			[]wantChange{{Compatible, "parameter-added", at + "query/limit"}}},
		{"added required", nil, []Parameter{{Name: "cursor", In: "query", Required: true, Schema: compatStr()}},
			[]wantChange{{Breaking, "parameter-added", at + "query/cursor"}}},
		{"became required", []Parameter{{Name: "limit", In: "query", Schema: integer}}, []Parameter{{Name: "limit", In: "query", Required: true, Schema: integer}},
			[]wantChange{{Breaking, "parameter-became-required", at + "query/limit"}}},
		{"became optional", []Parameter{{Name: "limit", In: "query", Required: true, Schema: integer}}, []Parameter{{Name: "limit", In: "query", Schema: integer}},
			[]wantChange{{Compatible, "parameter-became-optional", at + "query/limit"}}},
		{"moved to a header", []Parameter{{Name: "token", In: "query", Schema: compatStr()}}, []Parameter{{Name: "Token", In: "header", Schema: compatStr()}},
			[]wantChange{{Breaking, "parameter-location-changed", at + "query/token"}}},
		{"header names ignore case", []Parameter{{Name: "X-Token", In: "header", Schema: compatStr()}}, []Parameter{{Name: "x-token", In: "header", Schema: compatStr()}},
			nil},
		{"text widened from an integer to a string", []Parameter{{Name: "id", In: "query", Schema: integer}}, []Parameter{{Name: "id", In: "query", Schema: compatStr()}},
			[]wantChange{{Compatible, "request-type-widened", at + "query/id/schema/type"}}},
		{"text narrowed from a string to an integer", []Parameter{{Name: "id", In: "query", Schema: compatStr()}}, []Parameter{{Name: "id", In: "query", Schema: integer}},
			[]wantChange{{Breaking, "request-type-narrowed", at + "query/id/schema/type"}}},
		{"list elements narrowed",
			[]Parameter{{Name: "id", In: "query", Schema: &Schema{Type: "array", Items: compatStr()}}},
			[]Parameter{{Name: "id", In: "query", Schema: &Schema{Type: "array", Items: &Schema{Type: "boolean"}}}},
			[]wantChange{{Breaking, "request-type-narrowed", at + "query/id/schema/items/type"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAPIChanges(t, CompareDocuments(compatParamDoc(tt.old...), compatParamDoc(tt.cur...)), tt.want...)
		})
	}
}

// TestDuplicateParametersKeepTheFirst covers a list naming one parameter
// twice, which no Muzak document does: the first is the one compared.
func TestDuplicateParametersKeepTheFirst(t *testing.T) {
	t.Parallel()
	old := compatParamDoc(Parameter{Name: "a", In: "query", Schema: compatStr()}, Parameter{Name: "a", In: "query", Schema: &Schema{Type: "integer"}})
	cur := compatParamDoc(Parameter{Name: "a", In: "query", Schema: compatStr()})
	assertAPIChanges(t, CompareDocuments(old, cur))
}

func compatBodyDoc(body *RequestBody) *Document {
	o := compatOp("create")
	o.RequestBody = body
	return compatDoc(map[string]*PathItem{"/items": {Post: o}}, nil)
}

func TestRequestBody(t *testing.T) {
	t.Parallel()
	at := "/paths/~1items/post/requestBody"
	json := func(required bool) *RequestBody {
		return &RequestBody{Required: required, Content: map[string]MediaType{"application/json": {Schema: compatStr()}}}
	}
	form := func(schema *Schema) *RequestBody {
		return &RequestBody{Required: true, Content: map[string]MediaType{"multipart/form-data": {Schema: compatObj(map[string]*Schema{"n": schema})}}}
	}
	tests := []struct {
		name     string
		old, cur *RequestBody
		want     []wantChange
	}{
		{"none", nil, nil, nil},
		{"required body added", nil, json(true), []wantChange{{Breaking, "request-body-added", at}}},
		{"optional body added", nil, json(false), []wantChange{{Compatible, "request-body-added", at}}},
		{"body removed", json(true), nil, []wantChange{{Breaking, "request-body-removed", at}}},
		{"became required", json(false), json(true), []wantChange{{Breaking, "request-body-became-required", at}}},
		{"became optional", json(true), json(false), []wantChange{{Compatible, "request-body-became-optional", at}}},
		{"media type removed", json(true), form(compatStr()), []wantChange{
			{Breaking, "request-media-type-removed", at + "/content/application~1json"},
			{Compatible, "request-media-type-added", at + "/content/multipart~1form-data"},
		}},
		{"form value widened to text", form(&Schema{Type: "integer"}), form(compatStr()), []wantChange{
			{Compatible, "request-type-widened", at + "/content/multipart~1form-data/schema/properties/n/type"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAPIChanges(t, CompareDocuments(compatBodyDoc(tt.old), compatBodyDoc(tt.cur)), tt.want...)
		})
	}
}

func compatResponseDoc(responses map[string]*Response) *Document {
	o := compatOp("read")
	o.Responses = responses
	return compatDoc(map[string]*PathItem{"/items": {Get: o}}, nil)
}

func TestResponses(t *testing.T) {
	t.Parallel()
	at := "/paths/~1items/get/responses/"
	body := func(media string) *Response {
		return &Response{Description: "x", Content: map[string]MediaType{media: {Schema: compatStr()}}}
	}
	tests := []struct {
		name     string
		old, cur map[string]*Response
		want     []wantChange
	}{
		{"success removed", map[string]*Response{"200": body("application/json")}, map[string]*Response{"201": body("application/json")}, []wantChange{
			{Breaking, "response-status-removed", at + "200"},
			{PossiblyBreaking, "response-status-added", at + "201"},
		}},
		{"switching protocols removed", map[string]*Response{"101": {}}, map[string]*Response{}, []wantChange{{Breaking, "response-status-removed", at + "101"}}},
		{"error removed and added", map[string]*Response{"404": body("application/json"), "default": body("application/json")}, map[string]*Response{"409": body("application/json")}, []wantChange{
			{Compatible, "response-status-removed", at + "404"},
			{Compatible, "response-status-removed", at + "default"},
			{Compatible, "response-status-added", at + "409"},
		}},
		{"media type changed", map[string]*Response{"200": body("application/json")}, map[string]*Response{"200": body("text/html")}, []wantChange{
			{Breaking, "response-media-type-removed", at + "200/content/application~1json"},
			{Compatible, "response-media-type-added", at + "200/content/text~1html"},
		}},
		{"body gone", map[string]*Response{"200": body("application/json")}, map[string]*Response{"200": {Description: "x"}}, []wantChange{
			{Breaking, "response-media-type-removed", at + "200/content/application~1json"},
		}},
		{"body new", map[string]*Response{"200": nil}, map[string]*Response{"200": body("text/event-stream")}, []wantChange{
			{Compatible, "response-media-type-added", at + "200/content/text~1event-stream"},
		}},
		{"schema compared as a response", map[string]*Response{"200": body("application/json")}, map[string]*Response{"200": {Content: map[string]MediaType{"application/json": {Schema: &Schema{Type: []string{"string", "null"}}}}}}, []wantChange{
			{Breaking, "response-nullable-added", at + "200/content/application~1json/schema"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAPIChanges(t, CompareDocuments(compatResponseDoc(tt.old), compatResponseDoc(tt.cur)), tt.want...)
		})
	}
}

// TestResponseHeaders covers the headers a response documents, such as the
// Location of a redirect, which a client reads as it reads a member of the
// body: one that is gone, or may now be absent, breaks a client that relied
// on it, and its value, which travels as text, may only narrow.
func TestResponseHeaders(t *testing.T) {
	t.Parallel()
	at := "/paths/~1items/get/responses/302/headers/"
	redirect := func(headers map[string]*ResponseHeader) map[string]*Response {
		return map[string]*Response{"302": {Description: "Found", Headers: headers}}
	}
	header := func(required bool, schema *Schema) *ResponseHeader {
		return &ResponseHeader{Required: required, Schema: schema}
	}
	tests := []struct {
		name     string
		old, cur map[string]*ResponseHeader
		want     []wantChange
	}{
		{"required header removed", map[string]*ResponseHeader{"Location": header(true, compatStr())}, nil, []wantChange{
			{Breaking, "response-header-removed", at + "Location"},
		}},
		{"optional header removed", map[string]*ResponseHeader{"Retry-After": header(false, compatStr())}, map[string]*ResponseHeader{}, []wantChange{
			{PossiblyBreaking, "response-header-removed", at + "Retry-After"},
		}},
		{"header added", nil, map[string]*ResponseHeader{"Location": header(true, compatStr())}, []wantChange{
			{Compatible, "response-header-added", at + "Location"},
		}},
		{"header may now be absent", map[string]*ResponseHeader{"Location": header(true, compatStr())}, map[string]*ResponseHeader{"Location": header(false, compatStr())}, []wantChange{
			{Breaking, "response-header-became-optional", at + "Location"},
		}},
		{"header now always sent", map[string]*ResponseHeader{"Location": header(false, compatStr())}, map[string]*ResponseHeader{"Location": header(true, compatStr())}, []wantChange{
			{Compatible, "response-header-became-required", at + "Location"},
		}},
		{"name matched without regard to case", map[string]*ResponseHeader{"Location": header(true, compatStr())}, map[string]*ResponseHeader{"location": header(true, compatStr())}, nil},
		{"value widened", map[string]*ResponseHeader{"X-Count": header(true, &Schema{Type: "integer"})}, map[string]*ResponseHeader{"X-Count": header(true, compatStr())}, []wantChange{
			{Breaking, "response-type-widened", at + "X-Count/schema/type"},
		}},
		{"value narrowed", map[string]*ResponseHeader{"X-Count": header(true, compatStr())}, map[string]*ResponseHeader{"X-Count": header(true, &Schema{Type: "integer"})}, []wantChange{
			{Compatible, "response-type-narrowed", at + "X-Count/schema/type"},
		}},
		{"nil entry is no header", map[string]*ResponseHeader{"X-Gone": nil}, map[string]*ResponseHeader{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAPIChanges(t, CompareDocuments(compatResponseDoc(redirect(tt.old)), compatResponseDoc(redirect(tt.cur))), tt.want...)
		})
	}
}

func compatSecurityDoc(schemes map[string]SecurityScheme, requirements []SecurityRequirement) *Document {
	o := compatOp("read")
	o.Security = requirements
	doc := compatDoc(map[string]*PathItem{"/items": {Get: o}}, nil)
	doc.Components = &Components{SecuritySchemes: schemes}
	return doc
}

func TestSecurity(t *testing.T) {
	t.Parallel()
	at := "/paths/~1items/get/security"
	schemes := map[string]SecurityScheme{
		"bearer": BearerAuth("JWT"),
		"key":    APIKeyHeader("X-Key"),
		"oauth":  OAuth2(OAuthFlows{ClientCredentials: &OAuthFlow{TokenURL: "https://a.example/token", Scopes: map[string]string{"read": "", "write": ""}}}),
	}
	bearer := []SecurityRequirement{Require("bearer")}
	tests := []struct {
		name     string
		old, cur []SecurityRequirement
		want     []wantChange
	}{
		{"declared where nothing was", nil, bearer, []wantChange{{Breaking, "security-tightened", at}}},
		{"dropped", bearer, nil, []wantChange{{Compatible, "security-loosened", at}}},
		{"public either way", nil, []SecurityRequirement{}, nil},
		{"an alternative removed", []SecurityRequirement{Require("bearer"), Require("key")}, bearer, []wantChange{{Breaking, "security-tightened", at}}},
		{"an alternative added", bearer, []SecurityRequirement{Require("bearer"), Require("key")}, []wantChange{{Compatible, "security-loosened", at}}},
		{"a combination relaxed", []SecurityRequirement{{"bearer": nil, "key": nil}}, bearer, []wantChange{{Compatible, "security-loosened", at}}},
		{"a combination demanded", bearer, []SecurityRequirement{{"bearer": nil, "key": nil}}, []wantChange{{Breaking, "security-tightened", at}}},
		{"a scope demanded", []SecurityRequirement{Require("oauth", "read")}, []SecurityRequirement{Require("oauth", "read", "write")}, []wantChange{{Breaking, "security-tightened", at}}},
		{"same requirement", []SecurityRequirement{Require("oauth", "read")}, []SecurityRequirement{Require("oauth", "read")}, nil},
		{"an undeclared scheme compared by name", []SecurityRequirement{Require("ghost")}, []SecurityRequirement{Require("ghost")}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAPIChanges(t, CompareDocuments(compatSecurityDoc(schemes, tt.old), compatSecurityDoc(schemes, tt.cur)), tt.want...)
		})
	}
}

func TestSecurityMessages(t *testing.T) {
	t.Parallel()
	schemes := map[string]SecurityScheme{"oauth": OAuth2(OAuthFlows{Password: &OAuthFlow{TokenURL: "https://a.example/token", Scopes: map[string]string{"read": ""}}})}
	got := CompareDocuments(compatSecurityDoc(schemes, []SecurityRequirement{Require("oauth", "read")}), compatSecurityDoc(schemes, []SecurityRequirement{Require("oauth", "read", "admin")}))
	if len(got) != 1 || got[0].Message != `The operation no longer accepts credentials for "oauth" with "read".` {
		t.Errorf("changes = %+v", got)
	}
	got = CompareDocuments(compatSecurityDoc(schemes, nil), compatSecurityDoc(schemes, []SecurityRequirement{Require("oauth")}))
	if len(got) != 1 || got[0].Message != "The operation no longer accepts a request with no credentials." {
		t.Errorf("changes = %+v", got)
	}
}

// TestSecuritySchemes checks that a scheme is compared by what it asks a
// client for: renaming one changes nothing a client sends, while changing
// the header it is read from breaks every client configured for it.
func TestSecuritySchemes(t *testing.T) {
	t.Parallel()
	renamedOld := compatSecurityDoc(map[string]SecurityScheme{"bearer": BearerAuth("JWT")}, []SecurityRequirement{Require("bearer")})
	renamedCur := compatSecurityDoc(map[string]SecurityScheme{"jwt": HTTPAuth("Bearer")}, []SecurityRequirement{Require("jwt")})
	assertAPIChanges(t, CompareDocuments(renamedOld, renamedCur),
		wantChange{Compatible, "security-scheme-removed", "/components/securitySchemes/bearer"},
		wantChange{Compatible, "security-scheme-added", "/components/securitySchemes/jwt"},
	)

	headerOld := compatSecurityDoc(map[string]SecurityScheme{"key": APIKeyHeader("X-Key")}, []SecurityRequirement{Require("key")})
	headerCase := compatSecurityDoc(map[string]SecurityScheme{"key": APIKeyHeader("x-key")}, []SecurityRequirement{Require("key")})
	assertAPIChanges(t, CompareDocuments(headerOld, headerCase))
	headerCur := compatSecurityDoc(map[string]SecurityScheme{"key": APIKeyHeader("X-Token")}, []SecurityRequirement{Require("key")})
	assertAPIChanges(t, CompareDocuments(headerOld, headerCur),
		wantChange{Breaking, "security-scheme-changed", "/components/securitySchemes/key"},
		wantChange{Breaking, "security-tightened", "/paths/~1items/get/security"},
		wantChange{Compatible, "security-loosened", "/paths/~1items/get/security"},
	)

	flows := func(token string) map[string]SecurityScheme {
		return map[string]SecurityScheme{"oauth": OAuth2(OAuthFlows{
			AuthorizationCode: &OAuthFlow{AuthorizationURL: "https://a.example/authorize", TokenURL: token, Scopes: map[string]string{}},
		})}
	}
	got := CompareDocuments(compatSecurityDoc(flows("https://a.example/token"), nil), compatSecurityDoc(flows("https://b.example/token"), nil))
	assertAPIChanges(t, got, wantChange{Breaking, "security-scheme-changed", "/components/securitySchemes/oauth"})
}

func TestNilDocuments(t *testing.T) {
	t.Parallel()
	if got := CompareDocuments(nil, nil); len(got) != 0 {
		t.Errorf("nil against nil = %v", got)
	}
	doc := compatDoc(map[string]*PathItem{"/a": {Get: compatOp("a")}}, nil)
	assertAPIChanges(t, CompareDocuments(nil, doc), wantChange{Compatible, "path-added", "/paths/~1a"})
	assertAPIChanges(t, CompareDocuments(doc, nil), wantChange{Breaking, "path-removed", "/paths/~1a"})
	assertAPIChanges(t, CompareDocuments(doc, doc))
}

// TestOrderIsDeterministic compares two documents many times, since a
// comparison that walked a map in its own order would show it here.
func TestOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	old, cur := newIntegrationDocument(t), richDocument(t, false)
	first := CompareDocuments(old, cur)
	if len(first) < 10 {
		t.Fatalf("expected the two documents to differ widely, got %d changes", len(first))
	}
	for range 20 {
		if again := CompareDocuments(old, cur); !slices.Equal(again, first) {
			t.Fatalf("comparison is not deterministic:\n%s\nthen:\n%s", renderAPIChanges(first), renderAPIChanges(again))
		}
	}
	for i := 1; i < len(first); i++ {
		x, y := first[i-1], first[i]
		if x.Severity < y.Severity || (x.Severity == y.Severity && x.Location > y.Location) {
			t.Fatalf("changes %d and %d are out of order: %v, %v", i-1, i, x, y)
		}
	}
}
