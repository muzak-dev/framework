package muzak

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// This file is where a verifying security scheme stops describing and starts
// refusing. When the application is built, every route, mount and file mount
// whose WithSecurity names a scheme built by [JWTBearer] or [APIKeyVerifier]
// is given a gate: a guard placed ahead of every other, and on a route ahead
// of the body capture too, which admits a request only when one of the
// route's requirements is met, and hands the verified principal to the rest of
// the route. A route that names only descriptive schemes gets no gate and pays
// nothing.

// verifierSpec is what a verifying constructor attaches to the
// [SecurityScheme] it returns, and is nil on every other scheme. It is
// compiled and checked when the constructor is called, and errs holds every
// problem with the options, reported when the application is built.
type verifierSpec struct {
	jwt    *jwtSpec
	apiKey *apiKeySpec
	errs   []error
}

// verdict is what checking one scheme's credential on a request concluded.
type verdict uint8

const (
	// verdictUnknown means the credential has not been checked yet; a
	// scheme is only checked when a requirement needs it.
	verdictUnknown verdict = iota
	verdictAbsent
	verdictValid
	verdictInvalid
	// verdictUnavailable means the credential could not be judged, because
	// no key set has been fetched or a key lookup failed.
	verdictUnavailable
)

// schemeOutcome is one scheme's verdict on a request, with the principal and
// scopes of a valid credential and the reason a credential could not be
// judged.
type schemeOutcome struct {
	verdict   verdict
	principal any
	scopes    []string
	err       error
}

// credentialScheme is a verifying scheme as one application runs it.
type credentialScheme interface {
	// present reports whether the request carries a credential this scheme
	// reads, well-formed or not.
	present(c *Context) bool
	// verify judges the credential of a request on which present is true.
	verify(c *Context) schemeOutcome
	// principalType is the type of the principal a valid credential yields,
	// which a [Dep] or [From] of the route reads.
	principalType() reflect.Type
	// challenge is the WWW-Authenticate challenge a refusal names the scheme
	// with, or nil for a scheme that has none.
	challenge() *bearerChallenge
	// varyOn is the request header the response depends on when this scheme
	// decides it, or "" when the credential is not in a header.
	varyOn() string
}

// bearerChallenge holds the WWW-Authenticate values of one bearer scheme,
// built once so that a refusal formats nothing.
type bearerChallenge struct {
	// absent is the challenge for a request that presented no token, which
	// RFC 6750 says carries no error code; invalid is for one whose token was
	// refused.
	absent  string
	invalid string
	// base and suffix surround the error and scope parameters of an
	// insufficient_scope challenge; see [bearerChallenge.forbidden].
	base   string
	suffix string
}

// newBearerChallenge builds the challenge of a scheme with the given realm and,
// when it publishes one, the URL of its RFC 9728 metadata.
func newBearerChallenge(realm, metadataURL string) *bearerChallenge {
	base := `Bearer realm="` + realm + `"`
	suffix := ""
	if metadataURL != "" {
		suffix = `, resource_metadata="` + metadataURL + `"`
	}
	return &bearerChallenge{
		absent:  base + suffix,
		invalid: base + `, error="invalid_token"` + suffix,
		base:    base,
		suffix:  suffix,
	}
}

// forbidden is the insufficient_scope challenge naming the scopes required.
func (b *bearerChallenge) forbidden(scopes []string) string {
	out := b.base + `, error="insufficient_scope"`
	if len(scopes) > 0 {
		out += `, scope="` + strings.Join(scopes, " ") + `"`
	}
	return out + b.suffix
}

// maxGateSchemes bounds the distinct verifying schemes one route may name,
// which is what lets a request's verdicts live in a fixed array on the stack.
const maxGateSchemes = 16

// authGate is the compiled security of one route, mount or file mount: its
// requirements, each a set of schemes and the scopes each must grant, any one
// of which admits a request.
type authGate struct {
	schemes      []credentialScheme
	requirements []gateRequirement
	// anonymous is set when an empty requirement is among the alternatives,
	// which admits a request that presents no credential at all.
	anonymous bool
	vary      []string
}

// gateRequirement is one alternative: every member must be valid and grant
// its scopes.
type gateRequirement struct {
	members []gateMember
	// forbidden is the WWW-Authenticate value of a 403 refusing a request
	// that met this requirement but for its scopes, one per bearer scheme of
	// the requirement that names scopes; it is built once, by
	// [authGate.finish].
	forbidden []string
}

// gateMember is one scheme of a requirement and the scopes it must grant.
type gateMember struct {
	scheme int
	scopes []string
}

// guard returns the gate as the guard that runs first on the route.
func (g *authGate) guard() Guard { return g.admit }

// admit decides a request. The requirements are tried in the order they were
// declared, each scheme checked at most once and only when a requirement
// needs it, and the first requirement met admits the request and publishes
// its principals. Otherwise the refusal is the most specific one that is
// true: 403 when a requirement was met but for its scopes, 503 when a
// credential could not be judged, and 401 when no credential, or no valid one,
// was presented.
//
// A request that presents a credential is judged by it, even on a route that
// also admits anonymous requests: a token that is expired or forged is
// refused, rather than ignored, so that a client is told its credential is no
// longer good instead of being quietly served as nobody.
//
// It is linear in the number of requirements and their members, both fixed
// when the application is built, plus the cost of checking each credential
// once.
func (g *authGate) admit(c *Context) error {
	if len(g.vary) > 0 {
		c.w.varyOn(g.vary...)
	}
	var outcomes [maxGateSchemes]schemeOutcome
	forbidden := -1
	for ri := range g.requirements {
		req := &g.requirements[ri]
		met, scopesOnly := true, true
		for _, m := range req.members {
			o := g.evaluate(c, &outcomes, m.scheme)
			if o.verdict != verdictValid {
				met, scopesOnly = false, false
				break
			}
			if !grantsAll(o.scopes, m.scopes) {
				met = false
			}
		}
		if met {
			for _, m := range req.members {
				c.deps = append(c.deps, depValue{typ: g.schemes[m.scheme].principalType(), val: outcomes[m.scheme].principal})
			}
			return nil
		}
		if scopesOnly && forbidden < 0 {
			forbidden = ri
		}
	}
	if g.anonymous && !g.anyPresent(c, &outcomes) {
		return nil
	}
	header := c.w.Header()
	if forbidden >= 0 {
		for _, challenge := range g.requirements[forbidden].forbidden {
			header.Add("WWW-Authenticate", challenge)
		}
		return Forbidden("")
	}
	for i := range g.schemes {
		if outcomes[i].verdict == verdictUnavailable {
			return ServiceUnavailable("").Wrap(outcomes[i].err)
		}
	}
	g.challenge(c, &outcomes)
	return Unauthorized("")
}

// evaluate returns scheme i's verdict on the request, checking it the first
// time it is asked for.
func (g *authGate) evaluate(c *Context, outcomes *[maxGateSchemes]schemeOutcome, i int) *schemeOutcome {
	o := &outcomes[i]
	if o.verdict == verdictUnknown {
		if g.schemes[i].present(c) {
			*o = g.schemes[i].verify(c)
		} else {
			o.verdict = verdictAbsent
		}
	}
	return o
}

// anyPresent reports whether the request carries a credential for any of the
// gate's schemes.
func (g *authGate) anyPresent(c *Context, outcomes *[maxGateSchemes]schemeOutcome) bool {
	for i, s := range g.schemes {
		switch outcomes[i].verdict {
		case verdictAbsent:
			continue
		case verdictUnknown:
			if !s.present(c) {
				continue
			}
		}
		return true
	}
	return false
}

// challenge adds the WWW-Authenticate challenge of every bearer scheme of the
// gate to a 401. A scheme whose token was refused says invalid_token; one
// that was offered no token says nothing more, as RFC 6750 asks. A token not
// yet checked, because the requirement that named it failed on another
// member first, is checked here so that the challenge is true.
func (g *authGate) challenge(c *Context, outcomes *[maxGateSchemes]schemeOutcome) {
	header := c.w.Header()
	for i, s := range g.schemes {
		b := s.challenge()
		if b == nil {
			continue
		}
		value := b.absent
		if g.evaluate(c, outcomes, i).verdict == verdictInvalid {
			value = b.invalid
		}
		if !slices.Contains(header.Values("WWW-Authenticate"), value) {
			header.Add("WWW-Authenticate", value)
		}
	}
}

// grantsAll reports whether every required scope is among those granted. It
// is linear in the scopes granted for each scope required, and the required
// ones are fixed by the route's declaration.
func grantsAll(granted, required []string) bool {
	for _, scope := range required {
		if !slices.Contains(granted, scope) {
			return false
		}
	}
	return true
}

// principalTypes are the types a verifying scheme hands a route, which a [Dep]
// may name without a provider of its own when the route's security
// guarantees one.
var (
	claimsType          = reflect.TypeFor[*Claims]()
	apiKeyPrincipalType = reflect.TypeFor[*APIKeyPrincipal]()
)

// isPrincipalType reports whether t is a type a verifying scheme provides.
func isPrincipalType(t reflect.Type) bool {
	return t == claimsType || t == apiKeyPrincipalType
}

// appAuth is the security an application enforces, compiled once when it is
// built.
type appAuth struct {
	app *App
	// schemes are the verifying schemes routes name, each built once for
	// this application, by name.
	schemes map[string]credentialScheme
	// docs is the gate the documentation is served behind, from the security
	// declared on the application itself, and nil when there is none.
	docs *authGate
}

// buildAuth gives every route, mount and file mount that names a verifying
// scheme its gate, checks what cannot be enforced as declared, and registers
// each key set refresh as a lifecycle component. It runs once every route is
// resolved, and after [App.validateSecurity] has reported the schemes no
// route may name.
func (a *App) buildAuth(state *buildState) {
	auth := &appAuth{app: a, schemes: map[string]credentialScheme{}}
	a.auth = auth
	for _, rt := range a.routes {
		where := rt.Method + " " + rt.Path
		var gate *authGate
		if rt.securitySet {
			var errs []error
			gate, errs = auth.compile(rt.security, where, true)
			state.errs = append(state.errs, errs...)
		}
		if gate != nil {
			// Run by [App.run] ahead of the body capture and the guards.
			rt.gate = gate.guard()
		}
		state.errs = append(state.errs, checkPrincipalDeps(rt, gate)...)
	}
	for _, m := range state.mounts {
		if gate := auth.compileFor(m.securitySet, m.security, "mount at "+m.template, state); gate != nil {
			m.guards = append([]Guard{gate.guard()}, m.guards...)
			m.private = true
		}
	}
	for _, f := range state.frontends {
		if gate := auth.compileFor(f.securitySet, f.security, fmt.Sprintf("%s at %q", f.kind, f.mountPath()), state); gate != nil {
			f.guards = append([]Guard{gate.guard()}, f.guards...)
		}
	}
	if !a.opts.DisableDocs {
		auth.docs = auth.compileFor(a.cfg.securitySet, a.cfg.security, "the documentation", state)
	}
	for _, name := range slices.Sorted(maps.Keys(auth.schemes)) {
		if s, ok := auth.schemes[name].(*jwtScheme); ok && s.jwks != nil {
			state.lifecycles = append(state.lifecycles, s.jwks)
		}
	}
}

// compileFor compiles the security of a mount, a file mount or the
// documentation. A route's undeclared schemes are reported by
// [App.validateSecurity]; these are not routes, so they are reported here.
func (auth *appAuth) compileFor(set bool, requirements []SecurityRequirement, where string, state *buildState) *authGate {
	if !set {
		return nil
	}
	gate, errs := auth.compile(requirements, where, false)
	state.errs = append(state.errs, errs...)
	return gate
}

// compile builds the gate of one declaration, or returns nil when it names no
// verifying scheme, which leaves it documentation as it always was.
//
// Requirements are alternatives, and the gate can only judge the schemes it
// verifies. A single requirement naming a verifying scheme beside a
// descriptive one is enforced for the verifying one, and the guard that
// enforces the other refuses on its own, so both are still needed. But among
// alternatives, a request that satisfies a descriptive one could not be told
// from one that satisfies nothing, so alternatives that mix the two are a
// build error rather than a gate that would refuse a request the guard admits,
// or admit one nothing checked.
func (auth *appAuth) compile(requirements []SecurityRequirement, where string, routeDeclared bool) (*authGate, []error) {
	gate := &authGate{}
	var errs []error
	var verifying, describing []string
	index := map[string]int{}
	for _, requirement := range requirements {
		if len(requirement) == 0 {
			gate.anonymous = true
			continue
		}
		var req gateRequirement
		for _, name := range slices.Sorted(maps.Keys(requirement)) {
			declared, ok := auth.app.opts.SecuritySchemes[name]
			switch {
			case !ok:
				if !routeDeclared {
					errs = append(errs, fmt.Errorf("muzak: %s: security scheme %q is not declared; add it to AppOptions.SecuritySchemes", where, name))
				}
				continue
			case declared.verifier == nil:
				describing = append(describing, name)
				continue
			}
			verifying = append(verifying, name)
			for _, scope := range requirement[name] {
				if !isScopeToken(scope) {
					errs = append(errs, fmt.Errorf("muzak: %s: scope %q of the %q scheme is not a valid scope; "+
						"a scope is one or more printable ASCII characters other than space, '\"' and '\\'", where, scope, name))
				}
			}
			i, seen := index[name]
			if !seen {
				i = len(gate.schemes)
				index[name] = i
				gate.schemes = append(gate.schemes, auth.scheme(name, declared))
			}
			req.members = append(req.members, gateMember{scheme: i, scopes: slices.Clone(requirement[name])})
		}
		if len(req.members) == 0 {
			continue
		}
		gate.requirements = append(gate.requirements, req)
	}
	if len(verifying) == 0 {
		return nil, errs
	}
	if len(requirements) > 1 && len(describing) > 0 {
		errs = append(errs, fmt.Errorf("muzak: %s: WithSecurity offers alternatives that mix schemes Muzak verifies (%s) with schemes it only describes (%s); "+
			"a request that satisfies a described one cannot be told from one that satisfies nothing, so make every alternative verifying, "+
			"or enforce them all with a guard",
			where, quoteList(dedupeStrings(verifying)), quoteList(dedupeStrings(describing))))
	}
	if len(gate.schemes) > maxGateSchemes {
		errs = append(errs, fmt.Errorf("muzak: %s: WithSecurity names %d verifying schemes, over the limit of %d", where, len(gate.schemes), maxGateSchemes))
	}
	if len(errs) > 0 || slices.Contains(gate.schemes, nil) {
		// A scheme whose options are invalid has been reported already; the
		// application is not built either way.
		return nil, errs
	}
	gate.finish()
	return gate, nil
}

// finish computes what a refusal writes, and the headers the response varies
// on, so that a request formats none of it.
func (g *authGate) finish() {
	for ri := range g.requirements {
		req := &g.requirements[ri]
		for _, m := range req.members {
			if b := g.schemes[m.scheme].challenge(); b != nil && len(m.scopes) > 0 {
				value := b.forbidden(m.scopes)
				if !slices.Contains(req.forbidden, value) {
					req.forbidden = append(req.forbidden, value)
				}
			}
		}
	}
	for _, s := range g.schemes {
		if field := s.varyOn(); field != "" && !slices.Contains(g.vary, field) {
			g.vary = append(g.vary, field)
		}
	}
}

// scheme returns the application's instance of a verifying scheme, building
// it the first time a route names it, or nil when its options are invalid.
func (auth *appAuth) scheme(name string, declared SecurityScheme) credentialScheme {
	if s, ok := auth.schemes[name]; ok {
		return s
	}
	spec := declared.verifier
	if len(spec.errs) > 0 {
		return nil
	}
	var s credentialScheme
	if spec.jwt != nil {
		s = newJWTScheme(name, spec.jwt, Scoped(auth.app.logger, ScopeServer))
	} else {
		s = newAPIKeyScheme(name, spec.apiKey)
	}
	auth.schemes[name] = s
	return s
}

// docsGuards returns the guards the documentation runs: the gate of the
// application's own security first, then the guards given to [New].
func (a *App) docsGuards() []Guard {
	if a.auth == nil || a.auth.docs == nil {
		return a.cfg.guards
	}
	return append([]Guard{a.auth.docs.guard()}, a.cfg.guards...)
}

// isScopeToken reports whether s is a scope as RFC 6749 spells one: at least
// one character from %x21, %x23-5B and %x5D-7E. That is also what can be
// written inside the quoted scope parameter of a challenge.
func isScopeToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// quoteList quotes each name and joins them with commas, for an error.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

// checkPrincipalDeps reports every [Dep] of a principal type that the route's
// security does not always fill. A Dep is a promise that the value is there,
// so one is accepted only when every requirement of the route names a scheme
// that yields it and no requirement admits an anonymous request; otherwise
// the route reads it with [TryFrom].
func checkPrincipalDeps(rt *Route, gate *authGate) []error {
	var errs []error
	for _, d := range rt.plan.deps {
		if !isPrincipalType(d.typ) || hasProvider(rt.providers, d.typ) || gate.guarantees(d.typ) {
			continue
		}
		scheme := "JWTBearer"
		if d.typ == apiKeyPrincipalType {
			scheme = "APIKeyVerifier"
		}
		errs = append(errs, fmt.Errorf("muzak: %s %s: field %s is a muzak.Dep[%s], which only a %s scheme fills, but not every way into the route goes through one; "+
			"name such a scheme in every requirement of WithSecurity with no anonymous alternative, or read the value with muzak.TryFrom",
			rt.Method, rt.Path, d.name, d.typ, scheme))
	}
	return errs
}

// guarantees reports whether every request the gate admits carries a
// principal of type t.
func (g *authGate) guarantees(t reflect.Type) bool {
	if g == nil || g.anonymous || len(g.requirements) == 0 {
		return false
	}
	for _, req := range g.requirements {
		found := false
		for _, m := range req.members {
			if g.schemes[m.scheme].principalType() == t {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// jwtScheme is a [JWTBearer] scheme as one application runs it.
type jwtScheme struct {
	name string
	spec *jwtSpec
	// jwks is the application's copy of the key set, nil when the scheme
	// has only static keys.
	jwks *jwksCache
	ch   *bearerChallenge
}

// newJWTScheme builds an application's instance of a JWT scheme.
func newJWTScheme(name string, spec *jwtSpec, logger *slog.Logger) *jwtScheme {
	s := &jwtScheme{name: name, spec: spec}
	metadataURL := ""
	if spec.metadata != nil {
		metadataURL = spec.metadata.url
	}
	s.ch = newBearerChallenge(spec.realm, metadataURL)
	if spec.jwks != nil {
		s.jwks = newJWKSCache(name, spec, logger)
	}
	return s
}

func (s *jwtScheme) principalType() reflect.Type { return claimsType }
func (s *jwtScheme) challenge() *bearerChallenge { return s.ch }
func (s *jwtScheme) varyOn() string              { return "Authorization" }

// present reports whether the request has an Authorization header in the
// Bearer scheme, however malformed the rest of it.
func (s *jwtScheme) present(c *Context) bool {
	for _, value := range c.r.Header["Authorization"] {
		scheme, _, _ := strings.Cut(value, " ")
		if strings.EqualFold(scheme, "Bearer") {
			return true
		}
	}
	return false
}

// verify checks the request's bearer token: its structure, its signature
// against the static keys and then the key set, and its claims, in that
// order, so that nothing in a payload is read before its signature is known
// to be good.
func (s *jwtScheme) verify(c *Context) schemeOutcome {
	invalid := schemeOutcome{verdict: verdictInvalid}
	values := c.r.Header["Authorization"]
	if len(values) != 1 {
		// Two Authorization headers are two answers to one question, and
		// whichever a proxy in front would have used is not knowable here.
		return invalid
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	token = strings.TrimSpace(token)
	if !strings.EqualFold(scheme, "Bearer") || token == "" {
		return invalid
	}
	t, err := parseJWS(token, &s.spec.policy)
	if err != nil {
		return invalid
	}
	verified := verifyWith(s.spec.static.candidates(t.kid, t.hasKid), t)
	if !verified && s.jwks != nil {
		keys, err := s.jwks.candidates(c.Context(), t.kid, t.hasKid)
		if err != nil {
			return schemeOutcome{verdict: verdictUnavailable, err: err}
		}
		verified = verifyWith(keys, t)
	}
	if !verified {
		return invalid
	}
	claims, err := s.spec.readClaims(t)
	if err != nil {
		return invalid
	}
	claims.Scheme = s.name
	return schemeOutcome{verdict: verdictValid, principal: claims, scopes: claims.Scopes}
}

// verifyWith reports whether any of the keys that accept the token's
// algorithm verifies its signature. The keys are those the token's kid
// selected, so their number is bounded by the size of the set.
func verifyWith(keys []*verifyKey, t *jwsToken) bool {
	for _, k := range keys {
		if k.accepts(t.alg) && k.verify(t.alg, t.signingInput, t.signature) {
			return true
		}
	}
	return false
}

// errVerifierMismatch reports a verifying scheme whose descriptive fields were
// changed after it was built, so that what the document says is no longer
// what is enforced.
var errVerifierMismatch = errors.New("the scheme was built by a verifying constructor and then changed; " +
	"its Type, Scheme, In and Name describe what it verifies, so set them through the constructor's options")

// verifierProblems reports what is wrong with a verifying scheme: every
// problem with its options, and a description edited to say something other
// than what it enforces.
func (s SecurityScheme) verifierProblems() []error {
	v := s.verifier
	if v == nil {
		return nil
	}
	if (v.jwt != nil && (s.Type != "http" || !strings.EqualFold(s.Scheme, "bearer"))) ||
		(v.apiKey != nil && (s.Type != "apiKey" || s.In != v.apiKey.in || s.Name != v.apiKey.declared)) {
		return append(slices.Clone(v.errs), errVerifierMismatch)
	}
	return v.errs
}
