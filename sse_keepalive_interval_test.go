package muzak

import (
	"context"
	"slices"
	"testing"
	"time"
)

// A keepalive asked for every interval arrives every interval. Measured from
// fixed ticks it skipped whenever its own previous write had landed just after
// the tick before, so it arrived at 1, 3 and 4 intervals: twice the silence a
// proxy's idle timeout was being kept off with.
//
// The gaps between keepalives are held to those of a timer the test runs over
// the same stretch, re-armed for the interval each time it fires, which is
// what a keepalive every interval amounts to. On a loaded machine both wake
// late alike, and counting keepalives against the clock alone took that
// lateness for skipped ticks. A skipped tick puts a whole interval between
// the two, whatever the load.
func TestSSEKeepaliveArrivesEveryInterval(t *testing.T) {
	t.Parallel()
	const interval = 100 * time.Millisecond
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		<-stream.Context().Done()
		return nil
	}, WithSSE(SSEOptions{KeepAlive: interval}))
	mustBuild(t, app)
	srv := startSSEServer(t, app, false)

	reader, _, err := SSEDial(t.Context(), srv.URL+"/stream",
		SSEDialOptions{HTTPClient: srv.Client(), KeepComments: true})
	if err != nil {
		t.Fatalf("SSEDial() = %v", err)
	}
	defer reader.Close()

	ctx, cancel := context.WithCancel(t.Context())
	reference := make(chan []time.Time, 1)
	go func() {
		var fired []time.Time
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				reference <- fired
				return
			case <-timer.C:
				// Read when this goroutine runs rather than taken from the
				// channel, because the keepalive's goroutine has to run too
				// before it can write.
				fired = append(fired, time.Now())
				timer.Reset(interval)
			}
		}
	}()

	// Fifteen gaps, about a second and a half on an idle machine.
	const gaps = 15
	var arrived []time.Time
	for len(arrived) <= gaps {
		next, cancelNext := context.WithTimeout(t.Context(), sseTestTimeout)
		message, err := reader.Next(next)
		cancelNext()
		if err != nil {
			cancel()
			t.Fatalf("Next() = %v after %d keepalives", err, len(arrived))
		}
		if message.Comment == sseKeepAliveComment {
			arrived = append(arrived, time.Now())
		}
	}
	cancel()
	fired := <-reference

	got, want := medianGap(arrived), medianGap(fired)
	if want == 0 {
		t.Fatalf("the reference timer fired %d times while %d keepalives arrived", len(fired), len(arrived))
	}
	// Skipping every other tick made the typical gap two intervals.
	if got > want+interval/2 {
		t.Errorf("keepalives asked for every %v arrived a median %v apart, where a timer re-armed every interval fired a median %v apart",
			interval, got.Round(time.Millisecond), want.Round(time.Millisecond))
	}
}

// medianGap returns the median of the gaps between consecutive moments, or
// zero when there are too few moments to have a gap.
func medianGap(moments []time.Time) time.Duration {
	if len(moments) < 2 {
		return 0
	}
	gaps := make([]time.Duration, 0, len(moments)-1)
	for i := 1; i < len(moments); i++ {
		gaps = append(gaps, moments[i].Sub(moments[i-1]))
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2]
}
