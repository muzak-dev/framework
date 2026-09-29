package muzak

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A route whose input binds nothing has no use for the request body, but a
// handler on it may still want the bytes for itself: a proxy, an upload
// streamed to disk, a signature check over the raw request. What the framework
// does with such a body must not be to read the front of it away first.

type bodyLength struct {
	N int `json:"n"`
}

// readsTheBody answers with the number of bytes the handler itself could read.
func readsTheBody(ctx *Context, _ Empty) (bodyLength, error) {
	n, err := io.Copy(io.Discard, ctx.Request().Body)
	return bodyLength{N: int(n)}, err
}

func TestEmptyInputRouteLeavesTheWholeBodyToTheHandler(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/raw", readsTheBody)
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	// Either side of the 4 KiB the framework used to take, and a body well
	// past it, for both a content type the binder knows and one it does not.
	for _, size := range []int{1, 4095, 4096, 4097, 100000} {
		for _, contentType := range []string{"application/json", "application/octet-stream"} {
			t.Run(strconv.Itoa(size)+" "+contentType, func(t *testing.T) {
				res, err := http.Post(server.URL+"/raw", contentType, bytes.NewReader(make([]byte, size)))
				if err != nil {
					t.Fatalf("POST: %v", err)
				}
				defer func() { _ = res.Body.Close() }()
				var out bodyLength
				if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
					t.Fatalf("decoding the response (status %d): %v", res.StatusCode, err)
				}
				if out.N != size {
					t.Errorf("the handler read %d bytes of a %d byte body", out.N, size)
				}
			})
		}
	}
}

func TestEmptyInputRouteKeepsTheConnectionAfterAnUnreadBody(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	// The handler reads none of the body, which is what the drain is for.
	app.Post("/ignore", func(*Context, Empty) (bodyLength, error) { return bodyLength{}, nil })
	mustBuild(t, app)

	var opened atomic.Int32
	server := httptest.NewUnstartedServer(app)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	// One client, so that a connection the server left open is one it reuses.
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	for range 5 {
		res, err := client.Post(server.URL+"/ignore", "application/json", strings.NewReader(strings.Repeat("x", 2000)))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	if got := opened.Load(); got != 1 {
		t.Errorf("five requests opened %d connections, want them to share one", got)
	}
}

// Over HTTP/1 net/http discards a request body when the response head is first
// written, and an event stream commits its head before the handler runs, so
// there a handler can read none of it whatever the framework does. Over HTTP/2
// the body stays readable, which is what this stands in for by serving the
// request without a server in between: the framework must not have read the
// front of it away.
func TestEmptyInputEventStreamLeavesTheWholeBodyToTheHandler(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.SSEHandle(http.MethodPost, "/stream", func(ctx *Context, _ Empty, stream *SSEStream[bodyLength]) error {
		n, err := io.Copy(io.Discard, ctx.Request().Body)
		if err != nil {
			return err
		}
		return stream.Send(bodyLength{N: int(n)})
	})
	mustBuild(t, app)

	for _, size := range []int{1, 4096, 4097, 100000} {
		req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewReader(make([]byte, size)))
		req.Header.Set("Content-Type", "application/octet-stream")
		res := doRequest(t, app, req)
		if want := `"n":` + strconv.Itoa(size) + `}`; !strings.Contains(res.Body.String(), want) {
			t.Errorf("a %d byte body: the stream said %q, want it to contain %s", size, res.Body.String(), want)
		}
	}
}
