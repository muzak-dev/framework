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
// access control list that denies every user the right to write, removed
// when the test ends so that the directory can be removed with it. Windows
// has no permission bits that bind the owner of a file.
func denyCreate(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("icacls", dir, "/deny", "*S-1-1-0:(W)").CombinedOutput(); err != nil {
		t.Fatalf("icacls %s: %v: %s", dir, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("icacls", dir, "/remove:d", "*S-1-1-0").Run() })
}
