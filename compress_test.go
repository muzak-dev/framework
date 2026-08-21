package badele

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// prose is a body comfortably over the default minimum size and compressible
// enough that the result is unmistakably smaller.
var prose = strings.Repeat("the type signature is the contract. ", 200)

type proseOut struct {
	Body string `json:"body"`
}

// compressedApp returns an application whose one route answers with prose,
// behind the compression middleware.
func compressedApp(t *testing.T, opts CompressionOptions) *App {
	t.Helper()
	app := New(quietOptions())
	app.Use(Compress(opts))
	app.Get("/prose", func(ctx *Context, _ Empty) (proseOut, error) {
		return proseOut{Body: prose}, nil
	})
	return mustBuild(t, app)
}

// ask sends a request declaring the given Accept-Encoding.
func ask(t *testing.T, app *App, target, encoding string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if encoding != "" {
		req.Header.Set("Accept-Encoding", encoding)
	}
	return doRequest(t, app, req)
}

func TestCompressShrinksAResponseTheClientCanDecode(t *testing.T) {
	t.Parallel()
	app := compressedApp(t, CompressionOptions{})

	plain := ask(t, app, "/prose", "")
	assertStatus(t, plain, http.StatusOK)
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q for a client that asked for none", got)
	}

	zipped := ask(t, app, "/prose", "gzip")
	assertStatus(t, zipped, http.StatusOK)
	if got := zipped.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if zipped.Body.Len() >= plain.Body.Len() {
		t.Errorf("compressed body is %d bytes against %d uncompressed", zipped.Body.Len(), plain.Body.Len())
	}

	// The bytes on the wire have to decode back to exactly what the handler
	// returned, which is the only thing a client actually cares about.
	reader, err := gzip.NewReader(bytes.NewReader(zipped.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the compressed body: %v", err)
	}
	if !bytes.Equal(decoded, plain.Body.Bytes()) {
		t.Error("the decompressed body differs from the uncompressed one")
	}

	// A length describing the uncompressed body would be a lie, and net/http
	// cannot know the compressed one in advance.
	if got := zipped.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it removed once the body is encoded", got)
	}
}

func TestCompressAlwaysReportsThatItVaries(t *testing.T) {
	t.Parallel()
	app := compressedApp(t, CompressionOptions{})
	// Both answers have to carry Vary, not just the compressed one. A cache
	// that stored the uncompressed answer without it would go on to serve it
	// to a client that asked for gzip, and the other way round.
	for _, encoding := range []string{"", "gzip"} {
		rec := ask(t, app, "/prose", encoding)
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
			t.Errorf("Vary = %q for Accept-Encoding %q, want it to name Accept-Encoding", got, encoding)
		}
	}
}

func TestCompressNegotiatesTheEncoding(t *testing.T) {
	t.Parallel()
	app := compressedApp(t, CompressionOptions{})
	cases := []struct {
		name   string
		accept string
		want   string
	}{
		{name: "gzip", accept: "gzip", want: "gzip"},
		{name: "deflate alone", accept: "deflate", want: "deflate"},
		{name: "gzip preferred over deflate", accept: "deflate, gzip", want: "gzip"},
		{name: "quality values are tolerated", accept: "gzip;q=0.8, deflate;q=0.5", want: "gzip"},
		{name: "an explicit refusal is honoured", accept: "gzip;q=0, deflate", want: "deflate"},
		{name: "everything refused", accept: "gzip;q=0, deflate;q=0", want: ""},
		{name: "an encoding we cannot produce", accept: "br, zstd", want: ""},
		{name: "no header at all", accept: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := ask(t, app, "/prose", tc.accept)
			assertStatus(t, rec, http.StatusOK)
			if got := rec.Header().Get("Content-Encoding"); got != tc.want {
				t.Errorf("Content-Encoding = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompressDeflateDecodes(t *testing.T) {
	t.Parallel()
	app := compressedApp(t, CompressionOptions{})
	// Twice, so the second request takes a compressor from the pool and proves
	// a recycled one is reset rather than continuing the previous response.
	for range 2 {
		rec := ask(t, app, "/prose", "deflate")
		assertStatus(t, rec, http.StatusOK)

		decoded, err := io.ReadAll(flate.NewReader(bytes.NewReader(rec.Body.Bytes())))
		if err != nil {
			t.Fatalf("reading the deflated body: %v", err)
		}
		if !strings.Contains(string(decoded), prose) {
			t.Error("the deflated body did not decode back to what the handler returned")
		}
	}
}

type tinyOut struct {
	OK bool `json:"ok"`
}

func TestCompressLeavesASmallBodyAlone(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{}))
	app.Get("/tiny", func(ctx *Context, _ Empty) (tinyOut, error) {
		return tinyOut{OK: true}, nil
	})

	rec := ask(t, mustBuild(t, app), "/tiny", "gzip")
	assertStatus(t, rec, http.StatusOK)
	// Compressing a body that already fits in one packet cannot make it
	// arrive sooner, and gzip's own framing would make this one larger.
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q for a body under the minimum size", got)
	}
	assertJSON(t, rec, `{"ok":true}`)
}

func TestCompressHonoursTheConfiguredMinimum(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{MinSize: 4}))
	app.Get("/tiny", func(ctx *Context, _ Empty) (tinyOut, error) {
		return tinyOut{OK: true}, nil
	})

	rec := ask(t, mustBuild(t, app), "/tiny", "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip once the minimum allows it", got)
	}
}

func TestCompressSkipsWhatItShouldNotTouch(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{MinSize: 1}))

	// A handler writing the response itself, which is how a stream, an image
	// or an already-encoded body reaches the client.
	app.Get("/raw/{kind}", func(ctx *Context, in struct {
		Kind string `path:"kind"`
	}) (Empty, error) {
		w := ctx.ResponseWriter()
		switch in.Kind {
		case "image":
			w.Header().Set("Content-Type", "image/png")
		case "stream":
			w.Header().Set("Content-Type", "text/event-stream")
		case "encoded":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "br")
		case "unknown":
			// No content type at all.
		case "nocontent":
			w.Header().Set("Content-Type", "application/json")
			ctx.SetStatus(http.StatusNoContent)
			w.WriteHeader(http.StatusNoContent)
			return Empty{}, nil
		}
		_, _ = w.Write([]byte(prose))
		return Empty{}, nil
	})
	built := mustBuild(t, app)

	cases := []struct {
		kind string
		why  string
	}{
		{kind: "image", why: "an image is already compressed"},
		{kind: "stream", why: "an event stream must not be held in a compression window"},
		{kind: "encoded", why: "the handler encoded it already"},
		{kind: "unknown", why: "nothing declared what the body is"},
		{kind: "nocontent", why: "there is no body to compress"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			rec := ask(t, built, "/raw/"+tc.kind, "gzip")
			if got := rec.Header().Get("Content-Encoding"); got == "gzip" {
				t.Errorf("the body was compressed, but %s", tc.why)
			}
		})
	}
}

func TestCompressBuffersABodyOfUnknownLength(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{}))
	// The handler writes the body itself, so nothing declares its length and
	// the decision has to be made from the body as it arrives.
	app.Get("/streamed/{size}", func(ctx *Context, in struct {
		Size string `path:"size"`
	}) (Empty, error) {
		w := ctx.ResponseWriter()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		body := "small"
		if in.Size == "large" {
			body = prose
		}
		// Written in pieces, so the decision cannot depend on seeing it at once.
		for chunk := range strings.SplitSeq(body, " ") {
			if _, err := io.WriteString(w, chunk+" "); err != nil {
				return Empty{}, err
			}
		}
		return Empty{}, nil
	})
	built := mustBuild(t, app)

	small := ask(t, built, "/streamed/small", "gzip")
	assertStatus(t, small, http.StatusOK)
	if got := small.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q for a short streamed body", got)
	}
	if small.Body.String() != "small " {
		t.Errorf("body = %q, want the held bytes written out as they stand", small.Body.String())
	}

	large := ask(t, built, "/streamed/large", "gzip")
	assertStatus(t, large, http.StatusOK)
	if got := large.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q for a long streamed body, want gzip", got)
	}
	reader, err := gzip.NewReader(bytes.NewReader(large.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the compressed body: %v", err)
	}
	// Nothing may be lost from the part written before the decision was made.
	if !strings.HasPrefix(string(decoded), "the type signature is the contract.") {
		t.Errorf("decoded body starts %q, want the bytes held before the decision", decoded[:40])
	}
	var expected strings.Builder
	for chunk := range strings.SplitSeq(prose, " ") {
		expected.WriteString(chunk + " ")
	}
	if string(decoded) != expected.String() {
		t.Errorf("decoded %d bytes, want the whole %d byte body", len(decoded), expected.Len())
	}
}

func TestCompressFlushDeliversWhatIsBuffered(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{}))
	app.Get("/flushed", func(ctx *Context, _ Empty) (Empty, error) {
		w := ctx.ResponseWriter()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "first ")
		// A handler that flushes is telling the client not to wait, which has
		// to win over holding bytes back to judge their size.
		http.NewResponseController(w).Flush()
		_, _ = io.WriteString(w, "second")
		return Empty{}, nil
	})

	rec := ask(t, mustBuild(t, app), "/flushed", "gzip")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want a flushed response left uncompressed", got)
	}
	if rec.Body.String() != "first second" {
		t.Errorf("body = %q, want both writes delivered", rec.Body.String())
	}
}

func TestCompressWeakensAStrongETag(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{MinSize: 1}))
	app.Get("/tagged", func(ctx *Context, in struct {
		Tag string `query:"tag"`
	}) (tinyOut, error) {
		ctx.SetHeader("ETag", in.Tag)
		return tinyOut{OK: true}, nil
	})
	built := mustBuild(t, app)

	// A strong tag promises the bytes are identical, which they no longer are
	// once the representation has been encoded.
	rec := ask(t, built, `/tagged?tag=%22abc%22`, "gzip")
	if got := rec.Header().Get("ETag"); got != `W/"abc"` {
		t.Errorf("ETag = %q, want it weakened to W/\"abc\"", got)
	}
	// One that was already weak claims nothing about the bytes, so it stands.
	rec = ask(t, built, `/tagged?tag=W%2F%22abc%22`, "gzip")
	if got := rec.Header().Get("ETag"); got != `W/"abc"` {
		t.Errorf("ETag = %q, want the weak tag left as it was", got)
	}
}

func TestCompressLevelsAllProduceReadableBodies(t *testing.T) {
	t.Parallel()
	levels := map[string]CompressionLevel{
		"default": CompressionDefault,
		"fastest": CompressionFastest,
		"best":    CompressionBest,
	}
	for name, level := range levels {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := compressedApp(t, CompressionOptions{Level: level})
			// Two requests, so the second takes a compressor from the pool and
			// proves a recycled one is reset rather than continuing the last
			// response.
			for range 2 {
				rec := ask(t, app, "/prose", "gzip")
				assertStatus(t, rec, http.StatusOK)
				reader, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
				if err != nil {
					t.Fatalf("gzip.NewReader: %v", err)
				}
				decoded, err := io.ReadAll(reader)
				if err != nil {
					t.Fatalf("reading the compressed body: %v", err)
				}
				if !strings.Contains(string(decoded), "the type signature is the contract") {
					t.Fatal("the body did not decode back to what the handler returned")
				}
			}
		})
	}
}

func TestCompressHonoursACustomTypeList(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{MinSize: 1, ContentTypes: []string{"application/problem+json"}}))
	app.Get("/prose", func(ctx *Context, _ Empty) (proseOut, error) {
		return proseOut{Body: prose}, nil
	})
	// The list replaces the built-in one rather than adding to it, so a type
	// the defaults would have compressed is now left alone.
	rec := ask(t, mustBuild(t, app), "/prose", "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want application/json left alone by this policy", got)
	}
}

func TestCompressibleMediaTypes(t *testing.T) {
	t.Parallel()
	policy := CompressionOptions{}.withDefaults()
	cases := map[string]bool{
		"text/html; charset=utf-8":       true,
		"application/json":               true,
		"application/problem+json":       true,
		"image/svg+xml":                  true,
		"application/xhtml+xml":          true,
		"image/png":                      false,
		"application/octet-stream":       false,
		"text/event-stream":              false,
		"":                               false,
		"not a media type at all; ;; ;":  false,
		"application/vnd.badele+unknown": false,
	}
	for contentType, want := range cases {
		if got := policy.compressible(contentType); got != want {
			t.Errorf("compressible(%q) = %v, want %v", contentType, got, want)
		}
	}
}

// TestCompressWriterHandlesTheEdges drives the writer directly for the cases a
// handler cannot easily be made to produce: statuses that carry no body, a
// repeated WriteHeader, a handler that writes nothing at all, and a flush part
// way through a body already being compressed.
func TestCompressWriterHandlesTheEdges(t *testing.T) {
	t.Parallel()
	policy := CompressionOptions{MinSize: 1}.withDefaults()
	pool := &compressorPool{level: gzip.DefaultCompression}
	newWriter := func(rec *httptest.ResponseRecorder, encoding string) *compressWriter {
		w := &compressWriter{ResponseWriter: rec, policy: policy, pool: pool, encoding: encoding}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		return w
	}

	bodiless := map[string]int{
		"informational":   http.StatusContinue,
		"partial content": http.StatusPartialContent,
	}
	for name, status := range bodiless {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w := newWriter(rec, "gzip")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(prose))
			w.finish()
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q for status %d", got, status)
			}
			if rec.Body.String() != prose {
				t.Error("the body was not passed through untouched")
			}
		})
	}

	t.Run("repeated WriteHeader", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := newWriter(rec, "gzip")
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusOK)
		w.finish()
		// net/http ignores the second call, and so must this.
		if rec.Code != http.StatusTeapot {
			t.Errorf("status = %d, want the first one written", rec.Code)
		}
	})

	t.Run("handler wrote nothing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := newWriter(rec, "gzip")
		w.finish()
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want the 200 net/http would have written", rec.Code)
		}
	})

	for _, encoding := range []string{"gzip", "deflate"} {
		t.Run("flush while compressing "+encoding, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w := newWriter(rec, encoding)
			w.Header().Set("Content-Length", strconv.Itoa(len(prose)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(prose))
			// Nothing has necessarily reached the client yet: the compressor
			// holds a window of its own.
			w.Flush()
			if rec.Body.Len() == 0 {
				t.Error("flushing delivered none of the compressed body")
			}
			w.finish()
		})
	}

	t.Run("unwrap reaches the underlying writer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := newWriter(rec, "gzip")
		// This is what lets http.ResponseController find deadline control and
		// hijacking through the wrapper.
		if w.Unwrap() != http.ResponseWriter(rec) {
			t.Error("Unwrap did not return the writer being wrapped")
		}
	})
}

func TestCompressKeepsVaryAgainstAHandlerThatWritesItsOwn(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(Compress(CompressionOptions{}))
	// A handler whose response genuinely varies by something else, written the
	// way a handler naturally writes a header: by setting it.
	app.Get("/varying", func(ctx *Context, _ Empty) (proseOut, error) {
		ctx.SetHeader("Vary", "Cookie, Save-Data")
		return proseOut{Body: prose}, nil
	})
	// One that has already declared the dependency itself.
	app.Get("/declared", func(ctx *Context, _ Empty) (proseOut, error) {
		ctx.SetHeader("Vary", "accept-encoding, Cookie")
		return proseOut{Body: prose}, nil
	})
	// And one that says it varies by everything.
	app.Get("/everything", func(ctx *Context, _ Empty) (proseOut, error) {
		ctx.SetHeader("Vary", "*")
		return proseOut{Body: prose}, nil
	})
	built := mustBuild(t, app)

	// Setting a header replaces it, so a Vary added before the handler ran
	// would be gone by now. Losing it lets a cache hand this compressed body
	// to a client that cannot decode it.
	for _, target := range []string{"/varying", "/declared", "/everything"} {
		for _, encoding := range []string{"", "gzip"} {
			rec := ask(t, built, target, encoding)
			assertStatus(t, rec, http.StatusOK)
			vary := strings.Join(rec.Header().Values("Vary"), ", ")
			if !strings.Contains(strings.ToLower(vary), "accept-encoding") && !strings.Contains(vary, "*") {
				t.Errorf("%s with Accept-Encoding %q: Vary = %q, want it to still cover Accept-Encoding",
					target, encoding, vary)
			}
			// The handler's own fields have to survive too.
			if target == "/varying" && !strings.Contains(vary, "Cookie") {
				t.Errorf("%s: Vary = %q, want the handler's own fields kept", target, vary)
			}
		}
	}

	// The dependency is recorded once, not once per layer that thought of it.
	rec := ask(t, built, "/declared", "gzip")
	if count := strings.Count(strings.ToLower(strings.Join(rec.Header().Values("Vary"), ",")), "accept-encoding"); count != 1 {
		t.Errorf("Accept-Encoding appears %d times in Vary, want once", count)
	}
}
