package schemas

import (
	"badele"
)

// ItemOut is the response model for a single item.
type ItemOut struct {
	// ID identifies the item.
	ID string `json:"id" doc:"The item's identifier"`
	// Name is the item's display name.
	Name string `json:"name" doc:"The item's display name"`
	// Owner is the user the item belongs to, present only where the route
	// resolves the caller.
	Owner string `json:"owner,omitzero" doc:"The user the item belongs to"`
}

// ItemParams binds an item identifier from the path.
type ItemParams struct {
	// ID is read from the path template.
	ID string `path:"item_id" doc:"The item to operate on"`
}

// ItemCreateIn is the JSON request body for creating an item. Every field here
// comes from the body, because none of them carries a location tag.
type ItemCreateIn struct {
	// ID is the identifier the caller chooses.
	ID string `json:"id" doc:"The identifier to create the item under"`
	// Name is the item's display name.
	Name string `json:"name" doc:"The item's display name"`
	// Async asks for the work to be queued rather than performed inline, which
	// changes the status the handler reports.
	Async bool `json:"async,omitzero" doc:"Queue the work instead of doing it inline"`
}

// ItemRenameIn mixes a path parameter with a JSON body.
//
// The body can only ever reach Name: binding decodes into a scratch value and
// copies out the body-bound fields alone, so a crafted body cannot write to ID.
type ItemRenameIn struct {
	// ID is read from the path template.
	ID string `path:"item_id" doc:"The item to rename"`
	// Name is read from the JSON body.
	Name string `json:"name" doc:"The item's new display name"`
}

// ItemListOut is the response model for the item listing.
type ItemListOut struct {
	// Items is the full set of known items.
	Items []ItemOut `json:"items"`
	// Limit reports the per-user cap the service is configured with.
	Limit int `json:"limit" doc:"How many items one user may hold"`
}

// Validate constrains a new item.
func (in *ItemCreateIn) Validate(v *badele.Validation) {
	v.String(&in.ID).Trim().Lower().Required().MinLen(2).MaxLen(32).
		Matches(`^[a-z0-9-]+$`).
		Message("may only contain lower case letters, digits and hyphens")
	v.String(&in.Name).Trim().Required().MaxLen(80)
}

// Validate constrains a rename, covering a model that mixes a path parameter
// with a body member. The path parameter is reported as one, which comes free
// from the binding plan.
func (in *ItemRenameIn) Validate(v *badele.Validation) {
	v.String(&in.ID).Required()
	v.String(&in.Name).Trim().Required().MinLen(1).MaxLen(80)
}

// Validate constrains the item identifier read from the path.
func (in *ItemParams) Validate(v *badele.Validation) {
	v.String(&in.ID).Trim().Lower().Required().MaxLen(32)
}

// WSItemIn is the input of the item's WebSocket route.
//
// A handshake carries no body, so every field comes from the path, the query
// string, a header or a cookie. The pointer makes the query parameter
// optional: it stays nil when the client did not send one, which is how an
// absent value is told from a zero one.
type WSItemIn struct {
	// ItemID is read from the path template.
	ItemID string `path:"item_id" doc:"The item being talked about"`
	// Q is an optional number echoed back on every message.
	Q *int `query:"q" doc:"An optional number echoed back with each reply"`
}
