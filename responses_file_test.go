package muzak

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// FileResponse serves a file a handler names, and the name is very often what
// a client asked for. These tests are the attacks on that name, on the
// headers built from it, and on the filesystem behind it.

var fileModTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fileFS is the filesystem most of these tests serve from.
func fileFS() fstest.MapFS {
	file := func(data string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(data), ModTime: fileModTime}
	}
	return fstest.MapFS{
		"docs/guide.pdf":        file("%PDF-1.7 the guide"),
		"docs/notes.txt":        file("<html><script>alert(1)</script></html>"),
		"docs/README":           file("<html><script>alert(1)</script></html>"),
		"docs/page.html":        file("<h1>page</h1>"),
		"docs/logo.svg":         file(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"docs/digits.txt":       file("0123456789"),
		"docs/empty.txt":        file(""),
		"docs/sub/inner.txt":    file("inner"),
		".env":                  file("SECRET=1"),
		"docs/.git/config":      file("[core]"),
		".well-known/security":  file("contact"),
		"docs/pipe":             {Mode: fs.ModeNamedPipe},
		"docs/device":           {Mode: fs.ModeDevice | fs.ModeCharDevice},
		"docs/report.final.csv": file("a,b\n"),
	}
}

// fileApp serves fileFS at /files/{name...}, naming whatever file the request
// asks for, which is the shape that makes the name hostile.
func fileApp(t *testing.T, files fs.FS, edit func(*FileResponse), opts ...RouteOption) *App {
	t.Helper()
	app := New(quietOptions())
	app.Get("/files/{name...}", func(ctx *Context, in struct {
		Name string `path:"name"`
	}) (FileResponse, error) {
		out := FileResponse{FS: files, Name: in.Name}
		if edit != nil {
			edit(&out)
		}
		return out, nil
	}, opts...)
	return app
}

func TestFileResponseServesAFile(t *testing.T) {
	t.Parallel()
	app := fileApp(t, fileFS(), nil)
	rec := do(t, app, http.MethodGet, "/files/docs/guide.pdf")
	assertStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "%PDF-1.7 the guide" {
		t.Errorf("body = %q, want the file", rec.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Type":           "application/pdf",
		"Content-Length":         "18",
		"Last-Modified":          fileModTime.Format(http.TimeFormat),
		"Accept-Ranges":          "bytes",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The type comes from the extension and never from the bytes: a text file of
// markup is text, and a file the table does not know is octet-stream.
func TestFileResponseNeverSniffs(t *testing.T) {
	t.Parallel()
	app := fileApp(t, fileFS(), nil)
	for path, want := range map[string]string{
		"/files/docs/notes.txt":        "text/plain; charset=utf-8",
		"/files/docs/README":           "application/octet-stream",
		"/files/docs/report.final.csv": "text/csv; charset=utf-8",
		"/files/docs/empty.txt":        "text/plain; charset=utf-8",
	} {
		rec := do(t, app, http.MethodGet, path)
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type = %q, want %q", path, got, want)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "" {
			t.Errorf("%s: Content-Security-Policy = %q, want none for a passive type", path, got)
		}
	}
}

// HTML, SVG and other XML would run a client's script as the application, so
// they are sandboxed unless the handler says the file is part of the site.
func TestFileResponseSandboxesActiveContent(t *testing.T) {
	t.Parallel()
	app := fileApp(t, fileFS(), nil)
	for _, path := range []string{"/files/docs/page.html", "/files/docs/logo.svg"} {
		if got := do(t, app, http.MethodGet, path).Header().Get("Content-Security-Policy"); got != "sandbox" {
			t.Errorf("%s: Content-Security-Policy = %q, want sandbox", path, got)
		}
	}
	trusted := fileApp(t, fileFS(), func(out *FileResponse) { out.AllowActiveContent = true })
	if got := do(t, trusted, http.MethodGet, "/files/docs/page.html").Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("Content-Security-Policy = %q with AllowActiveContent, want none", got)
	}
	// A type named by the handler is judged the same way.
	named := fileApp(t, fileFS(), func(out *FileResponse) { out.ContentType = "application/xhtml+xml" })
	if got := do(t, named, http.MethodGet, "/files/docs/notes.txt").Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("a named XML type: Content-Security-Policy = %q, want sandbox", got)
	}
}

func TestFileResponseContentTypeOverride(t *testing.T) {
	t.Parallel()
	app := fileApp(t, fileFS(), func(out *FileResponse) { out.ContentType = "text/markdown; charset=utf-8" })
	if got := do(t, app, http.MethodGet, "/files/docs/README").Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the one the handler named", got)
	}
	bad := fileApp(t, fileFS(), func(out *FileResponse) { out.ContentType = "text/plain\r\nX-Injected: 1" })
	rec := do(t, bad, http.MethodGet, "/files/docs/README")
	assertStatus(t, rec, http.StatusInternalServerError)
	if rec.Header().Get("X-Injected") != "" {
		t.Error("a header was injected through the content type")
	}
}

// Every one of these names reaches outside what was meant to be served, or
// asks for a file the server must not hand out, and is answered as a file
// that does not exist, without the filesystem being asked to open it.
func TestFileResponseRefusesHostileNames(t *testing.T) {
	t.Parallel()
	files := &closeCountingFS{FS: fileFS()}
	app := New(quietOptions())
	var name string
	var mu sync.Mutex
	app.Get("/file", func(ctx *Context, _ Empty) (FileResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		return FileResponse{FS: files, Name: name}, nil
	})
	for _, hostile := range []string{
		"",
		".",
		"..",
		"../secret",
		"docs/../../secret",
		"docs/../.env",
		"/etc/passwd",
		"/docs/guide.pdf",
		"docs//guide.pdf",
		"docs/./guide.pdf",
		"docs/guide.pdf/",
		".env",
		"docs/.git/config",
		".well-known/security",
		"docs/.",
		`docs\guide.pdf`,
		`..\secret`,
		`docs\..\..\secret`,
		"docs/guide.pdf\x00.txt",
		"docs/\nguide.pdf",
		"docs/guide.pdf\r",
		"docs/guide\x7f.pdf",
		"docs/\x1b[31m",
		strings.Repeat("a/", maxFileNameLength/2) + "x",
	} {
		mu.Lock()
		name = hostile
		mu.Unlock()
		rec := do(t, app, http.MethodGet, "/file")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q: status %d, want 404", hostile, rec.Code)
		}
	}
	if opened := files.opened(); opened != 0 {
		t.Errorf("the filesystem was asked to open %d of the hostile names, want none", opened)
	}
}

// The checks Windows needs are run on every platform here, so that they are
// tested where the suite runs; the server consults them on Windows only.
func TestWindowsUnsafeNames(t *testing.T) {
	t.Parallel()
	for name, unsafe := range map[string]bool{
		"CON":                    true,
		"con":                    true,
		"con.txt":                true,
		"Con.tar.gz":             true,
		"CON .txt":               true,
		"PRN":                    true,
		"AUX":                    true,
		"NUL":                    true,
		"nul.json":               true,
		"COM1":                   true,
		"com9.log":               true,
		"LPT1":                   true,
		"lpt9":                   true,
		"COM\xc2\xb9":            true,
		"LPT\xc2\xb2.txt":        true,
		"LPT\xc2\xb3":            true,
		"CONIN$":                 true,
		"conout$.txt":            true,
		"docs/aux.txt":           true,
		"docs/nul/x":             true,
		"secret.txt.":            true,
		"secret.txt ":            true,
		"docs./secret.txt":       true,
		"docs /secret.txt":       true,
		"C:":                     true,
		"C:secret.txt":           true,
		"c:/windows/win.ini":     true,
		"secret.txt:$DATA":       true,
		"docs/file.txt::$DATA":   true,
		"GIT~1/config":           true,
		"ENV~1":                  true,
		"docs/ADMINI~1/x":        true,
		"console.txt":            false,
		"connect":                false,
		"COM10":                  false,
		"COM0":                   false,
		"LPT":                    false,
		"auxiliary.txt":          false,
		"docs/guide.pdf":         false,
		"a.b.c":                  false,
		"tilde~name.txt":         false,
		"docs/report.final.csv":  false,
		"COM\xc2\xb9x":           false,
		"\xe2\x80\xaereport.txt": false,
	} {
		if got := windowsUnsafeName(name); got != unsafe {
			t.Errorf("windowsUnsafeName(%q) = %v, want %v", name, got, unsafe)
		}
	}
}

func TestFileResponseRefusesWhatIsNotARegularFile(t *testing.T) {
	t.Parallel()
	files := &closeCountingFS{FS: fileFS()}
	app := fileApp(t, files, nil)
	for _, path := range []string{"/files/docs", "/files/docs/sub", "/files/docs/pipe", "/files/docs/device"} {
		if rec := do(t, app, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
	files.assertAllClosed(t)
}

// A file that is not there is an ordinary 404 and logs nothing. Any other
// failure to open it is a 404 to the client, which learns nothing about the
// server's filesystem, and logged for the operator.
func TestFileResponseFailuresToOpen(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Get("/missing", func(ctx *Context, _ Empty) (FileResponse, error) {
		return FileResponse{FS: fileFS(), Name: "docs/nope.pdf"}, nil
	})
	app.Get("/denied", func(ctx *Context, _ Empty) (FileResponse, error) {
		return FileResponse{FS: failingFS{openErr: &fs.PathError{Op: "open", Path: "/srv/private/x", Err: fs.ErrPermission}}, Name: "x"}, nil
	})
	app.Get("/stat", func(ctx *Context, _ Empty) (FileResponse, error) {
		return FileResponse{FS: failingFS{statErr: errors.New("disk on fire")}, Name: "x"}, nil
	})
	app.Get("/nofs", func(ctx *Context, _ Empty) (FileResponse, error) {
		return FileResponse{Name: "x"}, nil
	})

	rec := do(t, app, http.MethodGet, "/missing")
	assertStatus(t, rec, http.StatusNotFound)
	if strings.Contains(logs.String(), "request failed") {
		t.Errorf("a missing file was logged: %s", logs.String())
	}
	for _, path := range []string{"/denied", "/stat"} {
		rec := do(t, app, http.MethodGet, path)
		assertStatus(t, rec, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "/srv/private") || strings.Contains(rec.Body.String(), "disk on fire") {
			t.Errorf("%s: the body discloses the cause: %s", path, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "permission denied") || !strings.Contains(logs.String(), "disk on fire") {
		t.Errorf("the causes were not logged: %s", logs.String())
	}
	assertStatus(t, do(t, app, http.MethodGet, "/nofs"), http.StatusInternalServerError)
	if !strings.Contains(logs.String(), "with no FS") {
		t.Errorf("the missing FS was not logged: %s", logs.String())
	}
}

// http.ServeContent answers ranges and the conditional headers.
func TestFileResponseAnswersRangesAndConditions(t *testing.T) {
	t.Parallel()
	app := fileApp(t, fileFS(), nil)
	request := func(headers ...string) *httptest.ResponseRecorder {
		req, _ := http.NewRequest(http.MethodGet, "/files/docs/digits.txt", nil)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		return doRequest(t, app, req)
	}

	rec := request("Range", "bytes=2-5")
	assertStatus(t, rec, http.StatusPartialContent)
	if rec.Body.String() != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Errorf("range = %q with %q, want 2345 and bytes 2-5/10", rec.Body.String(), rec.Header().Get("Content-Range"))
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("a range's Content-Type = %q, want the file's", got)
	}

	rec = request("Range", "bytes=0-0,8-9")
	assertStatus(t, rec, http.StatusPartialContent)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "multipart/byteranges") {
		t.Errorf("two ranges: Content-Type = %q, want multipart/byteranges", rec.Header().Get("Content-Type"))
	}

	assertStatus(t, request("Range", "bytes=50-60"), http.StatusRequestedRangeNotSatisfiable)

	rec = request("If-Modified-Since", fileModTime.Format(http.TimeFormat))
	assertStatus(t, rec, http.StatusNotModified)
	if rec.Body.Len() != 0 {
		t.Errorf("a 304 carried %q", rec.Body.String())
	}
	assertStatus(t, request("If-Unmodified-Since", fileModTime.Add(-time.Hour).Format(http.TimeFormat)), http.StatusPreconditionFailed)

	rec = request("Range", "bytes=0-1", "If-Range", fileModTime.Add(-time.Hour).Format(http.TimeFormat))
	assertStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "0123456789" {
		t.Errorf("an If-Range that does not match = %q, want the whole file", rec.Body.String())
	}
}

func TestFileResponseAnswersHead(t *testing.T) {
	t.Parallel()
	files := &closeCountingFS{FS: fileFS()}
	res := newWireServer(t, fileApp(t, files, nil)).do(t, http.MethodHead, "/files/docs/digits.txt")
	if res.StatusCode != http.StatusOK || res.ContentLength != 10 || len(res.Data) != 0 {
		t.Errorf("HEAD = %d with length %d and %d bytes, want 200, 10 and none", res.StatusCode, res.ContentLength, len(res.Data))
	}
	files.assertAllClosed(t)
}

// A filesystem whose files cannot seek, such as a zip archive, is served
// from memory up to a bound, with ranges, and streamed past it, without.
func TestFileResponseFromAFilesystemThatCannotSeek(t *testing.T) {
	t.Parallel()
	large := bytes.Repeat([]byte("L"), maxBufferedFrontendFile+10)
	files := &closeCountingFS{FS: unseekableFS{fstest.MapFS{
		"small.txt": {Data: []byte("0123456789"), ModTime: fileModTime},
		"large.bin": {Data: large, ModTime: fileModTime},
	}}}
	app := fileApp(t, files, nil)

	req, _ := http.NewRequest(http.MethodGet, "/files/small.txt", nil)
	req.Header.Set("Range", "bytes=1-3")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusPartialContent)
	if rec.Body.String() != "123" {
		t.Errorf("a range of an unseekable file = %q, want 123", rec.Body.String())
	}

	server := newWireServer(t, app)
	res := server.do(t, http.MethodGet, "/files/large.bin", "Range", "bytes=0-1")
	if res.StatusCode != http.StatusOK || res.ReadErr != nil || !bytes.Equal(res.Data, large) {
		t.Errorf("a large unseekable file = %d with %d bytes (%v), want 200 and the whole file", res.StatusCode, len(res.Data), res.ReadErr)
	}
	if res.ContentLength != int64(len(large)) {
		t.Errorf("Content-Length = %d, want the size the filesystem reported", res.ContentLength)
	}
	files.assertAllClosed(t)
}

// A file that cannot be read once its header is out aborts the connection,
// and one that fails before anything is sent is an ordinary error.
func TestFileResponseThatFailsToRead(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/early", func(ctx *Context, _ Empty) (FileResponse, error) {
		return FileResponse{FS: failingReadFS{size: 10, data: []byte("abc")}, Name: "x.txt"}, nil
	})
	app.Get("/late", func(ctx *Context, _ Empty) (FileResponse, error) {
		data := bytes.Repeat([]byte("d"), maxBufferedFrontendFile+100)
		return FileResponse{FS: failingReadFS{size: int64(len(data)) + 100, data: data}, Name: "x.bin"}, nil
	})
	assertStatus(t, do(t, app, http.MethodGet, "/early"), http.StatusInternalServerError)
	if res, err := newWireServer(t, app).try(http.MethodGet, "/late"); !failedTransfer(res, err) {
		t.Errorf("a file that failed part way was read cleanly: %d bytes", len(res.Data))
	}
}

func TestFileResponseHonoursAStatusOtherThan200(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/gone", func(ctx *Context, _ Empty) (FileResponse, error) {
		ctx.SetStatus(http.StatusGone)
		return FileResponse{FS: fileFS(), Name: "docs/digits.txt"}, nil
	})
	app.Get("/nothing", func(ctx *Context, _ Empty) (FileResponse, error) {
		return FileResponse{FS: fileFS(), Name: "docs/digits.txt"}, nil
	}, Status(http.StatusNoContent))
	req, _ := http.NewRequest(http.MethodGet, "/gone", nil)
	req.Header.Set("Range", "bytes=0-1")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusGone)
	if rec.Body.String() != "0123456789" || rec.Header().Get("Content-Length") != "10" {
		t.Errorf("410 page = %q with length %q, want the whole file", rec.Body.String(), rec.Header().Get("Content-Length"))
	}
	rec = do(t, app, http.MethodGet, "/nothing")
	assertStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Errorf("a 204 carried %q", rec.Body.String())
	}
}

// Every way a request can end closes the file it opened.
func TestFileResponseAlwaysClosesTheFile(t *testing.T) {
	t.Parallel()
	files := &closeCountingFS{FS: fileFS()}
	app := fileApp(t, files, nil)
	for _, req := range []struct{ method, path, header, value string }{
		{http.MethodGet, "/files/docs/digits.txt", "", ""},
		{http.MethodHead, "/files/docs/digits.txt", "", ""},
		{http.MethodGet, "/files/docs/digits.txt", "Range", "bytes=1-2"},
		{http.MethodGet, "/files/docs/digits.txt", "Range", "bytes=90-99"},
		{http.MethodGet, "/files/docs/digits.txt", "If-Modified-Since", fileModTime.Format(http.TimeFormat)},
		{http.MethodGet, "/files/docs/sub", "", ""},
		{http.MethodGet, "/files/docs/pipe", "", ""},
	} {
		r, _ := http.NewRequest(req.method, req.path, nil)
		if req.header != "" {
			r.Header.Set(req.header, req.value)
		}
		doRequest(t, app, r)
	}
	if files.opened() == 0 {
		t.Fatal("nothing was opened")
	}
	files.assertAllClosed(t)
}

func TestFileResponseOffersADownload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*FileResponse)
		want string
	}{
		{"inline by default", nil, ""},
		{"download takes the file's name", func(o *FileResponse) { o.Download = true }, `attachment; filename="guide.pdf"`},
		{"a name of its own", func(o *FileResponse) { o.Download, o.Filename = true, "Guide 2026.pdf" }, `attachment; filename="Guide 2026.pdf"`},
		{"a name without a download", func(o *FileResponse) { o.Filename = "guide.pdf" }, `inline; filename="guide.pdf"`},
		{"a name outside ASCII", func(o *FileResponse) { o.Download, o.Filename = true, "r\xc3\xa9sum\xc3\xa9 \xe2\x82\xac.pdf" },
			`attachment; filename="r_sum_ _.pdf"; filename*=UTF-8''r%C3%A9sum%C3%A9%20%E2%82%AC.pdf`},
	} {
		app := fileApp(t, fileFS(), tc.edit)
		if got := do(t, app, http.MethodGet, "/files/docs/guide.pdf").Header().Get("Content-Disposition"); got != tc.want {
			t.Errorf("%s: Content-Disposition = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The filename is the one value of these headers a client often chose, when
// it uploaded the file. Nothing in it may end the header, close the quoted
// string, add a parameter or disguise the extension.
func TestContentDispositionResistsInjection(t *testing.T) {
	t.Parallel()
	for filename, want := range map[string]string{
		"report.pdf":                        `attachment; filename="report.pdf"`,
		"a\r\nSet-Cookie: x=1.pdf":          `attachment; filename="aSet-Cookie: x=1.pdf"`,
		"a\nb.pdf":                          `attachment; filename="ab.pdf"`,
		"a\x00b.pdf":                        `attachment; filename="ab.pdf"`,
		`x.pdf"; filename*=UTF-8''evil.exe`: `attachment; filename="x.pdf; filename*=UTF-8''evil.exe"`,
		`a\"b.pdf`:                          `attachment; filename="a_b.pdf"`,
		"../../etc/passwd":                  `attachment; filename=".._.._etc_passwd"`,
		`..\..\windows\win.ini`:             `attachment; filename=".._.._windows_win.ini"`,
		"invoice\xe2\x80\xaefdp.exe":        `attachment; filename="invoicefdp.exe"`,
		"a\xe2\x80\x8bb.pdf":                `attachment; filename="ab.pdf"`,
		"a\xe2\x80\xa8b.pdf":                `attachment; filename="ab.pdf"`,
		"a\xc2\x85b.pdf":                    `attachment; filename="ab.pdf"`,
		"\xff\xfeab.pdf":                    `attachment; filename="ab.pdf"`,
		"  spaced.pdf  ":                    `attachment; filename="spaced.pdf"`,
		"100%25.pdf":                        `attachment; filename="100_25.pdf"; filename*=UTF-8''100%2525.pdf`,
		"\r\n":                              "attachment",
		"":                                  "attachment",
	} {
		got := contentDisposition(true, filename)
		if got != want {
			t.Errorf("contentDisposition(%q) = %q, want %q", filename, got, want)
		}
		if strings.ContainsAny(got, "\r\n\x00") || strings.Count(got, `"`) > 2 {
			t.Errorf("contentDisposition(%q) = %q, which a header cannot carry safely", filename, got)
		}
	}
	if got := contentDisposition(false, ""); got != "" {
		t.Errorf("an inline response with no name = %q, want no header", got)
	}
}

// The cut never splits a character: one that would cross the bound is left
// out whole.
func TestCleanFilenameCutsOnACharacterBoundary(t *testing.T) {
	t.Parallel()
	name := cleanFilename(strings.Repeat("a", maxDispositionName-1) + "\xe2\x82\xac.pdf")
	if name != strings.Repeat("a", maxDispositionName-1) {
		t.Errorf("cleanFilename kept %d bytes ending %q, want the ASCII prefix alone", len(name), name[len(name)-4:])
	}
}

func TestContentDispositionBoundsTheName(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("\xe2\x82\xac", 1000) + ".pdf"
	got := contentDisposition(true, long)
	name := cleanFilename(long)
	if len(name) > maxDispositionName || !strings.HasPrefix(long, name) {
		t.Errorf("cleanFilename kept %d bytes, want at most %d and a whole number of characters", len(name), maxDispositionName)
	}
	if len(got) > 4*maxDispositionName {
		t.Errorf("the header is %d bytes, want it bounded by the name", len(got))
	}
}

// os.DirFS follows a symbolic link out of its directory, which is why the
// documentation sends a handler to os.Root; the root refuses the same link.
func TestFileResponseSymlinkEscape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	public := filepath.Join(dir, "public")
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("the secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(public, "ok.txt"), []byte("public"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(public, "leak.txt")); err != nil {
		t.Skipf("symbolic links are not available here: %v", err)
	}
	root, err := os.OpenRoot(public)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	followed := do(t, fileApp(t, os.DirFS(public), nil), http.MethodGet, "/files/leak.txt")
	if followed.Body.String() != "the secret" {
		t.Logf("os.DirFS did not follow the link here (%d); the platform refuses it already", followed.Code)
	}
	rooted := fileApp(t, root.FS(), nil)
	if rec := do(t, rooted, http.MethodGet, "/files/leak.txt"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("os.Root: %d %q, want the link refused with a 404", rec.Code, rec.Body.String())
	}
	if rec := do(t, rooted, http.MethodGet, "/files/ok.txt"); rec.Body.String() != "public" {
		t.Errorf("os.Root: %d %q, want the file inside served", rec.Code, rec.Body.String())
	}
}

// AutoETag leaves a file to ServeContent, and a guarded route's file is
// private to caches like anything else it answers.
func TestFileResponseIsNotAutoTaggedAndStaysPrivate(t *testing.T) {
	t.Parallel()
	app := fileApp(t, fileFS(), nil, AutoETag(), WithDependencies(func(ctx *Context) error { return nil }))
	rec := do(t, app, http.MethodGet, "/files/docs/digits.txt")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("ETag = %q, want none on a file", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != privateCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, privateCacheControl)
	}
}

// A compressed file loses Accept-Ranges, and a range is still answered from
// the file as it is.
func TestFileResponseWithCompression(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("compress me ", 500)
	files := fstest.MapFS{"big.txt": {Data: []byte(text), ModTime: fileModTime}}
	app := fileApp(t, files, nil)
	app.Use(Compress(CompressionOptions{}))
	server := newWireServer(t, app)

	res := server.do(t, http.MethodGet, "/files/big.txt", "Accept-Encoding", "gzip")
	if res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Accept-Ranges") != "" {
		t.Errorf("encoding %q and Accept-Ranges %q, want gzip and none", res.Header.Get("Content-Encoding"), res.Header.Get("Accept-Ranges"))
	}
	res = server.do(t, http.MethodGet, "/files/big.txt", "Accept-Encoding", "gzip", "Range", "bytes=0-6")
	if res.StatusCode != http.StatusPartialContent || string(res.Data) != "compres" || res.Header.Get("Content-Encoding") != "" {
		t.Errorf("a range = %d %q encoded %q, want 206 with the uncompressed bytes", res.StatusCode, res.Data, res.Header.Get("Content-Encoding"))
	}
}

// closeCountingFS counts the files opened and closed through it.
type closeCountingFS struct {
	fs.FS
	mu     sync.Mutex
	opens  int
	closes int
}

func (c *closeCountingFS) Open(name string) (fs.File, error) {
	file, err := c.FS.Open(name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	if seeker, ok := file.(io.ReadSeeker); ok {
		return &closeCountedSeeker{closeCountedFile{File: file, fs: c}, seeker}, nil
	}
	return &closeCountedFile{File: file, fs: c}, nil
}

func (c *closeCountingFS) opened() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

func (c *closeCountingFS) assertAllClosed(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opens != c.closes {
		t.Errorf("%d files were opened and %d closed, want every one closed once", c.opens, c.closes)
	}
}

type closeCountedFile struct {
	fs.File
	fs *closeCountingFS
}

func (f *closeCountedFile) Close() error {
	f.fs.mu.Lock()
	f.fs.closes++
	f.fs.mu.Unlock()
	return f.File.Close()
}

type closeCountedSeeker struct {
	closeCountedFile
	seeker io.ReadSeeker
}

func (f *closeCountedSeeker) Seek(offset int64, whence int) (int64, error) {
	return f.seeker.Seek(offset, whence)
}

// unseekableFS hides Seek from the files of the filesystem it wraps, as a zip
// archive's files do.
type unseekableFS struct{ fs.FS }

func (u unseekableFS) Open(name string) (fs.File, error) {
	file, err := u.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return struct{ fs.File }{file}, nil
}

// failingFS fails to open, or to describe what it opened.
type failingFS struct {
	openErr error
	statErr error
}

func (f failingFS) Open(name string) (fs.File, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return failingFile{f.statErr}, nil
}

type failingFile struct{ statErr error }

func (f failingFile) Stat() (fs.FileInfo, error) { return nil, f.statErr }
func (failingFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (failingFile) Close() error                 { return nil }

// failingReadFS serves one file that cannot seek, claims size bytes, and
// fails once data runs out, or ends there when eof is set.
type failingReadFS struct {
	size int64
	data []byte
	eof  bool
}

func (b failingReadFS) Open(name string) (fs.File, error) {
	return &failingReadFile{info: failingReadInfo{name: name, size: b.size}, rest: b.data, eof: b.eof}, nil
}

type failingReadFile struct {
	info failingReadInfo
	rest []byte
	eof  bool
}

func (f *failingReadFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *failingReadFile) Close() error               { return nil }
func (f *failingReadFile) Read(p []byte) (int, error) {
	if len(f.rest) == 0 && f.eof {
		return 0, io.EOF
	}
	if len(f.rest) == 0 {
		return 0, errors.New("the disk went away")
	}
	n := copy(p, f.rest)
	f.rest = f.rest[n:]
	return n, nil
}

type failingReadInfo struct {
	name string
	size int64
}

func (i failingReadInfo) Name() string       { return i.name }
func (i failingReadInfo) Size() int64        { return i.size }
func (i failingReadInfo) Mode() fs.FileMode  { return 0o644 }
func (i failingReadInfo) ModTime() time.Time { return fileModTime }
func (i failingReadInfo) IsDir() bool        { return false }
func (i failingReadInfo) Sys() any           { return nil }

func TestFileResponseAfterTheHandlerWroteIsIgnored(t *testing.T) {
	t.Parallel()
	files := &closeCountingFS{FS: fileFS()}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (FileResponse, error) {
		_, _ = ctx.ResponseWriter().Write([]byte("mine"))
		return FileResponse{FS: files, Name: "docs/digits.txt"}, nil
	})
	if rec := do(t, app, http.MethodGet, "/x"); rec.Body.String() != "mine" {
		t.Errorf("body = %q, want the handler's own", rec.Body.String())
	}
	if files.opened() != 0 {
		t.Error("a file was opened for a response that was already written")
	}
}

// A size the filesystem reported that the file has already outgrown is not
// promised to the client, which is sent the file chunked instead.
func TestFileResponseDoesNotPromiseASizeTheFileOutgrew(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("g"), maxBufferedFrontendFile+100)
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (FileResponse, error) {
		return FileResponse{FS: failingReadFS{size: 10, data: data, eof: true}, Name: "x.bin"}, nil
	})
	res := newWireServer(t, app).do(t, http.MethodGet, "/x")
	if res.ReadErr != nil || len(res.Data) != len(data) || res.ContentLength != -1 {
		t.Errorf("got %d bytes (%v) with length %d, want every byte, chunked", len(res.Data), res.ReadErr, res.ContentLength)
	}
}
