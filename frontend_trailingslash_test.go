package muzak

import (
	"net/http"
	"testing"
	"testing/fstest"
)

// A trailing slash names a directory. The resolver dropped it before looking
// the path up, which is right for a directory, served by its index.html with
// or without one, and wrong for a file: /index.html/ and /assets/app.js/
// served the file under a second URL, against which every relative reference
// in it resolves somewhere else. Such a path is now one with no file behind
// it, answered as /index.html/x already was.

func TestFrontendTrailingSlashAfterAFileNamesNothing(t *testing.T) {
	t.Parallel()
	dir := buildOutput(t, map[string]string{
		"index.html":     "<!doctype html>app",
		"page.html":      "<!doctype html>page",
		"assets/app.js":  "console.log('app')",
		"sub/index.html": "<!doctype html>sub",
	})
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: dir})
	app.Frontend("/plain", FrontendOptions{Dir: dir, NoFallback: true})
	app.Static("/static", StaticOptions{Dir: dir})
	mustBuild(t, app)

	tests := []struct {
		name   string
		target string
		html   bool
		status int
		body   string
	}{
		// What was there before still is.
		{"a file", "/assets/app.js", false, http.StatusOK, "console.log('app')"},
		{"a directory with its slash", "/sub/", true, http.StatusOK, "<!doctype html>sub"},
		{"a directory without it", "/sub", true, http.StatusOK, "<!doctype html>sub"},
		{"the root", "/", true, http.StatusOK, "<!doctype html>app"},
		// A file with a slash after it is a miss: a 404 for an asset, and for
		// a navigation the fallback, the application document, rather than the
		// file it named.
		{"an asset with a slash", "/assets/app.js/", false, http.StatusNotFound, ""},
		{"a page with a slash", "/page.html/", true, http.StatusOK, "<!doctype html>app"},
		{"the fallback itself with a slash", "/index.html/", false, http.StatusNotFound, ""},
		// Without a fallback a miss is a 404 whatever was asked for.
		{"a page with a slash, no fallback", "/plain/page.html/", true, http.StatusNotFound, ""},
		{"a file with a slash, static", "/static/page.html/", true, http.StatusNotFound, ""},
		{"a file, static", "/static/page.html", true, http.StatusOK, "<!doctype html>page"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ask := fetch
			if tc.html {
				ask = navigate
			}
			rec := ask(t, app, tc.target)
			if rec.Code != tc.status {
				t.Fatalf("GET %s = %d, want %d", tc.target, rec.Code, tc.status)
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Errorf("GET %s served %q, want %q", tc.target, rec.Body.String(), tc.body)
			}
		})
	}
}

func TestFrontendTrailingSlashAfterAnEmbeddedFileNamesNothing(t *testing.T) {
	t.Parallel()
	// An fs.FS that is not on disk is asked the same question, since what the
	// slash means is decided before the filesystem is.
	files := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html>app")},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{FS: files})
	mustBuild(t, app)
	if rec := fetch(t, app, "/assets/app.js/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /assets/app.js/ = %d, want 404", rec.Code)
	}
}
