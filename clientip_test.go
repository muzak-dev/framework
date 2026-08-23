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
