package muzak

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// waitForAddr blocks until the application's listener is bound and returns its
// address, failing the test if the server never starts.
func waitForAddr(t *testing.T, app *App) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := app.Addr(); addr != "" {
			return addr
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the server never reported a listening address")
	return ""
}

// waitFor blocks until condition holds, failing the test with what it was
// waiting for if it never does.
func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServerOptionDefaults(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	server := app.newServer()

	if server.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", server.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	}
	if server.ReadTimeout != DefaultReadTimeout {
		t.Errorf("ReadTimeout = %v", server.ReadTimeout)
	}
	if server.WriteTimeout != DefaultWriteTimeout {
		t.Errorf("WriteTimeout = %v", server.WriteTimeout)
	}
	if server.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v", server.IdleTimeout)
	}
	if server.MaxHeaderBytes != DefaultMaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d", server.MaxHeaderBytes)
	}
	if server.ErrorLog == nil {
		t.Error("the server has no error log, so net/http would write to the standard logger")
	}
}

func TestServerOptionOverrides(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ReadTimeout = 3 * time.Second
	// A negative value asks for no limit, which net/http spells as zero.
	opts.WriteTimeout = -1
	opts.MaxHeaderBytes = 4096

	app := New(opts)
	server := app.newServer()

	if server.ReadTimeout != 3*time.Second {
		t.Errorf("ReadTimeout = %v, want 3s", server.ReadTimeout)
	}
	if server.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 for an explicitly disabled limit", server.WriteTimeout)
	}
	if server.MaxHeaderBytes != 4096 {
		t.Errorf("MaxHeaderBytes = %d, want 4096", server.MaxHeaderBytes)
	}
}

func TestOrDefaultDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, fallback, want time.Duration
	}{
		{0, 5 * time.Second, 5 * time.Second},
		{2 * time.Second, 5 * time.Second, 2 * time.Second},
		{-1, 5 * time.Second, 0},
	}
	for _, tc := range tests {
		if got := orDefaultDuration(tc.in, tc.fallback); got != tc.want {
			t.Errorf("orDefaultDuration(%v, %v) = %v, want %v", tc.in, tc.fallback, got, tc.want)
		}
	}
}

func TestRunContextServesAndShutsDown(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts)
	app.Get("/x", okHandler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()

	addr := waitForAddr(t, app)
	res, err := http.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET = %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("response = %d %s", res.StatusCode, body)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("RunContext = %v, want nil after a requested shutdown", err)
	}
	// The listener is closed, so a further request must fail.
	if _, err := http.Get("http://" + addr + "/x"); err == nil {
		t.Error("the server still accepted a request after shutdown")
	}
}

func TestRunReportsBuildFailures(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("bad-path", okHandler)

	if err := app.Run(); err == nil {
		t.Fatal("Run succeeded on an application that cannot build")
	}
}

func TestRunReportsListenFailures(t *testing.T) {
	t.Parallel()
	// Bind a port first, then ask the application for the same one.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen = %v", err)
	}
	defer listener.Close()

	stopped := make(chan struct{})
	opts := quietOptions()
	opts.Addr = listener.Addr().String()
	app := New(opts, WithLifecycle(NewLifecycle("noop",
		nil,
		func(context.Context) error { close(stopped); return nil },
	)))
	app.Get("/x", okHandler)

	if err := app.Run(); err == nil {
		t.Fatal("Run succeeded despite the address being taken")
	}
	// A failed listen must still release the components it had started.
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Error("a failed listen leaked its lifecycle components")
	}
}

func TestShutdownIsIdempotentAndSafeBeforeStart(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", okHandler)

	if err := app.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown before start = %v, want nil", err)
	}

	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	running := New(opts)
	running.Get("/x", okHandler)

	done := make(chan error, 1)
	go func() { done <- running.RunContext(context.Background()) }()
	waitForAddr(t, running)

	for range 3 {
		if err := running.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown = %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Errorf("RunContext = %v", err)
	}
}

func TestGracefulShutdownWaitsForInFlightRequests(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	finished := make(chan struct{})

	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts)
	app.Get("/slow", func(ctx *Context, _ Empty) (rtOut, error) {
		<-release
		close(finished)
		return rtOut{OK: true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()

	addr := waitForAddr(t, app)
	responses := make(chan int, 1)
	go func() {
		res, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			responses <- 0
			return
		}
		defer res.Body.Close()
		responses <- res.StatusCode
	}()

	// Give the request time to reach the handler, then ask for a shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(release)

	<-finished
	if status := <-responses; status != http.StatusOK {
		t.Errorf("the in-flight request finished with %d, want 200", status)
	}
	if err := <-done; err != nil {
		t.Errorf("RunContext = %v", err)
	}
}

func TestShutdownDeadlineClosesRemainingConnections(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	release := make(chan struct{})
	defer close(release)

	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ShutdownTimeout = 50 * time.Millisecond
	opts.Logger = logger

	app := New(opts)
	app.Get("/stuck", func(ctx *Context, _ Empty) (rtOut, error) {
		<-release
		return rtOut{OK: true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()

	addr := waitForAddr(t, app)
	go func() {
		res, err := http.Get("http://" + addr + "/stuck")
		if err == nil {
			res.Body.Close()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	<-done
	if !strings.Contains(logs.String(), "Shutdown deadline reached") {
		t.Errorf("the deadline was not reported:\n%s", logs.String())
	}
}

func TestServesTLS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts ServerOptions
		want bool
	}{
		{"plain", ServerOptions{}, false},
		{"tls config", ServerOptions{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, true},
		{"certificate pair", ServerOptions{CertFile: "cert.pem", KeyFile: "key.pem"}, true},
		{"certificate without a key", ServerOptions{CertFile: "cert.pem"}, false},
		{"key without a certificate", ServerOptions{KeyFile: "key.pem"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.ServerOptions = tc.opts
			if got := New(opts).servesTLS(); got != tc.want {
				t.Errorf("servesTLS = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTLSServerFailsWithoutCertificates(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.CertFile = "does-not-exist.pem"
	opts.KeyFile = "does-not-exist.key"

	app := New(opts)
	app.Get("/x", okHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("Run succeeded with missing certificate files")
	}
}

func TestBaseContextIsUsed(t *testing.T) {
	t.Parallel()
	type key struct{}
	type out struct {
		Value string `json:"value"`
	}

	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.BaseContext = func(net.Listener) context.Context {
		return context.WithValue(context.Background(), key{}, "from-base")
	}

	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (out, error) {
		value, _ := ctx.Context().Value(key{}).(string)
		return out{Value: value}, nil
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.RunContext(runCtx) }()

	addr := waitForAddr(t, app)
	res, err := http.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatalf("GET = %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "from-base") {
		t.Errorf("body = %s, want the base context value", body)
	}

	cancel()
	<-done
}

func TestAddrIsEmptyBeforeAndAfterConstruction(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	if got := app.Addr(); got != "" {
		t.Errorf("Addr = %q before Run, want empty", got)
	}
	// A runner whose listener has been cleared reports nothing rather than
	// dereferencing a nil listener.
	app.server.last = &serverRunner{}
	if got := app.Addr(); got != "" {
		t.Errorf("Addr = %q with no listener, want empty", got)
	}
}
