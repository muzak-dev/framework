package muzak

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CodeMisdirectedRequest classifies a request for a host the application does
// not serve, refused because of [AppOptions.AllowedHosts].
const CodeMisdirectedRequest = "misdirected_request"

// misdirectedMessage is what a refused host is told, in English, and the
// fallback for the translation at muzak.http.421. It does not repeat the host,
// which the client chose and which nothing gains from being echoed.
const misdirectedMessage = "This server does not serve the requested host."

// errMisdirected is the refusal of a request whose Host is not allowed. It is
// shared, and never edited: [App.fail] only reads it.
var errMisdirected = &HTTPError{
	Status:     http.StatusMisdirectedRequest,
	Code:       CodeMisdirectedRequest,
	Message:    misdirectedMessage,
	MessageKey: "muzak.http.421",
}

// The bounds on a host. A DNS name is at most 253 characters without its
// trailing dot, and one label at most 63; anything longer is not a name any
// entry can match, so it is refused before it is examined. The longest Host
// header worth reading adds the trailing dot, a colon and a five digit port;
// an IPv6 literal in brackets is shorter than any of that.
const (
	maxHostName   = 253
	maxHostLabel  = 63
	maxHostHeader = maxHostName + len(".:65535")
)

// hostAllowlist is [AppOptions.AllowedHosts] parsed once: the exact hosts in a
// map keyed by their normalised name, and the wildcards as the suffixes they
// match.
//
// A request's Host is checked in time linear in its length, which is bounded
// by maxHostHeader before anything else is done: one map lookup for the exact
// entries and one suffix comparison per wildcard entry, and no allocation for
// a host that is not an IPv6 literal.
type hostAllowlist struct {
	exact     map[string]hostPorts
	wildcards []wildcardHost
}

// hostPorts is the ports one host is allowed with: any, or the ones listed.
type hostPorts struct {
	any   bool
	ports []uint16
}

// allows reports whether a request's port is allowed. A request that names no
// port matches only an entry that names none.
func (p hostPorts) allows(port int) bool {
	if p.any {
		return true
	}
	return port >= 0 && slices.Contains(p.ports, uint16(port)) //nolint:gosec // port is at most 65535, checked when it was parsed
}

// add widens the ports an entry allows.
func (p hostPorts) add(port int) hostPorts {
	if port < 0 {
		return hostPorts{any: true}
	}
	if !p.any && !slices.Contains(p.ports, uint16(port)) { //nolint:gosec // port is at most 65535, checked when it was parsed
		p.ports = append(p.ports, uint16(port)) //nolint:gosec // as above
	}
	return p
}

// wildcardHost is one "*." entry: the suffix a matching host ends with, which
// begins with the dot, and the ports it is allowed with.
type wildcardHost struct {
	suffix string
	ports  hostPorts
}

// newHostAllowlist parses every entry, reporting all that can never match at
// once. It returns nil for an empty list, which is the check turned off.
func newHostAllowlist(entries []string) (*hostAllowlist, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	list := &hostAllowlist{exact: make(map[string]hostPorts, len(entries))}
	var errs []error
	for _, entry := range entries {
		name, port, wildcard, err := parseHostEntry("AllowedHosts entry", entry)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !wildcard {
			list.exact[name] = list.exact[name].add(port)
			continue
		}
		suffix := "." + name
		i := slices.IndexFunc(list.wildcards, func(w wildcardHost) bool { return w.suffix == suffix })
		if i < 0 {
			list.wildcards = append(list.wildcards, wildcardHost{suffix: suffix})
			i = len(list.wildcards) - 1
		}
		list.wildcards[i].ports = list.wildcards[i].ports.add(port)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return list, nil
}

// parseHostEntry reads one [AppOptions.AllowedHosts] entry, or the host of
// [RedirectHTTPSOptions], into the normalised name it matches, its port or -1
// for any, and whether it is a "*." wildcard, or explains why it can never
// match a Host header. subject names what is being read in the message.
//
// Surrounding whitespace is ignored, as it is for a trusted proxy, since a
// list read from the environment is usually split at commas.
func parseHostEntry(subject, entry string) (name string, port int, wildcard bool, err error) {
	trimmed := strings.TrimSpace(entry)
	fail := func(why string) (string, int, bool, error) {
		return "", 0, false, fmt.Errorf("muzak: %s %q %s; a host is written as a name or an address and an optional port, "+
			"such as %q or %q", subject, entry, why, "example.com", "example.com:8443")
	}
	switch {
	case trimmed == "":
		return fail("is empty")
	case strings.Contains(trimmed, "://"):
		return fail("carries a scheme, which a Host header never does")
	case strings.ContainsAny(trimmed, "/\\"):
		return fail("carries a path, which a Host header never does")
	case strings.Contains(trimmed, "@"):
		return fail("carries credentials, which a Host header never does")
	case strings.ContainsAny(trimmed, "?#"):
		return fail("carries a query or a fragment, which a Host header never does")
	case !isASCIIOnly(trimmed):
		return fail("is not ASCII, and a client sends an internationalized name in its ASCII form; write its punycode (xn--) spelling")
	case strings.ContainsAny(trimmed, " \t"):
		return fail("holds whitespace")
	}
	rest := trimmed
	if after, ok := strings.CutPrefix(trimmed, "*."); ok {
		wildcard, rest = true, after
	}
	if strings.Contains(rest, "*") {
		return fail(`holds a "*" somewhere other than a leading "*.", where it is compared as a letter and so never matches`)
	}
	if strings.HasSuffix(rest, ":") {
		return fail("ends in a colon; name the port, or leave the colon out to allow any")
	}
	var buf hostBuffer
	normalised, port, ok := splitHostHeader(&buf, rest)
	if !ok {
		if !strings.HasPrefix(rest, "[") && strings.Count(rest, ":") > 1 {
			return fail("is an IPv6 address without brackets; write it as [2001:db8::1]")
		}
		return fail("is not a valid host name, IP address or port")
	}
	if port == 0 {
		return fail("names port 0, which no client connects to")
	}
	if wildcard && (normalised[0] == '[' || isIPv4Literal(string(normalised))) {
		return fail("puts a wildcard in front of an address, which has no subdomains")
	}
	return string(normalised), port, wildcard, nil
}

// match reports whether a request's Host is allowed.
func (l *hostAllowlist) match(host string) bool {
	var buf hostBuffer
	name, port, ok := splitHostHeader(&buf, host)
	if !ok {
		return false
	}
	// Indexing the map with the converted bytes does not copy them.
	if ports, found := l.exact[string(name)]; found && ports.allows(port) {
		return true
	}
	for _, w := range l.wildcards {
		// The suffix begins with a dot and is shorter than the name, so the
		// name has at least one label of its own in front of it: a wildcard
		// matches every subdomain and not the domain itself.
		if len(name) > len(w.suffix) && string(name[len(name)-len(w.suffix):]) == w.suffix && w.ports.allows(port) {
			return true
		}
	}
	return false
}

// hostBuffer holds a normalised host on the stack: the longest name, or an
// IPv6 literal in brackets with room to spare.
type hostBuffer [maxHostName + 2]byte

// splitHostHeader reads a Host header value into its normalised name, written
// into buf, and its port, which is -1 when none is named. It reports false for
// a value that is not a host an entry could match.
//
// The name is lowercased, ASCII only, so that a letter outside ASCII whose
// Unicode folding is an ASCII letter, the Kelvin sign for "k" for instance,
// is never taken for it; one trailing dot, which names the same host fully
// qualified, is removed. An IPv6 literal must be in brackets and carry no
// zone, and is written in its canonical form, so every spelling of one
// address matches the same entry. A name is letters, digits, hyphens and
// underscores in labels of 1 to 63 characters, at most 253 in all; nothing
// else, a percent-escape, an "@", a slash or a space included, is a host. An
// empty port, as in "example.com:", is the scheme's default and so names none.
func splitHostHeader(buf *hostBuffer, host string) (name []byte, port int, ok bool) {
	if host == "" || len(host) > maxHostHeader {
		return nil, 0, false
	}
	hostname, portText := host, ""
	if host[0] == '[' {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return nil, 0, false
		}
		hostname, portText = host[:end+1], host[end+1:]
		if portText != "" {
			if portText[0] != ':' {
				return nil, 0, false
			}
			portText = portText[1:]
		}
	} else if i := strings.LastIndexByte(host, ':'); i >= 0 {
		hostname, portText = host[:i], host[i+1:]
		if strings.IndexByte(hostname, ':') >= 0 {
			// More than one colon outside brackets: an IPv6 address written
			// bare, which a Host header may not carry because its last group
			// cannot be told from a port.
			return nil, 0, false
		}
	}
	if hostname == "" {
		return nil, 0, false
	}
	port = -1
	if portText != "" {
		if port, ok = parseHostPort(portText); !ok {
			return nil, 0, false
		}
	}
	if hostname[0] == '[' {
		addr, err := netip.ParseAddr(hostname[1 : len(hostname)-1])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return nil, 0, false
		}
		name = append(buf[:0], '[')
		name = addr.AppendTo(name)
		return append(name, ']'), port, true
	}
	hostname = strings.TrimSuffix(hostname, ".")
	if hostname == "" || len(hostname) > maxHostName {
		return nil, 0, false
	}
	name = buf[:len(hostname)]
	label := 0
	for i := 0; i < len(hostname); i++ {
		c := hostname[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		case c == '.':
			if label == 0 {
				return nil, 0, false
			}
			label = -1
		default:
			return nil, 0, false
		}
		if label++; label > maxHostLabel {
			return nil, 0, false
		}
		name[i] = c
	}
	if label == 0 {
		// A second trailing dot, which leaves an empty label at the end.
		return nil, 0, false
	}
	return name, port, true
}

// parseHostPort reads a port of one to five digits, at most 65535.
func parseHostPort(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, n <= 65535
}

// isIPv4Literal reports whether a normalised name is a dotted IPv4 address.
func isIPv4Literal(name string) bool {
	addr, err := netip.ParseAddr(name)
	return err == nil && addr.Is4()
}

// isASCIIOnly reports whether s holds only ASCII.
func isASCIIOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// quotableHost returns a Host header as a log line may record it: cut to a
// bounded length, and quoted with every byte that is not printable ASCII
// escaped, so a header crafted to forge a log line or to fill one records as
// the inert text it is.
func quotableHost(host string) string {
	host = truncateTo(host, maxHostHeader)
	if isPrintableASCII(host) {
		return host
	}
	return strconv.QuoteToASCII(host)
}

// edgeMiddleware builds the check of [AppOptions.AllowedHosts] and the
// redirect of [AppOptions.RedirectHTTPS], which run ahead of routing for every
// request, and returns nil when neither is configured, so an application that
// configures neither pays nothing for them.
func (a *App) edgeMiddleware() (Middleware, error) {
	if len(a.opts.AllowedHosts) == 0 && a.opts.RedirectHTTPS == nil {
		return nil, nil
	}
	var errs []error
	hosts, err := newHostAllowlist(a.opts.AllowedHosts)
	if err != nil {
		errs = append(errs, err)
	}
	var redirect *httpsRedirect
	if a.opts.RedirectHTTPS != nil {
		redirect, err = newHTTPSRedirect(*a.opts.RedirectHTTPS, len(a.opts.AllowedHosts) > 0)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a.edgeExempt != nil && a.edgeExempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			if hosts != nil && !hosts.match(r.Host) {
				a.refuseHost(w, r)
				return
			}
			if redirect != nil {
				rw := asResponseWriter(w)
				defer rw.commitVary()
				if redirect.redirected(rw, r, a.clientIP) {
					return
				}
				w = rw
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// refuseHost answers a request whose Host is not allowed with 421 through the
// error renderer, and records the host it named.
//
// 421 Misdirected Request is what RFC 9110 defines for a request "directed at
// a server that is unable or unwilling to produce an authoritative response
// for the target URI", which is exactly what an unlisted host is. It is also
// the status an HTTP/2 or HTTP/3 client acts on: one that reused a connection
// for a second host its certificate covers retries on a connection of its own
// when told 421, where a 400 would end the request as the client's fault.
func (a *App) refuseHost(w http.ResponseWriter, r *http.Request) {
	rw := asResponseWriter(w)
	c := a.acquire(rw, r)
	defer a.release(c)
	a.logger.WarnContext(r.Context(), "muzak: refused a request for a host this application does not serve",
		slog.String("host", quotableHost(r.Host)),
		slog.String("method", truncateForMessage(r.Method)),
		slog.String("path", truncateForMessage(r.URL.Path)),
		slog.String(RequestIDKey, c.RequestID()))
	a.fail(c, errMisdirected)
}
