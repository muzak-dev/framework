//go:build unix

package main

import (
	"io/fs"
	"os"
	"testing"
)

// restrictionsBind reports whether a directory's permission bits bind the
// user running the tests, which they do not for the superuser: a test of a
// refusal then holds the run to succeeding, as it does for that user.
var restrictionsBind = os.Geteuid() != 0

// accessDenied is how the system words a refusal of access.
const accessDenied = `permission denied`

// denyCreate keeps anything from being created in dir.
func denyCreate(t *testing.T, dir string) {
	t.Helper()
	chmod(t, dir, 0o555)
}

// denyExamine keeps entry, in dir, from being examined at all.
func denyExamine(t *testing.T, dir, _ string) {
	t.Helper()
	chmod(t, dir, 0o000)
}

// denyList keeps dir from being listed, while what is in it can still be
// reached by name.
func denyList(t *testing.T, dir string) {
	t.Helper()
	chmod(t, dir, 0o333)
}

// chmod changes a mode for the rest of the test.
func chmod(t *testing.T, name string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(name, 0o755) })
}

// createdPermission reports whether perm is what new may give a file it
// creates, or a directory: what want grants, less what the umask takes, and
// never less than the owner needs.
func createdPermission(perm fs.FileMode, dir bool) (want fs.FileMode, ok bool) {
	want = 0o644
	if dir {
		want = 0o755
	}
	return want, perm&^want == 0 && perm&0o600 == 0o600 && (!dir || perm&0o700 == 0o700)
}
