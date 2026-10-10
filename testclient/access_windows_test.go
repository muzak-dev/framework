//go:build windows

package testclient_test

import (
	"os/exec"
	"testing"
)

// restrictionsBind reports whether a directory's access control list binds
// the user running the tests, which a deny entry for everyone does, an
// administrator's included.
const restrictionsBind = true

// denyCreate keeps anything from being created in dir, by an entry in its
// access control list that denies every user the rights to add a file or a
// subdirectory, removed when the test ends so that the directory can be
// removed with it. Windows has no permission bits that bind the owner of a
// file. The generic write right is not denied: it carries the right to wait
// on the directory, which every open of it asks for, so the directory could
// not then be listed either.
func denyCreate(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("icacls", dir, "/deny", "*S-1-1-0:(WD,AD)").CombinedOutput(); err != nil {
		t.Fatalf("icacls %s: %v: %s", dir, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("icacls", dir, "/remove:d", "*S-1-1-0").Run() })
}
