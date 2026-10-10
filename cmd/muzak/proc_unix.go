//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// processGroup is the process group a child dev started leads, whose id is the
// child's process id.
type processGroup struct {
	pid int
}

// startGroup starts cmd as the leader of a process group of its own, so that
// a signal sent to the group reaches every process it starts too, and a
// Ctrl-C at the terminal, which the terminal sends to its foreground group,
// reaches dev alone and is forwarded from there rather than delivered twice.
func startGroup(cmd *exec.Cmd) (processGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return processGroup{}, err
	}
	return processGroup{pid: cmd.Process.Pid}, nil
}

// signal sends sig to every process in the group.
func (g processGroup) signal(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		// coverage: every signal dev forwards comes from os/signal, which
		// delivers a syscall.Signal on every Unix.
		s = syscall.SIGINT
	}
	_ = syscall.Kill(-g.pid, s)
}

// groupPollInterval is how often finish looks for processes of the group
// that are still running.
const groupPollInterval = 10 * time.Millisecond

// finish waits until deadline for every process left in the group to exit,
// and then kills those that have not.
//
// The leader may have exited already and been waited for. A process group
// lives on while any process is in it, so its id cannot be handed to another
// group while one of the application's processes remains; it can be reused
// only once the group is empty, and only after the system has handed out
// every other process id in between, which is why dev finishes a group within
// the grace period of its leader exiting rather than at some later time.
func (g processGroup) finish(deadline time.Time) {
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-g.pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(groupPollInterval)
	}
	_ = syscall.Kill(-g.pid, syscall.SIGKILL)
}

// release does nothing: a process group holds nothing of dev's once it is
// empty.
func (processGroup) release() {}
