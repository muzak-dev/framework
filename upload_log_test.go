package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A malformed form body is the client's mistake and is answered with a 400
// that carries no cause, so none of the client's bytes reach the log. The
// parser's error quoted the malformed part header whole: a one megabyte
// header wrote a five megabyte ERROR line.
func TestMalformedFormBodyIsNotLogged(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Post("/f", func(ctx *Context, _ struct {
		S string `form:"s" required:"false"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	built := mustBuild(t, app)

	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"multipart part header", "multipart/form-data; boundary=B",
			"--B\r\n" + strings.Repeat("\xff", 1<<20) + "\r\n\r\nx\r\n--B--\r\n"},
		{"urlencoded escape", "application/x-www-form-urlencoded",
			"s=%zz" + strings.Repeat("\xff", 1<<10)},
	} {
		req := httptest.NewRequest(http.MethodPost, "/f", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.contentType)
		rec := doRequest(t, built, req)
		assertStatus(t, rec, http.StatusBadRequest)
		if message := decodeError(t, rec).Error.Message; message != "the upload could not be read" && message != "the form could not be read" {
			t.Errorf("%s: message = %q, want the fixed phrase", tc.name, message)
		}
	}
	if out := logs.String(); strings.Contains(out, `"level":"ERROR"`) || strings.Contains(out, "\\ufffd") || len(out) > 4096 {
		t.Errorf("a malformed body was logged (%d bytes): %.300s", len(out), out)
	}
}
