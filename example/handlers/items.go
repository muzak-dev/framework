package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"badele"
	"badele-example/core"
	"badele-example/schemas"
)

// ListItems returns every known item.
func ListItems(ctx *badele.Context, _ badele.Empty) (schemas.ItemListOut, error) {
	store := badele.From[*core.ItemStore](ctx)
	settings := badele.From[core.Settings](ctx)

	stored := store.List()
	items := make([]schemas.ItemOut, 0, len(stored))
	for _, item := range stored {
		items = append(items, schemas.ItemOut{ID: item.ID, Name: item.Name})
	}
	return schemas.ItemListOut{Items: items, Limit: settings.ItemsPerUser}, nil
}

// ReadItem returns one item, attributing it to the caller resolved by the value
// dependency the route declares.
func ReadItem(ctx *badele.Context, in schemas.ItemParams) (schemas.ItemOut, error) {
	// Both type arguments are checked at compile time and no cast appears here.
	store := badele.From[*core.ItemStore](ctx)
	user := badele.From[core.CurrentUser](ctx)

	item, err := store.Get(in.ID)
	if err != nil {
		return schemas.ItemOut{}, asHTTPError(err)
	}
	return schemas.ItemOut{ID: item.ID, Name: item.Name, Owner: user.Username}, nil
}

// CreateItem adds an item.
//
// The route declares 201 as its status; this handler overrides it at run time
// when the caller asks for the work to be queued, which is the two mechanisms
// staying out of each other's way.
func CreateItem(ctx *badele.Context, in schemas.ItemCreateIn) (schemas.ItemOut, error) {
	store := badele.From[*core.ItemStore](ctx)

	if err := store.Create(core.Item{ID: in.ID, Name: in.Name}); err != nil {
		return schemas.ItemOut{}, asHTTPError(err)
	}
	if in.Async {
		ctx.SetStatus(http.StatusAccepted)
	}
	return schemas.ItemOut{ID: in.ID, Name: in.Name}, nil
}

// RenameItem changes an item's display name.
func RenameItem(ctx *badele.Context, in schemas.ItemRenameIn) (schemas.ItemOut, error) {
	store := badele.From[*core.ItemStore](ctx)

	item, err := store.Rename(in.ID, in.Name)
	if err != nil {
		return schemas.ItemOut{}, asHTTPError(err)
	}
	return schemas.ItemOut{ID: item.ID, Name: item.Name}, nil
}

// asHTTPError translates a store error into the response it deserves.
//
// The translation lives here rather than in the store, which keeps the store
// free of any knowledge about HTTP, and it is exhaustive rather than a
// fall-through: an error the service does not recognise becomes an opaque 500
// with the real cause logged and never sent.
func asHTTPError(err error) error {
	switch {
	case errors.Is(err, core.ErrItemNotFound):
		return badele.NewHTTPError(http.StatusNotFound, "Item not found")
	case errors.Is(err, core.ErrItemExists):
		return badele.NewHTTPError(http.StatusConflict, "Item already exists")
	default:
		return err
	}
}

// ItemSocket answers a WebSocket conversation about one item.
//
// The handshake has already succeeded by the time this runs: the input is
// bound, the guards have passed and the dependency is resolved, so what is
// left is the conversation itself. Returning ends it, and Badele closes the
// connection; returning nil closes it normally.
func ItemSocket(ctx *badele.Context, in schemas.WSItemIn, conn *badele.WSConn) error {
	session := badele.From[core.SessionOrToken](ctx)

	for {
		message, err := conn.ReadText(ctx.Context())
		if err != nil {
			// The peer closed, or the connection was lost. Either way there is
			// nothing left to say.
			return nil
		}
		if err := conn.WriteText(ctx.Context(), "credential: "+session.Value); err != nil {
			return err
		}
		if in.Q != nil {
			if err := conn.WriteText(ctx.Context(), fmt.Sprintf("q is %d", *in.Q)); err != nil {
				return err
			}
		}
		if err := conn.WriteText(ctx.Context(),
			fmt.Sprintf("you said %q, about item %s", message, in.ItemID)); err != nil {
			return err
		}
	}
}

// StreamItems streams every change to the item store as server-sent events.
//
// The request is an ordinary one: the guards have run and the dependency is
// resolved by the time this starts, and what is different is the third
// argument, which is the stream the handler owns until it returns. Everything
// it sends is a schemas.ItemOut, which the compiler enforces and the generated
// document describes.
//
//	curl -N 'http://localhost:8080/items/stream?token=jessica'
func StreamItems(ctx *badele.Context, _ badele.Empty, stream *badele.SSEStream[schemas.ItemOut]) error {
	store := badele.From[*core.ItemStore](ctx)

	// A browser sends back the identifier of the last event it saw when its
	// EventSource reconnects, which is what lets this pick the thread up
	// rather than start again. The value is the client's, so one that is not a
	// number is treated as no value at all rather than as an error.
	seen := 0
	if last := stream.LastEventID(); last != "" {
		if parsed, err := strconv.Atoi(last); err == nil {
			seen = parsed
		}
	}

	// Subscribing before the backlog is read is what stops a change made in
	// between from falling through the gap between the two.
	updates := store.Watch(stream.Context())
	for _, change := range store.Since(seen) {
		if err := sendChange(stream, change); err != nil {
			return err
		}
		seen = change.Seq
	}

	for {
		select {
		case <-stream.Context().Done():
			// The client went away or the server is shutting down. Either way
			// the conversation is over, and it is not a failure.
			return nil
		case change := <-updates:
			if change.Seq <= seen {
				// Already sent from the backlog above.
				continue
			}
			if err := sendChange(stream, change); err != nil {
				return err
			}
			seen = change.Seq
		}
	}
}

// sendChange writes one change as an event a browser can listen for by name
// and resume from by identifier.
func sendChange(stream *badele.SSEStream[schemas.ItemOut], change core.Change) error {
	item := schemas.ItemOut{ID: change.Item.ID, Name: change.Item.Name}
	return stream.SendEvent(badele.SSEEvent[schemas.ItemOut]{
		Name: "item_update",
		ID:   strconv.Itoa(change.Seq),
		Data: &item,
	})
}
