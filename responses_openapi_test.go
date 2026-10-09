package muzak

import (
	"encoding/json/v2"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// The document describes what each response type actually sends: bytes as
// binary content of the types the route produces, and a redirect as a status
// and a Location with no body. These tests read the document as a client
// generator does, from the wire.

// servedDocument fetches the OpenAPI document an application serves and
// decodes it into plain values.
func servedDocument(t *testing.T, app *App) map[string]any {
	t.Helper()
	rec := do(t, app, http.MethodGet, "/openapi.json")
	assertStatus(t, rec, http.StatusOK)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	return doc
}

// responseAt returns the response an operation documents for status.
func responseAt(t *testing.T, doc map[string]any, path, method, status string) map[string]any {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	op, _ := item[method].(map[string]any)
	responses, _ := op["responses"].(map[string]any)
	response, ok := responses[status].(map[string]any)
	if !ok {
		t.Fatalf("%s %s documents no %s response; it documents %v", method, path, status, documentedKeys(responses))
	}
	return response
}

func documentedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// assertWire fails unless value encodes to the JSON want, ignoring order.
func assertWire(t *testing.T, what string, value any, want string) {
	t.Helper()
	var expected any
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatalf("the expected value is not JSON: %v", err)
	}
	if !reflect.DeepEqual(value, expected) {
		got, _ := json.Marshal(value, json.Deterministic(true))
		t.Errorf("%s =\n%s\nwant\n%s", what, got, want)
	}
}

func TestOpenAPIDescribesBinaryResponses(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/blob", func(ctx *Context, _ Empty) (Bytes, error) { return Bytes{}, nil })
	app.Get("/report", func(ctx *Context, _ Empty) (Bytes, error) { return Bytes{}, nil },
		Produces("text/csv; charset=utf-8", "application/pdf", "image/*"))
	app.Post("/export", func(ctx *Context, _ Empty) (Stream, error) { return Stream{}, nil },
		Produces("application/zip"), Status(http.StatusCreated))
	app.Get("/file", func(ctx *Context, _ Empty) (FileResponse, error) { return FileResponse{}, nil })
	doc := servedDocument(t, app)

	assertWire(t, "GET /blob 200", responseAt(t, doc, "/blob", "get", "200"), `{
		"description": "OK",
		"content": {"application/octet-stream": {"schema": {
			"type": "string", "format": "binary", "contentMediaType": "application/octet-stream"}}}
	}`)
	assertWire(t, "GET /report 200", responseAt(t, doc, "/report", "get", "200"), `{
		"description": "OK",
		"content": {
			"text/csv; charset=utf-8": {"schema": {"type": "string", "format": "binary", "contentMediaType": "text/csv; charset=utf-8"}},
			"application/pdf": {"schema": {"type": "string", "format": "binary", "contentMediaType": "application/pdf"}},
			"image/*": {"schema": {"type": "string", "format": "binary"}}
		}
	}`)
	assertWire(t, "POST /export 201", responseAt(t, doc, "/export", "post", "201"), `{
		"description": "Created",
		"content": {"application/zip": {"schema": {"type": "string", "format": "binary", "contentMediaType": "application/zip"}}}
	}`)
	assertWire(t, "GET /file 200", responseAt(t, doc, "/file", "get", "200"), `{
		"description": "OK",
		"content": {"application/octet-stream": {"schema": {
			"type": "string", "format": "binary", "contentMediaType": "application/octet-stream"}}}
	}`)
}

func TestOpenAPIDescribesRedirects(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	to := func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{To: "/"}, nil }
	app.Get("/r", to)
	app.Post("/r", to)
	app.Get("/moved", to, Status(http.StatusMovedPermanently))
	doc := servedDocument(t, app)

	location := `{"Location": {
		"description": "Where the client is sent: a path on this origin, or an absolute URL to a host the route allows.",
		"required": true,
		"schema": {"type": "string", "format": "uri-reference"}
	}}`
	for _, tc := range []struct{ path, method, status, description string }{
		{"/r", "get", "302", "Found"},
		{"/r", "post", "303", "See Other"},
		{"/moved", "get", "301", "Moved Permanently"},
	} {
		assertWire(t, tc.method+" "+tc.path, responseAt(t, doc, tc.path, tc.method, tc.status),
			`{"description": "`+tc.description+`", "headers": `+location+`}`)
	}
	paths := doc["paths"].(map[string]any)
	responses := paths["/r"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
	if _, documented := responses["200"]; documented {
		t.Error("a redirecting route still documents a 200")
	}
}

// A model declared for another status is described the way the same type
// would be as a handler's own output.
func TestOpenAPIDescribesResponseModelsOfTheseTypes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/item", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{}, nil },
		WithResponseModel[Bytes](http.StatusPartialContent, "A range of the item"),
		WithResponseModel[Redirect](http.StatusTemporaryRedirect, "The item lives elsewhere for now"))
	doc := servedDocument(t, app)
	assertWire(t, "206", responseAt(t, doc, "/item", "get", "206"), `{
		"description": "A range of the item",
		"content": {"application/octet-stream": {"schema": {
			"type": "string", "format": "binary", "contentMediaType": "application/octet-stream"}}}
	}`)
	redirect := responseAt(t, doc, "/item", "get", "307")
	if _, hasContent := redirect["content"]; hasContent {
		t.Errorf("a redirect model documents a body: %v", redirect)
	}
	if headers, _ := redirect["headers"].(map[string]any); headers["Location"] == nil {
		t.Errorf("a redirect model documents no Location: %v", redirect)
	}
}

// The document's own JSON carries the new members only where they apply, so
// a route that uses none of these types is described exactly as before.
func TestOpenAPIJSONRoutesAreUnchanged(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/item", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"contentMediaType", `"headers"`, "binary"} {
		if strings.Contains(string(raw), member) {
			t.Errorf("a JSON-only document mentions %s", member)
		}
	}
}

func TestProducesIsABuildErrorWhereItDescribesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		register func(*App)
		want     string
	}{
		"a JSON route": {func(app *App) {
			app.Get("/x", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{}, nil }, Produces("text/csv"))
		}, "Produces describes a Bytes, Stream or FileResponse body"},
		"an HTML route": {func(app *App) {
			app.Get("/x", func(ctx *Context, _ Empty) (HTML, error) { return "", nil }, Produces("text/html"))
		}, "Produces describes a Bytes, Stream or FileResponse body"},
		"a redirect": {func(app *App) {
			app.Get("/x", func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{}, nil }, Produces("text/plain"))
		}, "Produces describes a Bytes, Stream or FileResponse body"},
		"no types": {func(app *App) {
			app.Get("/x", func(ctx *Context, _ Empty) (Bytes, error) { return Bytes{}, nil }, Produces())
		}, "Produces was given no media types"},
		"a type that is not one": {func(app *App) {
			app.Get("/x", func(ctx *Context, _ Empty) (Bytes, error) { return Bytes{}, nil }, Produces("csv"))
		}, `Produces was given "csv"`},
		"a type with a line break": {func(app *App) {
			app.Get("/x", func(ctx *Context, _ Empty) (Bytes, error) { return Bytes{}, nil }, Produces("text/csv\r\nX: y"))
		}, "which is not a media type"},
	} {
		app := New(quietOptions())
		tc.register(app)
		if msg := buildError(t, app); !strings.Contains(msg, tc.want) {
			t.Errorf("%s: build error = %q, want it to say %q", name, msg, tc.want)
		}
	}
}

// A pointer to one of these types would be encoded as JSON, which is never
// what the handler meant.
func TestPointerToAResponseTypeIsABuildError(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/b", func(ctx *Context, _ Empty) (*Bytes, error) { return nil, nil })
	app.Get("/s", func(ctx *Context, _ Empty) (*Stream, error) { return nil, nil })
	app.Get("/r", func(ctx *Context, _ Empty) (*Redirect, error) { return nil, nil })
	app.Get("/f", func(ctx *Context, _ Empty) (*FileResponse, error) { return nil, nil })
	msg := buildError(t, app)
	for _, name := range []string{"Bytes", "Stream", "Redirect", "FileResponse"} {
		if !strings.Contains(msg, "returns *muzak."+name+", which would be encoded as JSON; return muzak."+name+" by value") {
			t.Errorf("the build error does not name *muzak.%s: %s", name, msg)
		}
	}
}

// Every mistake on one route is reported together, as the rest of the build
// errors are.
func TestResponseBuildErrorsAreJoined(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{}, nil },
		Status(http.StatusOK), RedirectHosts("https://bad"), Produces("text/plain"))
	msg := buildError(t, app)
	for _, want := range []string{"declares status 200", "RedirectHosts was given", "Produces describes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the build error does not say %q: %s", want, msg)
		}
	}
}

// An event stream's body is its events, which Produces cannot describe.
func TestProducesIsABuildErrorOnAnEventStream(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.SSE("/events", func(_ *Context, _ Empty, stream *SSEStream[Bytes]) error { return nil }, Produces("text/plain"))
	if msg := buildError(t, app); !strings.Contains(msg, "Produces describes") {
		t.Errorf("build error = %q, want Produces refused", msg)
	}
}
