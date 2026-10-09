package muzak

import (
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
	"uuid"

	"muzak.dev/framework/i18n"
)

// halfDecoded is a model whose rules would run on whatever a failed decode
// left behind.
type halfDecoded struct {
	A int    `json:"a"`
	B string `json:"b"`
}

// halfDecodedRuns counts how often the model's rules ran, so that a test can
// tell a rule that was skipped from one whose failure was merely filtered.
var halfDecodedRuns atomic.Int64

func (in *halfDecoded) Validate(v *Validation) {
	halfDecodedRuns.Add(1)
	v.String(&in.B).Required()
}

// halfDecodedMixed is the same model with a header beside the body, so the
// body is decoded into a scratch value that is never copied when it fails.
type halfDecodedMixed struct {
	Tenant string `header:"X-Tenant" required:"true"`
	A      int    `json:"a"`
	B      string `json:"b"`
}

func (in *halfDecodedMixed) Validate(v *Validation) {
	v.String(&in.B).Required()
}

// TestValidationDoesNotRunOnAHalfDecodedBody covers a body that failed to
// decode part of the way through. The decoder stops at the first member it
// cannot read, so what the model holds is neither what the client sent nor a
// value it could have sent, and its rules used to run on it anyway: a client
// that sent "b" was told it was required, and with a header beside the body
// every required body member was reported missing, because the scratch value
// the body was decoded into is never copied out of a failed decode.
func TestValidationDoesNotRunOnAHalfDecodedBody(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/direct", func(*Context, halfDecoded) (Empty, error) { return Empty{}, nil })
	app.Post("/mixed", func(*Context, halfDecodedMixed) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	before := halfDecodedRuns.Load()
	rec := do(t, app, http.MethodPost, "/direct", `{"b":"present","a":"oops"}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Field != "a" || details[0].Location != "body" {
		t.Errorf("direct: details = %+v, want only the member that failed to decode", details)
	}
	if runs := halfDecodedRuns.Load() - before; runs != 0 {
		t.Errorf("the model's rules ran %d times over a body that did not decode", runs)
	}

	// A located parameter does not depend on the body, so a failure binding it
	// is still reported beside the body's.
	req := httptest.NewRequest(http.MethodPost, "/mixed", strings.NewReader(`{"b":"present","a":"oops"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = doRequest(t, app, req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	details = decodeError(t, rec).Error.Details
	if len(details) != 2 {
		t.Fatalf("mixed: details = %+v, want the header and the member", details)
	}
	if details[0].Location != "header" || details[0].Field != "X-Tenant" {
		t.Errorf("mixed: first detail = %+v, want the missing header", details[0])
	}
	if details[1].Location != "body" || details[1].Field != "a" {
		t.Errorf("mixed: second detail = %+v, want the member that failed to decode", details[1])
	}

	// A body that decodes is validated as before.
	rec = do(t, app, http.MethodPost, "/direct", `{"a":1}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if details := decodeError(t, rec).Error.Details; len(details) != 1 || details[0].Field != "b" {
		t.Errorf("a decoded body: details = %+v, want b reported", details)
	}
}

// decodeAddress and decodeTarget hold a member of each kind a body decode can
// fail on, nested the ways a failure has to be located through.
type decodeAddress struct {
	Zip int `json:"zip"`
}

type decodeTarget struct {
	A      int                      `json:"a"`
	B      string                   `json:"b"`
	Addr   decodeAddress            `json:"addr"`
	When   time.Time                `json:"when"`
	D      time.Duration            `json:"d"`
	N      int8                     `json:"n"`
	U      uint8                    `json:"u"`
	F      float32                  `json:"f"`
	Items  []decodeAddress          `json:"items"`
	Pair   [2]int                   `json:"pair"`
	ByName map[string]decodeAddress `json:"by_name"`
	ID     uuid.UUID                `json:"id"`
	Raw    []byte                   `json:"raw"`
	P      *int                     `json:"p"`
	Grid   [][]int                  `json:"grid"`
	Plain  decodeAddress
}

// decodeTargetApp serves decodeTarget with whatever translation store is given.
func decodeTargetApp(t *testing.T, opts I18nOptions) *App {
	t.Helper()
	options := quietOptions()
	options.I18n = opts
	app := New(options)
	app.Post("/d", func(*Context, decodeTarget) (Empty, error) { return Empty{}, nil })
	return mustBuild(t, app)
}

// decodeFailures pairs each broken body with the field and issue it should
// be reported under.
var decodeFailures = []struct {
	body, field, issue string
}{
	{`{"a":"x"}`, "a", "has the wrong type, a string is not accepted here"},
	{`{"addr":{"zip":"x"}}`, "addr.zip", "has the wrong type, a string is not accepted here"},
	{`{"items":[{"zip":1},{"zip":"x"}]}`, "items[1].zip", "has the wrong type, a string is not accepted here"},
	{`{"by_name":{"0":{"zip":"x"}}}`, "by_name.0.zip", "has the wrong type, a string is not accepted here"},
	{`{"grid":[[1],[2,true]]}`, "grid[1][1]", "has the wrong type, a boolean is not accepted here"},
	{`{"Plain":{"zip":{}}}`, "Plain.zip", "has the wrong type, an object is not accepted here"},
	{`{"addr":{"extra":1}}`, "addr.extra", "is not a field this endpoint accepts"},
	{`{"when":"yesterday"}`, "when", "is not in the expected format"},
	{`{"d":"5 parsecs"}`, "d", "must be a valid duration, such as 1500ms"},
	{`{"n":300}`, "n", "must be between -128 and 127"},
	{`{"n":-129}`, "n", "must be between -128 and 127"},
	{`{"u":256}`, "u", "must be between 0 and 255"},
	{`{"u":-1}`, "u", "must be a valid non-negative integer"},
	{`{"a":1.5}`, "a", "must be a valid integer"},
	{`{"a":1e3}`, "a", "must be a valid integer"},
	{`{"f":1e300}`, "f", "must be a valid number"},
	{`{"pair":[1,2,3]}`, "pair", "must have at most 2 items"},
	{`{"pair":[1,"x"]}`, "pair[1]", "has the wrong type, a string is not accepted here"},
	{`{"id":"nope"}`, "id", "is not in the expected format"},
	{`{"raw":"!!"}`, "raw", "is not in the expected format"},
	{`{"p":"x"}`, "p", "has the wrong type, a string is not accepted here"},
	{`[1]`, "", "has the wrong type, an array is not accepted here"},
	{`{"b":`, "", "is not valid JSON: unexpected EOF"},
}

// TestBodyDecodeFailuresNameTheFieldAndTheProblem covers the detail a body
// that failed to decode is reported with.
//
// The field was the last token of the decoder's JSON pointer, so a failure in
// "addr.zip" and one in "items[1].zip" were both reported as "zip", which
// names neither and disagrees with how validation names the same members. The
// issue was chosen from the JSON kind alone, so "yesterday" for a time was "a
// string is not accepted here" and 300 for an int8 was "a number is not
// accepted here", when a string and a number are exactly what each takes.
func TestBodyDecodeFailuresNameTheFieldAndTheProblem(t *testing.T) {
	t.Parallel()
	app := decodeTargetApp(t, I18nOptions{})
	for _, tc := range decodeFailures {
		rec := do(t, app, http.MethodPost, "/d", tc.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422\nbody: %s", tc.body, rec.Code, rec.Body.String())
			continue
		}
		details := decodeError(t, rec).Error.Details
		if len(details) != 1 {
			t.Errorf("%s: details = %+v, want one", tc.body, details)
			continue
		}
		if got := details[0]; got.Location != "body" || got.Field != tc.field || got.Issue != tc.issue {
			t.Errorf("%s: detail = %s %q %q, want body %q %q", tc.body, got.Location, got.Field, got.Issue, tc.field, tc.issue)
		}
	}
}

// TestBodyDecodeFailuresAreTranslated holds the decoder's messages to the rule
// every other message keeps: the English the locale ships reads exactly as the
// Go text does, and a translator can replace each one.
func TestBodyDecodeFailuresAreTranslated(t *testing.T) {
	t.Parallel()
	plain := decodeTargetApp(t, I18nOptions{})
	english := decodeTargetApp(t, I18nOptions{Store: i18n.Builtin()})
	for _, tc := range decodeFailures {
		before := decodeError(t, do(t, plain, http.MethodPost, "/d", tc.body)).Error.Details
		after := decodeError(t, do(t, english, http.MethodPost, "/d", tc.body)).Error.Details
		if len(before) != 1 || len(after) != 1 || before[0].Issue != after[0].Issue || before[0].Field != after[0].Field {
			t.Errorf("%s: the detail changed when i18n was turned on:\n without: %+v\n    with: %+v", tc.body, before, after)
		}
	}

	own, err := i18n.Load(fstest.MapFS{"locales/es.yml": &fstest.MapFile{Data: []byte(`es:
  muzak:
    binding:
      range: "debe estar entre %{min} y %{max}"
      unknown_field: "no es un campo de este servicio"
      wrong_string: "tiene el tipo equivocado, aqui no se acepta un texto"
      invalid_json: "no es JSON valido"
  errors:
    messages:
      too_many_items:
        one: "debe tener como mucho 1 elemento"
        other: "debe tener como mucho %{count} elementos"
`)}}, "locales")
	if err != nil {
		t.Fatal(err)
	}
	store, err := i18n.New(i18n.StoreOptions{Backend: i18n.NewChain(own.Backend(), i18n.Builtin().Backend())})
	if err != nil {
		t.Fatal(err)
	}
	spanish := decodeTargetApp(t, I18nOptions{Store: store})
	for body, want := range map[string]string{
		`{"n":300}`:         "debe estar entre -128 y 127",
		`{"addr":{"zz":1}}`: "no es un campo de este servicio",
		`{"a":"x"}`:         "tiene el tipo equivocado, aqui no se acepta un texto",
		`{"a":`:             "no es JSON valido",
		`{"pair":[1,2,3]}`:  "debe tener como mucho 2 elementos",
	} {
		req := httptest.NewRequest(http.MethodPost, "/d", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Language", "es")
		details := decodeError(t, doRequest(t, spanish, req)).Error.Details
		if len(details) != 1 || details[0].Issue != want {
			t.Errorf("%s in Spanish: details = %+v, want %q", body, details, want)
		}
	}
}

// TestParameterOutOfRangeSaysSo covers the parameter side of the same
// message: a whole number too large for its field used to be "must be a valid
// integer", which it is, and now names the range, as the body does.
func TestParameterOutOfRangeSaysSo(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/q", func(*Context, struct {
		N    int8    `query:"n"`
		U    uint16  `query:"u"`
		Many []int8  `query:"many"`
		Neg  uint8   `query:"neg"`
		Big  int64   `query:"big"`
		Huge uint64  `query:"huge"`
		F    float32 `query:"f"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	mustBuild(t, app)
	for query, want := range map[string]string{
		"n=128":                     "must be between -128 and 127",
		"u=65536":                   "must be between 0 and 65535",
		"many=1&many=200":           "entry 2 must be between -128 and 127",
		"neg=-1":                    "must be a valid non-negative integer",
		"big=9223372036854775808":   "must be between -9223372036854775808 and 9223372036854775807",
		"huge=18446744073709551616": "must be between 0 and 18446744073709551615",
		"n=1.5":                     "must be a valid integer",
		"f=1e39":                    "must be a valid number",
	} {
		rec := do(t, app, http.MethodGet, "/q?"+query)
		details := decodeError(t, rec).Error.Details
		if len(details) != 1 || details[0].Issue != want {
			t.Errorf("?%s: details = %+v, want %q", query, details, want)
		}
	}
}

// pathEmbedded, pathShape and pathHidden give bodyPath every way a member can
// be reached: through a pointer, an embedded struct by value or by pointer, a
// field the embed option inlines, and past fields the decoder never sees.
type pathEmbedded struct {
	Inner int `json:"inner"`
}

type pathHidden struct {
	Deep []*pathEmbedded `json:"deep"`
}

type pathShape struct {
	skipped int //nolint:unused // present to be walked past
	Ignored int `json:"-"`
	pathEmbedded
	*pathHidden
	Inlined pathEmbedded            `json:",embed"`
	Ptr     *[]pathEmbedded         `json:"ptr"`
	Loose   any                     `json:"loose"`
	Table   map[string][3]*struct{} `json:"table"`
}

// TestBodyPathFollowsTheType covers the walk that tells array positions from
// object members, through each of the shapes above.
func TestBodyPathFollowsTheType(t *testing.T) {
	t.Parallel()
	root := reflect.TypeFor[pathShape]()
	for pointer, want := range map[jsontext.Pointer]string{
		"":                  "",
		"/inner":            "inner",
		"/deep/2/inner":     "deep[2].inner",
		"/ptr/0/inner":      "ptr[0].inner",
		"/table/0/1":        "table.0[1]",
		"/loose/0/a":        "loose.0.a",
		"/missing/0/a":      "missing.0.a",
		"/Ignored/0":        "Ignored.0",
		"/skipped/0":        "skipped.0",
		"/a~1b/0":           "a/b.0",
		"/deep/0/missing/1": "deep[0].missing.1",
	} {
		if got := bodyPath(root, pointer); got != want {
			t.Errorf("bodyPath(%q) = %q, want %q", pointer, got, want)
		}
	}

	// A one-element array refused for a second element is "1 item".
	app := New(quietOptions())
	app.Post("/one", func(*Context, struct {
		Only [1]int `json:"only"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	mustBuild(t, app)
	details := decodeError(t, do(t, app, http.MethodPost, "/one", `{"only":[1,2]}`)).Error.Details
	if len(details) != 1 || details[0].Issue != "must have at most 1 item" {
		t.Errorf("details = %+v, want the singular", details)
	}
}
