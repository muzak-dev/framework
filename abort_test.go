package muzak

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// abortServer serves app on a real socket, capturing what net/http itself
// logs so a test can assert that an aborted response is not reported a second
// time as a crash.
func abortServer(t *testing.T, app *App) (*httptest.Server, *syncBuffer) {
	t.Helper()
	serverLog := &syncBuffer{}
	server := httptest.NewUnstartedServer(app)
	server.Config.ErrorLog = log.New(serverLog, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	return server, serverLog
}

// writeRowsThen is a handler that streams two CSV rows, flushes them so they
// are unmistakably on the wire, and then fails the way fail says.
func writeRowsThen(fail func() error) func(*Context, Empty) (rtOut, error) {
	return func(ctx *Context, _ Empty) (rtOut, error) {
		w := ctx.ResponseWriter()
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "id,amount\n1,100\n2,200\n")
		_ = http.NewResponseController(w).Flush()
		return rtOut{}, fail()
	}
}

// TestFailureAfterTheResponseStartedAbortsTheConnection is the regression test
// for a failure mid-stream that was swallowed: the handler's panic (or error)
// was logged and the response then ended cleanly, so a client reading an
// export saw a short body that looked complete. It must now see the transfer
// fail, the failure must be logged exactly once by Muzak and not at all by
// net/http, and the access log must say the connection was aborted.
func TestFailureAfterTheResponseStartedAbortsTheConnection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler func(*Context, Empty) (rtOut, error)
		logged  string
	}{
		{
			name: "a handler that panics",
			handler: writeRowsThen(func() error {
				// Rows 3 to N are never written.
				panic("row 3 could not be formatted")
			}),
			logged: "row 3 could not be formatted",
		},
		{
			name:    "a handler that returns an error",
			handler: writeRowsThen(func() error { return errors.New("rows.Err: connection reset by the database") }),
			logged:  "rows.Err: connection reset by the database",
		},
		{
			name:    "a handler that returns a deliberate 4xx",
			handler: writeRowsThen(func() error { return Conflict("the export changed underneath") }),
			logged:  "the export changed underneath",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := captureLogger(t)
			opts := quietOptions()
			opts.Logger = logger
			app := New(opts)
			app.Get("/export", tc.handler)
			mustBuild(t, app)
			server, serverLog := abortServer(t, app)

			status, body, err := fetchOverTheWire(t, server.URL+"/export")
			if err == nil {
				t.Fatalf("the truncated response ended cleanly: status %d, body %q", status, body)
			}
			if !strings.HasPrefix(body, "id,amount\n1,100\n") {
				t.Errorf("body = %q, want the rows that were flushed before the failure", body)
			}
			if strings.Contains(body, "error") {
				t.Errorf("an error envelope was appended to the stream: %q", body)
			}
			out := logs.String()
			if !strings.Contains(out, tc.logged) {
				t.Errorf("the failure was not logged:\n%s", out)
			}
			if !strings.Contains(out, "the connection was aborted") || !strings.Contains(out, `"aborted":true`) {
				t.Errorf("the abort was not recorded:\n%s", out)
			}
			if got := serverLog.String(); got != "" {
				t.Errorf("net/http logged the abort as well:\n%s", got)
			}
		})
	}
}

// TestPanicInMiddlewareAfterTheResponseStartedAbortsTheConnection covers the
// same failure one level up, where the Recovery middleware rather than the
// router is the one that catches it.
func TestPanicInMiddlewareAfterTheResponseStartedAbortsTheConnection(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "{\"id\":1}\n")
			_ = http.NewResponseController(w).Flush()
			panic("middleware bug")
		})
	})
	app.Get("/x", okHandler)
	mustBuild(t, app)
	server, serverLog := abortServer(t, app)

	if status, body, err := fetchOverTheWire(t, server.URL+"/x"); err == nil {
		t.Fatalf("the truncated response ended cleanly: status %d, body %q", status, body)
	}
	if n := strings.Count(logs.String(), "middleware bug"); n != 1 {
		t.Errorf("the panic was logged %d times, want once:\n%s", n, logs.String())
	}
	if got := serverLog.String(); got != "" {
		t.Errorf("net/http logged the abort as well:\n%s", got)
	}
}

// TestFailBeforeTheResponseStartedStillRenders pins the other side of the
// line: a failure before anything was written is an ordinary error response,
// and a panic in middleware is still recorded by the access log as the 500
// Recovery writes for it.
func TestFailBeforeTheResponseStartedStillRenders(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/boom" {
				panic("before anything was written")
			}
			next.ServeHTTP(w, r)
		})
	})
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, errors.New("nothing written yet")
	})
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/x"), http.StatusInternalServerError)
	assertStatus(t, do(t, app, "GET", "/boom"), http.StatusInternalServerError)
	if !strings.Contains(logs.String(), `"msg":"GET /boom","scope":"Request","status":500`) {
		t.Errorf("the access log did not record the panic as a 500:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), `"aborted"`) {
		t.Errorf("a response that had not started was reported as aborted:\n%s", logs.String())
	}
}

// TestFailOutsideNetHTTPPanicsWithTheAbortSentinel documents what an
// application serving Muzak through something other than net/http sees: the
// same panic any http.Handler may raise to abort a response.
func TestFailOutsideNetHTTPPanicsWithTheAbortSentinel(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", writeRowsThen(func() error { return errors.New("late") }))
	mustBuild(t, app)

	rec := httptest.NewRecorder()
	recovered := catchPanic(func() { app.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil)) })
	if recovered != http.ErrAbortHandler { //nolint:errorlint // recover yields any, not a wrapped error
		t.Fatalf("recovered %v, want ErrAbortHandler", recovered)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("id,amount")) || strings.Contains(rec.Body.String(), "error") {
		t.Errorf("body = %q, want the rows alone", rec.Body.String())
	}
}

// TestAccessLogLeavesAHijackedPanicUnmarked covers the access log seeing a
// panic pass on a connection that was taken over: the abort does not apply to
// it, so the line must not claim one.
func TestAccessLogLeavesAHijackedPanicUnmarked(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	handler := AccessLog(logger, AccessLogOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markHijacked(w)
		panic("after the handshake")
	}))
	recovered := catchPanic(func() { handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/ws", nil)) })
	if recovered != "after the handshake" {
		t.Fatalf("recovered %v, want the panic resumed unchanged", recovered)
	}
	if out := logs.String(); !strings.Contains(out, `"status":101`) || strings.Contains(out, "aborted") {
		t.Errorf("access log = %s, want the handshake status and no abort", out)
	}
}
