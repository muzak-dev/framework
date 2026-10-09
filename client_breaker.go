package muzak

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults applied to [CircuitBreakerOptions] once a Threshold turns the
// breaker on.
const (
	// DefaultCircuitCooldown is how long an open circuit refuses requests
	// before it lets one through to test the host, at thirty seconds.
	DefaultCircuitCooldown = 30 * time.Second
	// DefaultCircuitMaxHosts is how many failing hosts a client remembers at
	// once.
	DefaultCircuitMaxHosts = 1024
)

// CircuitBreakerOptions configures the circuit breaker of a [Client], which
// stops sending requests to a host that keeps failing.
//
// A host that is down answers slowly or not at all, and every caller that
// waits on it holds a goroutine, a connection and the request it is serving
// for as long as the timeout allows. The breaker counts each host's failures
// in a row, a connection error or a 5xx response, and once Threshold is
// reached it refuses requests to that host at once with [ErrCircuitOpen] for
// Cooldown. Then it lets one request through: if that succeeds the circuit
// closes, and if it fails the circuit stays open for another Cooldown. While
// that one request is out, every other one is refused rather than piling onto
// a host that has not yet shown it is back.
//
// A host is its scheme, name and port, so each step of a redirect counts
// against the host it reached.
type CircuitBreakerOptions struct {
	// Threshold is how many failures in a row open a host's circuit. Zero, the
	// default, leaves the breaker off and costs nothing per request.
	Threshold int

	// Cooldown is how long an open circuit refuses requests before it tests
	// the host again, defaulting to [DefaultCircuitCooldown].
	Cooldown time.Duration

	// MaxHosts bounds how many hosts the breaker remembers, defaulting to
	// [DefaultCircuitMaxHosts]. Only hosts that are failing are remembered,
	// since one that succeeds is forgotten, and the least recently used is
	// forgotten first once the bound is reached. Forgetting an open circuit
	// closes it, so a client that calls more failing hosts than this at once
	// protects the most active of them.
	MaxHosts int
}

// ErrCircuitOpen reports a request the client refused to send because the
// circuit to its host is open. Nothing was sent, and the request is not
// retried. Test for it with [errors.Is].
var ErrCircuitOpen = errors.New("muzak: the circuit to the host is open")

// circuitOpenError is the error a refused request reports, naming the host.
type circuitOpenError struct {
	origin    string
	threshold int
}

func (e *circuitOpenError) Error() string {
	return fmt.Sprintf("muzak: the circuit to %s is open after %d failures in a row, so the request was not sent; it will be tried again once the cooldown has passed",
		e.origin, e.threshold)
}

// Is makes the error match [ErrCircuitOpen].
func (e *circuitOpenError) Is(target error) bool { return target == ErrCircuitOpen }

// breakerState is where one host's circuit stands.
type breakerState uint8

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// breakerOutcome is what one round trip says about its host.
type breakerOutcome uint8

const (
	// outcomeSuccess is an answer that is not a server error.
	outcomeSuccess breakerOutcome = iota
	// outcomeFailure is a server error or a connection that failed.
	outcomeFailure
	// outcomeNeutral says nothing about the host: the caller gave up, or the
	// client refused the address before connecting.
	outcomeNeutral
)

// breakerEntry is one failing host.
type breakerEntry struct {
	key      string
	failures int
	state    breakerState
	reopen   time.Time
	probing  bool
}

// breakerSet holds the circuits of the hosts that are failing, bounded and
// evicted least recently used first. Every operation is a map lookup and a
// list move under one lock, so its cost does not depend on how many hosts it
// holds.
type breakerSet struct {
	mu      sync.Mutex
	hosts   map[string]*list.Element
	order   list.List
	options CircuitBreakerOptions
	// now is the clock cooldowns are measured on, a field so that a test can
	// move it.
	now func() time.Time
}

// newBreakerSet builds the breaker a set of options describes.
func newBreakerSet(opts CircuitBreakerOptions) *breakerSet {
	if opts.Cooldown <= 0 {
		opts.Cooldown = DefaultCircuitCooldown
	}
	if opts.MaxHosts <= 0 {
		opts.MaxHosts = DefaultCircuitMaxHosts
	}
	return &breakerSet{hosts: make(map[string]*list.Element), options: opts, now: time.Now}
}

// admit decides whether a request to a host may be sent, and reports whether
// it is the one request testing a circuit that has cooled down.
func (s *breakerSet) admit(key string) (probe bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	element := s.hosts[key]
	if element == nil {
		return false, nil
	}
	s.order.MoveToFront(element)
	entry := element.Value.(*breakerEntry)
	if entry.state == breakerOpen {
		if s.now().Before(entry.reopen) {
			return false, &circuitOpenError{origin: key, threshold: s.options.Threshold}
		}
		entry.state = breakerHalfOpen
	}
	if entry.state == breakerHalfOpen {
		if entry.probing {
			return false, &circuitOpenError{origin: key, threshold: s.options.Threshold}
		}
		entry.probing = true
		return true, nil
	}
	return false, nil
}

// record applies the outcome of a request admit let through.
//
// A success forgets the host, which is what closes its circuit. A failure
// counts towards the threshold, and a failed probe opens the circuit again
// for another cooldown. A neutral outcome changes nothing, except that a
// probe that ended that way frees the slot for the next request to probe
// with.
func (s *breakerSet) record(key string, probe bool, outcome breakerOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	element := s.hosts[key]
	switch outcome {
	case outcomeSuccess:
		if element != nil {
			s.order.Remove(element)
			delete(s.hosts, key)
		}
		return
	case outcomeNeutral:
		if element != nil && probe {
			element.Value.(*breakerEntry).probing = false
		}
		return
	}
	if element == nil {
		element = s.insert(key)
	} else {
		s.order.MoveToFront(element)
	}
	entry := element.Value.(*breakerEntry)
	switch {
	case probe:
		entry.state, entry.probing, entry.reopen = breakerOpen, false, s.now().Add(s.options.Cooldown)
	case entry.state == breakerClosed:
		entry.failures++
		if entry.failures >= s.options.Threshold {
			entry.state, entry.reopen = breakerOpen, s.now().Add(s.options.Cooldown)
		}
	}
}

// insert adds a host, forgetting the least recently used one if the set is
// full.
func (s *breakerSet) insert(key string) *list.Element {
	if len(s.hosts) >= s.options.MaxHosts {
		oldest := s.order.Back()
		s.order.Remove(oldest)
		delete(s.hosts, oldest.Value.(*breakerEntry).key)
	}
	element := s.order.PushFront(&breakerEntry{key: key})
	s.hosts[key] = element
	return element
}

// size reports how many hosts the set remembers.
func (s *breakerSet) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hosts)
}

// breakerTransport puts the breaker in front of the transport, where it sees
// every round trip, including each step of a redirect.
type breakerTransport struct {
	next     http.RoundTripper
	breakers *breakerSet
}

// RoundTrip refuses a request to a host whose circuit is open, and otherwise
// sends it and records what the answer says about the host.
func (t *breakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := breakerKey(req.URL)
	probe, err := t.breakers.admit(key)
	if err != nil {
		// A RoundTripper closes the request body whatever happens, and this
		// one is not going to send it.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	t.breakers.record(key, probe, breakerOutcomeOf(req, resp, err))
	return resp, err
}

// breakerOutcomeOf judges one round trip.
//
// A call that was cancelled says nothing about the host, since the caller
// went away, but one whose deadline passed does: a host that hangs is the one
// a breaker is most for, and it fails by running out the clock rather than by
// answering.
func breakerOutcomeOf(req *http.Request, resp *http.Response, err error) breakerOutcome {
	if err != nil {
		var refused *AddressRefusedError
		if errors.Is(req.Context().Err(), context.Canceled) || errors.As(err, &refused) {
			return outcomeNeutral
		}
		return outcomeFailure
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return outcomeFailure
	}
	return outcomeSuccess
}

// breakerKey names the host a request goes to: its scheme, name and port.
func breakerKey(u *url.URL) string {
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), originPort(u))
}
