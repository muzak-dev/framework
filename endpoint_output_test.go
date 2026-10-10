package muzak

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rawServer serves handler as it is, standing in for a service that answers
// however it likes, and returns a client pointed at it.
func rawServer(t *testing.T, handler http.HandlerFunc, opts ...func(*ClientOptions)) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	options := ClientOptions{AllowPrivateNetworks: true, BaseURL: srv.URL, MaxAttempts: 1}
	for _, opt := range opts {
		opt(&options)
	}
	client := NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestEndpointReturnsBytesWithTheirTypeAndName(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Bytes](http.MethodGet, "/report")
	for _, sent := range []Bytes{
		{ContentType: "text/csv; charset=utf-8", Data: []byte("a,b\n1,2\n"), Filename: "report.csv", Download: true},
		{ContentType: "application/pdf", Data: []byte("%PDF"), Filename: "r\xc3\xa9sum\xc3\xa9.pdf"},
		{ContentType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
	} {
		client := endpointServer(t, func(app *App) {
			app.Implement(ep, func(*Context, Empty) (Bytes, error) { return sent, nil })
		})
		got, err := ep.Call(context.Background(), client, Empty{})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		assertEqualValue(t, got, sent)
	}
}

func TestEndpointCleansTheFileNameAnotherServerOffers(t *testing.T) {
	t.Parallel()
	bytesEP := NewEndpoint[Empty, Bytes](http.MethodGet, "/report")
	streamEP := NewEndpoint[Empty, Stream](http.MethodGet, "/report")
	for disposition, want := range map[string]string{
		// A name offered for saving is a single name: a caller that saves
		// under it must not be led out of the directory it saves into.
		`attachment; filename="../../.ssh/authorized_keys"`: ".._.._.ssh_authorized_keys",
		`attachment; filename="C:\\Windows\\win.ini"`:       "C:_Windows_win.ini",
		// A control character, a line break or an override that draws the
		// name other than it is, as RFC 8187 lets a server spell them.
		`attachment; filename*=UTF-8''report%E2%80%AEfdp.exe%0D%0A`:  "reportfdp.exe",
		`attachment; filename*=UTF-8''%20%20spaced%00%20name.txt%20`: "spaced name.txt",
		`inline; filename="plain.txt"`:                               "plain.txt",
	} {
		client := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", disposition)
			_, _ = w.Write([]byte("data"))
		})
		got, err := bytesEP.Call(context.Background(), client, Empty{})
		if err != nil || got.Filename != want {
			t.Errorf("%s: got %q, %v; want %q", disposition, got.Filename, err, want)
		}
		stream, err := streamEP.Call(context.Background(), client, Empty{})
		if err != nil {
			t.Fatalf("%s: %v", disposition, err)
		}
		_ = stream.Body.(io.Closer).Close()
		if stream.Filename != want {
			t.Errorf("%s: the stream's name is %q, want %q", disposition, stream.Filename, want)
		}
	}
}

func TestEndpointReturnsAStreamToReadAndPassOn(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Stream](http.MethodGet, "/export")
	payload := strings.Repeat("streamed ", 10_000)
	upstream := endpointServer(t, func(app *App) {
		app.Implement(ep, func(*Context, Empty) (Stream, error) {
			return Stream{ContentType: "application/zip", Body: strings.NewReader(payload), Length: int64(len(payload)),
				Filename: "export.zip", Download: true}, nil
		})
	})
	got, err := ep.Call(context.Background(), upstream, Empty{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.ContentType != "application/zip" || got.Length != int64(len(payload)) || got.Filename != "export.zip" || !got.Download {
		t.Fatalf("got %+v", got)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil || string(body) != payload {
		t.Fatalf("read %d bytes, %v", len(body), err)
	}
	if err := got.Body.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}

	// A handler that answers with the stream it was handed passes it on, and
	// the framework closes it once it is sent.
	proxy := endpointServer(t, func(app *App) {
		app.Implement(ep, func(ctx *Context, _ Empty) (Stream, error) {
			return ep.Call(ctx.Context(), upstream, Empty{})
		})
	})
	got, err = ep.Call(context.Background(), proxy, Empty{})
	if err != nil {
		t.Fatalf("Call through the proxy: %v", err)
	}
	defer func() { _ = got.Body.(io.Closer).Close() }()
	body, _ = io.ReadAll(got.Body)
	if string(body) != payload || got.Filename != "export.zip" {
		t.Fatalf("the proxy passed on %d bytes, %+v", len(body), got)
	}
}

func TestEndpointReportsAStreamThatFailedAsARemoteError(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Stream](http.MethodGet, "/gone")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(*Context, Empty) (Stream, error) { return Stream{}, NotFound("no such export") })
	})
	got, err := ep.Call(context.Background(), client, Empty{})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != CodeNotFound || got.Body != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestEndpointReturnsTheRedirectRatherThanFollowingIt(t *testing.T) {
	t.Parallel()
	var followed atomic.Int32
	post := NewEndpoint[Empty, Redirect](http.MethodPost, "/login")
	get := NewEndpoint[Empty, Redirect](http.MethodGet, "/old")
	permanent := NewEndpoint[Empty, Redirect](http.MethodGet, "/moved")
	client := endpointServer(t, func(app *App) {
		app.Implement(post, func(*Context, Empty) (Redirect, error) { return Redirect{To: "/home?tab=1"}, nil })
		app.Implement(get, func(*Context, Empty) (Redirect, error) { return Redirect{To: "/new"}, nil })
		app.Implement(permanent, func(*Context, Empty) (Redirect, error) {
			return Redirect{To: "/elsewhere", Status: http.StatusPermanentRedirect}, nil
		})
		for _, path := range []string{"/home", "/new", "/elsewhere"} {
			app.Get(path, func(*Context, Empty) (Empty, error) {
				followed.Add(1)
				return Empty{}, nil
			})
		}
	})
	for _, tc := range []struct {
		ep   Endpoint[Empty, Redirect]
		want Redirect
	}{
		{post, Redirect{To: "/home?tab=1", Status: http.StatusSeeOther}},
		{get, Redirect{To: "/new", Status: http.StatusFound}},
		{permanent, Redirect{To: "/elsewhere", Status: http.StatusPermanentRedirect}},
	} {
		got, err := tc.ep.Call(context.Background(), client, Empty{})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		assertEqualValue(t, got, tc.want)
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("a redirect was followed %d times", n)
	}

	// An endpoint whose output is not a redirect follows one, as every
	// request the client sends does.
	type out struct {
		Where string `json:"where"`
	}
	moved := NewEndpoint[Empty, out](http.MethodGet, "/moved-json")
	raw := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved-json" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"where":"`+r.URL.Path+`"}`)
	})
	got, err := moved.Call(context.Background(), raw, Empty{})
	if err != nil || got.Where != "/final" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestEndpointRefusesAnAnswerThatIsNotARedirect(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Redirect](http.MethodGet, "/x")
	client := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Case") == "no-location" {
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "fine")
	})
	_, err := ep.Call(context.Background(), client, Empty{})
	assertErrorMentions(t, err, "answered with status 200 where the endpoint redirects")
	_, err = ep.Call(context.Background(), client, Empty{}, CallHeader("X-Case", "no-location"))
	assertErrorMentions(t, err, "status 302 and no Location")
}

func TestEndpointReturnsHTMLEmptyAndNoContent(t *testing.T) {
	t.Parallel()
	page := NewEndpoint[Empty, HTML](http.MethodGet, "/page")
	empty := NewEndpoint[Empty, Empty](http.MethodDelete, "/thing")
	type out struct {
		N int `json:"n"`
	}
	noContent := NewEndpoint[Empty, out](http.MethodPut, "/thing", Status(http.StatusNoContent))
	head := NewEndpoint[Empty, out](http.MethodHead, "/thing")
	client := endpointServer(t, func(app *App) {
		app.Implement(page, func(*Context, Empty) (HTML, error) { return "<p>hi \xc3\xbc</p>", nil })
		app.Implement(empty, func(*Context, Empty) (Empty, error) { return Empty{}, nil })
		app.Implement(noContent, func(*Context, Empty) (out, error) { return out{N: 1}, nil })
		app.Implement(head, func(*Context, Empty) (out, error) { return out{N: 1}, nil })
	})
	html, err := page.Call(context.Background(), client, Empty{})
	if err != nil || html != "<p>hi \xc3\xbc</p>" {
		t.Fatalf("got %q, %v", html, err)
	}
	if _, err := empty.Call(context.Background(), client, Empty{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, err := noContent.Call(context.Background(), client, Empty{}); err != nil || got != (out{}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := head.Call(context.Background(), client, Empty{}); err != nil || got != (out{}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestEndpointRefusesAnAnswerThatIsNotItsOutput(t *testing.T) {
	t.Parallel()
	type out struct {
		N int `json:"n"`
	}
	ep := NewEndpoint[Empty, out](http.MethodGet, "/x")
	for body, fragment := range map[string]string{
		"text/html|<html>":               `answered with "text/html" rather than JSON`,
		"|{}":                            `answered with "" rather than JSON`,
		"application/json|{":             "is not JSON that fits muzak.out",
		`application/json|{"n":"x"}`:     "is not JSON that fits",
		`application/json|{"n":1} {}`:    "is not JSON that fits",
		`application/json|{"n":1,"n":2}`: "is not JSON that fits",
	} {
		contentType, text, _ := strings.Cut(body, "|")
		client := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			} else {
				w.Header()["Content-Type"] = nil
			}
			_, _ = io.WriteString(w, text)
		})
		got, err := ep.Call(context.Background(), client, Empty{})
		assertErrorMentions(t, err, fragment)
		if got != (out{}) {
			t.Errorf("%q: a failed decode returned %+v", body, got)
		}
	}
}

func TestEndpointBoundsTheAnswer(t *testing.T) {
	t.Parallel()
	type out struct {
		S string `json:"s"`
	}
	ep := NewEndpoint[Empty, out](http.MethodGet, "/big")
	big := NewEndpoint[Empty, Bytes](http.MethodGet, "/big")
	body := `{"s":"` + strings.Repeat("x", 4096) + `"}`
	for _, chunked := range []bool{false, true} {
		client := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if chunked {
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, body)
		}, func(o *ClientOptions) { o.MaxResponseBytes = 1024 })
		_, err := ep.Call(context.Background(), client, Empty{})
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("chunked %v: got %v, want ErrResponseTooLarge", chunked, err)
		}
		_, err = big.Call(context.Background(), client, Empty{})
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("chunked %v: Bytes got %v, want ErrResponseTooLarge", chunked, err)
		}
	}
}

func TestEndpointReadsTheDefaultErrorEnvelope(t *testing.T) {
	t.Parallel()
	type withID struct {
		ID   string `path:"id"`
		Name string `json:"name"`
	}
	missing := NewEndpoint[withID, Empty](http.MethodPost, "/users/{id}")
	client := endpointServer(t, func(app *App) {
		app.Implement(missing, func(*Context, withID) (Empty, error) {
			return Empty{}, NotFound("no such user")
		})
	})
	_, err := missing.Call(context.Background(), client, withID{ID: "42"})
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("got %v, want a RemoteError", err)
	}
	if remote.StatusCode != http.StatusNotFound || remote.Code != CodeNotFound || remote.Message != "no such user" || remote.RequestID == "" {
		t.Fatalf("got %+v", remote)
	}
	if !strings.HasSuffix(remote.Error(), "answered with status 404 (not_found)") {
		t.Fatalf("message %q", remote.Error())
	}
}

func TestEndpointReadsValidationDetailsAndProblemDetails(t *testing.T) {
	t.Parallel()
	type in struct {
		Limit int    `query:"limit"`
		Name  string `json:"name"`
	}
	// The client sends a string where the server's own type wants a number,
	// which is the version skew a RemoteError exists to report.
	type skewed struct {
		Limit string `query:"limit"`
		Name  string `json:"name"`
	}
	server := NewEndpoint[in, Empty](http.MethodPost, "/v")
	caller := NewEndpoint[skewed, Empty](http.MethodPost, "/v")
	for _, problem := range []bool{false, true} {
		opts := quietOptions()
		if problem {
			opts.ProblemDetails = &ProblemOptions{}
		}
		app := New(opts)
		app.Implement(server, func(*Context, in) (Empty, error) { return Empty{}, nil })
		mustBuild(t, app)
		srv := httptest.NewServer(app)
		t.Cleanup(srv.Close)
		client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: srv.URL})
		_, err := caller.Call(context.Background(), client, skewed{Limit: "ten"})
		var remote *RemoteError
		if !errors.As(err, &remote) {
			t.Fatalf("problem %v: got %v", problem, err)
		}
		if remote.Code != CodeValidationError || remote.Message == "" || remote.RequestID == "" || len(remote.Details) != 1 {
			t.Fatalf("problem %v: got %+v", problem, remote)
		}
		if d := remote.Details[0]; d.Field != "limit" || d.Location != "query" || d.Issue != "must be a valid integer" {
			t.Fatalf("problem %v: detail %+v", problem, d)
		}
	}
}

func TestRemoteErrorReadsHostileBodiesWithoutTrustingThem(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Empty](http.MethodGet, "/x")
	manyDetails := `{"error":{"code":"c","details":[` + strings.TrimSuffix(strings.Repeat(`{"field":"f"},`, 2000), ",") + `]}}`
	for _, tc := range []struct {
		name, contentType, body string
		code, message           string
		details                 int
		inMessage               string
	}{
		{"not JSON", "application/json", "<html>", "", "", 0, ""},
		{"plain text", "text/plain", `{"error":{"code":"c"}}`, "", "", 0, ""},
		{"no content type", "", `{"error":{"code":"c"}}`, "", "", 0, ""},
		{"malformed content type", "application/json; =", `{"error":{"code":"c"}}`, "", "", 0, ""},
		{"wrong member type", "application/json", `{"error":{"code":5,"message":"m"}}`, "", "", 0, ""},
		{"duplicate member", "application/json", `{"error":{"code":"a","code":"b"}}`, "", "", 0, ""},
		{"invalid UTF-8", "application/json", "{\"error\":{\"code\":\"\xff\"}}", "", "", 0, ""},
		{"error is a string", "application/json", `{"error":"boom"}`, "", "", 0, ""},
		{"truncated", "application/json", `{"error":{"code":"c","message":"` + strings.Repeat("m", maxRemoteErrorBody) + `"}}`, "", "", 0, ""},
		{"line break in the code", "application/json", `{"error":{"code":"evil\r\nINJECTED: yes"}}`, "evil\r\nINJECTED: yes", "", 0, ""},
		{"long code", "application/json", `{"error":{"code":"` + strings.Repeat("c", 65) + `"}}`, strings.Repeat("c", 65), "", 0, ""},
		{"too many details", "application/json", manyDetails, "c", "", maxRemoteDetails, "(c)"},
		{"extra members", "application/vnd.api+json", `{"error":{"code":"x.y:z-1","message":"m","extra":[1]},"more":{}}`, "x.y:z-1", "m", 0, "(x.y:z-1)"},
		{"problem with a title only", ProblemContentType, `{"title":"Teapot","code":"teapot","request_id":"r"}`, "teapot", "Teapot", 0, "(teapot)"},
		{"problem that is not JSON", ProblemContentType, `nope`, "", "", 0, ""},
		{"empty", "application/json", ``, "", "", 0, ""},
		{"deep", "application/json", strings.Repeat("[", 20000) + strings.Repeat("]", 20000), "", "", 0, ""},
	} {
		client := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			if tc.contentType != "" {
				w.Header().Set("Content-Type", tc.contentType)
			} else {
				w.Header()["Content-Type"] = nil
			}
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, tc.body)
		})
		_, err := ep.Call(context.Background(), client, Empty{})
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.StatusCode != http.StatusTeapot {
			t.Fatalf("%s: got %v", tc.name, err)
		}
		if remote.Code != tc.code || remote.Message != tc.message || len(remote.Details) != tc.details {
			t.Errorf("%s: code %q message %q details %d", tc.name, remote.Code, remote.Message, len(remote.Details))
		}
		if len(remote.Body) > maxRemoteErrorBody {
			t.Errorf("%s: kept %d bytes of the body", tc.name, len(remote.Body))
		}
		message := remote.Error()
		if strings.ContainsAny(message, "\r\n") || len(message) > 200 {
			t.Errorf("%s: the message carries the body: %q", tc.name, message)
		}
		if tc.inMessage != "" && !strings.Contains(message, tc.inMessage) {
			t.Errorf("%s: message %q lacks %q", tc.name, message, tc.inMessage)
		}
	}
}

func TestDoJSONReadsTheErrorEnvelopeToo(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(*Context, Empty) (Empty, error) { return Empty{}, NotFound("gone") })
	mustBuild(t, app)
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	client := NewClient(ClientOptions{AllowPrivateNetworks: true})
	_, err := client.GetJSON[Empty](context.Background(), srv.URL+"/x")
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != CodeNotFound || remote.Message != "gone" {
		t.Fatalf("got %v %+v", err, remote)
	}
}

func TestEndpointOutputDecodesDurationsAsTheServerWritesThem(t *testing.T) {
	t.Parallel()
	type out struct {
		D time.Duration `json:"d"`
	}
	ep := NewEndpoint[Empty, out](http.MethodGet, "/d")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(*Context, Empty) (out, error) { return out{D: 1500 * time.Millisecond}, nil })
	})
	got, err := ep.Call(context.Background(), client, Empty{})
	if err != nil || got.D != 1500*time.Millisecond {
		t.Fatalf("got %+v, %v", got, err)
	}
}
