package muzak

import (
	"errors"
	"sync"
	"time"
)

// liveRegistry tracks the long-lived responses an application is still
// serving, so that a graceful shutdown can end them and so that there is a
// bound on how many of them one process holds at once, and optionally on how
// many of them any single client holds.
//
// Two kinds of response outlive the request that began them. A hijacked
// WebSocket connection is no longer one net/http knows about, so nothing else
// would ever tell those peers that the server is going away. An open event
// stream is one net/http does know about, which is worse: a shutdown would
// wait for every one of them until the deadline, because a handler that is
// still streaming is a handler that has not returned.
//
// The type parameter is the thing being held. The registry counts and lists;
// what it means to end one of them is passed in, because a connection says
// goodbye in a way a stream cannot.
type liveRegistry[T comparable] struct {
	mu sync.Mutex
	// limit is how many entries may be held at once, and zero means as many as
	// the process can carry.
	limit int
	// perKeyLimit is how many entries a single key may hold at once, and zero
	// turns the dimension off entirely: no key is ever recorded or checked,
	// so a registry that never configures one pays nothing beyond the plain
	// process-wide limit above.
	perKeyLimit int
	entries     map[T]struct{}
	// keyOf and perKey exist only while perKeyLimit is positive. keyOf
	// recovers the key an entry was admitted under, which is what lets
	// remove find the right counter to decrement without its caller having
	// to remember and repeat a key that was only ever relevant at admission
	// time; perKey holds each key's current count.
	keyOf  map[T]string
	perKey map[string]int

	draining bool
	// drained is closed by the last entry to go, which is what a shutdown
	// waits on. It is a channel rather than a wait group because a shutdown
	// that gives up must leave nothing behind waiting.
	drained chan struct{}
}

// admission says whether another entry can be taken, and why not when it
// cannot.
//
// The reason is reported rather than turned into an error here, because the
// error has to name what was refused, and a refused connection and a refused
// stream do not read the same.
type admission uint8

const (
	// admitted reports room for another entry.
	admitted admission = iota
	// registryDraining reports an application that is shutting down.
	registryDraining
	// registryFull reports an application already holding as many entries as
	// it is configured to.
	registryFull
	// registryKeyFull reports a single key already holding as many entries as
	// perKeyLimit allows, distinct from the process as a whole being full.
	registryKeyFull
)

// admits reports whether another entry can be taken, without taking one.
//
// It exists for the callers that have to refuse before they commit: a
// WebSocket handshake can only be answered with an ordinary error response
// before the connection is hijacked, and by then there is no response left.
// key is the entry's key for the per-key dimension, or the empty string when
// the caller has none to offer; see [liveRegistry.add].
func (g *liveRegistry[T]) admits(key string) admission {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.admitsLocked(key)
}

// admitsLocked is admits with the lock already held.
func (g *liveRegistry[T]) admitsLocked(key string) admission {
	switch {
	case g.draining:
		return registryDraining
	case g.limit > 0 && len(g.entries) >= g.limit:
		return registryFull
	case g.perKeyLimit > 0 && key != "" && g.perKey[key] >= g.perKeyLimit:
		return registryKeyFull
	default:
		return admitted
	}
}

// add records a new entry, reporting why it was refused when the application
// has stopped admitting them or the key already holds as many as it may.
// Checking and recording under one lock is what keeps two handshakes arriving
// together from both passing a limit of one.
//
// key identifies the entry for the per-key dimension of the limit. Pass the
// empty string when the caller has no key to offer, or when perKeyLimit is
// not configured at all, in which case it is never even looked at: the empty
// string is never counted against g.limit's per-process budget.
func (g *liveRegistry[T]) add(v T, key string) admission {
	g.mu.Lock()
	defer g.mu.Unlock()
	if refused := g.admitsLocked(key); refused != admitted {
		return refused
	}
	if g.entries == nil {
		g.entries = make(map[T]struct{})
	}
	g.entries[v] = struct{}{}
	if g.perKeyLimit > 0 && key != "" {
		if g.keyOf == nil {
			g.keyOf = make(map[T]string)
			g.perKey = make(map[string]int)
		}
		g.keyOf[v] = key
		g.perKey[key]++
	}
	return admitted
}

// remove forgets an entry whose handler has finished, and tells a waiting
// shutdown when it was the last one.
func (g *liveRegistry[T]) remove(v T) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, v)
	if key, ok := g.keyOf[v]; ok {
		delete(g.keyOf, v)
		if g.perKey[key] <= 1 {
			delete(g.perKey, key)
		} else {
			g.perKey[key]--
		}
	}
	if len(g.entries) == 0 && g.drained != nil {
		close(g.drained)
		g.drained = nil
	}
}

// count reports how many entries are held, which is what a test asserting that
// nothing was left behind asks for.
func (g *liveRegistry[T]) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.entries)
}

// shutdown stops admitting entries, ends every one it holds, and waits for the
// handlers to return, giving up after timeout. It returns how many entries it
// asked to end.
func (g *liveRegistry[T]) shutdown(timeout time.Duration, end func(T)) int {
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	g.mu.Lock()
	g.draining = true
	open := make([]T, 0, len(g.entries))
	for entry := range g.entries {
		open = append(open, entry)
	}
	if len(open) > 0 {
		g.drained = make(chan struct{})
	}
	drained := g.drained
	g.mu.Unlock()
	if len(open) == 0 {
		return 0
	}

	// Ending them concurrently matters: a peer that has stopped reading holds
	// up its own goodbye until the write timeout, and one such peer should not
	// delay everyone else's.
	for _, entry := range open {
		go end(entry)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
	}
	return len(open)
}

// errConnectionLimitNoAddress reports a request that cannot be attributed to
// an address once a per-client connection limit needs one to check against.
//
// It mirrors [errRateLimitNoAddress]: a listener that is not addressed by IP,
// such as one reached only through a proxy that strips forwarding headers,
// needs the per-client dimension turned off with a negative
// MaxConnectionsPerIP or MaxStreamsPerIP rather than have every connection
// refused because none of them can be told apart.
var errConnectionLimitNoAddress = errors.New("muzak: a per-client connection limit is configured but the client address could not be determined; " +
	"set MaxConnectionsPerIP or MaxStreamsPerIP to a negative value to disable it for a listener that is not addressed by IP")

// perClientKey resolves the key a [liveRegistry]'s per-client dimension
// admits an entry under.
//
// It returns the empty string, which liveRegistry treats as "no key" and
// skips the per-client check for entirely, whenever limit is not positive, so
// an application that never configures a per-client limit resolves no
// address and pays nothing for a dimension it does not use.
//
// A client is the prefix [ClientIPOptions.ConnectionIPv6Prefix] and
// [ClientIPOptions.ConnectionIPv4Prefix] choose, by default an IPv6 /56 and an
// exact IPv4 address. Keying on the exact IPv6 address would let a client
// holding a range open a fresh allowance of connections from every address
// in it, which is to say without limit, and keying on the /64 the rate
// limiter uses would still let one home's /56 hold 256 allowances, enough to
// take every slot of the process-wide cap.
func perClientKey(c *Context, limit int) (string, error) {
	if limit <= 0 {
		return "", nil
	}
	resolver := c.clientIPResolver()
	addr := resolver.resolve(c.r)
	if !addr.IsValid() {
		return "", errConnectionLimitNoAddress
	}
	return resolver.connectionKey(addr), nil
}
