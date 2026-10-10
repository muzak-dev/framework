//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// deliversSignals reports whether one process can send another a signal it
// handles, which every Unix can.
const deliversSignals = true

// killedExit is how dev reports an application that was killed.
const killedExit = `signal: killed`

// expectGone fails unless no process has the id, which also means it was
// waited for: a process that exited and was not would still be found, as a
// zombie.
func expectGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d is still there", pid)
}

// expectAlive fails unless a process has the id.
func expectAlive(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("process %d is gone: %v", pid, err)
	}
}

// TestDevStopsOnARealSignal runs this test binary as the muzak command, in a
// process of its own, and signals that process, as a terminal or a process
// manager would: the signal reaches dev through main, is forwarded to the
// application, and dev exits once the application has stopped, leaving no
// process and no build behind.
//
// A hang-up, which closing the terminal sends, and a quit, which Ctrl-\
// sends, reach dev alone, since the application runs in a group of its own:
// left to their default, they ended dev and left the application running,
// holding its port, with nobody to stop it.
func TestDevStopsOnARealSignal(t *testing.T) {
	t.Parallel()
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			p := newProject(t)
			cmd := exec.Command(os.Args[0], "dev", "-pkg", ".", "-poll", "20ms")
			cmd.Dir = p.dir
			cmd.Env = append(goEnv(), "MUZAK_TEST_RUN_MAIN=1", "FAKE_MARKER="+p.marker, "TMPDIR="+p.temp)
			stderr := &syncBuffer{}
			cmd.Stdout, cmd.Stderr = io.Discard, stderr
			// An application dev left running holds the output pipe open,
			// which the wait would otherwise wait on for as long as it runs.
			cmd.WaitDelay = 5 * time.Second
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			// Whatever happens to the test, the process is ended and
			// waited for; the wait below hands its result back for this.
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-exited
			})
			started := p.waitFor(t, `^start 1 (\d+) `)
			t.Cleanup(func() {
				// An application dev failed to stop is killed, so that a
				// failure leaves nothing running on the machine.
				if t.Failed() {
					_ = syscall.Kill(pidOf(t, started[1]), syscall.SIGKILL)
				}
			})
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-exited:
				exited <- err
				if err != nil {
					t.Errorf("muzak dev exited with %v\n%s", err, stderr)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("muzak dev did not exit\n%s", stderr)
			}
			if !p.recorded(`^signal 1 ` + started[1] + ` ` + sig.String() + `$`) {
				t.Errorf("the application did not receive %s:\n%s", sig, strings.Join(p.lines(), "\n"))
			}
			expectGone(t, pidOf(t, started[1]))
			expectNoBuilds(t, p)
		})
	}
}
