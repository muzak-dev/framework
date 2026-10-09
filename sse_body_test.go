package muzak

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An event stream reached by POST whose input binds no body leaves the body to
// the handler, and the stream's header goes out before the handler runs. Over
// HTTP/1.1, net/http drops an unread body when the header is written unless the
// response is put in full duplex mode, so a handler that read its own body got
// "http: invalid Read on closed Body" for one under 256 KiB, and a larger one
// cost the connection. These go through a real server, because a response
// recorder has no body to drop.

// bodyReport is what a handler that read its own body says about it.
type bodyReport struct {
	N   int    `json:"n"`
	Err string `json:"err,omitempty"`
}

// reportBody reads the whole request body and sends what it found.
func reportBody(ctx *Context, stream *SSEStream[bodyReport]) error {
	n, err := io.Copy(io.Discard, ctx.Request().Body)
	report := bodyReport{N: int(n)}
	if err != nil {
		report.Err = err.Error()
	}
	return stream.Send(report)
}

// queryOnlyIn binds a query parameter and nothing from the body, which leaves
// the body to the handler as much as [Empty] does.
type queryOnlyIn struct {
	Tag string `query:"tag"`
}

func TestSSEHandlerReadsItsOwnBodyOverHTTP1(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSEHandle(http.MethodPost, "/empty", func(ctx *Context, _ Empty, stream *SSEStream[bodyReport]) error {
			return reportBody(ctx, stream)
		}, WithSSE(SSEOptions{KeepAlive: -1}))
		app.SSEHandle(http.MethodPost, "/query", func(ctx *Context, _ queryOnlyIn, stream *SSEStream[bodyReport]) error {
			return reportBody(ctx, stream)
		}, WithSSE(SSEOptions{KeepAlive: -1}))
	})

	// Either side of the 256 KiB net/http discards on its own, past which it
	// gives up on the body and closes the connection instead.
	for _, path := range []string{"/empty", "/query?tag=x"} {
		for _, size := range []int{5, 300 << 10} {
			t.Run(path+" "+strconv.Itoa(size), func(t *testing.T) {
				reader := openStream(t, server.URL, path, func(o *SSEDialOptions) {
					o.Method = http.MethodPost
					o.Body = strings.NewReader(strings.Repeat("x", size))
					o.Header = http.Header{"Content-Type": {"application/octet-stream"}}
				})
				report, err := nextEvent(t, reader).Decode[bodyReport]()
				if err != nil {
					t.Fatal(err)
				}
				if report.N != size || report.Err != "" {
					t.Errorf("the handler read %d bytes of a %d byte body (error %q), want all of it", report.N, size, report.Err)
				}
			})
		}
	}
}

func TestSSEStreamOutlivesTheReadTimeoutAfterItsHandlerReadsTheBody(t *testing.T) {
	t.Parallel()
	// The request's read deadline is left in place while the body is the
	// handler's to read, and net/http takes it off once the body has been read
	// to its end. A stream that kept it would be ended by the read that watches
	// for a disconnect, at ReadTimeout, however healthy it was.
	const readTimeout = 200 * time.Millisecond
	app := New(quietOptions())
	app.SSEHandle(http.MethodPost, "/stream", func(ctx *Context, _ Empty, stream *SSEStream[bodyReport]) error {
		n, err := io.Copy(io.Discard, ctx.Request().Body)
		if err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return stream.Err()
		case <-time.After(3 * readTimeout):
		}
		return stream.Send(bodyReport{N: int(n)})
	}, WithSSE(SSEOptions{KeepAlive: -1}))
	mustBuild(t, app)
	server := httptest.NewUnstartedServer(app)
	server.Config.ReadTimeout = readTimeout
	server.Start()
	t.Cleanup(server.Close)

	reader := openStream(t, server.URL, "/stream", func(o *SSEDialOptions) {
		o.Method = http.MethodPost
		o.Body = strings.NewReader("hello")
	})
	if report, err := nextEvent(t, reader).Decode[bodyReport](); err != nil || report.N != 5 {
		t.Errorf("the event after the read timeout = %+v, %v; want the body's 5 bytes", report, err)
	}
}

func TestSSEHandlerReadsItsBodyUnderTheReadTimeout(t *testing.T) {
	t.Parallel()
	// A client that declares a body and then sends only part of it must not
	// hold the handler's read for as long as it likes: the body is read under
	// the server's ReadTimeout, as any other route's is. The stream used to
	// clear the read deadline as it opened, so that read had no bound at all.
	// A read of the connection that fails ends the request in net/http, so the
	// stream ends with it, which is what a request that never arrived in full
	// is due.
	const readTimeout = 200 * time.Millisecond
	read := make(chan bodyReport, 1)
	app := New(quietOptions())
	app.SSEHandle(http.MethodPost, "/stream", func(ctx *Context, _ Empty, stream *SSEStream[bodyReport]) error {
		n, err := io.Copy(io.Discard, ctx.Request().Body)
		report := bodyReport{N: int(n)}
		if err != nil {
			report.Err = err.Error()
		}
		read <- report
		<-stream.Context().Done()
		return stream.Err()
	}, WithSSE(SSEOptions{KeepAlive: -1}))
	mustBuild(t, app)
	server := httptest.NewUnstartedServer(app)
	server.Config.ReadTimeout = readTimeout
	server.Start()
	t.Cleanup(server.Close)

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(sseTestTimeout))
	started := time.Now()
	if _, err := fmt.Fprint(conn, "POST /stream HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n0123456789"); err != nil {
		t.Fatal(err)
	}

	select {
	case report := <-read:
		if report.N != 10 || report.Err == "" {
			t.Errorf("the handler's read = %+v, want the 10 bytes sent and then a failure", report)
		}
	case <-time.After(sseTestTimeout):
		t.Fatalf("the handler's read of a stalled body was still waiting after %v, want it bounded by the %v read timeout", sseTestTimeout, readTimeout)
	}
	// The stream opened, and then ended rather than being held open by a
	// handler that could no longer have anything to say.
	response, err := io.ReadAll(bufio.NewReader(conn))
	if err != nil {
		t.Fatalf("the stream did not end after the failed read: %v", err)
	}
	if !strings.HasPrefix(string(response), "HTTP/1.1 200 OK") || !strings.Contains(string(response), "text/event-stream") {
		t.Errorf("the response = %q, want an event stream that opened", response)
	}
	if elapsed := time.Since(started); elapsed > sseTestTimeout/2 {
		t.Errorf("the stream of a stalled body lasted %v, want it bounded by the %v read timeout", elapsed, readTimeout)
	}
}

// The handler of a stream whose body went unread is not left waiting on the
// context for a client going away; that is noticed by the next write instead,
// which is what this pins: ending the client ends the stream.
func TestSSEStreamWithAnUnreadBodyEndsWhenTheClientGoes(t *testing.T) {
	t.Parallel()
	ended := make(chan struct{})
	_, server := newSSETestApp(t, func(app *App) {
		app.SSEHandle(http.MethodPost, "/stream", func(_ *Context, _ Empty, stream *SSEStream[bodyReport]) error {
			defer close(ended)
			<-stream.Context().Done()
			return stream.Err()
		}, WithSSE(SSEOptions{KeepAlive: 20 * time.Millisecond}))
	})
	ctx, cancel := context.WithCancel(t.Context())
	reader, _, err := SSEDial(ctx, server.URL+"/stream", SSEDialOptions{
		Method: http.MethodPost,
		Body:   strings.NewReader("ignored"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = reader.Close()
	select {
	case <-ended:
	case <-time.After(sseTestTimeout):
		t.Fatal("the stream outlived its client")
	}
}
