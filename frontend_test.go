package badele

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// buildOutput writes a directory shaped the way a frontend build tool leaves
// one, and returns its path.
func buildOutput(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	return dir
}

// spa is the output of a single page application build.
var spa = map[string]string{
	"index.html":         "<!doctype html><div id=root></div>",
	"assets/app.js":      "console.log('app')",
	"assets/app.css":     "body{margin:0}",
	"favicon.ico":        "icon",
	"nested/index.html":  "<!doctype html>nested",
	".well-known/x.json": `{"ok":true}`,
}

// navigate sends the request a browser makes when following a link.
func navigate(t *testing.T, app *App, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	return doRequest(t, app, req)
}

// fetch sends the request a script or stylesheet makes.
func fetch(t *testing.T, app *App, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "*/*")
	return doRequest(t, app, req)
}

func TestFrontendServesTheBuildOutput(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: buildOutput(t, spa)})
	built := mustBuild(t, app)

	cases := []struct {
		target      string
		wantBody    string
		contentType string
	}{
		{target: "/", wantBody: "<div id=root>", contentType: "text/html"},
		{target: "/index.html", wantBody: "<div id=root>", contentType: "text/html"},
		{target: "/assets/app.js", wantBody: "console.log", contentType: "javascript"},
		{target: "/assets/app.css", wantBody: "margin:0", contentType: "css"},
		// A directory is served by the index.html inside it, the way every
		// static host does it.
		{target: "/nested/", wantBody: "nested", contentType: "text/html"},
		{target: "/nested", wantBody: "nested", contentType: "text/html"},
		// A dotted directory is ordinary content, and hiding it would break
		// the one every ACME client and app manifest looks for.
		{target: "/.well-known/x.json", wantBody: `"ok":true`, contentType: "json"},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			rec := fetch(t, built, tc.target)
			assertStatus(t, rec, http.StatusOK)
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
				t.Errorf("Content-Type = %q, want it to mention %q", got, tc.contentType)
			}
		})
	}
}

type pingOut struct {
	Pong bool `json:"pong"`
}

func TestFrontendNeverShadowsARoute(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	// The frontend build contains a file at the same path as a route, which is
	// what an "api" directory in the build output would produce.
	files := map[string]string{"index.html": "app", "api/ping": "static file"}
	app.Frontend("/", FrontendOptions{Dir: buildOutput(t, files)})
	app.Get("/api/ping", func(ctx *Context, _ Empty) (pingOut, error) {
		return pingOut{Pong: true}, nil
	})
	built := mustBuild(t, app)

	rec := fetch(t, built, "/api/ping")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"pong":true}`)

	// A method the route does not answer stays a 405 rather than falling
	// through to the file: the path is a route's, and saying otherwise would
	// hide the mistake.
	rec = doRequest(t, built, httptest.NewRequest(http.MethodPost, "/api/ping", nil))
	assertStatus(t, rec, http.StatusMethodNotAllowed)
}

func TestFrontendFallsBackForClientSideRouting(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: buildOutput(t, spa)})
	built := mustBuild(t, app)

	// A path the client-side router knows about and the build has no file for.
	rec := navigate(t, built, "/dashboard/settings")
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "<div id=root>") {
		t.Errorf("body = %q, want the application document", rec.Body.String())
	}

	// A missing asset is a missing asset. Answering it with HTML would turn a
	// 404 into a parse error somewhere further from the cause.
	rec = fetch(t, built, "/assets/missing.js")
	assertStatus(t, rec, http.StatusNotFound)
	if strings.Contains(rec.Body.String(), "<div id=root>") {
		t.Error("a missing script was answered with the application document")
	}

	// Anything that is not a read cannot be answered by a file.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/dashboard/settings", nil)
		req.Header.Set("Accept", "text/html")
		rec := doRequest(t, built, req)
		assertStatus(t, rec, http.StatusNotFound)
	}
}

func TestFrontendServesAStaticNotFoundPage(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"index.html": "home",
		"404.html":   "<!doctype html>not here",
		"about.html": "about",
	}
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: buildOutput(t, files)})
	built := mustBuild(t, app)

	// A build with a 404.html makes a page per file, so a missing path is
	// missing rather than something the client will route.
	rec := navigate(t, built, "/nowhere")
	assertStatus(t, rec, http.StatusNotFound)
	if !strings.Contains(rec.Body.String(), "not here") {
		t.Errorf("body = %q, want the static error page", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	// The page is served, but the status still tells the truth.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 kept", rec.Code)
	}
}

func TestFrontendFallbackIsConfigurable(t *testing.T) {
	t.Parallel()
	files := map[string]string{"index.html": "home", "404.html": "not here", "shell.html": "shell"}

	t.Run("an explicit fallback beats the automatic choice", func(t *testing.T) {
		app := New(quietOptions())
		app.Frontend("/", FrontendOptions{Dir: buildOutput(t, files), Fallback: "shell.html"})
		rec := navigate(t, mustBuild(t, app), "/nowhere")
		assertStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), "shell") {
			t.Errorf("body = %q, want the named fallback rather than 404.html", rec.Body.String())
		}
	})

	t.Run("no fallback at all", func(t *testing.T) {
		app := New(quietOptions())
		app.Frontend("/", FrontendOptions{Dir: buildOutput(t, files), NoFallback: true})
		rec := navigate(t, mustBuild(t, app), "/nowhere")
		assertStatus(t, rec, http.StatusNotFound)
		if strings.Contains(rec.Body.String(), "not here") {
			t.Error("a file was served although the mount asked for none")
		}
		// The ordinary envelope answers instead, so a client sees the same
		// shape it sees everywhere else.
		if got := decodeError(t, rec).Error.Status; got != http.StatusNotFound {
			t.Errorf("envelope status = %d, want 404", got)
		}
	})

	t.Run("a build with neither file", func(t *testing.T) {
		app := New(quietOptions())
		app.Frontend("/", FrontendOptions{Dir: buildOutput(t, map[string]string{"app.js": "x"})})
		rec := navigate(t, mustBuild(t, app), "/nowhere")
		assertStatus(t, rec, http.StatusNotFound)
	})
}

func TestFrontendMountsUnderAPrefix(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	ui := NewRouter()
	ui.Frontend("/", FrontendOptions{Dir: buildOutput(t, spa)})
	app.Include(ui, WithPrefix("/app"))
	app.Get("/api/ping", func(ctx *Context, _ Empty) (pingOut, error) { return pingOut{Pong: true}, nil })
	built := mustBuild(t, app)

	for _, target := range []string{"/app", "/app/", "/app/index.html"} {
		rec := navigate(t, built, target)
		assertStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), "<div id=root>") {
			t.Errorf("%s: body = %q, want the application document", target, rec.Body.String())
		}
	}
	rec := fetch(t, built, "/app/assets/app.js")
	assertStatus(t, rec, http.StatusOK)

	// Nothing outside the prefix is served by this mount.
	rec = navigate(t, built, "/elsewhere")
	assertStatus(t, rec, http.StatusNotFound)
	rec = fetch(t, built, "/api/ping")
	assertStatus(t, rec, http.StatusOK)
}

func TestFrontendPrefersTheMostSpecificMount(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	outer := NewRouter()
	outer.Frontend("/", FrontendOptions{Dir: buildOutput(t, map[string]string{"index.html": "outer"})})
	app.Include(outer)
	admin := NewRouter()
	admin.Frontend("/", FrontendOptions{Dir: buildOutput(t, map[string]string{"index.html": "admin"})})
	app.Include(admin, WithPrefix("/admin"))
	built := mustBuild(t, app)

	// Both mounts cover /admin. The one that says more about the path wins,
	// whichever order they were registered in.
	rec := navigate(t, built, "/admin")
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "admin") {
		t.Errorf("body = %q, want the admin mount", rec.Body.String())
	}
	rec = navigate(t, built, "/anything")
	if !strings.Contains(rec.Body.String(), "outer") {
		t.Errorf("body = %q, want the root mount", rec.Body.String())
	}
}

func TestFrontendServesFromAnEmbeddedFilesystem(t *testing.T) {
	t.Parallel()
	// An embed.FS is rooted at the directory embedded, which is why Dir names
	// a subdirectory of it rather than a directory on disk.
	embedded := fstest.MapFS{
		"dist/index.html":    {Data: []byte("embedded app")},
		"dist/assets/app.js": {Data: []byte("console.log('embedded')")},
	}
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{FS: embedded, Dir: "dist"})
	built := mustBuild(t, app)

	rec := navigate(t, built, "/")
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "embedded app") {
		t.Errorf("body = %q, want the embedded document", rec.Body.String())
	}
	rec = fetch(t, built, "/assets/app.js")
	assertStatus(t, rec, http.StatusOK)
	// A path with no file behind it still falls back, exactly as on disk.
	rec = navigate(t, built, "/deep/link")
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "embedded app") {
		t.Errorf("body = %q, want the fallback from the embedded filesystem", rec.Body.String())
	}
}

func TestFrontendRefusesToLeaveItsDirectory(t *testing.T) {
	t.Parallel()
	dir := buildOutput(t, spa)
	// A secret next to the build output, which is where one usually is.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.env"), []byte("TOKEN=hunter2"), 0o600); err != nil {
		t.Fatalf("writing the secret: %v", err)
	}
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: dir})
	built := mustBuild(t, app)

	escapes := []string{
		"/../secret.env",
		"/assets/../../secret.env",
		"/%2e%2e/secret.env",
		"/..%2fsecret.env",
		"/%2e%2e%2fsecret.env",
	}
	for _, target := range escapes {
		t.Run(target, func(t *testing.T) {
			rec := fetch(t, built, target)
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Fatalf("%s served a file outside the build output", target)
			}
			if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "TOKEN") {
				t.Fatalf("%s leaked content from outside the build output", target)
			}
		})
	}
}

func TestFrontendDoesNotListDirectories(t *testing.T) {
	t.Parallel()
	// assets has no index.html, so there is nothing to serve for it. Listing
	// what is inside would publish the shape of the build.
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: buildOutput(t, spa), NoFallback: true})
	built := mustBuild(t, app)

	rec := fetch(t, built, "/assets/")
	assertStatus(t, rec, http.StatusNotFound)
	if strings.Contains(rec.Body.String(), "app.js") {
		t.Error("the directory contents were listed")
	}
}

func TestFrontendRunsTheGuardsOfItsRouter(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	ui := NewRouter(WithDependencies(func(ctx *Context) error {
		if ctx.Query("token") == "" {
			return NewHTTPError(http.StatusUnauthorized, "a token is required")
		}
		return nil
	}))
	ui.Frontend("/", FrontendOptions{Dir: buildOutput(t, spa)})
	app.Include(ui, WithPrefix("/app"))
	built := mustBuild(t, app)

	// A frontend behind a guard is what protecting one with cookie
	// authentication looks like, so the guard has to run before a file is read.
	rec := navigate(t, built, "/app/")
	assertStatus(t, rec, http.StatusUnauthorized)
	if strings.Contains(rec.Body.String(), "<div id=root>") {
		t.Error("the document was served to an unauthorized request")
	}
	rec = navigate(t, built, "/app/?token=jessica")
	assertStatus(t, rec, http.StatusOK)
}

func TestFrontendAnswersConditionalAndRangeRequests(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: buildOutput(t, spa)})
	built := mustBuild(t, app)

	first := fetch(t, built, "/assets/app.js")
	assertStatus(t, first, http.StatusOK)
	modified := first.Header().Get("Last-Modified")
	if modified == "" {
		t.Fatal("no Last-Modified, so a client cannot revalidate")
	}

	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	req.Header.Set("If-Modified-Since", modified)
	rec := doRequest(t, built, req)
	assertStatus(t, rec, http.StatusNotModified)
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want none for a 304", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	req.Header.Set("Range", "bytes=0-6")
	rec = doRequest(t, built, req)
	assertStatus(t, rec, http.StatusPartialContent)
	if rec.Body.String() != "console" {
		t.Errorf("body = %q, want the requested range", rec.Body.String())
	}
}

func TestFrontendRegistrationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
		bind func(*App)
	}{
		{
			name: "path must be rooted",
			want: "path must begin with",
			bind: func(app *App) { app.Frontend("dist", FrontendOptions{Dir: "dist"}) },
		},
		{
			name: "no source",
			want: "set Dir, FS, or both",
			bind: func(app *App) { app.Frontend("/", FrontendOptions{}) },
		},
		{
			name: "contradictory fallback",
			want: "NoFallback cannot be combined",
			bind: func(app *App) {
				app.Frontend("/", FrontendOptions{Dir: "dist", NoFallback: true, Fallback: "index.html"})
			},
		},
		{
			name: "the build output is not there",
			want: "frontend at \"/\"",
			bind: func(app *App) {
				app.Frontend("/", FrontendOptions{Dir: filepath.Join(os.TempDir(), "badele-no-such-build")})
			},
		},
		{
			name: "a named fallback the build does not contain",
			want: `Fallback file "missing.html"`,
			bind: func(app *App) {
				app.Frontend("/", FrontendOptions{Dir: ".", Fallback: "missing.html"})
			},
		},
		{
			name: "a prefixed mount names itself",
			want: `frontend at "/app"`,
			bind: func(app *App) {
				ui := NewRouter()
				ui.Frontend("/", FrontendOptions{Dir: filepath.Join(os.TempDir(), "badele-no-such-build")})
				app.Include(ui, WithPrefix("/app"))
			},
		},
		{
			name: "a named error page the build does not contain",
			want: `NotFound file "missing.html"`,
			bind: func(app *App) {
				app.Frontend("/", FrontendOptions{Dir: ".", NotFound: "missing.html"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			tc.bind(app)
			if got := buildError(t, app); !strings.Contains(got, tc.want) {
				t.Errorf("build error = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

func TestFrontendCanDeferTheCheck(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "dist")
	logger, buf := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	// The directory is named but will be built later, which is what running
	// the backend before the frontend build looks like.
	app.Frontend("/", FrontendOptions{Dir: dir, SkipCheck: true})
	built := mustBuild(t, app)

	// Until it exists, a request fails rather than the application refusing to
	// start, and the reason is logged rather than sent.
	rec := navigate(t, built, "/")
	assertStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), dir) {
		t.Error("the response named a path on the server's filesystem")
	}
	if !strings.Contains(buf.String(), "could not be opened") {
		t.Errorf("log = %s, want the reason recorded", buf.String())
	}
}

func TestFrontendMatchesOnlyItsOwnPaths(t *testing.T) {
	t.Parallel()
	mount := &frontend{path: "/app"}
	cases := map[string]bool{
		"/app":          true,
		"/app/":         true,
		"/app/assets/x": true,
		"/application":  false,
		"/ap":           false,
		"/":             false,
		"/other/app":    false,
	}
	for target, want := range cases {
		if _, got := mount.matches(target); got != want {
			t.Errorf("matches(%q) = %v, want %v", target, got, want)
		}
	}
	// A mount at the root covers everything, including the root itself.
	root := &frontend{path: ""}
	for _, target := range []string{"/", "/anything", "/deep/link"} {
		if _, ok := root.matches(target); !ok {
			t.Errorf("the root mount did not match %q", target)
		}
	}
}

// readOnlyFS hides the Seeker its files would otherwise offer, which is what a
// filesystem that streams rather than maps its content looks like.
type readOnlyFS struct{ fstest.MapFS }

func (f readOnlyFS) Open(name string) (fs.File, error) {
	file, err := f.MapFS.Open(name)
	if err != nil {
		return nil, err
	}
	return readOnlyFile{file}, nil
}

type readOnlyFile struct{ fs.File }

func TestFrontendServesAFilesystemThatCannotSeek(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	// No Dir, so the filesystem is served from its root. Its files cannot
	// seek, so the content is read into memory to answer at all.
	app.Frontend("/", FrontendOptions{FS: readOnlyFS{fstest.MapFS{
		"index.html":   {Data: []byte("streamed app")},
		"missing.page": {Data: []byte("gone")},
	}}, NotFound: "missing.page"})
	built := mustBuild(t, app)

	rec := navigate(t, built, "/")
	assertStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "streamed app" {
		t.Errorf("body = %q, want the whole file", rec.Body.String())
	}

	// An error page with no extension anyone recognises still has to be
	// described as something rather than sent bare.
	rec = navigate(t, built, "/nowhere")
	assertStatus(t, rec, http.StatusNotFound)
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want the fallback media type", got)
	}
	if rec.Body.String() != "gone" {
		t.Errorf("body = %q, want the error page", rec.Body.String())
	}
}

// brokenFS reports a file through Stat and then fails to deliver it, which is
// what a filesystem does when something changes underneath a running server.
type brokenFS struct {
	fstest.MapFS
	failOpen bool
	failStat bool
	failRead bool
}

func (f brokenFS) Open(name string) (fs.File, error) {
	if f.failOpen {
		return nil, fs.ErrPermission
	}
	file, err := f.MapFS.Open(name)
	if err != nil {
		return nil, err
	}
	return brokenFile{File: file, failStat: f.failStat, failRead: f.failRead}, nil
}

type brokenFile struct {
	fs.File
	failStat bool
	failRead bool
}

func (f brokenFile) Stat() (fs.FileInfo, error) {
	if f.failStat {
		return nil, fs.ErrInvalid
	}
	return f.File.Stat()
}

func (f brokenFile) Read(b []byte) (int, error) {
	if f.failRead {
		return 0, fs.ErrInvalid
	}
	return f.File.Read(b)
}

func TestFrontendSurvivesAFilesystemThatFailsMidRequest(t *testing.T) {
	t.Parallel()
	content := fstest.MapFS{"index.html": {Data: []byte("app")}}

	cases := []struct {
		name  string
		files fs.FS
		want  int
	}{
		// Stat finds the file, so the mount builds and the request is accepted,
		// and only then does reading it fail.
		{name: "cannot be opened", files: brokenFS{MapFS: content, failOpen: true}, want: http.StatusNotFound},
		{name: "cannot be measured", files: brokenFS{MapFS: content, failStat: true}, want: http.StatusNotFound},
		{name: "cannot be read", files: brokenFS{MapFS: content, failRead: true}, want: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.Frontend("/", FrontendOptions{FS: tc.files})
			rec := navigate(t, mustBuild(t, app), "/")
			// The request fails rather than the server panicking or hanging,
			// and nothing about the filesystem reaches the client.
			assertStatus(t, rec, tc.want)
			if strings.Contains(rec.Body.String(), "permission") || strings.Contains(rec.Body.String(), "invalid") {
				t.Errorf("body = %q, want nothing about the filesystem error", rec.Body.String())
			}
		})
	}
}

// assertReadOnly checks that a path served from a filesystem answers a read and
// refuses everything else.
func assertReadOnly(t *testing.T, app *App, target string) {
	t.Helper()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		rec := doRequest(t, app, httptest.NewRequest(method, target, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, target, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s %s: Allow = %q, want \"GET, HEAD\"", method, target, got)
		}
		if rec.Body.Len() > 0 && !strings.Contains(rec.Body.String(), "not allowed") {
			t.Errorf("%s %s answered with %q, want no content of its own", method, target, rec.Body.String())
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := doRequest(t, app, httptest.NewRequest(method, target, nil))
		assertStatus(t, rec, http.StatusOK)
	}
}

func TestFrontendAnswersTheWrongMethodWithAllow(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: buildOutput(t, spa)})
	built := mustBuild(t, app)

	// A file that exists cannot be written to, and answering 404 would claim it
	// is not there. Serving it back for a DELETE would be worse still.
	assertReadOnly(t, built, "/assets/app.js")

	// A path that only the fallback covers is a different matter: nothing is
	// there, so it is missing rather than read-only.
	req := httptest.NewRequest(http.MethodPost, "/dashboard/settings", nil)
	req.Header.Set("Accept", "text/html")
	assertStatus(t, doRequest(t, built, req), http.StatusNotFound)
}
