package muzak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDrainDelayValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		delay    time.Duration
		shutdown time.Duration
		want     string
	}{
		{"negative", -time.Second, 0, "ServerOptions.DrainDelay is -1s; it must not be negative"},
		{"equal to the deadline", 15 * time.Second, 0, "DrainDelay (15s) is not shorter than ServerOptions.ShutdownTimeout (15s)"},
		{"past the deadline", 5 * time.Second, 2 * time.Second, "DrainDelay (5s) is not shorter than ServerOptions.ShutdownTimeout (2s)"},
		{"within the deadline", 5 * time.Second, 10 * time.Second, ""},
		{"unbounded shutdown", time.Hour, -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.DrainDelay = tc.delay
			opts.ShutdownTimeout = tc.shutdown
			app := New(opts)
			if tc.want == "" {
				mustBuild(t, app)
				return
			}
			if msg := buildError(t, app); !strings.Contains(msg, tc.want) {
				t.Errorf("build error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

// TestDrainDelayAppliesWithoutHealthAndCountsAgainstTheDeadline checks that a
// delay is honoured even when nothing probes readiness, since a platform that
// removes endpoints on its own clock needs it too, and that it is cut short by
// the deadline the caller of Shutdown passed rather than added to it.
func TestDrainDelayAppliesWithoutHealthAndCountsAgainstTheDeadline(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.DrainDelay = 250 * time.Millisecond
	app := New(opts)
	app.Get("/x", okHandler)

	_, done := startServer(t, app)
	began := time.Now()
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took < opts.DrainDelay {
		t.Errorf("Shutdown took %s, less than the DrainDelay of %s", took, opts.DrainDelay)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	opts.DrainDelay = 10 * time.Second
	opts.ShutdownTimeout = 20 * time.Second
	app = New(opts)
	app.Get("/x", okHandler)
	_, done = startServer(t, app)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began = time.Now()
	_ = app.Shutdown(ctx)
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("Shutdown took %s although its context gave it 100ms", took)
	}
	<-done
}

func TestBeginDrainWithoutADelayClosesThePoolAtOnce(t *testing.T) {
	t.Parallel()
	app := readyApp(t, HealthOptions{Checks: []HealthCheck{{Name: "db", Check: func(context.Context) error { return nil }}}})
	assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusOK)
	began := time.Now()
	app.beginDrain(context.Background(), app.logger)
	if took := time.Since(began); took > time.Second {
		t.Errorf("beginDrain took %s with no delay", took)
	}
	if !app.readiness.draining.Load() {
		t.Error("readiness was not marked as draining")
	}
	if err := app.background.reserve(); err == nil {
		t.Error("a task was accepted after the drain began")
	}
	app.reopenOperations()
	if app.readiness.draining.Load() {
		t.Error("reopening kept readiness draining")
	}
	app.health.checker.mu.Lock()
	cached := app.health.checker.cached
	app.health.checker.mu.Unlock()
	if cached != nil {
		t.Error("reopening kept the previous run's readiness result")
	}
	if err := app.background.reserve(); err != nil {
		t.Errorf("a task was refused after reopening: %v", err)
	}
}

// FuzzHealthProbe sends a probe with any method and any path, and checks the
// invariants a prober and an attacker alike can rely on: nothing panics, a
// probe is answered 200, 503 or 405 with a small JSON body that never carries
// a check's error, and a path that is not a health endpoint is left to the
// application.
func FuzzHealthProbe(f *testing.F) {
	for _, seed := range []struct{ method, path string }{
		{"GET", "/livez"}, {"HEAD", "/readyz"}, {"POST", "/readyz"}, {"OPTIONS", "/livez"},
		{"GET", "/live%7Az"}, {"GET", "/readyz/"}, {"GET", "//readyz"}, {"BREW", "/x"},
		{"GET", "/readyz?a=b"}, {strings.Repeat("M", 300), "/livez"},
	} {
		f.Add(seed.method, seed.path)
	}
	app := New(healthOptions(HealthOptions{
		ReportChecks:  true,
		CacheInterval: -1,
		Checks: []HealthCheck{
			{Name: "db", Check: func(context.Context) error { return context.DeadlineExceeded }},
			{Name: "ok", Check: func(context.Context) error { return nil }},
		},
	}))
	app.Get("/x", okHandler)
	if err := app.Build(); err != nil {
		f.Fatal(err)
	}
	if err := app.StartLifecycle(context.Background()); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, method, path string) {
		if !isHTTPToken(method) || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \r\n\x00#") {
			t.Skip()
		}
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Method = method
		parsed, err := req.URL.Parse(path)
		if err != nil {
			t.Skip()
		}
		req.URL = parsed
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if !app.isHealthPath(req.URL.Path) {
			return
		}
		switch rec.Code {
		case http.StatusOK, http.StatusServiceUnavailable:
			if method != http.MethodGet && method != http.MethodHead {
				t.Fatalf("%s %s answered %d", method, path, rec.Code)
			}
		case http.StatusMethodNotAllowed:
			if rec.Header().Get("Allow") != healthAllow {
				t.Fatalf("405 without Allow")
			}
		default:
			t.Fatalf("%s %s answered %d", method, path, rec.Code)
		}
		if rec.Body.Len() > 4096 {
			t.Fatalf("a probe answered %d bytes", rec.Body.Len())
		}
		if strings.Contains(rec.Body.String(), "deadline exceeded") {
			t.Fatalf("a check's error reached a probe: %s", rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("a probe response may be cached: %q", rec.Header().Get("Cache-Control"))
		}
	})
}
