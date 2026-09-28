package muzak

import (
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
	Header string
}

// clientIPResolver answers "who sent this request" for one application, with
// the trusted prefixes parsed once rather than once per request.
type clientIPResolver struct {
	trusted []netip.Prefix
	header  string
}

// defaultClientIPResolver trusts no proxy, which is what an application that
// has not been built yet, or one that could not parse its policy, falls back
// to.
var defaultClientIPResolver = &clientIPResolver{header: DefaultForwardedHeader}

// newClientIPResolver parses a policy, reporting every entry it could not
// understand at once. A policy with any unreadable entry is not applied at
// all, because a half-applied trust policy is one nobody can reason about.
func newClientIPResolver(opts ClientIPOptions) (*clientIPResolver, error) {
	resolver := &clientIPResolver{header: opts.Header}
	if resolver.header == "" {
		resolver.header = DefaultForwardedHeader
	}
	var errs []error
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
		// Masking discards any host bits the prefix was written with, so that
		// "10.1.2.3/8" means the same as "10.0.0.0/8" rather than never
		// matching anything.
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(trimmed)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("muzak: trusted proxy %q is not a valid address or CIDR prefix: %w", entry, err)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
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
func (r *clientIPResolver) resolve(req *http.Request) netip.Addr {
	nearest, ok := peerAddr(req.RemoteAddr)
	if !ok || !r.trusts(nearest) {
		return nearest
	}
	values := req.Header.Values(r.header)
	hops := 0
	for i := len(values) - 1; i >= 0; i-- {
		rest := values[i]
		for rest != "" {
			var field string
			field, rest = lastField(rest)
			if hops++; hops > maxForwardedHops {
				return nearest
			}
			addr, parsed := parseForwardedAddr(field)
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
		return addr.Unmap(), true
	}
	host, _, err := net.SplitHostPort(field)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// peerAddr parses the address net/http recorded for the connection, which
// carries a port for a TCP listener and does not for every other kind.
func peerAddr(remoteAddr string) (netip.Addr, bool) {
	if remoteAddr == "" {
		return netip.Addr{}, false
	}
	if addrPort, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	if addr, err := netip.ParseAddr(remoteAddr); err == nil {
		return addr.Unmap(), true
	}
	return netip.Addr{}, false
}

// ClientIP returns the address the request came from, as a string.
//
// It is the peer that opened the connection unless [ClientIPOptions] names
// that peer as a trusted proxy, in which case it is the address the proxy
// reported. The result is normalised, so an address written as an
// IPv4-in-IPv6 form and the same address written plainly are one value rather
// than two, which is what stops a client from being counted twice, or from
// evading a count, by rewriting its own address.
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
	resolver := defaultClientIPResolver
	if c.app != nil && c.app.clientIP != nil {
		resolver = c.app.clientIP
	}
	return resolver.resolve(c.r)
}

// clientIPv6PrefixBits is the length of the IPv6 prefix Muzak counts as one
// client wherever it bounds what a single client may do: the default rate
// limit tracker, [IPTracker], and the per-client connection caps,
// [WSOptions.MaxConnectionsPerIP] and [SSEOptions.MaxStreamsPerIP].
//
// A /64 is the block size an IPv6 network is built from and the smallest most
// providers hand to a subscriber, so a client holding one can present a new
// address on every request without ever leaving a range that is theirs alone.
// Counting by exact address would give each of those a fresh budget. An IPv4
// address is still counted exactly, since one is neither cheap nor plentiful.
const clientIPv6PrefixBits = 64

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
// default: an IPv4 address as itself, and an IPv6 one as its
// clientIPv6PrefixBits prefix, such as "2001:db8:1:2::/64".
//
// The IPv4 form is kept bare, the same string [Context.ClientIP] returns, so
// that a counter held in a shared storage under the previous key format is
// still the one counted after an upgrade. An IPv4-mapped IPv6 address never
// reaches this as an IPv6 one, because the resolver has already unmapped it.
func clientIdentity(addr netip.Addr) string {
	if addr.Is4() {
		return addr.String()
	}
	// The length is within range for an IPv6 address by construction, so the
	// only error Prefix can report cannot happen here.
	prefix, _ := clientPrefix(addr, 32, clientIPv6PrefixBits)
	return prefix.String()
}
