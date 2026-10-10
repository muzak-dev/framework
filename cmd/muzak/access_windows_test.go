//go:build windows

package main

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"testing"
)

// restrictionsBind reports whether a directory's access control list binds
// the user running the tests, which a deny entry for everyone does, an
// administrator's included.
const restrictionsBind = true

// accessDenied is how the system words a refusal of access.
const accessDenied = `Access is denied`

// denyEveryone adds an entry to name's access control list that denies every
// user rights, given as icacls abbreviates them, and removes it when the test
// ends, so that the directory can be removed with it. Windows has no
// permission bits that bind the owner of a file; its access control list is
// what refuses access there.
func denyEveryone(t *testing.T, name, rights string) {
	t.Helper()
	if out, err := exec.Command("icacls", name, "/deny", "*S-1-1-0:("+rights+")").CombinedOutput(); err != nil {
		t.Fatalf("icacls %s: %v: %s", name, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("icacls", name, "/remove:d", "*S-1-1-0").Run() })
}

// denyCreate keeps anything from being created in dir.
func denyCreate(t *testing.T, dir string) {
	t.Helper()
	denyEveryone(t, dir, "W")
}

// denyExamine keeps entry, in dir, from being examined at all: neither its own
// attributes nor the listing of dir that holds them can be read.
func denyExamine(t *testing.T, dir, entry string) {
	t.Helper()
	denyEveryone(t, filepath.Join(dir, entry), "RA")
	denyEveryone(t, dir, "RD")
}

// denyList keeps dir from being listed, while what is in it can still be
// reached by name.
func denyList(t *testing.T, dir string) {
	t.Helper()
	denyEveryone(t, dir, "RD")
}

// createdPermission reports whether perm is what new may give a file it
// creates, or a directory. Windows keeps no permission bits: Go reports a file
// as 0666, or 0444 when it is read-only, and a directory as 0777, so what can
// be held to there is that nothing new writes is read-only.
func createdPermission(perm fs.FileMode, dir bool) (want fs.FileMode, ok bool) {
	want = 0o666
	if dir {
		want = 0o777
	}
	return want, perm == want
}
