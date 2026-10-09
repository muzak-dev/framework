package muzak

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// AutoETag answers a request that already holds the body with 304. A wrong
// 304 is a stale page a user cannot get rid of, so every test here also
// checks the cases that must not be answered that way.

type etagOut struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// strongTagOf is the tag AutoETag promises for a body.
func strongTagOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`
}

// getWith sends a GET carrying the given headers, as alternating names and
// values.
func getWith(t *testing.T, app *App, method, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	return doRequest(t, app, req)
}

func TestAutoETagTagsAJSONBody(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/item", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: "a", Count: 1}, nil }, AutoETag())
	rec := do(t, app, http.MethodGet, "/item")
	assertStatus(t, rec, http.StatusOK)
	tag := rec.Header().Get("ETag")
	if tag != strongTagOf(rec.Body.String()) {
		t.Fatalf("ETag = %q, want %q, the first 128 bits of the body's SHA-256", tag, strongTagOf(rec.Body.String()))
	}
	if len(tag) != 24 || strings.HasPrefix(tag, "W/") {
		t.Errorf("ETag = %q, want a strong tag of 22 characters between quotes", tag)
	}
	for range 10 {
		if again := do(t, app, http.MethodGet, "/item").Header().Get("ETag"); again != tag {
			t.Fatalf("the tag changed from %q to %q for the same body", tag, again)
		}
	}
}

func TestAutoETagChangesWithTheBody(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	app := New(quietOptions())
	app.Get("/counter", func(ctx *Context, _ Empty) (etagOut, error) {
		return etagOut{Count: int(n.Load())}, nil
	}, AutoETag())
	first := do(t, app, http.MethodGet, "/counter").Header().Get("ETag")
	n.Add(1)
	second := do(t, app, http.MethodGet, "/counter")
	if second.Header().Get("ETag") == first {
		t.Fatal("the tag did not change when the body did")
	}
	// The client holding the old body is sent the new one.
	rec := getWith(t, app, http.MethodGet, "/counter", "If-None-Match", first)
	assertStatus(t, rec, http.StatusOK)
	if rec.Body.String() != second.Body.String() {
		t.Errorf("body = %q, want the new body %q", rec.Body.String(), second.Body.String())
	}
}

// A map is encoded with its keys sorted on a tagged route, so the same value
// always produces the same body and the same tag.
func TestAutoETagIsStableForAMap(t *testing.T) {
	t.Parallel()
	value := make(map[string]int, 64)
	for i := range 64 {
		value["key-"+strconv.Itoa(i)] = i
	}
	app := New(quietOptions())
	app.Get("/map", func(ctx *Context, _ Empty) (map[string]int, error) { return value, nil }, AutoETag())
	tag := do(t, app, http.MethodGet, "/map").Header().Get("ETag")
	for range 30 {
		if again := do(t, app, http.MethodGet, "/map").Header().Get("ETag"); again != tag {
			t.Fatalf("the tag of one map changed from %q to %q", tag, again)
		}
	}
}

// The 304 keeps exactly what RFC 9110 section 15.4.5 asks for and drops what
// describes the body it does not send.
func TestAutoETagNotModifiedCarriesTheRightHeaders(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/item", func(ctx *Context, _ Empty) (etagOut, error) {
		header := ctx.ResponseWriter().Header()
		header.Set("Cache-Control", "max-age=60")
		header.Set("Expires", "Thu, 01 Jan 2099 00:00:00 GMT")
		header.Set("Content-Location", "/item/v2")
		header.Set("Vary", "Accept")
		header.Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
		header.Set("Content-Language", "en")
		header.Set("Content-Disposition", "inline")
		header.Set("Set-Cookie", "seen=1")
		return etagOut{Name: "a"}, nil
	}, AutoETag())
	server := newWireServer(t, app)
	full := server.do(t, http.MethodGet, "/item")
	tag := full.Header.Get("ETag")

	res := server.do(t, http.MethodGet, "/item", "If-None-Match", tag)
	if res.StatusCode != http.StatusNotModified || len(res.Data) != 0 {
		t.Fatalf("got %d with %q, want 304 and no body", res.StatusCode, res.Data)
	}
	for name, want := range map[string]string{
		"ETag":             tag,
		"Cache-Control":    "max-age=60",
		"Expires":          "Thu, 01 Jan 2099 00:00:00 GMT",
		"Content-Location": "/item/v2",
		"Vary":             "Accept",
		"Set-Cookie":       "seen=1",
	} {
		if got := res.Header.Get(name); got != want {
			t.Errorf("304 %s = %q, want %q", name, got, want)
		}
	}
	if res.Header.Get("Date") == "" || res.Header.Get(HeaderRequestID) == "" {
		t.Errorf("304 lost Date or the request identifier: %v", res.Header)
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Language", "Content-Disposition", "Last-Modified", "Transfer-Encoding"} {
		if got := res.Header.Get(name); got != "" {
			t.Errorf("304 carries %s = %q, which describes a body it does not send", name, got)
		}
	}
}

func TestAutoETagComparesWeakly(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/item", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: "a"}, nil }, AutoETag())
	tag := do(t, app, http.MethodGet, "/item").Header().Get("ETag")
	opaque := strings.Trim(tag, `"`)
	for _, header := range []string{
		tag,
		"W/" + tag,
		`"other", ` + tag,
		`"x,y", W/"z" ,` + tag + `, "after"`,
		` ,, ` + tag + ` ,`,
		"*",
		" * ",
	} {
		if rec := getWith(t, app, http.MethodGet, "/item", "If-None-Match", header); rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %q: status %d, want 304", header, rec.Code)
		}
	}
	// The same, split over several lines of the field.
	if rec := getWith(t, app, http.MethodGet, "/item", "If-None-Match", `"a"`, "If-None-Match", tag); rec.Code != http.StatusNotModified {
		t.Errorf("a tag on the second line: status %d, want 304", rec.Code)
	}
	for _, header := range []string{
		`"other"`,
		opaque,
		`w/` + tag,
		`W/ ` + tag,
		`"` + opaque,
		tag + ` "other"`,
		`"other" ` + tag,
		`*, ` + tag,
		tag + `, *`,
		tag + `, "unterminated`,
		tag + `, unquoted`,
		`"` + opaque + "\x01" + `"`,
		`"` + opaque + ` "`,
		strings.ToUpper(tag),
		"",
	} {
		if rec := getWith(t, app, http.MethodGet, "/item", "If-None-Match", header); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("If-None-Match %q: status %d, want the full 200", header, rec.Code)
		}
	}
	// A malformed line spoils the field, wherever the match is.
	if rec := getWith(t, app, http.MethodGet, "/item", "If-None-Match", tag, "If-None-Match", `"broken`); rec.Code != http.StatusOK {
		t.Errorf("a malformed second line: status %d, want the full 200", rec.Code)
	}
}

// A tag a handler set is kept, compared, and may hold a comma.
func TestAutoETagKeepsTheHandlersTag(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/own", func(ctx *Context, _ Empty) (etagOut, error) {
		ctx.SetHeader("ETag", `"v1,2"`)
		return etagOut{Name: "a"}, nil
	}, AutoETag())
	app.Get("/malformed", func(ctx *Context, _ Empty) (etagOut, error) {
		ctx.SetHeader("ETag", `v1`)
		return etagOut{Name: "a"}, nil
	}, AutoETag())
	if got := do(t, app, http.MethodGet, "/own").Header().Get("ETag"); got != `"v1,2"` {
		t.Errorf("ETag = %q, want the handler's own", got)
	}
	for header, want := range map[string]int{
		`"v1,2"`:          http.StatusNotModified,
		`"v0", "v1,2"`:    http.StatusNotModified,
		`W/"v1,2"`:        http.StatusNotModified,
		`"v1"`:            http.StatusOK,
		`"2"`:             http.StatusOK,
		`"v1", "2"`:       http.StatusOK,
		`"v0" "v1,2"`:     http.StatusOK,
		`"v1,2`:           http.StatusOK,
		strongTagOf("{}"): http.StatusOK,
	} {
		if rec := getWith(t, app, http.MethodGet, "/own", "If-None-Match", header); rec.Code != want {
			t.Errorf("If-None-Match %q: status %d, want %d", header, rec.Code, want)
		}
	}
	// A tag that is not one is never matched, so its response is always sent.
	for _, header := range []string{"v1", `"v1"`, "*"} {
		if rec := getWith(t, app, http.MethodGet, "/malformed", "If-None-Match", header); rec.Code != http.StatusOK {
			t.Errorf("a malformed handler tag with If-None-Match %q: status %d, want 200", header, rec.Code)
		}
	}
}

// Only a 200 answering GET or HEAD is tagged.
func TestAutoETagAppliesToSuccessfulReadsOnly(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), AutoETag())
	out := func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: "a"}, nil }
	app.Get("/read", out)
	app.Post("/read", out)
	app.Put("/read", out)
	app.Get("/created", out, Status(http.StatusCreated))
	app.Get("/accepted", func(ctx *Context, _ Empty) (etagOut, error) {
		ctx.SetStatus(http.StatusAccepted)
		return etagOut{}, nil
	})
	app.Get("/fails", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{}, NotFound("") })

	tag := do(t, app, http.MethodGet, "/read").Header().Get("ETag")
	if tag == "" {
		t.Fatal("a GET was not tagged")
	}
	head := getWith(t, app, http.MethodHead, "/read")
	if head.Header().Get("ETag") != tag {
		t.Errorf("HEAD ETag = %q, want the GET's %q", head.Header().Get("ETag"), tag)
	}
	if rec := getWith(t, app, http.MethodHead, "/read", "If-None-Match", tag); rec.Code != http.StatusNotModified {
		t.Errorf("a conditional HEAD: status %d, want 304", rec.Code)
	}
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/read"}, {http.MethodPut, "/read"},
		{http.MethodGet, "/created"}, {http.MethodGet, "/accepted"}, {http.MethodGet, "/fails"},
	} {
		rec := getWith(t, app, req.method, req.path, "If-None-Match", "*")
		if rec.Code == http.StatusNotModified || rec.Header().Get("ETag") != "" {
			t.Errorf("%s %s: status %d with ETag %q, want it untagged and answered in full", req.method, req.path, rec.Code, rec.Header().Get("ETag"))
		}
	}
}

func TestAutoETagTagsHTMLAndBytesButNotStreamsOrRedirects(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), AutoETag())
	app.Get("/html", func(ctx *Context, _ Empty) (HTML, error) { return HTML("<p>hi</p>"), nil })
	app.Get("/bytes", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{ContentType: "image/png", Data: []byte("\x89PNG")}, nil
	})
	app.Get("/stream", func(ctx *Context, _ Empty) (Stream, error) {
		return Stream{Body: strings.NewReader("streamed")}, nil
	})
	app.Get("/redirect", func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{To: "/html"}, nil })

	for path, body := range map[string]string{"/html": "<p>hi</p>", "/bytes": "\x89PNG"} {
		rec := do(t, app, http.MethodGet, path)
		if got := rec.Header().Get("ETag"); got != strongTagOf(body) {
			t.Errorf("%s: ETag = %q, want %q", path, got, strongTagOf(body))
		}
		cached := getWith(t, app, http.MethodGet, path, "If-None-Match", strongTagOf(body))
		if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("Content-Type") != "" {
			t.Errorf("%s: revalidation = %d with %q as %q, want a bare 304", path, cached.Code, cached.Body.String(), cached.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/stream", "/redirect"} {
		rec := getWith(t, app, http.MethodGet, path, "If-None-Match", "*")
		if rec.Code == http.StatusNotModified || rec.Header().Get("ETag") != "" {
			t.Errorf("%s: status %d with ETag %q, want it left alone", path, rec.Code, rec.Header().Get("ETag"))
		}
	}
}

func TestAutoETagIsOffUnlessDeclared(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/plain", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: "a"}, nil })
	rec := do(t, app, http.MethodGet, "/plain")
	if rec.Header().Get("ETag") != "" {
		t.Fatalf("ETag = %q on a route that did not ask for one", rec.Header().Get("ETag"))
	}
	if rec := getWith(t, app, http.MethodGet, "/plain", "If-None-Match", strongTagOf(rec.Body.String())); rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200 from a route that does not tag", rec.Code)
	}
}

// Declared on the application, a router or the point of inclusion, it reaches
// every route beneath, and only those.
func TestAutoETagIsInherited(t *testing.T) {
	t.Parallel()
	out := func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: "a"}, nil }
	wide := New(quietOptions(), AutoETag())
	wide.Get("/x", out)
	routerWide := New(quietOptions())
	tagged := NewRouter(AutoETag())
	tagged.Get("/tagged", out)
	routerWide.Include(tagged)
	included := NewRouter()
	included.Get("/included", out)
	routerWide.Include(included, AutoETag())
	plain := NewRouter()
	plain.Get("/plain", out)
	routerWide.Include(plain)

	for app, paths := range map[*App]map[string]bool{
		wide:       {"/x": true},
		routerWide: {"/tagged": true, "/included": true, "/plain": false},
	} {
		for path, want := range paths {
			if got := do(t, app, http.MethodGet, path).Header().Get("ETag") != ""; got != want {
				t.Errorf("%s: tagged = %v, want %v", path, got, want)
			}
		}
	}
}

// Compression weakens the tag of the body it compresses and of the 304 it
// answers for one, and the weakened tag the client then sends still matches.
func TestAutoETagComposesWithCompression(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a long and compressible name ", 100)
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{}))
	app.Get("/big", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: big}, nil }, AutoETag())
	server := newWireServer(t, app)

	identity := server.do(t, http.MethodGet, "/big")
	strong := identity.Header.Get("ETag")
	if strong == "" || strings.HasPrefix(strong, "W/") {
		t.Fatalf("uncompressed ETag = %q, want a strong tag", strong)
	}
	compressed := server.do(t, http.MethodGet, "/big", "Accept-Encoding", "gzip")
	if compressed.Header.Get("Content-Encoding") != "gzip" || compressed.Header.Get("ETag") != "W/"+strong {
		t.Fatalf("compressed: encoding %q, ETag %q, want gzip and W/%s", compressed.Header.Get("Content-Encoding"), compressed.Header.Get("ETag"), strong)
	}
	for _, tc := range []struct {
		encoding, sent, want string
	}{
		{"gzip", "W/" + strong, "W/" + strong},
		{"gzip", strong, "W/" + strong},
		{"", strong, strong},
		{"", "W/" + strong, strong},
	} {
		res := server.do(t, http.MethodGet, "/big", "Accept-Encoding", tc.encoding, "If-None-Match", tc.sent)
		if res.StatusCode != http.StatusNotModified || res.Header.Get("ETag") != tc.want || len(res.Data) != 0 {
			t.Errorf("encoding %q sending %s: %d with ETag %q, want 304 with %s", tc.encoding, tc.sent, res.StatusCode, res.Header.Get("ETag"), tc.want)
		}
		if !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
			t.Errorf("encoding %q: the 304's Vary = %q, want Accept-Encoding", tc.encoding, res.Header.Get("Vary"))
		}
		if res.Header.Get("Content-Encoding") != "" {
			t.Errorf("encoding %q: the 304 carries Content-Encoding %q", tc.encoding, res.Header.Get("Content-Encoding"))
		}
	}
}

// A guarded response is private, and so is its 304, which a shared cache
// therefore cannot use to hand one user's body to another.
func TestAutoETagKeepsAGuardedResponsePrivate(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), AutoETag(), WithDependencies(func(ctx *Context) error {
		if ctx.Header("Authorization") == "" {
			return Unauthorized("")
		}
		return nil
	}))
	app.Get("/me", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: ctx.Header("Authorization")}, nil })
	full := getWith(t, app, http.MethodGet, "/me", "Authorization", "alice")
	tag := full.Header().Get("ETag")
	if full.Header().Get("Cache-Control") != privateCacheControl || tag == "" {
		t.Fatalf("200: Cache-Control %q and ETag %q, want private and a tag", full.Header().Get("Cache-Control"), tag)
	}
	rec := getWith(t, app, http.MethodGet, "/me", "Authorization", "alice", "If-None-Match", tag)
	if rec.Code != http.StatusNotModified || rec.Header().Get("Cache-Control") != privateCacheControl {
		t.Errorf("304: %d with Cache-Control %q, want 304 kept private", rec.Code, rec.Header().Get("Cache-Control"))
	}
	// Another user holding alice's tag is sent their own body, not a 304.
	if rec := getWith(t, app, http.MethodGet, "/me", "Authorization", "bob", "If-None-Match", tag); rec.Code != http.StatusOK {
		t.Errorf("another user with alice's tag: status %d, want 200", rec.Code)
	}
	// And one the guard refuses is refused, whatever tag it holds.
	if rec := getWith(t, app, http.MethodGet, "/me", "If-None-Match", tag); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated revalidation: status %d, want 401", rec.Code)
	}
}

// What middleware recorded the response depends on is merged into the 304's
// Vary as it is into the 200's.
func TestAutoETagNotModifiedKeepsTheFrameworksVary(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.CORS = CORSOptions{AllowedOrigins: []string{"https://app.example.com"}}
	app := New(opts, AutoETag())
	app.Get("/item", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{Name: "a"}, nil })
	tag := getWith(t, app, http.MethodGet, "/item", "Origin", "https://app.example.com").Header().Get("ETag")
	rec := getWith(t, app, http.MethodGet, "/item", "Origin", "https://app.example.com", "If-None-Match", tag)
	if rec.Code != http.StatusNotModified || !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("304 = %d with Vary %q, want Origin in it", rec.Code, rec.Header().Get("Vary"))
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("the 304 lost the CORS answer: %v", rec.Header())
	}
}

// A route that does not tag pays nothing for the feature: the request is not
// read for If-None-Match and no allocation is added. It is not parallel,
// because AllocsPerRun counts every allocation in the process.
func TestAutoETagOffCostsNothing(t *testing.T) {
	app := New(AppOptions{LoggerOptions: LoggerOptions{Format: LogFormatNone}, DisableAccessLog: true, DisableDocs: true})
	app.Get("/plain", func(ctx *Context, _ Empty) (benchOut, error) { return benchOut{ID: "x"}, nil })
	app.Get("/tagged", func(ctx *Context, _ Empty) (benchOut, error) { return benchOut{ID: "x"}, nil }, AutoETag())
	mustBuild(t, app)
	w := newDiscardWriter()

	plain := httptest.NewRequest(http.MethodGet, "/plain", nil)
	conditional := httptest.NewRequest(http.MethodGet, "/plain", nil)
	conditional.Header.Set("If-None-Match", strings.Repeat(`"x", `, 100)+"*")
	tagged := httptest.NewRequest(http.MethodGet, "/tagged", nil)

	serve(app, plain, w)
	if w.header.Get("ETag") != "" {
		t.Fatal("the untagged route was tagged")
	}
	base := testing.AllocsPerRun(200, func() { serve(app, plain, w) })
	withHeader := testing.AllocsPerRun(200, func() { serve(app, conditional, w) })
	if withHeader != base {
		t.Errorf("an If-None-Match on an untagged route cost %.0f allocations against %.0f without, want the header ignored", withHeader, base)
	}
	// The tagged route pays for its tag and nothing else: the digest it
	// renders and the header it sets.
	if tagCost := testing.AllocsPerRun(200, func() { serve(app, tagged, w) }) - base; tagCost < 1 || tagCost > 3 {
		t.Errorf("a tag cost %.0f allocations, want between 1 and 3", tagCost)
	}
}

// The If-None-Match reader is linear: a field of ten thousand tags is read
// once, wherever the match is, and a match at its end is found. It is not
// parallel, because the allocations it counts are the whole process's.
func TestIfNoneMatchIsLinearAndBounded(t *testing.T) {
	tags := make([]string, 10_000)
	for i := range tags {
		tags[i] = fmt.Sprintf(`W/"tag-%d,with,commas"`, i)
	}
	field := strings.Join(tags, ", ")
	if !ifNoneMatch([]string{field}, `"tag-9999,with,commas"`) {
		t.Error("the last of ten thousand tags was not matched")
	}
	if ifNoneMatch([]string{field}, `"tag-10000,with,commas"`) {
		t.Error("a tag that is not in the field was matched")
	}
	// A field that is malformed at its very end is ignored, after one pass.
	if ifNoneMatch([]string{field + `, "open`}, `"tag-1,with,commas"`) {
		t.Error("a field malformed at its end was used")
	}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			ifNoneMatch([]string{field}, `"tag-9999,with,commas"`)
		}
	})
	if result.AllocsPerOp() != 0 {
		t.Errorf("reading the field allocated %d times, want none", result.AllocsPerOp())
	}
}

func TestOpaqueTag(t *testing.T) {
	t.Parallel()
	for tag, want := range map[string]string{`"a"`: "a", `W/"a"`: "a", `""`: "", `"a,b"`: "a,b"} {
		if got, ok := opaqueTag(tag); !ok || got != want {
			t.Errorf("opaqueTag(%q) = %q, %v, want %q", tag, got, ok, want)
		}
	}
	for _, tag := range []string{"", `"`, "a", `W/a`, `w/"a"`, `"a`, `a"`, `"a"b"`, "\"a\x01\"", `"a b"`} {
		if _, ok := opaqueTag(tag); ok {
			t.Errorf("opaqueTag(%q) accepted a malformed tag", tag)
		}
	}
}
