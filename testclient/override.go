package testclient

import "muzak.dev/framework"

// Override replaces every provider of type T in the application under test
// with provide, before the client serves it:
//
//	client := testclient.New(t, buildApp(),
//		testclient.Override(func(*muzak.Context) (CurrentUser, error) {
//			return CurrentUser{Username: "alice"}, nil
//		}))
//
// It is [muzak.App.Override] applied at the one moment it can be, after the
// application is assembled and before it is built, so everything that method
// says holds: the override belongs to this application alone, it replaces the
// value and keeps the lifetime of the provider it stands in for, guards still
// run, and an override for a type nothing provides fails the test with the
// application's build error. Giving New an application that was already built
// panics, as overriding one does.
func Override[T any](provide func(ctx *muzak.Context) (T, error)) Option {
	return func(c *config) {
		c.overrides = append(c.overrides, func(app *muzak.App) { app.Override(provide) })
	}
}

// OverrideAcquire is [Override] for a fake that has to be released, applying
// [muzak.App.OverrideAcquire]: the Release provide returns runs once each
// request is over, with that request's failure.
func OverrideAcquire[T any](provide func(ctx *muzak.Context) (T, muzak.Release, error)) Option {
	return func(c *config) {
		c.overrides = append(c.overrides, func(app *muzak.App) { app.OverrideAcquire(provide) })
	}
}
