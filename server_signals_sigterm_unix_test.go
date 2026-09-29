//go:build unix

package muzak

import (
	"net/http"
	"syscall"
	"testing"
)

// TestRunSignalsStopsOnSIGTERM drives the signal-handling entry point.
//
// The signal is sent only once the listener is up, which is after
// signal.NotifyContext has installed its handler, so the test process receives
// it rather than being terminated by it.
func TestRunSignalsStopsOnSIGTERM(t *testing.T) {
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts)
	app.Get("/x", okHandler)

	done := make(chan error, 1)
	go func() { done <- app.RunSignals() }()

	addr := waitForAddr(t, app)
	res, err := http.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET = %v", err)
	}
	res.Body.Close()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("RunSignals = %v, want nil after an interrupt", err)
	}
}
