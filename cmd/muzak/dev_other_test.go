//go:build !unix

package main

import "testing"

// expectGone is not checked where there is no portable way to ask whether a
// process exists without holding a handle to it, which would keep it from
// being gone.
func expectGone(*testing.T, int) {}

// expectAlive is not checked, for the same reason.
func expectAlive(*testing.T, int) {}
