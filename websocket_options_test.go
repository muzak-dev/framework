package muzak

import (
	"testing"
	"time"
)

func TestWSOptionsNegativePongTimeoutFallsBackToTheDefault(t *testing.T) {
	t.Parallel()
	// Every other negative duration here removes a bound, but a keepalive whose
	// answer may take no time at all closes every connection at its first ping,
	// and one that may take forever is no keepalive. Neither is what a negative
	// value was written for, so it reads as unset.
	got := WSOptions{PingInterval: time.Second, PongTimeout: -1}.withDefaults()
	if got.PongTimeout != DefaultWSPongTimeout {
		t.Errorf("PongTimeout = %v, want %v", got.PongTimeout, DefaultWSPongTimeout)
	}
}
