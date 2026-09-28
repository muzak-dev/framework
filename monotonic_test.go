package muzak

import (
	"net"
	"testing"
	"time"
)

// hasMonotonicReading reports whether a time carries a reading of the
// monotonic clock. The == operator compares that reading along with the
// instant, and Round(0) strips it, so the two differ exactly when there is one
// to strip.
func hasMonotonicReading(t time.Time) bool { return t != t.Round(0) }

func TestMonotonicStampMeasuresOnTheMonotonicClock(t *testing.T) {
	t.Parallel()
	// A wall-clock step cannot be provoked without changing the system clock,
	// so this asserts what makes one harmless instead: every interval is an
	// offset from an epoch that carries a monotonic reading, which
	// time.Since uses in preference to the wall clock.
	var stamp monotonicStamp
	stamp.start()
	if !hasMonotonicReading(stamp.epoch) {
		t.Fatal("the stamp's epoch has no monotonic reading, so a wall-clock step would skew every interval")
	}
	if got := stamp.last(); got != 0 {
		t.Errorf("last() = %v straight after start, want the epoch itself", got)
	}
	time.Sleep(5 * time.Millisecond)
	if idle := stamp.since(); idle < 5*time.Millisecond {
		t.Errorf("since() = %v after sleeping 5ms, want at least that", idle)
	}
	before := stamp.now()
	stamp.mark()
	if marked := stamp.last(); marked < before || stamp.since() < 0 {
		t.Errorf("mark() recorded %v, want no earlier than %v and never in the future", marked, before)
	}
}

func TestLongLivedConnectionsKeepTimeOnTheMonotonicClock(t *testing.T) {
	t.Parallel()
	stream := newTestStream(t, SSEOptions{})
	if !hasMonotonicReading(stream.lastWrite.epoch) {
		t.Error("an event stream measures its keepalive against the wall clock")
	}
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	conn := newWSConn(server, nil, false, "", WSOptions{}.withDefaults())
	if !hasMonotonicReading(conn.lastPong.epoch) {
		t.Error("a websocket measures its pong deadline against the wall clock")
	}
}
