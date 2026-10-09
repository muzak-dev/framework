//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ownProcessGroup makes the child the leader of a process group of its own,
// whose id is its process id, so that a signal sent to the group reaches every
// process it starts too, and a Ctrl-C at the terminal, which the terminal
// sends to its foreground group, reaches dev alone and is forwarded from
// there rather than delivered twice.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to every process in the child's group.
func signalGroup(p *os.Process, sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		// coverage: every signal dev forwards comes from os/signal, which
		// delivers a syscall.Signal on every Unix.
		s = syscall.SIGINT
	}
	_ = syscall.Kill(-p.Pid, s)
}

// groupPollInterval is how often finishGroup looks for processes of the
// group that are still running.
const groupPollInterval = 10 * time.Millisecond

// finishGroup waits until deadline for every process left in the child's
// group to exit, and then kills those that have not.
//
// The leader may have exited already and been waited for. A process group
// lives on while any process is in it, so its id cannot be handed to another
// group while one of the application's processes remains; it can be reused
// only once the group is empty, and only after the system has handed out
// every other process id in between, which is why dev finishes a group within
// the grace period of its leader exiting rather than at some later time.
func finishGroup(p *os.Process, deadline time.Time) {
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-p.Pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(groupPollInterval)
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}
