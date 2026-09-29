package muzak

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// SecurityScheme describes one way a client proves who it is, as the OpenAPI
// document presents it: the credential a bearer token, an API key or a login
// carries, and where a client puts it.
//
// A scheme only describes. It is never consulted while a request is served,
// nothing is enforced because it is declared, and a route that names one is no
// more protected than it was. What refuses a request is a [Guard]; a scheme is
// how the document says which guard a route sits behind, so that a
// documentation tool can offer a client the means to authenticate. The two are
// declared separately and are only as truthful as the person who wrote both.
//
// Declare schemes in [OpenAPIOptions.SecuritySchemes], and say which a route
// needs with [WithSecurity]. The constructors below cover the common cases; a
// scheme they do not reach can be written as a literal, and is checked when the
// application is built either way.
type SecurityScheme struct {
	// Type is "http", "apiKey", "oauth2" or "openIdConnect", which is what the
	// constructors set.
	Type string `json:"type"`
	// Description explains the scheme to a person, as CommonMark.
	Description string `json:"description,omitzero"`
	// Name is the header, query parameter or cookie an "apiKey" is sent in.
	Name string `json:"name,omitzero"`
	// In is where an "apiKey" is sent: "header", "query" or "cookie".
	In string `json:"in,omitzero"`
	// Scheme is the HTTP authentication scheme of an "http" scheme, such as
	// "bearer" or "basic".
	Scheme string `json:"scheme,omitzero"`
	// BearerFormat hints at how a bearer token is written, such as "JWT". It
	// is documentation and nothing reads it.
	BearerFormat string `json:"bearerFormat,omitzero"`
	// Flows lists the OAuth 2.0 flows an "oauth2" scheme supports.
	Flows *OAuthFlows `json:"flows,omitzero"`
	// OpenIDConnectURL is the discovery document of an "openIdConnect" scheme.
	OpenIDConnectURL string `json:"openIdConnectUrl,omitzero"`
}

// OAuthFlows lists the OAuth 2.0 flows a scheme supports. At least one is
// required.
type OAuthFlows struct {
	// Implicit needs an AuthorizationURL.
	Implicit *OAuthFlow `json:"implicit,omitzero"`
	// Password needs a TokenURL.
	Password *OAuthFlow `json:"password,omitzero"`
	// ClientCredentials needs a TokenURL.
	ClientCredentials *OAuthFlow `json:"clientCredentials,omitzero"`
	// AuthorizationCode needs both.
	AuthorizationCode *OAuthFlow `json:"authorizationCode,omitzero"`
}

// OAuthFlow is one OAuth 2.0 flow: where a client is sent, and the scopes it
// may ask for.
type OAuthFlow struct {
	// AuthorizationURL is where a person grants access, for the flows that
	// have one. It must be an http or https URL.
	AuthorizationURL string `json:"authorizationUrl,omitzero"`
	// TokenURL is where a token is obtained, for the flows that have one.
	TokenURL string `json:"tokenUrl,omitzero"`
	// RefreshURL is where a token is refreshed, when that is somewhere else.
	RefreshURL string `json:"refreshUrl,omitzero"`
	// Scopes maps each scope to a sentence saying what it allows. It is
	// required, and may be empty.
	Scopes map[string]string `json:"scopes"`
}

// BearerAuth describes a bearer token sent in the Authorization header. format
// is a hint such as "JWT", and may be empty.
func BearerAuth(format string) SecurityScheme {
	return SecurityScheme{Type: "http", Scheme: "bearer", BearerFormat: format}
}

// BasicAuth describes HTTP Basic authentication.
func BasicAuth() SecurityScheme {
	return SecurityScheme{Type: "http", Scheme: "basic"}
}

// HTTPAuth describes any other HTTP authentication scheme registered with IANA,
// such as "digest", named as it appears in the Authorization header.
func HTTPAuth(scheme string) SecurityScheme {
	return SecurityScheme{Type: "http", Scheme: scheme}
}

// APIKeyHeader describes an API key sent in the named request header.
func APIKeyHeader(name string) SecurityScheme {
	return SecurityScheme{Type: "apiKey", In: "header", Name: name}
}

// APIKeyQuery describes an API key sent in the named query parameter. A key in
// a URL is written to logs and browser history, so prefer a header.
func APIKeyQuery(name string) SecurityScheme {
	return SecurityScheme{Type: "apiKey", In: "query", Name: name}
}

// APIKeyCookie describes an API key sent in the named cookie, such as a session.
func APIKeyCookie(name string) SecurityScheme {
	return SecurityScheme{Type: "apiKey", In: "cookie", Name: name}
}

// OAuth2 describes OAuth 2.0 with the given flows.
func OAuth2(flows OAuthFlows) SecurityScheme {
	return SecurityScheme{Type: "oauth2", Flows: &flows}
}

// OpenIDConnect describes OpenID Connect, discovered at the given URL.
func OpenIDConnect(discoveryURL string) SecurityScheme {
	return SecurityScheme{Type: "openIdConnect", OpenIDConnectURL: discoveryURL}
}

// SecurityRequirement names the schemes a client has to satisfy together, each
// with the scopes it needs. A scheme that has no scopes is named with a nil or
// empty list.
type SecurityRequirement map[string][]string

// Require is a [SecurityRequirement] of one scheme and the scopes it needs:
//
//	muzak.WithSecurity(muzak.Require("oauth", "items:read"))
//
// One that has to be satisfied together with another is written as a literal:
//
//	muzak.SecurityRequirement{"key": nil, "session": nil}
func Require(scheme string, scopes ...string) SecurityRequirement {
	return SecurityRequirement{scheme: scopes}
}

// WithSecurity says which of the application's security schemes a route, or
// every route beneath a router, is behind. It is documentation: it is written
// into the OpenAPI document so that a documentation tool can offer a client
// the means to authenticate, and it has no effect at runtime. Nothing is
// enforced because a route names a scheme, so the route still needs the
// [Guard] that does the refusing.
//
// The requirements are alternatives: a client satisfies any one of them. The
// schemes named within one requirement are all needed. An empty requirement
// among them says that a request with no credentials is accepted too.
//
//	admin := muzak.NewRouter(muzak.WithSecurity(muzak.Require("bearer")))
//	admin.Get("/report", report)
//	admin.Get("/status", status, muzak.Public())
//
// A declaration on a route replaces what a router declared, instead of adding
// to it, since alternatives and combinations cannot be told apart once they are
// merged. A route that never meets one carries no security information, as it
// did before. Every scheme named has to be declared in
// [OpenAPIOptions.SecuritySchemes], which is checked when the application is
// built, along with the scopes of an OAuth 2.0 scheme.
func WithSecurity(requirements ...SecurityRequirement) SharedOption {
	// Each requirement is copied, so the declaration cannot be changed by the
	// map it was made from once the application is built from it.
	declared := make([]SecurityRequirement, len(requirements))
	for i, requirement := range requirements {
		declared[i] = cloneRequirement(requirement)
	}
	if len(requirements) == 0 {
		// Left nil, where Public leaves an empty list, so that the two can be
		// told apart when the application is built.
		declared = nil
	}
	return securityOption(declared)
}

// Public says that a route, or every route beneath a router, needs no
// credentials, which the OpenAPI document states explicitly and a
// documentation tool shows as open. Like [WithSecurity] it is documentation
// only, and is how a route beneath a router that declared a requirement says it
// is an exception.
func Public() SharedOption {
	return securityOption([]SecurityRequirement{})
}

// securityOption applies a security declaration at either level. A nil list is
// a declaration of nothing, which is refused when the application is built.
func securityOption(requirements []SecurityRequirement) SharedOption {
	return sharedOption{
		route: func(c *routeConfig) { c.security, c.securitySet = requirements, true },
		router: func(c *routerConfig) {
			c.security, c.securitySet = requirements, true
		},
	}
}

// cloneRequirement copies a requirement and its scope lists, with every list
// non-nil because a null would be written where the document needs an array.
func cloneRequirement(requirement SecurityRequirement) SecurityRequirement {
	out := make(SecurityRequirement, len(requirement))
	for scheme, scopes := range requirement {
		out[scheme] = append([]string{}, scopes...)
	}
	return out
}

// schemeNameOK matches what OpenAPI allows a component to be called.
func schemeNameOK(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// absoluteWebURL reports whether a string is an http or https URL with a host.
// A scheme is a link a documentation page may offer to follow, so it is held to
// the two that lead somewhere a person can go.
func absoluteWebURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// validateSecuritySchemes reports every scheme that could not be described in
// the document, worded for the developer who declared it.
func (o OpenAPIOptions) validateSecuritySchemes() error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(o.SecuritySchemes)) {
		if err := o.SecuritySchemes[name].validate(); err != nil {
			errs = append(errs, fmt.Errorf("muzak: AppOptions.SecuritySchemes[%q]: %w", name, err))
		}
		if !schemeNameOK(name) {
			errs = append(errs, fmt.Errorf(
				"muzak: AppOptions.SecuritySchemes[%q]: a scheme's name may hold only letters, digits, '.', '_' and '-'", name))
		}
	}
	return errors.Join(errs...)
}

// validate reports what is wrong with a scheme on its own terms, or nil.
func (s SecurityScheme) validate() error {
	switch s.Type {
	case "http":
		if !isHTTPToken(s.Scheme) {
			return errors.New(`an "http" scheme needs Scheme, the authentication scheme as it appears in the Authorization header, such as "bearer"`)
		}
		if s.BearerFormat != "" && !strings.EqualFold(s.Scheme, "bearer") {
			return errors.New("BearerFormat describes a bearer token, and this scheme is not one")
		}
	case "apiKey":
		switch s.In {
		case "header":
			if !isHTTPToken(s.Name) {
				return errors.New("an API key in a header needs Name to be a valid header name")
			}
		case "query", "cookie":
			if s.Name == "" {
				return fmt.Errorf("an API key in a %s needs Name", s.In)
			}
		default:
			return fmt.Errorf(`an "apiKey" scheme needs In to be "header", "query" or "cookie", not %q`, s.In)
		}
	case "oauth2":
		return s.Flows.validate()
	case "openIdConnect":
		if !absoluteWebURL(s.OpenIDConnectURL) {
			return errors.New(`an "openIdConnect" scheme needs OpenIDConnectURL, an http or https URL`)
		}
	default:
		return fmt.Errorf(`the type %q is not "http", "apiKey", "oauth2" or "openIdConnect"`, s.Type)
	}
	return nil
}

// validate reports a set of flows that has none, or one that lacks the address
// its kind of flow goes to.
func (f *OAuthFlows) validate() error {
	if f == nil || (f.Implicit == nil && f.Password == nil && f.ClientCredentials == nil && f.AuthorizationCode == nil) {
		return errors.New(`an "oauth2" scheme needs at least one flow`)
	}
	for _, flow := range []struct {
		name          string
		flow          *OAuthFlow
		authorization bool
		token         bool
	}{
		{"Implicit", f.Implicit, true, false},
		{"Password", f.Password, false, true},
		{"ClientCredentials", f.ClientCredentials, false, true},
		{"AuthorizationCode", f.AuthorizationCode, true, true},
	} {
		if flow.flow == nil {
			continue
		}
		if flow.authorization && !absoluteWebURL(flow.flow.AuthorizationURL) {
			return fmt.Errorf("the %s flow needs AuthorizationURL, an http or https URL", flow.name)
		}
		if flow.token && !absoluteWebURL(flow.flow.TokenURL) {
			return fmt.Errorf("the %s flow needs TokenURL, an http or https URL", flow.name)
		}
		if flow.flow.RefreshURL != "" && !absoluteWebURL(flow.flow.RefreshURL) {
			return fmt.Errorf("the %s flow's RefreshURL is not an http or https URL", flow.name)
		}
	}
	return nil
}

// scopes lists every scope an OAuth 2.0 scheme's flows declare.
func (f *OAuthFlows) scopes() map[string]bool {
	out := map[string]bool{}
	for _, flow := range []*OAuthFlow{f.Implicit, f.Password, f.ClientCredentials, f.AuthorizationCode} {
		if flow == nil {
			continue
		}
		for scope := range flow.Scopes {
			out[scope] = true
		}
	}
	return out
}

// validateSecurity reports every route that names a scheme the application does
// not declare, or a scope an OAuth 2.0 scheme does not offer, or that declares
// WithSecurity and then nothing.
func (a *App) validateSecurity() error {
	var errs []error
	for _, rt := range a.routes {
		if !rt.securitySet {
			continue
		}
		where := rt.Method + " " + rt.Path
		if rt.security == nil {
			errs = append(errs, fmt.Errorf(
				"muzak: %s: WithSecurity names no requirement; declare one, or use Public for a route that needs none", where))
			continue
		}
		for _, requirement := range rt.security {
			for _, scheme := range slices.Sorted(maps.Keys(requirement)) {
				declared, ok := a.opts.SecuritySchemes[scheme]
				if !ok {
					errs = append(errs, fmt.Errorf(
						"muzak: %s: security scheme %q is not declared; add it to AppOptions.SecuritySchemes", where, scheme))
					continue
				}
				if declared.Type != "oauth2" || declared.Flows == nil {
					continue
				}
				offered := declared.Flows.scopes()
				for _, scope := range requirement[scheme] {
					if !offered[scope] {
						errs = append(errs, fmt.Errorf(
							"muzak: %s: scope %q is not offered by the %q scheme", where, scope, scheme))
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}

// securitySchemesForDocs copies the declared schemes into the document, so that
// the document and the options share nothing a later change to one could reach.
func (o OpenAPIOptions) securitySchemesForDocs() map[string]SecurityScheme {
	if len(o.SecuritySchemes) == 0 {
		return nil
	}
	out := make(map[string]SecurityScheme, len(o.SecuritySchemes))
	for name, scheme := range o.SecuritySchemes {
		if scheme.Flows != nil {
			flows := *scheme.Flows
			for _, flow := range []**OAuthFlow{&flows.Implicit, &flows.Password, &flows.ClientCredentials, &flows.AuthorizationCode} {
				if *flow == nil {
					continue
				}
				copied := **flow
				// Scopes is required, so a flow with none says so with an
				// empty object rather than null.
				copied.Scopes = maps.Clone(copied.Scopes)
				if copied.Scopes == nil {
					copied.Scopes = map[string]string{}
				}
				*flow = &copied
			}
			scheme.Flows = &flows
		}
		out[name] = scheme
	}
	return out
}

// securityForDocs copies a route's requirements into what its operation says,
// or returns nil for a route that declared none, which leaves the field out.
func (rt *Route) securityForDocs() []SecurityRequirement {
	if !rt.securitySet {
		return nil
	}
	out := make([]SecurityRequirement, len(rt.security))
	for i, requirement := range rt.security {
		out[i] = cloneRequirement(requirement)
	}
	return out
}
