package core

import (
	"context"
	"errors"
	"sync"

	"badele"
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

// ItemStore is the example's stand-in for a database.
//
// Unlike [ModelRegistry] it does not implement badele.Lifecycle, so it is
// registered with badele.LifecycleFunc instead. Both routes into the lifecycle
// are shown here because both come up in practice: a type you own can implement
// the interface, and a type you do not own needs the closure form.
type ItemStore struct {
	mu    sync.RWMutex
	items map[string]Item
}

// NewItemStore returns an empty store.
func NewItemStore() *ItemStore {
	return &ItemStore{items: map[string]Item{}}
}

// Lifecycle returns the option that registers the store's seeding and teardown
// with the application.
//
// The closures capture the store by pointer, so what they mutate is what
// handlers later read.
func (s *ItemStore) Lifecycle() badele.SingletonOption {
	return badele.LifecycleFunc("item-store",
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
	return item, nil
}
