//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// processGroup is a job object holding a child dev started and every process
// that child starts, which is how Windows lets a tree of processes be stopped
// together: a process a member starts joins the job with it.
//
// Windows has no interrupt one process can send another and have it handled,
// as os.Process.Signal documents, so stopping the group terminates the job at
// once rather than asking first.
type processGroup struct {
	process *os.Process
	job     syscall.Handle
}

// The job object functions the syscall package does not wrap. Each takes and
// returns handles and integers alone, so they are called as they are.
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procNtResumeProcess          = ntdll.NewProc("NtResumeProcess")
)

const (
	// createSuspended starts a process with its first thread suspended, so
	// that it can join the job before it runs and starts anything.
	createSuspended = 0x00000004
	// The access dev needs to the child to put it in the job and resume it.
	processAccess = 0x0001 | 0x0100 | 0x0800 // PROCESS_TERMINATE | PROCESS_SET_QUOTA | PROCESS_SUSPEND_RESUME
)

// startGroup starts cmd suspended, puts it in a job object of its own and
// lets it run, so that every process it starts is in the job too.
func startGroup(cmd *exec.Cmd) (processGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	if err := cmd.Start(); err != nil {
		return processGroup{}, err
	}
	g := processGroup{process: cmd.Process}
	handle, err := syscall.OpenProcess(processAccess, false, uint32(cmd.Process.Pid)) //nolint:gosec // a process id fits in 32 bits on Windows
	if err != nil {
		// coverage: dev has just started the process and holds it, so it can
		// open it; a process that cannot be resumed is stopped rather than
		// left suspended.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return processGroup{}, fmt.Errorf("opening the process: %w", err)
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	if job, _, _ := procCreateJobObjectW.Call(0, 0); job != 0 {
		if joined, _, _ := procAssignProcessToJobObject.Call(job, uintptr(handle)); joined != 0 {
			g.job = syscall.Handle(job)
		} else {
			// The process runs outside a job, and is stopped alone, which is
			// what a job that cannot take it leaves.
			_ = syscall.CloseHandle(syscall.Handle(job))
		}
	}
	if status, _, _ := procNtResumeProcess.Call(uintptr(handle)); status != 0 {
		// coverage: resuming a process dev created suspended and holds fails
		// only if the system refuses it outright.
		g.finish(time.Now())
		_ = cmd.Wait()
		g.release()
		return processGroup{}, errors.New("the process could not be resumed")
	}
	return g, nil
}

// signal stops the group at once; see processGroup.
func (g processGroup) signal(os.Signal) {
	g.finish(time.Now())
}

// finish terminates every process in the job, or the process alone when it
// could not join one.
func (g processGroup) finish(time.Time) {
	if g.job != 0 {
		_, _, _ = procTerminateJobObject.Call(uintptr(g.job), 1)
		return
	}
	_ = g.process.Kill()
}

// release closes the job, once its processes have been stopped.
func (g processGroup) release() {
	if g.job != 0 {
		_ = syscall.CloseHandle(g.job)
	}
}
