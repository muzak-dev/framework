package muzak

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fullwidthLoopback is 127.0.0.1 written in fullwidth digits and stops, which
// a URL parser takes for a name and net/http maps to the address before it
// dials. It is spelled in escapes because the sources are kept to ASCII.
const fullwidthLoopback = "http://\uff11\uff12\uff17\uff0e\uff10\uff0e\uff10\uff0e\uff11/"

// ssrfVectors are the ways a URL is written to reach somewhere a server's own
// client should not go. Every one of them must be refused by a client with the
// default policy without a single connection being attempted.
var ssrfVectors = []struct {
	url    string
	reason string
}{
	// The loopback interface, in every spelling a parser has been fooled by.
	{"http://127.0.0.1/", "loopback"},
	{"http://127.0.0.1:6379/", "loopback"},
	{"http://127.255.255.254/", "loopback"},
	{"http://127.1/", "non-canonical"},
	{"http://127.0.1/", "non-canonical"},
	{"http://0177.0.0.1/", "non-canonical"},
	{"http://0177.1/", "non-canonical"},
	{"http://0x7f.0.0.1/", "non-canonical"},
	{"http://0X7F.0.0.1/", "non-canonical"},
	{"http://0x7f.1/", "non-canonical"},
	{"http://0x7f000001/", "non-canonical"},
	{"http://2130706433/", "non-canonical"},
	{"http://017700000001/", "non-canonical"},
	{"http://127.000.000.001/", "non-canonical"},
	{"http://127.0.0.1./", "non-canonical"},
	{"http://0.0.0.0/", "unspecified"},
	{"http://0/", "non-canonical"},
	{"http://0.0.0.0:8080/admin", "unspecified"},
	{"http://[::1]/", "loopback"},
	{"http://[::]/", "unspecified"},
	{"http://[0:0:0:0:0:0:0:1]/", "loopback"},
	{"http://[::ffff:127.0.0.1]/", "loopback"},
	{"http://[::ffff:7f00:1]/", "loopback"},
	{"http://[0:0:0:0:0:ffff:7f00:1]/", "loopback"},
	{"http://[::127.0.0.1]/", "loopback"},
	{"http://[64:ff9b::7f00:1]/", "NAT64"},
	{"http://[64:ff9b::127.0.0.1]/", "NAT64"},
	{"http://[64:ff9b:1:7f00:0:100::]/", "NAT64"},
	{"http://[2002:7f00:1::]/", "6to4"},
	{"http://[2001:0:4136:e378:8000:63bf:3fff:fdd2]/", "Teredo"},
	{"http://localhost/", "localhost"},
	{"http://LOCALHOST/", "localhost"},
	{"http://localhost./", "localhost"},
	{"http://admin.localhost/", "localhost"},
	{"http://localhost:8080/", "localhost"},

	// Cloud metadata services, directly and through an IPv6 form.
	{"http://169.254.169.254/latest/meta-data/", "metadata"},
	{"http://169.254.170.2/v2/credentials", "metadata"},
	{"http://169.254.170.23/", "metadata"},
	{"http://0251.0376.0251.0376/", "non-canonical"},
	{"http://2852039166/", "non-canonical"},
	{"http://[::ffff:169.254.169.254]/", "metadata"},
	{"http://[::ffff:a9fe:a9fe]/", "metadata"},
	{"http://[64:ff9b::a9fe:a9fe]/", "metadata"},
	{"http://[2002:a9fe:a9fe::]/", "metadata"},
	{"http://[fd00:ec2::254]/", "metadata"},
	{"http://[fd00:ec2::23]/", "metadata"},
	{"http://[fd20:ce::254]/", "metadata"},
	{"http://168.63.129.16/", "metadata"},
	{"http://100.100.100.200/", "metadata"},
	{"http://192.0.0.192/", "metadata"},

	// Private, shared and special-purpose ranges.
	{"http://10.0.0.1/", "private"},
	{"http://10.255.255.255/", "private"},
	{"http://172.16.0.1/", "private"},
	{"http://172.31.255.255/", "private"},
	{"http://192.168.1.1/", "private"},
	{"http://100.64.0.1/", "carrier-grade"},
	{"http://100.127.255.255/", "carrier-grade"},
	{"http://198.18.0.1/", "benchmarking"},
	{"http://198.19.255.255/", "benchmarking"},
	{"http://240.0.0.1/", "reserved"},
	{"http://255.255.255.255/", "broadcast"},
	{"http://224.0.0.1/", "multicast"},
	{"http://239.255.255.250/", "multicast"},
	{"http://192.0.2.1/", "documentation"},
	{"http://198.51.100.7/", "documentation"},
	{"http://203.0.113.9/", "documentation"},
	{"http://192.0.0.8/", "IETF"},
	{"http://192.88.99.1/", "6to4 relay"},
	{"http://[fe80::1]/", "link-local"},
	{"http://[fe80::1%25eth0]/", "link-local"},
	{"http://[febf::1]/", "link-local"},
	{"http://[fec0::1]/", "site-local"},
	{"http://[ff02::1]/", "multicast"},
	{"http://[fc00::1]/", "unique local"},
	{"http://[fd12:3456:789a::1]/", "unique local"},
	{"http://[2001:db8::1]/", "documentation"},
	{"http://[3fff::1]/", "documentation"},
	{"http://[2001:2::1]/", "benchmarking"},
	{"http://[100::1]/", "discard"},
	{"http://[5f00::1]/", "segment routing"},
	{"http://[4000::1]/", "global unicast"},

	// Names ending in a number that is not an address at all.
	{"http://1.2.3.4.5/", "not an address"},
	{"http://999.0.0.1/", "not an address"},
	{"http://example.123/", "not an address"},
}

func TestClientRefusesEverySSRFVector(t *testing.T) {
	t.Parallel()
	client, network := newTestClient(t, ClientOptions{})
	for _, vector := range ssrfVectors {
		t.Run(vector.url, func(t *testing.T) {
			resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, vector.url))
			if resp != nil {
				_ = resp.Body.Close()
			}
			var refused *AddressRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Do(%s) error = %v, want an *AddressRefusedError", vector.url, err)
			}
			if !strings.Contains(refused.Error(), vector.reason) {
				t.Errorf("Do(%s) error = %q, want it to mention %q", vector.url, refused, vector.reason)
			}
			if !strings.HasPrefix(err.Error(), "muzak: ") {
				t.Errorf("error = %q, want it to start with muzak: ", err)
			}
		})
	}
	if dialled := network.connections(); len(dialled) != 0 {
		t.Errorf("the client attempted connections to %v, want none at all", dialled)
	}
	if lookups := network.resolutions(); lookups != 0 {
		t.Errorf("the client resolved %d names, want none: every vector is judged from the URL", lookups)
	}
}

func TestClientRefusesANameThatResolvesToAPrivateAddress(t *testing.T) {
	t.Parallel()
	client, network := newTestClient(t, ClientOptions{})
	network.resolve("intranet.example.com", netip.MustParseAddr("10.1.2.3"))
	network.resolve("metadata.example.com", netip.MustParseAddr("169.254.169.254"))
	network.resolve("mapped.example.com", netip.MustParseAddr("::ffff:127.0.0.1"))
	network.resolve("loopback6.example.com", netip.IPv6Loopback())

	for host, reason := range map[string]string{
		"intranet.example.com":  "private",
		"metadata.example.com":  "metadata",
		"mapped.example.com":    "loopback",
		"loopback6.example.com": "loopback",
	} {
		_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://"+host+"/"))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) || !strings.Contains(refused.Reason, reason) {
			t.Fatalf("Do(%s) error = %v, want it refused as %s", host, err, reason)
		}
		if refused.Host != host || !refused.Addr.IsValid() {
			t.Errorf("refusal = %+v, want it to name the host and the address it resolved to", refused)
		}
	}
	if dialled := network.connections(); len(dialled) != 0 {
		t.Errorf("the client attempted connections to %v, want none", dialled)
	}
}

func TestClientDialsOnlyTheAddressesThePolicyPasses(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("public")) })
	client, network := newTestClient(t, ClientOptions{})
	// A name whose owner lists a private address first, in the hope that
	// the client connects to whatever comes first.
	network.resolve("mixed.example.com", netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.5"), publicA)
	network.route(publicA, "80", server.Server)

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://mixed.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if body := drain(t, resp); body != "public" {
		t.Errorf("body = %q, want the public address's answer", body)
	}
	if dialled := network.connections(); len(dialled) != 1 || dialled[0] != "93.184.216.34:80" {
		t.Errorf("connections = %v, want only the public address", dialled)
	}
}

func TestClientChecksEveryConnectionAgainstDNSRebinding(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	client, network := newTestClient(t, ClientOptions{MaxAttempts: 1})
	network.serve("rebind.example.com", publicA, "80", server.Server)

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://rebind.example.com/"))
	if err != nil {
		t.Fatalf("first Do: %v", err)
	}
	drain(t, resp)

	// The name now answers with the metadata service, which is what a
	// rebinding attack does between a check and a fetch. A check made on the
	// name when the URL was first accepted would not see it; a check on the
	// connection does.
	network.resolve("rebind.example.com", netip.MustParseAddr("169.254.169.254"))
	_ = client.Close()
	_, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://rebind.example.com/"))
	var refused *AddressRefusedError
	if !errors.As(err, &refused) || refused.Addr != netip.MustParseAddr("169.254.169.254") {
		t.Fatalf("second Do error = %v, want the rebound address refused", err)
	}
	if hits := server.hits.Load(); hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
}

func TestClientControlChecksTheSocketAddress(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	// The production dialer, not a fake: its net.Dialer runs the policy on
	// the socket itself, after every other step has already been taken, so a
	// route around the address list still meets it.
	client := NewClient(ClientOptions{})
	defer client.Close()
	_, err = client.dialer.connect(t.Context(), "tcp", listener.Addr().String())
	var refused *AddressRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "loopback") {
		t.Fatalf("connect error = %v, want the loopback address refused on the socket", err)
	}
	select {
	case <-accepted:
		t.Fatal("the listener accepted a connection the policy refused")
	case <-time.After(50 * time.Millisecond):
	}

	if err := client.policy.control("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("control(public) = %v, want nil", err)
	}
	if err := client.policy.control("tcp", "[fe80::1%en0]:80", nil); err == nil {
		t.Error("control(link-local with a zone) = nil, want it refused")
	}
	// An address Control cannot read is refused rather than let through.
	if err := client.policy.control("unix", "/tmp/socket", nil); err == nil {
		t.Error("control(unreadable) = nil, want it refused")
	}
}

func TestClientProductionDialerConnectsWhenPrivateNetworksAreAllowed(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("internal")) })
	client := NewClient(ClientOptions{AllowPrivateNetworks: true})
	defer client.Close()
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, server.URL+"/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if body := drain(t, resp); body != "internal" {
		t.Errorf("body = %q, want %q", body, "internal")
	}
}

func TestClientAllowPrivateNetworksNeverOpensMetadata(t *testing.T) {
	t.Parallel()
	client, network := newTestClient(t, ClientOptions{AllowPrivateNetworks: true})
	for _, target := range []string{
		"http://169.254.169.254/", "http://[fd00:ec2::254]/", "http://168.63.129.16/",
		"http://[64:ff9b::a9fe:a9fe]/", "http://[::169.254.169.254]/", "http://169.254.1.1/",
		// The IPv4-translated form SIIT (RFC 2765) delivers to the address
		// it carries, as NAT64 does.
		"http://[::ffff:0:a9fe:a9fe]/",
	} {
		_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, target))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) || refused.kind != refusedMetadata {
			t.Errorf("Do(%s) error = %v, want a metadata refusal", target, err)
			continue
		}
		if !strings.Contains(err.Error(), "AllowedNetworks") || strings.Contains(err.Error(), "set ClientOptions.AllowPrivateNetworks") {
			t.Errorf("error = %q, want it to point at AllowedNetworks and not at the setting already on", err)
		}
	}
	if dialled := network.connections(); len(dialled) != 0 {
		t.Errorf("connections = %v, want none", dialled)
	}
}

func TestClientAllowedAndDeniedNetworks(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(http.ResponseWriter, *http.Request) {})
	client, network := newTestClient(t, ClientOptions{
		AllowedNetworks: []netip.Prefix{
			netip.MustParsePrefix("10.20.0.0/16"),
			netip.MustParsePrefix("169.254.169.254/32"),
			// The IPv4-in-IPv6 form of a prefix means the IPv4 one.
			netip.MustParsePrefix("::ffff:172.16.5.0/120"),
		},
		DeniedNetworks: []netip.Prefix{
			netip.MustParsePrefix("10.20.30.0/24"),
			netip.MustParsePrefix("93.184.216.0/24"),
			// Host bits are ignored, so this is 151.101.0.0/16.
			netip.MustParsePrefix("151.101.1.1/16"),
		},
	})
	for _, addr := range []string{"10.20.1.1", "169.254.169.254", "172.16.5.9"} {
		network.route(netip.MustParseAddr(addr), "80", server.Server)
		resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://"+addr+"/"))
		if err != nil {
			t.Errorf("Do(%s) = %v, want it allowed by AllowedNetworks", addr, err)
			continue
		}
		drain(t, resp)
	}
	for _, target := range []string{"http://10.20.30.40/", "http://10.21.0.1/", "http://169.254.169.253/",
		"http://93.184.216.34/", "http://151.101.200.1/", "http://[64:ff9b::5db8:d822]/"} {
		_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, target))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("Do(%s) error = %v, want it refused", target, err)
		}
	}
	_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://93.184.216.34/"))
	if err == nil || !strings.Contains(err.Error(), "DeniedNetworks") {
		t.Errorf("error = %v, want it to name DeniedNetworks", err)
	}
}

func TestClientInvalidOptionsPanic(t *testing.T) {
	t.Parallel()
	cases := map[string]ClientOptions{
		"zero allowed prefix": {AllowedNetworks: []netip.Prefix{{}}},
		"zero denied prefix":  {DeniedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), {}}},
		"proxy with no host":  {Proxy: &url.URL{Scheme: "http"}},
		"proxy scheme":        {Proxy: &url.URL{Scheme: "ftp", Host: "proxy.internal"}},
		"proxy not ascii":     {Proxy: &url.URL{Scheme: "http", Host: "pr\u00f6xy.internal:3128"}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			recovered := catchPanic(func() { NewClient(opts) })
			message, _ := recovered.(string)
			if !strings.HasPrefix(message, "muzak: ") {
				t.Fatalf("NewClient panicked with %v, want a muzak: sentence", recovered)
			}
		})
	}
}

func TestClientUsesTheProxyAndStillJudgesWhatItCan(t *testing.T) {
	proxy := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("via proxy to " + r.URL.String()))
	})
	proxyURL, _ := url.Parse(proxy.URL)
	// The proxy sits on the loopback interface, as an egress proxy on a
	// private address does, and is connected to all the same.
	client := NewClient(ClientOptions{Proxy: proxyURL, MaxAttempts: 1})
	defer client.Close()

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/items"))
	if err != nil {
		t.Fatalf("Do through the proxy: %v", err)
	}
	if body := drain(t, resp); body != "via proxy to http://api.example.com/items" {
		t.Errorf("body = %q, want the proxy's answer", body)
	}

	for _, target := range []string{"http://127.0.0.1/", "http://localhost/", "http://2130706433/",
		"http://169.254.169.254/", fullwidthLoopback} {
		_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, target))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("Do(%q) through the proxy error = %v, want it refused before reaching the proxy", target, err)
		}
	}
	if hits := proxy.hits.Load(); hits != 1 {
		t.Errorf("proxy hits = %d, want only the allowed request", hits)
	}

	// A proxy named by a name that resolves to a private address, as an
	// internal egress proxy is, is reached too, and over https the target
	// goes in a CONNECT the proxy is trusted to judge.
	_, port, _ := net.SplitHostPort(proxyURL.Host)
	named := NewClient(ClientOptions{Proxy: &url.URL{Scheme: "http", Host: "localhost:" + port}, MaxAttempts: 1})
	defer named.Close()
	resp, err = named.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/named"))
	if err != nil {
		t.Fatalf("Do through a named proxy: %v", err)
	}
	if body := drain(t, resp); body != "via proxy to http://api.example.com/named" {
		t.Errorf("body = %q, want the proxy's answer", body)
	}
	if got := clientProxyAddr(&url.URL{Scheme: "socks5", Host: "egress.internal"}); got != "egress.internal:1080" {
		t.Errorf("clientProxyAddr(socks5) = %q, want the scheme's port", got)
	}
	if got := clientProxyAddr(&url.URL{Scheme: "https", Host: "[fd00::1]"}); got != "[fd00::1]:443" {
		t.Errorf("clientProxyAddr(https) = %q, want the scheme's port", got)
	}
}

func TestClientIgnoresTheEnvironmentProxy(t *testing.T) {
	proxy := newCountingServer(t, func(http.ResponseWriter, *http.Request) {})
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	server := newCountingServer(t, func(http.ResponseWriter, *http.Request) {})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", server.Server)

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	if proxy.hits.Load() != 0 || server.hits.Load() != 1 {
		t.Errorf("proxy hits = %d, server hits = %d, want the request sent directly", proxy.hits.Load(), server.hits.Load())
	}
}

func TestClientResolvesThroughFullwidthDigitsToTheCheckedAddress(t *testing.T) {
	t.Parallel()
	// Fullwidth digits and a fullwidth stop are not a number to a URL parser,
	// but net/http maps them to "127.0.0.1" before it dials. The check runs
	// on what is dialled, so the mapping cannot get around it.
	client, network := newTestClient(t, ClientOptions{})
	_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, fullwidthLoopback))
	var refused *AddressRefusedError
	if !errors.As(err, &refused) || refused.Addr != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("Do error = %v, want the mapped loopback address refused", err)
	}
	if dialled := network.connections(); len(dialled) != 0 {
		t.Errorf("connections = %v, want none", dialled)
	}
}

func TestClientPercentEncodedHostIsJudgedDecoded(t *testing.T) {
	t.Parallel()
	// net/url refuses a percent-encoded host outright, so the only way to
	// send one is to build the URL by hand, and then it is a name ending in a
	// number that is not an address.
	encoded := "%31%32%37"
	if _, err := url.Parse("http://" + encoded + ".0.0.1/"); err == nil {
		t.Fatal("url.Parse accepted a percent-encoded host; this test's premise has changed")
	}
	client, network := newTestClient(t, ClientOptions{})
	req := mustRequest(t, t.Context(), http.MethodGet, "http://placeholder.example.com/")
	req.URL = &url.URL{Scheme: "http", Host: "%31%32%37.0.0.1", Path: "/"}
	_, err := client.Do(req)
	var refused *AddressRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Do error = %v, want the encoded host refused", err)
	}
	if dialled := network.connections(); len(dialled) != 0 {
		t.Errorf("connections = %v, want none", dialled)
	}
}

func TestClientDialerTriesAddressesInOrderAndReportsFailures(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(http.ResponseWriter, *http.Request) {})
	client, network := newTestClient(t, ClientOptions{MaxAttempts: 1})
	// The first public address refuses connections and the second answers.
	network.resolve("pair.example.com", publicB, publicA)
	network.route(publicA, "80", server.Server)
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://pair.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	if dialled := network.connections(); len(dialled) != 2 {
		t.Errorf("connections = %v, want the failing address and then the working one", dialled)
	}

	// A name with no usable address at all reports the connection error.
	network.resolve("dead.example.com", publicC)
	_, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://dead.example.com/"))
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Do(dead) error = %v, want the connection refusal", err)
	}

	// A name that resolves to nothing usable on the network asked for.
	network.resolve("empty.example.com")
	_, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://empty.example.com/"))
	if err == nil || !strings.Contains(err.Error(), "resolved to no addresses") {
		t.Errorf("Do(empty) error = %v, want no addresses reported", err)
	}

	// A name that does not exist is reported as such, and not retried.
	_, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://nowhere.example.com/"))
	var dns *net.DNSError
	if !errors.As(err, &dns) || !dns.IsNotFound {
		t.Errorf("Do(nowhere) error = %v, want the resolver's not found", err)
	}
}

func TestClientDialerCapsTheAddressesItTries(t *testing.T) {
	t.Parallel()
	client, network := newTestClient(t, ClientOptions{MaxAttempts: 1})
	// A zone owner can answer with as many records as fit in a response.
	many := make([]netip.Addr, 0, 300)
	for i := range 300 {
		many = append(many, netip.AddrFrom4([4]byte{93, 184, byte(i / 250), byte(1 + i%250)}))
	}
	network.resolve("many.example.com", many...)
	_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://many.example.com/"))
	if err == nil {
		t.Fatal("Do succeeded, want every address refusing connections")
	}
	if dialled := network.connections(); len(dialled) != maxDialAddresses {
		t.Errorf("connections attempted = %d, want the cap of %d", len(dialled), maxDialAddresses)
	}
}

func TestClientDialerNetworkFamilies(t *testing.T) {
	t.Parallel()
	client, network := newTestClient(t, ClientOptions{})
	network.resolve("dual.example.com", publicA, publicV6)
	got, err := client.dialer.resolve(t.Context(), "tcp6", "dual.example.com")
	if err != nil || len(got) != 1 || got[0] != publicV6 {
		t.Errorf("resolve(tcp6) = %v, %v, want only the IPv6 address", got, err)
	}
	got, err = client.dialer.resolve(t.Context(), "tcp4", "dual.example.com")
	if err != nil || len(got) != 1 || got[0] != publicA {
		t.Errorf("resolve(tcp4) = %v, %v, want only the IPv4 address", got, err)
	}
	if _, err := client.dialer.DialContext(t.Context(), "tcp", "no-port"); err == nil {
		t.Error("DialContext(no port) = nil error, want the address refused as malformed")
	}
	// The dialer judges a localhost name itself too, for a caller that
	// reaches it without going through Do.
	if _, err := client.dialer.DialContext(t.Context(), "tcp", "localhost:80"); err == nil {
		t.Error("DialContext(localhost) = nil error, want it refused")
	}
}

func TestClientDialTimeoutBoundsTheDial(t *testing.T) {
	t.Parallel()
	client, network := newTestClient(t, ClientOptions{DialTimeout: 50 * time.Millisecond, MaxAttempts: 1})
	network.resolve("blackhole.example.com", publicA, publicB)
	network.block.Store(true)
	start := time.Now()
	_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://blackhole.example.com/"))
	if err == nil {
		t.Fatal("Do succeeded against a host that never answers")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Do took %v, want it bounded by the dial timeout", elapsed)
	}
}

func TestClassifyAddressCoversTheRegistries(t *testing.T) {
	t.Parallel()
	policy := newAddressPolicy(ClientOptions{})
	for _, public := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:4700::1111", "2a00:1450:4001::200e",
		"64:ff9b::808:808", "2002:808:808::", "2620:4f:8000::1", "100.63.255.255", "100.128.0.0", "172.32.0.1",
		"198.17.255.255", "198.20.0.0", "223.255.255.255", "11.0.0.0", "192.169.0.1"} {
		if err := policy.check(public, netip.MustParseAddr(public)); err != nil {
			t.Errorf("check(%s) = %v, want a public address allowed", public, err)
		}
	}
}

// 64:ff9b:1::/48 is the NAT64 block RFC 8215 sets aside for a network's own
// translator, which may use a prefix of 48, 56, 64 or 96 bits inside it, and
// RFC 6052 puts the IPv4 address somewhere else for each. The client read
// every address there as though the prefix were 48 bits, so under a
// translator configured with 64:ff9b:1:abcd::/96, the address that delivers
// to 169.254.169.254 read as 171.205.0.0, a public address, and was let
// through by default; one that delivers to a private address, or to one in
// DeniedNetworks, went the same way, and AllowPrivateNetworks opened the
// metadata service through it. The block is not globally reachable, so it is
// refused as a private range is, and each layout is read for a metadata
// service or a denied network that no setting but AllowedNetworks reaches.
func TestClientJudgesLocalUseNAT64ByEveryLayout(t *testing.T) {
	t.Parallel()
	// 169.254.169.254 (a9fe:a9fe) placed as a translator with each prefix
	// length would place it, and 10.0.0.1 and 8.8.8.8 as one with a /96 does.
	metadata := []string{
		"64:ff9b:1:a9fe:a9:fe00::",      // a /48 prefix
		"64:ff9b:1:a9:fe:a9fe::",        // a /56 prefix
		"64:ff9b:1::a9:fea9:fe00:0",     // a /64 prefix
		"64:ff9b:1::a9fe:a9fe",          // a /96 prefix
		"64:ff9b:1:abcd::a9fe:a9fe",     // a /96 prefix with a subnet of its own
		"64:ff9b:1:abcd:a9:fea9:fe00:0", // a /64 prefix with a subnet of its own
	}
	private := []string{"64:ff9b:1:abcd::a00:1", "64:ff9b:1:abcd::7f00:1", "64:ff9b:1::808:808"}

	strict := newAddressPolicy(ClientOptions{})
	for _, target := range append(append([]string(nil), metadata...), private...) {
		err := strict.check(target, netip.MustParseAddr(target))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "NAT64") {
			t.Errorf("check(%s) = %v, want a local-use NAT64 address refused by default", target, err)
		}
	}

	relaxed := newAddressPolicy(ClientOptions{AllowPrivateNetworks: true})
	for _, target := range metadata {
		err := relaxed.check(target, netip.MustParseAddr(target))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) || refused.kind != refusedMetadata || !strings.Contains(err.Error(), "169.254.169.254") {
			t.Errorf("check(%s) with AllowPrivateNetworks = %v, want a metadata refusal naming 169.254.169.254", target, err)
		}
	}
	for _, target := range private {
		if err := relaxed.check(target, netip.MustParseAddr(target)); err != nil {
			t.Errorf("check(%s) with AllowPrivateNetworks = %v, want it allowed as a private range is", target, err)
		}
	}

	denied := newAddressPolicy(ClientOptions{AllowPrivateNetworks: true,
		DeniedNetworks: []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")}})
	for _, target := range []string{"64:ff9b:1::808:808", "64:ff9b:1:abcd::808:808", "64:ff9b:1:808:8:800::"} {
		err := denied.check(target, netip.MustParseAddr(target))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) || refused.kind != refusedDenied {
			t.Errorf("check(%s) = %v, want it refused by DeniedNetworks, which it may reach", target, err)
		}
	}

	// Naming the IPv6 block itself is how a network that relies on its own
	// translator reaches it.
	allowed := newAddressPolicy(ClientOptions{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("64:ff9b:1:abcd::/96")}})
	if err := allowed.check("x", netip.MustParseAddr("64:ff9b:1:abcd::808:808")); err != nil {
		t.Errorf("check with the block in AllowedNetworks = %v, want it allowed", err)
	}
}

func TestParseLegacyIPv4(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"127.1":               "127.0.0.1",
		"127.0.1":             "127.0.0.1",
		"0177.0.0.1":          "127.0.0.1",
		"0x7f.1":              "127.0.0.1",
		"0x7f000001":          "127.0.0.1",
		"2130706433":          "127.0.0.1",
		"017700000001":        "127.0.0.1",
		"0x":                  "0.0.0.0",
		"0":                   "0.0.0.0",
		"1.2.3.4":             "1.2.3.4",
		"0xff.0xff.0xffff":    "255.255.255.255",
		"4294967295":          "255.255.255.255",
		"0251.0376.0251.0376": "169.254.169.254",
	}
	for input, want := range cases {
		got, ok := parseLegacyIPv4(input)
		if !ok || got.String() != want {
			t.Errorf("parseLegacyIPv4(%q) = %v, %v, want %s", input, got, ok, want)
		}
	}
	for _, bad := range []string{"", ".", "1..2", "1.2.3.4.5", "256.0.0.1", "4294967296", "1.16777216",
		"08", "0x1g", "1.2.3.256", "-1", "1.2.3.a", "99999999999999999999999"} {
		if got, ok := parseLegacyIPv4(bad); ok {
			t.Errorf("parseLegacyIPv4(%q) = %v, want it refused", bad, got)
		}
	}
}

func TestEndsInNumber(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"127.1": true, "example.com": false, "0x7f": true, "1e3": false, "a.0x": true,
		"": false, "a.": false, "10.0.0.1x": false, "abc.0xfg": false,
	} {
		if got := endsInNumber(host); got != want {
			t.Errorf("endsInNumber(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAddressRefusedErrorMessages(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 500) + ".localhost"
	err := newAddressPolicy(ClientOptions{}).checkHost(long, false)
	if err == nil || len(err.Error()) > 400 {
		t.Errorf("error = %q, want a bounded message for a long host", err)
	}
	denied := &AddressRefusedError{Host: "x", Reason: "listed", kind: refusedDenied}
	if strings.Contains(denied.Error(), "AllowPrivateNetworks") {
		t.Errorf("a denied address's message %q offers a remedy that does not apply", denied)
	}
}
