package muzak

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// newTestSessionStore returns a memory store on a clock the test controls.
func newTestSessionStore(maxEntries int) (*MemorySessionStore, *sessionClock) {
	store := NewMemorySessionStore(MemorySessionOptions{MaxEntries: maxEntries})
	clock := newSessionClock()
	store.now = clock.now
	return store, clock
}

// TestMemorySessionStoreOperations covers the contract a store must keep:
// Create refuses a key that exists, Update reports and saves nothing for a
// key that does not, and Delete of a missing key is not an error.
func TestMemorySessionStoreOperations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := newTestSessionStore(0)
	if store.max != DefaultSessionMaxEntries {
		t.Fatalf("the default bound is %d", store.max)
	}
	if found, err := store.Update(ctx, "k", []byte("x"), time.Minute); found || err != nil {
		t.Fatalf("Update of a missing key = %v, %v; want false and nothing saved", found, err)
	}
	if store.Len() != 0 {
		t.Fatal("Update created an entry")
	}
	if err := store.Create(ctx, "k", []byte("one"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "k", []byte("two"), time.Minute); !errors.Is(err, errSessionExists) {
		t.Fatalf("Create of an existing key = %v", err)
	}
	if found, err := store.Update(ctx, "k", []byte("three"), time.Minute); !found || err != nil {
		t.Fatalf("Update of an existing key = %v, %v", found, err)
	}
	if data, found, _ := store.Load(ctx, "k"); !found || string(data) != "three" {
		t.Fatalf("Load = %q, %v", data, found)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("deleting a missing key = %v", err)
	}
	if _, found, _ := store.Load(ctx, "k"); found {
		t.Fatal("a deleted key still loads")
	}
}

// TestMemorySessionStoreCopies shows no caller shares a buffer with the
// store: changing what was saved or what was loaded changes nothing stored.
func TestMemorySessionStoreCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := newTestSessionStore(0)
	data := []byte("original")
	_ = store.Create(ctx, "k", data, time.Minute)
	data[0] = 'X'
	loaded, _, _ := store.Load(ctx, "k")
	loaded[1] = 'Y'
	again, _, _ := store.Load(ctx, "k")
	if string(again) != "original" {
		t.Fatalf("the stored data became %q", again)
	}
}

// TestMemorySessionStoreExpiry covers the TTL: an entry is there until it
// runs out and gone from then on, for Load, Update and Create alike.
func TestMemorySessionStoreExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, clock := newTestSessionStore(0)
	_ = store.Create(ctx, "k", []byte("x"), time.Minute)
	clock.advance(time.Minute - time.Nanosecond)
	if _, found, _ := store.Load(ctx, "k"); !found {
		t.Fatal("an entry is gone before its TTL")
	}
	clock.advance(time.Nanosecond)
	if _, found, _ := store.Load(ctx, "k"); found {
		t.Fatal("an entry loads at its TTL")
	}
	if store.Len() != 0 {
		t.Fatal("an expired entry was kept after it was found expired")
	}
	_ = store.Create(ctx, "k", []byte("x"), time.Minute)
	clock.advance(time.Minute)
	if found, _ := store.Update(ctx, "k", []byte("y"), time.Minute); found {
		t.Fatal("Update revived an expired entry")
	}
	if err := store.Create(ctx, "k", []byte("z"), time.Minute); err != nil {
		t.Fatalf("Create over an expired entry = %v", err)
	}
	// Update gives the entry its new TTL.
	clock.advance(30 * time.Second)
	_, _ = store.Update(ctx, "k", []byte("w"), time.Minute)
	clock.advance(45 * time.Second)
	if _, found, _ := store.Load(ctx, "k"); !found {
		t.Fatal("Update did not extend the entry")
	}
}

// TestMemorySessionStoreSweepsExpiredEntries shows expired entries are
// discarded as the store is written to, without a goroutine and without
// waiting for each to be asked for.
func TestMemorySessionStoreSweepsExpiredEntries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, clock := newTestSessionStore(0)
	for i := range 100 {
		_ = store.Create(ctx, "old"+strconv.Itoa(i), []byte("x"), time.Minute)
	}
	clock.advance(time.Hour)
	for i := range 100 {
		_ = store.Create(ctx, "new"+strconv.Itoa(i), []byte("x"), time.Hour)
	}
	if n := store.Len(); n > 100+memorySweep {
		t.Fatalf("%d entries are held after 100 writes past 100 expired ones", n)
	}
}

// TestMemorySessionStoreBound drives the bound: ten thousand more sessions
// than fit leave the store at its bound, discarding the least recently used,
// and each operation stays constant time however full it is.
func TestMemorySessionStoreBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const bound = 1000
	store, _ := newTestSessionStore(bound)
	for i := range bound {
		_ = store.Create(ctx, strconv.Itoa(i), []byte("x"), time.Hour)
	}
	// Using the oldest keeps it; the second oldest is the first to go.
	_, _, _ = store.Load(ctx, "0")
	_ = store.Create(ctx, "extra", []byte("x"), time.Hour)
	if _, found, _ := store.Load(ctx, "0"); !found {
		t.Error("the entry used most recently was discarded")
	}
	if _, found, _ := store.Load(ctx, "1"); found {
		t.Error("the entry used least recently was kept")
	}
	start := time.Now()
	for i := range 10_000 {
		_ = store.Create(ctx, "flood"+strconv.Itoa(i), []byte("x"), time.Hour)
	}
	if n := store.Len(); n != bound {
		t.Fatalf("the store holds %d entries, over its bound of %d", n, bound)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ten thousand writes to a full store took %s", elapsed)
	}
}

// TestMemorySessionStoreHonoursContext shows a call whose context has ended
// does nothing and says why.
func TestMemorySessionStoreHonoursContext(t *testing.T) {
	t.Parallel()
	store, _ := newTestSessionStore(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Load(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Load = %v", err)
	}
	if err := store.Create(ctx, "k", nil, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("Create = %v", err)
	}
	if _, err := store.Update(ctx, "k", nil, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("Update = %v", err)
	}
	if err := store.Delete(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v", err)
	}
	if store.Len() != 0 {
		t.Error("a cancelled call changed the store")
	}
}

// TestMemorySessionStoreConcurrentWriters runs many writers against one key
// and many against many keys under the race detector: every read sees one
// writer's whole record, never a mixture, and the bound holds throughout.
func TestMemorySessionStoreConcurrentWriters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemorySessionStore(MemorySessionOptions{MaxEntries: 64})
	_ = store.Create(ctx, "shared", bytes.Repeat([]byte{'0'}, 512), time.Hour)
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Go(func() {
			record := bytes.Repeat([]byte{byte('a' + w)}, 512)
			for i := range 200 {
				if _, err := store.Update(ctx, "shared", record, time.Hour); err != nil {
					t.Error(err)
					return
				}
				data, found, _ := store.Load(ctx, "shared")
				if found && !bytes.Equal(data, bytes.Repeat(data[:1], len(data))) {
					t.Errorf("a torn record was read: %q", data)
					return
				}
				key := strconv.Itoa(w) + "-" + strconv.Itoa(i)
				_ = store.Create(ctx, key, record, time.Hour)
				_ = store.Delete(ctx, strconv.Itoa(w)+"-"+strconv.Itoa(i-1))
				if n := store.Len(); n > 64 {
					t.Errorf("the store holds %d entries, over its bound", n)
					return
				}
			}
		})
	}
	wg.Wait()
}
