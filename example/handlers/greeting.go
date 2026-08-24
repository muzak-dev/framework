package handlers

import (
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/example/schemas"
)

// Greeting answers in whatever language the request asked for.
//
// Nothing here decides the language. The framework resolved it once, before the
// handler ran, from the query parameter or the Accept-Language header, and the
// context carries it: ctx.T looks a message up in it and ctx.L writes a date or
// an amount the way it is written in it.
func Greeting(ctx *muzak.Context, in schemas.GreetingParams) (schemas.GreetingOut, error) {
	return schemas.GreetingOut{
		Locale:   ctx.Locale(),
		Greeting: ctx.T("greeting.hello", "name", in.Name),
		// The count both prints and chooses: "no items", "one item" and
		// "5 items" are three different entries in the locale file, and the
		// language decides which of them a number selects.
		Items: ctx.T("greeting.items", "count", in.Items),
		Today: ctx.L(time.Now(), "as", "date", "format", "long"),
		Price: ctx.T("greeting.price", "amount", ctx.L(1234.5, "precision", 2)),
	}, nil
}

// AddCatalogueItem adds something to the catalogue, or explains why it cannot,
// in the language the request asked for.
func AddCatalogueItem(_ *muzak.Context, in schemas.CreateItem) (schemas.CreateItemOut, error) {
	return schemas.CreateItemOut{Name: in.Name}, nil
}
