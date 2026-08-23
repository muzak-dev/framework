package muzak

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
)

// assertNoGoroutineLeaks fails the test if Go's goroutine leak profile reports
// any leaked goroutine.
//
// The profile is the authoritative answer to "did anything get stuck": the
// runtime performs a garbage collection with leak detection and reports the
// goroutines that can never be resumed, which is exactly the failure mode a
// request handler, a dependency container or a pooled buffer could introduce.
// It is more precise than counting goroutines, because it does not confuse a
// leak with a goroutine that simply has not finished yet.
func assertNoGoroutineLeaks(t *testing.T) {
	t.Helper()
	profile := pprof.Lookup("goroutineleak")
	if profile == nil {
		t.Fatal("the goroutineleak profile is unavailable; Go 1.27 or later is required")
	}

	// Give anything still winding down a chance to finish before asking, so
	// that a slow but correct shutdown is not mistaken for a leak.
	for range 5 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}

	var buf bytes.Buffer
	if err := profile.WriteTo(&buf, 1); err != nil {
		t.Fatalf("writing the goroutine leak profile: %v", err)
	}
	report := buf.String()

	// The profile header reports the count; anything above zero is a leak.
	if strings.Contains(report, "goroutineleak profile: total 0") {
		return
	}
	t.Errorf("goroutines leaked:\n%s", report)
}

// TestNoGoroutineLeaksAcrossRequests drives a full application through many
// requests of every shape and then asserts that nothing was left running.
func TestNoGoroutineLeaksAcrossRequests(t *testing.T) {
	app := mustBuild(t, newIntegrationApp())

	requests := []struct {
		method string
		target string
		body   string
	}{
		{"GET", "/users/me?token=jessica", ""},
		{"GET", "/users/rick?token=jessica", ""},
		{"GET", "/users/?token=jessica&limit=3", ""},
		{"GET", "/users/?token=jessica&limit=bad", ""},
		{"GET", "/items/plumbus?token=jessica", ""},
		{"POST", "/items/?token=jessica", `{"name":"Portal Gun"}`},
		{"POST", "/items/?token=jessica", `{"bad json`},
		{"GET", "/nowhere?token=jessica", ""},
		{"DELETE", "/users/me?token=jessica", ""},
		{"OPTIONS", "/users/me?token=jessica", ""},
		{"GET", "/openapi.json", ""},
		{"GET", "/docs", ""},
		{"GET", "/users/me", ""},
	}

	var wg sync.WaitGroup
	for range 20 {
		for _, r := range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(r.method, r.target, strings.NewReader(r.body))
				if r.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				app.ServeHTTP(httptest.NewRecorder(), req)
			}()
		}
	}
	wg.Wait()

	assertNoGoroutineLeaks(t)
}

// TestNoGoroutineLeaksAcrossServerLifecycle covers the parts a request-only
// test cannot reach: the accept loop, the shutdown path and the lifecycle
// manager's own goroutines.
func TestNoGoroutineLeaksAcrossServerLifecycle(t *testing.T) {
	for range 3 {
		opts := quietOptions()
		opts.Addr = "127.0.0.1:0"

		component := &recorder{name: "resource"}
		app := New(opts, WithLifecycle(component))
		app.Get("/x", okHandler)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- app.RunContext(ctx) }()

		addr := waitForAddr(t, app)
		for range 5 {
			res, err := http.Get("http://" + addr + "/x")
			if err != nil {
				t.Fatalf("GET = %v", err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("RunContext = %v", err)
		}
		if started, stopped := component.counts(); started != 1 || stopped != 1 {
			t.Errorf("component started %d and stopped %d times, want 1 and 1", started, stopped)
		}
	}

	// http.Get leaves idle connections in the default transport's pool; those
	// are the caller's, not the server's, so they are closed before asking.
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	assertNoGoroutineLeaks(t)
}

// TestNoGoroutineLeaksFromFailedStartup checks that unwinding a partial
// start-up leaves nothing running either.
func TestNoGoroutineLeaksFromFailedStartup(t *testing.T) {
	for range 5 {
		healthy := &recorder{name: "healthy"}
		broken := &recorder{name: "broken", startErr: io.ErrUnexpectedEOF}
		slow := &recorder{name: "slow", startDelay: 20 * time.Millisecond}

		app := New(quietOptions(), WithLifecycle(healthy, broken, slow))
		app.Get("/x", okHandler)
		mustBuild(t, app)

		if err := app.StartLifecycle(context.Background()); err == nil {
			t.Fatal("StartLifecycle succeeded, want a failure")
		}
	}
	assertNoGoroutineLeaks(t)
}
