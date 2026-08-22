package testclient

import (
	"net/http"

	"badele"
)

// Subprotocols offers WebSocket subprotocols on a [Client.WS] call, in order
// of preference. It has no effect on any other kind of request.
func Subprotocols(names ...string) RequestOption {
	return func(r *request) { r.subprotocols = append(r.subprotocols, names...) }
}

// WS opens a WebSocket connection to the application and fails the test if the
// handshake is refused.
//
// Everything a request option can add applies to the handshake, so a header,
// a query parameter or a cookie is sent the way it would be for any other
// call, and the client's cookie jar carries whatever a login flow established:
//
//	client := testclient.New(t, app)
//	conn := client.WS("/items/plumbus/ws", testclient.Query("token", "jessica"))
//
//	if err := conn.WriteText(t.Context(), "hello"); err != nil {
//		t.Fatalf("WriteText = %v", err)
//	}
//	reply, err := conn.ReadText(t.Context())
//
// The connection is closed when the test finishes, so a handler blocked on a
// read is released even if the test forgets. Use [Client.TryWS] to assert on a
// handshake that is meant to be refused.
func (c *Client) WS(path string, opts ...RequestOption) *badele.WSConn {
	c.tb.Helper()
	conn, response := c.TryWS(path, opts...)
	if conn == nil {
		// coverage: aborts the running test, which Go does not permit a fake
		// testing.TB to observe; the refusal path itself is covered through
		// TryWS.
		c.tb.Fatalf("testclient: WS %s: the handshake was refused with status %d\nbody: %s",
			path, response.Status, response.Body)
	}
	return conn
}

// TryWS opens a WebSocket connection and returns whatever came back, for a
// test that expects the handshake to be refused.
//
// The connection is nil unless the handshake succeeded, and the response is
// the one the server sent, so a refusal is asserted on exactly as any other
// response is:
//
//	_, response := client.TryWS("/items/plumbus/ws")
//	response.AssertStatus(401)
func (c *Client) TryWS(path string, opts ...RequestOption) (*badele.WSConn, *Response) {
	c.tb.Helper()
	req, target := c.build(http.MethodGet, path, opts)

	conn, httpResponse, err := badele.WSDial(c.tb.Context(), target, badele.WSDialOptions{
		HTTPClient:   c.http,
		Header:       req.header,
		Subprotocols: req.subprotocols,
	})
	if httpResponse == nil {
		// coverage: aborts the running test; a nil response means the request
		// never reached the application at all.
		c.tb.Fatalf("testclient: WS %s failed: %v", path, err)
	}
	response := &Response{
		tb:      c.tb,
		method:  http.MethodGet,
		path:    path,
		Status:  httpResponse.StatusCode,
		Header:  httpResponse.Header,
		Cookies: httpResponse.Cookies(),
	}
	if conn == nil {
		response.Body = c.readBody(http.MethodGet, path, httpResponse)
		_ = httpResponse.Body.Close()
		return nil, response
	}
	// The body of a handshake that succeeded is the connection itself, so it
	// is closed by closing the connection and never before.
	c.tb.Cleanup(func() { _ = conn.Close(badele.WSStatusNormalClosure, "") })
	return conn, response
}
