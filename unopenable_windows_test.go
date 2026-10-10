//go:build windows

package muzak

import (
	"os"
	"syscall"
	"testing"
)

// makeUnopenable makes path a file that exists and cannot be opened: it is
// held open by a handle that shares it with nobody, as an editor or a virus
// scanner holding a file does, until the test ends. Permission bits stop no
// one on Windows, where a file with none is still read by its owner.
func makeUnopenable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("KEY=value\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(handle) })
}
