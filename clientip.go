package muzak

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DefaultForwardedHeader is the header consulted for the client's address when
// the request arrived from a trusted proxy and [ClientIPOptions.Header] does
// not name a different one.
const DefaultForwardedHeader = "X-Forwarded-For"

// maxForwardedHops bounds how many entries of a forwarding header are read
// before the walk gives up.
//
// The header is written by whatever sat in front of the server, and the part
// of it that came from the client is under the client's control: without a
// bound, a request carrying ten thousand addresses would have all ten thousand
// of them parsed on the way to deciding who sent it.
const maxForwardedHops = 64

// Defaults for how widely the per-client connection caps,
// [WSOptions.MaxConnectionsPerIP] and [SSEOptions.MaxStreamsPerIP], group
// addresses into one client. See [ClientIPOptions.ConnectionIPv6Prefix] and
// [ClientIPOptions.ConnectionIPv4Prefix].
const (
	// DefaultConnectionIPv6Prefix counts an IPv6 client by its /56. A /56 is
	// what most providers delegate to one home or small site, and what
	// RFC 6177 recommends they do, so a single subscriber holds 256 /64s and
	// counting any narrower would give each of them an allowance of its own.
	DefaultConnectionIPv6Prefix = 56
	// DefaultConnectionIPv4Prefix counts an IPv4 client by its exact
	// address, since one is neither cheap nor plentiful.
	DefaultConnectionIPv4Prefix = 32
)

// The narrowest bounds a connection prefix may be widened to. Anything wider
// is an allocation to a provider rather than to a subscriber, so one busy
// client would lock out every unrelated customer who shares it.
const (
	minConnectionIPv6Prefix = 32
	minConnectionIPv4Prefix = 16
)

// ClientIPOptions decides how Muzak works out which address a request came
// from.
//
// The zero value trusts nothing: the address of the peer that opened the
// connection is the client's address, and every forwarding header is ignored.
// That is the only safe default, because a forwarding header is a request
// header like any other, and a server that believes one without knowing who
// wrote it lets any client claim any address it likes. For a rate limiter, a
// deny list or an audit log, that is the whole game.
//
// Behind a proxy the default is wrong in the other direction, since every
// request then appears to come from the proxy. Naming the proxy in
// TrustedProxies is what makes the header believable:
//
//	app := muzak.New(muzak.AppOptions{
//		ClientIP: muzak.ClientIPOptions{TrustedProxies: []string{"10.0.0.0/8"}},
//	})
type ClientIPOptions struct {
	// TrustedProxies lists the addresses whose forwarding header is believed,
	// as plain addresses ("10.1.2.3") or CIDR prefixes ("10.0.0.0/8"). Both
	// address families are accepted.
	//
	// An entry that cannot be parsed is reported when the application is
	// built, rather than quietly widening or narrowing the policy.
	TrustedProxies []string

	// Header names the forwarding header to read, defaulting to
	// [DefaultForwardedHeader]. Set it to the header your proxy actually
	// writes, such as "CF-Connecting-IP" or "X-Real-IP", when that is not
	// X-Forwarded-For. It is consulted only for a request whose peer is
	// trusted.
	//
	// "Forwarded", the standard header of RFC 7239, is read as that RFC
	// defines it: the address is the for parameter of each entry, as in
	// for=203.0.113.9;proto=https, a quoted "[2001:db8::1]:443" included, and
	// other parameters are skipped. An entry whose for is "unknown" or an
	// obfuscated identifier such as "_hidden", or that has no for at all, ends
	// the walk at the nearest proxy already passed, exactly as an unreadable
	// address does in any other header.
	Header string

	// ConnectionIPv6Prefix is the length, in bits, of the IPv6 prefix that
	// the per-client connection caps, [WSOptions.MaxConnectionsPerIP] and
	// [SSEOptions.MaxStreamsPerIP], and the default rate limit tracker,
	// [IPTracker], count as one client. It defaults to
	// [DefaultConnectionIPv6Prefix], a /56.
	//
	// A client is handed a whole range of IPv6 addresses rather than one, and
	// can open every connection from a different address in it, so counting
	// addresses one by one would give each a fresh allowance. The prefix is
	// the range treated as one client. Set 48 where providers delegate a /48
	// to each site, or 64 where many unrelated users share one /56, such as
	// a campus or a carrier that hands each device a single /64; any length
	// from 32 to 128 is accepted, and a value outside that is reported when
	// the application is built. It is also the prefix the rate limiter's
	// default tracker, [IPTracker], counts as one client, so that one
	// subscriber has one budget however it is limited; [IPPrefixTracker] is
	// how a single route counts a different length.
	ConnectionIPv6Prefix int

	// ConnectionIPv4Prefix is the same for IPv4, defaulting to
	// [DefaultConnectionIPv4Prefix], which counts each address on its own.
	// Widen it only where one client is known to hold a whole block, and
	// never past what that client holds, because every other address in the
	// block then shares its allowance. Any length from 16 to 32 is accepted.
	ConnectionIPv4Prefix int
}

// clientIPResolver answers "who sent this request" for one application, with
// the trusted prefixes parsed once rather than once per request.
type clientIPResolver struct {
	trusted []netip.Prefix
	header  string
	// rfc7239 reports that header is Forwarded, whose entries are lists of
	// parameters to read the address out of rather than addresses.
	rfc7239 bool

	// connIPv4Bits and connIPv6Bits are the prefix lengths the per-client
	// connection caps group an address by, already validated.
	connIPv4Bits int
	connIPv6Bits int
}

// defaultClientIPResolver trusts no proxy, which is what an application that
// has not been built yet, or one that could not parse its policy, falls back
// to.
var defaultClientIPResolver = &clientIPResolver{
	header:       DefaultForwardedHeader,
	connIPv4Bits: DefaultConnectionIPv4Prefix,
	connIPv6Bits: DefaultConnectionIPv6Prefix,
}

// newClientIPResolver parses a policy, reporting every entry it could not
// understand at once. A policy with any unreadable entry is not applied at
// all, because a half-applied trust policy is one nobody can reason about.
func newClientIPResolver(opts ClientIPOptions) (*clientIPResolver, error) {
	resolver := &clientIPResolver{
		header:       opts.Header,
		connIPv4Bits: cmp.Or(opts.ConnectionIPv4Prefix, DefaultConnectionIPv4Prefix),
		connIPv6Bits: cmp.Or(opts.ConnectionIPv6Prefix, DefaultConnectionIPv6Prefix),
	}
	if resolver.header == "" {
		resolver.header = DefaultForwardedHeader
	}
	resolver.rfc7239 = strings.EqualFold(resolver.header, "Forwarded")
	var errs []error
	if resolver.connIPv6Bits < minConnectionIPv6Prefix || resolver.connIPv6Bits > 128 {
		errs = append(errs, fmt.Errorf("muzak: ClientIP.ConnectionIPv6Prefix is %d, but it must be between %d and 128; "+
			"a wider prefix is a provider's allocation rather than one client's, so leave it at zero for the /%d default or choose a length in range",
			opts.ConnectionIPv6Prefix, minConnectionIPv6Prefix, DefaultConnectionIPv6Prefix))
	}
	if resolver.connIPv4Bits < minConnectionIPv4Prefix || resolver.connIPv4Bits > 32 {
		errs = append(errs, fmt.Errorf("muzak: ClientIP.ConnectionIPv4Prefix is %d, but it must be between %d and 32; "+
			"a wider prefix groups unrelated networks into one client, so leave it at zero to count each address or choose a length in range",
			opts.ConnectionIPv4Prefix, minConnectionIPv4Prefix))
	}
	for _, entry := range opts.TrustedProxies {
		prefix, err := parseTrustedProxy(entry)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		resolver.trusted = append(resolver.trusted, prefix)
	}
	if len(errs) > 0 {
		return defaultClientIPResolver, errors.Join(errs...)
	}
	return resolver, nil
}

// parseTrustedProxy accepts either a bare address or a CIDR prefix, turning
// the first into the prefix that contains only itself.
func parseTrustedProxy(entry string) (netip.Prefix, error) {
	trimmed := strings.TrimSpace(entry)
	if strings.Contains(trimmed, "/") {
		prefix, err := netip.ParsePrefix(trimmed)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("muzak: trusted proxy %q is not a valid CIDR prefix: %w", entry, err)
		}
		// A prefix written in the IPv4-in-IPv6 form, such as
		// "::ffff:10.0.0.0/104", is the IPv4 prefix it wraps. Every address
		// is unmapped before it is compared, so left as written it would
		// parse, match nothing, and leave the proxy it names silently
		// untrusted. Its length counts the 96 bits of the ::ffff: prefix,
		// which a wider one does not reach into and so stays what it is.
		if addr := prefix.Addr(); addr.Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96)
		}
		// Masking discards any host bits the prefix was written with, so that
		// "10.1.2.3/8" means the same as "10.0.0.0/8" rather than never
		// matching anything.
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(trimmed)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("muzak: trusted proxy %q is not a valid address or CIDR prefix: %w", entry, err)
	}
	addr = normalizeAddr(addr)
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// normalizeAddr puts an address in the one form every comparison and every key
// is made from: an IPv4-in-IPv6 address unwrapped to the IPv4 it carries, and
// an IPv6 zone dropped.
//
// The zone is what a link-local address is scoped to on the host that wrote
// it ("fe80::1%eth0"), and it means nothing to this one. Kept, it would make
// a trusted prefix never contain the address, since a prefix does not contain
// a zoned address, and it would make one client many, since a client that
// appends "%a", "%b" and so on to its own address is a new string each time.
func normalizeAddr(addr netip.Addr) netip.Addr {
	return addr.WithZone("").Unmap()
}

// trusts reports whether an address is one whose forwarding header may be
// believed.
func (r *clientIPResolver) trusts(addr netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// resolve returns the address the request came from.
//
// The walk goes right to left through the forwarding header, which is the only
// direction that cannot be lied to: entries are appended by each hop, so the
// rightmost were written by the proxies nearest this server and the leftmost by
// whoever sent the request, who may have invented them. Each trusted hop is
// stepped over, and the first entry that is not a trusted proxy is the client.
// An entry of the Forwarded header is a list of parameters rather than an
// address, so it is split and read by its own pair of functions, and walked
// the same way.
func (r *clientIPResolver) resolve(req *http.Request) netip.Addr {
	nearest, ok := peerAddr(req.RemoteAddr)
	if !ok || !r.trusts(nearest) {
		return nearest
	}
	split, parse := lastField, parseForwardedAddr
	if r.rfc7239 {
		split, parse = lastForwardedElement, parseForwardedElement
	}
	values := req.Header.Values(r.header)
	hops := 0
	for i := len(values) - 1; i >= 0; i-- {
		rest := values[i]
		for rest != "" {
			var field string
			field, rest = split(rest)
			if hops++; hops > maxForwardedHops {
				return nearest
			}
			addr, parsed := parse(field)
			if !parsed {
				// A chain with an unreadable entry cannot be walked any
				// further: an obfuscated identifier or a mangled address hides
				// whatever is to its left, so the nearest hop that was
				// understood is the most that can be claimed.
				return nearest
			}
			if !r.trusts(addr) {
				return addr
			}
			nearest = addr
		}
	}
	// Every hop in the chain was a trusted proxy, so the leftmost entry is as
	// close to the client as this server can see.
	return nearest
}

// lastField splits the rightmost comma-separated field off a header value,
// returning it and what remains to its left. Splitting from the right keeps
// the walk allocation free, which matters because it runs on every request an
// application serves behind a proxy.
func lastField(value string) (field, rest string) {
	if i := strings.LastIndexByte(value, ','); i >= 0 {
		return strings.TrimSpace(value[i+1:]), value[:i]
	}
	return strings.TrimSpace(value), ""
}

// parseForwardedAddr reads one entry of a forwarding header, tolerating the
// port some proxies append and the brackets IPv6 needs when they do.
func parseForwardedAddr(field string) (netip.Addr, bool) {
	if field == "" {
		return netip.Addr{}, false
	}
	if addr, err := netip.ParseAddr(field); err == nil {
		return normalizeAddr(addr), true
	}
	host, _, err := net.SplitHostPort(field)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return normalizeAddr(addr), true
}

// lastForwardedElement splits the rightmost entry off a Forwarded header
// value, the way [lastField] does for a plain list, except that a comma inside
// a quoted string is part of the string rather than the end of an entry.
//
// That distinction is what keeps a proxy's own entry whole. A proxy may quote
// something the client chose into a parameter of the entry it appends, its
// Host header as host="..." for instance, and a split on every comma would
// let that text end the proxy's entry early and present the rest as a for of
// the client's choosing. Read from the right, the proxy's entry comes first
// and is well formed, so its quotes pair up; a quote the client left open to
// the left of it is only reached once that entry is done, and an entry that
// never closes its quote fails to parse rather than being believed.
func lastForwardedElement(value string) (element, rest string) {
	quoted := false
	for i := len(value) - 1; i >= 0; i-- {
		switch value[i] {
		case '"':
			// Met from the right, a quote outside a string closes one, and a
			// quote inside one opens it unless it is escaped.
			if !quoted || !escapedAt(value, i) {
				quoted = !quoted
			}
		case ',':
			if !quoted {
				return strings.TrimSpace(value[i+1:]), value[:i]
			}
		}
	}
	return strings.TrimSpace(value), ""
}

// escapedAt reports whether the byte at i is escaped by a quoted-pair, which it
// is when an odd number of backslashes run up to it: each pair of them is one
// escaped backslash, and only a backslash left over escapes what follows.
func escapedAt(value string, i int) bool {
	run := 0
	for i--; i >= 0 && value[i] == '\\'; i-- {
		run++
	}
	return run%2 == 1
}

// parseForwardedElement reads the address out of one entry of a Forwarded
// header: the node its for parameter names, with any port and the brackets an
// IPv6 address carries removed.
//
// An entry that is not well formed, has no for or names it twice is
// unreadable, and so is a for of "unknown" or an obfuscated identifier such as
// "_hidden", since neither is an address. The walk stops at the nearest
// trusted hop for any of them, as it does for an unreadable entry of a plain
// list, because whatever is to its left can no longer be attributed.
func parseForwardedElement(element string) (netip.Addr, bool) {
	node, ok := forwardedFor(element)
	if !ok {
		return netip.Addr{}, false
	}
	// The RFC brackets an IPv6 address whether or not a port follows, and a
	// bracketed address with no port is not something parseForwardedAddr
	// reads, so the brackets come off first. An address with a port goes
	// through as it is, and so does an unbracketed one, which some proxies
	// write and which is not ambiguous without a port.
	if len(node) > 2 && node[0] == '[' && node[len(node)-1] == ']' {
		node = node[1 : len(node)-1]
	}
	return parseForwardedAddr(node)
}

// forwardedFor returns the value of the for parameter of one Forwarded entry,
// unquoted. Parameter names are compared without regard to case, as the RFC
// says they are, and the other parameters are skipped. It reports false for an
// entry that is not well formed, has no for, or has more than one, which the
// RFC forbids and which leaves no way to tell which was written by whom.
func forwardedFor(element string) (string, bool) {
	var node string
	found := false
	for element != "" {
		pair, rest, ok := nextForwardedPair(element)
		if !ok {
			return "", false
		}
		element = rest
		name, value, _ := strings.Cut(pair, "=")
		if !strings.EqualFold(strings.TrimSpace(name), "for") {
			continue
		}
		if found {
			return "", false
		}
		node, ok = unquoteForwardedValue(strings.TrimSpace(value))
		if !ok {
			return "", false
		}
		found = true
	}
	return node, found
}

// nextForwardedPair splits the first parameter off a Forwarded entry, where a
// semicolon inside a quoted string is part of the string. It reports false for
// a quoted string that is never closed.
func nextForwardedPair(element string) (pair, rest string, ok bool) {
	quoted := false
	for i := 0; i < len(element); i++ {
		switch c := element[i]; {
		case quoted && c == '\\':
			// A quoted-pair: whatever follows the backslash is part of the
			// string, a quote included.
			i++
		case c == '"':
			quoted = !quoted
		case c == ';' && !quoted:
			return element[:i], element[i+1:], true
		}
	}
	return element, "", !quoted
}

// unquoteForwardedValue returns a parameter value as text: a token as it is,
// and a quoted string without its quotes and with its quoted-pairs resolved.
// A value that is neither, one with a quote or a backslash outside a quoted
// string or a quote left unescaped inside one, is refused.
func unquoteForwardedValue(value string) (string, bool) {
	if !strings.HasPrefix(value, `"`) {
		return value, !strings.ContainsAny(value, `"\`)
	}
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", false
	}
	inner := value[1 : len(value)-1]
	if !strings.ContainsAny(inner, `"\`) {
		return inner, true
	}
	var unquoted strings.Builder
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch c {
		case '"':
			return "", false
		case '\\':
			if i++; i == len(inner) {
				return "", false
			}
			c = inner[i]
		}
		unquoted.WriteByte(c)
	}
	return unquoted.String(), true
}

// peerAddr parses the address net/http recorded for the connection, which
// carries a port for a TCP listener and does not for every other kind.
func peerAddr(remoteAddr string) (netip.Addr, bool) {
	if remoteAddr == "" {
		return netip.Addr{}, false
	}
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return normalizeAddr(addrPort.Addr()), true
	}
	if addr, err := netip.ParseAddr(remoteAddr); err == nil {
		return normalizeAddr(addr), true
	}
	return netip.Addr{}, false
}

// ClientIP returns the address the request came from, as a string.
//
// It is the peer that opened the connection unless [ClientIPOptions] names
// that peer as a trusted proxy, in which case it is the address the proxy
// reported. The result is normalised, so an address written as an
// IPv4-in-IPv6 form and the same address written plainly are one value rather
// than two, and an IPv6 address carries no zone ("%eth0"), which is what stops
// a client from being counted twice, or from evading a count, by rewriting its
// own address.
//
// It returns the empty string when the connection has no address that can be
// parsed, which happens on a listener that is not addressed by IP, such as a
// Unix socket. Code that keys on the result must handle that; see [IPTracker]
// for how the rate limiter does.
func (c *Context) ClientIP() string {
	addr := c.ClientAddr()
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

// ClientAddr returns the address the request came from, as a [net/netip.Addr],
// for callers that want to compare it against a prefix rather than render it.
// The zero Addr, whose IsValid reports false, means the connection has no
// address that can be parsed.
func (c *Context) ClientAddr() netip.Addr {
	return c.clientIPResolver().resolve(c.r)
}

// clientIPResolver returns the application's resolver, or the default one for
// a context that has no application, as some tests build.
func (c *Context) clientIPResolver() *clientIPResolver {
	if c.app != nil && c.app.clientIP != nil {
		return c.app.clientIP
	}
	return defaultClientIPResolver
}

// connectionKey renders the key the per-client connection caps count a valid
// address under: the address itself when the configured prefix covers all of
// it, and the prefix otherwise, such as "2001:db8:0:ab00::/56".
func (r *clientIPResolver) connectionKey(addr netip.Addr) string {
	bits := r.connIPv4Bits
	if addr.Is6() {
		bits = r.connIPv6Bits
	}
	if bits >= addr.BitLen() {
		return addr.String()
	}
	// The length was validated for its family when the resolver was built,
	// so the only error Prefix can report cannot happen here.
	prefix, _ := addr.Prefix(bits)
	return prefix.String()
}

// clientPrefix truncates an address to the prefix length kept for its family.
// It is the arithmetic [IPPrefixTracker] is made of, and [clientIdentity]
// applies it with the lengths Muzak uses by default.
func clientPrefix(addr netip.Addr, ipv4Bits, ipv6Bits int) (netip.Prefix, error) {
	bits := ipv4Bits
	if addr.Is6() {
		bits = ipv6Bits
	}
	return addr.Prefix(bits)
}

// clientIdentity renders the key a valid client address is counted under by
// default: an IPv4 address as itself, and an IPv6 one as its ipv6Bits prefix,
// such as "2001:db8:1::/56". [IPTracker] passes the application's
// [ClientIPOptions.ConnectionIPv6Prefix], a /56 unless it is configured, so
// that one subscriber is one client for the rate limit as it is for the
// connection caps.
//
// The IPv4 form is kept bare, the same string [Context.ClientIP] returns, so
// that a counter held in a shared storage under the previous key format is
// still the one counted after an upgrade. An IPv4-mapped IPv6 address never
// reaches this as an IPv6 one, because the resolver has already unmapped it.
func clientIdentity(addr netip.Addr, ipv6Bits int) string {
	if addr.Is4() {
		return addr.String()
	}
	// The length was validated when the resolver was built, and is within
	// range for an IPv6 address, so the only error Prefix can report cannot
	// happen here.
	prefix, _ := clientPrefix(addr, 32, ipv6Bits)
	return prefix.String()
}
