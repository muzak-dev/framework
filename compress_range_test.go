package muzak

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A compressed 200 that came from http.ServeContent still says
// "Accept-Ranges: bytes", and the client may take it at its word. The Range
// request that follows is answered with an uncompressed 206, which is the
// right slice of the identity content under a Content-Range of the identity
// length: nothing is corrupted, a range is just never served compressed. This
// pins that, so that a change to either side cannot start splicing a slice of
// one representation into the other.
func TestCompressedContentAndItsRangeRequestsAgree(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 500)
	handler := Compress(CompressionOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		http.ServeContent(w, r, "a.txt", time.Unix(1700000000, 0), strings.NewReader(content))
	}))

	full := httptest.NewRequest(http.MethodGet, "/a.txt", nil)
	full.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, full)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("the full response was not compressed: %v", rec.Header())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != content {
		t.Fatal("the compressed body does not decode to the content")
	}

	ranged := httptest.NewRequest(http.MethodGet, "/a.txt", nil)
	ranged.Header.Set("Accept-Encoding", "gzip")
	ranged.Header.Set("Range", "bytes=5-14")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, ranged)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Error("a 206 was compressed; its Content-Range would then describe the wrong bytes")
	}
	if got, want := rec.Body.String(), content[5:15]; got != want {
		t.Errorf("range body = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Content-Range"), "bytes 5-14/5000"; got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
}
