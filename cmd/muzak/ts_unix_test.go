//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestTSCreatesAFileUnderTheUmask runs this test binary as the muzak command
// under a umask that keeps a new file from everyone but its owner, as a
// developer who keeps their files private sets one. The file -o creates takes
// the umask, as every file a program creates does, rather than being made
// readable by all; a file that was there already keeps its own permissions.
func TestTSCreatesAFileUnderTheUmask(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "openapi.json", documentOf(t, routesApp()))
	ts := func(out string) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", `umask 077 && exec "$0" "$@"`,
			os.Args[0], "ts", "-file", "openapi.json", "-o", out)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "MUZAK_TEST_RUN_MAIN=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("muzak ts -o %s failed: %v\n%s", out, err, output)
		}
	}
	ts("new.ts")
	if info, err := os.Stat(filepath.Join(dir, "new.ts")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("a file created under umask 077 has permissions %v (%v), want 0600", info.Mode().Perm(), err)
	}

	existing := filepath.Join(dir, "existing.ts")
	if err := os.WriteFile(existing, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o664); err != nil {
		t.Fatal(err)
	}
	ts("existing.ts")
	if info, err := os.Stat(existing); err != nil || info.Mode().Perm() != 0o664 {
		t.Errorf("a file that was there has permissions %v (%v), want the 0664 it had", info.Mode().Perm(), err)
	}
}
