package muzak

import (
	"bytes"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// quietOptions returns AppOptions that keep tests silent, so that a passing run
// prints nothing and a failing one prints only what the test reports.
func quietOptions() AppOptions {
	return AppOptions{
		Title:         "Test API",
		Version:       "1.0.0",
		LoggerOptions: LoggerOptions{Format: LogFormatNone},
	}
}

// captureLogger returns a logger writing JSON into a buffer, for tests that
// assert on what was logged.
func captureLogger(t *testing.T) (*slog.Logger, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	return NewLogger(LoggerOptions{
		Format: LogFormatJSON,
		Output: buf,
		Level:  slog.LevelDebug,
	}), buf
}

// syncBuffer is a bytes.Buffer safe for concurrent writes, which the logger
// needs because handlers run on many goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// do sends a request through an application and returns the recorded response.
func do(t *testing.T, app *App, method, target string, body ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if len(body) > 0 {
		reader = strings.NewReader(body[0])
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, reader)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

// doRequest sends a prepared request through an application.
func doRequest(t *testing.T, app *App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

// decodeError reads the standard error envelope out of a response, failing the
// test if the body is not one.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var out ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body is not an error envelope: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

// assertStatus fails the test unless the recorded response carried the wanted
// status.
func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, want, rec.Body.String())
	}
}

// assertJSON fails the test unless the response body is JSON equal to want,
// ignoring member order and whitespace.
func assertJSON(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var got, expected any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatalf("the expected value is not valid JSON: %v", err)
	}
	gotNormal, _ := json.Marshal(got, json.Deterministic(true))
	wantNormal, _ := json.Marshal(expected, json.Deterministic(true))
	if !bytes.Equal(gotNormal, wantNormal) {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", gotNormal, wantNormal)
	}
}

// mustBuild builds an application and fails the test if it cannot be built.
func mustBuild(t *testing.T, app *App) *App {
	t.Helper()
	if err := app.Build(); err != nil {
		t.Fatalf("Build() = %v", err)
	}
	return app
}

// buildError builds an application expecting failure and returns the message.
func buildError(t *testing.T, app *App) string {
	t.Helper()
	err := app.Build()
	if err == nil {
		t.Fatal("Build() succeeded, want an error")
	}
	return err.Error()
}
