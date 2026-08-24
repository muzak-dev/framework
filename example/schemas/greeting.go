package schemas

import (
	"muzak.dev/framework"
)

// GreetingParams binds the input of the greeting route.
type GreetingParams struct {
	// Name is who to greet.
	Name string `query:"name" doc:"Who to greet" default:"world"`
	// Items is how many things they have, which decides which plural form the
	// answer uses.
	Items int `query:"items" doc:"How many items they have" default:"0"`
}

// Validate keeps the greeting input sensible.
//
// The rules here are ordinary ones, and their messages are translated by the
// framework without this model saying anything about it: the wording lives in
// the locale files, keyed by the rule that failed.
func (in *GreetingParams) Validate(v *muzak.Validation) {
	v.String(&in.Name).MinLen(2).MaxLen(40)
	v.Number(&in.Items).Between(0, 1000)
}

// GreetingOut is the response model for the greeting route.
type GreetingOut struct {
	// Locale is the language the rest of this response was written in.
	Locale string `json:"locale"`
	// Greeting is a translated message with a value interpolated into it.
	Greeting string `json:"greeting"`
	// Items is a translated message whose wording depends on the count.
	Items string `json:"items"`
	// Today is a date written the way the locale writes one.
	Today string `json:"today"`
	// Price is an amount of money written the way the locale writes one.
	Price string `json:"price"`
}

// CreateItem is the request model for adding something to the catalogue.
//
// It shows the two ways an application phrases a message itself: a locale key
// scoped to one field of one model, which the framework finds without being
// told, and a key named outright for a check that is an ordinary condition.
type CreateItem struct {
	Name  string  `json:"name"`
	Cost  float64 `json:"cost"`
	Price float64 `json:"price"`
}

// Validate declares the rules for a new item.
func (in *CreateItem) Validate(v *muzak.Validation) {
	// The message for this one is written in the locale files under
	// errors.models.create_item.attributes.name.blank, which the framework
	// looks for before the general wording for a blank field.
	v.String(&in.Name).Required().MaxLen(80)
	v.Number(&in.Cost).Min(0)

	// A cross-field check, named so that it is translated like every rule.
	v.When(in.Price < in.Cost).
		RejectKey(&in.Price, "errors.item.price_below_cost")
}

// CreateItemOut confirms what was added.
type CreateItemOut struct {
	Name string `json:"name"`
}
