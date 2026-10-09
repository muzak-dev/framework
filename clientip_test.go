package muzak

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// forwardedRequest builds a request that arrived from peer carrying the given
// header values, which is the shape every resolution test needs.
func forwardedRequest(peer, header string, values ...string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer
	req.Header.Del(header)
	for _, value := range values {
		req.Header.Add(header, value)
	}
	return req
}

func TestClientIPResolverTrustsNothingByDefault(t *testing.T) {
	t.Parallel()
	resolver, err := newClientIPResolver(ClientIPOptions{})
	if err != nil {
		t.Fatalf("newClientIPResolver() = %v", err)
	}
	req := forwardedRequest("203.0.113.7:44321", DefaultForwardedHeader, "198.51.100.9")
	if got := resolver.resolve(req).String(); got != "203.0.113.7" {
		t.Errorf("resolve() = %q, want the peer address; a forwarding header must not be believed by default", got)
	}
}

func TestClientIPResolverWalksTheChain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		trusted []string
		peer    string
		values  []string
		want    string
	}{
		{
			name:    "one trusted proxy reports the client",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:    "trusted hops are stepped over",
			trusted: []string{"10.0.0.0/8", "172.16.0.0/12"},
			peer:    "10.1.2.3:9000",
			values:  []string{"198.51.100.9, 172.16.4.5"},
			want:    "198.51.100.9",
		},
		{
			name:    "a claimed address to the left of an untrusted hop is ignored",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"1.1.1.1, 203.0.113.4"},
			want:    "203.0.113.4",
		},
		{
			name:    "a chain of only trusted hops yields the leftmost",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"10.9.9.9, 10.8.8.8"},
			want:    "10.9.9.9",
		},
		{
			name:    "an absent header leaves the peer",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			want:    "10.1.2.3",
		},
		{
			name:    "an empty header value leaves the peer",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{""},
			want:    "10.1.2.3",
		},
		{
			name:    "an unreadable entry stops the walk",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"198.51.100.9, _hidden"},
			want:    "10.1.2.3",
		},
		{
			name:    "an unreadable entry stops the walk at the nearest trusted hop",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"198.51.100.9, unknown, 10.4.4.4"},
			want:    "10.4.4.4",
		},
		{
			name:    "an entry carrying a port is understood",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"198.51.100.9:51234"},
			want:    "198.51.100.9",
		},
		{
			name:    "an IPv6 entry carrying a port is understood",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"[2001:db8::1]:51234"},
			want:    "2001:db8::1",
		},
		{
			name:    "an IPv4-mapped entry is normalised",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"::ffff:198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:    "several header fields are read in order",
			trusted: []string{"10.0.0.0/8"},
			peer:    "10.1.2.3:9000",
			values:  []string{"198.51.100.9", "10.7.7.7"},
			want:    "198.51.100.9",
		},
		{
			name:    "a bare address is trusted exactly",
			trusted: []string{"10.1.2.3"},
			peer:    "10.1.2.3:9000",
			values:  []string{"198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:    "a neighbouring address is not trusted",
			trusted: []string{"10.1.2.3"},
			peer:    "10.1.2.4:9000",
			values:  []string{"198.51.100.9"},
			want:    "10.1.2.4",
		},
		{
			name:    "an IPv6 peer is matched by its prefix",
			trusted: []string{"2001:db8::/32"},
			peer:    "[2001:db8::5]:9000",
			values:  []string{"198.51.100.9"},
			want:    "198.51.100.9",
		},
		{
			name:    "a prefix written with host bits still matches",
			trusted: []string{"10.1.2.3/8"},
			peer:    "10.9.9.9:9000",
			values:  []string{"198.51.100.9"},
			want:    "198.51.100.9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolver, err := newClientIPResolver(ClientIPOptions{TrustedProxies: tc.trusted})
			if err != nil {
				t.Fatalf("newClientIPResolver() = %v", err)
			}
			req := forwardedRequest(tc.peer, DefaultForwardedHeader, tc.values...)
			if got := resolver.resolve(req).String(); got != tc.want {
				t.Errorf("resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIPResolverBoundsTheChain(t *testing.T) {
	t.Parallel()
	resolver, err := newClientIPResolver(ClientIPOptions{TrustedProxies: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatalf("newClientIPResolver() = %v", err)
	}
	// Every hop is trusted, so without a bound the walk would read all of
	// them and report the leftmost, which is exactly what a client flooding
	// the header would be asking for.
	hops := make([]string, maxForwardedHops+10)
	for i := range hops {
		hops[i] = "10.0.0.1"
	}
	hops[0] = "198.51.100.9"
	req := forwardedRequest("10.1.2.3:9000", DefaultForwardedHeader, strings.Join(hops, ", "))
	if got := resolver.resolve(req).String(); got != "10.0.0.1" {
		t.Errorf("resolve() = %q, want the walk to stop at the hop bound rather than reach the leftmost entry", got)
	}
}

func TestClientIPResolverUsesTheConfiguredHeader(t *testing.T) {
	t.Parallel()
	resolver, err := newClientIPResolver(ClientIPOptions{
		TrustedProxies: []string{"10.0.0.0/8"},
		Header:         "CF-Connecting-IP",
	})
	if err != nil {
		t.Fatalf("newClientIPResolver() = %v", err)
	}
	req := forwardedRequest("10.1.2.3:9000", "CF-Connecting-IP", "198.51.100.9")
	req.Header.Set(DefaultForwardedHeader, "1.1.1.1")
	if got := resolver.resolve(req).String(); got != "198.51.100.9" {
		t.Errorf("resolve() = %q, want the configured header to be the one read", got)
	}
}

// TestClientIPResolverReadsTheForwardedHeader is the regression test for the
// RFC 7239 Forwarded header, which ClientIPOptions.Header accepted and the
// resolver could not read. Each of its entries is a list of parameters, and
// "for=203.0.113.9;proto=https" is not an address, so the walk stopped at its
// first entry and every client behind the proxy resolved to the proxy itself:
// one IPTracker budget and one per-client connection allowance for all of
// them. The for parameter is now read out of each entry, with the same
// right-to-left walk and the same trust as any other forwarding header.
func TestClientIPResolverReadsTheForwardedHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		values []string
		want   string
	}{
		{"a bare address", []string{"for=203.0.113.9;proto=https"}, "203.0.113.9"},
		{"a parameter name in any case", []string{"proto=https;For=203.0.113.9;by=10.0.0.1"}, "203.0.113.9"},
		{"a quoted IPv4 address with a port", []string{`FOR="203.0.113.9:8080"`}, "203.0.113.9"},
		{"a bracketed IPv6 address with a port", []string{`for="[2001:db8::1]:443"`}, "2001:db8::1"},
		{"a bracketed IPv6 address", []string{`for="[2001:db8::1]"`}, "2001:db8::1"},
		{"an obfuscated port", []string{`for="[2001:db8::1]:_p1"`}, "2001:db8::1"},
		{"an IPv6 address without the brackets the RFC asks for", []string{`for="2001:db8::1"`}, "2001:db8::1"},
		{"a quoted-pair in the value", []string{`for="203.0.113.\9"`}, "203.0.113.9"},
		{"spaces around the parameters", []string{" for=203.0.113.9 ; proto=https "}, "203.0.113.9"},
		{"trusted hops are stepped over", []string{"for=198.51.100.1, for=10.4.4.4;by=10.0.0.1"}, "198.51.100.1"},
		{"several header fields are read in order", []string{"for=198.51.100.1", "for=10.7.7.7"}, "198.51.100.1"},
		{"a claimed address to the left of an untrusted hop is ignored", []string{"for=1.1.1.1, for=203.0.113.4"}, "203.0.113.4"},
		{"unknown stops the walk", []string{"for=unknown"}, "10.1.2.3"},
		{"unknown stops the walk at the nearest trusted hop", []string{"for=198.51.100.1, for=unknown, for=10.4.4.4"}, "10.4.4.4"},
		{"an obfuscated node stops the walk", []string{`for=198.51.100.1, for="_gazonk", for=10.4.4.4`}, "10.4.4.4"},
		{"an entry with no for stops the walk", []string{"for=198.51.100.1, proto=https"}, "10.1.2.3"},
		{"a for repeated in one entry stops the walk", []string{"for=198.51.100.1;for=10.4.4.4"}, "10.1.2.3"},
		{"an unterminated quote stops the walk", []string{`for="198.51.100.1`}, "10.1.2.3"},
		{"a quote inside a bare value stops the walk", []string{`for=198.51"100.1"`}, "10.1.2.3"},
		{"a stray quote inside a quoted value stops the walk", []string{`for="198.51"100.1"`}, "10.1.2.3"},
		{"a value of two quoted strings stops the walk", []string{`for="198.51"."100.1"`}, "10.1.2.3"},
		{"a dangling escape stops the walk", []string{`for="198.51.100.1\`}, "10.1.2.3"},
		{"an empty entry stops the walk", []string{"for=198.51.100.1,"}, "10.1.2.3"},
		{"an empty header value leaves the peer", []string{""}, "10.1.2.3"},
		// A proxy that copies something the client chose, its Host header
		// say, into a quoted parameter of its own entry must not let a comma
		// or a semicolon in it start an entry or a parameter of the client's.
		{"a separator inside a quoted parameter is data", []string{`for=198.51.100.7;host="a, for=6.6.6.6;x="`}, "198.51.100.7"},
		{"an escaped quote inside a quoted parameter is data", []string{`for=198.51.100.7;host="a\", for=6.6.6.6;x=\\"`}, "198.51.100.7"},
		// A client that leaves a quote open in the header it sends cannot
		// swallow the entry the proxy appends to it.
		{"an open quote from the client does not reach the proxy's entry", []string{`for="6.6.6.6, for=198.51.100.7`}, "198.51.100.7"},
	}
	resolver, err := newClientIPResolver(ClientIPOptions{TrustedProxies: []string{"10.0.0.0/8"}, Header: "forwarded"})
	if err != nil {
		t.Fatalf("newClientIPResolver() = %v", err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := forwardedRequest("10.1.2.3:9000", "Forwarded", tc.values...)
			if got := resolver.resolve(req).String(); got != tc.want {
				t.Errorf("resolve() with Forwarded %q = %q, want %q", tc.values, got, tc.want)
			}
		})
	}

	// The header is read only from a trusted peer, as any other is.
	req := forwardedRequest("203.0.113.50:9000", "Forwarded", "for=198.51.100.1")
	if got := resolver.resolve(req).String(); got != "203.0.113.50" {
		t.Errorf("resolve() from an untrusted peer = %q, want the peer", got)
	}

	// The walk is bounded by entries, as it is for X-Forwarded-For.
	hops := make([]string, maxForwardedHops+10)
	for i := range hops {
		hops[i] = "for=10.0.0.1"
	}
	hops[0] = "for=198.51.100.9"
	req = forwardedRequest("10.1.2.3:9000", "Forwarded", strings.Join(hops, ", "))
	if got := resolver.resolve(req).String(); got != "10.0.0.1" {
		t.Errorf("resolve() = %q, want the walk to stop at the hop bound", got)
	}
}

func TestClientIPResolverRejectsAnUnreadablePolicy(t *testing.T) {
	t.Parallel()
	resolver, err := newClientIPResolver(ClientIPOptions{
		TrustedProxies: []string{"10.0.0.0/8", "not-an-address", "10.0.0.0/99"},
	})
	if err == nil {
		t.Fatal("newClientIPResolver() = nil error, want one naming the entries it could not parse")
	}
	for _, want := range []string{"not-an-address", "10.0.0.0/99"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	// A policy that could not be parsed is not half-applied.
	req := forwardedRequest("10.1.2.3:9000", DefaultForwardedHeader, "198.51.100.9")
	if got := resolver.resolve(req).String(); got != "10.1.2.3" {
		t.Errorf("resolve() = %q, want the peer address for a policy that was refused", got)
	}
}

func TestPeerAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		remote string
		want   string
		ok     bool
	}{
		{remote: "192.0.2.1:1234", want: "192.0.2.1", ok: true},
		{remote: "[2001:db8::1]:1234", want: "2001:db8::1", ok: true},
		{remote: "192.0.2.1", want: "192.0.2.1", ok: true},
		{remote: "2001:db8::1", want: "2001:db8::1", ok: true},
		{remote: "[::ffff:192.0.2.1]:80", want: "192.0.2.1", ok: true},
		{remote: "[fe80::1%eth0]:80", want: "fe80::1", ok: true},
		{remote: "fe80::1%eth0", want: "fe80::1", ok: true},
		{remote: ""},
		{remote: "/var/run/muzak.sock"},
		{remote: "pipe"},
	}
	for _, tc := range cases {
		t.Run(tc.remote, func(t *testing.T) {
			t.Parallel()
			addr, ok := peerAddr(tc.remote)
			if ok != tc.ok {
				t.Fatalf("peerAddr(%q) ok = %v, want %v", tc.remote, ok, tc.ok)
			}
			if ok && addr.String() != tc.want {
				t.Errorf("peerAddr(%q) = %q, want %q", tc.remote, addr, tc.want)
			}
		})
	}
}

func TestParseForwardedAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		field string
		want  string
		ok    bool
	}{
		{field: "198.51.100.9", want: "198.51.100.9", ok: true},
		{field: "2001:db8::1", want: "2001:db8::1", ok: true},
		{field: "[2001:db8::1]:9", want: "2001:db8::1", ok: true},
		{field: "2001:db8::1%eth0", want: "2001:db8::1", ok: true},
		{field: "[2001:db8::1%eth0]:9", want: "2001:db8::1", ok: true},
		{field: "198.51.100.9:9", want: "198.51.100.9", ok: true},
		{field: ""},
		{field: "unknown"},
		{field: "_obfuscated"},
		{field: "198.51.100.9:notaport:9"},
		{field: "[nonsense]:9"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			addr, ok := parseForwardedAddr(tc.field)
			if ok != tc.ok {
				t.Fatalf("parseForwardedAddr(%q) ok = %v, want %v", tc.field, ok, tc.ok)
			}
			if ok && addr.String() != tc.want {
				t.Errorf("parseForwardedAddr(%q) = %q, want %q", tc.field, addr, tc.want)
			}
		})
	}
}

func TestUnquoteForwardedValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value, want string
		ok          bool
	}{
		{value: "203.0.113.9", want: "203.0.113.9", ok: true},
		{value: `"[2001:db8::1]:443"`, want: "[2001:db8::1]:443", ok: true},
		{value: `"a\"b\\c"`, want: `a"b\c`, ok: true},
		{value: `""`, want: "", ok: true},
		{value: `"`},
		{value: `"abc`},
		{value: `"a"b"`},
		{value: `"abc\"`},
		{value: `a"b`},
		{value: `a\b`},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			got, ok := unquoteForwardedValue(tc.value)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("unquoteForwardedValue(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseTrustedProxy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		entry string
		want  string
		bad   bool
	}{
		{entry: "10.0.0.0/8", want: "10.0.0.0/8"},
		{entry: " 10.0.0.0/8 ", want: "10.0.0.0/8"},
		{entry: "10.1.2.3/8", want: "10.0.0.0/8"},
		{entry: "10.1.2.3", want: "10.1.2.3/32"},
		{entry: "2001:db8::/32", want: "2001:db8::/32"},
		{entry: "::ffff:10.1.2.3", want: "10.1.2.3/32"},
		{entry: "::ffff:10.0.0.0/104", want: "10.0.0.0/8"},
		{entry: "::ffff:10.1.2.3/128", want: "10.1.2.3/32"},
		{entry: "fe80::1%eth0", want: "fe80::1/128"},
		{entry: "", bad: true},
		{entry: "nonsense", bad: true},
		{entry: "10.0.0.0/99", bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			t.Parallel()
			prefix, err := parseTrustedProxy(tc.entry)
			if tc.bad {
				if err == nil {
					t.Fatalf("parseTrustedProxy(%q) = %v, want an error", tc.entry, prefix)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTrustedProxy(%q) = %v", tc.entry, err)
			}
			if prefix.String() != tc.want {
				t.Errorf("parseTrustedProxy(%q) = %q, want %q", tc.entry, prefix, tc.want)
			}
		})
	}
}

func TestLastField(t *testing.T) {
	t.Parallel()
	cases := []struct{ value, field, rest string }{
		{value: "a, b, c", field: "c", rest: "a, b"},
		{value: "  a  ", field: "a"},
		{value: "a,", field: "", rest: "a"},
		{value: "", field: ""},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			field, rest := lastField(tc.value)
			if field != tc.field || rest != tc.rest {
				t.Errorf("lastField(%q) = (%q, %q), want (%q, %q)", tc.value, field, rest, tc.field, tc.rest)
			}
		})
	}
}

// clientIPApp serves the resolved client address at /whoami, which is how the
// tests below observe what a handler sees.
func clientIPApp(t *testing.T, opts ClientIPOptions) *App {
	t.Helper()
	appOpts := quietOptions()
	appOpts.ClientIP = opts
	app := New(appOpts)
	app.Get("/whoami", func(ctx *Context, _ Empty) (string, error) {
		return ctx.ClientIP(), nil
	})
	return app
}

func TestContextClientIP(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, clientIPApp(t, ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}}))

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set(DefaultForwardedHeader, "198.51.100.9")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `"198.51.100.9"`)
}

func TestContextClientIPWithoutAnAddress(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, clientIPApp(t, ClientIPOptions{}))

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.RemoteAddr = "/var/run/muzak.sock"
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `""`)
}

func TestContextClientAddr(t *testing.T) {
	t.Parallel()
	appOpts := quietOptions()
	appOpts.ClientIP = ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}}
	app := New(appOpts)
	var seen netip.Addr
	app.Get("/whoami", func(ctx *Context, _ Empty) (Empty, error) {
		seen = ctx.ClientAddr()
		return Empty{}, nil
	})
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set(DefaultForwardedHeader, "::ffff:198.51.100.9")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
	if !seen.IsValid() || seen.String() != "198.51.100.9" {
		t.Errorf("ClientAddr() = %q, want the mapped address unwrapped to 198.51.100.9", seen)
	}
}

func TestContextClientAddrWithoutAnApplication(t *testing.T) {
	t.Parallel()
	// A Context built by hand has no application to read a policy from, so it
	// falls back to trusting nothing rather than dereferencing nothing.
	c := &Context{r: forwardedRequest("203.0.113.7:1", DefaultForwardedHeader, "198.51.100.9")}
	if got := c.ClientIP(); got != "203.0.113.7" {
		t.Errorf("ClientIP() = %q, want the peer address", got)
	}
}

func TestBuildRejectsAnUnreadableTrustedProxy(t *testing.T) {
	t.Parallel()
	app := clientIPApp(t, ClientIPOptions{TrustedProxies: []string{"10.0.0.0/8", "nonsense"}})
	message := buildError(t, app)
	if !strings.Contains(message, "nonsense") {
		t.Errorf("build error = %q, want it to name the entry it could not parse", message)
	}
}

// TestClientIPDropsTheIPv6Zone is the regression test for a zoned address
// kept as written: a trusted proxy's link-local address with a zone was read
// as an untrusted client, and one client rotating the suffix of its own
// address was counted as as many.
func TestClientIPDropsTheIPv6Zone(t *testing.T) {
	t.Parallel()
	resolver, err := newClientIPResolver(ClientIPOptions{
		TrustedProxies:       []string{"10.0.0.0/8", "fe80::/10"},
		ConnectionIPv6Prefix: 128,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A trusted proxy that reports itself with a zone is still trusted, so
	// the walk steps over it to the client.
	req := forwardedRequest("10.1.2.3:9", DefaultForwardedHeader, "203.0.113.5, fe80::1%eth0")
	if got := resolver.resolve(req).String(); got != "203.0.113.5" {
		t.Errorf("resolve() = %q, want the client behind the zoned proxy", got)
	}
	req = forwardedRequest("[fe80::1%eth0]:9", DefaultForwardedHeader, "203.0.113.5")
	if got := resolver.resolve(req).String(); got != "203.0.113.5" {
		t.Errorf("resolve() = %q, want the client behind a zoned peer", got)
	}

	// Two spellings of one client are one client, however they are counted.
	keys := map[string]bool{}
	for _, zone := range []string{"a", "b", "c"} {
		req := forwardedRequest("10.1.2.3:9", DefaultForwardedHeader, "2001:db8::1%"+zone)
		addr := resolver.resolve(req)
		if addr.Zone() != "" || strings.Contains(addr.String(), "%") {
			t.Errorf("resolve() = %q, want no zone", addr)
		}
		keys[resolver.connectionKey(addr)] = true
	}
	if len(keys) != 1 {
		t.Errorf("zone suffixes made %d connection keys, want 1: %v", len(keys), keys)
	}

	app := mustBuild(t, clientIPApp(t, ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}}))
	req = httptest.NewRequest(http.MethodGet, "/whoami", nil)
	req.Header.Set(DefaultForwardedHeader, "2001:db8::1%evil")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `"2001:db8::1"`)
}

// TestClientIPTrustsAnIPv4MappedPrefix is the regression test for a trusted
// proxy written as an IPv4-in-IPv6 prefix, which parsed and then never
// matched, because a peer is always unmapped before it is compared.
func TestClientIPTrustsAnIPv4MappedPrefix(t *testing.T) {
	t.Parallel()
	resolver, err := newClientIPResolver(ClientIPOptions{TrustedProxies: []string{"::ffff:10.0.0.0/104"}})
	if err != nil {
		t.Fatal(err)
	}
	req := forwardedRequest("10.0.0.1:9", DefaultForwardedHeader, "8.8.8.8")
	if got := resolver.resolve(req).String(); got != "8.8.8.8" {
		t.Errorf("resolve() = %q, want the client the mapped-prefix proxy reported", got)
	}
	req = forwardedRequest("11.0.0.1:9", DefaultForwardedHeader, "8.8.8.8")
	if got := resolver.resolve(req).String(); got != "11.0.0.1" {
		t.Errorf("resolve() = %q, want the untrusted peer outside the prefix", got)
	}
}
