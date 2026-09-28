package muzak

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// uploadTree writes a directory the way an upload handler fills one: a file
// stored under a generated name with no extension, and files a client named,
// holding markup that runs script when a browser opens it.
func uploadTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"5f1c2a":      `<html><script>fetch("/api/me")</script>`,
		"avatar.svg":  `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"page.html":   `<script>alert(1)</script>`,
		"feed.xml":    `<?xml version="1.0"?><feed/>`,
		"photo.png":   "\x89PNG\r\n\x1a\n",
		"notes.txt":   "plain",
		"archive.zzq": `<html><script>alert(1)</script>`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestStaticServesUploadedMarkupInert is the regression test for a static
// mount over uploads serving markup a client wrote as active content: a file
// with no extension was sniffed and declared text/html, and an .svg was served
// inline, both from the application's origin with nothing to stop the script.
func TestStaticServesUploadedMarkupInert(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Static("/uploads", StaticOptions{Dir: uploadTree(t)})
	mustBuild(t, app)

	for target, want := range map[string]struct {
		contentType string
		sandboxed   bool
	}{
		"/uploads/5f1c2a":      {"application/octet-stream", false},
		"/uploads/archive.zzq": {"application/octet-stream", false},
		"/uploads/avatar.svg":  {"image/svg+xml", true},
		"/uploads/page.html":   {"text/html; charset=utf-8", true},
		"/uploads/feed.xml":    {contentTypeFor("feed.xml"), true},
		"/uploads/photo.png":   {"image/png", false},
		"/uploads/notes.txt":   {"text/plain; charset=utf-8", false},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			rec := do(t, app, method, target)
			assertStatus(t, rec, http.StatusOK)
			if got := rec.Header().Get("Content-Type"); got != want.contentType {
				t.Errorf("%s %s: Content-Type %q, want %q", method, target, got, want.contentType)
			}
			policy := rec.Header().Values("Content-Security-Policy")
			if sandboxed := slices.Contains(policy, "sandbox"); sandboxed != want.sandboxed {
				t.Errorf("%s %s: Content-Security-Policy %q, want sandboxed %v", method, target, policy, want.sandboxed)
			}
		}
	}
}

// TestStaticAllowActiveContent checks the opt-out: a site of pages served
// from a static mount gets its documents back as the browser expects them.
func TestStaticAllowActiveContent(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Static("/site", StaticOptions{Dir: uploadTree(t), AllowActiveContent: true})
	mustBuild(t, app)

	for _, target := range []string{"/site/page.html", "/site/avatar.svg"} {
		rec := do(t, app, "GET", target)
		assertStatus(t, rec, http.StatusOK)
		if policy := rec.Header().Values("Content-Security-Policy"); len(policy) != 0 {
			t.Errorf("GET %s: Content-Security-Policy %q with AllowActiveContent set", target, policy)
		}
	}
	// Opting in to active content is not opting in to sniffing.
	rec := do(t, app, "GET", "/site/5f1c2a")
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("extension-less file served as %q", got)
	}
}

// TestStaticSandboxKeepsEarlierPolicy checks that the sandbox is added beside
// a policy a middleware already set, which a browser enforces together, rather
// than replacing it with something looser.
func TestStaticSandboxKeepsEarlierPolicy(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "default-src 'self'")
			next.ServeHTTP(w, r)
		})
	})
	app.Static("/uploads", StaticOptions{Dir: uploadTree(t)})
	mustBuild(t, app)

	policy := do(t, app, "GET", "/uploads/avatar.svg").Header().Values("Content-Security-Policy")
	if !slices.Equal(policy, []string{"default-src 'self'", "sandbox"}) {
		t.Errorf("Content-Security-Policy %q, want the middleware's policy and the sandbox", policy)
	}
}

// TestFrontendDocumentsAreNotSandboxed checks that a frontend's own documents
// keep working: its index.html, its fallback and its error page are the
// application, and sandboxing them would turn its scripts off.
func TestFrontendDocumentsAreNotSandboxed(t *testing.T) {
	t.Parallel()
	modified := time.Unix(1700000000, 0)
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{FS: fstest.MapFS{
		"index.html": {Data: []byte("<h1>app</h1>"), ModTime: modified},
		"logo.svg":   {Data: []byte("<svg/>"), ModTime: modified},
		"LICENSE":    {Data: []byte("<html>not a page</html>"), ModTime: modified},
	}})
	app.Frontend("/site", FrontendOptions{FS: fstest.MapFS{
		"404.html": {Data: []byte("<h1>missing</h1>"), ModTime: modified},
	}})
	mustBuild(t, app)

	for target, want := range map[string]struct {
		status      int
		contentType string
	}{
		"/":             {http.StatusOK, "text/html; charset=utf-8"},
		"/logo.svg":     {http.StatusOK, "image/svg+xml"},
		"/account/page": {http.StatusOK, "text/html; charset=utf-8"},
		"/site/missing": {http.StatusNotFound, "text/html; charset=utf-8"},
		// A frontend file with no extension is not sniffed either.
		"/LICENSE": {http.StatusOK, "application/octet-stream"},
	} {
		rec := doRequest(t, app, withAccept(target, "text/html"))
		assertStatus(t, rec, want.status)
		if got := rec.Header().Get("Content-Type"); got != want.contentType {
			t.Errorf("GET %s: Content-Type %q, want %q", target, got, want.contentType)
		}
		if policy := rec.Header().Values("Content-Security-Policy"); len(policy) != 0 {
			t.Errorf("GET %s: frontend document served with Content-Security-Policy %q", target, policy)
		}
	}
}

func TestIsActiveContent(t *testing.T) {
	t.Parallel()
	for contentType, want := range map[string]bool{
		"text/html; charset=utf-8":         true,
		"TEXT/HTML":                        true,
		"image/svg+xml":                    true,
		"application/xhtml+xml":            true,
		"application/xml":                  true,
		"text/xml; charset=utf-8":          true,
		"text/xsl":                         true,
		"application/xslt+xml":             true,
		" application/rss+xml ; q=1":       true,
		"image/png":                        false,
		"text/plain; charset=utf-8":        false,
		"application/json":                 false,
		"application/octet-stream":         false,
		"text/javascript; charset=utf-8":   false,
		"application/xml-dtd":              false,
		"application/vnd.example+xml-like": false,
		"":                                 false,
	} {
		if got := isActiveContent(contentType); got != want {
			t.Errorf("isActiveContent(%q) = %v, want %v", contentType, got, want)
		}
	}
}

// withAccept returns a GET request for target carrying an Accept header.
func withAccept(target, accept string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Accept", accept)
	return req
}

// assertCacheControl fails the test unless the response carries exactly the
// Cache-Control value given, where the empty string means none.
func assertCacheControl(t *testing.T, what string, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rec.Header().Values("Cache-Control"); (want == "" && len(got) != 0) || (want != "" && !slices.Equal(got, []string{want})) {
		t.Errorf("%s: Cache-Control %q, want %q", what, got, want)
	}
}

// TestGuardedMountIsPrivate is the regression test for a guarded mount whose
// files a shared cache could keep: they were sent with Last-Modified and no
// Cache-Control, and a request authenticated by a cookie is not one a shared
// cache is told to leave alone.
func TestGuardedMountIsPrivate(t *testing.T) {
	t.Parallel()
	modified := time.Now().Add(-30 * 24 * time.Hour)
	files := fstest.MapFS{
		"index.html":  {Data: []byte("<p>app</p>"), ModTime: modified},
		"invoice.pdf": {Data: []byte("%PDF alice"), ModTime: modified},
	}
	pages := fstest.MapFS{"404.html": {Data: []byte("<p>missing</p>"), ModTime: modified}}

	// A provider inherited from the router the mounts are registered on.
	account := NewRouter(Needs(currentMountUser))
	account.Static("/files", StaticOptions{FS: files})
	account.Frontend("/app", FrontendOptions{FS: files})
	account.Frontend("/pages", FrontendOptions{FS: pages})
	// A guard inherited from the router's options.
	keyed := NewRouter(WithDependencies(RequireHeaderToken("X-API-Key", "k3y")))
	keyed.Static("/files", StaticOptions{FS: files})

	app := New(quietOptions())
	app.Include(account, WithPrefix("/account"))
	app.Include(keyed, WithPrefix("/keyed"))
	app.Static("/public", StaticOptions{FS: files})
	app.Frontend("/", FrontendOptions{FS: files})
	mustBuild(t, app)

	for _, tc := range []struct {
		target string
		status int
	}{
		{"/account/files/invoice.pdf", http.StatusOK},
		{"/account/app/", http.StatusOK},
		{"/account/app/settings", http.StatusOK},
		{"/account/pages/missing", http.StatusNotFound},
	} {
		req := withToken(tc.target)
		req.Header.Set("Accept", "text/html")
		rec := doRequest(t, app, req)
		assertStatus(t, rec, tc.status)
		assertCacheControl(t, "GET "+tc.target, rec, "private, no-cache")
	}

	req := withToken("/keyed/files/invoice.pdf")
	req.Header.Set("X-API-Key", "k3y")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertCacheControl(t, "file behind a guard", rec, "private, no-cache")
	if rec.Header().Get("Last-Modified") == "" {
		t.Error("guarded file lost its Last-Modified, which no-cache revalidates with")
	}

	// A mount nothing guards leaves caching to the application.
	for _, target := range []string{"/public/invoice.pdf", "/index.html", "/elsewhere"} {
		rec := doRequest(t, app, withAccept(target, "text/html"))
		assertStatus(t, rec, http.StatusOK)
		assertCacheControl(t, "GET "+target, rec, "")
	}
}

// TestAppGuardedMountIsPrivate checks a guard declared on the application
// itself, which every mount registered on it inherits, and that a public
// value a middleware chose does not survive on a guarded file.
func TestAppGuardedMountIsPrivate(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), Needs(currentMountUser))
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=31536000")
			next.ServeHTTP(w, r)
		})
	})
	app.Static("/files", StaticOptions{FS: fstest.MapFS{"a.txt": {Data: []byte("a")}}})
	mustBuild(t, app)

	rec := doRequest(t, app, withToken("/files/a.txt"))
	assertStatus(t, rec, http.StatusOK)
	assertCacheControl(t, "file behind an application guard", rec, "private, no-cache")
}

// varyValues returns every field a response's Vary headers name, in order.
func varyValues(rec *httptest.ResponseRecorder) []string {
	var fields []string
	for _, value := range rec.Header().Values("Vary") {
		for field := range strings.SplitSeq(value, ",") {
			fields = append(fields, strings.TrimSpace(field))
		}
	}
	return fields
}

// TestFallbackVariesOnAccept is the regression test for the single page
// application fallback answering one URL with 200 HTML or a 404 depending on
// Accept, without saying so: a cache that stored the 404 a script's request
// got served it to the next navigation, and the other way round.
func TestFallbackVariesOnAccept(t *testing.T) {
	t.Parallel()
	modified := time.Unix(1700000000, 0)
	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("declared") {
				w.Header().Set("Vary", "Origin, accept")
			}
			next.ServeHTTP(w, r)
		})
	})
	app.Frontend("/", FrontendOptions{FS: fstest.MapFS{
		"index.html": {Data: []byte("<h1>app</h1>"), ModTime: modified},
		"app.js":     {Data: []byte("app()"), ModTime: modified},
	}})
	app.Frontend("/pages", FrontendOptions{FS: fstest.MapFS{
		"index.html": {Data: []byte("<h1>home</h1>"), ModTime: modified},
		"404.html":   {Data: []byte("<h1>missing</h1>"), ModTime: modified},
	}})
	app.Frontend("/plain", FrontendOptions{NoFallback: true, FS: fstest.MapFS{
		"index.html": {Data: []byte("<h1>plain</h1>"), ModTime: modified},
	}})
	app.Static("/static", StaticOptions{FS: fstest.MapFS{"a.css": {Data: []byte("a{}")}}})
	mustBuild(t, app)

	for _, tc := range []struct {
		target, accept string
		status         int
	}{
		{"/account/settings", "text/html", http.StatusOK},
		{"/account/settings", "*/*", http.StatusNotFound},
		{"/", "text/html", http.StatusOK},
		{"/app.js", "*/*", http.StatusOK},
		{"/app.js", "text/html", http.StatusOK},
	} {
		rec := doRequest(t, app, withAccept(tc.target, tc.accept))
		assertStatus(t, rec, tc.status)
		if got := varyValues(rec); !slices.Equal(got, []string{"Accept"}) {
			t.Errorf("GET %s with Accept %s: Vary %q, want Accept", tc.target, tc.accept, got)
		}
	}

	// A field the response already names is not repeated.
	rec := doRequest(t, app, withAccept("/account/settings?declared", "text/html"))
	if got := varyValues(rec); !slices.Equal(got, []string{"Origin", "accept"}) {
		t.Errorf("Vary %q, want the declared fields unchanged", got)
	}

	// A mount whose answer does not depend on Accept does not say it does.
	for _, target := range []string{"/pages/", "/pages/missing", "/plain/", "/plain/missing", "/static/a.css"} {
		for _, accept := range []string{"text/html", "*/*"} {
			if got := varyValues(doRequest(t, app, withAccept(target, accept))); len(got) != 0 {
				t.Errorf("GET %s with Accept %s: Vary %q, want none", target, accept, got)
			}
		}
	}
}
