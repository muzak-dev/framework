package muzak

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// ProtectedResourcePath is where RFC 9728 places the metadata of a protected
// resource, between the host and the path of the resource's identifier.
const ProtectedResourcePath = "/.well-known/oauth-protected-resource"

// ResourceMetadata is the RFC 9728 protected resource metadata a [JWTBearer]
// scheme publishes when [JWTOptions.ResourceMetadata] is set: a JSON document
// telling an OAuth client which authorization servers issue tokens for this
// API and which scopes it understands, which is how a client that has only
// the API's URL, an MCP client among them, finds where to get a token.
//
// The document is served at [ProtectedResourcePath] followed by the path of
// Resource, so "https://api.example.com/v1" is described at
// "/.well-known/oauth-protected-resource/v1", by a GET route that is public,
// whatever [WithSecurity] the application declares, and left out of the
// OpenAPI document. The application's own guards still run for it, as they do
// for every route, so an application-wide guard keeps clients from reading
// it; declare such a guard on an included router instead. Every 401 and 403
// the scheme answers names the document in a resource_metadata parameter of
// its challenge.
//
// The path is the one in Resource, served by this application as it stands.
// An application behind a proxy that strips or adds a prefix must make the
// public URL and the served path agree.
type ResourceMetadata struct {
	// Resource is the API's resource identifier, the URL clients know it by
	// and the audience its tokens are issued for: an https URL with no query
	// and no fragment. It is required.
	Resource string

	// AuthorizationServers lists the issuer identifiers of the authorization
	// servers whose tokens the API accepts, each an https URL. It defaults to
	// [JWTOptions.Issuers].
	AuthorizationServers []string

	// ScopesSupported lists the scopes a client may ask for. Empty leaves the
	// member out.
	ScopesSupported []string

	// ResourceName is a name for the API a person reads.
	ResourceName string

	// ResourceDocumentation is a URL of documentation a developer reads.
	ResourceDocumentation string
}

// resourceMetadataSpec is the metadata compiled: where it is served, the URL
// a challenge names, and the document itself.
type resourceMetadataSpec struct {
	path string
	url  string
	body []byte
}

// protectedResourceDocument is the JSON RFC 9728 defines, with the members
// Muzak knows how to fill.
type protectedResourceDocument struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitzero"`
	ScopesSupported        []string `json:"scopes_supported,omitzero"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name,omitzero"`
	ResourceDocumentation  string   `json:"resource_documentation,omitzero"`
}

// compile checks the metadata and encodes it, returning every problem found.
func (m *ResourceMetadata) compile(issuers []string) (*resourceMetadataSpec, []error) {
	var errs []error
	resource, err := url.Parse(m.Resource)
	if err != nil || !secureOrLoopback(resource) || resource.RawQuery != "" || resource.Fragment != "" || resource.User != nil {
		return nil, []error{errors.New("JWTOptions.ResourceMetadata.Resource must be an https URL with a host and no query, fragment or credentials")}
	}
	resourcePath := resource.EscapedPath()
	if resourcePath == "/" {
		resourcePath = ""
	}
	if strings.Contains(resourcePath, "//") || strings.HasSuffix(resourcePath, "/") {
		errs = append(errs, errors.New("JWTOptions.ResourceMetadata.Resource has a path that cannot be served as a route; use one without empty segments or a trailing slash"))
	}
	servers := m.AuthorizationServers
	if servers == nil {
		servers = issuers
	}
	for i, server := range servers {
		u, err := url.Parse(server)
		if err != nil || !secureOrLoopback(u) || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, fmt.Errorf("JWTOptions.ResourceMetadata.AuthorizationServers[%d] must be an https URL with no query or fragment, "+
				"an issuer identifier as RFC 8414 defines one", i))
		}
	}
	for _, scope := range m.ScopesSupported {
		if !isScopeToken(scope) {
			errs = append(errs, fmt.Errorf("JWTOptions.ResourceMetadata.ScopesSupported names %q, which is not a valid scope", scope))
		}
	}
	if m.ResourceDocumentation != "" && !absoluteWebURL(m.ResourceDocumentation) {
		errs = append(errs, errors.New("JWTOptions.ResourceMetadata.ResourceDocumentation must be an http or https URL"))
	}
	if !utf8.ValidString(m.ResourceName) {
		errs = append(errs, errors.New("JWTOptions.ResourceMetadata.ResourceName is not valid UTF-8"))
	}
	spec := &resourceMetadataSpec{
		path: ProtectedResourcePath + resourcePath,
		url:  resource.Scheme + "://" + resource.Host + ProtectedResourcePath + resourcePath,
	}
	if !isQuotableHeaderText(spec.url) {
		errs = append(errs, errors.New("JWTOptions.ResourceMetadata.Resource cannot be quoted in a challenge; write it percent-encoded"))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	spec.body, err = json.Marshal(protectedResourceDocument{
		Resource:               m.Resource,
		AuthorizationServers:   slices.Clone(servers),
		ScopesSupported:        slices.Clone(m.ScopesSupported),
		BearerMethodsSupported: []string{"header"},
		ResourceName:           m.ResourceName,
		ResourceDocumentation:  m.ResourceDocumentation,
	})
	if err != nil {
		// coverage: every member is a string checked to be valid UTF-8 or a
		// URL that parsed, and json/v2 encodes any such value.
		return nil, []error{fmt.Errorf("JWTOptions.ResourceMetadata cannot be encoded: %w", err)}
	}
	return spec, nil
}

// secureOrLoopback reports whether u is an absolute https URL, or plain http
// to a loopback address, which is what a test serves.
func secureOrLoopback(u *url.URL) bool {
	if u.Host == "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Hostname()))
}

// registerAuthRoutes registers the metadata route of every [JWTBearer] scheme
// that publishes [ResourceMetadata]. It runs as the application is built, ahead
// of the routing tree being resolved, so that the route is resolved, counted
// and checked for collisions like any other: one at a path the application
// already answers is reported as registered twice.
func (a *App) registerAuthRoutes() {
	for _, name := range slices.Sorted(maps.Keys(a.opts.SecuritySchemes)) {
		v := a.opts.SecuritySchemes[name].verifier
		if v == nil || v.jwt == nil || v.jwt.metadata == nil || len(v.errs) > 0 {
			continue
		}
		body := v.jwt.metadata.body
		a.Get(v.jwt.metadata.path, func(*Context, struct{}) (Bytes, error) {
			return Bytes{ContentType: "application/json", Data: body}, nil
		}, Public(), Hidden(), OperationID("oauth_protected_resource_"+sanitizeIdent(name)))
	}
}
