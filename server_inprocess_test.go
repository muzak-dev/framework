package muzak

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// localListener opens a loopback listener for a test to hand to
// serveInProcess.
func localListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// closed reports whether a listener has been closed, by whether it still
// accepts. The deadline turns a listener left open into a failure rather than
// an Accept that waits forever.
func closed(listener net.Listener) bool {
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
	_, err := listener.Accept()
	return errors.Is(err, net.ErrClosed)
}

// TestServeInProcessIsARun covers the run the test client serves through: it
// starts the components, serves on the listener it was handed with the
// application's own server, in plain HTTP even with a certificate pair
// configured that does not exist, and stop shuts it down the way Shutdown
// does, after which the application can be served again.
func TestServeInProcessIsARun(t *testing.T) {
	t.Parallel()
	var starts, stops atomic.Int32
	opts := quietOptions()
	opts.CertFile, opts.KeyFile = "/nonexistent/cert.pem", "/nonexistent/key.pem"
	app := New(opts, WithLifecycle(NewLifecycle("db",
		func(context.Context) error { starts.Add(1); return nil },
		func(context.Context) error { stops.Add(1); return nil },
	)))
	app.Get("/x", okHandler)

	for round := range 2 {
		listener := localListener(t)
		stop, err := app.serveInProcess(listener)
		if err != nil {
			t.Fatalf("round %d: serveInProcess = %v", round, err)
		}
		if got := app.Addr(); got != listener.Addr().String() {
			t.Errorf("round %d: Addr = %q, want %q", round, got, listener.Addr())
		}
		res, err := http.Get("http://" + listener.Addr().String() + "/x") //nolint:noctx // a test against a local server
		if err != nil {
			t.Fatalf("round %d: GET = %v", round, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("round %d: status = %d, want 200", round, res.StatusCode)
		}
		if err := stop(context.Background()); err != nil {
			t.Errorf("round %d: stop = %v", round, err)
		}
		if got := stops.Load(); got != int32(round+1) {
			t.Errorf("round %d: components stopped %d times", round, got)
		}
	}
	if got := starts.Load(); got != 2 {
		t.Errorf("components started %d times, want once a round", got)
	}
}

// TestServeInProcessAfterShutdownOnlyWaits covers a test that shut the
// application down itself: the run is over, and stop has only to wait.
func TestServeInProcessAfterShutdownOnlyWaits(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	stop, err := app.serveInProcess(localListener(t))
	if err != nil {
		t.Fatalf("serveInProcess = %v", err)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if err := stop(context.Background()); err != nil {
		t.Errorf("stop after Shutdown = %v", err)
	}
	// Nothing was left pending by the stop, so the application serves again.
	stop, err = app.serveInProcess(localListener(t))
	if err != nil {
		t.Fatalf("serving again = %v", err)
	}
	if err := stop(context.Background()); err != nil {
		t.Errorf("stop = %v", err)
	}
}

// TestServeInProcessAtAnAddress covers the form the test client calls, which
// opens the socket itself and reports where the application is served.
func TestServeInProcessAtAnAddress(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler)
	base, stop, err := app.serveInProcessAt("127.0.0.1:0")
	if err != nil {
		t.Fatalf("serveInProcessAt = %v", err)
	}
	if base != "http://"+app.Addr() {
		t.Errorf("URL = %q, want the address the application listens on, %q", base, app.Addr())
	}
	if err := stop(context.Background()); err != nil {
		t.Errorf("stop = %v", err)
	}

	if _, _, err := app.serveInProcessAt("127.0.0.1:-1"); err == nil {
		t.Error("an address that cannot be bound was served")
	}
	broken := New(quietOptions())
	broken.Get("no-slash", okHandler)
	if _, _, err := broken.serveInProcessAt("127.0.0.1:0"); err == nil {
		t.Error("an application that cannot build was served")
	}
}

// TestServeInProcessRefusals covers each way the run can fail to serve. Every
// one of them closes the listener it was handed, which nobody else would.
func TestServeInProcessRefusals(t *testing.T) {
	t.Parallel()

	t.Run("a second run", func(t *testing.T) {
		t.Parallel()
		app := New(quietOptions())
		stop, err := app.serveInProcess(localListener(t))
		if err != nil {
			t.Fatalf("serveInProcess = %v", err)
		}
		defer func() { _ = stop(context.Background()) }()
		listener := localListener(t)
		if _, err := app.serveInProcess(listener); !errors.Is(err, errAlreadyRunning) {
			t.Errorf("a second run = %v, want it refused", err)
		}
		if !closed(listener) {
			t.Error("the refused run left its listener open")
		}
	})

	t.Run("a build failure", func(t *testing.T) {
		t.Parallel()
		app := New(quietOptions())
		app.Get("no-slash", okHandler)
		listener := localListener(t)
		if _, err := app.serveInProcess(listener); err == nil {
			t.Error("an application that cannot build was served")
		}
		if !closed(listener) {
			t.Error("the failed run left its listener open")
		}
	})

	t.Run("a shutdown before it", func(t *testing.T) {
		t.Parallel()
		app := New(quietOptions())
		if err := app.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown = %v", err)
		}
		listener := localListener(t)
		if _, err := app.serveInProcess(listener); !errors.Is(err, errShutDownBeforeServing) {
			t.Errorf("serveInProcess = %v, want it to report the shutdown", err)
		}
		if !closed(listener) {
			t.Error("the stopped run left its listener open")
		}
	})
}
