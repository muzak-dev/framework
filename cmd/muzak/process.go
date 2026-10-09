package main

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// process is a child dev started, and the one goroutine that waits for it.
// Every process is waited for, which is what keeps one that exits from
// staying behind as a zombie, and the goroutine ends when the process does.
type process struct {
	cmd *exec.Cmd
	// done is closed once the process has exited and been waited for, after
	// err is set.
	done chan struct{}
	err  error
}

// startProcess starts cmd in a process group of its own, where the platform
// has them, so that stopping it stops whatever it started too.
func startProcess(cmd *exec.Cmd) (*process, error) {
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// stop asks the process, and every process in its group, to stop with sig,
// gives them grace to do so, and kills whatever is left. It returns only once
// the process has been waited for. The process itself may have exited
// already; what it started may not have, and is asked all the same.
func (p *process) stop(sig os.Signal, grace time.Duration) {
	deadline := time.Now().Add(grace)
	signalGroup(p.cmd.Process, sig)
	timer := time.NewTimer(grace)
	select {
	case <-p.done:
	case <-timer.C:
	}
	timer.Stop()
	finishGroup(p.cmd.Process, deadline)
	<-p.done
}

// kill stops the process and its group at once, as a build that is no longer
// wanted is stopped.
func (p *process) kill() {
	finishGroup(p.cmd.Process, time.Now())
	<-p.done
}

// lockedWriter serializes writes to w, so that what dev says and what the
// processes it runs write, which exec copies on goroutines of its own when w
// is not a file, do not interleave mid-line or race.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// childOutput is the writer a child process is given for w: the file itself
// when w is one, so the child writes to the terminal directly and nothing is
// copied, and the locked writer otherwise.
func childOutput(w io.Writer, locked *lockedWriter) io.Writer {
	if f, ok := w.(*os.File); ok {
		return f
	}
	return locked
}
