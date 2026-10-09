package muzak

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRawHijackThroughTheResponseWriter is the regression test for the
// hijack Context.ResponseWriter documents. A handler that took the connection
// with http.NewResponseController and returned the zero value had the
// framework write a 200 onto the socket it no longer owned: net/http warned
// about a WriteHeader on a hijacked connection, the failed write was logged as
// an aborted request at error level, and the access log recorded
// "status=200 aborted=true" for a request that had gone exactly as intended.
// The wrapper now notices the hijack itself, as the WebSocket route already
// told it.
func TestRawHijackThroughTheResponseWriter(t *testing.T) {
	t.Parallel()
	for _, compressed := range []bool{false, true} {
		logger, logs := captureLogger(t)
		opts := quietOptions()
		opts.Logger = logger
		app := New(opts)
		if compressed {
			// Compression wraps the writer the handler sees once more, and
			// that wrapper must hear about the hijack too.
			app.Use(Compress(CompressionOptions{}))
		}
		app.Get("/raw", func(c *Context, _ Empty) (Empty, error) {
			conn, rw, err := http.NewResponseController(c.ResponseWriter()).Hijack()
			if err != nil {
				return Empty{}, err
			}
			defer func() { _ = conn.Close() }()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			_ = rw.Flush()
			return Empty{}, nil
		})

		var serverLog syncBuffer
		server := httptest.NewUnstartedServer(app)
		server.Config.ErrorLog = log.New(&serverLog, "", 0)
		server.Start()

		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(conn, "GET /raw HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\n\r\n")
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("compressed=%v: reading the handler's own response: %v", compressed, err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		_ = conn.Close()
		// Close waits for the handler, so every line it causes is written.
		server.Close()

		if string(body) != "hi" {
			t.Errorf("compressed=%v: body = %q, want the handler's own", compressed, body)
		}
		if out := serverLog.String(); out != "" {
			t.Errorf("compressed=%v: net/http logged %q, want nothing written after the hijack", compressed, out)
		}
		out := logs.String()
		if strings.Contains(out, `"level":"ERROR"`) || strings.Contains(out, "aborted") {
			t.Errorf("compressed=%v: logs = %s, want no failure and no abort", compressed, out)
		}
		if !strings.Contains(out, `"status":101`) {
			t.Errorf("compressed=%v: access log = %s, want the request recorded as taken over", compressed, out)
		}
	}
}
