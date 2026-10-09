package muzak

import (
	"net/http"
	"strings"
	"testing"
)

// A Bytes value is written as it is, under the type the handler named, and
// never through the JSON encoder.

func TestBytesIsWrittenAsGiven(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/csv", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{ContentType: "text/csv; charset=utf-8", Data: []byte("a,b\n1,2\n")}, nil
	})
	rec := do(t, app, http.MethodGet, "/csv")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != "a,b\n1,2\n" {
		t.Errorf("body = %q, want the bytes as given", got)
	}
	for name, want := range map[string]string{
		"Content-Type":           "text/csv; charset=utf-8",
		"Content-Length":         "8",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want none when neither a name nor a download was asked for", got)
	}
}

func TestBytesWithNoTypeIsOctetStream(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/blob", func(ctx *Context, _ Empty) (Bytes, error) {
		// Markup a client wrote, which a browser that guessed would render.
		return Bytes{Data: []byte("<script>alert(1)</script>")}, nil
	})
	rec := do(t, app, http.MethodGet, "/blob")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream, which a browser saves rather than renders", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// The security headers set nosniff too, but an application may turn them
// off, and a body of a client's bytes is what sniffing would turn into a page.
func TestBytesSetsNosniffWithoutTheSecurityHeaders(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.DisableSecurityHeaders = true
	app := New(opts)
	app.Get("/blob", func(ctx *Context, _ Empty) (Bytes, error) { return Bytes{Data: []byte("x")}, nil })
	rec := do(t, app, http.MethodGet, "/blob")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestBytesNilDataIsAnEmptyBody(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/empty", func(ctx *Context, _ Empty) (Bytes, error) { return Bytes{ContentType: "text/plain"}, nil })
	rec := do(t, app, http.MethodGet, "/empty")
	assertStatus(t, rec, http.StatusOK)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
		t.Errorf("body = %q with Content-Length %q, want empty with 0", rec.Body.String(), rec.Header().Get("Content-Length"))
	}
}

// A content type is the one value in these responses a handler might build
// from what a client sent, and a line break in it is a response splitting
// attempt. Every value that is not a media type fails the request instead of
// reaching the header, and the cause is logged.
func TestBytesRefusesAContentTypeThatIsNotAMediaType(t *testing.T) {
	t.Parallel()
	for _, contentType := range []string{
		"text/html\r\nSet-Cookie: session=stolen",
		"text/html\nX-Injected: 1",
		"text/html\rX-Injected: 1",
		"text/plain\x00",
		"text/plain; charset=\"utf-8\r\n\"",
		"text",
		"/plain",
		"text/",
		" text/plain",
		"text /plain",
		"text/plain;",
		"text/plain; charset",
		"text/plain; charset=",
		"text/plain; =utf-8",
		"text/plain; charset=\"unterminated",
		"text/plain;; charset=utf-8",
		"text/pl\xc3\xa4in",
		"text/plain\x7f",
		"text/{plain}",
		strings.Repeat("a", maxMediaTypeLength) + "/b",
	} {
		t.Run(contentType, func(t *testing.T) {
			t.Parallel()
			logger, logs := captureLogger(t)
			opts := quietOptions()
			opts.Logger = logger
			app := New(opts)
			app.Get("/x", func(ctx *Context, _ Empty) (Bytes, error) {
				return Bytes{ContentType: contentType, Data: []byte("<b>hi</b>")}, nil
			})
			rec := do(t, app, http.MethodGet, "/x")
			assertStatus(t, rec, http.StatusInternalServerError)
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want the error envelope's own", got)
			}
			if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("X-Injected") != "" {
				t.Errorf("a header was injected: %v", rec.Header())
			}
			if strings.Contains(rec.Body.String(), "<b>") {
				t.Errorf("body = %q, want the error envelope and not the handler's bytes", rec.Body.String())
			}
			if !strings.Contains(logs.String(), "is not a media type") {
				t.Errorf("the cause was not logged: %s", logs.String())
			}
		})
	}
}

func TestValidMediaTypeAcceptsWhatRFC9110Allows(t *testing.T) {
	t.Parallel()
	for _, contentType := range []string{
		"text/plain",
		"application/vnd.api+json",
		"text/csv; charset=utf-8",
		"text/csv;charset=utf-8",
		"text/csv ;\tcharset=utf-8 ",
		`multipart/form-data; boundary="a b;c=d"`,
		`text/plain; note="say \"hi\""`,
		"image/*",
		"*/*",
		"application/x.custom-type_1",
	} {
		if !validMediaType(contentType) {
			t.Errorf("validMediaType(%q) = false, want true", contentType)
		}
	}
}

func TestBytesHonoursTheDeclaredAndTheImperativeStatus(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/created", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{ContentType: "text/plain", Data: []byte("made")}, nil
	}, Status(http.StatusCreated))
	app.Post("/accepted", func(ctx *Context, _ Empty) (Bytes, error) {
		ctx.SetStatus(http.StatusAccepted)
		return Bytes{ContentType: "text/plain", Data: []byte("queued")}, nil
	}, Status(http.StatusCreated))

	rec := do(t, app, http.MethodPost, "/created")
	assertStatus(t, rec, http.StatusCreated)
	if rec.Body.String() != "made" {
		t.Errorf("body = %q, want made", rec.Body.String())
	}
	rec = do(t, app, http.MethodPost, "/accepted")
	assertStatus(t, rec, http.StatusAccepted)
	if rec.Body.String() != "queued" {
		t.Errorf("body = %q, want queued", rec.Body.String())
	}
}

func TestBytesSendsNoBodyAt204And304(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		app := New(quietOptions())
		app.Get("/x", func(ctx *Context, _ Empty) (Bytes, error) {
			ctx.SetStatus(status)
			return Bytes{ContentType: "text/plain", Data: []byte("never sent")}, nil
		})
		rec := do(t, app, http.MethodGet, "/x")
		assertStatus(t, rec, status)
		if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" || rec.Header().Get("Content-Length") != "" {
			t.Errorf("%d: body %q, Content-Type %q, Content-Length %q, want nothing", status,
				rec.Body.String(), rec.Header().Get("Content-Type"), rec.Header().Get("Content-Length"))
		}
	}
}

// A HEAD request runs the GET handler and is answered with its headers, the
// length included, and no body.
func TestBytesAnswersHead(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{ContentType: "text/plain", Data: []byte("twelve bytes")}, nil
	})
	server := newWireServer(t, app)
	res := server.do(t, http.MethodHead, "/x")
	if res.StatusCode != http.StatusOK || res.ContentLength != 12 || res.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("HEAD = %d with length %d and type %q, want 200, 12 and text/plain",
			res.StatusCode, res.ContentLength, res.Header.Get("Content-Type"))
	}
}

func TestBytesOffersADownload(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/export", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{ContentType: "text/csv", Data: []byte("a\n"), Filename: "export.csv", Download: true}, nil
	})
	app.Get("/inline", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{ContentType: "text/plain", Data: []byte("a\n"), Filename: "notes.txt"}, nil
	})
	app.Get("/unnamed", func(ctx *Context, _ Empty) (Bytes, error) {
		return Bytes{Data: []byte("a\n"), Download: true}, nil
	})
	for path, want := range map[string]string{
		"/export":  `attachment; filename="export.csv"`,
		"/inline":  `inline; filename="notes.txt"`,
		"/unnamed": "attachment",
	} {
		if got := do(t, app, http.MethodGet, path).Header().Get("Content-Disposition"); got != want {
			t.Errorf("%s: Content-Disposition = %q, want %q", path, got, want)
		}
	}
}

// A body the handler wrote itself is the response; the value returned after
// it is ignored, as an encoded one is.
func TestBytesAfterTheHandlerWroteIsIgnored(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Bytes, error) {
		ctx.ResponseWriter().WriteHeader(http.StatusTeapot)
		_, _ = ctx.ResponseWriter().Write([]byte("mine"))
		return Bytes{Data: []byte("ignored")}, nil
	})
	rec := do(t, app, http.MethodGet, "/x")
	assertStatus(t, rec, http.StatusTeapot)
	if rec.Body.String() != "mine" {
		t.Errorf("body = %q, want only what the handler wrote", rec.Body.String())
	}
}

// An error renderer may answer with Bytes, which is how an application sends
// a failure in a format of its own.
func TestErrorRendererMayAnswerWithBytes(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ErrorRenderer = func(ctx *Context, err error) (int, any) {
		return http.StatusNotFound, Bytes{ContentType: "text/plain; charset=utf-8", Data: []byte("not here")}
	}
	app := New(opts)
	app.Get("/x", okHandler)
	rec := do(t, app, http.MethodGet, "/missing")
	assertStatus(t, rec, http.StatusNotFound)
	if rec.Body.String() != "not here" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("got %q as %q, want the renderer's bytes as text/plain", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want an error's no-store", rec.Header().Get("Cache-Control"))
	}
}

func TestValidMediaTypeRefusesBrokenQuotedStrings(t *testing.T) {
	t.Parallel()
	for _, contentType := range []string{
		`text/plain; a="x\`,
		"text/plain; a=\"x\\\x01\"",
		"text/plain; a=\"x\x01\"",
		`text/plain; a="x"y`,
		`text/plain; a=""; b`,
	} {
		if validMediaType(contentType) {
			t.Errorf("validMediaType(%q) = true, want false", contentType)
		}
	}
}
