package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Redirect is a response that sends the client somewhere else, with a
// Location header and no body.
//
//	r.Post("/login", func(ctx *muzak.Context, in LoginIn) (muzak.Redirect, error) {
//		if err := sessions.Start(ctx, in); err != nil {
//			return muzak.Redirect{}, err
//		}
//		return muzak.Redirect{To: "/dashboard"}, nil
//	})
//
// The target is checked before it is sent, because a redirect to a place a
// client named is how a phishing link borrows an application's domain. By
// default only a path on this application's own origin is accepted: To must
// begin with exactly one "/", and is refused when it holds anything a URI
// cannot (a space, a backslash, a control character, a tab or a line break, or
// a character outside ASCII, which a handler percent-encodes first, as
// [url.URL.String] does), or when its path, percent-decoded as often as three
// times and with leading "." and ".." segments removed, begins with "//" or
// "/\", which a browser or a server that decodes once more reads as the name
// of another host. A query and a fragment may hold anything a URI may.
//
// An absolute URL is accepted only with the http or https scheme, a host and
// no user information, and only when the host is listed with [RedirectHosts]
// on the route or a router above it, or when External is set. A listed host
// matches case-insensitively and exactly otherwise: "accounts.example.com"
// admits neither "accounts.example.com.evil.com" nor "accounts.example.com."
// nor another port; see [RedirectHosts].
//
// A target that is refused, and a Status outside 301, 302, 303, 307 and 308,
// fails the request with a 500 whose cause is logged without the target,
// which may carry a token.
//
// A route returning Redirect is documented with the status it answers with
// and a Location header. Its status is the route's declared [Status] when that
// is a redirect status, and otherwise 302 Found for GET and HEAD and 303 See
// Other for every other method: after a form is posted, 303 is the one status
// every client follows with a GET, which is what keeps a refresh from posting
// the form again, while for a GET, 302 is the conventional "look over there"
// that any client handles. Declaring a status that is not a redirect on such
// a route is a build error.
type Redirect struct {
	// To is where the client is sent: a path on this origin such as
	// "/items/42?tab=history", or an absolute http or https URL whose host
	// [RedirectHosts] lists or that External vouches for.
	To string

	// Status is the redirect status to answer with: 301 or 308 for a move
	// that is permanent, 302, 303 or 307 for one that is not. 307 and 308
	// make the client repeat the method and body it used; 301, 302 and 303
	// let it switch to GET. Zero uses the route's status, as the type's
	// documentation describes.
	Status int

	// External accepts an absolute http or https URL to any host, for a
	// target the handler built from its own configuration rather than from
	// anything a client sent, such as an identity provider's authorization
	// endpoint. The target must still be a well-formed URL with a host and no
	// user information. Never set it for a URL taken from the request.
	External bool
}

// RedirectHosts lists the hosts a [Redirect] from a route, or from every route
// beneath a router, may send a client to by absolute URL. Declared on [New],
// it applies to the whole application:
//
//	app := muzak.New(opts, muzak.RedirectHosts("accounts.example.com", "localhost:5173"))
//
// An entry is a host name or an IP address, with an optional port; an IPv6
// address is written in brackets. A host is compared without regard to case,
// and must otherwise be exactly the one listed: there are no wildcards, a
// subdomain is not admitted by its parent, and a name with a trailing dot is
// not admitted by one without. An entry with no port admits the scheme's
// default port only, written or not, so another service on the same host is
// not reachable through it; an entry with a port admits that port only. An
// entry that is not a host is a build error.
//
// Lists add up: a route may send a client to any host it or a router above it
// lists.
func RedirectHosts(hosts ...string) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.output.redirectHosts = append(c.output.redirectHosts, hosts...) },
		router: func(c *routerConfig) { c.output.redirectHosts = append(c.output.redirectHosts, hosts...) },
	}
}

// redirectHost is one parsed [RedirectHosts] entry: a lower-case host, without
// the brackets of an IPv6 address, and its port, or "" for the default one.
type redirectHost struct {
	host string
	port string
}

// maxRedirectHostLength bounds a [RedirectHosts] entry, which is a DNS name of
// at most 253 characters with a port, or an address.
const maxRedirectHostLength = 270

// parseRedirectHost reads one [RedirectHosts] entry.
func parseRedirectHost(entry string) (redirectHost, error) {
	invalid := func(reason string) (redirectHost, error) {
		return redirectHost{}, fmt.Errorf("RedirectHosts was given %q, %s; give a host such as \"accounts.example.com\" or \"localhost:8080\"",
			truncateForMessage(entry), reason)
	}
	if entry == "" || len(entry) > maxRedirectHostLength {
		return invalid("which is not a host")
	}
	if strings.ContainsAny(entry, "/@*?#%") {
		return invalid("which is not a host name: a scheme, a path, user information and wildcards are not accepted")
	}
	host, port := entry, ""
	if strings.HasPrefix(entry, "[") {
		end := strings.IndexByte(entry, ']')
		if end < 0 {
			return invalid("which opens an IPv6 address it does not close")
		}
		host = entry[1:end]
		if rest := entry[end+1:]; rest != "" {
			if rest[0] != ':' {
				return invalid("which has something other than a port after its address")
			}
			port = rest[1:]
		}
		if addr, err := netip.ParseAddr(host); err != nil || !addr.Is6() || addr.Zone() != "" {
			return invalid("which is not an IPv6 address")
		}
	} else {
		if i := strings.LastIndexByte(entry, ':'); i >= 0 {
			host, port = entry[:i], entry[i+1:]
		}
		if !validHostName(host) {
			return invalid("which is not a host name")
		}
	}
	if entry[len(entry)-1] == ':' || (port != "" && !validPort(port)) {
		return invalid("whose port is not a number from 1 to 65535")
	}
	return redirectHost{host: strings.ToLower(host), port: port}, nil
}

// validHostName reports whether s is a DNS name or an IPv4 address written as
// one: labels of letters, digits and hyphens, none empty and none longer than
// 63, at most 253 characters in all, and no trailing dot.
func validHostName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// validPort reports whether s is a decimal port from 1 to 65535, written
// without a sign or leading zeros.
func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == s
}

// isRedirectStatus reports whether status is one a [Redirect] may answer with.
func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// defaultRedirectStatus is the status a [Redirect] answers with when neither
// it nor the route names one; see the type for why it depends on the method.
func defaultRedirectStatus(method string) int {
	if method == http.MethodGet || method == http.MethodHead {
		return http.StatusFound
	}
	return http.StatusSeeOther
}

// writeRedirect writes a [Redirect].
func (c *Context) writeRedirect(out Redirect) error {
	if c.w.written {
		return nil
	}
	status := out.Status
	if status == 0 {
		status = c.status
		if !isRedirectStatus(status) {
			status = defaultRedirectStatus(c.r.Method)
		}
	}
	if !isRedirectStatus(status) {
		return fmt.Errorf("muzak: %s %s returned a muzak.Redirect with status %d; use 301, 302, 303, 307 or 308",
			c.r.Method, c.route.pathOrRequest(c.r), status)
	}
	var hosts []redirectHost
	if c.route != nil {
		hosts = c.route.output.redirectHosts
	}
	if err := checkRedirectTarget(out.To, out.External, hosts); err != nil {
		// The target is left out of the message: it may carry a token in its
		// query, and the reason is what the operator needs.
		return fmt.Errorf("muzak: %s %s returned a muzak.Redirect that was refused: %w",
			c.r.Method, c.route.pathOrRequest(c.r), err)
	}
	c.w.Header().Set("Location", out.To)
	c.w.WriteHeader(status)
	return nil
}

// maxRedirectLength bounds a [Redirect] target. It is longer than any URL a
// browser keeps in its address bar, and short enough that every check below,
// each a pass over the target, stays cheap.
const maxRedirectLength = 8 << 10

// maxRedirectDecodes is how many times a relative target's path is
// percent-decoded before it is judged. One decoding is what a server behind a
// careless proxy applies, and each further one covers a further layer of
// escaping an attacker can hide "//" behind; three covers every layer a real
// deployment stacks.
const maxRedirectDecodes = 3

// Reasons a [Redirect] target is refused, written for the log.
var (
	errRedirectEmpty     = errors.New("its target is empty")
	errRedirectLong      = fmt.Errorf("its target is longer than %d bytes", maxRedirectLength)
	errRedirectCharacter = errors.New("its target holds a character a URI cannot, such as a space, a backslash, a control character or one outside ASCII; percent-encode it")
	errRedirectEscape    = errors.New("its target holds a percent sign that does not begin an escape")
	errRedirectRelative  = errors.New(`a relative target must begin with "/"`)
	errRedirectAuthority = errors.New(`its path begins, once decoded and with dot segments removed, with "//" or "/\", which a client reads as another host`)
	errRedirectControl   = errors.New("its path decodes to a control character")
	errRedirectURL       = errors.New("its target is not a URL")
	errRedirectScheme    = errors.New("only http and https targets are accepted")
	errRedirectNoHost    = errors.New(`an absolute target must name a host after "//"`)
	errRedirectUserinfo  = errors.New("its target carries user information, which disguises the host it really names")
	errRedirectEncoded   = errors.New("its target's host is percent-encoded")
)

// checkRedirectTarget reports why a [Redirect] may not send a client to the
// target, or nil when it may. It is linear in the target, which it bounds.
func checkRedirectTarget(to string, external bool, hosts []redirectHost) error {
	switch {
	case to == "":
		return errRedirectEmpty
	case len(to) > maxRedirectLength:
		return errRedirectLong
	}
	for i := 0; i < len(to); i++ {
		switch c := to[i]; {
		case c == '%':
			if i+2 >= len(to) || !isHex(to[i+1]) || !isHex(to[i+2]) {
				return errRedirectEscape
			}
		case !isURIChar(c):
			return errRedirectCharacter
		}
	}
	if to[0] == '/' {
		return checkRelativeRedirect(to)
	}
	return checkAbsoluteRedirect(to, external, hosts)
}

// checkRelativeRedirect judges a target that begins with "/", whose
// characters [checkRedirectTarget] has already checked.
//
// A browser reads "//host" and "/\host" as another host, and strips tabs and
// line breaks before it reads anything, which is why those were refused
// already. What is left is a path that only becomes one of those after
// something decodes it or resolves its dot segments, a proxy or a framework
// that redirects on its own, so the path is decoded up to
// [maxRedirectDecodes] times and its leading "." and ".." segments removed,
// and must still not begin with a second separator.
func checkRelativeRedirect(to string) error {
	path := to
	if end := strings.IndexAny(to, "?#"); end >= 0 {
		path = to[:end]
	}
	for range maxRedirectDecodes {
		decoded := unescapeLenient(path)
		if decoded == path {
			break
		}
		path = decoded
	}
	for i := 0; i < len(path); i++ {
		if path[i] < ' ' || path[i] == 0x7f {
			return errRedirectControl
		}
	}
	rest := path[1:]
	for {
		end := strings.IndexAny(rest, `/\`)
		if end < 0 {
			break
		}
		if segment := rest[:end]; segment != "." && segment != ".." {
			break
		}
		rest = rest[end+1:]
	}
	if rest != "" && (rest[0] == '/' || rest[0] == '\\') {
		return errRedirectAuthority
	}
	return nil
}

// checkAbsoluteRedirect judges a target that does not begin with "/".
func checkAbsoluteRedirect(to string, external bool, hosts []redirectHost) error {
	scheme, rest, found := strings.Cut(to, ":")
	if !found {
		return errRedirectRelative
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		if !validScheme(scheme) {
			return errRedirectRelative
		}
		return errRedirectScheme
	}
	if !strings.HasPrefix(rest, "//") {
		// "https:example.com" and "https:/example.com" are both read by a
		// browser as https://example.com, and by url.Parse as having no host.
		return errRedirectNoHost
	}
	authority := rest[2:]
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if strings.ContainsRune(authority, '@') {
		return errRedirectUserinfo
	}
	if strings.ContainsRune(authority, '%') {
		// url.Parse decodes an escape in a host, and so does a browser, so
		// "accounts%2Eexample.com" would be compared decoded and sent encoded.
		// Comparing it the way both read it is possible; refusing it is
		// simpler, and nothing legitimate escapes a host.
		return errRedirectEncoded
	}
	u, err := url.Parse(to)
	if err != nil || u.Opaque != "" {
		return errRedirectURL
	}
	if u.Host == "" || u.Hostname() == "" {
		return errRedirectNoHost
	}
	if external {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	for _, allowed := range hosts {
		if allowed.host != host {
			continue
		}
		if port == allowed.port || (allowed.port == "" && port == defaultPort(scheme)) {
			return nil
		}
	}
	return fmt.Errorf("its target's host %q is not listed with muzak.RedirectHosts; list it there, or set External for a URL the handler built itself",
		truncateTo(u.Host, 256))
}

// defaultPort is the port a scheme implies when a URL names none.
func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// validScheme reports whether s is a URI scheme as RFC 3986 section 3.1 spells
// it, which tells "javascript:" apart from a relative path such as "a:b"
// that has no scheme at all.
func validScheme(s string) bool {
	if s == "" || !('a' <= s[0] && s[0] <= 'z') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// isURIChar reports whether c may appear in a URI reference as RFC 3986
// spells it: an unreserved or a reserved character, or the percent sign that
// begins an escape.
func isURIChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-._~:/?#[]@!$&'()*+,;=%", c) >= 0
}

// isHex reports whether c is a hexadecimal digit.
func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// unhex returns the value of a hexadecimal digit [isHex] accepted.
func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	}
	return c - 'a' + 10
}

// unescapeLenient decodes every well-formed escape in s and leaves anything
// else as it is, which is how a browser and most servers read a path. It
// returns s itself, allocating nothing, when there is nothing to decode.
func unescapeLenient(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
