package muzak

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Public addresses the tests pretend their servers live at. They sit outside
// every special-purpose range, so the client's policy lets them through, and
// fakeNet then connects them to a test server on the loopback interface
// instead.
var (
	publicA  = netip.MustParseAddr("93.184.216.34")
	publicB  = netip.MustParseAddr("151.101.1.69")
	publicC  = netip.MustParseAddr("104.16.132.229")
	publicV6 = netip.MustParseAddr("2606:4700::6810:84e5")
)

// fakeNet stands in for DNS and for the network, so that a client can be
// pointed at public-looking hosts that are really test servers. Names resolve
// from a table, and a connection meant for an address goes to whatever server
// routes names for it. Everything it was asked to do is recorded, which is
// how a test proves that a refused request never reached a socket.
type fakeNet struct {
	mu      sync.Mutex
	names   map[string][]netip.Addr
	routes  map[string]string
	dialled []string
	lookups int

	// failures makes the first so many connections fail as a refused
	// connection does, which is what a host that is restarting looks like.
	failures atomic.Int32
	// block makes connect wait until its context ends, which is what a
	// host that drops packets looks like.
	block atomic.Bool
}

// newTestClient builds a client whose resolver and connections are a fakeNet,
// and whose waits between attempts take no time.
func newTestClient(t *testing.T, opts ClientOptions) (*Client, *fakeNet) {
	t.Helper()
	client := NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	network := &fakeNet{names: map[string][]netip.Addr{}, routes: map[string]string{}}
	client.dialer.lookup = network.lookup
	client.dialer.connect = network.connect
	client.retry.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	client.retry.jitter = func(time.Duration) time.Duration { return 0 }
	return client, network
}

// serve makes a host name resolve to an address and a connection to that
// address on a port reach a test server.
func (n *fakeNet) serve(host string, addr netip.Addr, port string, server *httptest.Server) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.names[host] = append(n.names[host], addr)
	n.routes[net.JoinHostPort(addr.String(), port)] = server.Listener.Addr().String()
}

// resolve makes a host name resolve to the given addresses, replacing what it
// resolved to before.
func (n *fakeNet) resolve(host string, addrs ...netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.names[host] = addrs
}

// route makes a connection to an address reach a test server.
func (n *fakeNet) route(addr netip.Addr, port string, server *httptest.Server) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[net.JoinHostPort(addr.String(), port)] = server.Listener.Addr().String()
}

func (n *fakeNet) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lookups++
	addrs, ok := n.names[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return append([]netip.Addr(nil), addrs...), nil
}

func (n *fakeNet) connect(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	n.dialled = append(n.dialled, address)
	target := n.routes[address]
	n.mu.Unlock()
	if n.block.Load() {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
	}
	if n.failures.Add(-1) >= 0 || target == "" {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, target)
}

// connections returns every address a connection was attempted to.
func (n *fakeNet) connections() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.dialled...)
}

// resolutions returns how many names were looked up.
func (n *fakeNet) resolutions() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lookups
}

// countingServer is a test server that counts its requests and answers them
// with a handler.
type countingServer struct {
	*httptest.Server
	hits atomic.Int32
}

func newCountingServer(t *testing.T, handler http.HandlerFunc) *countingServer {
	t.Helper()
	server := &countingServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// mustRequest builds a request or fails the test.
func mustRequest(t *testing.T, ctx context.Context, method, target string, body ...string) *http.Request {
	t.Helper()
	var req *http.Request
	var err error
	if len(body) > 0 {
		req, err = http.NewRequestWithContext(ctx, method, target, strings.NewReader(body[0]))
	} else {
		req, err = http.NewRequestWithContext(ctx, method, target, nil)
	}
	if err != nil {
		t.Fatalf("building %s %s: %v", method, target, err)
	}
	return req
}

// drain reads and closes a response body, returning what it held.
func drain(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	return string(body)
}
