package muzak

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
