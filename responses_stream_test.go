package muzak

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Stream's body belongs to Muzak once the handler returns it, and is closed
// exactly once on every way the request can end. Each test below takes one of
// those ways and checks the body with a recording closer.

func TestStreamCopiesTheBodyAndClosesIt(t *testing.T) {
	t.Parallel()
	body := newRecordingBody(strings.NewReader(strings.Repeat("x", 100_000)))
	app := New(quietOptions())
	app.Get("/download", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{ContentType: "application/zip", Body: body, Length: 100_000}, nil
	})
	res := newWireServer(t, app).do(t, http.MethodGet, "/download")
	if res.StatusCode != http.StatusOK || res.ReadErr != nil || len(res.Data) != 100_000 {
		t.Fatalf("got %d with %d bytes and %v, want 200 with the whole body", res.StatusCode, len(res.Data), res.ReadErr)
	}
	if res.ContentLength != 100_000 || res.Header.Get("Content-Type") != "application/zip" ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v, want the declared length and type and nosniff", res.Header)
	}
	body.waitClosed(t)
	body.assertClosedOnce(t)
}

// A body whose length is not known is sent chunked, with no Content-Length.
func TestStreamOfUnknownLengthIsChunked(t *testing.T) {
	t.Parallel()
	// Larger than net/http buffers before deciding, which would otherwise
	// count a small body and send its length after all.
	lines := strings.Repeat("{\"line\":true}\n", 1000)
	body := newRecordingBody(strings.NewReader(lines))
	app := New(quietOptions())
	app.Get("/lines", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{ContentType: "application/x-ndjson", Body: body}, nil
	})
	res := newWireServer(t, app).do(t, http.MethodGet, "/lines")
	if res.ReadErr != nil || string(res.Data) != lines {
		t.Fatalf("read %d bytes and %v, want every line", len(res.Data), res.ReadErr)
	}
	if res.ContentLength != -1 || len(res.TransferEncoding) == 0 || res.TransferEncoding[0] != "chunked" {
		t.Errorf("length %d and transfer encoding %v, want an unknown length sent chunked", res.ContentLength, res.TransferEncoding)
	}
	body.waitClosed(t)
	body.assertClosedOnce(t)
}

// A HEAD request is answered from the header alone: the body is never read,
// and it is still closed.
func TestStreamAnswersHeadWithoutReadingTheBody(t *testing.T) {
	t.Parallel()
	body := newRecordingBody(strings.NewReader("never read"))
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{ContentType: "text/plain", Body: body, Length: 10}, nil
	})
	res := newWireServer(t, app).do(t, http.MethodHead, "/x")
	if res.StatusCode != http.StatusOK || res.ContentLength != 10 {
		t.Errorf("HEAD = %d with length %d, want 200 and the declared length", res.StatusCode, res.ContentLength)
	}
	body.waitClosed(t)
	body.assertClosedOnce(t)
	if n := body.reads.Load(); n != 0 {
		t.Errorf("the body was read %d times for a HEAD request, want never", n)
	}
}

// A handler that returns an error beside a Stream it already opened gets the
// error rendered, and the body it can no longer close is closed for it.
func TestStreamReturnedWithAnErrorIsClosed(t *testing.T) {
	t.Parallel()
	body := newRecordingBody(strings.NewReader("abandoned"))
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: body}, Conflict("not now")
	})
	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusConflict)
	body.assertClosedOnce(t)
	if n := body.reads.Load(); n != 0 {
		t.Errorf("the body was read %d times, want never", n)
	}
}

// The same holds when the handler's Out is an interface that a Stream
// satisfies, since what it returned is decided at run time.
func TestStreamReturnedThroughAnInterfaceWithAnErrorIsClosed(t *testing.T) {
	t.Parallel()
	body := newRecordingBody(strings.NewReader("abandoned"))
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (any, error) {
		return Stream{Body: body}, errors.New("database went away")
	})
	app.Get("/fine", func(ctx *Context, _ Empty) (any, error) {
		return Stream{ContentType: "text/plain", Body: strings.NewReader("ok")}, nil
	})
	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusInternalServerError)
	body.assertClosedOnce(t)

	rec = do(t, app, http.MethodGet, "/fine")
	if rec.Body.String() != "ok" {
		t.Errorf("an interface holding a Stream wrote %q, want the stream's body", rec.Body.String())
	}
}

// A body that fails part way cannot be turned into an error response, because
// the header is on the wire. The connection is aborted, so the client sees a
// failed transfer rather than a body that looks complete, and the failure is
// logged.
func TestStreamThatFailsPartWayAbortsTheConnection(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	body := newRecordingBody(&brokenBody{data: []byte("row 1\nrow 2\n"), err: errors.New("cursor lost")})
	app := New(opts)
	app.Get("/rows", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{ContentType: "text/csv", Body: body}, nil
	})
	if res, err := newWireServer(t, app).try(http.MethodGet, "/rows"); !failedTransfer(res, err) {
		t.Fatalf("got %d with %q, want the transfer to fail", res.StatusCode, res.Data)
	}
	body.waitClosed(t)
	body.assertClosedOnce(t)
	waitForLog(t, logs, "cursor lost")
	if !strings.Contains(logs.String(), "the connection was aborted") {
		t.Errorf("the abort was not logged: %s", logs.String())
	}
}

// A body that ends before the Length it declared is a truncation the client
// would otherwise be left to notice, so it is treated as a failure too.
func TestStreamShorterThanItsLengthAbortsTheConnection(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	body := newRecordingBody(strings.NewReader("only ten b"))
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: body, Length: 1000}, nil
	})
	if res, err := newWireServer(t, app).try(http.MethodGet, "/x"); !failedTransfer(res, err) {
		t.Fatalf("read %d bytes cleanly, want the transfer to fail", len(res.Data))
	}
	body.waitClosed(t)
	body.assertClosedOnce(t)
	waitForLog(t, logs, "ended after 10 of the 1000 bytes")
}

// A body longer than its Length is cut at the length by net/http, and the
// write it refuses fails the request.
func TestStreamLongerThanItsLengthIsAFailure(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	body := newRecordingBody(strings.NewReader(strings.Repeat("y", 64<<10)))
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: body, Length: 5}, nil
	})
	if res, err := newWireServer(t, app).try(http.MethodGet, "/x"); err == nil && len(res.Data) > 5 {
		t.Errorf("read %d bytes, want no more than the 5 declared", len(res.Data))
	}
	body.waitClosed(t)
	body.assertClosedOnce(t)
	waitForLog(t, logs, "failed after")
}

// A panic while the body is read is recovered as any handler panic is, and the
// body is closed on the way out.
func TestStreamBodyThatPanicsIsClosed(t *testing.T) {
	t.Parallel()
	body := newRecordingBody(explodingBody{})
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: body}, nil
	})
	if res, err := newWireServer(t, app).try(http.MethodGet, "/x"); !failedTransfer(res, err) && res.StatusCode == http.StatusOK {
		t.Errorf("a panicking body produced a clean 200 of %q", res.Data)
	}
	body.waitClosed(t)
	body.assertClosedOnce(t)
}

// A client that goes away mid-transfer fails the copy, and the body is closed,
// which is what ends a producer feeding it through a pipe. The producer here
// stops on that failure or on the request's context, as the documentation
// asks, and nothing is left running.
func TestStreamClientDisconnectClosesTheBodyAndLeaksNothing(t *testing.T) {
	var body *recordingBody
	ready := make(chan struct{})
	app := New(quietOptions())
	app.Get("/live", func(ctx *Context, _ Empty) (Stream, error) {
		reader, writer := io.Pipe()
		// The request's context, and not the Context itself, is what the
		// producer keeps: the Context is pooled and reused once the handler
		// returns, which is before this goroutine is done.
		done := ctx.Context()
		go func() {
			chunk := bytes.Repeat([]byte("z"), 32<<10)
			for {
				if _, err := writer.Write(chunk); err != nil {
					return
				}
				select {
				case <-done.Done():
					_ = writer.CloseWithError(done.Err())
					return
				default:
				}
			}
		}()
		body = newRecordingBody(reader)
		close(ready)
		return Stream{ContentType: "application/octet-stream", Body: body}, nil
	})
	server := newWireServer(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/live", nil)
	transport := &http.Transport{}
	res, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if _, err := io.ReadFull(res.Body, make([]byte, 100<<10)); err != nil {
		t.Fatalf("reading the start of the stream: %v", err)
	}
	cancel()
	_ = res.Body.Close()
	transport.CloseIdleConnections()

	<-ready
	body.waitClosed(t)
	body.assertClosedOnce(t)
	server.Close()
	assertNoGoroutineLeaks(t)
}

func TestStreamSendsNoBodyAt204And304(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		body := newRecordingBody(strings.NewReader("never sent"))
		app := New(quietOptions())
		app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
			ctx.SetStatus(status)
			return Stream{Body: body, Length: 10}, nil
		})
		rec := do(t, app, http.MethodGet, "/x")
		assertStatus(t, rec, status)
		if rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "" {
			t.Errorf("%d: body %q with Content-Length %q, want nothing", status, rec.Body.String(), rec.Header().Get("Content-Length"))
		}
		body.assertClosedOnce(t)
		if body.reads.Load() != 0 {
			t.Errorf("%d: the body was read", status)
		}
	}
}

func TestStreamHonoursTheDeclaredStatus(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{ContentType: "text/plain", Body: strings.NewReader("made")}, nil
	}, Status(http.StatusCreated))
	rec := do(t, app, http.MethodPost, "/x")
	assertStatus(t, rec, http.StatusCreated)
	if rec.Body.String() != "made" {
		t.Errorf("body = %q, want made", rec.Body.String())
	}
}

func TestStreamWithNoBodyIsAServerError(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) { return Stream{ContentType: "text/plain"}, nil })
	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusInternalServerError)
	if !strings.Contains(logs.String(), "with no Body") {
		t.Errorf("the cause was not logged: %s", logs.String())
	}
}

func TestStreamWithABadContentTypeIsClosedAndRefused(t *testing.T) {
	t.Parallel()
	body := newRecordingBody(strings.NewReader("<script>"))
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{ContentType: "text/html\r\nX-Injected: yes", Body: body}, nil
	})
	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusInternalServerError)
	if rec.Header().Get("X-Injected") != "" {
		t.Error("a header was injected through the content type")
	}
	body.assertClosedOnce(t)
}

// A handler that wrote the response itself and still returned a Stream has
// its stream closed unread.
func TestStreamAfterTheHandlerWroteIsClosedUnread(t *testing.T) {
	t.Parallel()
	body := newRecordingBody(strings.NewReader("ignored"))
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		_, _ = ctx.ResponseWriter().Write([]byte("mine"))
		return Stream{Body: body}, nil
	})
	rec := do(t, app, http.MethodGet, "/x")
	if rec.Body.String() != "mine" {
		t.Errorf("body = %q, want only what the handler wrote", rec.Body.String())
	}
	body.assertClosedOnce(t)
	if body.reads.Load() != 0 {
		t.Error("the stream was read after the handler had answered")
	}
}

// A stream of text passes through compression like any other body, and comes
// out whole.
func TestStreamIsCompressedLikeAnyBody(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("compressible text ", 1000)
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{}))
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{ContentType: "text/plain", Body: strings.NewReader(text)}, nil
	})
	res := newWireServer(t, app).do(t, http.MethodGet, "/x", "Accept-Encoding", "gzip")
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", res.Header.Get("Content-Encoding"))
	}
	reader, err := gzip.NewReader(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil || string(plain) != text {
		t.Errorf("decompressed %d bytes (%v), want the %d sent", len(plain), err, len(text))
	}
}

// The copy goes through a pooled buffer even for a reader that offers a
// WriteTo of its own, which would otherwise allocate a buffer per response.
func TestStreamCopiesThroughThePooledBuffer(t *testing.T) {
	data := bytes.Repeat([]byte("q"), 256<<10)
	app := New(AppOptions{LoggerOptions: LoggerOptions{Format: LogFormatNone}, DisableAccessLog: true, DisableDocs: true})
	app.Get("/x", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: writerToBody{bytes.NewReader(data)}}, nil
	})
	mustBuild(t, app)
	req, _ := http.NewRequest(http.MethodGet, "/x", nil)
	result := testing.Benchmark(func(b *testing.B) {
		w := newDiscardWriter()
		b.ReportAllocs()
		for b.Loop() {
			serve(app, req, w)
		}
	})
	// A buffer per response would be 32 KiB of it. The race detector has
	// sync.Pool drop a share of what it is given, to flush out code that
	// relies on getting it back, so a pooled buffer is sometimes rebuilt there
	// too; the bound leaves room for that and none for a buffer every time.
	if perOp := result.AllocedBytesPerOp(); perOp > 20<<10 {
		t.Errorf("a stream allocated %d bytes per response, want the copy buffer pooled", perOp)
	}
}

// writerToBody is a reader whose WriteTo copies through a fresh buffer, as
// io.Copy does for one without.
type writerToBody struct{ *bytes.Reader }

func (b writerToBody) WriteTo(w io.Writer) (int64, error) {
	return io.CopyBuffer(w, struct{ io.Reader }{b.Reader}, make([]byte, 32<<10))
}

// Many streams ending every way they can, at once, leave nothing running.
func TestStreamsLeaveNoGoroutines(t *testing.T) {
	app := New(quietOptions())
	app.Get("/ok", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: newRecordingBody(strings.NewReader("fine")), Length: 4}, nil
	})
	app.Get("/fail", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: newRecordingBody(&brokenBody{data: []byte("a"), err: io.ErrUnexpectedEOF})}, nil
	})
	app.Get("/error", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: newRecordingBody(strings.NewReader("x"))}, errors.New("nope")
	})
	server := newWireServer(t, app)
	var wg sync.WaitGroup
	for range 10 {
		for _, target := range []struct{ method, path string }{
			{http.MethodGet, "/ok"}, {http.MethodHead, "/ok"}, {http.MethodGet, "/fail"}, {http.MethodGet, "/error"},
		} {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, target.method, server.URL+target.path, nil)
				transport := &http.Transport{}
				defer transport.CloseIdleConnections()
				if res, err := transport.RoundTrip(req); err == nil {
					_, _ = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
				}
			})
		}
	}
	wg.Wait()
	server.Close()
	assertNoGoroutineLeaks(t)
}
