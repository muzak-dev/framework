// Package schemas holds the request and response models the API exposes.
//
// Keeping them in one package with no dependencies of their own is what makes
// the contract easy to review: everything a client can send or receive is
// described here, and nothing else in the service can widen it by accident.
//
// The response types are deliberately separate from the storage types in the
// core package. A field added to a stored record does not reach a client until
// somebody writes it onto a response model, which is the compile-time guarantee
// this framework is built around.
package schemas
