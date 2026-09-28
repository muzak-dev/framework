package muzak

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// newPrefixWSApp serves a WebSocket route whose connections stay open until
// the test ends, behind a trusted proxy on the loopback address so that each
// handshake can claim the client address the test chooses.
func newPrefixWSApp(t *testing.T, clientIP ClientIPOptions, ws WSOptions) *httptest.Server {
	t.Helper()
	opts := quietOptions()
	clientIP.TrustedProxies = []string{"127.0.0.1/32"}
	opts.ClientIP = clientIP
	opts.WebSocket = ws
	app := New(opts)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	app.WS("/ws", func(ctx *Context, _ Empty, _ *WSConn) error {
		select {
		case <-block:
		case <-ctx.Context().Done():
		}
		return nil
	})
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return server
}

// dialFrom opens a handshake claiming the given client address and reports
// whether it was accepted.
func dialFrom(t *testing.T, server *httptest.Server, address string) bool {
	t.Helper()
	_, resp := dialRaw(t, server.URL, "/ws", "X-Forwarded-For", address)
	return resp.StatusCode == http.StatusSwitchingProtocols
}

func TestPerClientCapCountsOneSlash56AsOneClient(t *testing.T) {
	t.Parallel()
	// The proportions of the defaults, 64 per client and 1024 in all, scaled
	// down. Counted by the /64, the four /64s of one home's /56 took every
	// slot the process had.
	server := newPrefixWSApp(t, ClientIPOptions{}, WSOptions{MaxConnectionsPerIP: 2, MaxConnections: 8})

	accepted := 0
	for sub := range 4 {
		for host := range 2 {
			if dialFrom(t, server, fmt.Sprintf("2001:db8:0:ab%02x::%x", sub, host+1)) {
				accepted++
			}
		}
	}
	if accepted != 2 {
		t.Errorf("one /56 held %d connections with MaxConnectionsPerIP=2, want 2", accepted)
	}
	if !dialFrom(t, server, "192.0.2.10") {
		t.Error("an unrelated client was refused after one /56 had taken its share")
	}
}

func TestPerClientCapPrefixIsConfigurable(t *testing.T) {
	t.Parallel()
	t.Run("a narrower IPv6 prefix gives each /64 its own allowance", func(t *testing.T) {
		t.Parallel()
		server := newPrefixWSApp(t, ClientIPOptions{ConnectionIPv6Prefix: 64},
			WSOptions{MaxConnectionsPerIP: 1, MaxConnections: 100})
		for sub := range 3 {
			if !dialFrom(t, server, fmt.Sprintf("2001:db8:0:ab%02x::1", sub)) {
				t.Errorf("the /64 numbered %d was refused, want an allowance of its own", sub)
			}
		}
		if dialFrom(t, server, "2001:db8:0:ab00::2") {
			t.Error("a second address in a /64 already at its cap was accepted")
		}
	})
	t.Run("a wider IPv4 prefix counts the block as one client", func(t *testing.T) {
		t.Parallel()
		server := newPrefixWSApp(t, ClientIPOptions{ConnectionIPv4Prefix: 24},
			WSOptions{MaxConnectionsPerIP: 1, MaxConnections: 100})
		if !dialFrom(t, server, "198.51.100.1") {
			t.Fatal("the first address in the block was refused")
		}
		if dialFrom(t, server, "198.51.100.200") {
			t.Error("a second address in the same /24 was accepted past the cap")
		}
		if !dialFrom(t, server, "198.51.101.1") {
			t.Error("an address in the next /24 was refused")
		}
	})
}

func TestSSEPerClientCapUsesTheConnectionPrefix(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.ClientIP = ClientIPOptions{TrustedProxies: []string{"127.0.0.1/32"}}
	options.SSE = SSEOptions{MaxStreamsPerIP: 1, KeepAlive: -1}
	_, server := newSSETestAppWith(t, options, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			<-stream.Context().Done()
			return nil
		})
	})
	from := func(address string) func(*SSEDialOptions) {
		return func(o *SSEDialOptions) { o.Header = http.Header{"X-Forwarded-For": {address}} }
	}
	openStream(t, server.URL, "/stream", from("2001:db8:0:ab00::1"))
	if _, response := tryStream(t, server.URL, "/stream", from("2001:db8:0:abff::1")); response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a second /64 of the same /56 got %d, want 503", response.StatusCode)
	}
}

func TestConnectionKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		opts ClientIPOptions
		addr string
		want string
	}{
		{ClientIPOptions{}, "192.0.2.7", "192.0.2.7"},
		{ClientIPOptions{}, "2001:db8:1:2::7", "2001:db8:1::/56"},
		{ClientIPOptions{ConnectionIPv6Prefix: 48}, "2001:db8:1:2::7", "2001:db8:1::/48"},
		{ClientIPOptions{ConnectionIPv6Prefix: 128}, "2001:db8:1:2::7", "2001:db8:1:2::7"},
		{ClientIPOptions{ConnectionIPv4Prefix: 24}, "192.0.2.7", "192.0.2.0/24"},
	}
	for _, tc := range tests {
		resolver, err := newClientIPResolver(tc.opts)
		if err != nil {
			t.Fatalf("newClientIPResolver(%+v) = %v", tc.opts, err)
		}
		if got := resolver.connectionKey(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("connectionKey(%s) with %+v = %q, want %q", tc.addr, tc.opts, got, tc.want)
		}
	}
}

func TestConnectionPrefixIsValidatedAtBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts ClientIPOptions
		want []string
	}{
		{"an IPv6 prefix wider than a provider allocation", ClientIPOptions{ConnectionIPv6Prefix: 31}, []string{"ConnectionIPv6Prefix is 31"}},
		{"an IPv6 prefix longer than an address", ClientIPOptions{ConnectionIPv6Prefix: 129}, []string{"ConnectionIPv6Prefix is 129"}},
		{"a negative IPv6 prefix", ClientIPOptions{ConnectionIPv6Prefix: -1}, []string{"ConnectionIPv6Prefix is -1"}},
		{"an IPv4 prefix too wide", ClientIPOptions{ConnectionIPv4Prefix: 8}, []string{"ConnectionIPv4Prefix is 8"}},
		{"an IPv4 prefix longer than an address", ClientIPOptions{ConnectionIPv4Prefix: 33}, []string{"ConnectionIPv4Prefix is 33"}},
		{
			"both at once, reported together",
			ClientIPOptions{ConnectionIPv6Prefix: 200, ConnectionIPv4Prefix: 64, TrustedProxies: []string{"not an address"}},
			[]string{"ConnectionIPv6Prefix is 200", "ConnectionIPv4Prefix is 64", "not an address"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.ClientIP = tc.opts
			app := New(opts)
			app.Get("/", okHandler)
			message := buildError(t, app)
			for _, want := range tc.want {
				if !strings.Contains(message, want) {
					t.Errorf("Build() = %q, want it to report %q", message, want)
				}
			}
		})
	}
}

func TestSSEPerClientCapRefusesARequestWithNoAddress(t *testing.T) {
	t.Parallel()
	// A listener that is not addressed by IP gives the cap nothing to count,
	// and every stream sharing one key would be worse than refusing them.
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, _ *SSEStream[itemOut]) error {
		t.Error("the handler ran for a stream the cap could not attribute")
		return nil
	})
	mustBuild(t, app)
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.RemoteAddr = "/var/run/muzak.sock"
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusInternalServerError)
}
