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

// A compressed 200 that came from http.ServeContent no longer says
// "Accept-Ranges: bytes", because no range of the bytes it sent can be asked
// for. A client may send a Range request all the same, and it is answered with
// an uncompressed 206, which is the right slice of the identity content under a
// Content-Range of the identity length: nothing is corrupted, a range is just
// never served compressed. This pins that, so that a change to either side
// cannot start splicing a slice of one representation into the other. What a
// Range request carrying If-Range is answered with is pinned by
// TestCompressAnswersADateIfRangeWithTheWholeContent.
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
	if got := rec.Header().Get("Accept-Ranges"); got != "" {
		t.Errorf("the compressed response says Accept-Ranges: %s, inviting a range of bytes it did not send", got)
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

// A client resuming a download sends Range with If-Range, asking for the rest
// only if what it holds is still current. Holding a compressed 200, it has a
// weak entity tag at most, which If-Range may not carry, so it sends the
// Last-Modified date. That date matched, and ServeContent answered with an
// uncompressed 206 that a client which never looks at Content-Encoding splices
// onto the compressed bytes it already has. When an encoding is negotiated a
// date cannot tell which representation the client holds, so the whole
// content is sent, which is what an If-Range that does not match asks for.
func TestCompressAnswersADateIfRangeWithTheWholeContent(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcdefghij", 500)
	modified := time.Unix(1700000000, 0)
	serve := func(tag string) http.Handler {
		return Compress(CompressionOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			if tag != "" {
				w.Header().Set("ETag", tag)
			}
			http.ServeContent(w, r, "a.txt", modified, strings.NewReader(content))
		}))
	}
	date := modified.UTC().Format(http.TimeFormat)

	tests := []struct {
		name     string
		tag      string
		encoding string
		ifRange  string
		// partial says whether the answer is the 206 rather than all of it.
		partial bool
	}{
		{"a date, with gzip", "", "gzip", date, false},
		{"a date, with deflate", "", "deflate", date, false},
		{"a date, with gzip, under a strong tag", `"v1"`, "gzip", date, false},
		// A weak tag never matches If-Range, which is ServeContent's doing.
		{"the weakened tag", `"v1"`, "gzip", `W/"v1"`, false},
		// A strong tag is one only an uncompressed answer carried, so the
		// client holds the identity bytes and the range continues them.
		{"the strong tag, with gzip", `"v1"`, "gzip", `"v1"`, true},
		// A client that negotiated nothing was sent the identity bytes, which
		// a date names as well as a tag does.
		{"a date, with no encoding", "", "", date, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/a.txt", nil)
			if tc.encoding != "" {
				req.Header.Set("Accept-Encoding", tc.encoding)
			}
			req.Header.Set("Range", "bytes=100-")
			req.Header.Set("If-Range", tc.ifRange)
			rec := httptest.NewRecorder()
			serve(tc.tag).ServeHTTP(rec, req)

			if tc.partial {
				if rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != content[100:] {
					t.Errorf("status = %d, Content-Encoding = %q; want an uncompressed 206 of the rest", rec.Code, rec.Header().Get("Content-Encoding"))
				}
				return
			}
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Range") != "" {
				t.Fatalf("status = %d, Content-Range = %q; want the whole content with 200", rec.Code, rec.Header().Get("Content-Range"))
			}
			if got := rec.Header().Get("Content-Encoding"); got != tc.encoding {
				t.Errorf("Content-Encoding = %q, want %q", got, tc.encoding)
			}
			if got := rec.Header().Get("Accept-Ranges"); got != "" {
				t.Errorf("Accept-Ranges = %q on a compressed answer, want none", got)
			}
			if tc.encoding != "gzip" {
				return
			}
			zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := io.ReadAll(zr); string(got) != content {
				t.Error("the compressed body does not decode to the whole content")
			}
		})
	}
}
