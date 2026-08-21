// Package handlers holds the functions that answer requests.
//
// A handler is an ordinary typed function, so each one can be called directly
// from a unit test with a constructed input, without a router, a server or an
// HTTP request in sight. The routers package is what binds them to paths.
//
// Handlers depend on the schemas for their input and output types and on the
// core package for the resources they use. They never reach for a package level
// variable: anything a handler needs either arrives in its input or is
// published as a dependency and retrieved by type.
package handlers
