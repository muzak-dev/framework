//go:build !unix && !windows

package main

import "testing"

// deliversSignals reports whether one process can send another a signal it
// handles, which these systems cannot.
const deliversSignals = false

// killedExit is how dev reports an application that was killed.
const killedExit = `exit status 1`

// expectGone is not checked where there is no portable way to ask whether a
// process exists without holding a handle to it, which would keep it from
// being gone.
func expectGone(*testing.T, int) {}

// expectAlive is not checked, for the same reason.
func expectAlive(*testing.T, int) {}
