package muzak

import (
	"container/heap"
	"context"
	"math"
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
	// discards one from the same quota, the one closest to expiring, which is
	// the one whose loss costs least. Keeping the loss inside the quota being
	// counted is what stops a client who can mint keys under one quota (an API
	// key or a username counted before any guard, or an address per request
	// from its own range) from pushing out another quota's counters, a
	// per-account login limit or its own spent per-address budget among them,
	// and so resetting them. Only a quota holding no counter at all takes the
	// one it needs from the quota holding the most. A full table therefore
	// leaves each quota the room it had when it filled, and a quota whose
	// traffic grows after that gains room only as counters expire; size the
	// table for the keys the application really sees. A negative value removes
	// the bound, which is only appropriate when the set of keys is known to be
	// small and closed.
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
// [MemoryRateLimitOptions.MaxEntries] makes room for a new counter by
// discarding the one closest to expiring from the quota the new counter
// belongs to. Flooding one quota with fresh keys therefore only ever displaces
// that quota's own counters, beyond the single counter a quota holding none
// takes from the largest to be counted at all. Within a single quota it still
// can, which is why a quota whose keys a client chooses freely (a username on
// a login form) is best paired with one it cannot, such as its address.
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
	// partitions holds each quota's counters in the order they expire, so
	// that eviction can be confined to one quota. A quota with no counters
	// has no entry, so names a resolver stops producing do not accumulate.
	partitions map[string]*memoryExpiryHeap
	// stop is closed to end the sweeper and done is closed once it has ended,
	// so that Stop returns only when nothing of this storage is still running.
	stop chan struct{}
	done chan struct{}
}

// memoryCounter is one client's count within one quota's window.
type memoryCounter struct {
	key       string
	quota     string
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
//	app := muzak.New(muzak.AppOptions{Title: "Shop"},
//		muzak.WithRateLimit(muzak.RateLimitOptions{
//			Storage: muzak.NewMemoryRateLimitStorage(muzak.MemoryRateLimitOptions{MaxEntries: 10_000}),
//			Quotas:  []muzak.Quota{{Name: "default", Window: time.Minute, Limit: 60}},
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
		partitions: make(map[string]*memoryExpiryHeap),
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
	s.partitions = make(map[string]*memoryExpiryHeap)
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
			// A count that reaches the top of an int stays there rather than
			// wrapping to a negative one, which no limit would exceed.
			if counter.count < math.MaxInt {
				counter.count++
			}
			return counter.count, counter.expiresAt.Sub(now), nil
		}
		counter.count = 1
		counter.expiresAt = now.Add(window)
		heap.Fix(s.partitions[quota], counter.index)
		return 1, window, nil
	}

	s.makeRoom(now, quota)
	counter := &memoryCounter{key: stored, quota: quota, count: 1, expiresAt: now.Add(window)}
	s.entries[stored] = counter
	partition := s.partitions[quota]
	if partition == nil {
		partition = &memoryExpiryHeap{}
		s.partitions[quota] = partition
	}
	heap.Push(partition, counter)
	return 1, window, nil
}

// memoryKey joins a quota and a client key into the one string the table is
// indexed by. The separator is a NUL byte because a quota name is a token and
// so can never contain one, which is what keeps two different pairs from
// colliding into one counter.
func memoryKey(quota, key string) string {
	return quota + "\x00" + key
}

// makeRoom discards counters until there is space for one more under quota,
// taking the expired ones first and then, if the table is still full, the one
// closest to expiring in quota's own partition. Only a quota that holds no
// counter at all takes one from another, the quota that holds the most.
//
// Discarding a counter that has not expired means its client is counted from
// zero again, which is a real loss of enforcement. It happens only when the
// table is full, which takes more distinct keys than an application usually
// sees, and the alternative is holding every key an attacker cares to invent.
// Taking it from the quota being counted puts that loss where the new key
// came from, and that is the only placement a client cannot turn to its own
// use. A client can usually mint keys under some quota, an API key or a
// username counted before any guard has looked at it, and a new key that
// discarded another quota's oldest counter would let it reset its own counter
// there: spend an application-wide per-address quota early, so that its
// counter is the oldest, then mint keys elsewhere until that counter goes.
// Discarding from the largest quota, as this once did, did exactly that
// whenever the quota being minted was not the largest.
//
// The cost is that a full table no longer lets a small quota grow at the
// expense of a large one: each keeps the room it held when the table filled,
// and gains more only as counters expire. A quota in steady use already holds
// about as many counters as its traffic needs, so this matters only to one
// whose traffic grows while the table stays full, which is a table too small
// for the application or one being flooded, and in either case the counters a
// quota already holds are never taken by another's keys.
//
// A quota with nothing to give up, one that is new or whose counters have all
// expired, still has to be admitted, or its first client would never be
// counted. It takes the one counter it needs from the largest quota, which is
// the only way a key ever displaces another quota's counter, and which it can
// do only once before it holds a counter of its own to give up instead. The
// quotas are few, so finding the largest is a short scan.
func (s *MemoryRateLimitStorage) makeRoom(now time.Time, quota string) {
	if s.maxEntries <= 0 {
		return
	}
	s.expireLocked(now)
	for len(s.entries) >= s.maxEntries {
		if s.partitions[quota].Len() > 0 {
			s.discardFirst(quota)
			continue
		}
		var largest string
		for name, partition := range s.partitions {
			// Ties go to the lexically first name, so which quota loses a
			// counter does not depend on map iteration order.
			if held, most := partition.Len(), s.partitions[largest].Len(); held > most || (held == most && name < largest) {
				largest = name
			}
		}
		s.discardFirst(largest)
	}
}

// expireLocked discards every counter whose window has run out.
func (s *MemoryRateLimitStorage) expireLocked(now time.Time) {
	for name, partition := range s.partitions {
		for partition.Len() > 0 && !now.Before((*partition)[0].expiresAt) {
			s.discardFirst(name)
		}
	}
}

// discardFirst removes the counter closest to expiring from one quota, and the
// quota itself once it holds nothing, so that a quota name that is no longer
// in use costs nothing.
func (s *MemoryRateLimitStorage) discardFirst(quota string) {
	partition := s.partitions[quota]
	next := heap.Pop(partition).(*memoryCounter)
	delete(s.entries, next.key)
	if partition.Len() == 0 {
		delete(s.partitions, quota)
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

// Len implements [container/heap.Interface]. It is defined on the pointer, as
// every other use of a partition is, so that a quota with no partition yet
// reads as empty rather than as a nil dereference.
func (h *memoryExpiryHeap) Len() int {
	if h == nil {
		return 0
	}
	return len(*h)
}

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
