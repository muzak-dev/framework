// Package inprocess is the one way into the framework's server that the
// testclient package needs and an application should not have: serving an
// application on a listener the caller opened.
//
// The test client used to serve an application through httptest, whose
// server is configured by httptest rather than by the application. None of
// [muzak.dev/framework.ServerOptions] applied: a header over the framework's
// 64 KiB limit was answered 200 in a test and 431 in production, the HTTP/2
// settings and the handler accounting a shutdown relies on were absent, and
// App.Shutdown, with the drain that ends event streams and WebSockets, never
// ran at all. Going through the framework's own run path instead is what keeps
// a test answered the way production is, and this package is how the test
// client reaches that path without it becoming public API.
//
// The framework sets [Serve] when it is initialised, so it is always set by
// the time a package that imports both can call it.
package inprocess

import "context"

// Serve runs app, which must be a *muzak.App, on a socket opened on addr,
// exactly as App.Run runs it on AppOptions.Addr, except that it serves plain
// HTTP whatever TLS the application is configured with. It returns the URL
// the application is served at once it is serving, or the error that kept it
// from serving: a build failure, a component that would not start, a run
// already in progress, or an address that could not be bound.
//
// stop shuts this run down as App.Shutdown does, waits for it to return and
// reports what went wrong with either; it does nothing to a later run. A test
// that calls App.Shutdown itself ends the run in the same way, and stop then
// only waits.
var Serve func(app any, addr string) (url string, stop func(context.Context) error, err error)
