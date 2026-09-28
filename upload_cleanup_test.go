package muzak

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// spilledFiles records the temporary files a multipart form was written to,
// by opening each part the parser put on disk.
type spilledFiles struct {
	mu    sync.Mutex
	paths []string
}

func (s *spilledFiles) record(t *testing.T, r *http.Request) {
	t.Helper()
	if r.MultipartForm == nil {
		t.Error("the form was not parsed")
		return
	}
	for _, headers := range r.MultipartForm.File {
		for _, header := range headers {
			f, err := header.Open()
			if err != nil {
				t.Errorf("opening a part: %v", err)
				continue
			}
			if file, ok := f.(*os.File); ok {
				s.mu.Lock()
				s.paths = append(s.paths, file.Name())
				s.mu.Unlock()
			}
			_ = f.Close()
		}
	}
}

// assertRemoved waits briefly for every recorded file to be gone. The wait
// covers only the time between the client reading its response and the
// server's deferred work finishing, and is never needed in practice.
func (s *spilledFiles) assertRemoved(t *testing.T, want int) {
	t.Helper()
	s.mu.Lock()
	paths := append([]string(nil), s.paths...)
	s.mu.Unlock()
	if len(paths) != want {
		t.Fatalf("recorded %d spilled files, want %d", len(paths), want)
	}
	deadline := time.Now().Add(time.Second)
	for _, path := range paths {
		for {
			_, err := os.Stat(path)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("temporary file %s outlived its request", path)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// A form parsed by anything but the binder, here a guard and a handler that
// read it through Context.Request, is parsed on the copy of the request the
// middleware made, which net/http's own cleanup never sees. Its spilled files
// are removed when the request ends, whatever the route binds.
func TestFormParsedOutsideTheBinderLeavesNoFiles(t *testing.T) {
	t.Parallel()
	spilled := &spilledFiles{}
	guard := func(ctx *Context) error {
		// A memory budget of one byte puts every file part on disk, which is
		// what a large upload does under net/http's 32 MiB default.
		if err := ctx.Request().ParseMultipartForm(1); err != nil {
			return err
		}
		spilled.record(t, ctx.Request())
		if ctx.Request().FormValue("csrf") == "" {
			return NewHTTPError(http.StatusForbidden, "missing csrf token")
		}
		return nil
	}
	app := New(quietOptions())
	app.Post("/guarded", func(ctx *Context, _ Empty) (Empty, error) { return Empty{}, nil }, WithDependencies(guard))
	app.Post("/handler", func(ctx *Context, _ struct {
		Q string `query:"q" required:"false"`
	}) (Empty, error) {
		if err := ctx.Request().ParseMultipartForm(1); err != nil {
			return Empty{}, err
		}
		spilled.record(t, ctx.Request())
		return Empty{}, nil
	})
	built := mustBuild(t, app)
	srv := httptest.NewServer(built)
	defer srv.Close()

	body := "--B\r\nContent-Disposition: form-data; name=\"f\"; filename=\"a\"\r\n\r\n" +
		strings.Repeat("a", 64<<10) + "\r\n--B--\r\n"
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/guarded", http.StatusForbidden},
		{"/handler", http.StatusOK},
	} {
		resp, err := http.Post(srv.URL+tc.path, "multipart/form-data; boundary=B", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("POST %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}

	// Driven through the handler directly, as a test or an embedding server
	// does, there is no net/http cleanup at all.
	req := httptest.NewRequest(http.MethodPost, "/handler", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=B")
	assertStatus(t, doRequest(t, built, req), http.StatusOK)

	spilled.assertRemoved(t, 3)
}
