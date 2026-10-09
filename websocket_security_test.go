package muzak

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file are attacks rather than exercises. Each one plays the
// part of a hostile peer and asserts that the server ends the conversation on
// its own terms: nothing unbounded is allocated, nothing is disclosed, nothing
// is left running, and the connection is closed with the status the abuse
// calls for.

func TestWebSocketRefusesAFragmentationBomb(t *testing.T) {
	t.Parallel()
	// A message fragmented into empty continuation frames never grows, so the
	// read limit is never reached and a read would wait for as long as the
	// peer cared to keep sending. The frame count is what ends it.
	_, server := echoApp(t)
	conn := dialWS(t, server.URL, "/ws")

	var flood []byte
	key := []byte{1, 2, 3, 4}
	flood = append(flood, frameHeader(false, opText, 0, key)...)
	for range wsMaxFramesPerMessage {
		flood = append(flood, frameHeader(false, opContinuation, 0, key)...)
	}
	conn.sendRaw(flood)

	if reason := conn.expectClose(uint16(WSStatusPolicyViolation)); !strings.Contains(reason, "too many frames") {
		t.Errorf("close reason = %q, want it to name the abuse", reason)
	}
	conn.expectEOF()
}

// wsOverflowingContinuation is the second half of an attack on the read limit:
// a continuation frame, masked with a key of zeroes so that its payload would
// go through unchanged, declaring the largest length the wire format allows.
// Added to a single byte already received, that length wraps a signed sum
// negative.
func wsOverflowingContinuation(masked bool) []byte {
	frame := []byte{0x80 | opContinuation, 127}
	if masked {
		frame[1] |= 0x80
	}
	frame = binary.BigEndian.AppendUint64(frame, math.MaxInt64)
	if masked {
		frame = append(frame, 0, 0, 0, 0)
	}
	return frame
}

func TestWebSocketRefusesALengthThatOverflowsTheReadLimit(t *testing.T) {
	t.Parallel()
	// One byte of a message, then a continuation declaring the largest length
	// there is. A limit check that adds the two overflows, passes, and leaves
	// the server buffering whatever the peer cares to stream, far past the
	// limit, for as long as the read timeout allows. The check has to refuse
	// the frame from its header alone, before a byte of its payload is sent.
	reported := make(chan error, 1)
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			_, _, err := conn.Read(ctx.Context())
			reported <- err
			return nil
		})
	})
	conn := dialWS(t, server.URL, "/ws")

	conn.sendRaw(append(frameHeader(false, opBinary, 1, []byte{0, 0, 0, 0}), 'A'))
	conn.sendRaw(wsOverflowingContinuation(true))

	start := time.Now()
	if reason := conn.expectClose(uint16(WSStatusMessageTooBig)); !strings.Contains(reason, "byte limit") {
		t.Errorf("close reason = %q, want it to name the limit", reason)
	}
	if waited := time.Since(start); waited > wsTestTimeout/2 {
		t.Errorf("the refusal took %v, want it to come from the header alone", waited)
	}
	select {
	case err := <-reported:
		if status, ok := WSCloseStatus(err); !ok || status != WSStatusMessageTooBig {
			t.Errorf("read error = %v, want the message refused as too big", err)
		}
	case <-time.After(wsTestTimeout):
		t.Fatal("the handler's read did not end")
	}
	conn.expectEOF()
}

func TestWebSocketRefusesAPingFlood(t *testing.T) {
	t.Parallel()
	// Every ping is answered, so an endless run of them keeps a read from ever
	// returning while the handler waits for a message that is never coming.
	_, server := echoApp(t)
	conn := dialWS(t, server.URL, "/ws")

	// The answers are drained while the flood is sent, because a peer that
	// stopped reading would be a different attack.
	sending := make(chan struct{})
	go func() {
		defer close(sending)
		var flood []byte
		key := []byte{1, 2, 3, 4}
		for range wsMaxFramesPerMessage + 1 {
			flood = append(flood, frameHeader(true, opPing, 0, key)...)
		}
		// The server closes part way through, so a refused write is the
		// expected end of this rather than a failure.
		_ = conn.conn.SetWriteDeadline(time.Now().Add(wsTestTimeout))
		_, _ = conn.conn.Write(flood)
	}()

	for range wsMaxFramesPerMessage + 2 {
		_, opcode, payload := conn.recv()
		if opcode == opClose {
			if !strings.Contains(string(payload), "too many frames") {
				t.Errorf("close payload = %q, want it to name the abuse", payload)
			}
			<-sending
			return
		}
		if opcode != opPong {
			t.Fatalf("opcode %#x arrived where only pongs and a close should be", opcode)
		}
	}
	t.Fatal("the flood was answered to the end without the connection being closed")
}

func TestWebSocketDoesNotAllocateWhatIsMerelyDeclared(t *testing.T) {
	// This test is not parallel: it measures the whole process's live heap, so
	// anything else running at the same time is counted as growth here.
	// A frame header is six bytes and can declare a payload the size of the
	// whole read limit. Committing that memory before the bytes arrive would
	// make a handful of connections the cheapest denial of service there is.
	const limit = 32 << 20
	const connections = 8
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadLimit: limit}))
	})

	before := liveHeap()

	for range connections {
		conn := dialWS(t, server.URL, "/ws")
		// The header promises the whole limit; one byte of it is sent, so the
		// server is left waiting for the rest.
		conn.sendRaw(append(frameHeader(true, opBinary, limit, []byte{1, 2, 3, 4}), 0))
	}
	// Give every server-side read a moment to reach the point where it is
	// waiting for bytes that will never come.
	time.Sleep(100 * time.Millisecond)

	after := liveHeap()

	// Committing the declared length would be 256 MiB here. Committing a chunk
	// at a time is a few hundred kilobytes, so anything under a few megabytes
	// shows the promise was not taken at face value.
	const tolerated = 16 << 20
	if grown := after - before; grown > tolerated {
		t.Errorf("the heap grew by %d bytes for %d connections that sent one byte each, want less than %d",
			grown, connections, tolerated)
	}
}

func TestWebSocketRefusesADribbledMessage(t *testing.T) {
	t.Parallel()
	// A peer that sends a message one byte at a time, slowly enough, holds a
	// goroutine for as long as it likes unless a message is given a deadline
	// of its own once it has begun.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadTimeout: 100 * time.Millisecond}))
	})
	conn := dialWS(t, server.URL, "/ws")

	conn.sendRaw(append(frameHeader(true, opText, 64, []byte{1, 2, 3, 4}), 'a'^1))
	start := time.Now()
	if reason := conn.expectClose(uint16(WSStatusPolicyViolation)); !strings.Contains(reason, "did not arrive") {
		t.Errorf("close reason = %q, want it to say the message never arrived", reason)
	}
	if waited := time.Since(start); waited > wsTestTimeout/2 {
		t.Errorf("the server waited %v, want it bounded by the read timeout", waited)
	}
	conn.expectEOF()
}

func TestWebSocketWaitsIndefinitelyBetweenMessages(t *testing.T) {
	t.Parallel()
	// The message deadline must not become an idle deadline: waiting is what
	// most connections are for, and a connection that has said nothing for a
	// while is not misbehaving.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadTimeout: 50 * time.Millisecond}))
	})
	conn := dialWS(t, server.URL, "/ws")
	conn.text("first")
	conn.expectText("first")
	time.Sleep(200 * time.Millisecond)
	conn.text("still welcome")
	conn.expectText("still welcome")
}

func TestWebSocketMessageClockNeverWidensACallersDeadline(t *testing.T) {
	t.Parallel()
	// The time a message is given once it has begun must not extend a deadline
	// the caller set for itself, or a handler asking for a quick answer would
	// wait for the connection's own bound instead.
	reported := make(chan error, 1)
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			deadlined, cancel := context.WithTimeout(ctx.Context(), 50*time.Millisecond)
			defer cancel()
			_, _, err := conn.Read(deadlined)
			reported <- err
			return nil
		}, WithWebSocket(WSOptions{ReadTimeout: time.Hour}))
	})
	conn := dialWS(t, server.URL, "/ws")

	// The message begins and stops, so only a deadline can end the read.
	conn.sendRaw(append(frameHeader(true, opText, 64, []byte{1, 2, 3, 4}), 'a'^1))
	start := time.Now()
	select {
	case err := <-reported:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read error = %v, want the caller's own deadline", err)
		}
	case <-time.After(wsTestTimeout):
		t.Fatal("the read outlived the deadline the caller set for it")
	}
	if waited := time.Since(start); waited > wsTestTimeout/2 {
		t.Errorf("the read took %v, want it bounded by the caller's deadline", waited)
	}
}

func TestWebSocketRefusesAHandshakeCarryingABody(t *testing.T) {
	t.Parallel()
	// Whatever a handshake's body left unread would sit on the connection and
	// be taken for frames the moment it was upgraded, which is a way of
	// writing a peer's first messages for it.
	_, server := echoApp(t)
	address := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		t.Fatalf("setting a deadline: %v", err)
	}

	smuggled := "\x81\x85\x00\x00\x00\x00pwned"
	request := "GET /ws HTTP/1.1\r\nHost: " + address + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testWSKey + "\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n", len(smuggled)) + smuggled
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("sending the handshake: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<12))
	if !strings.Contains(string(body), "cannot carry a request body") {
		t.Errorf("body = %s, want it to say why", body)
	}
}

func TestWebSocketRefusesARepeatedHandshakeHeader(t *testing.T) {
	t.Parallel()
	_, server := echoApp(t)
	for _, header := range []string{"Sec-WebSocket-Key", "Sec-WebSocket-Version"} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			value := testWSKey
			if header == "Sec-WebSocket-Version" {
				value = "13"
			}
			// The second copy is sent by dialRaw's own header, so this one
			// makes the pair.
			_, response := dialRaw(t, server.URL, "/ws", header, value, header, value)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestWebSocketAcceptKeyRefusesAnOversizedKeyBeforeDecodingIt(t *testing.T) {
	// This test is not parallel: it measures what the whole process allocates,
	// so anything else running at the same time would be counted here.
	//
	// A key is sixteen bytes of base64, which is twenty four characters, so its
	// length alone says whether a header could be one. Decoding first would let
	// anyone who reaches the route, which on a public route is anyone at all,
	// buy an allocation three quarters the size of whatever key it sent, up to
	// the server's header limit, on every handshake it attempts.
	key := strings.Repeat("A", 512<<10)
	const attempts = 8

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range attempts {
		if _, err := wsAcceptKey(key); err == nil {
			t.Fatal("a key of half a mebibyte was accepted")
		}
	}
	runtime.ReadMemStats(&after)

	// Decoding the key would cost some three megabytes across the attempts.
	// Refusing it on its length costs the error and nothing else.
	const tolerated = 256 << 10
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > tolerated {
		t.Errorf("refusing %d oversized keys allocated %d bytes, want less than %d", attempts, allocated, tolerated)
	}
}

func TestWebSocketCrossSiteHijackingIsRefused(t *testing.T) {
	t.Parallel()
	// A WebSocket handshake is not subject to the same-origin policy and is
	// never preflighted, so a page anywhere on the internet can make a
	// visitor's browser open one, cookies and all. This is the check that
	// stops it, and it has to happen before the handler runs.
	var reached atomic.Bool
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			reached.Store(true)
			return nil
		})
	})

	_, response := dialRaw(t, server.URL, "/ws",
		"Origin", "https://evil.example",
		"Cookie", "session=the-victims-session")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if reached.Load() {
		t.Error("the handler ran for a handshake that was supposed to be refused")
	}
	if got := response.Header.Get("Upgrade"); got != "" {
		t.Errorf("Upgrade = %q, want nothing that suggests the connection was taken", got)
	}
}

func TestWebSocketOriginCannotBeTalkedAround(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
			AllowedOrigins: []string{"https://app.example.com"},
		}))
	})
	host := strings.TrimPrefix(server.URL, "http://")

	// Every one of these is a shape that has fooled an origin check somewhere.
	for _, origin := range []string{
		"https://app.example.com.evil.example",
		"https://evil.example/https://app.example.com",
		"https://app.example.com:9999@evil.example",
		"https://evil.example#https://app.example.com",
		"https://evil.example?https://app.example.com",
		"null",
		"",
		"https://" + host + ".evil.example",
		"https://evil.example/" + host,
		"http://evil.example\t",
	} {
		t.Run(origin, func(t *testing.T) {
			t.Parallel()
			if origin == "" {
				// An empty header is not the same as no header, and must not
				// be read as one that matched.
				return
			}
			_, response := dialRaw(t, server.URL, "/ws", "Origin", origin)
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("origin %q got status %d, want it refused with %d", origin, response.StatusCode, http.StatusForbidden)
			}
		})
	}
}

func TestWebSocketDisclosesNothingToARefusedPeer(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.WS("/ws", func(*Context, Empty, *WSConn) error {
		return fmt.Errorf("dial tcp 10.0.0.7:5432: connect: connection refused, while running SELECT * FROM secrets")
	})
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	conn := dialWS(t, server.URL, "/ws")
	reason := conn.expectClose(uint16(WSStatusInternalError))
	for _, secret := range []string{"10.0.0.7", "5432", "SELECT", "secrets", "muzak.", ".go:"} {
		if strings.Contains(reason, secret) {
			t.Errorf("the close reason %q leaks %q", reason, secret)
		}
	}
	waitForLog(t, logs, "10.0.0.7")
}

func TestWebSocketRefusesAnOversizedHandshakeValue(t *testing.T) {
	t.Parallel()
	_, server := echoApp(t)
	// A header the size of the header limit must not become an error body the
	// size of the header limit.
	huge := "https://" + strings.Repeat("a", 60<<10) + ".example"
	_, response := dialRaw(t, server.URL, "/ws", "Origin", huge)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if len(body) > 1<<10 {
		t.Errorf("the error body is %d bytes, want the client's own value cut down before it is echoed", len(body))
	}
}

func TestWebSocketNeverEchoesAClientSubprotocol(t *testing.T) {
	t.Parallel()
	// Only what the route offered can be answered with, so a client cannot
	// choose the bytes that go into a response header.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{Subprotocols: []string{"chat.v1"}}))
	})
	_, response := dialRaw(t, server.URL, "/ws",
		"Sec-WebSocket-Protocol", "evil\r\nX-Injected: yes")
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	if got := response.Header.Get("Sec-WebSocket-Protocol"); got != "" {
		t.Errorf("Sec-WebSocket-Protocol = %q, want nothing echoed", got)
	}
	if got := response.Header.Get("X-Injected"); got != "" {
		t.Errorf("X-Injected = %q, want the header block to have survived intact", got)
	}
}

func TestWebSocketRefusesASubprotocolThatIsNotAToken(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"evil\r\nX-Injected: yes", "with space", "", "quote\"d"} {
		app := New(quietOptions())
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{Subprotocols: []string{name}}))
		if got := buildError(t, app); !strings.Contains(got, "not a valid token") {
			t.Errorf("build error for %q = %q, want it refused as a token", name, got)
		}
	}
	app := New(quietOptions())
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{Subprotocols: []string{"chat.v1", "graphql-transport-ws"}}))
	mustBuild(t, app)
}

func TestWebSocketConnectionLimit(t *testing.T) {
	t.Parallel()
	// An unbounded number of connections is an unbounded number of file
	// descriptors, goroutines and buffers, all of them held by whoever asks.
	release := make(chan struct{})
	opts := quietOptions()
	opts.WebSocket = WSOptions{MaxConnections: 2}
	app := New(opts)
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		<-release
		return nil
	})
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	for i := range 2 {
		if conn, response := dialRaw(t, server.URL, "/ws"); response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("connection %d got status %d, want it accepted", i, response.StatusCode)
		} else {
			_ = conn
		}
	}
	_, refused := dialRaw(t, server.URL, "/ws")
	if refused.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d once the limit is reached", refused.StatusCode, http.StatusServiceUnavailable)
	}
	if got := refused.Header.Get("Retry-After"); got == "" {
		t.Error("no Retry-After was offered, so a client has nothing to go on")
	}
}

func TestWebSocketConnectionLimitBelongsToTheApplication(t *testing.T) {
	t.Parallel()
	// A route cannot raise or lower it, because the resource it bounds is the
	// process. Saying so at build time beats a limit that quietly means
	// something different depending on which route was reached.
	app := New(quietOptions())
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{MaxConnections: 10}))
	if got := buildError(t, app); !strings.Contains(got, "only be set on the application") {
		t.Errorf("build error = %q, want it to say where the limit belongs", got)
	}

	router := NewRouter()
	router.WS("/ws", wsEcho)
	viaRouter := New(quietOptions())
	viaRouter.Include(router, WithWebSocket(WSOptions{MaxConnections: 10}))
	if got := buildError(t, viaRouter); !strings.Contains(got, "only be set on the application") {
		t.Errorf("build error = %q, want a router to be refused too", got)
	}

	unlimited := New(quietOptions())
	unlimited.opts.WebSocket.MaxConnections = -1
	unlimited.WS("/ws", wsEcho)
	mustBuild(t, unlimited)
	if unlimited.websockets.limit != 0 {
		t.Errorf("limit = %d, want a negative setting to remove it", unlimited.websockets.limit)
	}
}

func TestWSConnectionLimitResolution(t *testing.T) {
	t.Parallel()
	for configured, want := range map[int]int{0: DefaultWSMaxConnections, -1: 0, 7: 7} {
		if got := wsConnectionLimit(configured); got != want {
			t.Errorf("wsConnectionLimit(%d) = %d, want %d", configured, got, want)
		}
	}
}

func TestWebSocketConnectionLimitPerIP(t *testing.T) {
	t.Parallel()
	// A WebSocket handshake needs no Origin header to succeed, so a single
	// unauthenticated, non-browser client can open connections in a tight
	// loop; MaxConnections alone bounds the process, not what one client can
	// take from it. This is the test for the dimension that does.
	release := make(chan struct{})
	opts := quietOptions()
	opts.WebSocket = WSOptions{MaxConnections: 10, MaxConnectionsPerIP: 1}
	opts.ClientIP = ClientIPOptions{TrustedProxies: []string{"127.0.0.1/32"}}
	app := New(opts)
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		<-release
		return nil
	})
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	if _, response := dialRaw(t, server.URL, "/ws", "X-Forwarded-For", "203.0.113.9"); response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the first connection got status %d, want it accepted", response.StatusCode)
	}

	// A second connection from the same address is refused even though the
	// process-wide MaxConnections has plenty of room left.
	_, refused := dialRaw(t, server.URL, "/ws", "X-Forwarded-For", "203.0.113.9")
	if refused.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a second connection from the same address got status %d, want %d", refused.StatusCode, http.StatusServiceUnavailable)
	}
	if got := refused.Header.Get("Retry-After"); got == "" {
		t.Error("no Retry-After was offered, so a client has nothing to go on")
	}

	// A different address is unaffected: the limit is per client, not global.
	if _, response := dialRaw(t, server.URL, "/ws", "X-Forwarded-For", "198.51.100.4"); response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("a connection from a different address got status %d, want it accepted", response.StatusCode)
	}
}

func TestWebSocketConnectionLimitPerIPBelongsToTheApplication(t *testing.T) {
	t.Parallel()
	// Like MaxConnections, the dimension this protects is a client's share of
	// the process, which a route cannot narrow or widen for itself.
	app := New(quietOptions())
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{MaxConnectionsPerIP: 10}))
	if got := buildError(t, app); !strings.Contains(got, "MaxConnectionsPerIP may only be set on the application") {
		t.Errorf("build error = %q, want it to say where the limit belongs", got)
	}

	router := NewRouter()
	router.WS("/ws", wsEcho)
	viaRouter := New(quietOptions())
	viaRouter.Include(router, WithWebSocket(WSOptions{MaxConnectionsPerIP: 10}))
	if got := buildError(t, viaRouter); !strings.Contains(got, "MaxConnectionsPerIP may only be set on the application") {
		t.Errorf("build error = %q, want a router to be refused too", got)
	}

	unlimited := New(quietOptions())
	unlimited.opts.WebSocket.MaxConnectionsPerIP = -1
	unlimited.WS("/ws", wsEcho)
	mustBuild(t, unlimited)
	if unlimited.websockets.perKeyLimit != 0 {
		t.Errorf("perKeyLimit = %d, want a negative setting to remove it", unlimited.websockets.perKeyLimit)
	}
}

func TestWSConnectionsPerIPLimitResolution(t *testing.T) {
	t.Parallel()
	for configured, want := range map[int]int{0: DefaultWSMaxConnectionsPerIP, -1: 0, 7: 7} {
		if got := wsConnectionsPerIPLimit(configured); got != want {
			t.Errorf("wsConnectionsPerIPLimit(%d) = %d, want %d", configured, got, want)
		}
	}
}

func TestWebSocketSurvivesAnAttackStorm(t *testing.T) {
	// Not parallel: the goroutine leak profile and the register are checked at
	// the end, and both are about the whole process.
	var handlers atomic.Int64
	app := New(quietOptions())
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		handlers.Add(1)
		defer handlers.Add(-1)
		for {
			typ, payload, err := conn.Read(ctx.Context())
			if err != nil {
				return nil
			}
			if err := conn.Write(ctx.Context(), typ, payload); err != nil {
				return err
			}
		}
	}, WithWebSocket(WSOptions{ReadLimit: 4 << 10, ReadTimeout: 100 * time.Millisecond}))
	mustBuild(t, app)
	server := httptest.NewServer(app)

	key := []byte{1, 2, 3, 4}
	attacks := []func() []byte{
		// A frame that is not allowed to exist at all.
		func() []byte { return []byte{0xC1, 0x80, 1, 2, 3, 4} },
		// A control frame too large to be one.
		func() []byte { return frameHeader(true, opPing, 126, key) },
		// A length that promises far more than the limit allows.
		func() []byte { return frameHeader(true, opBinary, 1<<40, key) },
		// A continuation with nothing to continue.
		func() []byte { return frameHeader(true, opContinuation, 0, key) },
		// Text that is not text.
		func() []byte { return append(frameHeader(true, opText, 2, key), 0xFF^1, 0xFE^2) },
		// A close code that must never be sent.
		func() []byte { return append(frameHeader(true, opClose, 2, key), 0x03^1, 0xEE^2) },
		// A frame from a client that forgot to mask.
		func() []byte { return frameHeader(true, opText, 0, nil) },
		// A header that promises a payload and then stops.
		func() []byte { return frameHeader(true, opBinary, 4<<10, key) },
		// Nothing at all, then a disconnection.
		func() []byte { return nil },
	}

	var wg sync.WaitGroup
	for round := range 6 {
		for i, attack := range attacks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				address := strings.TrimPrefix(server.URL, "http://")
				conn, err := net.Dial("tcp", address)
				if err != nil {
					t.Errorf("round %d attack %d: dialling: %v", round, i, err)
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(wsTestTimeout))
				handshake := "GET /ws HTTP/1.1\r\nHost: " + address + "\r\n" +
					"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
					"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + testWSKey + "\r\n\r\n"
				if _, err := conn.Write(append([]byte(handshake), attack()...)); err != nil {
					return
				}
				// Whatever comes back is read to the end, which is where the
				// server closing rather than hanging shows up.
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}
	wg.Wait()

	deadline := time.Now().Add(wsTestTimeout)
	for time.Now().Before(deadline) && handlers.Load() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if left := handlers.Load(); left != 0 {
		t.Errorf("%d handlers are still running after every attacker disconnected", left)
	}
	// A connection keeps its place in the register until it has been closed,
	// which comes a moment after its handler has returned, so the register is
	// waited on rather than read at once.
	settled := time.Now().Add(wsTestTimeout)
	for time.Now().Before(settled) && app.websockets.count() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if tracked := app.websockets.count(); tracked != 0 {
		t.Errorf("the register still holds %d connections, so a shutdown would wait on them", tracked)
	}

	server.Close()
	assertNoGoroutineLeaks(t)
}

func TestWebSocketKeepsConnectionsApart(t *testing.T) {
	t.Parallel()
	// Buffers are per connection and messages are handed to the caller rather
	// than lent, so no amount of concurrency can let one peer read another's
	// bytes.
	const peers, messages = 12, 40
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadLimit: 1 << 20}))
	})

	var wg sync.WaitGroup
	for peer := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dialWS(t, server.URL, "/ws")
			mine := strings.Repeat(string(rune('a'+peer)), 200+peer)
			for range messages {
				conn.text(mine)
				conn.expectText(mine)
			}
		}()
	}
	wg.Wait()
}

func TestWebSocketWithstandsConcurrentUse(t *testing.T) {
	t.Parallel()
	// Reads, writes, pings and closes all at once, which is what a handler
	// fanning a broadcast out across goroutines looks like when a peer goes
	// away in the middle of it.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 50 {
						_ = conn.WriteText(ctx.Context(), "broadcast")
						_ = conn.Ping(ctx.Context())
					}
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					if _, _, err := conn.Read(ctx.Context()); err != nil {
						return
					}
				}
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(20 * time.Millisecond)
				_ = conn.Close(WSStatusNormalClosure, "enough")
			}()
			wg.Wait()
			return nil
		})
	})

	conn := dialWS(t, server.URL, "/ws")
	for {
		_, opcode, _, err := conn.tryRecv()
		if err != nil {
			return
		}
		if opcode == opClose {
			return
		}
	}
}

func TestWebSocketJSONBombIsRefused(t *testing.T) {
	t.Parallel()
	// Deeply nested JSON is the classic way to turn a small message into a
	// large amount of work. It has to come back as an error rather than as a
	// stack overflow.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			var into any
			return conn.ReadJSON(ctx.Context(), &into)
		}, WithWebSocket(WSOptions{ReadLimit: 1 << 20}))
	})
	conn := dialWS(t, server.URL, "/ws")
	conn.text(strings.Repeat("[", 100000) + strings.Repeat("]", 100000))
	conn.expectClose(uint16(WSStatusInvalidFramePayload))
	conn.expectEOF()
}

func TestWebSocketHandshakeSlowlorisIsBounded(t *testing.T) {
	t.Parallel()
	// A handshake that never finishes arriving is the listener's problem
	// rather than the engine's, and this is the check that the application
	// hands it one that has a bound.
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	opts.ReadHeaderTimeout = 100 * time.Millisecond
	app := New(opts)
	app.WS("/ws", wsEcho)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()
	address := waitForAddr(t, app)

	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		t.Fatalf("setting a deadline: %v", err)
	}
	// The request begins and never ends.
	if _, err := conn.Write([]byte("GET /ws HTTP/1.1\r\nHost: " + address + "\r\n")); err != nil {
		t.Fatalf("sending the beginning of a handshake: %v", err)
	}
	if _, err := io.ReadAll(conn); err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("reading what came back: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunContext = %v", err)
	}
}

func TestWebSocketReadingManyMessagesDoesNotGrow(t *testing.T) {
	// Not parallel, for the reason given on the other test that weighs the
	// heap: the reading is process-wide.
	// Nothing a message passes through is retained, so a long conversation
	// costs what one message costs rather than what all of them do.
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{ReadLimit: 1 << 20}))
	})
	conn := dialWS(t, server.URL, "/ws")
	message := strings.Repeat("x", 8<<10)

	for range 50 {
		conn.text(message)
		conn.expectText(message)
	}
	before := liveHeap()

	for range 500 {
		conn.text(message)
		conn.expectText(message)
	}
	after := liveHeap()

	// Retaining every message would be four megabytes here.
	const tolerated = 1 << 20
	if grown := after - before; grown > tolerated {
		t.Errorf("the heap grew by %d bytes over 500 messages, want less than %d", grown, tolerated)
	}
}

// liveHeap reports how many bytes are still reachable, in signed bytes because
// a heap that shrank must not read as one that grew enormously.
//
// The collector is run more than once because one cycle leaves the previous
// cycle's garbage to be swept, and a reading taken then measures what has not
// been tidied away rather than what is being held.
func liveHeap() int64 {
	for range 3 {
		runtime.GC()
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}

// FuzzWebSocketHandshake throws arbitrary headers at the handshake checks.
//
// The handshake is the only part of a WebSocket an unauthenticated stranger
// reaches before anything else has run, so what is asserted here is that no
// combination of headers can panic it, and that one it accepts really did ask
// for a WebSocket in the way the specification requires.
func FuzzWebSocketHandshake(f *testing.F) {
	f.Add("Upgrade", "websocket", "13", testWSKey, "")
	f.Add("upgrade, keep-alive", "WebSocket", "13", testWSKey, "https://app.example.com")
	f.Add("", "", "", "", "")
	f.Add("Upgrade", "websocket", "13", "short", "null")
	f.Add("Upgrade", "websocket", "8", testWSKey, "\x00\r\n")

	cfg := &wsConfig{opts: WSOptions{AllowedOrigins: []string{"https://app.example.com"}}.withDefaults()}
	cfg.allowOrigin = wsOriginPolicy(cfg.opts)

	f.Fuzz(func(t *testing.T, connection, upgrade, version, key, origin string) {
		request := httptest.NewRequest("GET", "/ws", nil)
		for name, value := range map[string]string{
			"Connection":            connection,
			"Upgrade":               upgrade,
			"Sec-WebSocket-Version": version,
			"Sec-WebSocket-Key":     key,
			"Origin":                origin,
		} {
			if value == "" {
				continue
			}
			// A header value cannot carry a line break, and net/http would
			// never hand one over, so neither does this.
			request.Header.Set(name, strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
		}
		ctx := &Context{w: asResponseWriter(httptest.NewRecorder()), r: request}

		accept, err := cfg.checkHandshake(ctx)
		if err != nil {
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("checkHandshake returned %T, want an *HTTPError a client can be answered with", err)
			}
			if httpErr.Status < 400 || httpErr.Status > 499 {
				t.Errorf("checkHandshake refused with status %d, want one that blames the request", httpErr.Status)
			}
			return
		}
		// Whatever was accepted really did ask for a WebSocket.
		if !headerHasToken(request.Header, "Connection", "upgrade") ||
			!headerHasToken(request.Header, "Upgrade", "websocket") ||
			request.Header.Get("Sec-WebSocket-Version") != "13" {
			t.Fatal("checkHandshake accepted a request that did not ask to upgrade to websocket 13")
		}
		if len(accept) != 28 {
			t.Errorf("the accept value is %d characters, want the 28 a SHA-1 digest encodes to", len(accept))
		}
		if strings.ContainsAny(accept, "\r\n") {
			t.Error("the accept value could break the header block it is written into")
		}
	})
}
