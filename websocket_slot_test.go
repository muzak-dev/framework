package muzak

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openCountListener counts the connections it accepted that have not been
// closed yet, which is the number of file descriptors a server is holding
// regardless of what its own bookkeeping says.
type openCountListener struct {
	net.Listener
	open atomic.Int64
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
