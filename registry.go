package badele

import (
	"sync"
	"time"
)

// liveRegistry tracks the long-lived responses an application is still
// serving, so that a graceful shutdown can end them and so that there is a
// bound on how many of them one process holds at once.
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
	limit    int
	entries  map[T]struct{}
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
)

// admits reports whether another entry can be taken, without taking one.
//
// It exists for the callers that have to refuse before they commit: a
// WebSocket handshake can only be answered with an ordinary error response
// before the connection is hijacked, and by then there is no response left.
func (g *liveRegistry[T]) admits() admission {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.admitsLocked()
}

// admitsLocked is admits with the lock already held.
func (g *liveRegistry[T]) admitsLocked() admission {
	switch {
	case g.draining:
		return registryDraining
	case g.limit > 0 && len(g.entries) >= g.limit:
		return registryFull
	default:
		return admitted
	}
}

// add records a new entry, reporting why it was refused when the application
// has stopped admitting them. Checking and recording under one lock is what
// keeps two handshakes arriving together from both passing a limit of one.
func (g *liveRegistry[T]) add(v T) admission {
	g.mu.Lock()
	defer g.mu.Unlock()
	if refused := g.admitsLocked(); refused != admitted {
		return refused
	}
	if g.entries == nil {
		g.entries = make(map[T]struct{})
	}
	g.entries[v] = struct{}{}
	return admitted
}

// remove forgets an entry whose handler has finished, and tells a waiting
// shutdown when it was the last one.
func (g *liveRegistry[T]) remove(v T) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, v)
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
