package muzak

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// skipAllocationCountsUnderRace skips a test that compares exact allocation
// counts when it cannot, which is under the race detector. Without it the
// test runs, and the suite runs without -race as well as with it.
func skipAllocationCountsUnderRace(t *testing.T) {
	t.Helper()
	if raceDetector {
		t.Skip("sync.Pool drops a share of its items under -race, so allocation counts are only exact without it")
	}
}

// waitOut is a handler that waits for its context to end and returns what
// wrap makes of the context's error.
func waitOut(wrap func(ctx *Context, err error) error) Handler[Empty, rtOut] {
	return func(ctx *Context, _ Empty) (rtOut, error) {
		<-ctx.Context().Done()
		return rtOut{}, wrap(ctx, ctx.Context().Err())
	}
}

func TestTimeoutAnswers503WhenTheDeadlineFailsTheRequest(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	same := func(_ *Context, err error) error { return err }
	app.Get("/plain", waitOut(same), Timeout(30*time.Millisecond))
	app.Get("/wrapped", waitOut(func(_ *Context, err error) error {
		return fmt.Errorf("querying the ledger: %w", err)
	}), Timeout(30*time.Millisecond))
	app.Get("/cause", waitOut(func(ctx *Context, _ error) error {
		return context.Cause(ctx.Context())
	}), Timeout(30*time.Millisecond))
	app.Get("/provider", okHandler, Timeout(30*time.Millisecond), Needs(func(ctx *Context) (*int, error) {
		<-ctx.Context().Done()
		return nil, ctx.Context().Err()
	}))
	mustBuild(t, app)
	for _, path := range []string{"/plain", "/wrapped", "/cause", "/provider"} {
		began := time.Now()
		rec := do(t, app, http.MethodGet, path)
		assertStatus(t, rec, http.StatusServiceUnavailable)
		if took := time.Since(began); took < 30*time.Millisecond {
			t.Errorf("%s answered after %s, before its deadline", path, took)
		}
		if got := rec.Header().Get(HeaderRetryAfter); got != "1" {
			t.Errorf("%s Retry-After = %q, want 1", path, got)
		}
		body := decodeError(t, rec)
		if body.Error.Code != CodeServiceUnavailable || body.RequestID == "" {
			t.Errorf("%s error = %+v", path, body)
		}
		if strings.Contains(rec.Body.String(), "ledger") || strings.Contains(rec.Body.String(), "deadline exceeded") {
			t.Errorf("%s leaks its cause to the client: %s", path, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "querying the ledger: context deadline exceeded") {
		t.Errorf("the cause of a timeout was not logged:\n%s", logs.String())
	}
	// HEAD runs the GET route, deadline included.
	assertStatus(t, do(t, app, http.MethodHead, "/plain"), http.StatusServiceUnavailable)
}

func TestTimeoutIsRenderedByTheApplicationRenderer(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ErrorRenderer = func(c *Context, err error) (int, any) {
		var coder StatusCoder
		if !errors.As(err, &coder) {
			return http.StatusInternalServerError, nil
		}
		return coder.HTTPStatus(), map[string]any{"problem": coder.HTTPStatus()}
	}
	app := New(opts)
	app.Get("/x", waitOut(func(_ *Context, err error) error { return err }), Timeout(10*time.Millisecond))
	mustBuild(t, app)
	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusServiceUnavailable)
	assertJSON(t, rec, `{"problem":503}`)
}

func TestTimeoutSendsASuccessThatIgnoredTheDeadline(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	var expired atomic.Bool
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		time.Sleep(60 * time.Millisecond)
		expired.Store(ctx.Context().Err() != nil)
		return rtOut{OK: true}, nil
	}, Timeout(10*time.Millisecond))
	mustBuild(t, app)
	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"ok":true}`)
	if !expired.Load() {
		t.Error("the handler's context had not expired after its deadline")
	}
}

func TestTimeoutAbortsAResponseThatHadStarted(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/stream", func(ctx *Context, _ Empty) (rtOut, error) {
		w := ctx.ResponseWriter()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"row":1},`))
		<-ctx.Context().Done()
		return rtOut{}, ctx.Context().Err()
	}, Timeout(20*time.Millisecond))
	mustBuild(t, app)
	rec := httptest.NewRecorder()
	recovered := catchPanic(func() { app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil)) })
	if recovered != http.ErrAbortHandler { //nolint:errorlint // comparing the recovered sentinel
		t.Fatalf("a deadline after the response started was not aborted: %v", recovered)
	}
	if rec.Code != http.StatusOK || rec.Header().Get(HeaderRetryAfter) != "" {
		t.Errorf("a started response was turned into a %d with Retry-After %q", rec.Code, rec.Header().Get(HeaderRetryAfter))
	}
}

func TestTimeoutLeavesOtherFailuresAlone(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	// An error that chose its own status keeps it.
	app.Get("/gateway", waitOut(func(_ *Context, err error) error {
		return GatewayTimeout("the ledger did not answer").Wrap(err)
	}), Timeout(10*time.Millisecond))
	// A shorter deadline the handler set itself is not the route's.
	app.Get("/own", func(ctx *Context, _ Empty) (rtOut, error) {
		inner, cancel := context.WithTimeout(ctx.Context(), time.Millisecond)
		defer cancel()
		<-inner.Done()
		return rtOut{}, inner.Err()
	}, Timeout(time.Minute))
	// A route without a deadline that fails with one is an ordinary failure.
	app.Get("/none", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, context.DeadlineExceeded
	})
	// The deadline passing does not turn an unrelated error into a timeout.
	app.Get("/unrelated", waitOut(func(_ *Context, _ error) error { return errors.New("disk full") }), Timeout(10*time.Millisecond))
	entered := make(chan struct{})
	app.Get("/left", func(ctx *Context, _ Empty) (rtOut, error) {
		close(entered)
		<-ctx.Context().Done()
		return rtOut{}, ctx.Context().Err()
	}, Timeout(time.Minute))
	mustBuild(t, app)

	assertStatus(t, do(t, app, http.MethodGet, "/gateway"), http.StatusGatewayTimeout)
	for _, path := range []string{"/own", "/none", "/unrelated"} {
		rec := do(t, app, http.MethodGet, path)
		assertStatus(t, rec, http.StatusInternalServerError)
		if rec.Header().Get(HeaderRetryAfter) != "" {
			t.Errorf("%s was answered with a Retry-After", path)
		}
	}
	// A client that leaves cancels the context too, which is not the
	// deadline passing.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	rec := doRequest(t, app, httptest.NewRequest(http.MethodGet, "/left", nil).WithContext(ctx))
	if rec.Code == http.StatusServiceUnavailable {
		t.Error("a client leaving was reported as the route's deadline")
	}
}

func TestTimeoutIsVisibleToEverythingTheRouteRuns(t *testing.T) {
	t.Parallel()
	var guardLeft, providerLeft, handlerLeft atomic.Int64
	remaining := func(ctx *Context, into *atomic.Int64) {
		if deadline, ok := ctx.Context().Deadline(); ok {
			into.Store(int64(time.Until(deadline)))
		}
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		remaining(ctx, &handlerLeft)
		if ctx.Request().Context() != ctx.Context() {
			t.Error("Request and Context disagree about the request's context")
		}
		return rtOut{OK: true}, nil
	}, Timeout(time.Hour),
		WithDependencies(func(ctx *Context) error { remaining(ctx, &guardLeft); return nil }),
		Needs(func(ctx *Context) (*int, error) { remaining(ctx, &providerLeft); return new(int), nil }))
	app.Get("/none", func(ctx *Context, _ Empty) (rtOut, error) {
		if _, ok := ctx.Context().Deadline(); ok {
			t.Error("a route without a Timeout has a deadline")
		}
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)
	assertStatus(t, do(t, app, http.MethodGet, "/x"), http.StatusOK)
	assertStatus(t, do(t, app, http.MethodGet, "/none"), http.StatusOK)
	for name, left := range map[string]*atomic.Int64{"guard": &guardLeft, "provider": &providerLeft, "handler": &handlerLeft} {
		if got := time.Duration(left.Load()); got <= 59*time.Minute || got > time.Hour {
			t.Errorf("the %s saw %s left, want just under the route's hour", name, got)
		}
	}
}

func TestTimeoutNarrowerDeclarationWins(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Timeout(time.Second))
	app.Get("/app", okHandler)
	app.Get("/longer", okHandler, Timeout(time.Hour))
	app.Get("/removed", okHandler, Timeout(-1))
	app.Get("/zero", okHandler, Timeout(0))
	inner := NewRouter(Timeout(2 * time.Second))
	inner.Get("/router", okHandler)
	inner.Get("/router-removed", okHandler, Timeout(-time.Second))
	app.Include(inner, WithPrefix("/in"))
	atInclude := NewRouter()
	atInclude.Get("/at", okHandler)
	app.Include(atInclude, WithPrefix("/inc"), Timeout(3*time.Second))
	cleared := NewRouter(Timeout(-1))
	cleared.Get("/c", okHandler)
	app.Include(cleared, WithPrefix("/cleared"))
	mustBuild(t, app)
	want := map[string]time.Duration{
		"/app":               time.Second,
		"/longer":            time.Hour,
		"/removed":           0,
		"/zero":              time.Second,
		"/in/router":         2 * time.Second,
		"/in/router-removed": 0,
		"/inc/at":            3 * time.Second,
		"/cleared/c":         0,
	}
	for _, rt := range app.routes {
		if got, ok := want[rt.Path]; ok && rt.timeout != got {
			t.Errorf("%s timeout = %s, want %s", rt.Path, rt.timeout, got)
		}
	}
}

func TestTimeoutOnStreamRoutes(t *testing.T) {
	t.Parallel()
	sse := func(ctx *Context, _ Empty, stream *SSEStream[string]) error { return nil }
	ws := func(ctx *Context, _ Empty, conn *WSConn) error { return nil }

	app := New(quietOptions())
	app.SSE("/events", sse, Timeout(time.Second))
	app.WS("/ws", ws, Timeout(time.Second))
	msg := buildError(t, app)
	for _, want := range []string{
		"muzak: SSE GET /events: Timeout cannot be declared",
		"bound it with SSEOptions.MaxLifetime instead",
		"muzak: WS /ws: Timeout cannot be declared",
		"bound it with WSOptions.MaxLifetime instead",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("build error %q does not mention %q", msg, want)
		}
	}

	// A router's deadline is meant for the requests beneath it, not for the
	// streams that happen to share it, and removing one is always allowed.
	app = New(quietOptions(), Timeout(time.Second))
	app.SSE("/events", sse)
	app.WS("/ws", ws)
	app.SSE("/removed", sse, Timeout(-1))
	app.Get("/x", okHandler)
	mustBuild(t, app)
	for _, rt := range app.routes {
		if rt.sse != nil || rt.websocket != nil {
			if rt.timeout != 0 {
				t.Errorf("%s %s inherited a deadline of %s", rt.Method, rt.Path, rt.timeout)
			}
		} else if rt.timeout != time.Second {
			t.Errorf("%s %s timeout = %s", rt.Method, rt.Path, rt.timeout)
		}
	}
}

func TestTimeoutIsDocumented(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/timed", okHandler, Timeout(time.Second))
	app.Get("/declared", okHandler, Timeout(time.Second), WithResponseDoc(http.StatusServiceUnavailable, "Our own words."))
	app.Get("/untimed", okHandler)
	mustBuild(t, app)
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Description string `json:"description"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &parsed, json.RejectUnknownMembers(false)); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Paths["/timed"]["get"].Responses["503"].Description; !strings.Contains(got, "took longer than this operation allows") {
		t.Errorf("the timed route documents 503 as %q", got)
	}
	if got := parsed.Paths["/declared"]["get"].Responses["503"].Description; got != "Our own words." {
		t.Errorf("a route's own 503 was replaced with %q", got)
	}
	if _, ok := parsed.Paths["/untimed"]["get"].Responses["503"]; ok {
		t.Error("a route without a deadline documents a 503")
	}
}

// TestTimeoutCostsNothingWhenUnset compares a route without a deadline in an
// application where no route has one against a route whose inherited deadline
// was removed: neither may pay for the option existing.
func TestTimeoutCostsNothingWhenUnset(t *testing.T) {
	skipAllocationCountsUnderRace(t)
	measure := func(app *App, path string) float64 {
		mustBuild(t, app)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		return testing.AllocsPerRun(200, func() { app.ServeHTTP(httptest.NewRecorder(), req) })
	}
	plain := New(quietOptions())
	plain.Get("/x", okHandler)
	removed := New(quietOptions(), Timeout(time.Second))
	removed.Get("/x", okHandler, Timeout(-1))
	timed := New(quietOptions())
	timed.Get("/x", okHandler, Timeout(time.Second))
	base := measure(plain, "/x")
	if got := measure(removed, "/x"); got != base {
		t.Errorf("a route whose deadline was removed allocates %v times, a plain one %v", got, base)
	}
	// A deadline costs a context and a request copy, and nothing per byte.
	if got := measure(timed, "/x"); got > base+6 {
		t.Errorf("a route with a deadline allocates %v times against %v without", got, base)
	}
}
