package muzak

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"
)

// epText reads and writes itself as text with a value receiver for writing,
// as an identifier type of an application's own would.
type epText struct{ A, B string }

func (t epText) MarshalText() ([]byte, error) { return []byte(t.A + "~" + t.B), nil }

func (t *epText) UnmarshalText(b []byte) error {
	a, rest, ok := strings.Cut(string(b), "~")
	if !ok {
		return errors.New("no separator")
	}
	t.A, t.B = a, rest
	return nil
}

// epPtrText writes itself with a pointer receiver, which a call has to copy
// the value to reach.
type epPtrText struct{ N int }

func (t *epPtrText) MarshalText() ([]byte, error) { return []byte("n" + strconv.Itoa(t.N)), nil }

func (t *epPtrText) UnmarshalText(b []byte) error {
	n, err := strconv.Atoi(strings.TrimPrefix(string(b), "n"))
	t.N = n
	return err
}

// epShared is embedded by value and holds located fields beside a body
// member, the shape that makes the binder narrow the body.
type epShared struct {
	Tenant string `header:"X-Tenant"`
	Trace  string `query:"trace"`
	Note   string `json:"note"`
}

type epChild struct {
	Label string   `json:"label"`
	Score float64  `json:"score"`
	Kids  []string `json:"kids"`
}

// epIn exercises every location and every kind of value the binder reads.
type epIn struct {
	epShared
	ID     string        `path:"id"`
	Num    int64         `path:"num"`
	Rest   string        `path:"rest"`
	Q      string        `query:"q"`
	QP     *string       `query:"qp"`
	QS     []string      `query:"qs"`
	QI8    int8          `query:"qi8"`
	QU16   uint16        `query:"qu16"`
	QU64   uint64        `query:"qu64"`
	QF32   float32       `query:"qf32"`
	QF64   float64       `query:"qf64"`
	QB     bool          `query:"qb"`
	QD     time.Duration `query:"qd"`
	QT     time.Time     `query:"qt"`
	QU     uuid.UUID     `query:"qu"`
	QIs    []int         `query:"qis"`
	QTxt   epText        `query:"qtxt"`
	QPT    epPtrText     `query:"qpt"`
	QPS    *[]string     `query:"qps"`
	QSP    []*int        `query:"qsp"`
	QBytes []byte        `query:"qbytes"`
	QAddr  netip.Addr    `query:"qaddr"`
	QNest  [][]string    `query:"qnest"`
	H      string        `header:"X-H"`
	HS     []string      `header:"X-HS"`
	HI     *int          `header:"X-HI"`
	HT     *time.Time    `header:"X-HT"`
	C      string        `cookie:"c"`
	CI     int           `cookie:"ci"`
	User   Dep[depUser]
	Name   string         `json:"name"`
	Tags   []string       `json:"tags"`
	Meta   map[string]int `json:"meta"`
	Child  *epChild       `json:"child"`
	Wait   time.Duration  `json:"wait"`
	When   time.Time      `json:"when"`
	Blob   []byte         `json:"blob"`
	Hidden string         `json:"-"`
}

// epOut is what the handler answers with, as rich as the input.
type epOut struct {
	Echo   string            `json:"echo"`
	Count  int               `json:"count"`
	Ratio  float64           `json:"ratio"`
	Wait   time.Duration     `json:"wait"`
	When   time.Time         `json:"when"`
	ID     uuid.UUID         `json:"id"`
	Tags   []string          `json:"tags"`
	Attrs  map[string]string `json:"attrs"`
	Child  *epChild          `json:"child"`
	Blob   []byte            `json:"blob"`
	Absent *string           `json:"absent"`
}

var epEndpoint = NewEndpoint[epIn, epOut](http.MethodPost, "/rt/{id}/n/{num}/{rest...}", Summary("round trip"))

// epServer serves epEndpoint, answering with out and recording each input.
func epServer(t *testing.T, out epOut) (*Client, *capture[epIn]) {
	t.Helper()
	seen := &capture[epIn]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(epEndpoint, func(_ *Context, in epIn) (epOut, error) {
			if in.User.Get().Name != "provided" {
				return epOut{}, errors.New("the dependency was not filled from the provider")
			}
			in.User = Dep[depUser]{}
			seen.record(in)
			return out, nil
		}, Needs(func(*Context) (depUser, error) { return depUser{Name: "provided"}, nil }))
	})
	return client, seen
}

func epRichOut() epOut {
	return epOut{
		Echo: "\xc3\xbcn\xc3\xafcode \xe2\x80\xa8 \"quoted\" </script>", Count: -7, Ratio: 0.1,
		Wait: 1500 * time.Millisecond, When: time.Date(2026, 10, 9, 8, 7, 6, 5, time.UTC),
		ID:   uuid.MustParse("0f8fad5b-d9cb-469f-a165-70867728950e"),
		Tags: []string{"a", "b"}, Attrs: map[string]string{"k": "v"},
		Child: &epChild{Label: "kid", Score: 2.5, Kids: []string{"x"}},
		Blob:  []byte{0, 1, 2, 255},
	}
}

func TestEndpointRoundTripsEveryLocationAndKind(t *testing.T) {
	t.Parallel()
	out := epRichOut()
	client, seen := epServer(t, out)

	qp := "pointer value"
	seven, nine := 7, 9
	when := time.Date(2025, 1, 2, 3, 4, 5, 123456789, time.UTC)
	full := epIn{
		epShared: epShared{Tenant: "acme corp", Trace: "t-1&x=y", Note: "embedded body member"},
		ID:       "id with space ?#% .. \xc3\xbc and+plus",
		Num:      math.MinInt64,
		Rest:     "a/b c/%2F/\xc3\xbcn\xc3\xaf/?x=#y/..x/.hidden",
		Q:        "q&r=s;t+u v%w#x?y",
		QP:       &qp,
		QS:       []string{"one", "", "two & three", "\xc3\xbc"},
		QI8:      -128, QU16: 65535, QU64: math.MaxUint64,
		QF32: float32(math.SmallestNonzeroFloat32), QF64: -0.000001234e-300,
		QB: true, QD: -90 * time.Minute, QT: when,
		QU:     uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"),
		QIs:    []int{1, -2, 3},
		QTxt:   epText{A: "left side", B: "right&side"},
		QPT:    epPtrText{N: 42},
		QPS:    &[]string{"x", "y"},
		QSP:    []*int{&seven, &nine},
		QBytes: []byte{0, 'a', 255},
		QAddr:  netip.MustParseAddr("fe80::1%eth0"),
		QNest:  [][]string{{"p"}, {"q"}},
		H:      "value, with; commas=and \"quotes\" and \tinner tab and \xc3\xbc",
		HS:     []string{"a", "b c", `"x,y"`, `"esc\"aped"`},
		HI:     &seven,
		HT:     &when,
		C:      "hello world, ok",
		CI:     -12,
		Name:   "body \xe2\x80\xa8 name",
		Tags:   []string{"t1", "t2"},
		Meta:   map[string]int{"a": 1, "b": 2},
		Child:  &epChild{Label: "c", Score: math.MaxFloat64, Kids: []string{"k"}},
		Wait:   time.Hour + time.Nanosecond,
		When:   when,
		Blob:   []byte("bytes\x00here"),
	}
	got, err := epEndpoint.Call(context.Background(), client, full)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	assertEqualValue(t, got, out)
	assertEqualValue(t, seen.last(t), full)
}

func TestEndpointRoundTripsTheZeroValue(t *testing.T) {
	t.Parallel()
	client, seen := epServer(t, epOut{})
	// A path parameter cannot be empty, and the trailing parameter is the
	// one that may.
	sent := epIn{ID: "x"}
	got, err := epEndpoint.Call(context.Background(), client, sent)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// JSON has no nil slice or map: what the server sends as [] and {} comes
	// back empty rather than nil, which is the one difference JSON forces.
	want := epOut{Tags: []string{}, Attrs: map[string]string{}, Blob: []byte{}}
	assertEqualValue(t, got, want)
	received := seen.last(t)
	// The body's own members are sent as JSON too, so their nil slices and
	// maps arrive empty for the same reason. Every located field arrives as
	// it was: absent, and so zero.
	received.Tags, received.Meta, received.Blob = nil, nil, nil
	assertEqualValue(t, received, sent)
}

// epDefaultsIn has defaults that a call must not trigger by leaving a zero
// value out.
type epDefaultsIn struct {
	Limit  int     `query:"limit" default:"50"`
	Mode   string  `header:"X-Mode" default:"fast"`
	Ptr    *int    `query:"ptr" default:"9"`
	Format string  `cookie:"format" default:"json"`
	Ratio  float64 `json:"ratio" default:"0.5"`
}

func TestEndpointSendsZeroValuesSoDefaultsDoNotReplaceThem(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[epDefaultsIn, Empty](http.MethodPost, "/defaults")
	seen := &capture[epDefaultsIn]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, in epDefaultsIn) (Empty, error) {
			seen.record(in)
			return Empty{}, nil
		})
	})
	if _, err := ep.Call(context.Background(), client, epDefaultsIn{}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Every zero value was sent, so none was replaced by its default; the nil
	// pointer was not sent, and the server applied the default, which is what
	// a nil pointer means.
	nine := 9
	assertEqualValue(t, seen.last(t), epDefaultsIn{Ptr: &nine})
}

// epListsIn holds lists in every location that takes one.
type epListsIn struct {
	Q  []string   `query:"q"`
	H  []int      `header:"X-H"`
	HB []byte     `header:"X-HB"`
	T  []epText   `query:"t"`
	TT []*epText  `header:"X-TT"`
	D  []string   `json:"d"`
	P  *[]float64 `query:"p"`
}

func TestEndpointRoundTripsLists(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[epListsIn, Empty](http.MethodPut, "/lists")
	seen := &capture[epListsIn]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, in epListsIn) (Empty, error) {
			seen.record(in)
			return Empty{}, nil
		})
	})
	for _, sent := range []epListsIn{
		{Q: []string{"a"}, H: []int{1}, HB: []byte{7}, T: []epText{{"a", "b"}}, TT: []*epText{{"c", "d"}}, D: []string{"x"}, P: &[]float64{1.5}},
		{Q: []string{"a", "a", ""}, H: []int{-1, 0, 1}, HB: []byte{0, 255}, D: []string{}, P: &[]float64{0, -2e-10, 1e21}},
	} {
		if _, err := ep.Call(context.Background(), client, sent); err != nil {
			t.Fatalf("Call: %v", err)
		}
		assertEqualValue(t, seen.last(t), sent)
	}

	// An empty list has no spelling in a request, so it arrives absent.
	if _, err := ep.Call(context.Background(), client, epListsIn{Q: []string{}, P: &[]float64{}, D: []string{"x"}}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	assertEqualValue(t, seen.last(t), epListsIn{D: []string{"x"}})
}

// epFormIn is sent as a form, with values and files.
type epFormIn struct {
	ID     string   `path:"id"`
	Name   string   `form:"name"`
	Count  *int     `form:"count" required:"false"`
	Tags   []string `form:"tag" required:"false"`
	Avatar []byte   `file:"avatar"`
	Extra  [][]byte `file:"extra" required:"false"`
	Note   string   `form:"note" required:"false"`
}

func TestEndpointRoundTripsAFormWithFiles(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		ep := NewEndpoint[epFormIn, Empty](method, "/forms/{id}")
		seen := &capture[epFormIn]{}
		client := endpointServer(t, func(app *App) {
			app.Implement(ep, func(_ *Context, in epFormIn) (Empty, error) {
				seen.record(in)
				return Empty{}, nil
			})
		})
		three := 3
		sent := epFormIn{
			ID: "f1", Name: "line one\r\nline two\x00 and \xff bytes", Count: &three,
			Tags:   []string{"a", "", "c\r\n--boundary"},
			Avatar: []byte("\x89PNG\r\n\x1a\n binary"),
			Extra:  [][]byte{[]byte("first"), {}, []byte("third")},
		}
		if _, err := ep.Call(context.Background(), client, sent); err != nil {
			t.Fatalf("%s: Call: %v", method, err)
		}
		assertEqualValue(t, seen.last(t), sent)
	}
}

func TestEndpointSendsAnEmptyFileThatIsNotNil(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[epFormIn, Empty](http.MethodPost, "/forms/{id}")
	seen := &capture[epFormIn]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, in epFormIn) (Empty, error) {
			seen.record(in)
			return Empty{}, nil
		})
	})
	if _, err := ep.Call(context.Background(), client, epFormIn{ID: "x", Name: "n", Avatar: []byte{}}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got := seen.last(t)
	if got.Avatar == nil || len(got.Avatar) != 0 || got.Extra != nil || got.Count != nil {
		t.Fatalf("got %#v, want an empty avatar and nothing else", got)
	}

	// A nil file is not sent, and a required one is then refused by the
	// server, as a missing file is.
	_, err := ep.Call(context.Background(), client, epFormIn{ID: "x", Name: "n"})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnprocessableEntity || len(remote.Details) != 1 || remote.Details[0].Field != "avatar" {
		t.Fatalf("got %v (%+v), want a 422 naming the avatar", err, remote)
	}
}

// epBodyOnlyIn is decoded straight into the input, every field a member.
type epBodyOnlyIn struct {
	A string            `json:"a"`
	B *int              `json:"b,omitzero"`
	C map[string][]bool `json:"c"`
}

// epSelfDecoding decodes itself, which the binder leaves to its method.
type epSelfDecoding struct {
	ID    string `path:"id"`
	Value string `json:"value"`
}

func (s *epSelfDecoding) UnmarshalJSON(b []byte) error {
	var raw struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Value = strings.ToUpper(raw.Value)
	return nil
}

func TestEndpointRoundTripsBodyShapes(t *testing.T) {
	t.Parallel()
	direct := NewEndpoint[epBodyOnlyIn, epBodyOnlyIn](http.MethodPost, "/direct")
	self := NewEndpoint[epSelfDecoding, Empty](http.MethodPost, "/self/{id}")
	seenSelf := &capture[epSelfDecoding]{}
	client := endpointServer(t, func(app *App) {
		app.Implement(direct, func(_ *Context, in epBodyOnlyIn) (epBodyOnlyIn, error) { return in, nil })
		app.Implement(self, func(_ *Context, in epSelfDecoding) (Empty, error) {
			seenSelf.record(in)
			return Empty{}, nil
		})
	})
	two := 2
	sent := epBodyOnlyIn{A: "a", B: &two, C: map[string][]bool{"x": {true, false}}}
	got, err := direct.Call(context.Background(), client, sent)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	assertEqualValue(t, got, sent)

	// The input decodes itself, so it is written whole and reads itself
	// back; the path parameter still comes from the path.
	if _, err := self.Call(context.Background(), client, epSelfDecoding{ID: "p", Value: "shout"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	assertEqualValue(t, seenSelf.last(t), epSelfDecoding{ID: "p", Value: "SHOUT"})
}

// epSelfDecodingSecrets decodes itself, with located fields that carry
// credentials beside its one body member, and encodes itself as any struct is.
type epSelfDecodingSecrets struct {
	epSelfDecodingShared
	Token  string `header:"Authorization"`
	Sess   string `cookie:"session"`
	ID     string `path:"id"`
	Search string `query:"search"`
	User   Dep[depUser]
	Value  string `json:"value"`
}

// epSelfDecodingShared is embedded by value and holds a located field.
type epSelfDecodingShared struct {
	Tenant string `header:"X-Tenant"`
}

func (s *epSelfDecodingSecrets) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Value, _ = raw["value"].(string)
	return nil
}

// epSelfEncodingSecrets reads and writes itself, and writes every field it
// has, as a careless encoder would.
type epSelfEncodingSecrets struct {
	Token string `header:"Authorization"`
	User  Dep[*depUser]
	Value string `json:"value"`
}

func (s *epSelfEncodingSecrets) UnmarshalJSON(b []byte) error {
	var raw struct{ Value string }
	err := json.Unmarshal(b, &raw)
	s.Value = raw.Value
	return err
}

func (s epSelfEncodingSecrets) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"Value": s.Value, "Token": s.Token, "User": s.User.Get()})
}

func TestEndpointKeepsLocatedFieldsOutOfABodyTheInputDecodesItself(t *testing.T) {
	t.Parallel()
	ep := NewEndpoint[epSelfDecodingSecrets, Empty](http.MethodPost, "/self/{id}")
	writes := NewEndpoint[epSelfEncodingSecrets, Empty](http.MethodPost, "/writes")
	var (
		mu   sync.Mutex
		body []byte
	)
	client := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	sentBody := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(body)
	}
	sent := epSelfDecodingSecrets{epSelfDecodingShared{"tenant-secret"}, "Bearer token-secret", "session-secret", "id-secret", "search-secret", Dep[depUser]{}, "v"}
	if _, err := ep.Call(context.Background(), client, sent); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The input is written whole, as the binder reads it whole, but a
	// located field is the path's, a header's or a cookie's to carry, and a
	// credential in one does not belong in a body that is logged and kept.
	if got := sentBody(); got != `{"value":"v"}` {
		t.Fatalf("the body is %s", got)
	}
	// One that writes itself is handed nothing to write but its members.
	if _, err := writes.Call(context.Background(), client, epSelfEncodingSecrets{Token: "Bearer token-secret", Value: "v"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := sentBody(); strings.Contains(got, "secret") || !strings.Contains(got, `"Value":"v"`) {
		t.Fatalf("the body is %s", got)
	}
}

func TestEndpointRoundTripsTheCatchAllEdges(t *testing.T) {
	t.Parallel()
	type in struct {
		Rest string `path:"rest"`
	}
	root := NewEndpoint[in, in](http.MethodGet, "/{rest...}")
	under := NewEndpoint[in, in](http.MethodGet, "/files/{rest...}")
	client := endpointServer(t, func(app *App) {
		app.Implement(root, func(_ *Context, v in) (in, error) { return v, nil })
		app.Implement(under, func(_ *Context, v in) (in, error) { return in{Rest: "under:" + v.Rest}, nil })
	})
	for _, rest := range []string{"", "a", "a/b/c", "%2F", "x y/\xc3\xbc/%/?/#", "...", ".a/b."} {
		got, err := root.Call(context.Background(), client, in{Rest: rest})
		if err != nil {
			t.Fatalf("%q: Call: %v", rest, err)
		}
		// "files/..." at the root goes to the route registered for it, which
		// is how the router resolves the two, and is not a change of value.
		if strings.HasPrefix(rest, "files/") {
			continue
		}
		assertEqualValue(t, got, in{Rest: rest})
	}
	got, err := under.Call(context.Background(), client, in{Rest: ""})
	if err != nil || got.Rest != "under:" {
		t.Fatalf("got %+v, %v; want the empty remainder under /files/", got, err)
	}
}

func TestEndpointRoundTripsEveryNumberAtItsBounds(t *testing.T) {
	t.Parallel()
	type nums struct {
		I8  int8    `query:"i8"`
		I16 int16   `query:"i16"`
		I32 int32   `header:"X-I32"`
		I64 int64   `path:"i64"`
		U8  uint8   `query:"u8"`
		U32 uint32  `cookie:"u32"`
		U   uint    `query:"u"`
		F32 float32 `query:"f32"`
		F64 float64 `header:"X-F64"`
	}
	ep := NewEndpoint[nums, nums](http.MethodGet, "/nums/{i64}")
	client := endpointServer(t, func(app *App) {
		app.Implement(ep, func(_ *Context, v nums) (nums, error) { return v, nil })
	})
	for _, sent := range []nums{
		{I8: math.MinInt8, I16: math.MinInt16, I32: math.MinInt32, I64: math.MinInt64, F32: -math.MaxFloat32, F64: -math.MaxFloat64},
		{I8: math.MaxInt8, I16: math.MaxInt16, I32: math.MaxInt32, I64: math.MaxInt64, U8: math.MaxUint8, U32: math.MaxUint32, U: math.MaxUint,
			F32: math.SmallestNonzeroFloat32, F64: math.SmallestNonzeroFloat64},
		{F32: 1e-7, F64: 123456789012345678901234567890},
		{F32: float32(math.Copysign(0, -1)), F64: math.Copysign(0, -1)},
	} {
		got, err := ep.Call(context.Background(), client, sent)
		if err != nil {
			t.Fatalf("%+v: Call: %v", sent, err)
		}
		assertEqualValue(t, got, sent)
		if math.Signbit(sent.F64) != math.Signbit(got.F64) {
			t.Fatalf("the sign of zero was lost: %v", got.F64)
		}
	}
}
