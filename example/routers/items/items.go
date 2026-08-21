// Package items exposes the item routes, including the ones that demonstrate
// value dependencies and a status decided at run time.
package items

import (
	"net/http"
	"sync"

	"badele"
)

// ItemOut is the response model for a single item.
type ItemOut struct {
	// ID identifies the item.
	ID string `json:"id" doc:"The item's identifier"`
	// Name is the item's display name.
	Name string `json:"name" doc:"The item's display name"`
	// Owner is the user the item belongs to.
	Owner string `json:"owner,omitzero" doc:"The user the item belongs to"`
}

// Params binds an item identifier from the path.
type Params struct {
	// ID is read from the path template.
	ID string `path:"item_id" doc:"The item to operate on"`
}

// CreateBody is the JSON request body for creating an item. Every field here
// comes from the body, because none of them carries a location tag.
type CreateBody struct {
	// ID is the identifier the caller chooses.
	ID string `json:"id" doc:"The identifier to create the item under"`
	// Name is the item's display name.
	Name string `json:"name" doc:"The item's display name"`
	// Async asks for the work to be queued rather than performed inline,
	// which changes the status the handler reports.
	Async bool `json:"async,omitzero" doc:"Queue the work instead of doing it inline"`
}

// UpdateIn mixes a path parameter with a JSON body. The body can only ever
// reach Name, because binding copies out the body-bound fields alone.
type UpdateIn struct {
	// ID is read from the path template.
	ID string `path:"item_id" doc:"The item to update"`
	// Name is read from the JSON body.
	Name string `json:"name" doc:"The item's new display name"`
}

// ListOut is the response model for the item listing.
type ListOut struct {
	// Items is the full set of known items.
	Items []ItemOut `json:"items"`
}

// store is the example's stand-in for a database.
var store = struct {
	sync.RWMutex
	items map[string]ItemOut
}{items: map[string]ItemOut{
	"foo": {ID: "foo", Name: "Foo"},
	"bar": {ID: "bar", Name: "Bar"},
}}

// NewRouter returns the items router.
func NewRouter() *badele.Router {
	r := badele.NewRouter(badele.WithTags("items"))

	r.Get("/items/", list,
		badele.Summary("List items"))

	r.Get("/items/{item_id}", read,
		badele.Summary("Read an item"),
		badele.WithResponseDoc(http.StatusNotFound, "The item does not exist"),
		badele.Needs(currentUser))

	r.Post("/items/", create,
		badele.Summary("Create an item"),
		badele.Status(http.StatusCreated),
		badele.WithResponseDoc(http.StatusConflict, "An item with that identifier already exists"))

	r.Put("/items/{item_id}", update,
		badele.Summary("Rename an item"),
		badele.WithResponseDoc(http.StatusNotFound, "The item does not exist"))

	return r
}

// currentUser is declared here so the items package does not depend on the
// application package. In a real service this would live in a shared auth
// package and be reused by every router that needs it.
func currentUser(ctx *badele.Context) (Owner, error) {
	token, present := badele.BearerToken(ctx)
	if !present {
		return Owner{}, badele.NewHTTPError(http.StatusUnauthorized, "unauthorized")
	}
	_ = token
	return Owner{Username: "fakecurrentuser"}, nil
}

// Owner is the value produced by the currentUser dependency.
type Owner struct {
	// Username identifies the caller.
	Username string
}

// list returns every known item.
func list(ctx *badele.Context, _ badele.Empty) (ListOut, error) {
	store.RLock()
	defer store.RUnlock()
	out := ListOut{Items: make([]ItemOut, 0, len(store.items))}
	for _, item := range store.items {
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// read returns one item, attributing it to the caller resolved by the value
// dependency the route declares.
func read(ctx *badele.Context, in Params) (ItemOut, error) {
	// The type argument is checked at compile time and there is no cast.
	owner := badele.From[Owner](ctx)

	store.RLock()
	item, found := store.items[in.ID]
	store.RUnlock()
	if !found {
		return ItemOut{}, badele.NewHTTPError(http.StatusNotFound, "Item not found")
	}
	item.Owner = owner.Username
	return item, nil
}

// create adds an item. The route declares 201 as its status; the handler
// overrides it at run time when the caller asks for the work to be queued,
// which shows the two mechanisms staying out of each other's way.
func create(ctx *badele.Context, in CreateBody) (ItemOut, error) {
	store.Lock()
	defer store.Unlock()
	if _, exists := store.items[in.ID]; exists {
		return ItemOut{}, badele.NewHTTPError(http.StatusConflict, "Item already exists")
	}
	item := ItemOut{ID: in.ID, Name: in.Name}
	store.items[in.ID] = item
	if in.Async {
		ctx.SetStatus(http.StatusAccepted)
	}
	return item, nil
}

// update renames an item.
func update(ctx *badele.Context, in UpdateIn) (ItemOut, error) {
	store.Lock()
	defer store.Unlock()
	item, found := store.items[in.ID]
	if !found {
		return ItemOut{}, badele.NewHTTPError(http.StatusNotFound, "Item not found")
	}
	item.Name = in.Name
	store.items[in.ID] = item
	return item, nil
}
