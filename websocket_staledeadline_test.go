package muzak

import (
	"bufio"
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// gatedDeadlineConn is a network connection that lets a test hold one call to
// SetReadDeadline in mid-air, which is what a goroutine that the scheduler has
// not got round to running looks like from the connection's side.
type gatedDeadlineConn struct {
	net.Conn
	// afterFirstData runs once, right after the first Read that returned bytes.
	afterFirstData func()
	dataSeen       atomic.Bool
	// hold makes a SetReadDeadline that names a moment already past wait for
	// gate, which is what the cancelled context's callback calls it with.
	hold atomic.Bool
	gate chan struct{}
}

func (g *gatedDeadlineConn) Read(p []byte) (int, error) {
	n, err := g.Conn.Read(p)
	if n > 0 && g.afterFirstData != nil && g.dataSeen.CompareAndSwap(false, true) {
		g.afterFirstData()
	}
	return n, err
}

func (g *gatedDeadlineConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && time.Until(t) <= 0 && g.hold.Load() {
		<-g.gate
	}
	return g.Conn.SetReadDeadline(t)
}

// context.AfterFunc's stop function reports false when the callback has
// already started, and does not wait for it. A caller's context cancelled at
// the instant its read completes can therefore have its callback run after the
// next read has cleared the deadline, and move that read's deadline into the
// past: a healthy connection fails a read whose context is still live.
func TestWSConnStaleContextCallbackDoesNotDeadlineTheNextRead(t *testing.T) {
	t.Parallel()
	serverSide, clientSide := net.Pipe()
	gated := &gatedDeadlineConn{Conn: serverSide, gate: make(chan struct{})}
	settings := WSOptions{}.withDefaults()
	server := newWSConn(gated, bufio.NewReader(gated), false, "", settings)
	client := newWSConn(clientSide, bufio.NewReader(clientSide), true, "", settings)
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientSide.Close()
	})

	first, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	// The context is cancelled while the read is still running, so its
	// callback starts before the read can stop it, and is then held before it
	// reaches the connection.
	gated.hold.Store(true)
	gated.afterFirstData = cancelFirst
	release := time.AfterFunc(100*time.Millisecond, func() { close(gated.gate) })
	defer release.Stop()

	// The peer keeps reading, so a connection that fails is told so at once
	// rather than left waiting for a close frame nobody takes.
	go func() {
		for {
			if _, _, err := client.Read(t.Context()); err != nil {
				return
			}
		}
	}()
	go func() { _ = client.WriteText(t.Context(), "one") }()
	if text, err := server.ReadText(first); err != nil || text != "one" {
		t.Fatalf("the first read = %q, %v", text, err)
	}

	// The next read has a context of its own, which is live. Nothing was
	// cancelled after the first read returned, so nothing may end this one.
	second, cancelSecond := context.WithCancel(t.Context())
	defer cancelSecond()
	type result struct {
		text string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		text, err := server.ReadText(second)
		got <- result{text, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("the second read ended by itself with %q, %v; the first read's context was still acting on the connection", r.text, r.err)
	case <-time.After(300 * time.Millisecond):
	}

	go func() { _ = client.WriteText(t.Context(), "two") }()
	select {
	case r := <-got:
		if r.err != nil || r.text != "two" {
			t.Fatalf("the second read = %q, %v, want two", r.text, r.err)
		}
	case <-time.After(wsTestTimeout):
		t.Fatal("the second read never returned")
	}
}
