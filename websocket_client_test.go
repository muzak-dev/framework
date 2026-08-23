package muzak

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serveRawWS answers a handshake by hand and hands the connection to fn, for
// the tests that need a server doing something Muzak would never do.
//
// The extra headers replace the ones the handshake would otherwise write, so a
// test can answer with a wrong accept value or a subprotocol nobody offered.
func serveRawWS(t *testing.T, replace map[string]string, fn func(conn net.Conn)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept, err := wsAcceptKey(r.Header.Get("Sec-WebSocket-Key"))
		if err != nil {
			t.Errorf("the client sent a key the server could not use: %v", err)
			return
		}
		headers := map[string]string{
			"Upgrade":              "websocket",
			"Connection":           "Upgrade",
			"Sec-WebSocket-Accept": accept,
		}
		for name, value := range replace {
			headers[name] = value
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijacking: %v", err)
			return
		}
		response := "HTTP/1.1 101 Switching Protocols\r\n"
		for name, value := range headers {
			if value != "" {
				response += name + ": " + value + "\r\n"
			}
		}
		if _, err := brw.WriteString(response + "\r\n"); err != nil || brw.Flush() != nil {
			_ = conn.Close()
			return
		}
		fn(conn)
	}))
	t.Cleanup(server.Close)
	return server
}

// dialClient opens a connection with the dialer under test and closes it when
// the test finishes.
func dialClient(t *testing.T, serverURL string, opts WSDialOptions) *WSConn {
	t.Helper()
	conn, _, err := WSDial(t.Context(), serverURL, opts)
	if err != nil {
		t.Fatalf("WSDial(%s) = %v", serverURL, err)
	}
	t.Cleanup(func() { _ = conn.Close(WSStatusNormalClosure, "") })
	return conn
}

func TestWSDialRoundTrip(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadLimit: 1 << 20}))
	})
	conn := dialClient(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws", WSDialOptions{})

	if err := conn.WriteText(t.Context(), "hello"); err != nil {
		t.Fatalf("WriteText = %v", err)
	}
	got, err := conn.ReadText(t.Context())
	if err != nil || got != "hello" {
		t.Fatalf("ReadText = %q, %v, want %q", got, err, "hello")
	}

	// Larger than the scratch buffer, so that the chunked masking a client
	// does for a big message is exercised rather than assumed.
	large := make([]byte, 3*wsScratchSize+11)
	for i := range large {
		large[i] = byte(i * 3)
	}
	if err := conn.WriteBinary(t.Context(), large); err != nil {
		t.Fatalf("WriteBinary = %v", err)
	}
	echoed, err := conn.ReadBinary(t.Context())
	if err != nil {
		t.Fatalf("ReadBinary = %v", err)
	}
	if string(echoed) != string(large) {
		t.Errorf("the echoed payload of %d bytes does not match the %d sent", len(echoed), len(large))
	}

	// The payload the caller passed must not have been masked in place.
	for i := range large {
		if large[i] != byte(i*3) {
			t.Fatalf("the caller's payload was modified at byte %d", i)
		}
	}
}

func TestWSDialJSONAndPing(t *testing.T) {
	t.Parallel()
	type note struct {
		Text string `json:"text"`
	}
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			var in note
			if err := conn.ReadJSON(ctx.Context(), &in); err != nil {
				return err
			}
			if err := conn.Ping(ctx.Context()); err != nil {
				return err
			}
			return conn.WriteJSON(ctx.Context(), note{Text: strings.ToUpper(in.Text)})
		})
	})
	conn := dialClient(t, server.URL+"/ws", WSDialOptions{})
	if err := conn.WriteJSON(t.Context(), note{Text: "shout"}); err != nil {
		t.Fatalf("WriteJSON = %v", err)
	}
	// The ping the server sends is answered by the read below without the
	// caller having to know it happened.
	var out note
	if err := conn.ReadJSON(t.Context(), &out); err != nil {
		t.Fatalf("ReadJSON = %v", err)
	}
	if out.Text != "SHOUT" {
		t.Errorf("text = %q, want %q", out.Text, "SHOUT")
	}
}

func TestWSDialSendsHeadersAndNegotiates(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			return conn.WriteText(ctx.Context(), ctx.Header("Authorization")+"|"+conn.Subprotocol())
		}, WithWebSocket(WSOptions{Subprotocols: []string{"chat.v1"}}))
	})
	conn := dialClient(t, server.URL+"/ws", WSDialOptions{
		Header:       http.Header{"Authorization": []string{"Bearer secret"}},
		Subprotocols: []string{"chat.v1", "chat.v2"},
	})
	if conn.Subprotocol() != "chat.v1" {
		t.Errorf("Subprotocol() = %q, want %q", conn.Subprotocol(), "chat.v1")
	}
	got, err := conn.ReadText(t.Context())
	if err != nil {
		t.Fatalf("ReadText = %v", err)
	}
	if got != "Bearer secret|chat.v1" {
		t.Errorf("the server saw %q", got)
	}
}

func TestWSDialRefusals(t *testing.T) {
	t.Parallel()
	_, refused := newWSTestApp(t, func(app *App) {
		app.WS("/guarded", wsEcho)
	}, WithDependencies(func(*Context) error {
		return NewHTTPError(http.StatusForbidden, "no entry")
	}))

	t.Run("a handshake the server refused", func(t *testing.T) {
		t.Parallel()
		conn, response, err := WSDial(t.Context(), refused.URL+"/guarded", WSDialOptions{})
		if err == nil {
			t.Fatal("WSDial succeeded, want the refusal reported")
		}
		if conn != nil {
			t.Error("a connection was returned for a handshake that failed")
		}
		if response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("response = %v, want the 403 the server sent", response)
		}
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil || !strings.Contains(string(body), "no entry") {
			t.Errorf("body = %q, %v, want the server's own message", body, readErr)
		}
	})

	cases := []struct {
		name    string
		replace map[string]string
		opts    WSDialOptions
		message string
	}{
		{
			name:    "an answer that does not upgrade",
			replace: map[string]string{"Upgrade": "", "Connection": ""},
			message: "without upgrading",
		},
		{
			name:    "an accept value that does not match",
			replace: map[string]string{"Sec-WebSocket-Accept": "AAAAAAAAAAAAAAAAAAAAAAAAAAA="},
			message: "does not match",
		},
		{
			name:    "a subprotocol nobody offered",
			replace: map[string]string{"Sec-WebSocket-Protocol": "chat.v9"},
			message: "was not offered",
		},
		{
			name:    "a subprotocol other than the one offered",
			replace: map[string]string{"Sec-WebSocket-Protocol": "chat.v9"},
			opts:    WSDialOptions{Subprotocols: []string{"chat.v1"}},
			message: "was not offered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := serveRawWS(t, tc.replace, func(conn net.Conn) { _ = conn.Close() })
			_, _, err := WSDial(t.Context(), server.URL, tc.opts)
			if err == nil {
				t.Fatal("WSDial succeeded, want the answer refused")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error = %q, want it to mention %q", err, tc.message)
			}
		})
	}
}

func TestWSDialRefusesAMaskedFrameFromAServer(t *testing.T) {
	t.Parallel()
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		// A server must never mask. One that does is refused, which is the
		// mirror image of the rule a client is held to.
		key := []byte{1, 2, 3, 4}
		payload := []byte("masked")
		for i := range payload {
			payload[i] ^= key[i%4]
		}
		frame := append(frameHeader(true, opText, len(payload), key), payload...)
		_, _ = conn.Write(frame)
		// The client's own close frame is read so that the connection is not
		// reset before it arrives.
		_, _ = io.Copy(io.Discard, conn)
	})
	conn, _, err := WSDial(t.Context(), server.URL, WSDialOptions{})
	if err != nil {
		t.Fatalf("WSDial = %v", err)
	}
	defer func() { _ = conn.Close(WSStatusNormalClosure, "") }()

	_, _, readErr := conn.Read(t.Context())
	status, ok := WSCloseStatus(readErr)
	if !ok || status != WSStatusProtocolError {
		t.Fatalf("read error = %v, want a protocol error", readErr)
	}
	if !strings.Contains(readErr.Error(), "must not be masked") {
		t.Errorf("error = %q, want it to name the rule that was broken", readErr)
	}
}

func TestWSDialReadLimit(t *testing.T) {
	t.Parallel()
	server := serveRawWS(t, nil, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		payload := make([]byte, 200)
		_, _ = conn.Write(append(frameHeader(true, opBinary, len(payload), nil), payload...))
		_, _ = io.Copy(io.Discard, conn)
	})
	conn, _, err := WSDial(t.Context(), server.URL, WSDialOptions{ReadLimit: 100})
	if err != nil {
		t.Fatalf("WSDial = %v", err)
	}
	defer func() { _ = conn.Close(WSStatusNormalClosure, "") }()

	_, _, readErr := conn.Read(t.Context())
	if status, ok := WSCloseStatus(readErr); !ok || status != WSStatusMessageTooBig {
		t.Fatalf("read error = %v, want the message refused as too big", readErr)
	}
}

func TestWSDialCancellationClosesTheConnection(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			_, _, err := conn.Read(ctx.Context())
			return err
		})
	})
	conn := dialClient(t, server.URL+"/ws", WSDialOptions{})

	// A connection reached through an HTTP client has no deadlines to set, so
	// cancelling a read can only end it by closing the transport.
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, _, err := conn.Read(ctx); !errors.Is(err, context.Canceled) && conn.failure() == nil {
		t.Fatalf("read error = %v, want the cancellation to end the connection", err)
	}
	if _, _, err := conn.Read(t.Context()); err == nil {
		t.Error("a read after cancellation succeeded, want the connection to be finished")
	}
}

func TestWSDialStripsTheClientTimeout(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			time.Sleep(60 * time.Millisecond)
			return conn.WriteText(ctx.Context(), "late but here")
		})
	})
	// A client timeout bounds the whole life of a response body, so a
	// connection dialled with one would be cut off mid-conversation.
	conn := dialClient(t, server.URL+"/ws", WSDialOptions{
		HTTPClient: &http.Client{Timeout: 30 * time.Millisecond},
	})
	got, err := conn.ReadText(t.Context())
	if err != nil || got != "late but here" {
		t.Fatalf("ReadText = %q, %v, want the message to arrive after the client timeout", got, err)
	}
}

func TestWSRequestURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, out string
		fails   bool
	}{
		{in: "ws://example.com/chat", out: "http://example.com/chat"},
		{in: "wss://example.com/chat", out: "https://example.com/chat"},
		{in: "http://example.com/chat", out: "http://example.com/chat"},
		{in: "https://example.com/chat?room=1", out: "https://example.com/chat?room=1"},
		{in: "ftp://example.com", fails: true},
		{in: "/chat", fails: true},
		{in: "ws://exa mple.com", fails: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := wsRequestURL(tc.in)
			if tc.fails {
				if err == nil {
					t.Fatalf("wsRequestURL(%q) = %q, want a refusal", tc.in, got)
				}
				return
			}
			if err != nil || got != tc.out {
				t.Errorf("wsRequestURL(%q) = %q, %v, want %q", tc.in, got, err, tc.out)
			}
		})
	}
}

func TestWSDialReportsATransportFailure(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}
	_, _, err = WSDial(t.Context(), "ws://"+address+"/ws", WSDialOptions{})
	if err == nil || !strings.Contains(err.Error(), "handshake failed") {
		t.Fatalf("WSDial to a closed port = %v, want the handshake reported as failed", err)
	}
}

func TestWSDialClient(t *testing.T) {
	t.Parallel()
	if got := wsDialClient(nil); got == nil || got.CheckRedirect == nil {
		t.Fatal("no client was built for a caller that supplied none")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("building a cookie jar: %v", err)
	}
	caller := &http.Client{Timeout: time.Second, Jar: jar}
	dialer := wsDialClient(caller)
	if dialer == caller {
		t.Fatal("the caller's own client was handed back, so changing it would change theirs")
	}
	if dialer.Timeout != 0 {
		t.Error("the caller's timeout was not removed, and would have bounded the conversation")
	}
	if dialer.Jar != jar {
		t.Error("the cookie jar was lost, so a session established earlier would not reach the handshake")
	}
	if caller.Timeout != time.Second || caller.CheckRedirect != nil {
		t.Error("the caller's own client was modified")
	}
	// Following a redirect would send the handshake headers, an Authorization
	// header among them, to whatever host the answer named.
	if dialer.CheckRedirect == nil {
		t.Fatal("the dialer follows redirects")
	}
	if err := dialer.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want the redirect refused", err)
	}
}

func TestWSDialDoesNotFollowARedirect(t *testing.T) {
	t.Parallel()
	// A server that answers a handshake with a redirect gets no second
	// request, and the caller is told what it actually said.
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Store(true)
	}))
	t.Cleanup(elsewhere.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)

	conn, response, err := WSDial(t.Context(), redirecting.URL, WSDialOptions{
		Header: http.Header{"Authorization": []string{"Bearer secret"}},
	})
	if err == nil || conn != nil {
		t.Fatalf("WSDial = %v, %v, want the redirect refused", conn, err)
	}
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the redirect itself", response.StatusCode)
	}
	if reached.Load() {
		t.Error("the handshake was sent on to the host the redirect named, headers and all")
	}
}

func TestWSDialRejectsAnAddressThatIsNotOne(t *testing.T) {
	t.Parallel()
	conn, response, err := WSDial(t.Context(), "ftp://example.com", WSDialOptions{})
	if err == nil || conn != nil || response != nil {
		t.Fatalf("WSDial(ftp) = %v, %v, %v, want a refusal and nothing else", conn, response, err)
	}
	if !strings.Contains(err.Error(), "not a websocket address") {
		t.Errorf("error = %q, want it to say what a websocket address looks like", err)
	}
}
