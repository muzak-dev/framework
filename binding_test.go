package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"muzak.dev/framework/internal/radix"
)

// scalars exercises every parameter type the binder claims to support.
type scalars struct {
	Str      string        `query:"str"`
	Bool     bool          `query:"bool"`
	Int      int           `query:"int"`
	Int8     int8          `query:"int8"`
	Int16    int16         `query:"int16"`
	Int32    int32         `query:"int32"`
	Int64    int64         `query:"int64"`
	Uint     uint          `query:"uint"`
	Uint8    uint8         `query:"uint8"`
	Uint16   uint16        `query:"uint16"`
	Uint32   uint32        `query:"uint32"`
	Uint64   uint64        `query:"uint64"`
	Float32  float32       `query:"float32"`
	Float64  float64       `query:"float64"`
	Duration time.Duration `query:"duration"`
	Time     time.Time     `query:"time"`
	UUID     uuid.UUID     `query:"uuid"`
	Optional *int          `query:"optional"`
	Tags     []string      `query:"tag"`
	Numbers  []int         `query:"n"`
}

func TestSetterForEverySupportedType(t *testing.T) {
	t.Parallel()
	target := reflect.New(reflect.TypeFor[scalars]()).Elem()
	plan, err := newBindPlan(reflect.TypeFor[scalars](), "GET", "/x")
	if err != nil {
		t.Fatalf("newBindPlan: %v", err)
	}

	values := map[string][]string{
		"str": {"hello"}, "bool": {"true"},
		"int": {"-1"}, "int8": {"-8"}, "int16": {"-16"}, "int32": {"-32"}, "int64": {"-64"},
		"uint": {"1"}, "uint8": {"8"}, "uint16": {"16"}, "uint32": {"32"}, "uint64": {"64"},
		"float32": {"1.5"}, "float64": {"2.5"},
		"duration": {"1500ms"},
		"time":     {"2026-08-21T14:32:07Z"},
		"uuid":     {"0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"},
		"optional": {"7"},
		"tag":      {"a", "b"},
		"n":        {"1", "2", "3"},
	}
	for i := range plan.params {
		p := &plan.params[i]
		if err := p.set(fieldByIndex(target, p.index), values[p.name]); err != nil {
			t.Fatalf("setting %q: %v", p.name, err)
		}
	}

	got := target.Interface().(scalars)
	if got.Str != "hello" || !got.Bool {
		t.Errorf("string/bool = %q/%v", got.Str, got.Bool)
	}
	if got.Int != -1 || got.Int8 != -8 || got.Int16 != -16 || got.Int32 != -32 || got.Int64 != -64 {
		t.Errorf("signed integers = %+v", got)
	}
	if got.Uint != 1 || got.Uint8 != 8 || got.Uint16 != 16 || got.Uint32 != 32 || got.Uint64 != 64 {
		t.Errorf("unsigned integers = %+v", got)
	}
	if got.Float32 != 1.5 || got.Float64 != 2.5 {
		t.Errorf("floats = %v/%v", got.Float32, got.Float64)
	}
	if got.Duration != 1500*time.Millisecond {
		t.Errorf("duration = %v", got.Duration)
	}
	if got.Time.Year() != 2026 || got.Time.Minute() != 32 {
		t.Errorf("time = %v", got.Time)
	}
	if got.UUID.String() != "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31" {
		t.Errorf("uuid = %v", got.UUID)
	}
	if got.Optional == nil || *got.Optional != 7 {
		t.Errorf("optional pointer = %v", got.Optional)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Errorf("string slice = %v", got.Tags)
	}
	if len(got.Numbers) != 3 || got.Numbers[2] != 3 {
		t.Errorf("int slice = %v", got.Numbers)
	}
}

func TestSetterErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		typ  reflect.Type
		raw  []string
		want string
	}{
		{"bool", reflect.TypeFor[bool](), []string{"maybe"}, "must be true or false"},
		{"int", reflect.TypeFor[int](), []string{"x"}, "must be a valid integer"},
		{"int8 overflow", reflect.TypeFor[int8](), []string{"999"}, "must be between -128 and 127"},
		{"uint", reflect.TypeFor[uint](), []string{"-1"}, "must be a valid non-negative integer"},
		{"float", reflect.TypeFor[float64](), []string{"x"}, "must be a valid number"},
		{"duration", reflect.TypeFor[time.Duration](), []string{"soon"}, "must be a valid duration"},
		{"uuid", reflect.TypeFor[uuid.UUID](), []string{"not-a-uuid"}, "uuid"},
		{"pointer", reflect.TypeFor[*int](), []string{"x"}, "must be a valid integer"},
		{"slice element", reflect.TypeFor[[]int](), []string{"1", "bad"}, "entry 2 must be a valid integer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			set, err := setterFor(tc.typ)
			if err != nil {
				t.Fatalf("setterFor(%s) = %v", tc.typ, err)
			}
			target := reflect.New(tc.typ).Elem()
			err = set(target, tc.raw)
			if err == nil {
				t.Fatalf("setting %v from %v succeeded, want an error", tc.typ, tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestSetterForRejectsUnsupportedTypes(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{
		reflect.TypeFor[chan int](),
		reflect.TypeFor[func()](),
		reflect.TypeFor[map[string]string](),
		reflect.TypeFor[[]chan int](),
		reflect.TypeFor[*chan int](),
	} {
		t.Run(typ.String(), func(t *testing.T) {
			t.Parallel()
			if _, err := setterFor(typ); err == nil {
				t.Errorf("setterFor(%s) succeeded, want an error", typ)
			}
		})
	}
}

func TestBindingSources(t *testing.T) {
	t.Parallel()
	type in struct {
		Path   string `path:"id"`
		Query  string `query:"q"`
		Header string `header:"X-Thing"`
		Cookie string `cookie:"session"`
	}
	type out struct {
		Path   string `json:"path"`
		Query  string `json:"query"`
		Header string `json:"header"`
		Cookie string `json:"cookie"`
	}

	app := New(quietOptions())
	app.Get("/things/{id}", func(ctx *Context, i in) (out, error) {
		return out{Path: i.Path, Query: i.Query, Header: i.Header, Cookie: i.Cookie}, nil
	})
	mustBuild(t, app)

	req := httptest.NewRequest("GET", "/things/abc?q=search", nil)
	req.Header.Set("X-Thing", "header-value")
	req.AddCookie(&http.Cookie{Name: "session", Value: "cookie-value"})

	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"path":"abc","query":"search","header":"header-value","cookie":"cookie-value"}`)
}

func TestBindingOptionalAndRequired(t *testing.T) {
	t.Parallel()
	type in struct {
		Optional string `query:"optional"`
		Default  string `query:"defaulted" default:"fallback"`
		Required string `query:"required" required:"true"`
	}
	type out struct {
		Optional string `json:"optional"`
		Default  string `json:"default"`
		Required string `json:"required"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, i in) (out, error) {
		return out{Optional: i.Optional, Default: i.Default, Required: i.Required}, nil
	})
	mustBuild(t, app)

	t.Run("absent optional stays zero and the default applies", func(t *testing.T) {
		rec := do(t, app, "GET", "/x?required=yes")
		assertStatus(t, rec, http.StatusOK)
		assertJSON(t, rec, `{"optional":"","default":"fallback","required":"yes"}`)
	})

	t.Run("an explicit empty value beats the default", func(t *testing.T) {
		rec := do(t, app, "GET", "/x?required=yes&defaulted=")
		assertStatus(t, rec, http.StatusOK)
		assertJSON(t, rec, `{"optional":"","default":"","required":"yes"}`)
	})

	t.Run("a missing required parameter fails", func(t *testing.T) {
		rec := do(t, app, "GET", "/x")
		assertStatus(t, rec, http.StatusUnprocessableEntity)
		detail := decodeError(t, rec).Error.Details[0]
		if detail.Field != "required" || detail.Issue != "is required" {
			t.Errorf("detail = %+v", detail)
		}
	})
}

func TestBindingHeadersAndCookiesAreOptional(t *testing.T) {
	t.Parallel()
	type in struct {
		Header string `header:"X-Absent"`
		Cookie string `cookie:"absent"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/x"), http.StatusOK)
}

func TestBindingRequiredHeader(t *testing.T) {
	t.Parallel()
	type in struct {
		Token string `header:"X-Token" required:"true"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if got := decodeError(t, rec).Error.Details[0].Location; got != "header" {
		t.Errorf("location = %q, want %q", got, "header")
	}
}

func TestBindingEmbeddedStructs(t *testing.T) {
	t.Parallel()
	type paging struct {
		Limit  int `query:"limit" default:"10"`
		Offset int `query:"offset" default:"0"`
	}
	type in struct {
		paging
		Search string `query:"q"`
	}
	type out struct {
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
		Search string `json:"q"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, i in) (out, error) {
		return out{Limit: i.Limit, Offset: i.Offset, Search: i.Search}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x?limit=5&q=term")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"limit":5,"offset":0,"q":"term"}`)
}

// TestBindingEmbeddedBodyStruct covers the case where an embedded struct
// declares no located parameters and is therefore body content.
func TestBindingEmbeddedBodyStruct(t *testing.T) {
	t.Parallel()
	type audit struct {
		By string `json:"by"`
	}
	type in struct {
		audit
		Name string `json:"name"`
	}
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, i in) (in, error) { return i, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/x", `{"by":"someone","name":"thing"}`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"by":"someone","name":"thing"}`)
}

func TestBindingSkipsUnexportedAndIgnoredFields(t *testing.T) {
	t.Parallel()
	type in struct {
		Name    string `json:"name"`
		Ignored string `json:"-"`
		hidden  string //nolint:unused // present to prove unexported fields are skipped
	}
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, i in) (rtOut, error) {
		if i.Ignored != "" || i.hidden != "" {
			return rtOut{}, NewHTTPError(500, "a skipped field was populated")
		}
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "POST", "/x", `{"name":"thing"}`)
	assertStatus(t, rec, http.StatusOK)
}

func TestBindingBodyErrors(t *testing.T) {
	t.Parallel()
	type in struct {
		Name string `json:"name"`
		N    int    `json:"n,omitzero"`
	}
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	tests := []struct {
		name   string
		body   string
		status int
		field  string
		issue  string
	}{
		{
			name: "unknown member", body: `{"name":"a","extra":1}`,
			status: 422, field: "extra", issue: "is not a field this endpoint accepts",
		},
		{
			name: "wrong type", body: `{"name":123}`,
			status: 422, field: "name", issue: "has the wrong type, a number is not accepted here",
		},
		{
			name: "duplicate member", body: `{"name":"a","name":"b"}`,
			status: 422, issue: "is not valid JSON",
		},
		{
			name: "malformed json", body: `{"name":`,
			status: 422, issue: "is not valid JSON",
		},
		{
			name: "trailing content", body: `{"name":"a"} {"name":"b"}`,
			status: 422, issue: "is not valid JSON",
		},
		{
			name: "missing body", body: "",
			status: 422, field: "", issue: "is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec = do(t, app, "POST", "/x", tc.body)
			assertStatus(t, rec, tc.status)
			details := decodeError(t, rec).Error.Details
			if len(details) != 1 {
				t.Fatalf("details = %d entries, want 1\nbody: %s", len(details), rec.Body.String())
			}
			if details[0].Location != "body" {
				t.Errorf("location = %q, want %q", details[0].Location, "body")
			}
			if tc.field != "" && details[0].Field != tc.field {
				t.Errorf("field = %q, want %q", details[0].Field, tc.field)
			}
			if !strings.Contains(details[0].Issue, tc.issue) {
				t.Errorf("issue = %q, want it to mention %q", details[0].Issue, tc.issue)
			}
		})
	}
}

// TestBindingBodyErrorsDoNotLeakGoTypes checks that a decoding failure never
// names a server-side type in the response.
func TestBindingBodyErrorsDoNotLeakGoTypes(t *testing.T) {
	t.Parallel()
	type secretShape struct {
		Name string `json:"name"`
	}
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, i secretShape) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/x", `{"name":[1,2,3]}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if body := rec.Body.String(); strings.Contains(body, "secretShape") || strings.Contains(body, "muzak.") {
		t.Errorf("the response named a Go type: %s", body)
	}
}

func TestAllowUnknownFields(t *testing.T) {
	t.Parallel()
	type in struct {
		Name string `json:"name"`
	}
	app := New(quietOptions())
	app.Post("/strict", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	app.Post("/lax", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil }, AllowUnknownFields())
	mustBuild(t, app)

	assertStatus(t, do(t, app, "POST", "/strict", `{"name":"a","extra":1}`), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/lax", `{"name":"a","extra":1}`), http.StatusOK)

	// Relaxing unknown members must not relax the other strictness rules.
	assertStatus(t, do(t, app, "POST", "/lax", `{"name":"a","name":"b"}`), http.StatusUnprocessableEntity)
}

func TestAllowUnknownFieldsInheritsFromRouter(t *testing.T) {
	t.Parallel()
	type in struct {
		Name string `json:"name"`
	}
	child := NewRouter(AllowUnknownFields())
	child.Post("/lax", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	app := New(quietOptions())
	app.Include(child)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "POST", "/lax", `{"name":"a","extra":1}`), http.StatusOK)
}

func TestContentTypeChecking(t *testing.T) {
	t.Parallel()
	type in struct {
		Name string `json:"name"`
	}
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	tests := []struct {
		name        string
		contentType string
		status      int
	}{
		{"json", "application/json", http.StatusOK},
		{"json with charset", "application/json; charset=utf-8", http.StatusOK},
		{"structured suffix", "application/vnd.api+json", http.StatusOK},
		// A body with no Content-Type is what a cross-site fetch of a Blob
		// sends without a preflight, so it is refused rather than guessed at.
		{"absent", "", http.StatusUnsupportedMediaType},
		{"text", "text/plain", http.StatusUnsupportedMediaType},
		{"form", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"malformed", "application/json; charset=", http.StatusUnsupportedMediaType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"a"}`))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			assertStatus(t, doRequest(t, app, req), tc.status)
		})
	}
}

func TestBodySizeLimit(t *testing.T) {
	t.Parallel()
	type in struct {
		Name string `json:"name"`
	}
	opts := quietOptions()
	opts.MaxBodySize = 32
	app := New(opts)
	app.Post("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	assertStatus(t, do(t, app, "POST", "/x", `{"name":"fits"}`), http.StatusOK)

	rec := do(t, app, "POST", "/x", `{"name":"`+strings.Repeat("x", 200)+`"}`)
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
}

// TestBodyReadFailure covers the path where the request body errors part way
// through, which a truncated upload produces in practice.
func TestBodyReadFailure(t *testing.T) {
	t.Parallel()
	type in struct {
		Name string `json:"name"`
	}
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	req := httptest.NewRequest("POST", "/x", &failingReader{})
	req.Header.Set("Content-Type", "application/json")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusBadRequest)
	if code := decodeError(t, rec).Error.Code; code != CodeBadRequest {
		t.Errorf("code = %q, want %q", code, CodeBadRequest)
	}
}

// failingReader fails on the first read, standing in for a connection that
// drops mid-body.
type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) {
	return 0, &net0Error{}
}

// net0Error is a stand-in transport error.
type net0Error struct{}

func (*net0Error) Error() string { return "connection reset" }

func TestEmptyInputSkipsBinding(t *testing.T) {
	t.Parallel()
	plan, err := newBindPlan(emptyType, "GET", "/x")
	if err != nil {
		t.Fatalf("newBindPlan(Empty) = %v", err)
	}
	if !plan.empty {
		t.Error("the plan for Empty is not marked empty")
	}
	if plan.body != nil || len(plan.params) != 0 {
		t.Error("the plan for Empty has work to do")
	}
	if err := plan.bind(nil, reflect.Value{}, nil); err != nil {
		t.Errorf("binding an empty plan = %v, want nil", err)
	}
}

// TestEmptyInputDrainsTheBody keeps a keep-alive connection reusable when a
// client sends a body to a route that reads none.
func TestEmptyInputDrainsTheBody(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "POST", "/x", `{"ignored":true}`), http.StatusOK)
}

func TestParamSourceString(t *testing.T) {
	t.Parallel()
	tests := map[paramSource]string{
		srcPath: "path", srcQuery: "query", srcHeader: "header", srcCookie: "cookie",
	}
	for source, want := range tests {
		if got := source.String(); got != want {
			t.Errorf("paramSource(%d).String() = %q, want %q", source, got, want)
		}
	}
}

func TestTemplateParams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want []string
	}{
		{"/users", nil},
		{"/users/{id}", []string{"id"}},
		{"/users/{id}/posts/{post_id}", []string{"id", "post_id"}},
		{"/files/{path...}", []string{"path"}},
		{"/", nil},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			got := templateParams(tc.path)
			if len(got) != len(tc.want) {
				t.Fatalf("templateParams(%q) = %v, want %v", tc.path, got, tc.want)
			}
			for _, name := range tc.want {
				if !got[name] {
					t.Errorf("templateParams(%q) is missing %q", tc.path, name)
				}
			}
		})
	}
}

func TestFieldByIndexAllocatesPointers(t *testing.T) {
	t.Parallel()
	type inner struct{ Value string }
	type outer struct{ Inner *inner }

	target := reflect.New(reflect.TypeFor[outer]()).Elem()
	field := fieldByIndex(target, []int{0, 0})
	field.SetString("reached")

	got := target.Interface().(outer)
	if got.Inner == nil || got.Inner.Value != "reached" {
		t.Errorf("fieldByIndex did not allocate through the pointer: %+v", got)
	}
}

func TestTotalFields(t *testing.T) {
	t.Parallel()
	type mixed struct {
		A string
		B string
		c string //nolint:unused // present to prove unexported fields are not counted
	}
	if got := totalFields(reflect.TypeFor[mixed]()); got != 2 {
		t.Errorf("totalFields = %d, want 2", got)
	}
}

func TestPercentEncodedPathParameters(t *testing.T) {
	t.Parallel()
	type in struct {
		Name string `path:"name"`
	}
	type out struct {
		Name string `json:"name"`
	}
	app := New(quietOptions())
	app.Get("/things/{name}", func(ctx *Context, i in) (out, error) {
		return out{Name: i.Name}, nil
	})
	mustBuild(t, app)

	t.Run("an encoded separator stays inside one segment", func(t *testing.T) {
		rec := do(t, app, "GET", "/things/a%2Fb")
		assertStatus(t, rec, http.StatusOK)
		assertJSON(t, rec, `{"name":"a/b"}`)
	})

	t.Run("encoded spaces are decoded", func(t *testing.T) {
		rec := do(t, app, "GET", "/things/hello%20world")
		assertStatus(t, rec, http.StatusOK)
		assertJSON(t, rec, `{"name":"hello world"}`)
	})

}

// TestUnescapeParams covers percent-decoding directly, including the malformed
// input that net/http normalises away before a handler could ever see it.
func TestUnescapeParams(t *testing.T) {
	t.Parallel()

	t.Run("a valid escape is decoded", func(t *testing.T) {
		t.Parallel()
		params := paramsWith(t, "name", "a%2Fb")
		if err := unescapeParams(params); err != nil {
			t.Fatalf("unescapeParams = %v", err)
		}
		if got, _ := params.Get("name"); got != "a/b" {
			t.Errorf("value = %q, want %q", got, "a/b")
		}
	})

	t.Run("a malformed escape is rejected", func(t *testing.T) {
		t.Parallel()
		params := paramsWith(t, "name", "%zz")
		err := unescapeParams(params)
		if err == nil {
			t.Fatal("unescapeParams accepted a malformed escape")
		}
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
			t.Errorf("error = %v, want a 400 HTTPError", err)
		}
	})
}

// paramsWith builds a radix.Params holding one captured value, by matching a
// throwaway tree so that the captured value goes through the same code path a
// request would use.
func paramsWith(t *testing.T, name, value string) *radix.Params {
	t.Helper()
	tree := radix.New[string]()
	if err := tree.Insert("/{"+name+"}", "route"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	params := &radix.Params{}
	if _, ok := tree.Lookup("/"+value, params); !ok {
		t.Fatalf("the throwaway tree did not match /%s", value)
	}
	return params
}

func FuzzBindQuery(f *testing.F) {
	type in struct {
		Str      string        `query:"str"`
		Num      int           `query:"num"`
		Flag     bool          `query:"flag"`
		Duration time.Duration `query:"duration"`
		List     []int         `query:"list"`
		Optional *int          `query:"optional"`
		Ident    uuid.UUID     `query:"ident"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	if err := app.Build(); err != nil {
		f.Fatalf("Build: %v", err)
	}

	for _, seed := range []string{
		"str=a&num=1&flag=true", "num=999999999999999999999999", "flag=", "duration=1h",
		"list=1&list=2", "optional=", "ident=0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31",
		"str=%00", "num=-", "%zz=1", "str=" + strings.Repeat("a", 5000),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, query string) {
		// The query is assigned to RawQuery rather than spliced into the
		// target, because httptest.NewRequest parses its argument as a request
		// line and rejects anything containing a space. A real server hands the
		// handler an already parsed URL, which is what this reproduces.
		req := httptest.NewRequest("GET", "/x", nil)
		req.URL.RawQuery = query
		rec := httptest.NewRecorder()
		// The contract: any query string produces a well-formed response and
		// never a panic or a 5xx.
		app.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("query %q produced status %d\nbody: %s", query, rec.Code, rec.Body.String())
		}
	})
}

func FuzzBindBody(f *testing.F) {
	type nested struct {
		Deep []string `json:"deep"`
	}
	type in struct {
		Name   string         `json:"name"`
		Count  int            `json:"count,omitzero"`
		Nested nested         `json:"nested,omitzero"`
		Extra  map[string]int `json:"extra,omitzero"`
	}
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	if err := app.Build(); err != nil {
		f.Fatalf("Build: %v", err)
	}

	for _, seed := range []string{
		`{"name":"a"}`, `{"name":"a","count":1}`, `{}`, `[]`, `null`, `"x"`, ``,
		`{"name":"a","name":"b"}`, `{"nested":{"deep":["x"]}}`, `{"extra":{"k":1}}`,
		`{"name":"` + strings.Repeat("x", 2000) + `"}`,
		"{\"name\":\"\xff\xfe\"}",
		strings.Repeat(`{"nested":`, 300),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("body %q produced status %d\nresponse: %s", body, rec.Code, rec.Body.String())
		}
	})
}

func FuzzBindHeaders(f *testing.F) {
	type in struct {
		Token string        `header:"X-Token"`
		Count int           `header:"X-Count"`
		Wait  time.Duration `header:"X-Wait"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	if err := app.Build(); err != nil {
		f.Fatalf("Build: %v", err)
	}

	for _, seed := range []string{"abc", "", "1", "-", strings.Repeat("z", 3000), "\t"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		req := httptest.NewRequest("GET", "/x", nil)
		// Header values must not carry control characters; net/http rejects
		// those before a handler ever sees them.
		if strings.ContainsAny(value, "\r\n\x00") {
			t.Skip("net/http rejects control characters in header values")
		}
		req.Header.Set("X-Token", value)
		req.Header.Set("X-Count", value)
		req.Header.Set("X-Wait", value)
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("header %q produced status %d", value, rec.Code)
		}
	})
}

// An embedded struct holding both a located parameter and a body member.
type embInner struct {
	Role string `query:"role"`
	Note string `json:"note"`
}
type embOuter struct {
	embInner
	Name string `json:"name"`
}
type embOut struct {
	Role string `json:"role"`
	Note string `json:"note"`
	Name string `json:"name"`
}

func TestBodyCannotOverwriteEmbeddedLocatedField(t *testing.T) {
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, in embOuter) (embOut, error) {
		return embOut{Role: in.Role, Note: in.Note, Name: in.Name}, nil
	}, AllowUnknownFields())
	mustBuild(t, app)

	rec := do(t, app, "POST", "/x?role=viewer", `{"note":"n","name":"x","Role":"admin"}`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"role":"viewer","note":"n","name":"x"}`)
}

// specialHeaders binds the two headers net/http moves off the header map.
type specialHeaders struct {
	Host   string `header:"Host"`
	Length int64  `header:"Content-Length"`
	Agent  string `header:"User-Agent"`
}

func TestHeadersNetHTTPMovesOffTheMap(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/echo", func(ctx *Context, in specialHeaders) (specialHeaders, error) {
		return in, nil
	})

	req := httptest.NewRequest(http.MethodPost, "http://api.example.test/echo", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "probe/1")
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	// Host and Content-Length live on the request rather than in its header
	// map, so a binder that read only the map would report both as absent.
	assertJSON(t, rec, `{"Host":"api.example.test","Length":5,"Agent":"probe/1"}`)

	// Neither is always knowable. A request with no authority and a body of
	// unannounced length leaves both absent rather than reporting a zero.
	req = httptest.NewRequest(http.MethodPost, "http://api.example.test/echo", strings.NewReader("hello"))
	req.Host = ""
	req.ContentLength = -1
	rec = doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"Host":"","Length":0,"Agent":""}`)
}

// textCoded round-trips as text rather than as a JSON object.
type textCoded struct {
	value string
}

func (c textCoded) MarshalText() ([]byte, error) { return []byte(c.value), nil }

func (c *textCoded) UnmarshalText(text []byte) error {
	c.value = string(text)
	return nil
}

type textCodedIn struct {
	Stamp textCoded `header:"X-Stamp"`
}

func TestTextCodedTypesAreDocumentedAsStrings(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/stamped", func(ctx *Context, in textCodedIn) (Empty, error) { return Empty{}, nil })

	doc, err := mustBuild(t, app).Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	schema := doc.Paths["/stamped"].Get.Parameters[0].Schema
	if schema.Type != "string" {
		t.Errorf("schema type = %v, want string; a type that parses from text is a string on the wire", schema.Type)
	}
	// Describing it by walking its fields would also have named a component
	// for it, which would document the Go struct rather than the value.
	if doc.Components != nil {
		if _, named := doc.Components.Schemas["textCoded"]; named {
			t.Error("a text-coded type was given a component of its own")
		}
	}
}

func TestALocationTagIgnoresWhatFollowsTheFirstComma(t *testing.T) {
	// A struct tag that looks like a json tag gets written like one, and the
	// failure is silent in the worst way: `query:"limit,omitzero"` binds a
	// parameter named `limit,omitzero`, nothing ever sends that, and the
	// endpoint answers the unfiltered question rather than refusing.
	//
	// Options mean nothing in a location tag - `default` and `required` are
	// tags of their own - so discarding them costs nothing and stops the trap.
	type in struct {
		Plain   string `query:"plain"`
		Omitted string `query:"omitted,omitzero"`
		Several int    `query:"several,omitempty,string"`
	}

	type out struct {
		OK bool `json:"ok"`
	}

	var got in
	app := New(quietOptions())
	app.Get("/t", func(_ *Context, body in) (out, error) {
		got = body
		return out{OK: true}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/t?plain=a&omitted=b&several=7", nil)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	if got.Plain != "a" || got.Omitted != "b" || got.Several != 7 {
		t.Errorf("bound %+v; the options were read as part of the name", got)
	}
}
