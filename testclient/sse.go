package testclient

import (
	"context"
	"net/http"
	"testing"
	"time"

	"muzak.dev/framework"
)

// KeepComments delivers the comment lines of an event stream as messages of
// their own, which is how a test sees the keepalives a server sends. It has no
// effect on any other kind of request.
func KeepComments() RequestOption {
	return func(r *request) { r.sseComments = true }
}

// SSE opens an event stream and fails the test if the response is not one.
//
// Everything a request option can add applies to the request, so a header, a
// query parameter or a cookie is sent the way it would be for any other call,
// and the client's cookie jar carries whatever a login flow established:
//
//	client := testclient.New(t, app)
//	stream := client.SSE("/items/stream", testclient.Query("token", "jessica"))
//
//	item := stream.Decode[ItemOut]()
//
// The stream is closed when the test finishes, so a handler blocked on a send
// is released even if the test forgets. Use [Client.SSEDo] for a stream opened
// with another method, and [Client.TrySSE] to assert on a request that is
// meant to be refused.
func (c *Client) SSE(path string, opts ...RequestOption) *SSEStream {
	c.tb.Helper()
	return c.SSEDo(http.MethodGet, path, opts...)
}

// SSEDo opens an event stream with an arbitrary method, which is what a stream
// answering a posted document takes:
//
//	stream := client.SSEDo(http.MethodPost, "/chat/stream", testclient.JSON(prompt))
func (c *Client) SSEDo(method, path string, opts ...RequestOption) *SSEStream {
	c.tb.Helper()
	stream, response := c.TrySSE(method, path, opts...)
	if stream == nil {
		// coverage: aborts the running test, which Go does not permit a fake
		// testing.TB to observe; the refusal path itself is covered through
		// TrySSE.
		c.tb.Fatalf("testclient: SSE %s %s: the stream was refused with status %d\nbody: %s",
			method, path, response.Status, response.Body)
	}
	return stream
}

// TrySSE opens an event stream and returns whatever came back, for a test that
// expects the request to be refused.
//
// The stream is nil unless the response was an event stream, and the response
// is the one the server sent, so a refusal is asserted on exactly as any other
// response is:
//
//	_, response := client.TrySSE(http.MethodGet, "/items/stream")
//	response.AssertStatus(503)
func (c *Client) TrySSE(method, path string, opts ...RequestOption) (*SSEStream, *Response) {
	c.tb.Helper()
	req, target := c.build(method, path, opts)

	reader, httpResponse, err := muzak.SSEDial(c.tb.Context(), target, muzak.SSEDialOptions{
		HTTPClient:   c.http,
		Method:       method,
		Body:         req.body,
		Header:       req.header,
		KeepComments: req.sseComments,
	})
	if httpResponse == nil {
		// coverage: aborts the running test; a nil response means the request
		// never reached the application at all.
		c.tb.Fatalf("testclient: SSE %s %s failed: %v", method, path, err)
	}
	response := &Response{
		tb:      c.tb,
		method:  method,
		path:    path,
		Status:  httpResponse.StatusCode,
		Header:  httpResponse.Header,
		Cookies: httpResponse.Cookies(),
	}
	if reader == nil {
		response.Body = c.readBody(method, path, httpResponse)
		_ = httpResponse.Body.Close()
		return nil, response
	}
	// The body of a stream that opened is the stream itself, so it is closed
	// by closing the reader and never before.
	c.tb.Cleanup(func() { _ = reader.Close() })
	return &SSEStream{
		tb:       c.tb,
		method:   method,
		path:     path,
		reader:   reader,
		timeout:  c.http.Timeout,
		Response: response,
	}, response
}

// SSEStream is an open event stream under test.
//
// Reads are bounded by the client's timeout, so a stream that never sends the
// event a test is waiting for fails the test rather than hanging it.
type SSEStream struct {
	// Response carries the status, headers and cookies the stream opened with.
	Response *Response

	tb      testing.TB
	method  string
	path    string
	reader  *muzak.SSEReader
	timeout time.Duration
}

// Next returns the next event, failing the test if the stream ends or fails
// first.
func (s *SSEStream) Next() muzak.SSEMessage {
	s.tb.Helper()
	message, err := s.TryNext()
	if err != nil {
		// coverage: aborts the running test; a stream that ended when an event
		// was expected is covered through TryNext.
		s.tb.Fatalf("testclient: SSE %s %s: reading an event: %v", s.method, s.path, err)
	}
	return message
}

// TryNext returns the next event and whatever ended the stream, for a test
// that expects it to end:
//
//	_, err := stream.TryNext()
//	if !errors.Is(err, muzak.ErrSSEStreamEnded) {
//		t.Fatalf("err = %v, want the stream to have ended", err)
//	}
func (s *SSEStream) TryNext() (muzak.SSEMessage, error) {
	s.tb.Helper()
	ctx := s.tb.Context()
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	return s.reader.Next(ctx)
}

// Decode reads the next event and decodes its data into a value of type T,
// failing the test if the stream ends first or the data does not fit:
//
//	item := stream.Decode[ItemOut]()
func (s *SSEStream) Decode[T any]() T {
	s.tb.Helper()
	message := s.Next()
	value, err := message.Decode[T]()
	if err != nil {
		// coverage: aborts the running test; the decoding itself is covered by
		// every call that reads an event.
		s.tb.Fatalf("testclient: SSE %s %s: %v\ndata: %s", s.method, s.path, err, message.Data)
	}
	return value
}

// LastEventID returns the identifier of the last complete event that carried
// one, which is what a test resuming a stream sends back with
// testclient.Header("Last-Event-ID", id). An event the stream cut off part way
// leaves it where it was; see [muzak.SSEReader.LastEventID].
func (s *SSEStream) LastEventID() string { return s.reader.LastEventID() }

// Close ends the stream, which is what a test does to check that the handler
// notices. The stream is closed at the end of the test in any case.
func (s *SSEStream) Close() { _ = s.reader.Close() }
