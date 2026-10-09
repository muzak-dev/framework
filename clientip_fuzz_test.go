package muzak

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// FuzzClientIP feeds arbitrary peers and forwarding headers to the resolver
// and holds it to the properties that make its answer safe to key a limit on:
// it is always in normal form, it ignores the header unless the peer is
// trusted, and writing the same client as an IPv4-mapped address or with an
// IPv6 zone cannot change who it is taken to be.
func FuzzClientIP(f *testing.F) {
	for _, seed := range []struct{ peer, header string }{
		{"10.1.2.3:4567", "203.0.113.9"},
		{"10.1.2.3:4567", "203.0.113.9, 10.9.9.9"},
		{"[::ffff:10.1.2.3]:80", "::ffff:203.0.113.9"},
		{"[fe80::1%eth0]:80", "fe80::2%eth1, 2001:db8::1%a"},
		{"[fe80::1%eth0]:80", "2001:db8::1%a, 2001:db8::1%b"},
		{"172.16.5.5:1", "[2001:db8::1]:8080, ::ffff:172.16.9.9"},
		{"192.0.2.1", "not-an-address"},
		{"", "203.0.113.9"},
		{"@", "203.0.113.9"},
		{"[::ffff:192.0.2.1%z]:1", ",,,"},
		{"10.0.0.1:1", strings.Repeat("10.0.0.2, ", 80) + "203.0.113.9"},
	} {
		f.Add(seed.peer, seed.header)
	}

	trusting, err := newClientIPResolver(ClientIPOptions{
		TrustedProxies: []string{"10.0.0.0/8", "fd00::/8", "::ffff:172.16.0.0/108", "192.0.2.1", "fe80::1%eth0"},
	})
	if err != nil {
		f.Fatal(err)
	}
	trustsNothing, err := newClientIPResolver(ClientIPOptions{})
	if err != nil {
		f.Fatal(err)
	}

	normal := func(t *testing.T, what string, addr netip.Addr) {
		t.Helper()
		if addr.IsValid() && (addr.Zone() != "" || addr.Is4In6()) {
			t.Fatalf("%s = %q, want an address with no zone and not IPv4-mapped", what, addr)
		}
	}

	f.Fuzz(func(t *testing.T, peer, header string) {
		req := forwardedRequest(peer, DefaultForwardedHeader, header)
		peerAddr, peerOK := peerAddr(peer)

		got := trusting.resolve(req)
		normal(t, "trusting resolver", got)
		if !peerOK {
			if got.IsValid() {
				t.Fatalf("resolved %q for an unparseable peer %q", got, peer)
			}
			return
		}
		if !trusting.trusts(peerAddr) && got != peerAddr {
			t.Fatalf("an untrusted peer %q was replaced by %q", peer, got)
		}

		// With nothing trusted the header is not read at all.
		plain := trustsNothing.resolve(req)
		normal(t, "default resolver", plain)
		if plain != peerAddr {
			t.Fatalf("the default resolver returned %q for peer %q, want the peer", plain, peerAddr)
		}

		// The same client, written another way, is the same client.
		if peerAddr.Is4() {
			mapped := "[::ffff:" + peerAddr.String() + "]:1"
			if other := trusting.resolve(forwardedRequest(mapped, DefaultForwardedHeader, header)); other != trusting.resolve(forwardedRequest(peerAddr.String()+":1", DefaultForwardedHeader, header)) {
				t.Fatalf("peer %s and its mapped form resolved to different clients (%q)", peerAddr, other)
			}
		} else {
			zoned := "[" + peerAddr.String() + "%zone]:1"
			if a, b := trusting.resolve(forwardedRequest(zoned, DefaultForwardedHeader, header)), trusting.resolve(forwardedRequest("["+peerAddr.String()+"]:1", DefaultForwardedHeader, header)); a != b {
				t.Fatalf("peer %s with and without a zone resolved to %q and %q", peerAddr, a, b)
			}
		}

		// Whatever it answers, it has a stable key.
		if got.IsValid() {
			if key := trusting.connectionKey(got); key == "" || strings.Contains(key, "%") {
				t.Fatalf("connection key %q for %q", key, got)
			}
			if id := clientIdentity(got, DefaultConnectionIPv6Prefix); id == "" || strings.Contains(id, "%") {
				t.Fatalf("identity %q for %q", id, got)
			}
		}
	})
}

// FuzzForwardedHeader feeds arbitrary RFC 7239 Forwarded values to a resolver
// that reads that header, standing in for whatever a client sent, and holds it
// to what makes the header safe to believe: the answer is in normal form, an
// untrusted peer is never replaced, and the entry a trusted proxy adds is the
// one believed however the client's part is written. That holds whether the
// proxy appends its entry to the client's line, adds a line of its own, or
// quotes the client's text, escaped, into a parameter of its entry, which is
// where a parser that split on every comma or semicolon would be talked into
// a client's for.
func FuzzForwardedHeader(f *testing.F) {
	for _, seed := range []string{
		"for=203.0.113.9;proto=https",
		`for="[2001:db8::1]:443"`,
		`For="203.0.113.9:80", for=10.0.0.2`,
		`for=unknown, for="_hidden"`,
		`for="6.6.6.6`,
		`for=6.6.6.6;host="a, for=7.7.7.7;x="`,
		`x="\", for=6.6.6.6`,
		`for="\\\"", for=6.6.6.6\`,
		";;;,,,\"\"\"",
		strings.Repeat("for=10.0.0.2, ", 80) + "for=203.0.113.9",
	} {
		f.Add(seed)
	}

	resolver, err := newClientIPResolver(ClientIPOptions{TrustedProxies: []string{"10.0.0.0/8", "fd00::/8"}, Header: "Forwarded"})
	if err != nil {
		f.Fatal(err)
	}
	const proxy, untrusted = "10.0.0.1:443", "203.0.113.50:443"
	client := netip.MustParseAddr("198.51.100.77")
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`)

	f.Fuzz(func(t *testing.T, sent string) {
		got := resolver.resolve(forwardedRequest(proxy, "Forwarded", sent))
		if !got.IsValid() || got.Zone() != "" || got.Is4In6() {
			t.Fatalf("resolve(%q) = %q, want a valid address with no zone and not IPv4-mapped", sent, got)
		}
		if other := resolver.resolve(forwardedRequest(untrusted, "Forwarded", sent)); other.String() != "203.0.113.50" {
			t.Fatalf("an untrusted peer was replaced by %q from %q", other, sent)
		}

		for _, req := range []*http.Request{
			forwardedRequest(proxy, "Forwarded", sent+", for="+client.String()),
			forwardedRequest(proxy, "Forwarded", sent, "for="+client.String()),
			forwardedRequest(proxy, "Forwarded", sent+`, for="`+client.String()+`:4711";host="`+escape.Replace(sent)+`"`),
			forwardedRequest(proxy, "Forwarded", sent, `proto=https;host="`+escape.Replace(sent)+`";for=`+client.String()),
		} {
			if got := resolver.resolve(req); got != client {
				t.Fatalf("Forwarded %q resolved to %q, want the client the trusted proxy reported, %s",
					req.Header.Values("Forwarded"), got, client)
			}
		}
	})
}
