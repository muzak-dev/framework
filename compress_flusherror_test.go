package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A handler that streams through http.ResponseController learns from a failed
// flush that the client has gone. The compression wrapper answered nil for it
// whatever became of the bytes underneath.
func TestCompressReportsAFlushThatFailed(t *testing.T) {
	t.Parallel()
	for _, encoding := range []string{"", "gzip", "deflate"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			t.Parallel()
			var flushErr error
			handler := Compress(CompressionOptions{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(strings.Repeat("event\n", 400)))
				flushErr = http.NewResponseController(w).Flush()
			}))

			req := httptest.NewRequest(http.MethodGet, "/stream", nil)
			req.Header.Set("Accept-Encoding", encoding)
			handler.ServeHTTP(&brokenWriter{recorder: httptest.NewRecorder(), failFlush: 1}, req)

			if !errors.Is(flushErr, errBrokenWriter) {
				t.Fatalf("Flush through Compress = %v, want the error of the writer underneath", flushErr)
			}
		})
	}
}
