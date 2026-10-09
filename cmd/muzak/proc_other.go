//go:build !unix

package main

import (
	"os"
	"os/exec"
	"time"
)

// ownProcessGroup does nothing where there are no Unix process groups. On
// Windows a process started by the application is not stopped with it: doing
// that takes a job object, which the standard library does not offer.
func ownProcessGroup(*exec.Cmd) {}

// signalGroup kills the process. Windows has no way to send another process
// an interrupt it can handle, as os.Process.Signal documents, so an
// application dev runs there is stopped without the chance to shut down
// gracefully.
func signalGroup(p *os.Process, _ os.Signal) {
	_ = p.Kill()
}

// finishGroup kills the process if it is still running.
func finishGroup(p *os.Process, _ time.Time) {
	_ = p.Kill()
}
