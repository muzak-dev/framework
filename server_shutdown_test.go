package muzak

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stopProbe is a lifecycle component that records what a shutdown looked
// like at the moment it was asked to stop.
type stopProbe struct {
	active      *atomic.Int32
	activeAtEnd atomic.Int32
	hasDeadline atomic.Bool
	remaining   atomic.Int64
	stopped     atomic.Bool
	delay       time.Duration
}

func newStopProbe(active *atomic.Int32) *stopProbe {
	p := &stopProbe{active: active}
	p.activeAtEnd.Store(-1)
	return p
}

func (p *stopProbe) component() Lifecycle {
	return NewLifecycle("probe", nil, func(ctx context.Context) error {
		if p.active != nil {
			p.activeAtEnd.Store(p.active.Load())
		}
		deadline, ok := ctx.Deadline()
		p.hasDeadline.Store(ok)
		p.remaining.Store(int64(time.Until(deadline)))
		time.Sleep(p.delay)
		p.stopped.Store(true)
		return nil
	})
}

// startServer runs app on a loopback port and returns its address and the
// channel Run's result arrives on.
func startServer(t *testing.T, app *App) (string, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	return waitForAddr(t, app), done
}

// TestShutdownSpendsOneDeadline is the regression test for a shutdown that
// gave its timeout to the WebSocket drain, then again to the event streams,
// then again to ordinary requests, and then stopped the lifecycle components
// with no deadline while handlers were still running. Here a WebSocket client
// that never reads, an event stream, a request waiting on its context and a
// request whose body trickles in are all in flight at once.
func TestShutdownSpendsOneDeadline(t *testing.T) {
	t.Parallel()
	var active atomic.Int32
	probe := newStopProbe(&active)
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ShutdownTimeout = 500 * time.Millisecond
	opts.WebSocket = WSOptions{WriteTimeout: 5 * time.Second}
	app := New(opts, WithLifecycle(probe.component()))

	wsIn, sseIn, slowIn := make(chan struct{}), make(chan struct{}), make(chan struct{})
	app.WS("/feed", func(ctx *Context, _ Empty, conn *WSConn) error {
		active.Add(1)
		defer active.Add(-1)
		close(wsIn)
		chunk := make([]byte, 64<<10)
		for {
			if err := conn.WriteBinary(ctx.Context(), chunk); err != nil {
				return err
			}
		}
	})
	app.SSE("/events", func(ctx *Context, _ Empty, s *SSEStream[string]) error {
		active.Add(1)
		defer active.Add(-1)
		close(sseIn)
		<-ctx.Context().Done()
		return nil
	})
	app.Get("/slow", func(ctx *Context, _ Empty) (rtOut, error) {
		active.Add(1)
		defer active.Add(-1)
		close(slowIn)
		<-ctx.Context().Done()
		return rtOut{OK: true}, nil
	})
	type upload struct {
		Name string `json:"name"`
	}
	app.Post("/upload", func(_ *Context, in upload) (upload, error) { return in, nil })
	addr, done := startServer(t, app)

	// A WebSocket client that never reads, so the handler's writes and the
	// server's goodbye both block until the socket buffers are drained.
	ws, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.(*net.TCPConn).SetReadBuffer(4096)
	fmt.Fprintf(ws, "GET /feed HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)
	if status, _ := bufio.NewReader(ws).ReadString('\n'); !strings.Contains(status, "101") {
		t.Fatalf("handshake answered %q", status)
	}
	// A request whose body never finishes arriving.
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fmt.Fprintf(slow, "POST /upload HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"name\":", addr)
	go func() {
		if res, err := http.Get("http://" + addr + "/events"); err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	go func() {
		if res, err := http.Get("http://" + addr + "/slow"); err == nil {
			res.Body.Close()
		}
	}()
	<-wsIn
	<-sseIn
	<-slowIn
	time.Sleep(200 * time.Millisecond) // let the socket buffers fill

	began := time.Now()
	_ = app.Shutdown(context.Background())
	took := time.Since(began)
	<-done

	if limit := opts.ShutdownTimeout + shutdownHandlerGrace + 400*time.Millisecond; took > limit {
		t.Errorf("Shutdown took %v with ShutdownTimeout %v, want one deadline for the whole drain", took, opts.ShutdownTimeout)
	}
	if got := probe.activeAtEnd.Load(); got != 0 {
		t.Errorf("lifecycle Stop ran while %d handlers were still running", got)
	}
	if !probe.hasDeadline.Load() {
		t.Error("lifecycle Stop was given a context with no deadline")
	}
	// The drain used the whole budget, so Stop is given the floor rather than
	// a context that has already expired.
	if got := time.Duration(probe.remaining.Load()); got < lifecycleStopFloor/2 || got > lifecycleStopFloor {
		t.Errorf("lifecycle Stop had %v left, want about %v", got, lifecycleStopFloor)
	}
}

// TestShutdownBoundsAHandlerThatIgnoresEverything covers the one overlap the
// shutdown admits: a handler that honours neither its context nor its closed
// connection is not waited for past the grace period, and is reported.
func TestShutdownBoundsAHandlerThatIgnoresEverything(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	var active atomic.Int32
	probe := newStopProbe(&active)
	hold := make(chan struct{})
	defer close(hold)
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ShutdownTimeout = 200 * time.Millisecond
	opts.Logger = logger
	app := New(opts, WithLifecycle(probe.component()))
	in := make(chan struct{})
	app.Get("/stuck", func(*Context, Empty) (rtOut, error) {
		active.Add(1)
		defer active.Add(-1)
		close(in)
		<-hold // a call that watches nothing
		return rtOut{OK: true}, nil
	})
	addr, done := startServer(t, app)
	go func() {
		if res, err := http.Get("http://" + addr + "/stuck"); err == nil {
			res.Body.Close()
		}
	}()
	<-in

	began := time.Now()
	_ = app.Shutdown(context.Background())
	took := time.Since(began)
	<-done
	if limit := opts.ShutdownTimeout + shutdownHandlerGrace + 400*time.Millisecond; took > limit {
		t.Errorf("Shutdown took %v, want it bounded by the deadline and the grace period", took)
	}
	if got := probe.activeAtEnd.Load(); got != 1 {
		t.Errorf("lifecycle Stop saw %d handlers running, want the one that ignores everything", got)
	}
	if !strings.Contains(logs.String(), "1 handler still running after the shutdown deadline") {
		t.Errorf("the overlap was not reported:\n%s", logs.String())
	}
}

// TestShutdownGivesStopWhatIsLeft covers a drain with nothing to wait for:
// the lifecycle components are given the rest of the deadline, and with the
// timeout disabled they are given no deadline at all.
func TestShutdownGivesStopWhatIsLeft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		timeout      time.Duration
		wantDeadline bool
	}{
		{5 * time.Second, true},
		{-1, false},
	} {
		probe := newStopProbe(nil)
		opts := quietOptions()
		opts.Addr = "127.0.0.1:0"
		opts.ShutdownTimeout = tc.timeout
		app := New(opts, WithLifecycle(probe.component()))
		app.Get("/x", okHandler)
		_, done := startServer(t, app)
		if err := app.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown = %v", err)
		}
		<-done
		if probe.hasDeadline.Load() != tc.wantDeadline {
			t.Errorf("ShutdownTimeout %v: Stop had a deadline = %v, want %v", tc.timeout, probe.hasDeadline.Load(), tc.wantDeadline)
		}
		if remaining := time.Duration(probe.remaining.Load()); tc.wantDeadline && (remaining < 4*time.Second || remaining > 5*time.Second) {
			t.Errorf("Stop had %v left of a 5s budget with nothing to drain", remaining)
		}
	}
}

// TestRunReturnsAfterShutdownFinishes covers a Shutdown called from another
// goroutine, as a signal handler does: Run used to return as soon as the
// listener closed, so a main function could exit while components were still
// being stopped.
func TestRunReturnsAfterShutdownFinishes(t *testing.T) {
	t.Parallel()
	probe := newStopProbe(nil)
	probe.delay = 100 * time.Millisecond
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(probe.component()))
	app.Get("/x", okHandler)
	_, done := startServer(t, app)
	go func() { _ = app.Shutdown(context.Background()) }()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
	if !probe.stopped.Load() {
		t.Error("Run returned before the lifecycle components had stopped")
	}
}

// TestHandlerTrackerWaits covers the counter behind the wait for handlers
// directly, including a second wait after one gave up and a handler finishing
// with nobody waiting.
func TestHandlerTrackerWaits(t *testing.T) {
	t.Parallel()
	var tracker handlerTracker
	release := make(chan struct{})
	finished := make(chan struct{})
	handler := tracker.track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(finished)
	}()
	waitFor(t, func() bool { return tracker.active.Load() == 1 }, "the handler to start")

	for range 2 {
		if running := tracker.wait(time.Millisecond); running != 1 {
			t.Errorf("wait gave up with %d running, want 1", running)
		}
	}
	// The handler returning while a wait is parked wakes it.
	time.AfterFunc(20*time.Millisecond, func() { close(release) })
	if running := tracker.wait(5 * time.Second); running != 0 {
		t.Errorf("wait = %d once the handler returned, want 0", running)
	}
	<-finished
	if running := tracker.wait(time.Second); running != 0 {
		t.Errorf("wait = %d after the handler returned, want 0", running)
	}
	// A handler that finishes while nobody waits wakes nobody.
	handler = tracker.track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := tracker.active.Load(); got != 0 {
		t.Errorf("active = %d, want 0", got)
	}
}
