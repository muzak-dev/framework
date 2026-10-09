package muzak

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// AddressRefusedError reports a request a [Client] would not send, because the
// address it would have connected to is one its policy refuses.
//
// It is what a request forgery looks like from the inside: a URL that came
// from somewhere else naming the loopback interface, a private network or a
// cloud metadata service, either directly, through a name that resolves
// there, or through a redirect that points there. Detect it with [errors.As]:
//
//	var refused *muzak.AddressRefusedError
//	if errors.As(err, &refused) {
//		return muzak.BadRequest("that address cannot be fetched")
//	}
//
// A refusal is never retried, and nothing was sent: the check runs before the
// connection is opened.
type AddressRefusedError struct {
	// Host is the host as the request named it, or the address the dialer was
	// about to connect to when the refusal came from there.
	Host string
	// Addr is the address that was refused. It is the zero Addr when the host
	// was refused by its name alone, as a name ending in a number that is not
	// an address is.
	Addr netip.Addr
	// Reason is what the address is, as a phrase such as "a loopback
	// address" or "a cloud metadata service address".
	Reason string

	// kind decides which remedy the message offers.
	kind refusalKind
}

// refusalKind distinguishes the refusals that are lifted in different ways.
type refusalKind uint8

const (
	// refusedClass is an address in a special-purpose range, which
	// AllowPrivateNetworks or AllowedNetworks reaches.
	refusedClass refusalKind = iota
	// refusedMetadata is a metadata service, which only AllowedNetworks
	// reaches.
	refusedMetadata
	// refusedDenied is an address listed in DeniedNetworks, which nothing
	// reaches.
	refusedDenied
	// refusedAmbiguous is a host that resolvers disagree about, which is
	// reached by writing it differently.
	refusedAmbiguous
)

// Error implements the error interface.
func (e *AddressRefusedError) Error() string {
	target := strconv.Quote(clientShorten(e.Host))
	if e.Addr.IsValid() && e.Addr.String() != e.Host {
		target += " (" + e.Addr.String() + ")"
	}
	message := "muzak: the client refused to connect to " + target + ", which is " + e.Reason
	switch e.kind {
	case refusedMetadata:
		message += "; only an entry in ClientOptions.AllowedNetworks reaches it, because AllowPrivateNetworks never does"
	case refusedDenied:
	case refusedAmbiguous:
		message += "; write the address in its usual dotted form if it is the one meant"
	default:
		message += "; set ClientOptions.AllowPrivateNetworks, or list the network in ClientOptions.AllowedNetworks, if the call is meant to reach it"
	}
	return message
}

// addressRange is one special-purpose block and what to call an address in it.
type addressRange struct {
	prefix netip.Prefix
	reason string
}

// metadataRanges are where cloud providers serve instance metadata, which on
// most of them includes credentials for the instance's role. Reaching one is
// the usual point of a request forgery, so these stay refused when
// AllowPrivateNetworks is set and are reached only by naming them in
// AllowedNetworks.
//
// The whole of IPv4 link-local is here rather than the one well known
// address, because the providers keep adding services to it: AWS serves
// metadata at 169.254.169.254, task credentials at 169.254.170.2 and pod
// identity at 169.254.170.23, and nothing a service calls on purpose lives
// anywhere else in the block.
var metadataRanges = []addressRange{
	{netip.MustParsePrefix("169.254.0.0/16"), "an IPv4 link-local address, where cloud metadata services live"},
	{netip.MustParsePrefix("168.63.129.16/32"), "a cloud metadata service address (Azure)"},
	{netip.MustParsePrefix("100.100.100.200/32"), "a cloud metadata service address (Alibaba Cloud)"},
	{netip.MustParsePrefix("192.0.0.192/32"), "a cloud metadata service address (Oracle Cloud)"},
	{netip.MustParsePrefix("fd00:ec2::254/128"), "a cloud metadata service address (AWS)"},
	{netip.MustParsePrefix("fd00:ec2::23/128"), "a cloud metadata service address (AWS)"},
	{netip.MustParsePrefix("fd20:ce::254/128"), "a cloud metadata service address (Google Cloud)"},
}

// specialRanges are the blocks of the IANA special-purpose registries that do
// not reach the public internet, in the order they are checked: a narrower
// block that sits inside a wider one comes first, so that it is the one named.
var specialRanges = []addressRange{
	{netip.MustParsePrefix("0.0.0.0/8"), "an unspecified or this-network address, which reaches the local host"},
	{netip.MustParsePrefix("10.0.0.0/8"), "a private address"},
	{netip.MustParsePrefix("100.64.0.0/10"), "a carrier-grade NAT shared address"},
	{netip.MustParsePrefix("127.0.0.0/8"), "a loopback address"},
	{netip.MustParsePrefix("172.16.0.0/12"), "a private address"},
	{netip.MustParsePrefix("192.0.0.0/24"), "an IETF protocol assignment"},
	{netip.MustParsePrefix("192.0.2.0/24"), "a documentation address"},
	{netip.MustParsePrefix("192.88.99.0/24"), "a deprecated 6to4 relay address"},
	{netip.MustParsePrefix("192.168.0.0/16"), "a private address"},
	{netip.MustParsePrefix("198.18.0.0/15"), "a benchmarking address"},
	{netip.MustParsePrefix("198.51.100.0/24"), "a documentation address"},
	{netip.MustParsePrefix("203.0.113.0/24"), "a documentation address"},
	{netip.MustParsePrefix("224.0.0.0/4"), "a multicast address"},
	{netip.MustParsePrefix("255.255.255.255/32"), "the broadcast address"},
	{netip.MustParsePrefix("240.0.0.0/4"), "a reserved address"},

	{netip.MustParsePrefix("::/128"), "the unspecified address, which reaches the local host"},
	{netip.MustParsePrefix("::1/128"), "the loopback address"},
	{netip.MustParsePrefix("::/96"), "an IPv4-compatible address, a deprecated form"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "a local-use NAT64 address, which reaches whatever IPv4 address this network's own translator maps it to"},
	{netip.MustParsePrefix("100::/64"), "a discard-only address"},
	{netip.MustParsePrefix("2001::/32"), "a Teredo tunnel address"},
	{netip.MustParsePrefix("2001:2::/48"), "a benchmarking address"},
	{netip.MustParsePrefix("2001:10::/28"), "a deprecated ORCHID address"},
	{netip.MustParsePrefix("2001:db8::/32"), "a documentation address"},
	{netip.MustParsePrefix("3fff::/20"), "a documentation address"},
	{netip.MustParsePrefix("5f00::/16"), "a segment routing identifier"},
	{netip.MustParsePrefix("fc00::/7"), "a unique local (private) address"},
	{netip.MustParsePrefix("fe80::/10"), "a link-local address"},
	{netip.MustParsePrefix("fec0::/10"), "a deprecated site-local address"},
	{netip.MustParsePrefix("ff00::/8"), "a multicast address"},
}

// The IPv6 blocks that carry an IPv4 address inside them. An address in one
// of them is judged by the IPv4 address it reaches as well as by itself,
// because a translator or a tunnel delivers it there: 64:ff9b::7f00:1 is the
// loopback interface of whatever gateway translates it.
var (
	globalUnicast  = netip.MustParsePrefix("2000::/3")
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	nat64LocalUse  = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	ipv4Compatible = netip.MustParsePrefix("::/96")
)

// classifyAddress names the special-purpose range an address belongs to, and
// reports whether that range is a metadata service. An empty reason means the
// address is an ordinary public one.
//
// The address must already be unmapped and carry no zone. The cost is a scan
// of two fixed tables, so it does not depend on anything a caller sends.
func classifyAddress(addr netip.Addr) (reason string, metadata bool) {
	for _, r := range metadataRanges {
		if r.prefix.Contains(addr) {
			return r.reason, true
		}
	}
	for _, r := range specialRanges {
		if r.prefix.Contains(addr) {
			return r.reason, false
		}
	}
	if addr.Is6() && !globalUnicast.Contains(addr) && !nat64WellKnown.Contains(addr) {
		// Everything IANA has allocated for the public internet sits in
		// 2000::/3. Listing the rest one block at a time would leave whatever
		// is assigned next reachable until someone noticed.
		return "an address outside the IPv6 global unicast range", false
	}
	return "", false
}

// embeddedIPv4 returns the IPv4 address an IPv6 address carries, for the forms
// that deliver to it at a place the form fixes: NAT64 under its well known
// prefix, 6to4, and the deprecated IPv4-compatible form. The second result
// names the form. The local-use NAT64 block has no one place; see
// [localUseNAT64].
//
// Teredo carries an IPv4 address too, but its block is refused outright: the
// protocol is retired, and judging the address it hides would mean undoing
// the obfuscation it applies for no caller that still uses it.
func embeddedIPv4(addr netip.Addr) (netip.Addr, string, bool) {
	if !addr.Is6() {
		return netip.Addr{}, "", false
	}
	b := addr.As16()
	switch {
	case nat64WellKnown.Contains(addr):
		return netip.AddrFrom4([4]byte(b[12:16])), "a NAT64 address", true
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte(b[2:6])), "a 6to4 address", true
	case ipv4Compatible.Contains(addr) && addr != netip.IPv6Unspecified() && addr != netip.IPv6Loopback():
		return netip.AddrFrom4([4]byte(b[12:16])), "an IPv4-compatible address", true
	}
	return netip.Addr{}, "", false
}

// nat64LocalUseLayouts are the places an IPv4 address can sit in an address of
// 64:ff9b:1::/48, the block RFC 8215 sets aside for a network's own
// translator: one for each prefix length RFC 6052 lets the network choose
// inside the block (48, 56, 64 and 96 bits), skipping the octet at bits 64 to
// 71. Which one a translator uses is its own configuration, which the client
// cannot see, so an address there is read every way it could be meant.
var nat64LocalUseLayouts = [...][4]uint8{
	{6, 7, 9, 10},
	{7, 9, 10, 11},
	{9, 10, 11, 12},
	{12, 13, 14, 15},
}

// localUseNAT64 returns every IPv4 address an address in 64:ff9b:1::/48 may
// be translated to, one for each of [nat64LocalUseLayouts], and false for an
// address outside the block.
//
// Reading only the /48 layout let 64:ff9b:1:abcd::a9fe:a9fe, which a
// translator with a /96 prefix delivers to 169.254.169.254, through as the
// public 171.205.0.0. The block is refused as a private range is (see
// [specialRanges]), and these readings are what keeps a metadata service and
// a denied network refused through it when private ranges are allowed.
func localUseNAT64(addr netip.Addr) (inner [len(nat64LocalUseLayouts)]netip.Addr, ok bool) {
	if !nat64LocalUse.Contains(addr) {
		return inner, false
	}
	b := addr.As16()
	for i, at := range nat64LocalUseLayouts {
		inner[i] = netip.AddrFrom4([4]byte{b[at[0]], b[at[1]], b[at[2]], b[at[3]]})
	}
	return inner, true
}

// addressPolicy is what a [Client] may connect to: the special-purpose ranges
// refused unless private networks are allowed, metadata refused unless named,
// and the caller's own lists on top.
type addressPolicy struct {
	allowPrivate bool
	allowed      []netip.Prefix
	denied       []netip.Prefix
}

// newAddressPolicy builds the policy a set of options describes. A prefix that
// is not valid panics, because a deny list with a hole in it fails open and an
// allow list with one fails in a way nobody would think to look for.
func newAddressPolicy(opts ClientOptions) *addressPolicy {
	policy := &addressPolicy{allowPrivate: opts.AllowPrivateNetworks}
	var problems []string
	policy.allowed, problems = normalizePrefixes("AllowedNetworks", opts.AllowedNetworks, problems)
	policy.denied, problems = normalizePrefixes("DeniedNetworks", opts.DeniedNetworks, problems)
	if len(problems) > 0 {
		panic("muzak: " + strings.Join(problems, "; ") +
			"; build each prefix with netip.ParsePrefix and check its error, since a list with a hole in it does not mean what it says")
	}
	return policy
}

// normalizePrefixes checks and normalizes a configured list. A prefix written
// in the IPv4-in-IPv6 form becomes the IPv4 prefix it wraps, since every
// address is unmapped before it is compared, and host bits are masked off so
// that "10.1.2.3/8" means "10.0.0.0/8".
func normalizePrefixes(field string, prefixes []netip.Prefix, problems []string) ([]netip.Prefix, []string) {
	normalized := make([]netip.Prefix, 0, len(prefixes))
	for i, prefix := range prefixes {
		if !prefix.IsValid() {
			problems = append(problems, fmt.Sprintf("ClientOptions.%s[%d] is not a valid prefix", field, i))
			continue
		}
		if addr := prefix.Addr(); addr.Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96)
		}
		normalized = append(normalized, netip.PrefixFrom(prefix.Addr().WithZone(""), prefix.Bits()).Masked())
	}
	return normalized, problems
}

// listed reports whether any of a list of prefixes contains an address.
func listed(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// listedAny reports whether any of a list of prefixes contains any of addrs.
func listedAny(prefixes []netip.Prefix, addrs []netip.Addr) bool {
	for _, addr := range addrs {
		if listed(prefixes, addr) {
			return true
		}
	}
	return false
}

// check returns an [*AddressRefusedError] for an address the policy refuses,
// naming host as the caller wrote it, and nil otherwise.
func (p *addressPolicy) check(host string, addr netip.Addr) error {
	if refused := p.refusal(host, addr); refused != nil {
		return refused
	}
	return nil
}

// refusal is check with the refusal's own type, for the callers that build on
// it. It is a separate function so that check never returns a nil pointer
// dressed as a non-nil error.
//
// The order is what makes the lists mean what they say: DeniedNetworks
// refuses whatever else would allow it, AllowedNetworks allows whatever else
// would refuse it, a metadata service stays refused when private networks are
// allowed, and only then do the special-purpose ranges apply. An address that
// carries an IPv4 address inside it is judged by both, so that a NAT64 or 6to4
// spelling of a refused address is refused with it. A local-use NAT64 address,
// whose IPv4 address could sit in any of several places, is denied or refused
// as a metadata service when any reading of it would be, and allowed through
// AllowedNetworks only by its own IPv6 address, since a reading an allowed
// network contains may not be the one the translator uses.
func (p *addressPolicy) refusal(host string, addr netip.Addr) *AddressRefusedError {
	normal := addr.WithZone("").Unmap()
	inner, form, embeds := embeddedIPv4(normal)
	local, localUse := localUseNAT64(normal)
	if listed(p.denied, normal) || (embeds && listed(p.denied, inner)) || (localUse && listedAny(p.denied, local[:])) {
		return &AddressRefusedError{Host: host, Addr: normal, Reason: "listed in ClientOptions.DeniedNetworks", kind: refusedDenied}
	}
	if listed(p.allowed, normal) || (embeds && listed(p.allowed, inner)) {
		return nil
	}
	reason, metadata := classifyAddress(normal)
	if embeds {
		if innerReason, innerMetadata := classifyAddress(inner); innerReason != "" {
			reason = form + " reaching " + inner.String() + ", which is " + innerReason
			metadata = metadata || innerMetadata
		}
	}
	if localUse {
		// The block's own reason stands unless a reading of it reaches a
		// metadata service, which AllowPrivateNetworks does not open.
		for _, candidate := range local {
			if innerReason, innerMetadata := classifyAddress(candidate); innerMetadata {
				reason = "a local-use NAT64 address that may reach " + candidate.String() + ", which is " + innerReason
				metadata = true
				break
			}
		}
	}
	switch {
	case reason == "":
		return nil
	case metadata:
		return &AddressRefusedError{Host: host, Addr: normal, Reason: reason, kind: refusedMetadata}
	case p.allowPrivate:
		return nil
	}
	return &AddressRefusedError{Host: host, Addr: normal, Reason: reason, kind: refusedClass}
}

// checkHost judges a host as a URL names it, before anything is resolved.
//
// An address written literally is judged as the address it is. A name is left
// for the dialer to judge once it has been resolved, because only the address
// actually connected to is worth checking, with three exceptions that are
// refused here:
//
//   - a name ending in a number that is not a canonical address, such as
//     "127.1", "0x7f.0.0.1" or "2130706433". Browsers, curl and the C
//     resolver read those as IPv4 addresses and Go reads them as names, and a
//     check and a fetch that disagree about what a host is are how a request
//     forgery filter is usually got around;
//   - localhost and the names under it, which RFC 6761 reserves for the
//     loopback interface, refused unless loopback is allowed;
//   - a name outside ASCII when the request goes through a proxy, because the
//     transport maps it before the proxy sees it, so 127.0.0.1 written in
//     fullwidth digits would arrive at the proxy as 127.0.0.1 after this
//     check had read it as a name.
func (p *addressPolicy) checkHost(host string, proxied bool) error {
	if addr, err := netip.ParseAddr(host); err == nil {
		return p.check(host, addr)
	}
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if endsInNumber(name) {
		addr, ok := parseLegacyIPv4(name)
		if !ok {
			return &AddressRefusedError{Host: host, Reason: "a name ending in a number that is not an address", kind: refusedAmbiguous}
		}
		reason := "a non-canonical spelling of " + addr.String()
		if refused := p.refusal(host, addr); refused != nil {
			reason = refused.Reason + ", written in a non-canonical form"
		}
		return &AddressRefusedError{Host: host, Addr: addr, Reason: reason, kind: refusedAmbiguous}
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		if refused := p.refusal(host, netip.AddrFrom4([4]byte{127, 0, 0, 1})); refused != nil {
			refused.Addr, refused.Reason = netip.Addr{}, "a localhost name, reserved for the loopback interface"
			return refused
		}
		return nil
	}
	if proxied && !isASCII(host) {
		return &AddressRefusedError{Host: host,
			Reason: "a name outside ASCII, which the proxy would be asked for in a form this client cannot check; write it in its xn-- form",
			kind:   refusedDenied}
	}
	return nil
}

// isASCII reports whether a string holds only ASCII bytes.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// endsInNumber reports whether a lowercased host's last label is a number, in
// decimal or in 0x hexadecimal, which is the test the WHATWG URL standard uses
// to decide that a host is meant as an IPv4 address. No top-level domain is
// numeric, so a name that ends in one is an address or a mistake.
func endsInNumber(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last == "" {
		return false
	}
	if hex, ok := strings.CutPrefix(last, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return strings.Trim(last, "0123456789") == ""
}

// parseLegacyIPv4 reads a host the way inet_aton and the WHATWG URL standard
// do: one to four parts, each decimal, octal with a leading zero or
// hexadecimal with 0x, the last filling every byte the others did not. It
// exists to say what a non-canonical host means, never to connect to it.
//
// The cost is linear in the length of the host, and nothing is allocated.
func parseLegacyIPv4(host string) (netip.Addr, bool) {
	var parts [4]uint64
	count := 0
	for rest := host; ; {
		part, after, more := strings.Cut(rest, ".")
		if count == len(parts) {
			return netip.Addr{}, false
		}
		value, ok := parseLegacyIPv4Part(part)
		if !ok {
			return netip.Addr{}, false
		}
		parts[count] = value
		count++
		if !more {
			break
		}
		rest = after
	}
	var value uint64
	for i := range count - 1 {
		if parts[i] > 0xff {
			return netip.Addr{}, false
		}
		value |= parts[i] << (8 * (3 - i))
	}
	last := parts[count-1]
	if last >= 1<<(8*(5-count)) {
		return netip.Addr{}, false
	}
	value |= last
	// The checks above leave value below 1<<32, so its low four bytes are the
	// whole address.
	var bytes [8]byte
	binary.BigEndian.PutUint64(bytes[:], value)
	return netip.AddrFrom4([4]byte(bytes[4:])), true
}

// parseLegacyIPv4Part reads one part of a legacy IPv4 address. "0x" alone is
// zero, as it is to the URL standard.
func parseLegacyIPv4Part(part string) (uint64, bool) {
	base := 10
	switch {
	case part == "":
		return 0, false
	case strings.HasPrefix(part, "0x"):
		base, part = 16, part[2:]
		if part == "" {
			return 0, true
		}
	case len(part) > 1 && part[0] == '0':
		base, part = 8, part[1:]
	}
	value, err := strconv.ParseUint(part, base, 64)
	return value, err == nil
}

// maxDialAddresses bounds how many of the addresses a name resolves to are
// tried. A resolver answer is written by whoever runs the zone, and the
// dialer would otherwise try every one of them before giving up.
const maxDialAddresses = 16

// minDialShare is the least time one address is given when the dial timeout
// is split between several, which is what net.Dialer gives one too: less
// than this and a slow but working address is abandoned for no reason.
const minDialShare = 2 * time.Second

// clientDialer opens connections for a [Client], applying its address policy
// to every address it is about to connect to. It is the one place a
// connection is opened, so a name that resolves somewhere it should not, a
// name that resolves somewhere different the second time it is asked, and a
// redirect to either, all meet the same check.
type clientDialer struct {
	policy  *addressPolicy
	timeout time.Duration
	// proxyAddr is the host:port of the configured proxy, which is connected
	// to without the policy: it is the caller's own egress, configured in
	// code, and is trusted to apply its own.
	proxyAddr string

	// lookup resolves a name, connect opens a connection to an address
	// the policy has passed, and connectProxy opens one to the proxy. They are
	// fields so that a test can resolve and connect somewhere else.
	lookup       func(ctx context.Context, host string) ([]netip.Addr, error)
	connect      func(ctx context.Context, network, address string) (net.Conn, error)
	connectProxy func(ctx context.Context, network, address string) (net.Conn, error)
}

// newClientDialer builds the dialer a client uses in production. The
// connection itself is opened by a net.Dialer whose Control checks the address
// once more, on the socket, immediately before connecting, so that a change
// to how the address list is assembled cannot open a way around the policy.
func newClientDialer(policy *addressPolicy, timeout time.Duration, proxyAddr string) *clientDialer {
	checked := &net.Dialer{KeepAlive: 30 * time.Second, Control: policy.control}
	trusted := &net.Dialer{KeepAlive: 30 * time.Second}
	return &clientDialer{
		policy:    policy,
		timeout:   timeout,
		proxyAddr: proxyAddr,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		connect:      checked.DialContext,
		connectProxy: trusted.DialContext,
	}
}

// control is the net.Dialer hook that runs on the socket just before it
// connects, with the numeric address it is connecting to.
func (p *addressPolicy) control(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		// net.Dialer hands Control the numeric ip:port it is about to
		// connect to, so this is reached only if that ever changes, and then
		// it fails closed rather than open.
		return &AddressRefusedError{Host: address, Reason: "an address that could not be read", kind: refusedDenied}
	}
	return p.check(addrPort.Addr().String(), addrPort.Addr())
}

// DialContext resolves an address, refuses whatever the policy refuses and
// connects to the first of the rest that answers.
//
// Every address a name resolves to is judged, and only the ones that pass are
// tried, in the order the resolver gave them. A name whose every address is
// refused reports the first refusal. The whole of it, the lookup included, is
// bounded by the dial timeout, shared between the addresses the way net.Dialer
// shares it, so one address that never answers cannot use all of it.
func (d *clientDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.timeout)
		defer cancel()
	}
	if d.proxyAddr != "" && address == d.proxyAddr {
		return d.connectProxy(ctx, network, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	candidates, err := d.resolve(ctx, network, host)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	var (
		allowed []netip.Addr
		refusal error
	)
	for _, addr := range candidates {
		if err := d.policy.check(host, addr); err != nil {
			if refusal == nil {
				refusal = err
			}
			continue
		}
		if len(allowed) < maxDialAddresses {
			allowed = append(allowed, addr)
		}
	}
	if len(allowed) == 0 {
		if refusal == nil {
			refusal = fmt.Errorf("muzak: %q resolved to no addresses the client can use over %s", clientShorten(host), network)
		}
		return nil, &net.OpError{Op: "dial", Net: network, Err: refusal}
	}
	var first error
	for i, addr := range allowed {
		conn, err := d.connectOne(ctx, network, net.JoinHostPort(addr.String(), port), len(allowed)-i)
		if err == nil {
			return conn, nil
		}
		if first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, first
}

// connectOne connects to one address, giving it its share of what is left of
// the dial timeout when others are still waiting behind it.
func (d *clientDialer) connectOne(ctx context.Context, network, address string, left int) (net.Conn, error) {
	if deadline, ok := ctx.Deadline(); ok && left > 1 {
		remaining := time.Until(deadline)
		share := max(remaining/time.Duration(left), min(minDialShare, remaining))
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, share)
		defer cancel()
	}
	return d.connect(ctx, network, address)
}

// resolve returns the addresses a host stands for on a network: itself when it
// is written as an address, and what the resolver answers otherwise, narrowed
// to the family the network asks for.
func (d *clientDialer) resolve(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	if err := d.policy.checkHost(host, false); err != nil {
		return nil, err
	}
	addrs, err := d.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	family := addrs[:0:0]
	for _, addr := range addrs {
		addr = addr.Unmap()
		if (network == "tcp4" && !addr.Is4()) || (network == "tcp6" && !addr.Is6()) {
			continue
		}
		family = append(family, addr)
	}
	return family, nil
}

// clientShorten bounds a value on its way into an error message, so that a
// host the size of a URL limit does not become a log line that size.
func clientShorten(value string) string {
	const most = 96
	if len(value) <= most {
		return value
	}
	return value[:most] + "..."
}
