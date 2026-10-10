//go:build !unix && !windows

package main

import (
	"io/fs"
	"testing"
)

// restrictionsBind reports whether a directory's permissions bind the user
// running the tests, which these systems give no way to arrange.
const restrictionsBind = false

// accessDenied is how a refusal of access is worded.
const accessDenied = `permission denied`

// denyCreate does nothing; see restrictionsBind.
func denyCreate(*testing.T, string) {}

// denyExamine does nothing; see restrictionsBind.
func denyExamine(*testing.T, string, string) {}

// denyList does nothing; see restrictionsBind.
func denyList(*testing.T, string) {}

// createdPermission reports whether perm is what new may give a file it
// creates, or a directory, as on Unix.
func createdPermission(perm fs.FileMode, dir bool) (want fs.FileMode, ok bool) {
	want = 0o644
	if dir {
		want = 0o755
	}
	return want, perm&^want == 0 && perm&0o600 == 0o600 && (!dir || perm&0o700 == 0o700)
}
