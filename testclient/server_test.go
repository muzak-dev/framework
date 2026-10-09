package testclient_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// fidelityKey carries a value from the server's BaseContext to a handler.
type fidelityKey struct{}

// TestClientServesWithTheApplicationsServer is the regression test for a
// test server that was httptest's rather than the application's. None of
// ServerOptions applied: httptest admits a header block of a mebibyte where
// the framework stops at 64 KiB, so a request production answers 431 was
// answered 200 in a test, and a BaseContext, a timeout or an HTTP/2 setting
// configured on the application never reached the server under test.
func TestClientServesWithTheApplicationsServer(t *testing.T) {
	t.Parallel()
	app := muzak.New(muzak.AppOptions{
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
		ServerOptions: muzak.ServerOptions{
			BaseContext: func(net.Listener) context.Context {
				return context.WithValue(context.Background(), fidelityKey{}, "from the base context")
			},
		},
	})
	app.Get("/base", func(ctx *muzak.Context, _ muzak.Empty) (map[string]any, error) {
		return map[string]any{"value": ctx.Context().Value(fidelityKey{})}, nil
	})
	client := testclient.New(t, app)

	client.Get("/base").AssertStatus(http.StatusOK).AssertJSON(`{"value":"from the base context"}`)

	huge := strings.Repeat("x", 100<<10)
	client.Get("/base", testclient.Header("X-Padding", huge)).AssertStatus(http.StatusRequestHeaderFieldsTooLarge)
	if got := app.Addr(); "http://"+got != client.URL() {
		t.Errorf("App.Addr = %q, want the address the client sends to, %q", got, client.URL())
	}
}

// TestAppShutdownDrainsTheClientsServer checks the other half of serving the
// way production does: App.Shutdown reaches the server under test, ending an
// open event stream and stopping the lifecycle components only once the
// requests still running have returned. Under httptest a Shutdown found no run
// to stop and returned at once, with the stream still open.
func TestAppShutdownDrainsTheClientsServer(t *testing.T) {
	t.Parallel()
	var handlerDone, stoppedEarly atomic.Bool
	app := muzak.New(muzak.AppOptions{
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	}, muzak.WithLifecycle(muzak.NewLifecycle("pool", nil, func(context.Context) error {
		stoppedEarly.Store(!handlerDone.Load())
		return nil
	})))
	entered := make(chan struct{})
	release := make(chan struct{})
	app.Get("/slow", func(*muzak.Context, muzak.Empty) (map[string]bool, error) {
		close(entered)
		<-release
		handlerDone.Store(true)
		return map[string]bool{"ok": true}, nil
	})
	app.SSE("/idle", func(_ *muzak.Context, _ muzak.Empty, stream *muzak.SSEStream[muzak.Empty]) error {
		<-stream.Context().Done()
		return stream.Err()
	})
	client := testclient.New(t, app)
	stream := client.SSE("/idle")

	slow := make(chan int, 1)
	go func() {
		res, err := client.HTTPClient().Get(client.URL() + "/slow") //nolint:noctx // a test against a local server
		if err != nil {
			slow <- 0
			return
		}
		_ = res.Body.Close()
		slow <- res.StatusCode
	}()
	<-entered

	shutdown := make(chan error, 1)
	go func() { shutdown <- app.Shutdown(context.Background()) }()
	if _, err := stream.TryNext(); !errors.Is(err, muzak.ErrSSEStreamEnded) {
		t.Errorf("TryNext() = %v, want the stream ended by the shutdown", err)
	}
	close(release)
	select {
	case err := <-shutdown:
		if err != nil {
			t.Errorf("Shutdown = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if status := <-slow; status != http.StatusOK {
		t.Errorf("the request in flight got %d, want it to finish with 200", status)
	}
	if stoppedEarly.Load() {
		t.Error("the components were stopped while a request was still using them")
	}
}
