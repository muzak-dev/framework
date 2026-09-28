package muzak

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestFailureAfterAFlushBeforeAnyWriteAbortsTheConnection is the regression
// test for the "send the headers early" idiom: a Flush before the first Write
// makes net/http send 200 and the headers, but the wrapper did not record that
// the response had started, so the failure that followed rendered the JSON
// error envelope into the stream as though it were the body. The client must
// see the transfer fail instead.
func TestFailureAfterAFlushBeforeAnyWriteAbortsTheConnection(t *testing.T) {
	t.Parallel()
	flushThen := func(fail func() error) func(*Context, Empty) (rtOut, error) {
		return func(ctx *Context, _ Empty) (rtOut, error) {
			w := ctx.ResponseWriter()
			w.Header().Set("Content-Type", "text/csv")
			_ = http.NewResponseController(w).Flush()
			return rtOut{}, fail()
		}
	}
	tests := []struct {
		name    string
		handler func(*Context, Empty) (rtOut, error)
		logged  string
	}{
		{
			name:    "a handler that returns an error",
			handler: flushThen(func() error { return errors.New("db failed") }),
			logged:  "db failed",
		},
		{
			name:    "a handler that panics",
			handler: flushThen(func() error { panic("row 1 could not be formatted") }),
			logged:  "row 1 could not be formatted",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := captureLogger(t)
			opts := quietOptions()
			opts.Logger = logger
			app := New(opts)
			app.Get("/stream", tc.handler)
			mustBuild(t, app)
			server, serverLog := abortServer(t, app)

			status, body, err := fetchOverTheWire(t, server.URL+"/stream")
			if err == nil {
				t.Fatalf("the failed stream ended cleanly: status %d, body %q", status, body)
			}
			if strings.Contains(body, "error") {
				t.Errorf("an error envelope was appended to the stream: %q", body)
			}
			out := logs.String()
			if !strings.Contains(out, tc.logged) {
				t.Errorf("the failure was not logged:\n%s", out)
			}
			if !strings.Contains(out, `"aborted":true`) {
				t.Errorf("the abort was not recorded:\n%s", out)
			}
			if got := serverLog.String(); got != "" {
				t.Errorf("net/http logged the abort as well:\n%s", got)
			}
		})
	}
}

// TestSetStatusAfterAFlushIsReportedAsIgnored covers the other reader of the
// same fact: the status line has been sent by the flush, so SetStatus must say
// it is too late rather than silently accept a code that will never be sent.
func TestSetStatusAfterAFlushIsReportedAsIgnored(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		_ = http.NewResponseController(ctx.ResponseWriter()).Flush()
		ctx.SetStatus(http.StatusAccepted)
		return rtOut{}, nil
	})
	mustBuild(t, app)
	server, _ := abortServer(t, app)

	if status, _, _ := fetchOverTheWire(t, server.URL+"/x"); status != http.StatusOK {
		t.Errorf("status = %d, want the 200 the flush sent", status)
	}
	if !strings.Contains(logs.String(), "SetStatus called after the response body started") {
		t.Errorf("SetStatus after a flush was not reported:\n%s", logs.String())
	}
}
