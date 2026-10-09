package muzak

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

// TestTypedResponsesSettleTheirReleases holds Bytes, Stream, Redirect and
// FileResponse to the contract every other response keeps with Acquire: the
// releases run once the response is known to be sendable and before anything
// is written, so a success commits, a response that turns out not to be
// sendable rolls back, and a release that fails replaces the success. Without
// it, the releases fell through to the safety net that runs when the Context
// goes back to the pool, after the response, and were told the request
// failed, so a transaction rolled back behind every file, stream and redirect
// a client had already been told succeeded.
func TestTypedResponsesSettleTheirReleases(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"report.csv": {Data: []byte("a,b\n1,2\n")}}
	outputs := map[string]func(*Context, Empty) (any, error){
		"bytes": func(*Context, Empty) (any, error) {
			return Bytes{ContentType: "text/plain", Data: []byte("hello")}, nil
		},
		"stream": func(*Context, Empty) (any, error) {
			return Stream{ContentType: "text/plain", Body: io.NopCloser(strings.NewReader("hello"))}, nil
		},
		"redirect": func(*Context, Empty) (any, error) { return Redirect{To: "/next"}, nil },
		"file":     func(*Context, Empty) (any, error) { return FileResponse{FS: files, Name: "report.csv"}, nil },
	}
	register := func(app *App, name string, log *releaseLog, releaseErr error) {
		switch name {
		case "bytes":
			app.Get("/"+name, func(ctx *Context, in Empty) (Bytes, error) {
				v, err := outputs[name](ctx, in)
				return v.(Bytes), err
			}, acquireAs[relA](log, "a", releaseErr))
		case "stream":
			app.Get("/"+name, func(ctx *Context, in Empty) (Stream, error) {
				v, err := outputs[name](ctx, in)
				return v.(Stream), err
			}, acquireAs[relA](log, "a", releaseErr))
		case "redirect":
			app.Get("/"+name, func(ctx *Context, in Empty) (Redirect, error) {
				v, err := outputs[name](ctx, in)
				return v.(Redirect), err
			}, acquireAs[relA](log, "a", releaseErr))
		case "file":
			app.Get("/"+name, func(ctx *Context, in Empty) (FileResponse, error) {
				v, err := outputs[name](ctx, in)
				return v.(FileResponse), err
			}, acquireAs[relA](log, "a", releaseErr))
		}
	}
	wantStatus := map[string]int{"bytes": http.StatusOK, "stream": http.StatusOK, "redirect": http.StatusFound, "file": http.StatusOK}

	for name := range outputs {
		t.Run(name+" commits on success", func(t *testing.T) {
			t.Parallel()
			log := newReleaseLog()
			app := New(quietOptions())
			register(app, name, log, nil)
			mustBuild(t, app)
			assertStatus(t, do(t, app, http.MethodGet, "/"+name, ""), wantStatus[name])
			if failure := log.failure(t, "a"); failure != nil {
				t.Errorf("the release was told the request failed: %v", failure)
			}
			assertEvents(t, log, "acquire a, release a")
		})

		t.Run(name+" lets a failing release replace the success", func(t *testing.T) {
			t.Parallel()
			log := newReleaseLog()
			app := New(quietOptions())
			register(app, name, log, errors.New("the commit failed: secret detail"))
			mustBuild(t, app)
			rec := do(t, app, http.MethodGet, "/"+name, "")
			assertStatus(t, rec, http.StatusInternalServerError)
			if strings.Contains(rec.Body.String(), "secret detail") || rec.Header().Get("Location") != "" ||
				strings.Contains(rec.Body.String(), "hello") {
				t.Errorf("the failed release leaked into the response: %q, Location %q", rec.Body.String(), rec.Header().Get("Location"))
			}
		})
	}

	// What turns out not to be sendable is a failure the releases are told of,
	// so a transaction is rolled back rather than committed behind an error.
	refused := map[string]func(*App, *releaseLog){
		"a refused redirect target": func(app *App, log *releaseLog) {
			app.Get("/x", func(*Context, Empty) (Redirect, error) {
				return Redirect{To: "//evil.example"}, nil
			}, acquireAs[relA](log, "a", nil))
		},
		"a missing file": func(app *App, log *releaseLog) {
			app.Get("/x", func(*Context, Empty) (FileResponse, error) {
				return FileResponse{FS: files, Name: "missing.csv"}, nil
			}, acquireAs[relA](log, "a", nil))
		},
		"a content type that is not one": func(app *App, log *releaseLog) {
			app.Get("/x", func(*Context, Empty) (Bytes, error) {
				return Bytes{ContentType: "text/plain\r\nX-Injected: 1", Data: []byte("x")}, nil
			}, acquireAs[relA](log, "a", nil))
		},
	}
	for name, register := range refused {
		t.Run(name+" rolls back", func(t *testing.T) {
			t.Parallel()
			log := newReleaseLog()
			app := New(quietOptions())
			register(app, log)
			mustBuild(t, app)
			rec := do(t, app, http.MethodGet, "/x", "")
			if rec.Code < 400 {
				t.Fatalf("status = %d, want a failure", rec.Code)
			}
			if log.failure(t, "a") == nil {
				t.Error("the release was told the request succeeded")
			}
		})
	}
}

// A Stream's body is read only after the route's releases have run, which is
// what lets a failing commit still turn the success into an error, and why a
// body must not read from a value an Acquire provider hands out: by the time
// it is read, that value has been released. The Stream documentation says so,
// and this pins the order it describes.
func TestStreamBodyIsReadAfterTheReleases(t *testing.T) {
	t.Parallel()
	log := newReleaseLog()
	app := New(quietOptions())
	app.Get("/x", func(*Context, Empty) (Stream, error) {
		return Stream{ContentType: "text/plain", Body: &loggedBody{Reader: strings.NewReader("hello"), log: log}}, nil
	}, acquireAs[relA](log, "a", nil))
	mustBuild(t, app)
	rec := do(t, app, http.MethodGet, "/x", "")
	assertStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want hello", rec.Body.String())
	}
	assertEvents(t, log, "acquire a, release a, read body")
}

// loggedBody records its first read in a release log.
type loggedBody struct {
	io.Reader
	log  *releaseLog
	read bool
}

func (b *loggedBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		b.log.add("read body")
	}
	return b.Reader.Read(p)
}
