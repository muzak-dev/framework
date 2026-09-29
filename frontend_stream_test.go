package muzak

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// countingFS serves files that cannot seek and count what has been read from
// them, so that a test can see how much was held before the first byte of a
// response was written.
type countingFS struct {
	fstest.MapFS
	read     *atomic.Int64
	zeroSize bool
}

func (f countingFS) Open(name string) (fs.File, error) {
	file, err := f.MapFS.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingFile{File: file, read: f.read, zeroSize: f.zeroSize}, nil
}

type countingFile struct {
	fs.File
	read     *atomic.Int64
	zeroSize bool
}

func (f *countingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.read.Add(int64(n))
	return n, err
}

func (f *countingFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if f.zeroSize {
		return zeroSizeInfo{info}, err
	}
	return info, err
}

type zeroSizeInfo struct{ fs.FileInfo }

func (zeroSizeInfo) Size() int64 { return 0 }

// firstWriteWriter records how much of a file had been read when the response
// began.
type firstWriteWriter struct {
	*httptest.ResponseRecorder
	read       *atomic.Int64
	readAtSend int64
	wrote      bool
}

func (w *firstWriteWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.readAtSend = w.read.Load()
	}
	return w.ResponseRecorder.Write(p)
}

// A filesystem whose files cannot seek used to be read whole into memory,
// whatever their size, before a byte was sent. What is held is bounded, and a
// larger file is streamed.
func TestFrontendStreamsALargeFileThatCannotSeek(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("0123456789abcdef"), (3*maxBufferedFrontendFile)/16)
	for name, zeroSize := range map[string]bool{"size reported": false, "size unknown": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var read atomic.Int64
			app := New(quietOptions())
			app.Frontend("/", FrontendOptions{FS: countingFS{
				MapFS:    fstest.MapFS{"index.html": {Data: []byte("app")}, "big.bin": {Data: big, ModTime: time.Unix(1700000000, 0)}},
				read:     &read,
				zeroSize: zeroSize,
			}})
			mustBuild(t, app)

			w := &firstWriteWriter{ResponseRecorder: httptest.NewRecorder(), read: &read}
			req := httptest.NewRequest(http.MethodGet, "/big.bin", nil)
			req.Header.Set("Accept", "*/*")
			app.ServeHTTP(w, req)

			assertStatus(t, w.ResponseRecorder, http.StatusOK)
			if !bytes.Equal(w.Body.Bytes(), big) {
				t.Fatalf("body is %d bytes, want the %d of the file", w.Body.Len(), len(big))
			}
			if limit := int64(maxBufferedFrontendFile + 1<<20); w.readAtSend > limit {
				t.Errorf("%d bytes were read before the response began, want at most about %d", w.readAtSend, limit)
			}
			if !zeroSize {
				if got := w.Header().Get("Content-Length"); got != strconv.Itoa(len(big)) {
					t.Errorf("Content-Length = %q, want %d", got, len(big))
				}
			}
			if w.Header().Get("Last-Modified") == "" {
				t.Error("Last-Modified was dropped from a streamed file")
			}
		})
	}
}

// A small file is still buffered, which is what lets ServeContent answer a
// range request and a conditional one from it.
func TestFrontendStillBuffersASmallFileThatCannotSeek(t *testing.T) {
	t.Parallel()
	var read atomic.Int64
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{FS: countingFS{
		MapFS: fstest.MapFS{"a.txt": {Data: []byte("0123456789")}},
		read:  &read,
	}})
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodGet, "/a.txt", nil)
	req.Header.Set("Range", "bytes=2-4")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusPartialContent)
	if rec.Body.String() != "234" {
		t.Errorf("body = %q, want the range", rec.Body.String())
	}
}
