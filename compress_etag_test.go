package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Compress weakens a strong entity tag on a body it compresses, because the
// bytes are no longer the ones the tag promised. That has two consequences on
// the way back in. A 304 for the same client must carry the tag in the same
// form, or a cache cannot use it to refresh what it stored. And If-Match is
// compared strongly, so the weak tag the client now holds could never match:
// every conditional write from a client that accepts gzip was refused.

// taggedContent serves a text file under a strong entity tag, through
// http.ServeContent, which answers If-None-Match, If-Match and Range the way
// net/http does for everybody.
func taggedContent(tag string) http.Handler {
	content := strings.Repeat("abcdefghij", 500)
	return Compress(CompressionOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", tag)
		http.ServeContent(w, r, "a.txt", time.Unix(1700000000, 0), strings.NewReader(content))
	}))
}

// conditional sends a GET carrying the given headers.
func conditional(handler http.Handler, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/a.txt", nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCompressWeakensTheTagOfANotModifiedAnswer(t *testing.T) {
	t.Parallel()
	handler := taggedContent(`"v1"`)

	full := conditional(handler, "Accept-Encoding", "gzip")
	if full.Code != http.StatusOK || full.Header().Get("ETag") != `W/"v1"` {
		t.Fatalf("the compressed answer = %d with ETag %q, want 200 with W/\"v1\"", full.Code, full.Header().Get("ETag"))
	}

	// RFC 9111 section 4.3.4: a 304 carrying a strong tag refreshes only the
	// stored responses with that same strong tag, and the one stored here has
	// a weak one, so a strong 304 cannot be used for it at all.
	revalidated := conditional(handler, "Accept-Encoding", "gzip", "If-None-Match", `W/"v1"`)
	if revalidated.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", revalidated.Code)
	}
	if got := revalidated.Header().Get("ETag"); got != `W/"v1"` {
		t.Errorf("the 304's ETag = %q, want W/\"v1\", the form the 200 it revalidates carried", got)
	}
	if got := revalidated.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("the 304's Vary = %q, want it to name Accept-Encoding", got)
	}

	// A client that negotiated nothing was sent the tag as the handler wrote
	// it, and is answered with it the same way.
	identity := conditional(handler, "If-None-Match", `"v1"`)
	if identity.Code != http.StatusNotModified || identity.Header().Get("ETag") != `"v1"` {
		t.Errorf("an uncompressed revalidation = %d with ETag %q, want 304 with the strong tag", identity.Code, identity.Header().Get("ETag"))
	}
	// A tag that was weak already is left as it is.
	weak := conditional(taggedContent(`W/"w1"`), "Accept-Encoding", "gzip", "If-None-Match", `W/"w1"`)
	if weak.Code != http.StatusNotModified || weak.Header().Get("ETag") != `W/"w1"` {
		t.Errorf("a weak tag's revalidation = %d with ETag %q, want 304 with W/\"w1\"", weak.Code, weak.Header().Get("ETag"))
	}
}

func TestCompressRestoresTheStrongTagsOfAnIfMatch(t *testing.T) {
	t.Parallel()
	strong := taggedContent(`"v1"`)

	tests := []struct {
		name    string
		handler http.Handler
		ifMatch string
		want    int
	}{
		// The tag the compressed answer carried names the content the strong
		// tag does, so it matches it.
		{"the weakened tag", strong, `W/"v1"`, http.StatusOK},
		{"the weakened tag among others", strong, `"v0", W/"v1"`, http.StatusOK},
		{"the strong tag", strong, `"v1"`, http.StatusOK},
		{"any tag", strong, `*`, http.StatusOK},
		// The rest is unchanged: a different tag is still refused, and so is a
		// weak tag against a resource whose own tag is weak, which If-Match's
		// strong comparison never matches.
		{"a different tag", strong, `W/"v2"`, http.StatusPreconditionFailed},
		{"a weak tag against a weak one", taggedContent(`W/"w1"`), `W/"w1"`, http.StatusPreconditionFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, encoding := range []string{"gzip", ""} {
				rec := conditional(tc.handler, "Accept-Encoding", encoding, "If-Match", tc.ifMatch)
				if rec.Code != tc.want {
					t.Errorf("If-Match %s with Accept-Encoding %q: status = %d, want %d", tc.ifMatch, encoding, rec.Code, tc.want)
				}
			}
		})
	}
}

func TestCompressLeavesTheRequestOfAHandlerAloneWithoutAWeakIfMatch(t *testing.T) {
	t.Parallel()
	// The request is copied only when there is something to restore, so a
	// handler upstream and downstream see one request in the usual case.
	var seen *http.Request
	handler := Compress(CompressionOptions{})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r
	}))
	for _, ifMatch := range []string{"", `"v1"`, `*`} {
		req := httptest.NewRequest(http.MethodPut, "/", nil)
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if seen != req {
			t.Errorf("If-Match %q: the handler was given a copy of the request", ifMatch)
		}
	}

	req := httptest.NewRequest(http.MethodPut, "/", nil)
	req.Header.Set("If-Match", `W/"v1"`)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if got := seen.Header.Get("If-Match"); got != `"v1"` {
		t.Errorf("the handler saw If-Match %q, want the strong tag", got)
	}
	if got := req.Header.Get("If-Match"); got != `W/"v1"` {
		t.Errorf("the caller's request was changed to If-Match %q, want it left as it arrived", got)
	}
}

func TestCompressRestoresTheIfMatchARouteBinds(t *testing.T) {
	t.Parallel()
	// The same through an application, where the handler compares the header
	// it binds with the tag it issued, which is how a conditional write is
	// usually written.
	type conditionalIn struct {
		IfMatch string `header:"If-Match"`
	}
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{MinSize: 1}))
	app.Put("/item", func(_ *Context, in conditionalIn) (tinyOut, error) {
		if in.IfMatch != `"v1"` {
			return tinyOut{}, PreconditionFailed("the item has changed")
		}
		return tinyOut{OK: true}, nil
	})
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodPut, "/item", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("If-Match", `W/"v1"`)
	if rec := doRequest(t, app, req); rec.Code != http.StatusOK {
		t.Errorf("a write conditional on the tag a compressed read carried = %d, want 200", rec.Code)
	}
}

func TestStrongEntityTags(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{`W/"v1"`, `"v1"`},
		{`"a", W/"b" , W/"c"`, `"a", "b" , "c"`},
		// A tag's own characters are copied as they are, including a W/ and a
		// comma, both of which an opaque tag may contain.
		{`"aW/", W/"b,W/c"`, `"aW/", "b,W/c"`},
		// A W/ that does not precede a tag is not a weakness indicator.
		{`W/ "v1"`, `W/ "v1"`},
		{`W/`, `W/`},
		{`w/"v1"`, `w/"v1"`},
	}
	for _, tc := range tests {
		if got := strongEntityTags(tc.in); got != tc.want {
			t.Errorf("strongEntityTags(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
