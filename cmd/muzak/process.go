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
	// group is the process together with whatever it starts, as the platform
	// holds them; see processGroup.
	group processGroup
	// done is closed once the process has exited and been waited for, after
	// err is set.
	done chan struct{}
	err  error
}

// startProcess starts cmd in a group of its own, so that stopping it stops
// whatever it started too.
func startProcess(cmd *exec.Cmd) (*process, error) {
	group, err := startGroup(cmd)
	if err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, group: group, done: make(chan struct{})}
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
	p.group.signal(sig)
	timer := time.NewTimer(grace)
	select {
	case <-p.done:
	case <-timer.C:
	}
	timer.Stop()
	p.group.finish(deadline)
	<-p.done
	p.group.release()
}

// kill stops the process and its group at once, as a build that is no longer
// wanted is stopped, and as what a process that ended left behind is.
func (p *process) kill() {
	p.group.finish(time.Now())
	<-p.done
	p.group.release()
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
