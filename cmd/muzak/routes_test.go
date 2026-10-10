package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"muzak.dev/framework"
)

func TestRoutesPrintsTheOperations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "openapi.json", documentOf(t, routesApp()))
	r := runIn(t, dir, "routes", "-file", "openapi.json")
	r.expect(t, exitOK)
	golden(t, "routes.golden", r.stdout)
}

func TestRoutesReadsStandardInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := documentOf(t, routesApp())
	c, _, _ := testConsole(dir)
	c.stdin = bytes.NewReader(data)
	r := runWith(c, "routes", "-file", "-")
	r.expect(t, exitOK)
	golden(t, "routes.golden", r.stdout)
}

// serveDocument serves data at /openapi.json from a loopback server.
func serveDocument(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL + "/openapi.json"
}

func TestRoutesFetchesFromARunningApplication(t *testing.T) {
	t.Parallel()
	data := documentOf(t, routesApp())
	url := serveDocument(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi.json" || r.Header.Get("Accept") != "application/json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	r := runIn(t, t.TempDir(), "routes", "-url", url)
	r.expect(t, exitOK)
	golden(t, "routes.golden", r.stdout)
}

func TestRoutesFetchesFromAnApplicationServedByMuzak(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(routesApp())
	t.Cleanup(server.Close)
	r := runIn(t, t.TempDir(), "routes", "-url", server.URL+"/openapi.json")
	r.expect(t, exitOK)
	golden(t, "routes.golden", r.stdout)
}

func TestRoutesFetchFailures(t *testing.T) {
	t.Parallel()
	notFound := serveDocument(t, http.NotFound)
	tooLarge := serveDocument(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxDocumentBytes+1))
	})
	declaredTooLarge := serveDocument(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "999999999")
		w.WriteHeader(http.StatusOK)
	})
	notADocument := serveDocument(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>\x1b[2J</html>"))
	})
	// A member the document type has no field for, whose name is an escape
	// sequence, which the decoder's message quotes.
	hostileMember := serveDocument(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "paths": {}, "\u001b[2J\u0007": 1}`))
	})
	release := make(chan struct{})
	slow := serveDocument(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// A server that sends the header and the start of the document, and
	// then nothing.
	stalled := serveDocument(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"openapi": "3.1.0", `))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	// Only the servers that never finish are given a short time; the others
	// have the time a loaded machine needs to send sixteen mebibytes.
	const short = 500 * time.Millisecond
	cases := []struct {
		url     string
		timeout time.Duration
		want    string
	}{
		{notFound, 0, `answered with status 404 rather than the document`},
		{tooLarge, 0, `exceeds the client's limit`},
		{declaredTooLarge, 0, `exceeds the client's limit`},
		{notADocument, 0, `muzak: http://127\.0\.0\.1:\d+: the OpenAPI document is malformed`},
		{hostileMember, 0, `muzak: http://127\.0\.0\.1:\d+: the OpenAPI document is malformed`},
		{slow, short, `muzak: http://127\.0\.0\.1:\d+ did not send the document within 500ms`},
		{stalled, short, `muzak: http://127\.0\.0\.1:\d+ did not send the document within 500ms`},
		{closedURL + "/openapi.json", 0, `muzak: GET http://127\.0\.0\.1:\d+ `},
		// A metadata address is refused before anything is sent, even
		// though loopback and private addresses are allowed.
		{"http://169.254.169.254/openapi.json", 0, `169\.254\.169\.254.*(metadata|link-local)`},
	}
	for _, c := range cases {
		console, _, _ := testConsole(t.TempDir())
		if c.timeout != 0 {
			console.fetchTimeout = c.timeout
		}
		r := runWith(console, "routes", "-url", c.url)
		r.expect(t, exitFailure, c.want)
		checkPrintable(t, r.stderr, true)
	}
}

func TestRoutesCommandLineErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runIn(t, dir, "routes").expect(t, exitUsage, `routes needs a document, from -url or -file`)
	runIn(t, dir, "routes", "-url", "http://x", "-file", "y").expect(t, exitUsage, `routes reads one document, and was given both -url and -file`)
	runIn(t, dir, "routes", "-file", "a.json", "b.json").expect(t, exitUsage, `routes takes no arguments, and was given "b.json"`)
	for _, url := range []string{"localhost:8080/openapi.json", "ftp://example.com/x", "/openapi.json", "http://", "http://[::1"} {
		runIn(t, dir, "routes", "-url", url).expect(t, exitUsage, `-url is ".*", and it has to be an absolute http or https URL`)
	}
	runIn(t, dir, "routes", "-file", "missing.json").expect(t, exitFailure, `muzak: the document cannot be opened: .*missing\.json`)
	writeDocument(t, dir, "bad.json", []byte(`{"openapi": "3.0.3", "info": {"title": "x", "version": "1"}, "paths": {}}`))
	runIn(t, dir, "routes", "-file", "bad.json").expect(t, exitFailure, `muzak: bad\.json: the document says it is OpenAPI "3\.0\.3"`)
	c, _, _ := testConsole(dir)
	c.stdin = strings.NewReader("{")
	runWith(c, "routes", "-file", "-").expect(t, exitFailure, `muzak: standard input: the OpenAPI document is malformed`)
}

func TestRoutesOfADocumentWithNoOperations(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "empty.json", []byte(`{"openapi": "3.1.0", "info": {"title": "x", "version": "1"}, "paths": {}}`))
	r := runIn(t, dir, "routes", "-file", "empty.json")
	r.expect(t, exitOK)
	if r.stdout != "The document describes no operations.\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
}

// hostileDocument names every operation, path, summary, scheme and scope with
// what a terminal would obey: colour, a cursor move, a title change, a bell,
// line and column breaks, and the Unicode controls that reorder text.
const hostileDocument = `{
  "openapi": "3.1.0",
  "info": {"title": "\u001b]0;owned\u0007", "version": "1"},
  "paths": {
    "/a\u001b[2J\u001b[Hb": {
      "get": {
        "operationId": "op\u001b[31mred\u001b[0m",
        "summary": "line\nbreak\tcolumn\r\u0085\u2028\u202eevil\u2066\u200b\u0000\u007f",
        "security": [{"sch\u001beme": ["sc\nope", "\u202e"]}],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/b": {
      "post": {
        "operationId": "plain",
        "summary": "\u001b[1A\u001b[2Kfake row",
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

func TestRoutesEscapesWhatTheDocumentHolds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "hostile.json", []byte(hostileDocument))
	r := runIn(t, dir, "routes", "-file", "hostile.json")
	r.expect(t, exitOK)
	checkPrintable(t, r.stdout, true)
	if lines := strings.Count(r.stdout, "\n"); lines != 3 {
		t.Errorf("the table has %d lines, want a header and two rows:\n%s", lines, r.stdout)
	}
	for _, want := range []string{`/a\x1b[2J\x1b[Hb`, `op\x1b[31mred\x1b[0m`, `line\nbreak\tcolumn\r`, `sch\x1beme[sc\nope,`, `\x1b[1A\x1b[2Kfake row`} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the table does not show %q:\n%s", want, r.stdout)
		}
	}
}

func TestRoutesCutsALongSummary(t *testing.T) {
	t.Parallel()
	op := &muzak.Operation{Summary: strings.Repeat("word ", 40)}
	got := summary(op)
	if len([]rune(got)) != maxSummary || !strings.HasSuffix(got, "...") {
		t.Errorf("summary is %d runes: %q", len([]rune(got)), got)
	}
	exact := &muzak.Operation{Summary: strings.Repeat("x", maxSummary)}
	if got := summary(exact); got != exact.Summary {
		t.Errorf("a summary of exactly %d runes was cut to %q", maxSummary, got)
	}
	if got := summary(&muzak.Operation{Deprecated: true}); got != "(deprecated)" {
		t.Errorf("a deprecated operation with no summary shows %q", got)
	}
	if got := summary(&muzak.Operation{}); got != "-" {
		t.Errorf("an operation with no summary shows %q", got)
	}
}

func TestRoutesRendersSecurity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   []muzak.SecurityRequirement
		want string
	}{
		{nil, "-"},
		{[]muzak.SecurityRequirement{}, "none"},
		{[]muzak.SecurityRequirement{{}}, "none"},
		{[]muzak.SecurityRequirement{{}, {"key": nil}}, "none | key"},
		{[]muzak.SecurityRequirement{{"oauth": {"b", "a"}}}, "oauth[b,a]"},
		{[]muzak.SecurityRequirement{{"z": nil, "a": {}}, {"m": {"s"}}}, "a+z | m[s]"},
	}
	for _, c := range cases {
		if got := security(c.in); got != c.want {
			t.Errorf("security(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRoutesSkipsANullPathItem(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	doc := &muzak.Document{Paths: map[string]*muzak.PathItem{"/gone": nil, "/here": {Get: &muzak.Operation{OperationID: "here"}}}}
	if err := writeRoutes(&out, doc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "/gone") || !strings.Contains(out.String(), "/here") {
		t.Errorf("table:\n%s", out.String())
	}
}

func FuzzRoutes(f *testing.F) {
	f.Add([]byte(hostileDocument))
	f.Add([]byte(`{"openapi": "3.1.0", "info": {"title": "x", "version": "1"}, "paths": {}}`))
	f.Add([]byte(`{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/x":{"get":{"operationId":"a\tb","responses":{}}}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := muzak.ReadDocument(bytes.NewReader(data))
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := writeRoutes(&out, doc); err != nil {
			t.Fatal(err)
		}
		checkPrintable(t, out.String(), true)
		// Each byte of the document is at most four of the table, and the
		// columns pad each row by a bounded amount per operation.
		if out.Len() > 8*len(data)+200 {
			t.Fatalf("a document of %d bytes printed %d", len(data), out.Len())
		}
	})
}
