package muzak

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestClientIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"192.0.2.7", "192.0.2.7"},
		{"2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::/64"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"fe80::1%eth0", "fe80::/64"},
	}
	for _, tc := range tests {
		if got := clientIdentity(netip.MustParseAddr(tc.in)); got != tc.want {
			t.Errorf("clientIdentity(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIPTrackerCountsAnIPv6ClientByItsSlash64 is the regression test for a
// default tracker that keyed on the exact address, so a client holding an IPv6
// /64 (the block most providers hand out) got a fresh budget from every one of
// its 2^64 addresses. IPv4 keys, including an IPv4-mapped peer, are unchanged.
func TestIPTrackerCountsAnIPv6ClientByItsSlash64(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.RateLimit = RateLimitOptions{Quotas: []Quota{{Name: "q", Window: time.Minute, Limit: 1}}}
	app := New(opts)
	app.Get("/x", okHandler)
	mustBuild(t, app)

	from := func(remote string) int {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.RemoteAddr = remote
		return doRequest(t, app, req).Code
	}
	if code := from("[2001:db8:1:2::1]:443"); code != http.StatusOK {
		t.Fatalf("first request from the /64 = %d, want 200", code)
	}
	for i := range 20 {
		if code := from(fmt.Sprintf("[2001:db8:1:2::%x]:443", 0x100+i)); code != http.StatusTooManyRequests {
			t.Fatalf("another address in the same /64 got %d, want 429 from the shared budget", code)
		}
	}
	if code := from("[2001:db8:1:3::1]:443"); code != http.StatusOK {
		t.Errorf("a neighbouring /64 = %d, want a budget of its own", code)
	}
	if code := from("192.0.2.1:1"); code != http.StatusOK {
		t.Errorf("an IPv4 client = %d, want 200", code)
	}
	if code := from("[::ffff:192.0.2.1]:1"); code != http.StatusTooManyRequests {
		t.Errorf("the same IPv4 client written as mapped IPv6 = %d, want the budget it already spent", code)
	}

	ctx := &Context{r: httptest.NewRequest(http.MethodGet, "/", nil)}
	ctx.r.RemoteAddr = "192.0.2.7:1"
	if key, err := IPTracker(ctx); err != nil || key != "ip:192.0.2.7" {
		t.Errorf("IPTracker = %q, %v; want the IPv4 key spelled as it always was", key, err)
	}
}

// TestIPv6FloodCannotResetAnotherQuota replays the attack end to end: a client
// flooding the application-wide limit from fresh IPv6 addresses used to fill
// the shared in-memory table and evict a per-account login counter, handing
// the attacker a fresh set of guesses. From one /64 the flood is now a single
// counter, and from many /64s it evicts only counters of its own quota.
func TestIPv6FloodCannotResetAnotherQuota(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		storage RateLimitStorage
		address func(i int) string
		flood   int
	}{
		{
			name:    "from one /64 against the default storage",
			address: func(i int) string { return fmt.Sprintf("[2001:db8:1:2::%x:%x]:443", i>>16, i&0xffff) },
			flood:   2_000,
		},
		{
			name:    "from many /64s against a small storage",
			storage: NewMemoryRateLimitStorage(MemoryRateLimitOptions{MaxEntries: 64}),
			address: func(i int) string { return fmt.Sprintf("[2001:db8:%x:%x::1]:443", i>>16, i&0xffff) },
			flood:   1_000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.RateLimit = RateLimitOptions{
				Storage: tc.storage,
				Quotas:  []Quota{{Name: "global", Window: time.Hour, Limit: 1000}},
			}
			app := New(opts)
			app.Get("/login", okHandler, WithRateLimit(RateLimitOptions{
				Tracker: func(ctx *Context) (string, error) { return "user:" + ctx.Header("X-User"), nil },
				Quotas:  []Quota{{Name: "login", Window: time.Minute, Limit: 3}},
			}))
			app.Get("/x", okHandler)
			mustBuild(t, app)

			login := func() int {
				req := httptest.NewRequest(http.MethodGet, "/login", nil)
				req.Header.Set("X-User", "alice")
				return doRequest(t, app, req).Code
			}
			for range 3 {
				login()
			}
			if code := login(); code != http.StatusTooManyRequests {
				t.Fatalf("fourth login = %d, want 429", code)
			}
			for i := range tc.flood {
				req := httptest.NewRequest(http.MethodGet, "/x", nil)
				req.RemoteAddr = tc.address(i)
				doRequest(t, app, req)
			}
			if code := login(); code != http.StatusTooManyRequests {
				t.Errorf("login after the flood = %d, want 429: the flood reset alice's counter", code)
			}
		})
	}
}

// TestWebSocketPerClientCapCountsAnIPv6ClientByItsSlash64 is the regression
// test for a per-client connection cap keyed on the exact address, which let
// one /64 hold any number of connections by using a new address for each.
func TestWebSocketPerClientCapCountsAnIPv6ClientByItsSlash64(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ClientIP = ClientIPOptions{TrustedProxies: []string{"127.0.0.1/32"}}
	opts.WebSocket = WSOptions{MaxConnectionsPerIP: 2, MaxConnections: 1000}
	app := New(opts)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		select {
		case <-block:
		case <-ctx.Context().Done():
		}
		return nil
	})
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	accepted := 0
	for i := range 10 {
		_, resp := dialRaw(t, server.URL, "/ws", "X-Forwarded-For", fmt.Sprintf("2001:db8:1:1::%x", 0x100+i))
		if resp.StatusCode == http.StatusSwitchingProtocols {
			accepted++
		}
	}
	if accepted != 2 {
		t.Errorf("%d connections accepted from one /64 with MaxConnectionsPerIP=2, want 2", accepted)
	}
	if _, resp := dialRaw(t, server.URL, "/ws", "X-Forwarded-For", "2001:db8:1:2::1"); resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("a neighbouring /64 got %d, want an allowance of its own", resp.StatusCode)
	}
}

// TestPerClientKeyWithoutAnAddress covers a per-client cap on a listener with
// no IP address, which is refused rather than counted under one shared key.
func TestPerClientKeyWithoutAnAddress(t *testing.T) {
	t.Parallel()
	ctx := &Context{r: httptest.NewRequest(http.MethodGet, "/", nil)}
	ctx.r.RemoteAddr = "/var/run/muzak.sock"
	if _, err := perClientKey(ctx, 1); err == nil {
		t.Error("perClientKey accepted a request with no address")
	}
	ctx.r.RemoteAddr = "[2001:db8::7]:1"
	if key, err := perClientKey(ctx, 1); err != nil || key != "2001:db8::/64" {
		t.Errorf("perClientKey = %q, %v; want the /64", key, err)
	}
	if key, err := perClientKey(ctx, 0); err != nil || key != "" {
		t.Errorf("perClientKey with no limit = %q, %v; want no key at all", key, err)
	}
}

// TestIPPrefixTracker covers the tracker for a prefix length other than the
// default one, and the lengths it refuses to be built with.
func TestIPPrefixTracker(t *testing.T) {
	t.Parallel()
	tracker := IPPrefixTracker(24, 48)
	ctx := &Context{r: httptest.NewRequest(http.MethodGet, "/", nil)}
	for remote, want := range map[string]string{
		"192.0.2.7:1":            "ip:192.0.2.0/24",
		"[::ffff:192.0.2.9]:1":   "ip:192.0.2.0/24",
		"[2001:db8:1:2::7]:1":    "ip:2001:db8:1::/48",
		"[2001:db8:1:ffff::1]:1": "ip:2001:db8:1::/48",
	} {
		ctx.r.RemoteAddr = remote
		if key, err := tracker(ctx); err != nil || key != want {
			t.Errorf("IPPrefixTracker(24, 48) for %s = %q, %v; want %q", remote, key, err, want)
		}
	}
	ctx.r.RemoteAddr = "/var/run/muzak.sock"
	if _, err := tracker(ctx); err == nil {
		t.Error("IPPrefixTracker counted a request with no address")
	}
	for _, bits := range [][2]int{{-1, 64}, {33, 64}, {32, -1}, {32, 129}} {
		if recovered := catchPanic(func() { IPPrefixTracker(bits[0], bits[1]) }); recovered == nil {
			t.Errorf("IPPrefixTracker(%d, %d) was built, want a panic", bits[0], bits[1])
		}
	}
}
