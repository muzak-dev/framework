package muzak

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fuzzIn puts a string in every location a request has.
type fuzzIn struct {
	ID   string   `path:"id"`
	Rest string   `path:"rest"`
	Q    string   `query:"q"`
	QS   []string `query:"qs"`
	H    string   `header:"X-H"`
	HS   []string `header:"X-Hs"`
	C    string   `cookie:"c"`
	Body string   `json:"body"`
}

// fuzzForm puts one in every part of a form.
type fuzzForm struct {
	ID   string   `path:"id"`
	F    string   `form:"f"`
	FS   []string `form:"fs" required:"false"`
	File []byte   `file:"file"`
}

// fuzzSeen is what the server saw beside the bound value: anything a value
// managed to add to the request would show here.
type fuzzSeen struct {
	headers []string
	cookies []string
	query   []string
}

// fuzzExpectedHeaders are the headers a call and net/http write, besides the
// ones the input binds.
var fuzzExpectedHeaders = []string{"Accept", "Accept-Encoding", "Content-Length", "Content-Type", "Cookie", "User-Agent", "X-H", "X-Hs"}

// FuzzEndpointRoundTrip sends arbitrary strings through every location of a
// request and holds a call to its promise: the handler receives exactly what
// was sent, or nothing is sent and the call is refused with ErrCallRefused.
// Nothing a value holds may add a header, a cookie or a query parameter.
func FuzzEndpointRoundTrip(f *testing.F) {
	for _, seed := range [][2]string{
		{"", ""}, {".", ".."}, {"/", "a/b"}, {"%2F", "%"}, {"\r\n", "X-Evil: 1"}, {"\x00", "\x7f"},
		{" a ", "\t"}, {"a,b", `"q"`}, {`"open`, `close"`}, {"\xc3\xbc", "\xff\xfe"}, {"+", "&=;#?"},
		{"\xe2\x80\xa8", "\xe2\x80\xae"}, {"a;c=forged", "x=y"}, {"a//b", "/lead"}, {"trail/", "a/./b"},
	} {
		f.Add(seed[0], seed[1])
	}
	ep := NewEndpoint[fuzzIn, Empty](http.MethodPost, "/f/{id}/{rest...}")
	form := NewEndpoint[fuzzForm, Empty](http.MethodPut, "/form/{id}")
	var (
		mu       sync.Mutex
		lastIn   fuzzIn
		lastForm fuzzForm
		seen     fuzzSeen
		calls    int
	)
	record := func(ctx *Context) {
		seen = fuzzSeen{}
		for name := range ctx.Request().Header {
			seen.headers = append(seen.headers, name)
		}
		for _, c := range ctx.Request().Cookies() {
			seen.cookies = append(seen.cookies, c.Name)
		}
		for key := range ctx.Request().URL.Query() {
			seen.query = append(seen.query, key)
		}
		calls++
	}
	client := endpointServer(f, func(app *App) {
		app.Implement(ep, func(ctx *Context, in fuzzIn) (Empty, error) {
			mu.Lock()
			defer mu.Unlock()
			lastIn = in
			record(ctx)
			return Empty{}, nil
		})
		app.Implement(form, func(ctx *Context, in fuzzForm) (Empty, error) {
			mu.Lock()
			defer mu.Unlock()
			lastForm = in
			record(ctx)
			return Empty{}, nil
		})
	})
	valid := fuzzIn{ID: "id", Rest: "r", Q: "q", QS: []string{"a"}, H: "h", HS: []string{"x"}, C: "c", Body: "b"}

	f.Fuzz(func(t *testing.T, a, b string) {
		variants := []fuzzIn{}
		for _, edit := range []func(*fuzzIn){
			func(in *fuzzIn) { in.ID = a },
			func(in *fuzzIn) { in.Rest = a },
			func(in *fuzzIn) { in.Q = a; in.QS = []string{a, b} },
			func(in *fuzzIn) { in.H = a },
			func(in *fuzzIn) { in.HS = []string{a, b} },
			func(in *fuzzIn) { in.C = a },
			func(in *fuzzIn) { in.Body = a },
		} {
			in := valid
			edit(&in)
			variants = append(variants, in)
		}
		check := func(err error, before int, got func() bool) {
			t.Helper()
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrCallRefused):
				if calls != before {
					t.Fatalf("a refused call reached the handler: %v", err)
				}
				// A refusal is printable ASCII written by the framework, so a
				// value holding anything else shows in it only if it leaked.
				unprintable := strings.ContainsFunc(a, func(r rune) bool { return r < 0x20 || r > 0x7e })
				if !strings.HasPrefix(err.Error(), "muzak: ") || (unprintable && strings.Contains(err.Error(), a)) {
					t.Fatalf("the refusal is not a muzak sentence free of the value: %q", err)
				}
			case err != nil:
				t.Fatalf("the call failed with something other than a refusal, so the request it wrote was not one the server reads: %v", err)
			case calls != before+1:
				t.Fatal("the call succeeded without reaching the handler once")
			case !got():
				t.Fatal("the handler received a different value than the one sent")
			}
			for _, name := range seen.headers {
				if !slices.Contains(fuzzExpectedHeaders, name) {
					t.Fatalf("a value added the header %q", name)
				}
			}
			if len(seen.cookies) > 1 || (len(seen.cookies) == 1 && seen.cookies[0] != "c") {
				t.Fatalf("a value added cookies: %v", seen.cookies)
			}
			for _, key := range seen.query {
				if key != "q" && key != "qs" && key != "" {
					t.Fatalf("a value added the query parameter %q", key)
				}
			}
		}
		for _, in := range variants {
			mu.Lock()
			before := calls
			mu.Unlock()
			_, err := ep.Call(context.Background(), client, in)
			check(err, before, func() bool { return equalFuzzIn(lastIn, in) })
		}

		sent := fuzzForm{ID: "id", F: a, FS: []string{a, b}, File: []byte(b)}
		mu.Lock()
		before := calls
		mu.Unlock()
		_, err := form.Call(context.Background(), client, sent)
		check(err, before, func() bool { return equalFuzzForm(lastForm, sent) })
	})
}

// equalFuzzIn compares what arrived with what was sent.
func equalFuzzIn(got, want fuzzIn) bool {
	return got.ID == want.ID && got.Rest == want.Rest && got.Q == want.Q && slices.Equal(got.QS, want.QS) &&
		got.H == want.H && slices.Equal(got.HS, want.HS) && got.C == want.C && got.Body == want.Body
}

// equalFuzzForm compares what arrived with what was sent.
func equalFuzzForm(got, want fuzzForm) bool {
	return got.ID == want.ID && got.F == want.F && slices.Equal(got.FS, want.FS) && string(got.File) == string(want.File)
}
