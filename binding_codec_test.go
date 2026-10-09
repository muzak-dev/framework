package muzak

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"
)

// The types below are refused by encoding/json/v2 whatever a request sends.
// Each used to build, then answered every request that reached the problem
// with a 422 blaming the client, often with an empty field and nothing logged.

type CodecBase struct {
	X int `json:"x"`
}

type codecEmbeddedOptionsIn struct {
	CodecBase `json:",omitzero"`
	Y         int `json:"y"`
}

type codecUnixIn struct {
	T time.Time `json:"t,format:unix"`
}

type codecNestedIn struct {
	Items []codecUnixIn `json:"items"`
}

type codecChanIn struct {
	C chan int `json:"c"`
}

type codecStringTagIn struct {
	S string `json:"s,string"`
}

type codecReaderIn struct {
	R io.Reader `json:"r"`
}

type codecFuncIn struct {
	Callback *func() `json:"callback"`
}

type codecMixedIn struct {
	ID int `path:"id"`
	codecUnixIn
}

// codecConflictIn names one member twice, once by tag and once by the field
// name the tag-less field is known by.
type codecConflictIn struct {
	A    int `json:"Same"`
	Same int
}

// TestBodyTypeTheDecoderRefusesIsABuildError covers the build-time half: the
// route does not build, and the error names the type and what is wrong.
func TestBodyTypeTheDecoderRefusesIsABuildError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{"embedded struct with options", reflect.TypeFor[codecEmbeddedOptionsIn](),
			[]string{"muzak.codecEmbeddedOptionsIn", "cannot have any options other than `embed`"}},
		{"format on a time", reflect.TypeFor[codecUnixIn](),
			[]string{"muzak.codecUnixIn", "unsupported `format` tag option"}},
		{"reached through a slice", reflect.TypeFor[codecNestedIn](),
			[]string{"field Items", "muzak.codecUnixIn", "unsupported `format` tag option"}},
		{"conflicting names", reflect.TypeFor[codecConflictIn](),
			[]string{"conflict over JSON object name \"Same\""}},
		{"a channel", reflect.TypeFor[codecChanIn](),
			[]string{"field C", "chan int", "tag it json:\"-\""}},
		{"a function behind a pointer", reflect.TypeFor[codecFuncIn](),
			[]string{"field Callback", "func()"}},
		{"an interface with methods", reflect.TypeFor[codecReaderIn](),
			[]string{"field R", "io.Reader"}},
		{"the string option on a string", reflect.TypeFor[codecStringTagIn](),
			[]string{"field S", "the `string` option"}},
		{"the string option on a time", reflect.TypeFor[struct {
			At *time.Time `json:"at,string"` //nolint:staticcheck // the misuse under test
		}](), []string{"field At", "the `string` option"}},
		{"beside a located field", reflect.TypeFor[codecMixedIn](),
			[]string{"unsupported `format` tag option"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := newBindPlan(tc.typ, "POST", "/x/{id}")
			if err == nil {
				t.Fatal("newBindPlan succeeded, want a build error")
			}
			if !strings.HasPrefix(err.Error(), "muzak: POST /x/{id}: ") {
				t.Errorf("error = %q, want the muzak prefix and the route", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}

	// Every such route is reported in the one build error.
	app := New(quietOptions())
	app.Post("/a", func(*Context, codecUnixIn) (Empty, error) { return Empty{}, nil })
	app.Post("/b", func(*Context, codecChanIn) (Empty, error) { return Empty{}, nil })
	err := app.Build()
	if err == nil || !strings.Contains(err.Error(), "POST /a") || !strings.Contains(err.Error(), "POST /b") {
		t.Errorf("Build() = %v, want both routes reported", err)
	}
}

// TestBodyTypesTheDecoderAcceptsStillBuild guards the other side: a type that
// decodes itself, an empty interface and the ordinary shapes are not refused,
// and neither is a field the decoder never sees.
func TestBodyTypesTheDecoderAcceptsStillBuild(t *testing.T) {
	t.Parallel()
	type self struct {
		When   time.Time         `json:"when"`
		Any    any               `json:"any"`
		Number int               `json:"number,string"`
		Ptr    *float64          `json:"ptr,string"`
		Skip   chan int          `json:"-"`
		hidden func()            //nolint:unused // never decoded
		Tree   map[string][]self `json:"tree"`
		D      time.Duration     `json:"d"`
		ID     uuid.UUID         `json:"id,string"` //nolint:staticcheck // a type that reads itself decides what the option means
	}
	if _, err := newBindPlan(reflect.TypeFor[self](), "POST", "/x"); err != nil {
		t.Errorf("newBindPlan = %v, want the type accepted", err)
	}
}

// codecHiddenBase is embedded by pointer and unexported, which encoding/json/v2
// cannot allocate: it fails only when a member of it is sent, so it is not
// refused at build, and it is the one way left to reach a failure that is the
// type's fault at request time.
type codecHiddenBase struct {
	X int `json:"x"`
}

type codecHiddenIn struct {
	*codecHiddenBase
	Y int `json:"y"`
}

// TestBodyTypeFaultIsAServerError covers the request-time half: a failure the
// type is responsible for is a logged 500, not a 422 sent to a client that
// did nothing wrong.
func TestBodyTypeFaultIsAServerError(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Post("/h", func(*Context, codecHiddenIn) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	rec := do(t, app, http.MethodPost, "/h", `{"x":1}`)
	assertStatus(t, rec, http.StatusInternalServerError)
	body := decodeError(t, rec)
	if body.Error.Code != CodeInternalError || len(body.Error.Details) != 0 {
		t.Errorf("error = %+v, want an opaque internal error", body.Error)
	}
	if strings.Contains(rec.Body.String(), "codecHidden") {
		t.Errorf("the response named a Go type: %s", rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, "cannot set embedded pointer") {
		t.Errorf("the fault was not logged: %s", out)
	}

	// A body that does not reach the fault decodes as before.
	rec = do(t, app, http.MethodPost, "/h", `{"y":1}`)
	assertStatus(t, rec, http.StatusOK)
}

// TestTypeFaultLooksThroughPointers covers the classification on its own. A
// type that decodes itself judged the value it was given, so its failure is
// the request's; time.Time has the methods but is read natively, so a failure
// without a JSON kind is the type's, as it is for a type that cannot be
// decoded into at all.
func TestTypeFaultLooksThroughPointers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		goType reflect.Type
		want   bool
	}{
		{reflect.TypeFor[*uuid.UUID](), false},
		{reflect.TypeFor[*time.Time](), true},
		{reflect.TypeFor[**chan int](), true},
		{nil, true},
	} {
		err := &json.SemanticError{GoType: tc.goType, Err: errors.New("refused")}
		if got := typeFault(err); got != tc.want {
			t.Errorf("typeFault(%v) = %v, want %v", tc.goType, got, tc.want)
		}
	}
	if typeFault(&json.SemanticError{GoType: reflect.TypeFor[int](), JSONKind: '"'}) {
		t.Error("typeFault = true for a value of the wrong kind, want the request blamed")
	}
	if typeFault(&json.SemanticError{GoType: reflect.TypeFor[time.Duration](), Err: errNotDuration}) {
		t.Error("typeFault = true for a duration that did not parse, want the request blamed")
	}
}
