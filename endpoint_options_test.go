package muzak

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// headerEcho answers with the headers a guard or a handler would read beside
// the input.
type headerEcho struct {
	Auth   string `json:"auth"`
	Key    string `json:"key"`
	Cookie string `json:"cookie"`
	Bound  string `json:"bound"`
	Other  string `json:"other"`
}

type cookieIn struct {
	Session string `cookie:"session"`
	Tenant  string `header:"X-Tenant"`
}

func TestCallHeaderAddsWhatTheInputDoesNotBind(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[cookieIn, headerEcho](http.MethodGet, "/h")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(ctx *Context, in cookieIn) (headerEcho, error) {
			other, _ := ctx.Request().Cookie("other")
			out := headerEcho{Auth: ctx.Header("Authorization"), Key: ctx.Header("Idempotency-Key"), Cookie: in.Session, Bound: in.Tenant}
			if other != nil {
				out.Other = other.Value
			}
			return out, nil
		})
	})
	got, err := ep.Call(context.Background(), client, cookieIn{Session: "s1", Tenant: "acme"},
		CallHeader("authorization", "Bearer token"),
		CallHeader("Idempotency-Key", "k-1"),
		CallHeader("Cookie", "other=o; session=forged"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The input's own cookie is the one the server reads, though a cookie of
	// the same name was added after it.
	want := headerEcho{Auth: "Bearer token", Key: "k-1", Cookie: "s1", Bound: "acme", Other: "o"}
	assertEqualValue(t, got, want)

	// With no cookie of its own, the added one is sent as it is.
	got, err = ep.Call(context.Background(), client, cookieIn{}, CallHeader("Cookie", "other=only"))
	if err != nil || got.Other != "only" {
		t.Fatalf("got %+v, %v", got, err)
	}

	for _, tc := range []struct {
		name, value, fragment string
	}{
		{"X-Tenant", "x", "which the input binds itself"},
		{"x-tenant", "x", "which the input binds itself"},
		{"Host", "evil.example", "which a call writes itself"},
		{"Content-Type", "text/plain", "which a call writes itself"},
		{"Content-Length", "1", "which a call writes itself"},
		{"Bad Name", "x", "not a valid header name"},
		{"", "x", "not a valid header name"},
		{"Authorization", "Bearer x\r\nX-Evil: 1", "control character"},
		{"Authorization", " padded", "begins or ends"},
	} {
		_, err := ep.Call(context.Background(), client, cookieIn{}, CallHeader(tc.name, tc.value))
		assertCallRefused(t, err, tc.fragment)
	}
}

// validatedIn declares rules, and reads a dependency in them.
type validatedIn struct {
	Name  string `json:"name"`
	Limit *int   `query:"limit" required:"true"`
	Who   Dep[depUser]
	Note  string  `json:"note"`
	Score float64 `json:"score"`
}

func (in *validatedIn) Validate(v *Validation) {
	v.String(&in.Name).Required().MinLen(3)
	v.Number(&in.Score).Min(0)
	// A rule reading a dependency sees its zero value before sending.
	if in.Who.Get().Name == "blocked" {
		v.Reject(&in.Note, "is blocked")
	}
}

func TestValidateFirstChecksBeforeSending(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[validatedIn, Empty](http.MethodPost, "/v")
	seen := &capture[validatedIn]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, in validatedIn) (Empty, error) {
			seen.record(in)
			return Empty{}, nil
		}, Needs(func(*Context) (depUser, error) { return depUser{Name: "server"}, nil }))
	})
	bad := validatedIn{Name: "ab", Score: -1}

	_, err := ep.Call(context.Background(), client, bad, ValidateFirst())
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("got %v, want a ValidationError", err)
	}
	fields := []string{}
	for _, d := range verr.Details {
		fields = append(fields, d.Location+":"+d.Field)
	}
	if strings.Join(fields, ",") != "query:limit,body:name,body:score" {
		t.Fatalf("details %v", verr.Details)
	}
	if seen.count() != 0 {
		t.Fatal("a value that failed validation was sent")
	}

	// Without the option the server is the one to refuse, with the same
	// details.
	_, err = ep.Call(context.Background(), client, bad)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnprocessableEntity || len(remote.Details) != 3 {
		t.Fatalf("got %v %+v", err, remote)
	}

	ten := 10
	if _, err := ep.Call(context.Background(), client, validatedIn{Name: "abc", Limit: &ten}, ValidateFirst()); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if seen.count() != 1 {
		t.Fatal("a valid value was not sent")
	}
}

func TestValidateFirstHonoursSkipValidationButNotRequiredness(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[validatedIn, Empty](http.MethodPost, "/v", SkipValidation())
	seen := &capture[validatedIn]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, in validatedIn) (Empty, error) {
			seen.record(in)
			return Empty{}, nil
		}, Needs(func(*Context) (depUser, error) { return depUser{}, nil }))
	})
	ten := 10
	if _, err := ep.Call(context.Background(), client, validatedIn{Name: "x", Limit: &ten}, ValidateFirst()); err != nil {
		t.Fatalf("the rules ran though the endpoint skips them: %v", err)
	}
	_, err := ep.Call(context.Background(), client, validatedIn{Name: "x"}, ValidateFirst())
	var verr *ValidationError
	if !errors.As(err, &verr) || len(verr.Details) != 1 || verr.Details[0].Field != "limit" {
		t.Fatalf("got %v", err)
	}
}

func TestValidateFirstReportsMissingFormValuesAndFiles(t *testing.T) {
	t.Parallel()
	type in struct {
		ID    string   `path:"id"`
		Query *string  `query:"q"`
		Name  string   `form:"name"`
		Tags  []string `form:"tag"`
		File  []byte   `file:"file"`
		Files [][]byte `file:"files"`
		Opt   []byte   `file:"opt" required:"false"`
	}
	ep := NewEndpoint[in, Empty](http.MethodPost, "/f/{id}")
	client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: "http://127.0.0.1:1"})
	// The path and the optional query parameter are no failure: a path
	// parameter is checked when the path is written, and an optional one
	// may be left out.
	_, err := ep.Call(context.Background(), client, in{Files: [][]byte{}}, ValidateFirst())
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("got %v", err)
	}
	got := []string{}
	for _, d := range verr.Details {
		got = append(got, d.Location+":"+d.Field+" "+d.Issue)
	}
	want := "form:tag is required,file:file is required,file:files is required"
	if strings.Join(got, ",") != want {
		t.Fatalf("details %q, want %q", strings.Join(got, ","), want)
	}
}

// tenantIn binds a header a client's Propagate may also write.
type tenantIn struct {
	Tenant *string `header:"X-Tenant"`
}

func TestEndpointRefusesAPropagateThatRewritesABoundHeader(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[tenantIn, tenantIn](http.MethodGet, "/tenant")
	sets := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, v tenantIn) (tenantIn, error) { return v, nil })
	}, func(o *ClientOptions) {
		// As the documentation of DefaultPropagate composes one.
		o.Propagate = func(ctx context.Context, h http.Header) {
			DefaultPropagate(ctx, h)
			h.Set("X-Tenant", "from-propagate")
		}
	})
	mine := "mine"
	_, err := ep.Call(context.Background(), sets, tenantIn{Tenant: &mine})
	assertCallRefused(t, err, "X-Tenant", "Propagate")
	if strings.Contains(err.Error(), "mine") || strings.Contains(err.Error(), "from-propagate") {
		t.Errorf("the refusal quotes a value: %v", err)
	}
	// A field left out receives what Propagate adds, as documented.
	got, err := ep.Call(context.Background(), sets, tenantIn{})
	if err != nil || got.Tenant == nil || *got.Tenant != "from-propagate" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Plain requests are Propagate's to change, as they always were.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+sets.baseURL.Host+"/tenant", nil)
	req.Header.Set("X-Tenant", "mine")
	resp, err := sets.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

// epPanickingIn has rules that panic on the zero input they are compiled
// against, as rules that read a Dep without allowing for its absence do.
type epPanickingIn struct {
	Q    string `query:"q"`
	User Dep[*depUser]
}

func (in *epPanickingIn) Validate(*Validation) {
	if in.User.Get() == nil {
		panic("the rules met a user that was not there")
	}
}

func TestEndpointPanicsAlikeOnEveryCallItCannotCompile(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[epPanickingIn, Empty](http.MethodGet, "/panics")
	client := rawServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// The first call meets the rules' panic while compiling the call, and
	// every later one meets it again, rather than a plan that was never
	// built.
	for i := range 3 {
		func() {
			defer func() {
				if got := recover(); got != "the rules met a user that was not there" {
					t.Errorf("call %d panicked with %v, want the rules' own panic", i+1, got)
				}
			}()
			_, _ = ep.Call(context.Background(), client, epPanickingIn{})
		}()
	}
}

func TestEndpointCompilesOnceUnderConcurrentFirstCalls(t *testing.T) {
	t.Parallel()
	type in struct {
		ID string `path:"id"`
		Q  []int  `query:"q"`
	}
	ep := NewEndpoint[in, in](http.MethodGet, "/c/{id}")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, v in) (in, error) { return v, nil })
	})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 64 {
		wg.Go(func() {
			sent := in{ID: "id-" + strconv.Itoa(i), Q: []int{i, -i}}
			got, err := ep.Call(context.Background(), client, sent)
			if err == nil && (got.ID != sent.ID || len(got.Q) != 2 || got.Q[0] != i) {
				err = errors.New("the answer belongs to another call: " + got.ID)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestEndpointCarriesTenThousandEntries(t *testing.T) {
	t.Parallel()
	type in struct {
		Q []int    `query:"q"`
		H []string `header:"X-H"`
	}
	type out struct {
		Q, H int
	}
	ep := NewEndpoint[in, out](http.MethodGet, "/many")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, v in) (out, error) {
			for i, q := range v.Q {
				if q != i {
					return out{}, errors.New("an entry moved")
				}
			}
			return out{Q: len(v.Q), H: len(v.H)}, nil
		})
	})
	const n = 10_000
	sent := in{Q: make([]int, n), H: make([]string, n)}
	for i := range n {
		sent.Q[i] = i
		sent.H[i] = "h" + strconv.Itoa(i%10)
	}
	got, err := ep.Call(context.Background(), client, sent)
	if err != nil || got != (out{Q: n, H: n}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestImplementCostsWhatHandleCosts(t *testing.T) {
	// The comparison is of exact allocation counts, which exactAllocs reads
	// steadily under the race detector too.
	type in struct {
		ID string `path:"id"`
		Q  int    `query:"q"`
	}
	ep := NewEndpoint[in, in](http.MethodGet, "/a/{id}")
	handler := func(_ *Context, v in) (in, error) { return v, nil }
	implemented := New(quietOptions())
	implemented.Implement(ep, handler)
	handled := New(quietOptions())
	handled.Get("/a/{id}", handler)
	mustBuild(t, implemented)
	mustBuild(t, handled)
	measure := func(app *App) float64 {
		return exactAllocs(200, func() {
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/a/x?q=1", nil))
		})
	}
	if a, b := measure(implemented), measure(handled); a != b {
		t.Fatalf("a request to an implemented endpoint allocates %v times, and to the same route registered with Get %v", a, b)
	}
}

func TestEndpointCallsLeaveNothingRunning(t *testing.T) {
	type in struct {
		ID   string `path:"id"`
		Body string `json:"body"`
	}
	ep := NewEndpoint[in, in](http.MethodPost, "/l/{id}")
	stream := NewEndpoint[Empty, Stream](http.MethodGet, "/s")
	fail := NewEndpoint[Empty, Empty](http.MethodGet, "/fail")
	app := New(quietOptions())
	app.Implement(ep, func(_ *Context, v in) (in, error) { return v, nil })
	app.Implement(stream, func(*Context, Empty) (Stream, error) {
		return Stream{Body: strings.NewReader("stream")}, nil
	})
	app.Implement(fail, func(*Context, Empty) (Empty, error) { return Empty{}, NotFound("") })
	mustBuild(t, app)
	srv := httptest.NewServer(app)
	client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: srv.URL})
	for i := range 50 {
		if _, err := ep.Call(context.Background(), client, in{ID: strconv.Itoa(i), Body: "b"}); err != nil {
			t.Fatal(err)
		}
		s, err := stream.Call(context.Background(), client, Empty{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, s.Body)
		_ = s.Body.(io.Closer).Close()
		if _, err := fail.Call(context.Background(), client, Empty{}); err == nil {
			t.Fatal("want an error")
		}
		if _, err := ep.Call(context.Background(), client, in{ID: ""}); !errors.Is(err, ErrCallRefused) {
			t.Fatal(err)
		}
	}
	_ = client.Close()
	srv.Close()
	assertNoGoroutineLeaks(t)
}
