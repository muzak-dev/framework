package muzak

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTruncateForMessage(t *testing.T) {
	t.Parallel()
	exact := "/" + strings.Repeat("a", maxQuotedLength-1)
	long := exact + "bcd"
	// A two-byte character straddling the bound is dropped whole rather than
	// cut in half.
	accented := strings.Repeat("a", maxQuotedLength-1) + strings.Repeat(string(rune(0xE9)), 4)
	// A run of continuation bytes is not UTF-8 at all, and moves the cut back
	// no more than a real character could.
	continuation := strings.Repeat("\x80", 2*maxQuotedLength)

	for _, tc := range []struct {
		name, in, want string
	}{
		{"short", "/users/42", "/users/42"},
		{"exactly the bound", exact, exact},
		{"over the bound", long, exact + truncatedMarker},
		{"multibyte at the bound", accented, strings.Repeat("a", maxQuotedLength-1) + truncatedMarker},
		{"not utf-8", continuation, continuation[:maxQuotedLength-3] + truncatedMarker},
	} {
		if got := truncateForMessage(tc.in); got != tc.want {
			t.Errorf("%s: got %d bytes ending %q, want %d bytes ending %q", tc.name,
				len(got), got[max(0, len(got)-20):], len(tc.want), tc.want[max(0, len(tc.want)-20):])
		}
	}
}

// A path of a mebibyte of bytes that are not UTF-8 used to be quoted in full
// into the 404, three bytes out for each one in, and into the access log line,
// six. Both now quote at most a kibibyte of it.
func TestLongPathIsTruncatedInNotFoundAndAccessLog(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	options := quietOptions()
	options.LoggerOptions = LoggerOptions{Format: LogFormatJSON, Output: logs, Level: slog.LevelInfo}
	app := New(options)
	app.Get("/x", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.URL.Path = "/" + strings.Repeat("\xff", 1_000_000)
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	assertStatus(t, rec, http.StatusNotFound)
	if n := rec.Body.Len(); n > 4096 {
		t.Errorf("the 404 body is %d bytes for a path the client chose", n)
	}
	if message := decodeError(t, rec).Error.Message; !strings.HasSuffix(message, truncatedMarker) {
		t.Errorf("message = %.80q..., want it to end with the truncation marker", message)
	}
	if n := len(logs.String()); n > 16384 {
		t.Errorf("the access log wrote %d bytes for one request", n)
	}
	if !strings.Contains(logs.String(), truncatedMarker) {
		t.Error("the access log line does not mark the path as truncated")
	}
}

// A path of ordinary length reads exactly as it did.
func TestShortPathIsQuotedWhole(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	options := quietOptions()
	options.LoggerOptions = LoggerOptions{Format: LogFormatJSON, Output: logs, Level: slog.LevelInfo}
	app := New(options)
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/caf%C3%A9/menu")
	assertStatus(t, rec, http.StatusNotFound)
	if message := decodeError(t, rec).Error.Message; message != "no route matches GET /caf%C3%A9/menu" {
		t.Errorf("message = %q", message)
	}
	if want := "GET /caf" + string(rune(0xE9)) + "/menu"; !strings.Contains(logs.String(), want) {
		t.Errorf("log = %s, want it to name %q", logs, want)
	}
}

// The method is a client-chosen token too, and a request built by hand may
// carry any bytes in it; the 405 quotes a bounded amount of either.
func TestLongMethodIsTruncated(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	for _, method := range []string{strings.Repeat("M", 100_000), strings.Repeat("\x01", 100_000)} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Method = method
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		assertStatus(t, rec, http.StatusMethodNotAllowed)
		if n := rec.Body.Len(); n > 4096 {
			t.Errorf("the 405 body is %d bytes for a method the client chose", n)
		}
	}
}
