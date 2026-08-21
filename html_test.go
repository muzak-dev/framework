package badele

import (
	"net/http"
	"strconv"
	"testing"
)

const uploadForm = `<body>
<form action="/files/" enctype="multipart/form-data" method="post">
<input name="files" type="file" multiple>
<input type="submit">
</form>
</body>`

func TestHTMLBypassesJSONEncoding(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/", func(ctx *Context, _ Empty) (HTML, error) {
		return HTML(uploadForm), nil
	})

	rec := do(t, mustBuild(t, app), "GET", "/")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != uploadForm {
		t.Errorf("body = %q, want the document as written", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(uploadForm)) {
		t.Errorf("Content-Length = %q, want %d", got, len(uploadForm))
	}
}

func TestHTMLKeepsTheRouteStatusAndHeaders(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/pages", func(ctx *Context, _ Empty) (HTML, error) {
		ctx.SetHeader("Content-Type", "text/html; charset=iso-8859-1")
		ctx.SetHeader("X-Page", "new")
		return HTML("<p>made</p>"), nil
	}, Status(http.StatusCreated))

	rec := do(t, mustBuild(t, app), "POST", "/pages")
	assertStatus(t, rec, http.StatusCreated)
	if got := rec.Header().Get("X-Page"); got != "new" {
		t.Errorf("X-Page = %q, want the header the handler set", got)
	}
	// A handler that sets its own content type keeps it, exactly as a JSON
	// route does.
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=iso-8859-1" {
		t.Errorf("Content-Type = %q, want the one the handler set", got)
	}
}

func TestHTMLWritesNoBodyForAStatusThatCarriesNone(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/nothing", func(ctx *Context, _ Empty) (HTML, error) {
		return HTML("<p>ignored</p>"), nil
	}, Status(http.StatusNoContent))

	rec := do(t, mustBuild(t, app), "GET", "/nothing")
	assertStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want none", rec.Body.String())
	}
}

func TestHTMLIsDocumentedAsHTML(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/", func(ctx *Context, _ Empty) (HTML, error) { return HTML(uploadForm), nil })

	doc, err := mustBuild(t, app).Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	content := doc.Paths["/"].Get.Responses["200"].Content
	if _, described := content["application/json"]; described {
		t.Error("an HTML route was documented as returning JSON")
	}
	schema := content["text/html"].Schema
	if schema == nil || schema.Type != "string" {
		t.Errorf("text/html schema = %+v, want a string", schema)
	}
}

func TestHTMLErrorsAreStillJSON(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/fails", func(ctx *Context, _ Empty) (HTML, error) {
		return "", NewHTTPError(http.StatusNotFound, "no such page")
	})

	rec := do(t, mustBuild(t, app), "GET", "/fails")
	assertStatus(t, rec, http.StatusNotFound)
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the error envelope to stay JSON", got)
	}
	if message := decodeError(t, rec).Error.Message; message != "no such page" {
		t.Errorf("message = %q, want the error the handler returned", message)
	}
}
