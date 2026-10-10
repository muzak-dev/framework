//go:build !unix && !windows

package main

import (
	"os"
	"os/exec"
	"time"
)

// processGroup is a child dev started, alone: where there are neither process
// groups nor job objects, what the child starts cannot be reached through it.
type processGroup struct {
	process *os.Process
}

// startGroup starts cmd.
func startGroup(cmd *exec.Cmd) (processGroup, error) {
	if err := cmd.Start(); err != nil {
		return processGroup{}, err
	}
	return processGroup{process: cmd.Process}, nil
}

// signal kills the process, which these systems give no way to interrupt.
func (g processGroup) signal(os.Signal) {
	_ = g.process.Kill()
}

// finish kills the process if it is still running.
func (g processGroup) finish(time.Time) {
	_ = g.process.Kill()
}

// release does nothing.
func (processGroup) release() {}
