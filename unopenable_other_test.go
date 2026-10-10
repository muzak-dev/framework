//go:build !windows

package muzak

import (
	"os"
	"path/filepath"
	"testing"
)

// makeUnopenable makes path a file that exists and cannot be opened: a
// symbolic link to itself, which every open refuses as a loop, the
// superuser's included, where a file with no permission bits stops everyone
// but the superuser.
func makeUnopenable(t *testing.T, path string) {
	t.Helper()
	if err := os.Symlink(filepath.Base(path), path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
}
