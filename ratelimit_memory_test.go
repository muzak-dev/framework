package badele

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// testClock is a clock a test moves by hand, so that a window can be waited
// out without waiting.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Now()}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newTestStorage returns a memory storage on a clock the test controls.
func newTestStorage(t *testing.T, opts MemoryRateLimitOptions) (*MemoryRateLimitStorage, *testClock) {
	t.Helper()
	clock := newTestClock()
	storage := NewMemoryRateLimitStorage(opts)
	storage.now = clock.now
	return storage, clock
}

// increment counts one request and fails the test if the storage could not.
func increment(t *testing.T, s RateLimitStorage, quota, key string, window time.Duration) (int, time.Duration) {
	t.Helper()
	count, reset, err := s.Increment(context.Background(), quota, key, window)
	if err != nil {
		t.Fatalf("Increment(%q, %q) = %v", quota, key, err)
	}
	return count, reset
}

func TestMemoryStorageCountsWithinAWindow(t *testing.T) {
	t.Parallel()
	storage, clock := newTestStorage(t, MemoryRateLimitOptions{})

	count, reset := increment(t, storage, "short", "ip:1.2.3.4", time.Minute)
	if count != 1 || reset != time.Minute {
		t.Fatalf("first Increment = (%d, %s), want (1, 1m0s)", count, reset)
	}

	clock.advance(20 * time.Second)
	count, reset = increment(t, storage, "short", "ip:1.2.3.4", time.Minute)
	if count != 2 {
		t.Errorf("second Increment count = %d, want 2", count)
	}
	if reset != 40*time.Second {
		t.Errorf("second Increment reset = %s, want 40s; a window must not be extended by the requests it counts", reset)
	}
}

func TestMemoryStorageRestartsAnExpiredWindow(t *testing.T) {
	t.Parallel()
	storage, clock := newTestStorage(t, MemoryRateLimitOptions{})

	increment(t, storage, "short", "ip:1.2.3.4", time.Minute)
	increment(t, storage, "short", "ip:1.2.3.4", time.Minute)
	clock.advance(time.Minute)

	count, reset := increment(t, storage, "short", "ip:1.2.3.4", time.Minute)
	if count != 1 || reset != time.Minute {
		t.Errorf("Increment after the window = (%d, %s), want (1, 1m0s)", count, reset)
	}
	if storage.Len() != 1 {
		t.Errorf("Len() = %d, want the counter to be reused rather than duplicated", storage.Len())
	}
}

func TestMemoryStorageKeepsQuotasAndKeysApart(t *testing.T) {
	t.Parallel()
	storage, _ := newTestStorage(t, MemoryRateLimitOptions{})

	increment(t, storage, "short", "ip:1.2.3.4", time.Minute)
	if count, _ := increment(t, storage, "long", "ip:1.2.3.4", time.Minute); count != 1 {
		t.Errorf("a second quota started at %d, want 1", count)
	}
	if count, _ := increment(t, storage, "short", "ip:5.6.7.8", time.Minute); count != 1 {
		t.Errorf("a second key started at %d, want 1", count)
	}
	// A quota name cannot contain a NUL byte, so no pair of quota and key can
	// be spelled two ways.
	if count, _ := increment(t, storage, "short\x00ip:1.2.3.4", "", time.Minute); count != 1 {
		t.Errorf("a key that imitates the separator collided with a real one at %d, want 1", count)
	}
	if storage.Len() != 4 {
		t.Errorf("Len() = %d, want 4 distinct counters", storage.Len())
	}
}

func TestMemoryStorageDiscardsExpiredCountersToMakeRoom(t *testing.T) {
	t.Parallel()
	storage, clock := newTestStorage(t, MemoryRateLimitOptions{MaxEntries: 4})

	for i := range 4 {
		increment(t, storage, "short", "ip:"+strconv.Itoa(i), time.Minute)
	}
	if storage.Len() != 4 {
		t.Fatalf("Len() = %d, want 4", storage.Len())
	}

	clock.advance(2 * time.Minute)
	increment(t, storage, "short", "ip:fresh", time.Minute)
	if storage.Len() != 1 {
		t.Errorf("Len() = %d, want the expired counters to have made room for the new one", storage.Len())
	}
}

func TestMemoryStorageDiscardsTheSoonestToExpireWhenFull(t *testing.T) {
	t.Parallel()
	storage, _ := newTestStorage(t, MemoryRateLimitOptions{MaxEntries: 3})

	// Three live counters with different windows, so that which one is
	// discarded is decided rather than arbitrary.
	increment(t, storage, "q", "soonest", time.Minute)
	increment(t, storage, "q", "middle", 5*time.Minute)
	increment(t, storage, "q", "latest", 10*time.Minute)

	increment(t, storage, "q", "new", time.Hour)
	if storage.Len() != 3 {
		t.Fatalf("Len() = %d, want the table to stay at its bound", storage.Len())
	}
	if count, _ := increment(t, storage, "q", "soonest", time.Minute); count != 1 {
		t.Errorf("the soonest-to-expire counter survived with count %d, want it discarded", count)
	}
	if count, _ := increment(t, storage, "q", "latest", 10*time.Minute); count != 2 {
		// It was counted once before and once here: the counter that expires
		// last is the one worth keeping.
		t.Errorf("the latest-to-expire counter count = %d, want 2, meaning it was kept", count)
	}
}

func TestMemoryStorageCanBeUnbounded(t *testing.T) {
	t.Parallel()
	storage, _ := newTestStorage(t, MemoryRateLimitOptions{MaxEntries: -1})

	for i := range 100 {
		increment(t, storage, "q", strconv.Itoa(i), time.Minute)
	}
	if storage.Len() != 100 {
		t.Errorf("Len() = %d, want 100; a negative MaxEntries removes the bound", storage.Len())
	}
}

func TestMemoryStorageSweepsExpiredCounters(t *testing.T) {
	t.Parallel()
	storage, clock := newTestStorage(t, MemoryRateLimitOptions{SweepInterval: time.Millisecond})
	if err := storage.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Stop(context.Background()); err != nil {
			t.Errorf("Stop() = %v", err)
		}
	})

	increment(t, storage, "q", "ip:1.2.3.4", time.Minute)
	clock.advance(2 * time.Minute)

	deadline := time.Now().Add(2 * time.Second)
	for storage.Len() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Len() = %d after the sweep interval, want the expired counter discarded", storage.Len())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMemoryStorageLifecycleIsIdempotent(t *testing.T) {
	t.Parallel()
	storage, _ := newTestStorage(t, MemoryRateLimitOptions{SweepInterval: time.Hour})
	if storage.Name() != "ratelimit-memory" {
		t.Errorf("Name() = %q, want %q", storage.Name(), "ratelimit-memory")
	}

	ctx := context.Background()
	for range 3 {
		if err := storage.Start(ctx); err != nil {
			t.Fatalf("Start() = %v", err)
		}
	}
	increment(t, storage, "q", "ip:1.2.3.4", time.Minute)
	for range 3 {
		if err := storage.Stop(ctx); err != nil {
			t.Fatalf("Stop() = %v", err)
		}
	}
	if storage.Len() != 0 {
		t.Errorf("Len() = %d after Stop, want the counters released", storage.Len())
	}
	// Stopping and starting again leaves a usable storage rather than one
	// whose sweeper has gone.
	if err := storage.Start(ctx); err != nil {
		t.Fatalf("Start() after Stop = %v", err)
	}
	if count, _ := increment(t, storage, "q", "ip:1.2.3.4", time.Minute); count != 1 {
		t.Errorf("count after a restart = %d, want 1", count)
	}
	if err := storage.Stop(ctx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

func TestMemoryStorageWithSweepingOff(t *testing.T) {
	t.Parallel()
	storage, _ := newTestStorage(t, MemoryRateLimitOptions{SweepInterval: -1})
	if err := storage.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if storage.stop != nil {
		t.Error("a storage with sweeping off started a sweeper")
	}
	if err := storage.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

func TestMemoryStorageIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	storage := NewMemoryRateLimitStorage(MemoryRateLimitOptions{MaxEntries: 32})

	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				key := fmt.Sprintf("ip:%d", (worker*200+i)%64)
				if _, _, err := storage.Increment(context.Background(), "q", key, time.Minute); err != nil {
					t.Errorf("Increment() = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := storage.Len(); got > 32 {
		t.Errorf("Len() = %d, want the table never to exceed its bound of 32", got)
	}
	if got := len(storage.expiry); got != storage.Len() {
		t.Errorf("the expiry heap holds %d counters and the table holds %d; they must agree", got, storage.Len())
	}
}

func TestMemoryStorageSharedCounterIsExact(t *testing.T) {
	t.Parallel()
	storage := NewMemoryRateLimitStorage(MemoryRateLimitOptions{})

	const goroutines, each = 16, 100
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if _, _, err := storage.Increment(context.Background(), "q", "shared", time.Hour); err != nil {
					t.Errorf("Increment() = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	count, _, err := storage.Increment(context.Background(), "q", "shared", time.Hour)
	if err != nil {
		t.Fatalf("Increment() = %v", err)
	}
	if want := goroutines*each + 1; count != want {
		t.Errorf("count = %d, want %d; a lost increment is a limit that can be raced past", count, want)
	}
}

func TestMemoryStorageDefaults(t *testing.T) {
	t.Parallel()
	storage := NewMemoryRateLimitStorage(MemoryRateLimitOptions{})
	if storage.maxEntries != DefaultRateLimitMaxEntries {
		t.Errorf("maxEntries = %d, want %d", storage.maxEntries, DefaultRateLimitMaxEntries)
	}
	if storage.sweepEvery != DefaultRateLimitSweepInterval {
		t.Errorf("sweepEvery = %s, want %s", storage.sweepEvery, DefaultRateLimitSweepInterval)
	}
}

func TestMemoryExpiryHeapClearsDiscardedSlots(t *testing.T) {
	t.Parallel()
	storage, _ := newTestStorage(t, MemoryRateLimitOptions{MaxEntries: 2})
	increment(t, storage, "q", "a", time.Minute)
	increment(t, storage, "q", "b", 2*time.Minute)
	increment(t, storage, "q", "c", 3*time.Minute)

	// The slice the heap grew to still has room for the counter that was
	// discarded, and holding a reference there would keep a key alive that
	// nothing can reach.
	heapSlice := storage.expiry[:cap(storage.expiry)]
	for i := len(storage.expiry); i < len(heapSlice); i++ {
		if heapSlice[i] != nil {
			t.Errorf("slot %d of the expiry heap still holds a discarded counter", i)
		}
	}
}
