package muzak

import (
	"strings"
	"testing"
	"time"
)

// A retry shorter than a millisecond is written as 1, because 0 asks a client
// to reconnect at once.
func TestSSERetryShorterThanAMillisecondIsNotZero(t *testing.T) {
	t.Parallel()
	for _, retry := range []time.Duration{time.Nanosecond, 500 * time.Microsecond, 999_999 * time.Nanosecond} {
		frame := sseFrame{retry: retry}
		if got := string(frame.appendFields(nil)); !strings.Contains(got, "retry: 1\n") {
			t.Fatalf("a retry of %v was written as %q, want retry: 1", retry, got)
		}
	}
}
