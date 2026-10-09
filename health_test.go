package muzak

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// healthOptions returns quiet options with the health endpoints enabled.
func healthOptions(health HealthOptions) AppOptions {
	opts := quietOptions()
	health.Enabled = true
	opts.Health = health
	return opts
}

// readyApp builds an application with the given health options whose
// lifecycle has started, which is what readiness waits for.
func readyApp(t *testing.T, health HealthOptions) *App {
	t.Helper()
	app := New(healthOptions(health))
	app.Get("/x", okHandler)
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatalf("StartLifecycle = %v", err)
	}
	return app
}

// countingCheck is a readiness check that counts its runs and returns what err
// holds at the time.
type countingCheck struct {
	runs  atomic.Int32
	delay time.Duration
	err   atomic.Pointer[error]
}

func (c *countingCheck) check(ctx context.Context) error {
	c.runs.Add(1)
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := c.err.Load(); err != nil {
		return *err
	}
	return nil
}

func (c *countingCheck) fail(err error) { c.err.Store(&err) }

func TestHealthDisabledServesNothing(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, New(quietOptions()))
	for _, path := range []string{DefaultLivenessPath, DefaultReadinessPath} {
		assertStatus(t, do(t, app, http.MethodGet, path), http.StatusNotFound)
		if app.isHealthPath(path) {
			t.Errorf("isHealthPath(%q) = true with health disabled", path)
		}
	}
	if app.health != nil {
		t.Error("the health endpoints were prepared although they are disabled")
	}
}

func TestHealthLivenessAndReadinessAnswer(t *testing.T) {
	t.Parallel()
	app := readyApp(t, HealthOptions{})
	for _, path := range []string{DefaultLivenessPath, DefaultReadinessPath} {
		rec := do(t, app, http.MethodGet, path)
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Body.String(); got != `{"status":"ok"}` {
			t.Errorf("%s body = %q", path, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Errorf("%s Content-Type = %q", path, got)
		}
		if rec.Header().Get(HeaderRequestID) == "" {
			t.Errorf("%s carries no request identifier", path)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s is not served with the security headers", path)
		}
		if !app.isHealthPath(path) {
			t.Errorf("isHealthPath(%q) = false", path)
		}
	}
	if app.isHealthPath("/x") || app.isHealthPath("/livez/") {
		t.Error("isHealthPath reports a path that is not a health endpoint")
	}
	// A query string does not change the path, and a trailing slash does.
	assertStatus(t, do(t, app, http.MethodGet, "/readyz?verbose=1"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, "/readyz/"), http.StatusNotFound)
}

func TestHealthCustomPaths(t *testing.T) {
	t.Parallel()
	app := readyApp(t, HealthOptions{LivenessPath: "/_/alive", ReadinessPath: "/_/ready"})
	assertStatus(t, do(t, app, http.MethodGet, "/_/alive"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, "/_/ready"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, DefaultLivenessPath), http.StatusNotFound)
}

func TestHealthMethods(t *testing.T) {
	t.Parallel()
	app := readyApp(t, HealthOptions{})
	for _, path := range []string{DefaultLivenessPath, DefaultReadinessPath} {
		head := do(t, app, http.MethodHead, path)
		assertStatus(t, head, http.StatusOK)
		if head.Body.Len() != 0 {
			t.Errorf("HEAD %s wrote a body: %q", path, head.Body.String())
		}
		if got := head.Header().Get("Content-Length"); got != "15" {
			t.Errorf("HEAD %s Content-Length = %q, want the GET body's 15", path, got)
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch, "PROPFIND"} {
			rec := do(t, app, method, path)
			assertStatus(t, rec, http.StatusMethodNotAllowed)
			if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("%s %s Allow = %q", method, path, got)
			}
			if body := decodeError(t, rec); body.Error.Code != CodeMethodNotAllowed || body.RequestID == "" {
				t.Errorf("%s %s error = %+v", method, path, body)
			}
		}
	}
}

func TestHealthRefusedMethodIsRenderedByTheApplicationRenderer(t *testing.T) {
	t.Parallel()
	opts := healthOptions(HealthOptions{})
	opts.ErrorRenderer = func(c *Context, err error) (int, any) {
		return http.StatusMethodNotAllowed, map[string]string{"custom": err.Error()}
	}
	app := mustBuild(t, New(opts))
	rec := do(t, app, http.MethodPost, DefaultLivenessPath)
	assertStatus(t, rec, http.StatusMethodNotAllowed)
	if !strings.Contains(rec.Body.String(), `"custom"`) {
		t.Errorf("body = %s, want the custom renderer's", rec.Body.String())
	}
}

func TestHealthReadinessFollowsTheLifecycleByHand(t *testing.T) {
	t.Parallel()
	var probedDuringStart atomic.Int32
	var app *App
	component := NewLifecycle("db", func(context.Context) error {
		// Starting is exactly when readiness must not claim the application
		// is ready, and liveness must say it is alive.
		probedDuringStart.Store(int32(do(t, app, http.MethodGet, DefaultReadinessPath).Code))
		assertStatus(t, do(t, app, http.MethodGet, DefaultLivenessPath), http.StatusOK)
		return nil
	}, nil)
	app = New(healthOptions(HealthOptions{}), WithLifecycle(component))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, DefaultReadinessPath)
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if got := rec.Body.String(); got != `{"status":"unavailable"}` {
		t.Errorf("body = %q", got)
	}
	assertStatus(t, do(t, app, http.MethodGet, DefaultLivenessPath), http.StatusOK)

	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := probedDuringStart.Load(); got != http.StatusServiceUnavailable {
		t.Errorf("readiness during Start = %d, want 503", got)
	}
	assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusOK)

	if err := app.StopLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusServiceUnavailable)
	assertStatus(t, do(t, app, http.MethodGet, DefaultLivenessPath), http.StatusOK)
}

func TestHealthReadinessStaysDownWhenStartFails(t *testing.T) {
	t.Parallel()
	broken := &recorder{name: "broken", startErr: io.ErrUnexpectedEOF}
	app := New(healthOptions(HealthOptions{}), WithLifecycle(broken))
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err == nil {
		t.Fatal("StartLifecycle succeeded")
	}
	assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusServiceUnavailable)
}

// TestHealthReadinessAcrossARun follows readiness through a whole run: ready
// while serving, unavailable from the moment a shutdown begins while the
// listener still serves through DrainDelay, and ready again on a second run.
func TestHealthReadinessAcrossARun(t *testing.T) {
	t.Parallel()
	opts := healthOptions(HealthOptions{})
	opts.Addr = "127.0.0.1:0"
	opts.DrainDelay = 400 * time.Millisecond
	opts.ShutdownTimeout = 5 * time.Second
	stopped := &recorder{name: "resource"}
	app := New(opts, WithLifecycle(stopped))
	app.Get("/x", okHandler)

	for run := range 2 {
		done := make(chan error, 1)
		go func() { done <- app.Run() }()
		var addr string
		waitFor(t, func() bool {
			addr = app.Addr()
			if addr == "" {
				return false
			}
			status, _, err := fetchOverTheWire(t, "http://"+addr+DefaultReadinessPath)
			return err == nil && status == http.StatusOK
		}, "the run to report itself ready")

		began := time.Now()
		shutdown := make(chan error, 1)
		go func() { shutdown <- app.Shutdown(context.Background()) }()
		// The listener is still open during the delay, and answers.
		waitFor(t, func() bool {
			status, _, err := fetchOverTheWire(t, "http://"+addr+DefaultReadinessPath)
			return err == nil && status == http.StatusServiceUnavailable
		}, "readiness to report unavailable while the listener is still open")
		if status, _, err := fetchOverTheWire(t, "http://"+addr+DefaultLivenessPath); err != nil || status != http.StatusOK {
			t.Errorf("run %d: liveness during the drain delay = %d, %v", run, status, err)
		}
		if status, _, err := fetchOverTheWire(t, "http://"+addr+"/x"); err != nil || status != http.StatusOK {
			t.Errorf("run %d: a request during the drain delay = %d, %v", run, status, err)
		}
		if err := <-shutdown; err != nil {
			t.Fatalf("run %d: Shutdown = %v", run, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("run %d: Run = %v", run, err)
		}
		if took := time.Since(began); took < opts.DrainDelay {
			t.Errorf("run %d: the shutdown took %s, less than the DrainDelay", run, took)
		}
		if started, stopped := stopped.counts(); started != run+1 || stopped != run+1 {
			t.Errorf("run %d: component started %d and stopped %d times", run, started, stopped)
		}
		assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusServiceUnavailable)
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}

// TestHealthReadinessDropsBeforeInFlightRequestsDrain checks that readiness
// reports unavailable from the start of a shutdown with no drain delay, while
// a request is still in flight and before any component stops.
func TestHealthReadinessDropsBeforeInFlightRequestsDrain(t *testing.T) {
	t.Parallel()
	opts := healthOptions(HealthOptions{})
	opts.Addr = "127.0.0.1:0"
	release := make(chan struct{})
	entered := make(chan struct{})
	var stopsAtProbe atomic.Int32
	stopsAtProbe.Store(-1)
	var stops atomic.Int32
	app := New(opts, WithLifecycle(NewLifecycle("db", nil, func(context.Context) error {
		stops.Add(1)
		return nil
	})))
	app.Get("/slow", func(ctx *Context, _ Empty) (rtOut, error) {
		close(entered)
		<-release
		return rtOut{OK: true}, nil
	})
	addr, done := startServer(t, app)
	go func() { _, _, _ = fetchOverTheWire(t, "http://"+addr+"/slow") }()
	<-entered

	shutdown := make(chan error, 1)
	go func() { shutdown <- app.Shutdown(context.Background()) }()
	waitFor(t, func() bool {
		return do(t, app, http.MethodGet, DefaultReadinessPath).Code == http.StatusServiceUnavailable
	}, "readiness to drop while a request is in flight")
	stopsAtProbe.Store(stops.Load())
	close(release)
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := stopsAtProbe.Load(); got != 0 {
		t.Errorf("a component had stopped %d times when readiness first dropped, want 0", got)
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
}

func TestHealthChecksDecideReadiness(t *testing.T) {
	t.Parallel()
	db := &countingCheck{}
	app := readyApp(t, HealthOptions{
		CacheInterval: -1,
		Checks:        []HealthCheck{{Name: "database", Check: db.check}},
	})
	assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusOK)

	db.fail(errors.New("dial tcp 10.0.0.5:5432: connect: password=hunter2 rejected"))
	rec := do(t, app, http.MethodGet, DefaultReadinessPath)
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if got := rec.Body.String(); got != `{"status":"unavailable"}` {
		t.Errorf("body = %q, want no detail", got)
	}
	// Liveness never depends on a check.
	assertStatus(t, do(t, app, http.MethodGet, DefaultLivenessPath), http.StatusOK)
	if got := db.runs.Load(); got != 2 {
		t.Errorf("the check ran %d times for two readiness probes and one liveness probe, want 2", got)
	}
}

// TestHealthBodiesNeverCarryErrorText is the attacker's view of a failing
// check: nothing it returned, panicked with or timed out on reaches a probe.
func TestHealthBodiesNeverCarryErrorText(t *testing.T) {
	t.Parallel()
	const secret = "postgres://admin:hunter2@10.0.0.5"
	for _, report := range []bool{false, true} {
		logger, logs := captureLogger(t)
		opts := healthOptions(HealthOptions{
			CacheInterval: -1,
			ReportChecks:  report,
			Checks: []HealthCheck{
				{Name: "db", Check: func(context.Context) error { return errors.New("connect " + secret) }},
				{Name: "cache", Check: func(context.Context) error { panic("redis " + secret) }},
				{Name: "queue", Timeout: 20 * time.Millisecond, Check: func(ctx context.Context) error {
					<-ctx.Done()
					return errors.New("queue " + secret)
				}},
				{Name: "ok", Check: func(context.Context) error { return nil }},
			},
		})
		opts.Logger = logger
		app := New(opts)
		mustBuild(t, app)
		if err := app.StartLifecycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			rec := do(t, app, method, DefaultReadinessPath)
			if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "redis") {
				t.Fatalf("%s readiness body leaks a check's error: %s", method, rec.Body.String())
			}
			for name, values := range rec.Header() {
				if strings.Contains(strings.Join(values, " "), "hunter2") {
					t.Fatalf("header %s leaks a check's error", name)
				}
			}
		}
		rec := do(t, app, http.MethodGet, DefaultReadinessPath)
		assertStatus(t, rec, http.StatusServiceUnavailable)
		if report {
			assertJSON(t, rec, `{"status":"unavailable","checks":{"db":"fail","cache":"fail","queue":"fail","ok":"ok"}}`)
			if !strings.HasPrefix(rec.Body.String(), `{"status":"unavailable","checks":{"db":"fail","cache":"fail","queue":"fail"`) {
				t.Errorf("checks are not listed in the order they were declared: %s", rec.Body.String())
			}
		}
		// The operator, unlike the prober, is told why.
		if !strings.Contains(logs.String(), "connect "+secret) || !strings.Contains(logs.String(), "recovered from a panic in a readiness check") {
			t.Errorf("the log does not say why readiness failed:\n%s", logs.String())
		}
	}
}

func TestHealthReportChecksWhenHealthy(t *testing.T) {
	t.Parallel()
	app := readyApp(t, HealthOptions{
		ReportChecks: true,
		Checks: []HealthCheck{
			{Name: "db.primary", Check: func(context.Context) error { return nil }},
			{Name: "cache_1", Check: func(context.Context) error { return nil }},
		},
	})
	rec := do(t, app, http.MethodGet, DefaultReadinessPath)
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != `{"status":"ok","checks":{"db.primary":"ok","cache_1":"ok"}}` {
		t.Errorf("body = %s", got)
	}
	// Liveness never lists checks.
	if got := do(t, app, http.MethodGet, DefaultLivenessPath).Body.String(); got != `{"status":"ok"}` {
		t.Errorf("liveness body = %s", got)
	}
}

// TestHealthProbeFloodRunsTheChecksOncePerInterval is the reason the result is
// shared: thousands of concurrent probes must not become thousands of pings.
func TestHealthProbeFloodRunsTheChecksOncePerInterval(t *testing.T) {
	// Not in parallel: thousands of requests at once would starve the timing
	// of tests running beside it.
	db := &countingCheck{delay: 50 * time.Millisecond}
	cache := &countingCheck{delay: 10 * time.Millisecond}
	app := readyApp(t, HealthOptions{
		CacheInterval: time.Minute,
		Checks: []HealthCheck{
			{Name: "db", Check: db.check},
			{Name: "cache", Check: cache.check},
		},
	})
	flood := func() {
		const probes = 3000
		var wg sync.WaitGroup
		var failures atomic.Int32
		start := make(chan struct{})
		for range probes {
			wg.Go(func() {
				<-start
				if do(t, app, http.MethodGet, DefaultReadinessPath).Code != http.StatusOK {
					failures.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		if n := failures.Load(); n > 0 {
			t.Errorf("%d probes were not answered 200", n)
		}
	}
	flood()
	if db.runs.Load() != 1 || cache.runs.Load() != 1 {
		t.Fatalf("a flood of probes ran the checks %d and %d times, want once each", db.runs.Load(), cache.runs.Load())
	}
	flood()
	if db.runs.Load() != 1 {
		t.Fatalf("a second flood within the interval ran the checks again (%d runs)", db.runs.Load())
	}
	// The interval passing is simulated rather than slept through, so that a
	// flood slowed down by the race detector cannot outlast it.
	checker := app.health.checker
	checker.mu.Lock()
	checker.cachedAt = time.Now().Add(-time.Hour)
	checker.mu.Unlock()
	flood()
	if db.runs.Load() != 2 || cache.runs.Load() != 2 {
		t.Fatalf("after the interval the checks ran %d and %d times in all, want twice each", db.runs.Load(), cache.runs.Load())
	}
}

// TestHealthHangingCheckIsBounded checks a check that ignores its context
// entirely: the probe is answered at the check's timeout, and the check is not
// started again, so no goroutine piles up behind it, until it returns.
func TestHealthHangingCheckIsBounded(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var runs atomic.Int32
	const timeout = time.Second
	app := readyApp(t, HealthOptions{
		CacheInterval: -1,
		Checks: []HealthCheck{{Name: "stuck", Timeout: timeout, Check: func(context.Context) error {
			runs.Add(1)
			<-release
			return nil
		}}},
	})
	began := time.Now()
	assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusServiceUnavailable)
	if took := time.Since(began); took < timeout || took > 5*timeout {
		t.Errorf("the probe took %s, want about the check's timeout of %s", took, timeout)
	}
	// Were the check started again, each of these would wait out its timeout,
	// and each would leave another goroutine parked behind it.
	for range 50 {
		began = time.Now()
		assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusServiceUnavailable)
		if took := time.Since(began); took > timeout/2 {
			t.Fatalf("a probe behind a check still running took %s; it should fail at once", took)
		}
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("the hung check was started %d times, want once", got)
	}
	close(release)
	waitFor(t, func() bool {
		return do(t, app, http.MethodGet, DefaultReadinessPath).Code == http.StatusOK
	}, "the check to be run again once it returned")
	if got := runs.Load(); got < 2 {
		t.Errorf("the check was not run again after it returned (%d runs)", got)
	}
}

func TestHealthCheckAnsweringLateCountsAsFailed(t *testing.T) {
	t.Parallel()
	// The check ignores its context and returns nil, but only after its
	// timeout; a longer check keeps the run waiting long enough to receive it.
	app := readyApp(t, HealthOptions{
		CacheInterval: -1,
		Checks: []HealthCheck{
			{Name: "late", Timeout: 20 * time.Millisecond, Check: func(context.Context) error {
				time.Sleep(60 * time.Millisecond)
				return nil
			}},
			// Its timeout is far beyond its sleep, which a loaded machine can
			// overrun by hundreds of milliseconds, so that it reports ok.
			{Name: "slow", Timeout: 5 * time.Second, Check: func(context.Context) error {
				time.Sleep(120 * time.Millisecond)
				return nil
			}},
		},
		ReportChecks: true,
	})
	rec := do(t, app, http.MethodGet, DefaultReadinessPath)
	assertStatus(t, rec, http.StatusServiceUnavailable)
	if got := rec.Body.String(); got != `{"status":"unavailable","checks":{"late":"fail","slow":"ok"}}` {
		t.Errorf("body = %s", got)
	}
}

func TestHealthCheckContextHasItsTimeout(t *testing.T) {
	t.Parallel()
	var deadline atomic.Int64
	app := readyApp(t, HealthOptions{Checks: []HealthCheck{{Name: "db", Check: func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if ok {
			deadline.Store(int64(time.Until(d)))
		}
		return nil
	}}}})
	assertStatus(t, do(t, app, http.MethodGet, DefaultReadinessPath), http.StatusOK)
	if got := time.Duration(deadline.Load()); got <= 0 || got > DefaultHealthCheckTimeout {
		t.Errorf("the check's context expires in %s, want at most the default timeout", got)
	}
}

func TestHealthFailureLoggingFollowsChangesOfState(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	db := &countingCheck{}
	opts := healthOptions(HealthOptions{CacheInterval: -1, Checks: []HealthCheck{{Name: "db", Check: db.check}}})
	opts.Logger = logger
	app := New(opts)
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.fail(errors.New("refused"))
	for range 20 {
		do(t, app, http.MethodGet, DefaultReadinessPath)
	}
	db.err.Store(nil)
	do(t, app, http.MethodGet, DefaultReadinessPath)
	text := logs.String()
	if n := strings.Count(text, `"level":"WARN","msg":"muzak: a readiness check failed`); n != 1 {
		t.Errorf("a check failing on 20 probes was logged at warn level %d times, want once:\n%s", n, text)
	}
	if n := strings.Count(text, "a readiness check is still failing"); n != 19 {
		t.Errorf("the repeated failures were logged %d times at debug level, want 19", n)
	}
	if !strings.Contains(text, "a readiness check passes again") {
		t.Error("the recovery was not logged")
	}
}

func TestHealthWaitingProbeGivesUpWithItsClient(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	checker := newHealthChecker(HealthOptions{
		CacheInterval: time.Hour,
		Checks: []HealthCheck{{Name: "slow", Timeout: 5 * time.Second, Check: func(context.Context) error {
			<-release
			return nil
		}}},
	}, slog.New(discardHandler{}))
	leader := make(chan *healthReport, 1)
	go func() { leader <- checker.result(context.Background()) }()
	waitFor(t, func() bool {
		checker.mu.Lock()
		defer checker.mu.Unlock()
		return checker.flight != nil
	}, "the first probe to start the checks")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report := checker.result(ctx); report != nil {
		t.Errorf("a probe whose client left got %+v, want nil", report)
	}
	close(release)
	if report := <-leader; report == nil || report.status != http.StatusOK {
		t.Errorf("the leading probe got %+v", report)
	}
	// Cached now, so a probe whose client has left is still answered.
	if report := checker.result(ctx); report == nil {
		t.Error("a cached result was not served")
	}
	checker.forget()
	checker.mu.Lock()
	cached := checker.cached
	checker.mu.Unlock()
	if cached != nil {
		t.Error("forget kept the cached result")
	}
}

func TestHealthReadinessIgnoresAResultFoundAfterShutdownBegan(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	app := readyApp(t, HealthOptions{Checks: []HealthCheck{{Name: "db", Check: func(context.Context) error {
		close(entered)
		<-release
		return nil
	}}}})
	got := make(chan int, 1)
	go func() { got <- do(t, app, http.MethodGet, DefaultReadinessPath).Code }()
	<-entered
	app.readiness.draining.Store(true)
	close(release)
	if status := <-got; status != http.StatusServiceUnavailable {
		t.Errorf("a probe whose checks passed after the shutdown began = %d, want 503", status)
	}
}

// TestHealthIsAnsweredAheadOfTheApplication checks that nothing standing in
// front of the routes refuses a probe: a middleware installed with Use, an
// application-wide guard, the rate limit or the CORS policy.
func TestHealthIsAnsweredAheadOfTheApplication(t *testing.T) {
	t.Parallel()
	opts := healthOptions(HealthOptions{})
	opts.RateLimit = RateLimitOptions{Quotas: []Quota{{Name: "all", Limit: 1, Window: time.Hour}}}
	opts.CORS = CORSOptions{AllowedOrigins: []string{"https://app.example.com"}}
	var reached atomic.Int32
	app := New(opts, WithDependencies(func(*Context) error {
		reached.Add(1)
		return Unauthorized("")
	}))
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached.Add(1)
			http.Error(w, "denied", http.StatusForbidden)
		})
	})
	app.Get("/x", okHandler)
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		req := httptest.NewRequest(http.MethodGet, DefaultReadinessPath, nil)
		req.Header.Set("Origin", "https://app.example.com")
		rec := doRequest(t, app, req)
		assertStatus(t, rec, http.StatusOK)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("the CORS policy answered a probe")
		}
		assertStatus(t, do(t, app, http.MethodGet, DefaultLivenessPath), http.StatusOK)
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("a guard or middleware ran %d times for probes", n)
	}
	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusForbidden)
}

func TestHealthIsNotDocumented(t *testing.T) {
	t.Parallel()
	app := readyApp(t, HealthOptions{})
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "livez") || strings.Contains(string(raw), "readyz") {
		t.Errorf("the OpenAPI document describes a health endpoint: %s", raw)
	}
}

func TestHealthAccessLog(t *testing.T) {
	t.Parallel()
	probe := func(t *testing.T, mutate func(*AppOptions), method, path string) string {
		t.Helper()
		logger, logs := captureLogger(t)
		opts := healthOptions(HealthOptions{})
		opts.Logger = logger
		mutate(&opts)
		app := mustBuild(t, New(opts))
		do(t, app, method, path)
		return logs.String()
	}
	t.Run("debug by default whatever the status", func(t *testing.T) {
		for _, path := range []string{DefaultLivenessPath, DefaultReadinessPath} {
			text := probe(t, func(*AppOptions) {}, http.MethodGet, path)
			if !strings.Contains(text, `"level":"DEBUG","msg":"GET `+path+`"`) {
				t.Errorf("probe of %s not logged at debug level:\n%s", path, text)
			}
			if strings.Contains(text, `"level":"ERROR"`) || strings.Contains(text, `"level":"WARN"`) {
				t.Errorf("a probe raised the log level:\n%s", text)
			}
		}
		text := probe(t, func(*AppOptions) {}, http.MethodDelete, DefaultLivenessPath)
		if !strings.Contains(text, `"level":"DEBUG","msg":"DELETE /livez"`) || !strings.Contains(text, `"status":405`) {
			t.Errorf("a refused probe is not recorded with its status:\n%s", text)
		}
	})
	t.Run("configured level", func(t *testing.T) {
		text := probe(t, func(o *AppOptions) { o.Health.AccessLogLevel = slog.LevelInfo }, http.MethodGet, DefaultReadinessPath)
		if !strings.Contains(text, `"level":"INFO","msg":"GET /readyz"`) || !strings.Contains(text, `"status":503`) {
			t.Errorf("probe not logged at the configured level:\n%s", text)
		}
	})
	t.Run("skipped path", func(t *testing.T) {
		text := probe(t, func(o *AppOptions) { o.AccessLogOptions.SkipPaths = []string{DefaultLivenessPath} }, http.MethodGet, DefaultLivenessPath)
		if strings.Contains(text, "GET /livez") {
			t.Errorf("a skipped path was logged:\n%s", text)
		}
	})
	t.Run("access log disabled", func(t *testing.T) {
		text := probe(t, func(o *AppOptions) { o.DisableAccessLog = true }, http.MethodGet, DefaultReadinessPath)
		if strings.Contains(text, "GET /readyz") {
			t.Errorf("a probe was logged with the access log disabled:\n%s", text)
		}
	})
	t.Run("hostile method is cut to length", func(t *testing.T) {
		method := strings.Repeat("X", 5000)
		text := probe(t, func(*AppOptions) {}, method, DefaultLivenessPath)
		if strings.Contains(text, strings.Repeat("X", maxQuotedLength+1)) {
			t.Error("a client-chosen method was logged at full length")
		}
	})
}

func TestHealthBuildErrors(t *testing.T) {
	t.Parallel()
	nop := func(context.Context) error { return nil }
	cases := []struct {
		name   string
		opts   func(*AppOptions)
		routes func(*App)
		want   string
	}{
		{"configured but not enabled", func(o *AppOptions) {
			o.Health = HealthOptions{Checks: []HealthCheck{{Name: "db", Check: nop}}}
		}, nil, "HealthOptions.Enabled is false"},
		{"report without enabled", func(o *AppOptions) { o.Health = HealthOptions{ReportChecks: true} }, nil, "HealthOptions.Enabled is false"},
		{"same paths", func(o *AppOptions) { o.Health.LivenessPath, o.Health.ReadinessPath = "/health", "/health" }, nil, "are both \"/health\""},
		{"relative", func(o *AppOptions) { o.Health.LivenessPath = "livez" }, nil, "write it as \"/livez\""},
		{"needs encoding", func(o *AppOptions) { o.Health.ReadinessPath = "/ready z" }, nil, "holds \" \""},
		{"control character", func(o *AppOptions) { o.Health.ReadinessPath = "/ready\nz" }, nil, "holds \"\\n\""},
		{"trailing slash", func(o *AppOptions) { o.Health.LivenessPath = "/livez/" }, nil, "write it as \"/livez\""},
		{"dot segment", func(o *AppOptions) { o.Health.LivenessPath = "/a/../livez" }, nil, "not in its clean form"},
		{"exact route", nil, func(a *App) { a.Get("/livez", okHandler) }, "the route GET /livez also answers"},
		{"parameter route", nil, func(a *App) {
			a.Get("/{name}", okHandler)
			a.Post("/{name}", okHandler)
		}, "the route GET, POST /{name} also answers"},
		{"frontend mount", func(o *AppOptions) { o.Health.ReadinessPath = "/app/readyz" }, func(a *App) {
			a.Frontend("/app", FrontendOptions{FS: fstest.MapFS{"index.html": {Data: []byte("x")}}})
		}, "lies under the frontend mounted at \"/app\""},
		// A frontend or a handler at the root answers only what nothing else
		// does, except its own root, which a probe at "/" would take from it:
		// the home page of a single page application answered with
		// {"status":"ok"}.
		{"root of a frontend at the root", func(o *AppOptions) { o.Health.LivenessPath = "/" }, func(a *App) {
			a.Frontend("/", FrontendOptions{FS: fstest.MapFS{"index.html": {Data: []byte("x")}}})
		}, "lies under the frontend mounted at \"/\""},
		{"root of a handler at the root", func(o *AppOptions) { o.Health.ReadinessPath = "/" }, func(a *App) {
			a.Mount("/", http.NotFoundHandler())
		}, "lies under the handler mounted at \"/\""},
		{"openapi path", func(o *AppOptions) { o.Health.LivenessPath = "/openapi.json" }, nil, "OpenAPI document"},
		{"docs path", func(o *AppOptions) { o.Health.LivenessPath = "/docs" }, nil, "documentation UI"},
		{"under docs", func(o *AppOptions) { o.Health.LivenessPath = "/docs/livez" }, nil, "documentation UI"},
		{"unnamed check", func(o *AppOptions) { o.Health.Checks = []HealthCheck{{Check: nop}} }, nil, "Checks[0] has no Name"},
		{"bad name", func(o *AppOptions) { o.Health.Checks = []HealthCheck{{Name: "db\"}", Check: nop}} }, nil, "is named"},
		{"long name", func(o *AppOptions) {
			o.Health.Checks = []HealthCheck{{Name: strings.Repeat("a", maxHealthCheckName+1), Check: nop}}
		}, nil, "at most 64"},
		{"duplicate", func(o *AppOptions) {
			o.Health.Checks = []HealthCheck{{Name: "db", Check: nop}, {Name: "db", Check: nop}}
		}, nil, "two checks named \"db\""},
		{"nil check", func(o *AppOptions) { o.Health.Checks = []HealthCheck{{Name: "db"}} }, nil, "has no Check function"},
		{"negative timeout", func(o *AppOptions) {
			o.Health.Checks = []HealthCheck{{Name: "db", Check: nop, Timeout: -time.Second}}
		}, nil, "negative Timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.Health.Enabled = true
			if tc.opts != nil {
				tc.opts(&opts)
			}
			app := New(opts)
			if tc.routes != nil {
				tc.routes(app)
			}
			if msg := buildError(t, app); !strings.Contains(msg, tc.want) {
				t.Errorf("build error = %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

func TestHealthPathsThatDoNotCollide(t *testing.T) {
	t.Parallel()
	frontend := FrontendOptions{FS: fstest.MapFS{"index.html": {Data: []byte("<p>app</p>")}}}
	t.Run("a frontend at the root", func(t *testing.T) {
		t.Parallel()
		app := New(healthOptions(HealthOptions{}))
		app.Frontend("/", frontend)
		mustBuild(t, app)
		assertStatus(t, do(t, app, http.MethodGet, DefaultLivenessPath), http.StatusOK)
	})
	t.Run("documentation disabled", func(t *testing.T) {
		t.Parallel()
		opts := healthOptions(HealthOptions{LivenessPath: "/openapi.json"})
		opts.DisableDocs = true
		mustBuild(t, New(opts))
	})
	t.Run("docs path without a UI", func(t *testing.T) {
		t.Parallel()
		opts := healthOptions(HealthOptions{LivenessPath: "/docs"})
		opts.DocsUI = nil
		mustBuild(t, New(opts))
	})
	t.Run("the root of an application nothing else answers there", func(t *testing.T) {
		t.Parallel()
		// What a load balancer that probes "/" by default needs.
		app := New(healthOptions(HealthOptions{LivenessPath: "/"}))
		app.Get("/users/{id}", okHandler)
		app.Frontend("/app", frontend)
		app.Mount("/legacy", http.NotFoundHandler())
		mustBuild(t, app)
		assertStatus(t, do(t, app, http.MethodGet, "/"), http.StatusOK)
	})
	t.Run("a sibling of a parameter route", func(t *testing.T) {
		t.Parallel()
		app := New(healthOptions(HealthOptions{}))
		app.Get("/users/{id}", okHandler)
		mustBuild(t, app)
	})
}

// TestHealthCostsNothingWhenOffOrForOtherPaths compares the allocations of an
// ordinary request with health disabled and enabled: the endpoints must cost
// the rest of the application nothing.
func TestHealthCostsNothingWhenOffOrForOtherPaths(t *testing.T) {
	skipAllocationCountsUnderRace(t)
	measure := func(opts AppOptions) float64 {
		app := New(opts)
		app.Get("/x", okHandler)
		mustBuild(t, app)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		return testing.AllocsPerRun(200, func() {
			app.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
	off := measure(quietOptions())
	on := measure(healthOptions(HealthOptions{Checks: []HealthCheck{{Name: "db", Check: func(context.Context) error { return nil }}}}))
	if on != off {
		t.Errorf("an ordinary request allocates %v times with health enabled and %v without", on, off)
	}

	app := readyApp(t, HealthOptions{})
	req := httptest.NewRequest(http.MethodGet, DefaultReadinessPath, nil)
	probe := testing.AllocsPerRun(200, func() { app.ServeHTTP(httptest.NewRecorder(), req) })
	if probe > off {
		t.Errorf("a readiness probe allocates %v times, more than an ordinary request's %v", probe, off)
	}
}

func TestHealthOptionsDefaults(t *testing.T) {
	t.Parallel()
	got := HealthOptions{Enabled: true}.withDefaults()
	if got.LivenessPath != DefaultLivenessPath || got.ReadinessPath != DefaultReadinessPath ||
		got.CacheInterval != DefaultHealthCacheInterval || got.AccessLogLevel != slog.LevelDebug {
		t.Errorf("defaults = %+v", got)
	}
	if off := (HealthOptions{}).withDefaults(); off.configured() {
		t.Errorf("a disabled configuration was given defaults: %+v", off)
	}
	if !(HealthOptions{AccessLogLevel: slog.LevelInfo}).configured() || !(HealthOptions{CacheInterval: time.Second}).configured() ||
		!(HealthOptions{LivenessPath: "/a"}).configured() || !(HealthOptions{ReadinessPath: "/b"}).configured() {
		t.Error("configured misses a field")
	}
}

func TestDescribeEntryFallsBackToTheMethods(t *testing.T) {
	t.Parallel()
	entry := &pathEntry{methods: map[string][]*Route{http.MethodGet: nil}}
	if got := describeEntry(map[string]*pathEntry{}, entry); got != "GET" {
		t.Errorf("describeEntry = %q", got)
	}
}

func TestNoGoroutineLeaksFromHealthChecks(t *testing.T) {
	app := readyApp(t, HealthOptions{
		CacheInterval: -1,
		Checks: []HealthCheck{
			{Name: "slow", Timeout: 30 * time.Millisecond, Check: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}},
			{Name: "panics", Check: func(context.Context) error { panic("boom") }},
		},
	})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { do(t, app, http.MethodGet, DefaultReadinessPath) })
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)
	assertNoGoroutineLeaks(t)
}
