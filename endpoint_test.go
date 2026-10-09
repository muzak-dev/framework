package muzak

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// refIn has one field of each shape a value can be refused in.
type refIn struct {
	ID   string    `path:"id"`
	Rest string    `path:"rest"`
	H    string    `header:"X-H"`
	HS   []string  `header:"X-HS"`
	HP   []*string `header:"X-HP"`
	C    string    `cookie:"c"`
	F    float64   `query:"f"`
	FS   []float32 `query:"fs"`
	Body string    `json:"body"`
}

func TestEndpointRefusesWhatARequestCannotCarry(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[refIn, Empty](http.MethodPost, "/ref/{id}/{rest...}")
	seen := &capture[refIn]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, in refIn) (Empty, error) {
			seen.record(in)
			return Empty{}, nil
		})
	})
	ok := refIn{ID: "id", Rest: "r"}
	cases := []struct {
		name      string
		edit      func(*refIn)
		fragments []string
	}{
		{"empty path parameter", func(in *refIn) { in.ID = "" }, []string{`path parameter "id"`, "is empty"}},
		{"dot path parameter", func(in *refIn) { in.ID = "." }, []string{"dot segment"}},
		{"dot dot path parameter", func(in *refIn) { in.ID = ".." }, []string{"dot segment"}},
		{"slash in a path parameter", func(in *refIn) { in.ID = "SECRET/x" }, []string{`holds a "/"`}},
		{"leading slash in the remainder", func(in *refIn) { in.Rest = "/etc/SECRET" }, []string{"segment 1", `"rest"`, "is empty"}},
		{"doubled slash in the remainder", func(in *refIn) { in.Rest = "a//b" }, []string{"segment 2", "is empty"}},
		{"trailing slash in the remainder", func(in *refIn) { in.Rest = "a/" }, []string{"segment 2", "is empty"}},
		{"dot in the remainder", func(in *refIn) { in.Rest = "a/./b" }, []string{"segment 2", "dot segment"}},
		{"traversal in the remainder", func(in *refIn) { in.Rest = "a/../SECRET" }, []string{"segment 2", "dot segment"}},
		{"header line break", func(in *refIn) { in.H = "SECRET\r\nX-Evil: 1" }, []string{"header X-H", "control character"}},
		{"header line feed", func(in *refIn) { in.H = "a\nb" }, []string{"control character"}},
		{"header NUL", func(in *refIn) { in.H = "a\x00b" }, []string{"control character"}},
		{"header DEL", func(in *refIn) { in.H = "a\x7fb" }, []string{"control character"}},
		{"header other control", func(in *refIn) { in.H = "a\x1bb" }, []string{"control character"}},
		{"header leading space", func(in *refIn) { in.H = " SECRET" }, []string{"begins or ends"}},
		{"header trailing tab", func(in *refIn) { in.H = "SECRET\t" }, []string{"begins or ends"}},
		{"header list comma", func(in *refIn) { in.HS = []string{"a", "SECRET,b"} }, []string{"header X-Hs", "comma"}},
		{"header list empty entry", func(in *refIn) { in.HS = []string{"a", ""} }, []string{"header X-Hs", "empty"}},
		{"header list padded entry", func(in *refIn) { in.HS = []string{" a"} }, []string{"entry 1 of the header X-Hs", "begins or ends"}},
		{"header list open quote", func(in *refIn) { in.HS = []string{`"open`, "b"} }, []string{"unbalanced double quote"}},
		{"header list line break", func(in *refIn) { in.HS = []string{"ok", "a\r\nb"} }, []string{"entry 2 of the header X-Hs", "control character"}},
		{"header list nil entry", func(in *refIn) { in.HP = []*string{nil} }, []string{`entry 1 of the header parameter "X-Hp"`, "nil pointer"}},
		{"cookie semicolon", func(in *refIn) { in.C = "SECRET;x=y" }, []string{`cookie "c"`}},
		{"cookie quote", func(in *refIn) { in.C = `a"b` }, []string{`cookie "c"`}},
		{"cookie backslash", func(in *refIn) { in.C = `a\b` }, []string{`cookie "c"`}},
		{"cookie outside ASCII", func(in *refIn) { in.C = "\xc3\xbc" }, []string{`cookie "c"`}},
		{"cookie control", func(in *refIn) { in.C = "a\x01" }, []string{`cookie "c"`}},
		{"not a number", func(in *refIn) { in.F = math.NaN() }, []string{`query parameter "f"`, "not a finite number"}},
		{"infinity", func(in *refIn) { in.F = math.Inf(1) }, []string{"not a finite number"}},
		{"negative infinity", func(in *refIn) { in.F = math.Inf(-1) }, []string{"not a finite number"}},
		{"infinite entry", func(in *refIn) { in.FS = []float32{1, float32(math.Inf(1))} }, []string{`entry 2 of the query parameter "fs"`}},
		{"body not UTF-8", func(in *refIn) { in.Body = "SECRET\xff" }, []string{"body could not be encoded"}},
	}
	for _, tc := range cases {
		in := ok
		tc.edit(&in)
		_, err := ep.Call(context.Background(), client, in)
		assertRefused(t, err, tc.fragments...)
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the refusal repeats the value: %v", tc.name, err)
		}
	}
	if n := seen.count(); n != 0 {
		t.Fatalf("the handler ran %d times for requests that should never have been sent", n)
	}
	// The same endpoint still works for a value it can carry.
	if _, err := ep.Call(context.Background(), client, ok); err != nil {
		t.Fatalf("Call: %v", err)
	}
}

func TestEndpointRefusesAPathParameterWithNoValue(t *testing.T) {
	t.Parallel()
	type in struct {
		ID   *string  `path:"id"`
		List []string `path:"list"`
	}
	ep := NewEndpoint[in, Empty](http.MethodGet, "/p/{id}/{list}")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(*Context, in) (Empty, error) { return Empty{}, nil })
	})
	id := "x"
	_, err := ep.Call(context.Background(), client, in{List: []string{"a"}})
	assertRefused(t, err, `path parameter "id" holds 0 values`)
	_, err = ep.Call(context.Background(), client, in{ID: &id, List: []string{"a", "b"}})
	assertRefused(t, err, `path parameter "list" holds 2 values`)
	if _, err := ep.Call(context.Background(), client, in{ID: &id, List: []string{"a"}}); err != nil {
		t.Fatalf("Call: %v", err)
	}
}

// onlyUnmarshal reads itself from text and cannot write itself.
type onlyUnmarshal struct{ v string }

func (o *onlyUnmarshal) UnmarshalText(b []byte) error { o.v = string(b); return nil }

// failingText cannot write itself, whatever it holds.
type failingText struct{}

func (failingText) MarshalText() ([]byte, error) { return nil, errors.New("the type refuses") }
func (*failingText) UnmarshalText([]byte) error  { return nil }

func TestEndpointReportsAnInputItCannotWrite(t *testing.T) {
	t.Parallel()
	client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: "http://127.0.0.1:1"})
	call := func(err error, fragments ...string) {
		t.Helper()
		assertErrorMentions(t, err, fragments...)
		if errors.Is(err, ErrCallRefused) {
			t.Errorf("a type that cannot be written is the declaration's fault, not the value's: %v", err)
		}
	}
	type textOnly struct {
		V onlyUnmarshal `query:"v"`
	}
	_, err := NewEndpoint[textOnly, Empty](http.MethodGet, "/x").Call(context.Background(), client, textOnly{})
	call(err, "field V", "has no MarshalText", "encoding.TextMarshaler")

	type file struct {
		F File `file:"f"`
	}
	_, err = NewEndpoint[file, Empty](http.MethodPost, "/x").Call(context.Background(), client, file{})
	call(err, "field F is a muzak.File", "declare it []byte")
	type files struct {
		F []File `file:"f"`
	}
	_, err = NewEndpoint[files, Empty](http.MethodPost, "/x").Call(context.Background(), client, files{})
	call(err, "field F is a []muzak.File")

	type unbound struct {
		Q string `query:"q"`
	}
	_, err = NewEndpoint[unbound, Empty](http.MethodGet, "/a/{id}").Call(context.Background(), client, unbound{})
	call(err, `path parameter "id" is bound by no field`)

	_, err = NewEndpoint[reservedHeaderIn, Empty](http.MethodGet, "/x").Call(context.Background(), client, reservedHeaderIn{})
	call(err, "header Host", "net/http writes itself")
	type contentType struct {
		CT   string `header:"Content-Type"`
		Body string `json:"body"`
	}
	_, err = NewEndpoint[contentType, Empty](http.MethodPost, "/x").Call(context.Background(), client, contentType{})
	call(err, "header Content-Type", "describe the body")

	type badNames struct {
		H string `header:"X Bad"`
		C string `cookie:"a b"`
	}
	_, err = NewEndpoint[badNames, Empty](http.MethodGet, "/x").Call(context.Background(), client, badNames{})
	call(err, `header "X Bad"`, `cookie "a b"`)

	type badForm struct {
		F string `form:"a\nb"`
	}
	_, err = NewEndpoint[badForm, Empty](http.MethodPost, "/x").Call(context.Background(), client, badForm{})
	call(err, "control character")

	type duplicates struct {
		A string `query:"q"`
		B string `query:"q"`
		C string `header:"X-A"`
		D string `header:"x-a"`
		E string `form:"f"`
		F string `form:"f"`
		G []byte `file:"f"`
	}
	_, err = NewEndpoint[duplicates, Empty](http.MethodPost, "/x").Call(context.Background(), client, duplicates{})
	call(err, `fields A and B both bind the query parameter "q"`, `fields C and D both bind the header parameter "x-a"`,
		`fields E and F both bind the form field "f"`, `fields E and G both bind the form field "f"`)

	_, err = NewEndpoint[Empty, FileResponse](http.MethodGet, "/x").Call(context.Background(), client, Empty{})
	call(err, "muzak.FileResponse", "declare the endpoint's output as muzak.Stream")

	// A type that cannot be written is refused wherever it sits, and a path
	// parameter it binds is reported once, for its type, and not again as
	// bound by nothing.
	type wrapped struct {
		ID onlyUnmarshal    `path:"id"`
		P  *onlyUnmarshal   `query:"p"`
		S  []onlyUnmarshal  `header:"X-S"`
		F  []*onlyUnmarshal `form:"f"`
	}
	_, err = NewEndpoint[wrapped, Empty](http.MethodPost, "/w/{id}").Call(context.Background(), client, wrapped{})
	call(err, "field ID", "field P", "field S", "field F")
	if strings.Contains(err.Error(), "bound by no field") {
		t.Errorf("the path parameter was reported twice: %v", err)
	}

	// What the server refuses to bind is refused here with its own words.
	_, err = NewEndpoint[int, Empty](http.MethodGet, "/x").Call(context.Background(), client, 0)
	call(err, "must be a struct")

	// The compiled answer is kept: the same error, without compiling again.
	ep := NewEndpoint[textOnly, Empty](http.MethodGet, "/x")
	_, first := ep.Call(context.Background(), client, textOnly{})
	_, second := ep.Call(context.Background(), client, textOnly{})
	if first == nil || !errors.Is(second, first) {
		t.Fatalf("want the same error twice, got %v and %v", first, second)
	}
}

func TestEndpointRefusesAValueItsTypeCannotWrite(t *testing.T) {
	t.Parallel()
	type in struct {
		V failingText `query:"v"`
	}
	ep := NewEndpoint[in, Empty](http.MethodGet, "/x")
	client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: "http://127.0.0.1:1"})
	_, err := ep.Call(context.Background(), client, in{})
	assertRefused(t, err, `query parameter "v"`, "could not be written as text", "the type refuses")

	// A number that is not finite is refused in the path and in a form as
	// it is in the query.
	type path struct {
		F float64 `path:"f"`
	}
	_, err = NewEndpoint[path, Empty](http.MethodGet, "/n/{f}").Call(context.Background(), client, path{F: math.NaN()})
	assertRefused(t, err, `path parameter "f"`, "not a finite number")
	type form struct {
		F []float32 `form:"f"`
	}
	_, err = NewEndpoint[form, Empty](http.MethodPost, "/n").Call(context.Background(), client, form{F: []float32{float32(math.Inf(-1))}})
	assertRefused(t, err, `entry 1 of the form parameter "f"`, "not a finite number")
}

type reservedHeaderIn struct {
	H string `header:"Host"`
}

func TestEndpointReportsEveryReservedHeader(t *testing.T) {
	t.Parallel()
	for name := range headersNotBound {
		// Lower-cased, since the name is compared once canonical.
		in := reflect.StructOf([]reflect.StructField{{
			Name: "H", Type: reflect.TypeFor[string](), Tag: reflect.StructTag(`header:"` + strings.ToLower(name) + `"`),
		}})
		plan, err := compileCall(in, emptyType, http.MethodGet, "/x", nil)
		if plan != nil || err == nil || !strings.Contains(err.Error(), "net/http writes itself") {
			t.Errorf("%s: want a refusal, got %v", name, err)
		}
	}
}

func TestEndpointLeavesAnAcceptTheInputBindsAsItWasSent(t *testing.T) {
	t.Parallel()
	type in struct {
		Accept *string `header:"Accept"`
	}
	type out struct {
		Accept *string `json:"accept"`
	}
	ep := NewEndpoint[in, out](http.MethodGet, "/accept")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, v in) (out, error) { return out{Accept: v.Accept}, nil })
	})
	got, err := ep.Call(context.Background(), client, in{})
	if err != nil || got.Accept != nil {
		t.Fatalf("an Accept left out arrived as %v, %v", got.Accept, err)
	}
	text := "text/csv"
	got, err = ep.Call(context.Background(), client, in{Accept: &text})
	if err != nil || got.Accept == nil || *got.Accept != text {
		t.Fatalf("got %v, %v", got.Accept, err)
	}
}

func TestEndpointLeavesAUserAgentTheInputBindsAsItWasSent(t *testing.T) {
	t.Parallel()
	type in struct {
		UA *string `header:"User-Agent"`
	}
	type out struct {
		UA *string `json:"ua"`
	}
	ep := NewEndpoint[in, out](http.MethodGet, "/ua")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, v in) (out, error) { return out{UA: v.UA}, nil })
	})
	// Left out, it arrives absent, rather than as net/http's own.
	got, err := ep.Call(context.Background(), client, in{})
	if err != nil || got.UA != nil {
		t.Fatalf("a User-Agent left out arrived as %v, %v", got.UA, err)
	}
	agent := "billing/1.2"
	got, err = ep.Call(context.Background(), client, in{UA: &agent})
	if err != nil || got.UA == nil || *got.UA != agent {
		t.Fatalf("got %v, %v", got.UA, err)
	}
	empty := ""
	_, err = ep.Call(context.Background(), client, in{UA: &empty})
	assertRefused(t, err, "User-Agent is empty")
}

func TestEndpointMayBindContentTypeWithoutABody(t *testing.T) {
	t.Parallel()
	type in struct {
		CT string `header:"Content-Type"`
	}
	ep := NewEndpoint[in, in](http.MethodGet, "/ct")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, v in) (in, error) { return v, nil })
	})
	got, err := ep.Call(context.Background(), client, in{CT: "text/plain"})
	if err != nil || got.CT != "text/plain" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestEndpointDeclarationMistakes(t *testing.T) {
	t.Parallel()
	client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: "http://127.0.0.1:1"})
	for _, tc := range []struct {
		method, path string
		fragments    []string
	}{
		{"GE T", "/x", []string{`method "GE T"`, "not an HTTP method"}},
		{"", "/x", []string{`method ""`}},
		{http.MethodGet, "x", []string{"GET x", `must begin with "/"`}},
		{http.MethodGet, "", []string{`must begin with "/"`}},
		{http.MethodGet, "/a/{id", []string{"GET /a/{id"}},
		{http.MethodGet, "/a/{x}/{x}", []string{"declared twice"}},
		{http.MethodGet, "/a/{rest...}/b", []string{"final segment"}},
		{http.MethodGet, "/a/%zz", []string{"percent-encoded"}},
	} {
		ep := NewEndpoint[Empty, Empty](tc.method, tc.path)
		_, err := ep.Call(context.Background(), client, Empty{})
		assertErrorMentions(t, err, tc.fragments...)

		app := New(quietOptions())
		app.Implement(ep, func(*Context, Empty) (Empty, error) { return Empty{}, nil })
		app.Get("/fine", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
		assertErrorMentions(t, app.Build(), tc.fragments...)
	}
}

func TestEndpointZeroValue(t *testing.T) {
	t.Parallel()
	var ep Endpoint[Empty, Empty]
	if ep.Method() != "" || ep.Path() != "" {
		t.Fatalf("the zero Endpoint names %q %q", ep.Method(), ep.Path())
	}
	client := NewClient(ClientOptions{BaseURL: "http://127.0.0.1:1"})
	if _, err := ep.Call(context.Background(), client, Empty{}); !errors.Is(err, errNoEndpoint) {
		t.Fatalf("got %v", err)
	}
	app := New(quietOptions())
	rt := app.Implement(ep, func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	if rt == nil {
		t.Fatal("Implement returned no route")
	}
	assertErrorMentions(t, app.Build(), "not declared with muzak.NewEndpoint")
}

func TestEndpointMethodAndPath(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Empty]("patch", "/things/{id}")
	if ep.Method() != http.MethodPatch || ep.Path() != "/things/{id}" {
		t.Fatalf("got %q %q", ep.Method(), ep.Path())
	}
}

func TestImplementRegistersAsHandleWouldWithTheEndpointsOptionsFirst(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Empty](http.MethodPost, "/things",
		Summary("from the endpoint"), Description("described once"), WithTags("shared"), Status(http.StatusCreated))
	app := New(quietOptions())
	rt := app.Implement(ep, func(*Context, Empty) (Empty, error) { return Empty{}, nil },
		Summary("from the implementation"), WithTags("local"))
	mustBuild(t, app)
	if rt.Method != http.MethodPost || rt.Path != "/things" || rt.Status != http.StatusCreated {
		t.Fatalf("route %s %s %d", rt.Method, rt.Path, rt.Status)
	}
	if rt.Summary != "from the implementation" || rt.Description != "described once" {
		t.Fatalf("summary %q description %q", rt.Summary, rt.Description)
	}
	if strings.Join(rt.Tags, ",") != "shared,local" {
		t.Fatalf("tags %v", rt.Tags)
	}
	assertStatus(t, do(t, app, http.MethodPost, "/things"), http.StatusCreated)

	// Identical to Handle: the same document either way.
	handled := New(quietOptions())
	handled.Handle(http.MethodPost, "/things", func(*Context, Empty) (Empty, error) { return Empty{}, nil },
		Summary("from the endpoint"), Description("described once"), WithTags("shared"), Status(http.StatusCreated),
		Summary("from the implementation"), WithTags("local"))
	a, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	b, err := handled.Document()
	if err != nil {
		t.Fatal(err)
	}
	aJSON, _ := a.Marshal()
	bJSON, _ := b.Marshal()
	if string(aJSON) != string(bJSON) {
		t.Fatalf("Implement and Handle describe the route differently:\n%s\n%s", aJSON, bJSON)
	}
}

func TestImplementUnderAPrefixIsCalledWithThePrefixInTheBaseURL(t *testing.T) {
	t.Parallel()
	type in struct {
		ID string `path:"id"`
	}
	ep := NewEndpoint[in, in](http.MethodGet, "/items/{id}")
	app := New(quietOptions())
	r := NewRouter()
	r.Implement(ep, func(_ *Context, v in) (in, error) { return v, nil })
	app.Include(r, WithPrefix("/api/v1"))
	mustBuild(t, app)
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	for _, base := range []string{srv.URL + "/api/v1", srv.URL + "/api/v1/"} {
		client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: base})
		got, err := ep.Call(context.Background(), client, in{ID: "a b"})
		if err != nil || got.ID != "a b" {
			t.Fatalf("%s: got %+v, %v", base, got, err)
		}
	}
}

func TestImplementPanicsOnceTheApplicationIsBuilt(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, New(quietOptions()))
	ep := NewEndpoint[Empty, Empty](http.MethodGet, "/late")
	recovered := catchPanic(func() {
		app.Implement(ep, func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	})
	if recovered == nil || !strings.Contains(recovered.(string), "Implement was called after the application was built") {
		t.Fatalf("recovered %v", recovered)
	}
}

func TestImplementWithANilHandlerIsABuildError(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Implement(NewEndpoint[Empty, Empty](http.MethodGet, "/x"), nil)
	assertErrorMentions(t, app.Build(), "GET /x: handler is nil")
}

func TestEndpointCallNeedsAClientWithABaseURL(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Empty](http.MethodGet, "/x")
	_, err := ep.Call(context.Background(), nil, Empty{})
	assertErrorMentions(t, err, "GET /x was called with a nil *Client")
	_, err = ep.Call(context.Background(), NewClient(ClientOptions{}), Empty{})
	assertErrorMentions(t, err, "has no BaseURL", "ClientOptions.BaseURL")
}

func TestClientBaseURLMustBeABase(t *testing.T) {
	t.Parallel()
	for raw, fragment := range map[string]string{
		"ftp://host/":         "not an absolute http or https URL",
		"/relative":           "not an absolute http or https URL",
		"http://":             "names no host",
		"http://u:p@host/":    "user information",
		"http://host/?q=1":    "query or a fragment",
		"http://host/?":       "query or a fragment",
		"http://host/#frag":   "query or a fragment",
		"http://host/#":       "query or a fragment",
		"http://host/%zz":     "is not a URL",
		"http://h\x00ost/api": "is not a URL",
	} {
		recovered := catchPanic(func() { NewClient(ClientOptions{BaseURL: raw}) })
		message, _ := recovered.(string)
		if !strings.Contains(message, fragment) || !strings.HasPrefix(message, "muzak: ClientOptions.BaseURL") {
			t.Errorf("%q: recovered %v, want it to mention %q", raw, recovered, fragment)
		}
	}
}

func TestEndpointReturnsTransportErrorsAsTheyAre(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[Empty, Empty](http.MethodGet, "/x")
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	// The default client refuses loopback, and says so with its own error.
	strict := NewClient(ClientOptions{BaseURL: srv.URL})
	_, err := ep.Call(context.Background(), strict, Empty{})
	var refused *AddressRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("got %v, want an AddressRefusedError", err)
	}

	// A cancelled context is the context's error.
	client := NewClient(ClientOptions{AllowPrivateNetworks: true, BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ep.Call(ctx, client, Empty{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCallRefused) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	// A nil context fails to build the request, before anything is sent.
	//nolint:staticcheck // a nil context is the mistake under test.
	_, err = ep.Call(nil, client, Empty{})
	assertErrorMentions(t, err, "muzak: GET /x: the request could not be built", "nil Context")
}
