package muzak

import (
	"testing"
	"time"
)

func TestSSEReaderRetryIsNeverNegativeOrBeyondItsCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		field string
		want  time.Duration
	}{
		{"an ordinary delay", "2500", 2500 * time.Millisecond},
		{"zero", "0", 0},
		{"the cap itself", "3600000", sseMaxRetry},
		{"just past the cap", "3600001", sseMaxRetry},
		{"one that would wrap a duration", "9223372036855", sseMaxRetry},
		{"the largest integer", "9223372036854775807", sseMaxRetry},
		{"more digits than an integer holds", "99999999999999999999999", sseMaxRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _ := serveRaw(t, "retry: "+tc.field+"\ndata: x\n\n")
			reader := openStream(t, server.URL, "/")
			readAll(t, reader)
			if got := reader.Retry(); got != tc.want {
				t.Errorf("retry: %s gave Retry() = %v, want %v", tc.field, got, tc.want)
			}
		})
	}
}

func TestSSEReaderIgnoresARetryThatIsNotOnlyDigits(t *testing.T) {
	t.Parallel()
	// The format asks for digits and nothing else, so a sign is not part of a
	// delay however leniently an integer parser would read it.
	for _, field := range []string{"+5", "-5", "-0", "5 ", "0x10", "1e3", "", "  5"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			server, _ := serveRaw(t, "retry: 250\ndata: a\n\nretry:"+field+"\ndata: b\n\n")
			reader := openStream(t, server.URL, "/")
			readAll(t, reader)
			if got := reader.Retry(); got != 250*time.Millisecond {
				t.Errorf("retry:%q replaced the delay of 250ms with %v", field, got)
			}
		})
	}
}
