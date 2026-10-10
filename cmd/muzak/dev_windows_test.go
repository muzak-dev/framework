//go:build windows

package main

import (
	"syscall"
	"testing"
	"time"
)

// deliversSignals reports whether one process can send another a signal it
// handles, which Windows cannot: dev stops the application at once there.
const deliversSignals = false

// killedExit is how dev reports an application that was killed, which on
// Windows is terminated with exit code 1.
const killedExit = `exit status 1`

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited.
const stillActive = 259

// processRunning reports whether a running process has the id. The handle it
// opens to ask is closed before it returns, so asking keeps nothing alive.
func processRunning(pid int) bool {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid)) //nolint:gosec // a process id fits in 32 bits on Windows
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}

// expectGone fails unless no running process has the id.
func expectGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}

// expectAlive fails unless a running process has the id.
func expectAlive(t *testing.T, pid int) {
	t.Helper()
	if !processRunning(pid) {
		t.Errorf("process %d is not running", pid)
	}
}
