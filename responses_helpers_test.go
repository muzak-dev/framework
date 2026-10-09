package muzak

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// wireServer serves an application over a real connection, for what a
// response recorder cannot show: a HEAD answered by net/http, a body cut
// short, a connection aborted after its header was sent.
type wireServer struct {
	*httptest.Server
}

// newWireServer starts a server for app that is closed when the test ends.
func newWireServer(t *testing.T, app *App) *wireServer {
	t.Helper()
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return &wireServer{server}
}

// wireResponse is a response read to its end, or to the error that ended it.
type wireResponse struct {
	*http.Response
	Data    []byte
	ReadErr error
}

// do sends one request with the given headers, as alternating names and
// values, and reads the whole response, failing the test if none arrives.
func (s *wireServer) do(t *testing.T, method, path string, headers ...string) *wireResponse {
	t.Helper()
	res, err := s.try(method, path, headers...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

// try is [wireServer.do] for a request whose response may never arrive: a
// connection aborted before the server flushed its header is seen by the
// client as no response at all.
func (s *wireServer) try(method, path string, headers ...string) (*wireResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.URL+path, nil)
	if err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	// No transparent decompression, so what the test sees is what was sent.
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	res, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, readErr := io.ReadAll(res.Body)
	return &wireResponse{Response: res, Data: data, ReadErr: readErr}, nil
}

// failedTransfer reports whether a request's response failed on the way:
// either it never arrived or its body ended in an error.
func failedTransfer(res *wireResponse, err error) bool {
	return err != nil || res.ReadErr != nil
}

// recordingBody is a Stream body that counts how often it is read and closed,
// which is what proves a body was closed on a given path, and closed once.
type recordingBody struct {
	io.Reader
	reads  atomic.Int32
	closes atomic.Int32
	// closed is closed on the first Close, for a test waiting for one that
	// happens on the server's goroutine.
	closed   chan struct{}
	closeOne sync.Once
}

// newRecordingBody wraps r.
func newRecordingBody(r io.Reader) *recordingBody {
	return &recordingBody{Reader: r, closed: make(chan struct{})}
}

func (b *recordingBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.Reader.Read(p)
}

// Close records the close and passes it on to the reader underneath, which
// for a pipe is what tells its writer to stop.
func (b *recordingBody) Close() error {
	b.closes.Add(1)
	b.closeOne.Do(func() { close(b.closed) })
	if closer, ok := b.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// waitClosed fails the test unless the body is closed within a few seconds.
func (b *recordingBody) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-b.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the Stream's body was never closed")
	}
}

// assertClosedOnce fails the test unless the body was closed exactly once.
func (b *recordingBody) assertClosedOnce(t *testing.T) {
	t.Helper()
	if n := b.closes.Load(); n != 1 {
		t.Errorf("the body was closed %d times, want exactly once", n)
	}
}

// brokenBody yields its data and then fails with err.
type brokenBody struct {
	data []byte
	err  error
}

func (r *brokenBody) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// explodingBody panics on its first read.
type explodingBody struct{}

func (explodingBody) Read([]byte) (int, error) { panic("the body exploded") }
