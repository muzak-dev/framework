package muzak

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// FuzzMountStripPrefix strips a two-segment prefix from any escaped path and
// checks what the handler would be given: a rooted path, whose escaped form
// is the end of the one that arrived, and decodes to the path given.
func FuzzMountStripPrefix(f *testing.F) {
	// "///!" is the input that found a remainder respelled as "/%21".
	for _, seed := range []string{"/a/b", "/a/b/", "/a/b/c%2Fd", "/%61/b//x", "/a", "/", "/a/b/%zz", "/a/b/caf%C3%A9", "///!", "/a/b/(x)*'y'"} {
		f.Add(seed)
	}
	m := &mountPoint{prefix: "/a/b", segments: 2}
	f.Fuzz(func(t *testing.T, escaped string) {
		u, err := url.ParseRequestURI(escaped)
		if err != nil || u.RawQuery != "" || u.Host != "" {
			return
		}
		r := &http.Request{URL: u, RequestURI: escaped}
		stripped := m.stripped(r)
		if stripped == r || stripped.URL == r.URL {
			t.Fatal("the original request or URL was modified rather than copied")
		}
		got := stripped.URL.EscapedPath()
		if !strings.HasPrefix(stripped.URL.Path, "/") || !strings.HasPrefix(got, "/") {
			t.Fatalf("%q was stripped to path %q escaped %q", escaped, stripped.URL.Path, got)
		}
		if got != "/" && !strings.HasSuffix(u.EscapedPath(), got) {
			t.Fatalf("%q was stripped to %q, which is not the end of it", escaped, got)
		}
		if decoded, err := url.PathUnescape(got); err == nil && decoded != stripped.URL.Path {
			t.Fatalf("%q: escaped %q decodes to %q, but the path is %q", escaped, got, decoded, stripped.URL.Path)
		}
		if r.URL.EscapedPath() != u.EscapedPath() || stripped.RequestURI != escaped {
			t.Fatal("stripping changed the request it was given")
		}
	})
}
