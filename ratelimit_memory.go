package badele

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Defaults applied to a memory rate limit storage when
// [MemoryRateLimitOptions] leaves them unset.
const (
	// DefaultRateLimitMaxEntries is how many counters one storage holds before
	// it starts discarding them, at 100000. A counter is a short key and two
	// words, so a full table costs a few megabytes, which is a bound worth
	// having: the table is keyed by something the client influences, so
	// without one the limiter becomes the exhaustion it was added to prevent.
	DefaultRateLimitMaxEntries = 100_000
	// DefaultRateLimitSweepInterval is how often expired counters are
	// discarded, at one minute.
	DefaultRateLimitSweepInterval = time.Minute
)

// MemoryRateLimitOptions configures [NewMemoryRateLimitStorage].
type MemoryRateLimitOptions struct {
	// MaxEntries is how many counters the storage holds at once, defaulting to
	// [DefaultRateLimitMaxEntries]. Once it is full, admitting a counter
	// discards the one closest to expiring, which is the one whose loss costs
	// least. A negative value removes the bound, which is only appropriate
	// when the set of keys is known to be small and closed.
	MaxEntries int

	// SweepInterval is how often expired counters are discarded, defaulting to
	// [DefaultRateLimitSweepInterval]. Sweeping is housekeeping rather than
	// safety, since MaxEntries is what actually bounds the table, and it
	// happens only while the storage is started. A negative value turns it
	// off.
	SweepInterval time.Duration
}

// MemoryRateLimitStorage counts requests in the process that serves them.
//
// It is what an application that names no storage of its own gets, and it is
// the right answer for a single process. It is the wrong answer for several:
// counters held in one process are not shared with the next, so a limit of a
// hundred a minute becomes a hundred a minute per process. Reach for a storage
// backed by something the processes share once there is more than one.
//
// The table is bounded in two ways, because it is keyed by something the
// client influences and an unbounded one would be a memory leak with a name.
// Expired counters are swept periodically, and a table at
// [MemoryRateLimitOptions.MaxEntries] discards the counter closest to expiring
// to make room for a new one.
//
// It implements [Lifecycle], so an application that uses it starts and stops
// it as part of its own start-up and shutdown. Stopping releases every counter
// it holds, so no key outlives the server that was counting it.
type MemoryRateLimitStorage struct {
	maxEntries int
	sweepEvery time.Duration
	// now is the clock, replaceable so that a test can move time without
	// waiting for it.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*memoryCounter
	expiry  memoryExpiryHeap
	// stop is closed to end the sweeper and done is closed once it has ended,
	// so that Stop returns only when nothing of this storage is still running.
	stop chan struct{}
	done chan struct{}
}

// memoryCounter is one client's count within one quota's window.
type memoryCounter struct {
	key       string
	count     int
	expiresAt time.Time
	// index is where this counter sits in the expiry heap, maintained by the
	// heap itself so that a counter whose window restarted can be moved
	// without searching for it.
	index int
}

// NewMemoryRateLimitStorage returns a rate limit storage that counts in
// memory.
//
//	app := badele.New(badele.AppOptions{Title: "Shop"},
//		badele.WithRateLimit(badele.RateLimitOptions{
//			Storage: badele.NewMemoryRateLimitStorage(badele.MemoryRateLimitOptions{MaxEntries: 10_000}),
//			Quotas:  []badele.Quota{{Name: "default", Window: time.Minute, Limit: 60}},
//		}),
//	)
//
// Naming one is only necessary to change its bounds; a policy that leaves
// [RateLimitOptions.Storage] unset is given one of these with its defaults.
func NewMemoryRateLimitStorage(opts MemoryRateLimitOptions) *MemoryRateLimitStorage {
	maxEntries := opts.MaxEntries
	switch {
	case maxEntries == 0:
		maxEntries = DefaultRateLimitMaxEntries
	case maxEntries < 0:
		maxEntries = 0
	}
	return &MemoryRateLimitStorage{
		maxEntries: maxEntries,
		sweepEvery: orDefaultDuration(opts.SweepInterval, DefaultRateLimitSweepInterval),
		now:        time.Now,
		entries:    make(map[string]*memoryCounter),
	}
}

// Name implements [Lifecycle].
func (s *MemoryRateLimitStorage) Name() string { return "ratelimit-memory" }

// Start begins sweeping expired counters. It implements [Lifecycle] and is
// idempotent, so a storage that has been registered twice is still swept by
// exactly one goroutine.
//
// The context is deliberately not watched. A lifecycle Start is given a
// context that is cancelled as soon as start-up finishes, which is what lets a
// failing component abandon its siblings, so a sweeper that honoured it would
// stop the moment the server began serving.
func (s *MemoryRateLimitStorage) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil || s.sweepEvery <= 0 {
		return nil
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.sweep(s.stop, s.done, s.sweepEvery)
	return nil
}

// Stop ends the sweeper and discards every counter held. It implements
// [Lifecycle] and is idempotent.
//
// Releasing the counters matters beyond the memory: a key is derived from
// whatever the tracker read, an address, a user identifier or an API key, and
// none of that should outlive the server that was counting it.
func (s *MemoryRateLimitStorage) Stop(context.Context) error {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop, s.done = nil, nil
	s.entries = make(map[string]*memoryCounter)
	s.expiry = nil
	s.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}
	return nil
}

// Len reports how many counters the storage is holding, which is what a metric
// or a test asking whether anything is accumulating wants.
func (s *MemoryRateLimitStorage) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Increment implements [RateLimitStorage].
//
// A counter whose window has run out is reused rather than replaced, so a key
// that keeps being seen does not churn the table, and the window is only ever
// set when a counter starts: extending it on every request would produce a
// limit that never resets.
func (s *MemoryRateLimitStorage) Increment(_ context.Context, quota, key string, window time.Duration) (int, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	stored := memoryKey(quota, key)
	if counter, held := s.entries[stored]; held {
		if now.Before(counter.expiresAt) {
			counter.count++
			return counter.count, counter.expiresAt.Sub(now), nil
		}
		counter.count = 1
		counter.expiresAt = now.Add(window)
		heap.Fix(&s.expiry, counter.index)
		return 1, window, nil
	}

	s.makeRoom(now)
	counter := &memoryCounter{key: stored, count: 1, expiresAt: now.Add(window)}
	s.entries[stored] = counter
	heap.Push(&s.expiry, counter)
	return 1, window, nil
}

// memoryKey joins a quota and a client key into the one string the table is
// indexed by. The separator is a NUL byte because a quota name is a token and
// so can never contain one, which is what keeps two different pairs from
// colliding into one counter.
func memoryKey(quota, key string) string {
	return quota + "\x00" + key
}

// makeRoom discards counters until there is space for another, taking the
// expired ones first and then, if the table is still full, the one closest to
// expiring.
//
// Discarding a counter that has not expired means its client is counted from
// zero again, which is a real loss of enforcement. It happens only when the
// table is full, which takes more distinct keys than an application usually
// sees, and the alternative is holding every key an attacker cares to invent.
func (s *MemoryRateLimitStorage) makeRoom(now time.Time) {
	if s.maxEntries <= 0 {
		return
	}
	s.expireLocked(now)
	for len(s.entries) >= s.maxEntries {
		next := s.expiry[0]
		heap.Pop(&s.expiry)
		delete(s.entries, next.key)
	}
}

// expireLocked discards every counter whose window has run out.
func (s *MemoryRateLimitStorage) expireLocked(now time.Time) {
	for len(s.expiry) > 0 && !now.Before(s.expiry[0].expiresAt) {
		next := s.expiry[0]
		heap.Pop(&s.expiry)
		delete(s.entries, next.key)
	}
}

// sweep discards expired counters until the storage is stopped.
func (s *MemoryRateLimitStorage) sweep(stop, done chan struct{}, every time.Duration) {
	defer close(done)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.expireLocked(s.now())
			s.mu.Unlock()
		case <-stop:
			return
		}
	}
}

// memoryExpiryHeap orders counters by when their window ends, so that the one
// to discard first is always at the front.
type memoryExpiryHeap []*memoryCounter

func (h memoryExpiryHeap) Len() int { return len(h) }

func (h memoryExpiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }

func (h memoryExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

// Push implements [container/heap.Interface]. It is called by the heap and
// never directly.
func (h *memoryExpiryHeap) Push(x any) {
	counter := x.(*memoryCounter)
	counter.index = len(*h)
	*h = append(*h, counter)
}

// Pop implements [container/heap.Interface]. The popped counter's slot is
// cleared so that a discarded counter is not kept alive by the slice's backing
// array.
func (h *memoryExpiryHeap) Pop() any {
	old := *h
	last := len(old) - 1
	counter := old[last]
	old[last] = nil
	counter.index = -1
	*h = old[:last]
	return counter
}
