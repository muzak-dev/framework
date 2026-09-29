package muzak

import (
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// noop is a handler for routes whose behaviour is not what a test looks at.
func noop(ctx *Context, _ Empty) (rtOut, error) { return rtOut{OK: true}, nil }

// securedOptions declares one scheme of each kind an application is likely to use.
func securedOptions() AppOptions {
	opts := quietOptions()
	opts.SecuritySchemes = map[string]SecurityScheme{
		"bearer":  BearerAuth("JWT"),
		"basic":   BasicAuth(),
		"key":     APIKeyHeader("X-API-Key"),
		"session": APIKeyCookie("sid"),
		"oauth": OAuth2(OAuthFlows{AuthorizationCode: &OAuthFlow{
			AuthorizationURL: "https://auth.example.com/authorize",
			TokenURL:         "https://auth.example.com/token",
			Scopes:           map[string]string{"items:read": "Read items"},
		}}),
	}
	return opts
}

func marshalDoc(t *testing.T, app *App) string {
	t.Helper()
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	out, err := doc.Marshal()
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	return string(out)
}

func TestAnApplicationThatDeclaresNoSecurityEmitsNone(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", noop)
	out := marshalDoc(t, app)
	for _, word := range []string{"security", "securitySchemes"} {
		if strings.Contains(out, word) {
			t.Errorf("the document mentions %q although nothing declared it:\n%s", word, out)
		}
	}
}

func TestSecuritySchemesAreEmittedAsComponents(t *testing.T) {
	t.Parallel()
	app := New(securedOptions())
	app.Get("/x", noop)
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	schemes := doc.Components.SecuritySchemes
	if len(schemes) != 5 {
		t.Fatalf("schemes = %v", schemes)
	}
	if s := schemes["bearer"]; s.Type != "http" || s.Scheme != "bearer" || s.BearerFormat != "JWT" {
		t.Errorf("bearer = %+v", s)
	}
	if s := schemes["key"]; s.Type != "apiKey" || s.In != "header" || s.Name != "X-API-Key" {
		t.Errorf("key = %+v", s)
	}
	if s := schemes["session"]; s.In != "cookie" || s.Name != "sid" {
		t.Errorf("session = %+v", s)
	}
	// A declared scheme is documented whether or not a route names it, which
	// is what lets a documentation page offer to authenticate.
	if doc.Paths["/x"].Get.Security != nil {
		t.Errorf("a route that declared nothing carries %v", doc.Paths["/x"].Get.Security)
	}
	out := marshalDoc(t, app)
	if !strings.Contains(out, `"securitySchemes"`) || !strings.Contains(out, `"authorizationUrl"`) {
		t.Errorf("the document does not carry the declared schemes:\n%s", out)
	}
	if strings.Contains(out, `"security":`) {
		t.Errorf("an operation carries a security list nobody declared:\n%s", out)
	}
}

func TestSecurityRequirementsAreInheritedAndOverridden(t *testing.T) {
	t.Parallel()
	app := New(securedOptions(), WithSecurity(Require("bearer")))
	admin := NewRouter(WithSecurity(Require("key"), Require("basic")))
	admin.Get("/report", noop)
	admin.Get("/status", noop, Public())
	admin.Get("/either", noop, WithSecurity(Require("bearer"), SecurityRequirement{}))
	app.Include(admin)
	app.Get("/things", noop)
	app.Get("/scoped", noop, WithSecurity(Require("oauth", "items:read")))
	app.Get("/both", noop, WithSecurity(SecurityRequirement{"key": nil, "session": nil}))

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	names := func(op *Operation) [][]string {
		var out [][]string
		for _, requirement := range op.Security {
			var schemes []string
			for name := range requirement {
				schemes = append(schemes, name)
			}
			slices.Sort(schemes)
			out = append(out, schemes)
		}
		return out
	}
	tests := []struct {
		path string
		want [][]string
	}{
		{"/things", [][]string{{"bearer"}}},
		{"/report", [][]string{{"key"}, {"basic"}}},
		{"/status", [][]string{}},
		{"/either", [][]string{{"bearer"}, {}}},
		{"/scoped", [][]string{{"oauth"}}},
		{"/both", [][]string{{"key", "session"}}},
	}
	for _, tc := range tests {
		op := doc.Paths[tc.path].Get
		if op == nil {
			t.Fatalf("%s is not documented", tc.path)
		}
		if got := names(op); !slices.EqualFunc(got, tc.want, slices.Equal) {
			t.Errorf("%s security = %v, want %v", tc.path, got, tc.want)
		}
	}
	if got := doc.Paths["/scoped"].Get.Security[0]["oauth"]; !slices.Equal(got, []string{"items:read"}) {
		t.Errorf("scopes = %v", got)
	}

	// An open route says so explicitly, and a scheme with no scopes carries
	// an empty list: null is not what an OpenAPI tool accepts.
	out := marshalDoc(t, app)
	var generic map[string]any
	if err := json.Unmarshal([]byte(out), &generic); err != nil {
		t.Fatal(err)
	}
	paths := generic["paths"].(map[string]any)
	status := paths["/status"].(map[string]any)["get"].(map[string]any)
	if security, present := status["security"].([]any); !present || len(security) != 0 {
		t.Errorf("a public route's security = %v, want an empty list", status["security"])
	}
	things := paths["/things"].(map[string]any)["get"].(map[string]any)
	bearer := things["security"].([]any)[0].(map[string]any)["bearer"]
	if list, ok := bearer.([]any); !ok || len(list) != 0 {
		t.Errorf("bearer scopes = %#v, want an empty list", bearer)
	}
}

// A scheme is a description. Naming one must not make a route refuse anything,
// and must not make it accept anything either.
func TestASecuritySchemeEnforcesNothing(t *testing.T) {
	t.Parallel()
	app := New(securedOptions())
	app.Get("/open", noop, WithSecurity(Require("bearer")))
	guarded := NewRouter(WithDependencies(func(ctx *Context) error {
		return NewHTTPError(http.StatusUnauthorized, "no")
	}), WithSecurity(Require("bearer")))
	guarded.Get("/closed", noop)
	app.Include(guarded)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/open"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/closed"), http.StatusUnauthorized)
}

func TestSecurityIsCheckedWhenTheApplicationIsBuilt(t *testing.T) {
	t.Parallel()
	good := "https://auth.example.com/x"
	tests := []struct {
		name    string
		schemes map[string]SecurityScheme
		route   []RouteOption
		want    string
	}{
		{"undeclared scheme", nil, []RouteOption{WithSecurity(Require("nope"))}, `security scheme "nope" is not declared`},
		{"undeclared among declared", map[string]SecurityScheme{"a": BasicAuth()}, []RouteOption{WithSecurity(Require("b"))}, `"b" is not declared`},
		{"no requirement", map[string]SecurityScheme{"a": BasicAuth()}, []RouteOption{WithSecurity()}, "names no requirement"},
		{"bad name", map[string]SecurityScheme{"a b": BasicAuth()}, nil, "may hold only letters"},
		{"empty name", map[string]SecurityScheme{"": BasicAuth()}, nil, "may hold only letters"},
		{"no type", map[string]SecurityScheme{"a": {}}, nil, "is not"},
		{"http without scheme", map[string]SecurityScheme{"a": {Type: "http"}}, nil, "needs Scheme"},
		{"http scheme with space", map[string]SecurityScheme{"a": HTTPAuth("be arer")}, nil, "needs Scheme"},
		{"format on basic", map[string]SecurityScheme{"a": {Type: "http", Scheme: "basic", BearerFormat: "JWT"}}, nil, "BearerFormat"},
		{"key without place", map[string]SecurityScheme{"a": {Type: "apiKey", Name: "k"}}, nil, `needs In`},
		{"key header name", map[string]SecurityScheme{"a": APIKeyHeader("bad name")}, nil, "valid header name"},
		{"key query name", map[string]SecurityScheme{"a": APIKeyQuery("")}, nil, "needs Name"},
		{"oauth without flows", map[string]SecurityScheme{"a": {Type: "oauth2"}}, nil, "at least one flow"},
		{"oauth empty flows", map[string]SecurityScheme{"a": OAuth2(OAuthFlows{})}, nil, "at least one flow"},
		{"implicit without url", map[string]SecurityScheme{"a": OAuth2(OAuthFlows{Implicit: &OAuthFlow{}})}, nil, "AuthorizationURL"},
		{"password without token url", map[string]SecurityScheme{"a": OAuth2(OAuthFlows{Password: &OAuthFlow{}})}, nil, "TokenURL"},
		{"code needs both", map[string]SecurityScheme{"a": OAuth2(OAuthFlows{AuthorizationCode: &OAuthFlow{AuthorizationURL: good}})}, nil, "TokenURL"},
		{"script url", map[string]SecurityScheme{"a": OAuth2(OAuthFlows{Implicit: &OAuthFlow{AuthorizationURL: "javascript:alert(1)"}})}, nil, "AuthorizationURL"},
		{"refresh url", map[string]SecurityScheme{"a": OAuth2(OAuthFlows{ClientCredentials: &OAuthFlow{TokenURL: good, RefreshURL: "ftp://x/y"}})}, nil, "RefreshURL"},
		{"openid url", map[string]SecurityScheme{"a": OpenIDConnect("javascript:1")}, nil, "OpenIDConnectURL"},
		{"unknown scope", map[string]SecurityScheme{"a": OAuth2(OAuthFlows{ClientCredentials: &OAuthFlow{TokenURL: good, Scopes: map[string]string{"r": ""}}})},
			[]RouteOption{WithSecurity(Require("a", "w"))}, `scope "w" is not offered`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.SecuritySchemes = tc.schemes
			app := New(opts)
			app.Get("/x", noop, tc.route...)
			err := app.Build()
			if err == nil {
				t.Fatalf("Build succeeded, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidSecuritySchemesBuild(t *testing.T) {
	t.Parallel()
	opts := securedOptions()
	opts.SecuritySchemes["oidc"] = OpenIDConnect("https://auth.example.com/.well-known/openid-configuration")
	opts.SecuritySchemes["digest"] = HTTPAuth("digest")
	opts.SecuritySchemes["q"] = APIKeyQuery("api_key")
	opts.SecuritySchemes["legacy"] = OAuth2(OAuthFlows{Password: &OAuthFlow{TokenURL: "http://auth.example.com/token"}})
	app := New(opts)
	app.Get("/x", noop, WithSecurity(Require("oidc"), Require("legacy")))
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	// A flow with no scopes still writes the object OpenAPI requires.
	if scopes := doc.Components.SecuritySchemes["legacy"].Flows.Password.Scopes; scopes == nil {
		t.Error("a flow's scopes are null rather than empty")
	}
	// The declaration is left as it was given.
	if opts.SecuritySchemes["legacy"].Flows.Password.Scopes != nil {
		t.Error("building the document changed the declaration")
	}
}

func TestSecurityDeclarationsAreCopied(t *testing.T) {
	t.Parallel()
	requirement := Require("bearer", "a")
	option := WithSecurity(requirement)
	requirement["bearer"][0] = "changed"
	requirement["other"] = nil
	app := New(securedOptions())
	app.Get("/x", noop, option)
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	got := doc.Paths["/x"].Get.Security
	if len(got) != 1 || len(got[0]) != 1 || got[0]["bearer"][0] != "a" {
		t.Errorf("security = %v: the declaration followed a change made after it", got)
	}
}
