//go:build unix

package testclient_test

import (
	"os"
	"testing"
)

// restrictionsBind reports whether a directory's permission bits bind the
// user running the tests, which they do not for the superuser: a test of a
// refusal then holds the run to succeeding, as it does for that user.
var restrictionsBind = os.Geteuid() != 0

// denyCreate keeps anything from being created in dir.
func denyCreate(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}
