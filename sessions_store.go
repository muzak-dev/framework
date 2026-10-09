package muzak

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
	"time"
)

// SessionStore keeps sessions on the server, for [SessionOptions.Store].
//
// The cookie then carries only a random 256-bit identifier, and the store
// holds the session under a key derived from it: the SHA-256 digest of the
// identifier, never the identifier itself. A copy of the store's contents, a
// backup or a dump of a cache, therefore holds no cookie anyone could
// present, and since an attacker cannot choose a digest, how long a lookup
// takes says nothing about the identifiers that exist.
//
// The data is opaque: a record this package encodes and decodes, which
// carries its own creation and last-use times and is checked against
// [SessionOptions.IdleTimeout] and [SessionOptions.MaxLifetime] whatever the
// store returns. The TTL each write is given is how long the store should
// keep the entry, and lets it discard what nobody will ask for again.
//
// A write never resurrects a session another request deleted. That is why
// there is no single Save: a new session is created, and an existing one is
// only updated if it still exists. Without the distinction, a request that
// read a session just before the user signed out in another tab would write
// it back as it ended, and the sign-out would not stick. Against Redis, say,
// Create is SET with NX and Update is SET with XX; against SQL, an INSERT and
// an UPDATE whose row count is checked.
//
// Every method must return when its context ends: each call is bounded by
// [SessionOptions.StoreTimeout]. A store must be safe for concurrent use, and
// must never write half of a session: two requests writing one session may
// leave either one's record, never a mixture. If the store also satisfies
// [Lifecycle], the application starts it before serving and stops it after
// draining.
type SessionStore interface {
	// Load returns the record saved under key, and false when there is none
	// or it has expired.
	Load(ctx context.Context, key string) (data []byte, found bool, err error)

	// Create saves a new session's record under key for ttl. The key was
	// drawn from 256 random bits a moment ago, so one that already exists is
	// a failure of the random source, and returning an error for it is
	// correct.
	Create(ctx context.Context, key string, data []byte, ttl time.Duration) error

	// Update replaces the record saved under key and gives it ttl to live,
	// reporting false, and saving nothing, when there is no such session
	// any more.
	Update(ctx context.Context, key string, data []byte, ttl time.Duration) (found bool, err error)

	// Delete removes the session saved under key. Deleting one that does not
	// exist is not an error.
	Delete(ctx context.Context, key string) error
}

// sessionIDBytes is how much randomness a session identifier carries.
const sessionIDBytes = 32

// newSessionID draws a new session identifier and returns it as the cookie
// carries it, with the key the store holds it under.
func newSessionID() (id, key string) {
	var raw [sessionIDBytes]byte
	// crypto/rand does not fail; see cookieSealer.seal.
	_, _ = rand.Read(raw[:])
	return cookieEncoding.EncodeToString(raw[:]), sessionStoreKey(raw[:])
}

// parseSessionID decodes the identifier a cookie carries, reporting false for
// anything that is not one this package could have issued. It is linear in a
// value of fixed length, and anything longer is refused unread.
func parseSessionID(value string) ([]byte, bool) {
	if len(value) != cookieEncoding.EncodedLen(sessionIDBytes) || !isBase64URL(value) {
		return nil, false
	}
	raw, err := cookieEncoding.DecodeString(value)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// sessionStoreKey returns the key a store holds a session under: the
// SHA-256 digest of its identifier, so that the store never holds the
// credential itself; see [SessionStore].
func sessionStoreKey(id []byte) string {
	sum := sha256.Sum256(id)
	return cookieEncoding.EncodeToString(sum[:])
}

// DefaultSessionMaxEntries is how many sessions a [MemorySessionStore] holds
// before it discards the least recently used, at 10000.
const DefaultSessionMaxEntries = 10_000

// MemorySessionOptions configures [NewMemorySessionStore].
type MemorySessionOptions struct {
	// MaxEntries is how many sessions the store holds at once, defaulting to
	// [DefaultSessionMaxEntries]; zero or a negative number takes the
	// default, since an unbounded store is a memory leak an anonymous client
	// can drive. Once it is full, creating a session discards the one used
	// least recently, which signs that user out, so size it above the number
	// of sessions the application really has: each holds at most
	// [SessionOptions.MaxSize] bytes.
	MaxEntries int
}

// MemorySessionStore keeps sessions in the memory of the process that serves
// them.
//
//	app := muzak.New(muzak.AppOptions{
//		Sessions: &muzak.SessionOptions{
//			Store: muzak.NewMemorySessionStore(muzak.MemorySessionOptions{MaxEntries: 50_000}),
//		},
//		CrossOriginProtection: &muzak.CrossOriginOptions{},
//	})
//
// It is for tests and for an application that runs as a single process:
// sessions are not shared with a second replica, and a restart signs everyone
// out. It is bounded by [MemorySessionOptions.MaxEntries], discards an entry
// once its TTL has passed, and does that housekeeping a few entries at a time
// as it is used, so it starts no goroutine. Every operation is constant time,
// apart from the copy of the data it stores or returns, which is copied so
// that no caller ever shares a buffer with the store or with another request.
// It is safe for concurrent use.
type MemorySessionStore struct {
	max int
	// now is the clock, replaceable so that a test can move time without
	// waiting for it.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*list.Element
	// order holds the entries most recently used first, which is the order
	// in which they are kept when the store is full.
	order *list.List
}

// memorySession is one stored session.
type memorySession struct {
	key     string
	data    []byte
	expires time.Time
}

// errSessionExists is what Create reports for a key that is already taken.
var errSessionExists = errors.New("muzak: a session with this key already exists")

// memorySweep is how many of the least recently used entries each write
// examines for expiry, which keeps an idle table from holding data past its
// TTL for long without a goroutine to sweep it.
const memorySweep = 4

// NewMemorySessionStore returns a [SessionStore] that keeps sessions in
// memory.
func NewMemorySessionStore(opts MemorySessionOptions) *MemorySessionStore {
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultSessionMaxEntries
	}
	return &MemorySessionStore{
		max:     maxEntries,
		now:     time.Now,
		entries: make(map[string]*list.Element),
		order:   list.New(),
	}
}

// Len reports how many sessions the store holds, expired ones it has not yet
// discarded included.
func (s *MemorySessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Load implements [SessionStore].
func (s *MemorySessionStore) Load(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.live(key, s.now())
	if entry == nil {
		return nil, false, nil
	}
	s.order.MoveToFront(s.entries[key])
	return bytes.Clone(entry.data), true, nil
}

// Create implements [SessionStore].
func (s *MemorySessionStore) Create(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.live(key, now) != nil {
		return errSessionExists
	}
	s.sweep(now)
	for len(s.entries) >= s.max {
		s.remove(s.order.Back())
	}
	s.entries[key] = s.order.PushFront(&memorySession{key: key, data: bytes.Clone(data), expires: now.Add(ttl)})
	return nil
}

// Update implements [SessionStore].
func (s *MemorySessionStore) Update(ctx context.Context, key string, data []byte, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry := s.live(key, now)
	if entry == nil {
		return false, nil
	}
	entry.data = bytes.Clone(data)
	entry.expires = now.Add(ttl)
	s.order.MoveToFront(s.entries[key])
	s.sweep(now)
	return true, nil
}

// Delete implements [SessionStore].
func (s *MemorySessionStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.entries[key]; ok {
		s.remove(element)
	}
	return nil
}

// live returns the unexpired entry under key, discarding it if it has
// expired.
func (s *MemorySessionStore) live(key string, now time.Time) *memorySession {
	element, ok := s.entries[key]
	if !ok {
		return nil
	}
	entry := element.Value.(*memorySession)
	if !now.Before(entry.expires) {
		s.remove(element)
		return nil
	}
	return entry
}

// sweep discards whichever of the least recently used few entries have
// expired; see [memorySweep].
func (s *MemorySessionStore) sweep(now time.Time) {
	element := s.order.Back()
	for range memorySweep {
		if element == nil {
			return
		}
		previous := element.Prev()
		if !now.Before(element.Value.(*memorySession).expires) {
			s.remove(element)
		}
		element = previous
	}
}

// remove discards one entry, dropping its data so that nothing keeps it.
func (s *MemorySessionStore) remove(element *list.Element) {
	entry := s.order.Remove(element).(*memorySession)
	delete(s.entries, entry.key)
	entry.data = nil
}
