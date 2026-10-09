package muzak

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The frame opcodes, written out here rather than imported, so that these
// tests describe the wire format independently of the code that implements it.
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// testWSKey and testWSAccept are the handshake pair from RFC 6455 section 1.3,
// which is what lets the accept header be checked against a value nothing in
// this module computed.
const (
	testWSKey    = "dGhlIHNhbXBsZSBub25jZQ=="
	testWSAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
)

// wsTestTimeout bounds every read a test makes, so that a broken engine fails
// a test rather than hanging one.
const wsTestTimeout = 5 * time.Second

// rawConn is a hand-written WebSocket client used by these tests.
//
// It shares no code with the engine under test, on purpose: frames are built
// and read here byte by byte from the specification, so a mistake in the codec
// cannot hide by being made identically on both sides of the conversation.
type rawConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

// newWSTestApp builds an application with the given routes and serves it over
// a real socket, which is what a handshake needs: a response recorder has no
// connection to hand over.
func newWSTestApp(t *testing.T, register func(app *App), opts ...RouterOption) (*App, *httptest.Server) {
	t.Helper()
	app := New(quietOptions(), opts...)
	register(app)
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return app, server
}

// dialRaw performs a handshake by hand and returns the connection underneath
// it. The extra arguments are header name and value pairs.
func dialRaw(t *testing.T, serverURL, path string, headers ...string) (*rawConn, *http.Response) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parsing the server URL: %v", err)
	}
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatalf("dialling %s: %v", parsed.Host, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var request strings.Builder
	request.WriteString("GET " + path + " HTTP/1.1\r\n")
	request.WriteString("Host: " + parsed.Host + "\r\n")
	sent := map[string]bool{}
	for i := 0; i+1 < len(headers); i += 2 {
		request.WriteString(headers[i] + ": " + headers[i+1] + "\r\n")
		sent[strings.ToLower(headers[i])] = true
	}
	for _, standard := range [][2]string{
		{"upgrade", "Upgrade: websocket"},
		{"connection", "Connection: Upgrade"},
		{"sec-websocket-version", "Sec-WebSocket-Version: 13"},
		{"sec-websocket-key", "Sec-WebSocket-Key: " + testWSKey},
	} {
		if !sent[standard[0]] {
			request.WriteString(standard[1] + "\r\n")
		}
	}
	request.WriteString("\r\n")

	if err := conn.SetDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		t.Fatalf("setting a deadline: %v", err)
	}
	if _, err := conn.Write([]byte(request.String())); err != nil {
		t.Fatalf("sending the handshake: %v", err)
	}
	br := bufio.NewReader(conn)
	response, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the handshake response: %v", err)
	}
	return &rawConn{t: t, conn: conn, br: br}, response
}

// dialWS performs a handshake and fails the test unless it succeeded.
func dialWS(t *testing.T, serverURL, path string, headers ...string) *rawConn {
	t.Helper()
	conn, response := dialRaw(t, serverURL, path, headers...)
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<10))
		t.Fatalf("handshake status = %d, want 101\nbody: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Sec-WebSocket-Accept"); got != testWSAccept {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, testWSAccept)
	}
	return conn
}

// send writes one masked frame, which is what every frame from a client has to
// be.
func (c *rawConn) send(fin bool, opcode byte, payload []byte) {
	c.t.Helper()
	key := [4]byte{0x11, 0x22, 0x33, 0x44}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ key[i%4]
	}
	c.sendRaw(append(frameHeader(fin, opcode, len(payload), key[:]), masked...))
}

// sendUnmasked writes a frame with no masking key, which a server must refuse.
func (c *rawConn) sendUnmasked(fin bool, opcode byte, payload []byte) {
	c.t.Helper()
	c.sendRaw(append(frameHeader(fin, opcode, len(payload), nil), payload...))
}

// sendRaw writes bytes straight onto the connection, for the malformed frames
// no helper would build.
func (c *rawConn) sendRaw(b []byte) {
	c.t.Helper()
	if err := c.conn.SetWriteDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		c.t.Fatalf("setting a write deadline: %v", err)
	}
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("writing a frame: %v", err)
	}
}

// text sends one whole text message.
func (c *rawConn) text(message string) {
	c.t.Helper()
	c.send(true, opText, []byte(message))
}

// frameHeader builds a frame header from its parts.
func frameHeader(fin bool, opcode byte, length int, key []byte) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	header := []byte{first}
	mask := byte(0)
	if key != nil {
		mask = 0x80
	}
	switch {
	case length <= 125:
		header = append(header, mask|byte(length))
	case length <= 0xFFFF:
		header = append(header, mask|126, byte(length>>8), byte(length))
	default:
		header = append(header, mask|127)
		for shift := 56; shift >= 0; shift -= 8 {
			header = append(header, byte(length>>shift))
		}
	}
	return append(header, key...)
}

// recv reads one frame, failing the test if none arrives.
func (c *rawConn) recv() (fin bool, opcode byte, payload []byte) {
	c.t.Helper()
	fin, opcode, payload, err := c.tryRecv()
	if err != nil {
		c.t.Fatalf("reading a frame: %v", err)
	}
	return fin, opcode, payload
}

// tryRecv reads one frame and reports whatever went wrong instead of failing,
// for the tests that expect the connection to have gone.
func (c *rawConn) tryRecv() (fin bool, opcode byte, payload []byte, err error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		return false, 0, nil, err
	}
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	opcode = head[0] & 0x0F
	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.br, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.br, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(extended[:]))
	}
	if head[1]&0x80 != 0 {
		return false, 0, nil, errors.New("the server masked a frame, which it must never do")
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, opcode, payload, nil
}

// expectText reads one frame and checks that it is the text expected.
func (c *rawConn) expectText(want string) {
	c.t.Helper()
	fin, opcode, payload := c.recv()
	if !fin || opcode != opText || string(payload) != want {
		c.t.Fatalf("frame = (fin %v, opcode %#x, %q), want a final text frame of %q", fin, opcode, payload, want)
	}
}

// expectClose reads the close frame and checks the status it carries,
// returning the reason so that a test can assert on it.
func (c *rawConn) expectClose(want uint16) string {
	c.t.Helper()
	fin, opcode, payload := c.recv()
	if !fin || opcode != opClose {
		c.t.Fatalf("frame = (fin %v, opcode %#x), want a close frame", fin, opcode)
	}
	if want == 0 {
		if len(payload) != 0 {
			c.t.Fatalf("close payload = % x, want none", payload)
		}
		return ""
	}
	if len(payload) < 2 {
		c.t.Fatalf("close payload = % x, want a status code of %d", payload, want)
	}
	if got := binary.BigEndian.Uint16(payload); got != want {
		c.t.Fatalf("close status = %d (%q), want %d", got, payload[2:], want)
	}
	return string(payload[2:])
}

// expectEOF checks that the server closed the transport after its close frame,
// which is what the specification requires of a server.
func (c *rawConn) expectEOF() {
	c.t.Helper()
	if _, _, _, err := c.tryRecv(); err == nil {
		c.t.Fatal("the connection is still open, want it closed after the close frame")
	}
}

// waitForLog waits for a message to appear in a captured log, for the records
// written after the client has already been told the connection is over.
func waitForLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(wsTestTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the log never mentioned %q; it holds:\n%s", want, logs.String())
}

// wsEcho sends every message back as it arrived.
func wsEcho(ctx *Context, _ Empty, conn *WSConn) error {
	for {
		typ, payload, err := conn.Read(ctx.Context())
		if err != nil {
			return nil
		}
		if err := conn.Write(ctx.Context(), typ, payload); err != nil {
			return err
		}
	}
}

// echoApp serves the echo handler at /ws.
func echoApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	return newWSTestApp(t, func(app *App) { app.WS("/ws", wsEcho) })
}

func TestWebSocketEchoesTextAndBinary(t *testing.T) {
	t.Parallel()
	_, server := echoApp(t)
	conn := dialWS(t, server.URL, "/ws")

	conn.text("hello")
	conn.expectText("hello")

	conn.send(true, opBinary, []byte{0x00, 0xFF, 0x7F})
	fin, opcode, payload := conn.recv()
	if !fin || opcode != opBinary || string(payload) != "\x00\xff\x7f" {
		t.Fatalf("frame = (fin %v, opcode %#x, % x), want the binary payload back", fin, opcode, payload)
	}

	// An empty message is a message, and must come back as one.
	conn.text("")
	conn.expectText("")

	conn.send(true, opClose, closePayload(1000, "done"))
	conn.expectClose(1000)
	conn.expectEOF()
}

// closePayload builds the payload of a close frame.
func closePayload(status uint16, reason string) []byte {
	payload := binary.BigEndian.AppendUint16(nil, status)
	return append(payload, reason...)
}

func TestWebSocketReassemblesFragments(t *testing.T) {
	t.Parallel()
	_, server := echoApp(t)
	conn := dialWS(t, server.URL, "/ws")

	conn.send(false, opText, []byte("one "))
	conn.send(false, opContinuation, []byte("two "))
	conn.send(true, opContinuation, []byte("three"))
	conn.expectText("one two three")

	// A fragment may be empty, and the message still ends where FIN says.
	conn.send(false, opText, []byte("only"))
	conn.send(true, opContinuation, nil)
	conn.expectText("only")
}

func TestWebSocketAnswersControlFramesBetweenFragments(t *testing.T) {
	t.Parallel()
	_, server := echoApp(t)
	conn := dialWS(t, server.URL, "/ws")

	conn.send(false, opText, []byte("interrupted"))
	conn.send(true, opPing, []byte("are you there"))

	fin, opcode, payload := conn.recv()
	if !fin || opcode != opPong || string(payload) != "are you there" {
		t.Fatalf("frame = (fin %v, opcode %#x, %q), want the ping answered with its own payload", fin, opcode, payload)
	}

	// A pong the server did not ask for is ignored rather than answered.
	conn.send(true, opPong, []byte("unsolicited"))
	conn.send(true, opContinuation, []byte(" message"))
	conn.expectText("interrupted message")
}

func TestWebSocketRefusesMalformedFrames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		send   func(*rawConn)
		status uint16
	}{
		{
			name:   "unmasked frame from a client",
			send:   func(c *rawConn) { c.sendUnmasked(true, opText, []byte("plain")) },
			status: 1002,
		},
		{
			name:   "reserved opcode",
			send:   func(c *rawConn) { c.send(true, 0x3, nil) },
			status: 1002,
		},
		{
			name:   "reserved control opcode",
			send:   func(c *rawConn) { c.send(true, 0xB, nil) },
			status: 1002,
		},
		{
			name:   "reserved bit set",
			send:   func(c *rawConn) { c.sendRaw([]byte{0xC1, 0x80, 1, 2, 3, 4}) },
			status: 1002,
		},
		{
			name:   "fragmented control frame",
			send:   func(c *rawConn) { c.send(false, opPing, nil) },
			status: 1002,
		},
		{
			name:   "oversized control frame",
			send:   func(c *rawConn) { c.send(true, opPing, make([]byte, 126)) },
			status: 1002,
		},
		{
			name:   "length not minimally encoded",
			send:   func(c *rawConn) { c.sendRaw([]byte{0x81, 0xFE, 0x00, 0x05, 1, 2, 3, 4, 0, 0, 0, 0, 0}) },
			status: 1002,
		},
		{
			name:   "continuation with nothing to continue",
			send:   func(c *rawConn) { c.send(true, opContinuation, []byte("orphan")) },
			status: 1002,
		},
		{
			name: "a new message before the last one finished",
			send: func(c *rawConn) {
				c.send(false, opText, []byte("first"))
				c.send(true, opText, []byte("second"))
			},
			status: 1002,
		},
		{
			name:   "text that is not UTF-8",
			send:   func(c *rawConn) { c.send(true, opText, []byte{0xFF, 0xFE}) },
			status: 1007,
		},
		{
			name: "text that is not UTF-8 across fragments",
			send: func(c *rawConn) {
				c.send(false, opText, []byte{0xC3})
				c.send(true, opContinuation, []byte{0x28})
			},
			status: 1007,
		},
		{
			name:   "close payload of one byte",
			send:   func(c *rawConn) { c.send(true, opClose, []byte{0x03}) },
			status: 1002,
		},
		{
			name:   "close with a reserved status",
			send:   func(c *rawConn) { c.send(true, opClose, closePayload(1005, "")) },
			status: 1002,
		},
		{
			name:   "close with a reason that is not UTF-8",
			send:   func(c *rawConn) { c.send(true, opClose, append(closePayload(1000, ""), 0xFF)) },
			status: 1007,
		},
	}
	_, server := echoApp(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := dialWS(t, server.URL, "/ws")
			tc.send(conn)
			conn.expectClose(tc.status)
			conn.expectEOF()
		})
	}
}

func TestWebSocketRefusesAMessageOverTheReadLimit(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadLimit: 64}))
	})

	t.Run("one oversized frame", func(t *testing.T) {
		t.Parallel()
		conn := dialWS(t, server.URL, "/ws")
		conn.send(true, opText, []byte(strings.Repeat("a", 65)))
		conn.expectClose(1009)
	})

	t.Run("fragments that add up", func(t *testing.T) {
		t.Parallel()
		conn := dialWS(t, server.URL, "/ws")
		conn.send(false, opText, []byte(strings.Repeat("a", 40)))
		conn.send(true, opContinuation, []byte(strings.Repeat("b", 40)))
		conn.expectClose(1009)
	})

	t.Run("a message at the limit is accepted", func(t *testing.T) {
		t.Parallel()
		conn := dialWS(t, server.URL, "/ws")
		conn.text(strings.Repeat("a", 64))
		conn.expectText(strings.Repeat("a", 64))
	})
}

func TestWebSocketWithoutAWriteTimeout(t *testing.T) {
	t.Parallel()
	// A negative duration disables the bound, as it does everywhere else. What
	// must not happen is the handshake being written under a deadline of
	// "now", which is what adding a disabled timeout to the clock would give.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{WriteTimeout: -1}))
	})
	conn := dialWS(t, server.URL, "/ws")
	conn.text("no deadline in sight")
	conn.expectText("no deadline in sight")
	conn.send(true, opClose, closePayload(1000, ""))
	conn.expectClose(1000)
}

func TestWebSocketCloseHandshake(t *testing.T) {
	t.Parallel()
	_, server := echoApp(t)

	t.Run("a close with no status is answered with none", func(t *testing.T) {
		t.Parallel()
		conn := dialWS(t, server.URL, "/ws")
		conn.send(true, opClose, nil)
		conn.expectClose(0)
		conn.expectEOF()
	})

	t.Run("a status is echoed back", func(t *testing.T) {
		t.Parallel()
		conn := dialWS(t, server.URL, "/ws")
		conn.send(true, opClose, closePayload(4001, "application code"))
		conn.expectClose(4001)
		conn.expectEOF()
	})

	t.Run("the server closes when the handler returns", func(t *testing.T) {
		t.Parallel()
		_, server := newWSTestApp(t, func(app *App) {
			app.WS("/hello", func(ctx *Context, _ Empty, conn *WSConn) error {
				return conn.WriteText(ctx.Context(), "goodbye")
			})
		})
		conn := dialWS(t, server.URL, "/hello")
		conn.expectText("goodbye")
		conn.expectClose(1000)
		conn.expectEOF()
	})
}

func TestWebSocketHandlerOutcomes(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	failure := errors.New("the database is on fire")

	app := New(AppOptions{Title: "Test API", Version: "1.0.0", Logger: logger})
	app.WS("/fails", func(*Context, Empty, *WSConn) error { return failure })
	app.WS("/rejects", func(*Context, Empty, *WSConn) error {
		return &WSCloseError{Status: WSStatusPolicyViolation, Reason: "not for you"}
	})
	app.WS("/panics", func(*Context, Empty, *WSConn) error { panic("handler exploded") })
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	t.Run("an error closes with an internal error and says nothing", func(t *testing.T) {
		conn := dialWS(t, server.URL, "/fails")
		reason := conn.expectClose(uint16(WSStatusInternalError))
		if strings.Contains(reason, "database") {
			t.Errorf("close reason = %q, want nothing about the failure", reason)
		}
		waitForLog(t, logs, "the database is on fire")
	})

	t.Run("a close error chooses the status", func(t *testing.T) {
		conn := dialWS(t, server.URL, "/rejects")
		if reason := conn.expectClose(uint16(WSStatusPolicyViolation)); reason != "not for you" {
			t.Errorf("close reason = %q, want %q", reason, "not for you")
		}
	})

	t.Run("a panic closes with an internal error and is recovered", func(t *testing.T) {
		conn := dialWS(t, server.URL, "/panics")
		conn.expectClose(uint16(WSStatusInternalError))
		conn.expectEOF()
		// The connection is closed before the panic reaches the recovery, so
		// the client sees the close first and the log arrives a moment later.
		waitForLog(t, logs, "handler exploded")
	})
}

func TestWebSocketLargeMessages(t *testing.T) {
	t.Parallel()
	// Larger than the scratch buffer in both directions, so that the write
	// path that sends a header and a payload separately is exercised along
	// with the read path that grows its buffer.
	payload := make([]byte, 5*wsScratchSize+17)
	for i := range payload {
		payload[i] = byte(i)
	}
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadLimit: 1 << 20}))
	})
	conn := dialWS(t, server.URL, "/ws")
	conn.send(true, opBinary, payload)
	fin, opcode, echoed := conn.recv()
	if !fin || opcode != opBinary {
		t.Fatalf("frame = (fin %v, opcode %#x), want a final binary frame", fin, opcode)
	}
	if string(echoed) != string(payload) {
		t.Errorf("the echoed payload of %d bytes does not match the %d sent", len(echoed), len(payload))
	}
}

func TestWebSocketConcurrentWritersDoNotInterleave(t *testing.T) {
	t.Parallel()
	const writers, each = 8, 25
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/fan", func(ctx *Context, _ Empty, conn *WSConn) error {
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					message := strings.Repeat(string(rune('a'+w)), 200)
					for range each {
						if err := conn.WriteText(ctx.Context(), message); err != nil {
							return
						}
					}
				}()
			}
			wg.Wait()
			return nil
		})
	})

	conn := dialWS(t, server.URL, "/fan")
	counts := map[string]int{}
	for range writers * each {
		fin, opcode, payload := conn.recv()
		if !fin || opcode != opText {
			t.Fatalf("frame = (fin %v, opcode %#x), want a final text frame", fin, opcode)
		}
		message := string(payload)
		if len(message) != 200 || strings.Count(message, message[:1]) != 200 {
			t.Fatalf("message %q is not one writer's own, so two writes interleaved", message)
		}
		counts[message[:1]]++
	}
	if len(counts) != writers {
		t.Errorf("messages arrived from %d writers, want %d", len(counts), writers)
	}
	for letter, count := range counts {
		if count != each {
			t.Errorf("writer %q sent %d messages, want %d", letter, count, each)
		}
	}
}

func TestWebSocketReadTypeMismatches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		handler WSHandler[Empty]
		send    func(*rawConn)
		status  uint16
	}{
		{
			name: "text expected but binary sent",
			path: "/text",
			handler: func(ctx *Context, _ Empty, conn *WSConn) error {
				_, err := conn.ReadText(ctx.Context())
				return err
			},
			send:   func(c *rawConn) { c.send(true, opBinary, []byte{1, 2, 3}) },
			status: uint16(WSStatusUnsupportedData),
		},
		{
			name: "binary expected but text sent",
			path: "/binary",
			handler: func(ctx *Context, _ Empty, conn *WSConn) error {
				_, err := conn.ReadBinary(ctx.Context())
				return err
			},
			send:   func(c *rawConn) { c.text("words") },
			status: uint16(WSStatusUnsupportedData),
		},
		{
			name: "json expected but binary sent",
			path: "/json-binary",
			handler: func(ctx *Context, _ Empty, conn *WSConn) error {
				var out map[string]string
				return conn.ReadJSON(ctx.Context(), &out)
			},
			send:   func(c *rawConn) { c.send(true, opBinary, []byte("{}")) },
			status: uint16(WSStatusUnsupportedData),
		},
		{
			name: "json expected but nonsense sent",
			path: "/json-bad",
			handler: func(ctx *Context, _ Empty, conn *WSConn) error {
				var out map[string]string
				return conn.ReadJSON(ctx.Context(), &out)
			},
			send:   func(c *rawConn) { c.text("{not json") },
			status: uint16(WSStatusInvalidFramePayload),
		},
	}
	_, server := newWSTestApp(t, func(app *App) {
		for _, tc := range cases {
			app.WS(tc.path, tc.handler)
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := dialWS(t, server.URL, tc.path)
			tc.send(conn)
			conn.expectClose(tc.status)
		})
	}
}

func TestWebSocketJSONRoundTrip(t *testing.T) {
	t.Parallel()
	type message struct {
		Room string `json:"room"`
		Text string `json:"text"`
	}
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/json", func(ctx *Context, _ Empty, conn *WSConn) error {
			var in message
			if err := conn.ReadJSON(ctx.Context(), &in); err != nil {
				return err
			}
			in.Text = strings.ToUpper(in.Text)
			return conn.WriteJSON(ctx.Context(), in)
		})
	})
	conn := dialWS(t, server.URL, "/json")
	conn.text(`{"room":"lobby","text":"hi"}`)
	conn.expectText(`{"room":"lobby","text":"HI"}`)
}

func TestWebSocketWriteRejectsWhatCannotBeSent(t *testing.T) {
	t.Parallel()
	reported := make(chan []error, 1)
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			reported <- []error{
				conn.Write(ctx.Context(), WSText, []byte{0xFF}),
				conn.WriteText(ctx.Context(), "\xff"),
				conn.Write(ctx.Context(), WSMessageType(9), nil),
				conn.WriteJSON(ctx.Context(), make(chan int)),
			}
			return nil
		})
	})
	conn := dialWS(t, server.URL, "/ws")
	errs := <-reported
	for i, err := range errs {
		if err == nil {
			t.Errorf("write %d succeeded, want a refusal", i)
		}
	}
	if !strings.Contains(errs[2].Error(), "unknown message type") {
		t.Errorf("error = %q, want it to name the message type", errs[2])
	}
	conn.expectClose(1000)
}

func TestWebSocketContextCancellationEndsTheConnection(t *testing.T) {
	t.Parallel()
	reported := make(chan error, 1)
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			cancellable, cancel := context.WithCancel(ctx.Context())
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			_, _, err := conn.Read(cancellable)
			reported <- err
			// A cancelled read leaves the stream at an unknown position, so
			// everything after it must refuse too.
			if _, _, second := conn.Read(ctx.Context()); second == nil {
				return errors.New("a read after cancellation succeeded")
			}
			return nil
		})
	})
	conn := dialWS(t, server.URL, "/ws")
	// The cancellation came between messages, so the peer is told the
	// connection is going away rather than finding it gone; it used to see
	// the end of the stream with no close frame before it.
	conn.expectClose(uint16(WSStatusGoingAway))
	err := <-reported
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want it to report the cancellation", err)
	}
	conn.expectEOF()
}

func TestWebSocketReadDeadlineFromContext(t *testing.T) {
	t.Parallel()
	reported := make(chan error, 1)
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			deadlined, cancel := context.WithTimeout(ctx.Context(), 20*time.Millisecond)
			defer cancel()
			_, _, err := conn.Read(deadlined)
			reported <- err
			return nil
		})
	})
	conn := dialWS(t, server.URL, "/ws")
	// A deadline on a read is how a handler bounds how long its peer may stay
	// silent, so the peer is told that is what happened, with the status the
	// connection's own read timeout uses, rather than finding the stream
	// ended with no close frame before it as it used to.
	conn.expectClose(uint16(WSStatusPolicyViolation))
	if err := <-reported; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read error = %v, want the deadline to be reported", err)
	}
	conn.expectEOF()
}

func TestWebSocketKeepaliveClosesASilentPeer(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
			PingInterval: 20 * time.Millisecond,
			PongTimeout:  40 * time.Millisecond,
		}))
	})
	conn := dialWS(t, server.URL, "/ws")

	// The first ping is answered, so the connection survives it.
	fin, opcode, payload := conn.recv()
	if !fin || opcode != opPing {
		t.Fatalf("frame = (fin %v, opcode %#x), want a keepalive ping", fin, opcode)
	}
	conn.send(true, opPong, payload)

	// The second is not, so the connection is closed for going quiet.
	for {
		_, opcode, _ := conn.recv()
		if opcode == opClose {
			break
		}
	}
}

func TestWebSocketSurvivesTheListenerWriteTimeout(t *testing.T) {
	t.Parallel()
	// A hijacked connection keeps whatever deadline the listener set for one
	// request, so a WebSocket that outlives the write timeout only works if
	// the upgrade cleared it.
	//
	// The handshake itself still has to be read and answered inside the
	// timeouts, which a loaded machine did not always manage in 40ms, so they
	// are longer and the quiet spell below is three of them.
	const timeout = 250 * time.Millisecond
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ReadTimeout = timeout
	opts.WriteTimeout = timeout
	app := New(opts)
	app.WS("/ws", wsEcho)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()
	addr := waitForAddr(t, app)

	conn := dialWS(t, "http://"+addr, "/ws")
	conn.text("first")
	conn.expectText("first")
	time.Sleep(3 * timeout)
	conn.text("still here")
	conn.expectText("still here")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunContext = %v", err)
	}
}

func TestWebSocketShutdownClosesOpenConnections(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts)
	handlerReading := make(chan struct{})
	handlerReturned := make(chan struct{})
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		defer close(handlerReturned)
		for first := true; ; first = false {
			if _, _, err := conn.Read(ctx.Context()); err != nil {
				return nil
			}
			if first {
				// Shutting down before the handler runs is a different path,
				// covered by the register's own test, so the signal here keeps
				// this one on the path it means to exercise.
				close(handlerReading)
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()
	addr := waitForAddr(t, app)

	conn := dialWS(t, "http://"+addr, "/ws")
	conn.text("still talking")
	<-handlerReading

	cancel()
	if reason := conn.expectClose(uint16(WSStatusGoingAway)); !strings.Contains(reason, "shutting down") {
		t.Errorf("close reason = %q, want it to say the server is shutting down", reason)
	}
	select {
	case <-handlerReturned:
	case <-time.After(wsTestTimeout):
		t.Fatal("the handler did not return once its connection was closed")
	}
	if err := <-done; err != nil {
		t.Fatalf("RunContext = %v", err)
	}
}

func TestWebSocketRefusesAHandshakeWhileShuttingDown(t *testing.T) {
	t.Parallel()
	var registry liveRegistry[*WSConn]
	// A timeout of zero falls back to the default rather than giving up at
	// once, which is what an application that disabled its own would get.
	if closed := registry.shutdown(0, wsCloseGoingAway); closed != 0 {
		t.Errorf("shutdown of an empty register closed %d connections, want 0", closed)
	}
	if registry.add(&WSConn{}, "") != registryDraining {
		t.Error("a connection was accepted after the register began draining")
	}
	// Forgetting a connection that was never tracked must not unbalance the
	// wait group, which a second shutdown would then hang on.
	registry.remove(&WSConn{})
	if closed := registry.shutdown(time.Second, wsCloseGoingAway); closed != 0 {
		t.Errorf("the second shutdown closed %d connections, want 0", closed)
	}
}

func TestWebSocketNoGoroutineLeaks(t *testing.T) {
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho)
		app.WS("/keepalive", wsEcho, WithWebSocket(WSOptions{PingInterval: 10 * time.Millisecond}))
	})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dialWS(t, server.URL, "/ws")
			conn.text("hello")
			conn.expectText("hello")
			conn.send(true, opClose, closePayload(1000, ""))
			conn.expectClose(1000)
		}()
	}
	wg.Wait()

	conn := dialWS(t, server.URL, "/keepalive")
	fin, opcode, payload := conn.recv()
	if !fin || opcode != opPing {
		t.Fatalf("frame = (fin %v, opcode %#x), want a keepalive ping", fin, opcode)
	}
	conn.send(true, opPong, payload)
	conn.send(true, opClose, closePayload(1000, ""))
	conn.expectClose(1000)

	server.Close()
	assertNoGoroutineLeaks(t)
}
