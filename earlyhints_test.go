package muzak

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
)

// getWithHints fetches url, counting the 103 responses that arrive before the
// final one, and returns the final status, its header and its body.
func getWithHints(t *testing.T, url string, header map[string]string) (hints int32, res *http.Response, body string) {
	t.Helper()
	var count atomic.Int32
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints {
				count.Add(1)
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range header {
		req.Header.Set(name, value)
	}
	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	res, err = (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	var reader io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		reader = gz
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return count.Load(), res, string(raw)
}

// earlyHintsApp answers /page with 103 Early Hints and then its real
// response, and /missing with 103 and then an error.
func earlyHintsApp(t *testing.T, middleware ...Middleware) *httptest.Server {
	t.Helper()
	app := New(quietOptions())
	for _, mw := range middleware {
		app.Use(mw)
	}
	hint := func(ctx *Context) {
		ctx.ResponseWriter().Header().Set("Link", "</app.css>; rel=preload; as=style")
		ctx.ResponseWriter().WriteHeader(http.StatusEarlyHints)
	}
	app.Get("/page", func(ctx *Context, _ Empty) (versionOut, error) {
		hint(ctx)
		return versionOut{V: "body " + strings.Repeat("x", 2048)}, nil
	})
	app.Get("/missing", func(ctx *Context, _ Empty) (versionOut, error) {
		hint(ctx)
		return versionOut{}, NotFound("")
	})
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return server
}

// A 103 Early Hints used to be taken for the response itself, so what the
// handler returned afterwards was dropped and an error after it aborted the
// connection. The hint now goes out ahead of the real response, which follows
// as it would have without it.
func TestEarlyHintsPrecedeTheResponse(t *testing.T) {
	t.Parallel()
	for name, middleware := range map[string][]Middleware{
		"plain":      nil,
		"compressed": {Compress(CompressionOptions{})},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := earlyHintsApp(t, middleware...)
			accept := map[string]string{"Accept-Encoding": "gzip"}

			hints, res, body := getWithHints(t, server.URL+"/page", accept)
			if hints != 1 {
				t.Errorf("got %d Early Hints responses, want 1", hints)
			}
			if res.StatusCode != http.StatusOK || !strings.Contains(body, `"v":"body `) {
				t.Errorf("after 103: status %d, body %.60q, want 200 with the handler's body", res.StatusCode, body)
			}
			if name == "compressed" && res.Header.Get("Content-Encoding") != "gzip" {
				t.Errorf("Content-Encoding = %q, want the final response compressed", res.Header.Get("Content-Encoding"))
			}

			_, res, body = getWithHints(t, server.URL+"/missing", accept)
			if res.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code"`) {
				t.Errorf("error after 103: status %d, body %.60q, want the 404 envelope", res.StatusCode, body)
			}
		})
	}
}

func TestResponseWriterInformationalStatus(t *testing.T) {
	t.Parallel()
	rw := asResponseWriter(httptest.NewRecorder())
	rw.WriteHeader(http.StatusEarlyHints)
	if rw.written || rw.Status() != 0 {
		t.Errorf("after 103: written = %v, status = %d, want the response still unstarted", rw.written, rw.Status())
	}
	// 101 is the final response of its exchange, as net/http treats it.
	rw.WriteHeader(http.StatusSwitchingProtocols)
	if !rw.written || rw.Status() != http.StatusSwitchingProtocols {
		t.Errorf("after 101: written = %v, status = %d, want it recorded", rw.written, rw.Status())
	}
}
