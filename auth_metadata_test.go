package muzak

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

// TestResourceMetadataIsPublishedAndNamed checks the RFC 9728 document is
// served where the RFC puts it, public whatever the application requires, and
// named by every challenge.
func TestResourceMetadataIsPublishedAndNamed(t *testing.T) {
	opts := jwtOptions()
	opts.ResourceMetadata = &ResourceMetadata{
		Resource:              "https://api.example.com/v1",
		ScopesSupported:       []string{"items:read", "items:write"},
		ResourceName:          "Items API",
		ResourceDocumentation: "https://docs.example.com/items",
	}
	appOpts := quietOptions()
	appOpts.SecuritySchemes = map[string]SecurityScheme{"jwt": testScheme(opts)}
	app := New(appOpts, WithSecurity(Require("jwt")))
	app.Get("/me", me, WithSecurity(Require("jwt", "items:read")))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, ProtectedResourcePath+"/v1")
	assertStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	assertJSON(t, rec, `{
		"resource": "https://api.example.com/v1",
		"authorization_servers": ["`+testIssuer+`"],
		"scopes_supported": ["items:read", "items:write"],
		"bearer_methods_supported": ["header"],
		"resource_name": "Items API",
		"resource_documentation": "https://docs.example.com/items"
	}`)

	const metadata = `resource_metadata="https://api.example.com/.well-known/oauth-protected-resource/v1"`
	rec = do(t, app, http.MethodGet, "/me")
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api", `+metadata {
		t.Fatalf("no token: WWW-Authenticate = %q", got)
	}
	rec = withBearer(t, app, http.MethodGet, "/me", "bad")
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api", error="invalid_token", `+metadata {
		t.Fatalf("bad token: WWW-Authenticate = %q", got)
	}
	rec = withBearer(t, app, http.MethodGet, "/me", signJWT(t, "RS256", testRSAKey(), "", nil))
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="api", error="insufficient_scope", scope="items:read", `+metadata {
		t.Fatalf("no scope: WWW-Authenticate = %q", got)
	}

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(doc)
	if strings.Contains(string(encoded), "oauth-protected-resource") {
		t.Fatal("the metadata route is in the OpenAPI document")
	}
}

// TestResourceMetadataAtTheRoot checks a resource with no path is described
// at the well-known path itself, and that the authorization servers may be
// named apart from the issuers.
func TestResourceMetadataAtTheRoot(t *testing.T) {
	for _, resource := range []string{"https://api.example.com", "https://api.example.com/"} {
		opts := jwtOptions()
		opts.ResourceMetadata = &ResourceMetadata{Resource: resource, AuthorizationServers: []string{"https://login.example.com"}}
		app := mustBuild(t, jwtApp(t, opts))
		rec := do(t, app, http.MethodGet, ProtectedResourcePath)
		assertStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), `"authorization_servers":["https://login.example.com"]`) || strings.Contains(rec.Body.String(), "scopes_supported") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	}
}

// TestResourceMetadataIsOffByDefault checks nothing is served unless asked.
func TestResourceMetadataIsOffByDefault(t *testing.T) {
	app := mustBuild(t, jwtApp(t, jwtOptions()))
	assertStatus(t, do(t, app, http.MethodGet, ProtectedResourcePath), http.StatusNotFound)
	if got := do(t, app, http.MethodGet, "/me").Header().Get("WWW-Authenticate"); strings.Contains(got, "resource_metadata") {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}

// TestResourceMetadataOptionsAreChecked covers every value that cannot be
// published.
func TestResourceMetadataOptionsAreChecked(t *testing.T) {
	cases := map[string]struct {
		meta ResourceMetadata
		want string
	}{
		"no resource":      {ResourceMetadata{}, "Resource must be an https URL"},
		"plain http":       {ResourceMetadata{Resource: "http://api.example.com"}, "Resource must be an https URL"},
		"query":            {ResourceMetadata{Resource: "https://api.example.com/?a=1"}, "Resource must be an https URL"},
		"fragment":         {ResourceMetadata{Resource: "https://api.example.com/#a"}, "Resource must be an https URL"},
		"credentials":      {ResourceMetadata{Resource: "https://u:p@api.example.com/"}, "Resource must be an https URL"},
		"trailing slash":   {ResourceMetadata{Resource: "https://api.example.com/v1/"}, "cannot be served as a route"},
		"empty segment":    {ResourceMetadata{Resource: "https://api.example.com//v1"}, "cannot be served as a route"},
		"server not https": {ResourceMetadata{Resource: "https://api.example.com", AuthorizationServers: []string{"http://login.example.com"}}, "AuthorizationServers[0] must be an https URL"},
		"server query":     {ResourceMetadata{Resource: "https://api.example.com", AuthorizationServers: []string{"https://login.example.com/?x"}}, "AuthorizationServers[0]"},
		"bad scope":        {ResourceMetadata{Resource: "https://api.example.com", ScopesSupported: []string{"a b"}}, `ScopesSupported names "a b"`},
		"bad docs":         {ResourceMetadata{Resource: "https://api.example.com", ResourceDocumentation: "javascript:alert(1)"}, "ResourceDocumentation must be an http or https URL"},
		"bad name":         {ResourceMetadata{Resource: "https://api.example.com", ResourceName: "\xff"}, "ResourceName is not valid UTF-8"},
		"quote in host":    {ResourceMetadata{Resource: `https://api"example.com`}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := jwtOptions()
			opts.ResourceMetadata = &tc.meta
			msg := buildError(t, jwtApp(t, opts))
			if !strings.Contains(msg, "JWTOptions.ResourceMetadata") || !strings.Contains(msg, tc.want) {
				t.Fatalf("build error = %s\nwant %q", msg, tc.want)
			}
		})
	}
	// The issuers stand in for the servers, so a non-URL issuer needs them
	// named.
	opts := jwtOptions()
	opts.Issuers = []string{"login-service"}
	opts.ResourceMetadata = &ResourceMetadata{Resource: "https://api.example.com"}
	if msg := buildError(t, jwtApp(t, opts)); !strings.Contains(msg, "AuthorizationServers[0]") {
		t.Fatalf("build error = %s", msg)
	}
	// A route of the application's own at the path collides.
	opts = jwtOptions()
	opts.ResourceMetadata = &ResourceMetadata{Resource: "https://api.example.com"}
	app := jwtApp(t, opts)
	app.Get(ProtectedResourcePath, me)
	if msg := buildError(t, app); !strings.Contains(msg, "GET "+ProtectedResourcePath+" is registered twice") {
		t.Fatalf("build error = %s", msg)
	}
}
