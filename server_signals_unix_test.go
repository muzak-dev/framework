//go:build unix

package muzak

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runSignalsHelperEnv marks the copy of the test binary that plays the server
// for TestRunSignalsSecondSignalEndsTheProcess.
const runSignalsHelperEnv = "MUZAK_RUNSIGNALS_HELPER"

// TestRunSignalsHelperProcess is not a test in its own right: it is the
// server that TestRunSignalsSecondSignalEndsTheProcess starts and signals. It
// runs RunSignals with a request that never finishes, so the drain lasts the
// whole of a long ShutdownTimeout unless a signal ends the process.
func TestRunSignalsHelperProcess(t *testing.T) {
	if os.Getenv(runSignalsHelperEnv) == "" {
		t.Skip("helper for TestRunSignalsSecondSignalEndsTheProcess")
	}
	entered := make(chan struct{})
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ShutdownTimeout = time.Minute
	app := New(opts)
	app.Get("/hang", func(ctx *Context, _ Empty) (rtOut, error) {
		close(entered)
		select {} // a request that never ends by itself
	})
	go func() {
		for app.Addr() == "" {
			time.Sleep(5 * time.Millisecond)
		}
		go func() { _, _ = http.Get("http://" + app.Addr() + "/hang") }() //nolint:noctx // a helper process
		<-entered
		fmt.Println("ready")
	}()
	_ = app.RunSignals()
	fmt.Println("returned")
}

// TestRunSignalsSecondSignalEndsTheProcess is the regression test for a second
// Ctrl-C that did nothing: signal.NotifyContext keeps intercepting signals
// until its stop function runs, which was only when RunSignals returned, so
// during a drain of up to ShutdownTimeout an operator's second interrupt was
// swallowed. It must now fall through to the default action and end the
// process.
func TestRunSignalsSecondSignalEndsTheProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunSignalsHelperProcess$")
	cmd.Env = append(os.Environ(), runSignalsHelperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "ready" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		t.Fatal("the helper server never became ready")
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// Long enough for the first signal to start the drain and release the
	// handler, which is what a second interrupt has to get past.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the helper ended with %v, want it killed by the second signal", err)
		}
		if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Errorf("the helper ended with %v, want it terminated by SIGINT", exit)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a second SIGINT during the drain was swallowed")
	}
}
