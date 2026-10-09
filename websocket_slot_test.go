package muzak

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openCountListener counts the connections it accepted that have not been
// closed yet, which is the number of file descriptors a server is holding
// regardless of what its own bookkeeping says. It also counts how many it
// accepted in all, so that a connection already closed can be told from one
// not yet accepted.
type openCountListener struct {
	net.Listener
	open     atomic.Int64
	accepted atomic.Int64
}

type openCountConn struct {
	net.Conn
	once sync.Once
	l    *openCountListener
}

func (l *openCountListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.open.Add(1)
	l.accepted.Add(1)
	return &openCountConn{Conn: c, l: l}, nil
}

func (c *openCountConn) Close() error {
	c.once.Do(func() { c.l.open.Add(-1) })
	return c.Conn.Close()
}

// startCountedServer serves app over a listener that counts open sockets.
func startCountedServer(t *testing.T, app *App) (*httptest.Server, *openCountListener) {
	t.Helper()
	server := httptest.NewUnstartedServer(app)
	counted := &openCountListener{Listener: server.Listener}
	server.Listener = counted
	server.Start()
	t.Cleanup(server.Close)
	return server, counted
}

func TestWebSocketSlotIsHeldUntilTheSocketCloses(t *testing.T) {
	t.Parallel()
	// A handler that returns while its peer is healthy leaves Close waiting
	// for the peer's own close frame, and a hostile peer never sends one. If
	// the connection's slot were freed when the handler returned, the socket
	// would outlive it by the whole grace period, and a peer opening
	// connections as fast as it can would hold far more sockets than the cap
	// says one address may.
	opts := quietOptions()
	opts.WebSocket = WSOptions{MaxConnections: 4, MaxConnectionsPerIP: 4}
	app := New(opts)
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		_, _, _ = conn.Read(ctx.Context())
		return nil
	})
	mustBuild(t, app)
	server, counted := startCountedServer(t, app)

	var peak int64
	for range 100 {
		conn, response := dialRaw(t, server.URL, "/ws")
		if response.StatusCode != http.StatusSwitchingProtocols {
			_ = conn.conn.Close()
			time.Sleep(time.Millisecond)
			continue
		}
		// The handler returns, and the server waits for a close frame that
		// this peer never sends.
		conn.send(true, opText, []byte("bye"))
		peak = max(peak, counted.open.Load())
	}
	if peak > 5 {
		t.Errorf("at most 4 connections may be open, and %d sockets were held at once", peak)
	}
}

func TestWebSocketSlotIsHeldWhileACloseWaitsForAStalledWriter(t *testing.T) {
	t.Parallel()
	// The other way a close lingers: a broadcast goroutine is stuck writing to
	// a peer that does not read, so the close frame waits for the write half
	// for as long as the write timeout allows.
	opts := quietOptions()
	opts.WebSocket = WSOptions{MaxConnections: 3, MaxConnectionsPerIP: 3}
	app := New(opts)
	big := make([]byte, 4<<20)
	app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
		go func() {
			for conn.WriteBinary(ctx.Context(), big) == nil {
			}
		}()
		_, _, _ = conn.Read(ctx.Context())
		return nil
	}, WithWebSocket(WSOptions{WriteTimeout: 2 * time.Second}))
	mustBuild(t, app)
	server, counted := startCountedServer(t, app)

	var peak int64
	for range 20 {
		conn, response := dialRaw(t, server.URL, "/ws")
		if response.StatusCode != http.StatusSwitchingProtocols {
			_ = conn.conn.Close()
			continue
		}
		conn.send(true, opText, []byte("bye"))
		time.Sleep(5 * time.Millisecond)
		peak = max(peak, counted.open.Load())
	}
	if peak > 4 {
		t.Errorf("at most 3 connections may be open, and %d sockets were held at once", peak)
	}
}

// holdHandshakeListener delays the first handshake response it writes, after
// putting it on the wire, until released. What the server does between telling
// a peer it is connected and finishing with that connection is exactly the
// interval in which a second handshake must already see the room taken.
type holdHandshakeListener struct {
	net.Listener
	sent    chan struct{}
	release chan struct{}
	once    sync.Once
}

type holdHandshakeConn struct {
	net.Conn
	l *holdHandshakeListener
}

func (l *holdHandshakeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &holdHandshakeConn{Conn: c, l: l}, nil
}

func (c *holdHandshakeConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err == nil && strings.HasPrefix(string(b), "HTTP/1.1 101") {
		c.l.once.Do(func() {
			close(c.l.sent)
			select {
			case <-c.l.release:
			case <-time.After(wsTestTimeout):
			}
		})
	}
	return n, err
}

func TestWebSocketRoomIsTakenBeforeThePeerIsToldItIsConnected(t *testing.T) {
	t.Parallel()
	// The handshake response is what tells a peer the connection is open, so a
	// client that dials again the moment it reads it must find the room taken.
	// Counting the connection after that response was written left a window in
	// which the second handshake was admitted past the limit.
	for name, limits := range map[string]WSOptions{
		"per address": {MaxConnections: 10, MaxConnectionsPerIP: 1},
		"per process": {MaxConnections: 1, MaxConnectionsPerIP: 10},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.WebSocket = limits
			app := New(opts)
			app.WS("/ws", wsEcho)
			mustBuild(t, app)

			server := httptest.NewUnstartedServer(app)
			held := &holdHandshakeListener{Listener: server.Listener, sent: make(chan struct{}), release: make(chan struct{})}
			server.Listener = held
			server.Start()
			t.Cleanup(server.Close)
			t.Cleanup(func() {
				select {
				case <-held.release:
				default:
					close(held.release)
				}
			})

			first := make(chan int, 1)
			go func() {
				_, response := dialRaw(t, server.URL, "/ws")
				first <- response.StatusCode
			}()
			select {
			case <-held.sent:
			case <-time.After(wsTestTimeout):
				t.Fatal("the first handshake was never answered")
			}
			if status := <-first; status != http.StatusSwitchingProtocols {
				t.Fatalf("the first connection got status %d, want it accepted", status)
			}
			_, response := dialRaw(t, server.URL, "/ws")
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("a second connection got status %d while the first was still being set up, want %d",
					response.StatusCode, http.StatusServiceUnavailable)
			}
			close(held.release)
		})
	}
}

func TestLiveRegistryReservationsCountAgainstTheLimits(t *testing.T) {
	t.Parallel()
	registry := liveRegistry[*WSConn]{limit: 2, perKeyLimit: 1}
	if got := registry.reserve("a"); got != admitted {
		t.Fatalf("first reservation = %v, want admitted", got)
	}
	if got := registry.reserve("a"); got != registryKeyFull {
		t.Errorf("second reservation for the same key = %v, want the key reported full", got)
	}
	if got := registry.reserve("b"); got != admitted {
		t.Fatalf("reservation for another key = %v, want admitted", got)
	}
	if got := registry.reserve("c"); got != registryFull {
		t.Errorf("third reservation = %v, want the registry reported full", got)
	}

	// Recording a reserved entry keeps the room it held, and giving the
	// reservation back frees it.
	first := &WSConn{}
	if got := registry.addReserved(first, "a"); got != admitted {
		t.Fatalf("addReserved = %v, want admitted", got)
	}
	registry.unreserve("b")
	if registry.reserved != 0 || len(registry.perKeyReserved) != 0 {
		t.Errorf("reservations left behind: %d, %v", registry.reserved, registry.perKeyReserved)
	}
	if got := registry.admits("a"); got != registryKeyFull {
		t.Errorf("admits(a) = %v, want the recorded entry to hold its key's room", got)
	}
	if got := registry.admits("b"); got != admitted {
		t.Errorf("admits(b) = %v, want the returned room to be free", got)
	}
	registry.remove(first)
	if got := registry.admits("a"); got != admitted {
		t.Errorf("admits(a) after remove = %v, want admitted", got)
	}

	// A reservation that outlives the start of a shutdown is not recorded.
	registry.draining = true
	registry.reserved, registry.perKeyReserved = 1, map[string]int{"a": 1}
	if got := registry.addReserved(&WSConn{}, "a"); got != registryDraining {
		t.Errorf("addReserved while draining = %v, want it refused", got)
	}
	if registry.reserved != 0 || registry.count() != 0 {
		t.Errorf("draining refusal left reserved=%d entries=%d", registry.reserved, registry.count())
	}
}
