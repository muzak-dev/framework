package muzak

import (
	"testing"
	"time"
)

// A write deadline on an HTTP/2 stream cannot interrupt a frame that is
// already being written, so a client that grants a large window and stops
// reading the socket held every stream of its connection past WriteTimeout.
// http.HTTP2Config.WriteByteTimeout is what bounds that write, so it has to
// follow the option, and a disabled option has to disable it too.
func TestHTTP2WriteByteTimeoutFollowsWriteTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		write time.Duration
		want  time.Duration
	}{
		{"the default", 0, DefaultWriteTimeout},
		{"an explicit value", 7 * time.Second, 7 * time.Second},
		{"disabled", -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.WriteTimeout = tc.write
			server := New(opts).newServer()

			if server.HTTP2 == nil {
				t.Fatal("the server has no HTTP/2 configuration, so a stalled socket write is unbounded over h2")
			}
			if got := server.HTTP2.WriteByteTimeout; got != tc.want {
				t.Errorf("HTTP2.WriteByteTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}
