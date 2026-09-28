package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

type encodedUserIn struct {
	ID string `path:"id"`
}

type encodedUserOut struct {
	Route string `json:"route"`
	ID    string `json:"id"`
}

// encodedPaths spells "admin" three ways a client can choose between, every
// one of which net/http decodes to the same segment.
var encodedPaths = []string{"admin", "%61dmin", "admi%6E", "%61%64%6d%69%6e"}

// TestEncodedSegmentReachesStaticRoute is the regression test for a guard
// bypass: the tree compared a static segment with the escaped request text, so
// "/users/%61dmin" missed the guarded "/users/admin" and was answered by the
// public "/users/{id}" with id "admin". Every spelling must now reach the
// static route, and with it the guard.
func TestEncodedSegmentReachesStaticRoute(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/users/admin", func(_ *Context, _ Empty) (encodedUserOut, error) {
		return encodedUserOut{Route: "static", ID: "admin"}, nil
	}, WithDependencies(RequireBearerToken("s3cret")))
	app.Get("/users/{id}", func(_ *Context, in encodedUserIn) (encodedUserOut, error) {
		return encodedUserOut{Route: "param", ID: in.ID}, nil
	})
	mustBuild(t, app)

	for _, spelling := range encodedPaths {
		rec := do(t, app, "GET", "/users/"+spelling)
		assertStatus(t, rec, http.StatusUnauthorized)

		req := httptest.NewRequest("GET", "/users/"+spelling, nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec = doRequest(t, app, req)
		assertJSON(t, rec, `{"route":"static","id":"admin"}`)
	}

	// An encoded separator is data, never structure: it stays inside the one
	// segment the parameter captures, and is decoded for the handler.
	assertJSON(t, do(t, app, "GET", "/users/admin%2F"), `{"route":"param","id":"admin/"}`)
	assertJSON(t, do(t, app, "GET", "/users/b%6Fb"), `{"route":"param","id":"bob"}`)
}

// TestEncodedSegmentMatchesEverywhereTheTreeIs covers the other ways a request
// reaches the tree: through an included router's prefix, a versioned path, the
// automatic HEAD, OPTIONS and 405 answers, and the WebSocket and event stream
// routes. Each must see the decoded segment the same way a plain route does.
func TestEncodedSegmentMatchesEverywhereTheTreeIs(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)

	admin := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	admin.Get("/panel", func(_ *Context, _ Empty) (encodedUserOut, error) {
		return encodedUserOut{Route: "panel"}, nil
	}, WithVersion("1"))
	app.Include(admin, WithPrefix("/admin"))
	app.Get("/{section}/panel", func(_ *Context, _ Empty) (encodedUserOut, error) {
		return encodedUserOut{Route: "public"}, nil
	}, WithVersion("1"))

	app.WS("/ws/admin", func(_ *Context, _ Empty, _ *WSConn) error { return nil },
		WithDependencies(RequireBearerToken("s3cret")), WithVersion("1"))
	app.SSE("/events/admin", func(_ *Context, _ Empty, _ *SSEStream[encodedUserOut]) error { return nil },
		WithDependencies(RequireBearerToken("s3cret")), WithVersion("1"))
	app.Get("/{section}/{id}", func(_ *Context, _ Empty) (encodedUserOut, error) {
		return encodedUserOut{Route: "public"}, nil
	}, WithVersion("1"))
	mustBuild(t, app)

	for _, target := range []string{
		"/v1/admin/panel", "/%761/admin/panel", "/v1/%61dmin/panel", "/v1/admin/p%61nel",
		"/v1/ws/%61dmin", "/v1/events/admi%6E", "/v1/%77s/admin",
	} {
		assertStatus(t, do(t, app, "GET", target), http.StatusUnauthorized)
	}
	assertStatus(t, do(t, app, "HEAD", "/v1/admin/p%61nel"), http.StatusUnauthorized)

	rec := do(t, app, "OPTIONS", "/v1/%61dmin/panel")
	assertStatus(t, rec, http.StatusNoContent)
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want the static route's methods", allow)
	}
	rec = do(t, app, "DELETE", "/v1/%61dmin/panel")
	assertStatus(t, rec, http.StatusMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow = %q, want the static route's methods", allow)
	}

	// The parameter routes still answer everything the static ones do not.
	assertStatus(t, do(t, app, "GET", "/v1/public/panel"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/v1/ws/%61dmin2"), http.StatusOK)
}

// TestPatternSpellingIsDecodedToo pins the other half of comparing decoded
// segments: a pattern is decoded when it is registered, so it answers the
// request it describes, and one that could never match anything is refused.
func TestPatternSpellingIsDecodedToo(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/discounts/100%25", func(_ *Context, _ Empty) (encodedUserOut, error) {
		return encodedUserOut{Route: "percent"}, nil
	})
	app.Get("/caf\u00e9", func(_ *Context, _ Empty) (encodedUserOut, error) {
		return encodedUserOut{Route: "cafe"}, nil
	})
	mustBuild(t, app)
	assertJSON(t, do(t, app, "GET", "/discounts/100%25"), `{"route":"percent","id":""}`)
	assertJSON(t, do(t, app, "GET", "/caf%C3%A9"), `{"route":"cafe","id":""}`)

	for _, pattern := range []string{"/a%2Fb", "/100%"} {
		broken := New(quietOptions())
		broken.Get(pattern, func(_ *Context, _ Empty) (Empty, error) { return Empty{}, nil })
		if msg := buildError(t, broken); !strings.Contains(msg, "percent") {
			t.Errorf("Build error for %q = %q, want it to explain the encoding", pattern, msg)
		}
	}

	twice := New(quietOptions())
	twice.Get("/users/admin", func(_ *Context, _ Empty) (Empty, error) { return Empty{}, nil })
	twice.Get("/users/%61dmin", func(_ *Context, _ Empty) (Empty, error) { return Empty{}, nil })
	if msg := buildError(t, twice); !strings.Contains(msg, "duplicate") {
		t.Errorf("two spellings of one path built, error %q", msg)
	}
}

// TestNotFoundMessageSurvivesAnyPath is the regression test for a 404 that
// became a 500: the message quoted the decoded path, GET /%ff decodes to a
// byte that is not UTF-8, and the JSON envelope could not be encoded. Every
// request nothing answers must get its 404, or its 405, in the envelope.
func TestNotFoundMessageSurvivesAnyPath(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/things", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Get("/things/{id}", func(*Context, encodedUserIn) (Empty, error) { return Empty{}, nil })
	app.Static("/static", StaticOptions{FS: fstest.MapFS{"a.txt": {Data: []byte("a")}}})
	mustBuild(t, app)

	for target, quoted := range map[string]string{
		"/%ff":            "GET /%ff",
		"/%FF%FE/x":       "GET /%FF%FE/x",
		"/a%00b":          "GET /a%00b",
		"/line%0Abreak":   "GET /line%0Abreak",
		"/static/%ff":     "GET /static/%ff",
		"/caf%C3%A9":      "GET /caf%C3%A9",
		"/plain/path":     "GET /plain/path",
		"/things/a/%ff/b": "GET /things/a/%ff/b",
	} {
		rec := do(t, app, "GET", target)
		assertStatus(t, rec, http.StatusNotFound)
		if got := decodeError(t, rec).Error.Message; got != "no route matches "+quoted {
			t.Errorf("GET %s: message = %q, want it to quote %q", target, got, quoted)
		}
	}

	// A method net/http would never admit, handed to ServeHTTP directly.
	odd := httptest.NewRequest("GET", "/nothing", nil)
	odd.Method = "G\xffT"
	rec := doRequest(t, app, odd)
	assertStatus(t, rec, http.StatusNotFound)
	if got := decodeError(t, rec).Error.Message; got != `no route matches "G\xffT" /nothing` {
		t.Errorf("message = %q, want the method quoted", got)
	}
	odd = httptest.NewRequest("GET", "/things", nil)
	odd.Method = "P\x00ST"
	assertStatus(t, doRequest(t, app, odd), http.StatusMethodNotAllowed)
	odd = httptest.NewRequest("GET", "/static/a.txt", nil)
	odd.Method = "P\x00ST"
	assertStatus(t, doRequest(t, app, odd), http.StatusMethodNotAllowed)
}
