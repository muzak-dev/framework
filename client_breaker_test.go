package muzak

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestClientCircuitBreakerTransitions(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	server := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) })
	client, network := newTestClient(t, ClientOptions{
		MaxAttempts:    1,
		CircuitBreaker: CircuitBreakerOptions{Threshold: 3, Cooldown: time.Minute},
	})
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	client.breakers.now = clock.Now
	network.serve("api.example.com", publicA, "80", server.Server)
	send := func() error {
		t.Helper()
		resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
		if err == nil {
			drain(t, resp)
		}
		return err
	}

	// Closed: failures are sent and counted.
	for range 3 {
		if err := send(); err != nil {
			t.Fatalf("Do while closed: %v", err)
		}
	}
	// Open: refused at once, nothing sent, and a body that was not sent is
	// still closed.
	posted := &closeRecorder{Reader: strings.NewReader("{}")}
	req := mustRequest(t, t.Context(), http.MethodPost, "http://api.example.com/")
	req.Body = posted
	if _, err := client.Do(req); !errors.Is(err, ErrCircuitOpen) || !posted.closed {
		t.Errorf("POST while open = %v, body closed %v, want it refused and its body closed", err, posted.closed)
	}
	err := send()
	if !errors.Is(err, ErrCircuitOpen) || !strings.Contains(err.Error(), "http://api.example.com:80") ||
		!strings.Contains(err.Error(), "3 failures") {
		t.Fatalf("Do while open = %v, want ErrCircuitOpen naming the host", err)
	}
	if hits := server.hits.Load(); hits != 3 {
		t.Errorf("hits = %d, want the open circuit to send nothing", hits)
	}

	// Half open after the cooldown: one probe, which fails and reopens it.
	clock.Advance(time.Minute)
	if err := send(); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := send(); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("after a failed probe = %v, want the circuit open again", err)
	}

	// A successful probe closes it and forgets the host.
	clock.Advance(time.Minute)
	status.Store(http.StatusOK)
	if err := send(); err != nil {
		t.Fatalf("successful probe: %v", err)
	}
	if err := send(); err != nil {
		t.Errorf("after a successful probe = %v, want the circuit closed", err)
	}
	if size := client.breakers.size(); size != 0 {
		t.Errorf("the breaker remembers %d hosts, want a healthy host forgotten", size)
	}
}

func TestClientCircuitBreakerIsNotRetriedAndCountsConnectionErrors(t *testing.T) {
	t.Parallel()
	server := statusServer(t, nil, 200)
	client, network := newTestClient(t, ClientOptions{CircuitBreaker: CircuitBreakerOptions{Threshold: 2}})
	network.serve("api.example.com", publicA, "80", server.Server)
	network.failures.Store(100)
	_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	// Two connection errors open the circuit, and the third attempt meets it
	// and stops rather than being retried into it.
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Do error = %v, want the circuit to open part way", err)
	}
	if dialled := len(network.connections()); dialled != 2 {
		t.Errorf("connections = %d, want 2", dialled)
	}
}

func TestClientCircuitBreakerIgnoresWhatSaysNothingAboutTheHost(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	client, network := newTestClient(t, ClientOptions{MaxAttempts: 1, CircuitBreaker: CircuitBreakerOptions{Threshold: 1}})
	network.serve("api.example.com", publicA, "80", server.Server)
	for range 3 {
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(20*time.Millisecond, cancel)
		_, err := client.Do(mustRequest(t, ctx, http.MethodGet, "http://api.example.com/"))
		cancel()
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatal("a caller giving up opened the circuit")
		}
	}

	// A host that hangs until the deadline passes has failed, and is the
	// host a breaker is most for.
	hanging, network := newTestClient(t, ClientOptions{
		MaxAttempts: 1, Timeout: 20 * time.Millisecond, CircuitBreaker: CircuitBreakerOptions{Threshold: 1},
	})
	network.serve("api.example.com", publicA, "80", server.Server)
	if _, err := hanging.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do against a hanging host = %v, want the timeout", err)
	}
	if _, err := hanging.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/")); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("Do after a timeout = %v, want the circuit open", err)
	}
	// A refused address is the client's decision, not the host's failure.
	set := newBreakerSet(CircuitBreakerOptions{Threshold: 1})
	req := mustRequest(t, t.Context(), http.MethodGet, "http://x.example.com/")
	if outcome := breakerOutcomeOf(req, nil, &AddressRefusedError{}); outcome != outcomeNeutral {
		t.Errorf("outcome of a refusal = %v, want neutral", outcome)
	}
	set.record("k", false, outcomeNeutral)
	if set.size() != 0 {
		t.Error("a neutral outcome was remembered")
	}
}

func TestBreakerHalfOpenAdmitsOneProbeUnderConcurrency(t *testing.T) {
	t.Parallel()
	set := newBreakerSet(CircuitBreakerOptions{Threshold: 1, Cooldown: time.Second})
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	set.now = clock.Now
	set.record("https://api.example.com:443", false, outcomeFailure)
	clock.Advance(time.Second)

	var probes, refused atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			probe, err := set.admit("https://api.example.com:443")
			switch {
			case probe && err == nil:
				probes.Add(1)
			case errors.Is(err, ErrCircuitOpen):
				refused.Add(1)
			}
		})
	}
	wg.Wait()
	if probes.Load() != 1 || refused.Load() != 199 {
		t.Fatalf("probes = %d, refused = %d, want exactly one probe and the rest refused", probes.Load(), refused.Load())
	}

	// A probe that ends neutrally frees the slot for the next one.
	set.record("https://api.example.com:443", true, outcomeNeutral)
	if probe, err := set.admit("https://api.example.com:443"); !probe || err != nil {
		t.Errorf("admit after a neutral probe = %v, %v, want a new probe", probe, err)
	}
	// A late failure from a request admitted before the circuit opened does
	// not extend the cooldown.
	set.record("https://api.example.com:443", false, outcomeFailure)
	if probe, err := set.admit("https://api.example.com:443"); probe || !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("admit while the probe is out = %v, %v, want a refusal", probe, err)
	}
}

func TestBreakerUnderConcurrentTraffic(t *testing.T) {
	t.Parallel()
	set := newBreakerSet(CircuitBreakerOptions{Threshold: 5, Cooldown: time.Millisecond, MaxHosts: 16})
	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Go(func() {
			for i := range 500 {
				key := "http://host" + strconv.Itoa((worker+i)%40) + ":80"
				probe, err := set.admit(key)
				if err != nil {
					continue
				}
				set.record(key, probe, breakerOutcome(i%3))
			}
		})
	}
	wg.Wait()
	if size := set.size(); size > 16 {
		t.Errorf("size = %d, want it within MaxHosts", size)
	}
}

func TestBreakerMapIsBounded(t *testing.T) {
	t.Parallel()
	set := newBreakerSet(CircuitBreakerOptions{Threshold: 3, MaxHosts: 8})
	start := time.Now()
	for i := range 10_000 {
		set.record("http://h"+strconv.Itoa(i)+".example.com:80", false, outcomeFailure)
		if size := set.size(); size > 8 {
			t.Fatalf("size = %d after %d hosts, want at most 8", size, i+1)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("10000 failing hosts took %v, want each one constant time", elapsed)
	}
	// The most recently failing hosts are the ones kept.
	set.mu.Lock()
	_, newest := set.hosts["http://h9999.example.com:80"]
	_, oldest := set.hosts["http://h0.example.com:80"]
	set.mu.Unlock()
	if !newest || oldest {
		t.Errorf("newest kept = %v, oldest kept = %v, want least recently used evicted", newest, oldest)
	}
	if def := newBreakerSet(CircuitBreakerOptions{Threshold: 1}); def.options.MaxHosts != DefaultCircuitMaxHosts ||
		def.options.Cooldown != DefaultCircuitCooldown {
		t.Errorf("defaults = %+v, want the documented ones", def.options)
	}
}

func TestClientCircuitBreakerKeysEachRedirectHop(t *testing.T) {
	t.Parallel()
	failing := statusServer(t, nil, 503)
	origin := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://downstream.example.com/", http.StatusFound)
	})
	client, network := newTestClient(t, ClientOptions{MaxAttempts: 1, CircuitBreaker: CircuitBreakerOptions{Threshold: 1}})
	network.serve("origin.example.com", publicA, "80", origin.Server)
	network.serve("downstream.example.com", publicB, "80", failing.Server)
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://origin.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	_, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://origin.example.com/"))
	if !errors.Is(err, ErrCircuitOpen) || !strings.Contains(err.Error(), "downstream.example.com") {
		t.Errorf("Do error = %v, want the downstream host's circuit open", err)
	}
	if origin.hits.Load() != 2 {
		t.Errorf("origin hits = %d, want the origin's own circuit closed", origin.hits.Load())
	}
	if key := breakerKey(&url.URL{Scheme: "https", Host: "[::1]"}); key != "https://[::1]:443" {
		t.Errorf("breakerKey = %q, want the default port filled in", key)
	}
}
