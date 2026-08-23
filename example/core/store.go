package core

import (
	"context"
	"errors"
	"sync"

	"muzak.dev/framework"
)

// Errors the store reports. Handlers translate them into HTTP errors, which
// keeps the store itself free of any knowledge about HTTP.
var (
	// ErrItemNotFound reports a lookup for an item that does not exist.
	ErrItemNotFound = errors.New("item not found")
	// ErrItemExists reports a create that collides with an existing item.
	ErrItemExists = errors.New("item already exists")
)

// Item is one stored item. It is the store's own type, deliberately separate
// from the response models in the schemas package: a field added here does not
// become visible to clients until a handler puts it on a response type.
type Item struct {
	ID   string
	Name string
}

// Change is one thing that happened to an item, numbered so that a client can
// say what it has already seen.
type Change struct {
	// Seq numbers the change within this process, from one.
	Seq int
	// Item is the item as it stands after the change.
	Item Item
}

// changeLogSize is how many changes the store remembers for a client that
// reconnects. A log that grew without bound would be a memory leak with a
// respectable name.
const changeLogSize = 128

// watcherBuffer is how many changes a subscriber may fall behind by before it
// starts losing them. Dropping is deliberate: a slow reader must not hold up
// the write that produced the change, nor the readers that are keeping up.
const watcherBuffer = 16

// ItemStore is the example's stand-in for a database.
//
// Unlike [ModelRegistry] it does not implement muzak.Lifecycle, so it is
// registered with muzak.LifecycleFunc instead. Both routes into the lifecycle
// are shown here because both come up in practice: a type you own can implement
// the interface, and a type you do not own needs the closure form.
type ItemStore struct {
	mu    sync.RWMutex
	items map[string]Item
	// seq numbers the changes, log remembers the recent ones for a client that
	// reconnects, and watchers are the streams following along.
	seq      int
	log      []Change
	watchers map[chan Change]struct{}
}

// NewItemStore returns an empty store.
func NewItemStore() *ItemStore {
	return &ItemStore{items: map[string]Item{}, watchers: map[chan Change]struct{}{}}
}

// Watch returns a channel carrying every change made from now on, until ctx is
// cancelled.
//
// A subscriber that falls more than [watcherBuffer] changes behind loses the
// ones it did not keep up with, rather than blocking the writer. That is what
// [ItemStore.Since] is for: a stream reads the backlog by sequence number and
// only then follows the live changes.
func (s *ItemStore) Watch(ctx context.Context) <-chan Change {
	updates := make(chan Change, watcherBuffer)
	s.mu.Lock()
	s.watchers[updates] = struct{}{}
	s.mu.Unlock()

	// Unsubscribing when the caller's context ends is what keeps a finished
	// stream from being sent changes nobody will read.
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.watchers, updates)
	})
	return updates
}

// Since returns the remembered changes numbered after seq, which is how a
// stream resumes from the last event a client saw.
func (s *ItemStore) Since(seq int) []Change {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Change, 0, len(s.log))
	for _, change := range s.log {
		if change.Seq > seq {
			out = append(out, change)
		}
	}
	return out
}

// record numbers a change, remembers it and hands it to every watcher. The
// store's lock must be held.
func (s *ItemStore) record(item Item) {
	s.seq++
	change := Change{Seq: s.seq, Item: item}
	s.log = append(s.log, change)
	if len(s.log) > changeLogSize {
		s.log = s.log[len(s.log)-changeLogSize:]
	}
	for watcher := range s.watchers {
		select {
		case watcher <- change:
		default:
			// This subscriber is behind. It loses the change rather than
			// holding up everyone else, and picks the thread up from Since
			// when it reconnects.
		}
	}
}

// Lifecycle returns the option that registers the store's seeding and teardown
// with the application.
//
// The closures capture the store by pointer, so what they mutate is what
// handlers later read.
func (s *ItemStore) Lifecycle() muzak.SingletonOption {
	return muzak.LifecycleFunc("item-store",
		func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.items["foo"] = Item{ID: "foo", Name: "Foo"}
			s.items["bar"] = Item{ID: "bar", Name: "Bar"}
			return nil
		},
		func(ctx context.Context) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			clear(s.items)
			clear(s.watchers)
			s.log = nil
			return nil
		},
	)
}

// List returns every stored item.
func (s *ItemStore) List() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out
}

// Get returns one item, or [ErrItemNotFound].
func (s *ItemStore) Get(id string) (Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, found := s.items[id]
	if !found {
		return Item{}, ErrItemNotFound
	}
	return item, nil
}

// Create stores a new item, or reports [ErrItemExists].
func (s *ItemStore) Create(item Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[item.ID]; exists {
		return ErrItemExists
	}
	s.items[item.ID] = item
	s.record(item)
	return nil
}

// Rename changes an item's name, or reports [ErrItemNotFound].
func (s *ItemStore) Rename(id, name string) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, found := s.items[id]
	if !found {
		return Item{}, ErrItemNotFound
	}
	item.Name = name
	s.items[id] = item
	s.record(item)
	return item, nil
}
