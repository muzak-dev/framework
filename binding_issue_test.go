package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// A repeated parameter that fails names the entry by position and never
// quotes the value, which is the client's own text: quoting 450 KB of bytes
// that are not UTF-8 sent back four times as much.
func TestRepeatedParameterFailureNamesTheEntryNotTheValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/echo", func(ctx *Context, in struct {
		IDs []int `header:"X-Ids"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	built := mustBuild(t, app)

	value := strings.Repeat("\xff", 450_000)
	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	req.Header.Set("X-Ids", value)
	rec := doRequest(t, built, req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if rec.Body.Len() > 1024 {
		t.Errorf("a 450 KB header produced a %d byte response, want the value left out", rec.Body.Len())
	}
	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Issue != "entry 1 must be a valid integer" {
		t.Errorf("details = %+v, want one naming entry 1 and no value", details)
	}

	rec = do(t, built, http.MethodGet, "/echo")
	assertStatus(t, rec, http.StatusOK)
}

// A standard library TextUnmarshaler writes errors that name Go internals and
// quote the input back; the client gets the same fixed phrase a built-in kind
// would produce instead.
func TestTextUnmarshalerFailureIsAFixedPhrase(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/events", func(ctx *Context, in struct {
		Since time.Time    `query:"since"`
		From  netip.Addr   `query:"from"`
		Seen  []netip.Addr `query:"seen"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	built := mustBuild(t, app)

	rec := do(t, built, http.MethodGet, "/events?since=yesterday&from=10.0.0.999&seen=10.0.0.1&seen=nope")
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	body := rec.Body.String()
	for _, leak := range []string{"2006-01-02T15:04:05Z07:00", "ParseAddr", "yesterday", "10.0.0.999", "nope"} {
		if strings.Contains(body, leak) {
			t.Errorf("the response carries %q: %s", leak, body)
		}
	}
	issues := map[string]string{}
	for _, detail := range decodeError(t, rec).Error.Details {
		issues[detail.Field] = detail.Issue
	}
	want := map[string]string{
		"since": "is not in the expected format",
		"from":  "is not in the expected format",
		"seen":  "entry 2 is not in the expected format",
	}
	for field, issue := range want {
		if issues[field] != issue {
			t.Errorf("issue for %s = %q, want %q", field, issues[field], issue)
		}
	}
}

// hexColour is a parameter type whose own failure is written for the client,
// as an HTTPError, which is the one kind of error passed through.
type hexColour string

func (h *hexColour) UnmarshalText(text []byte) error {
	if string(text) == "silent" {
		return &HTTPError{Status: http.StatusBadRequest}
	}
	if len(text) != 7 || text[0] != '#' {
		return NewHTTPError(http.StatusBadRequest, "must be a colour such as #ff8800").Wrap(errors.New("the internal reason"))
	}
	*h = hexColour(text)
	return nil
}

func TestTextUnmarshalerHTTPErrorIsPassedThrough(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/paint", func(ctx *Context, in struct {
		Fill    hexColour   `query:"fill"`
		Strokes []hexColour `query:"stroke"`
		Silent  hexColour   `query:"silent"`
	}) (Empty, error) {
		return Empty{}, nil
	})
	built := mustBuild(t, app)

	rec := do(t, built, http.MethodGet, "/paint?fill=red&stroke=%23000000&stroke=blue&silent=silent")
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if strings.Contains(rec.Body.String(), "internal reason") {
		t.Errorf("the response carries the HTTPError's cause: %s", rec.Body.String())
	}
	got := map[string]ErrorDetail{}
	for _, detail := range decodeError(t, rec).Error.Details {
		got[detail.Field] = detail
	}
	if issue := got["fill"].Issue; issue != "must be a colour such as #ff8800" {
		t.Errorf("fill issue = %q, want the HTTPError's message", issue)
	}
	if issue := got["stroke"].Issue; issue != "entry 2 must be a colour such as #ff8800" {
		t.Errorf("stroke issue = %q, want the entry and the HTTPError's message", issue)
	}
	// An HTTPError with nothing to say falls back to the fixed phrase rather
	// than sending an empty issue.
	if issue := got["silent"].Issue; issue != "is not in the expected format" {
		t.Errorf("silent issue = %q, want the fixed phrase", issue)
	}

	issue, key, args := paramIssue(&hexColourKeyed)
	if issue != hexColourKeyed.Message || key != "errors.colour" || len(args) != 1 {
		t.Errorf("paramIssue = %q %q %v, want the HTTPError's key and arguments carried", issue, key, args)
	}
	if _, key, _ := paramIssue(errors.New("a parser's own words")); key != "muzak.binding.format" {
		t.Errorf("key for a TextUnmarshaler failure = %q, want muzak.binding.format", key)
	}
}

var hexColourKeyed = HTTPError{Status: http.StatusBadRequest, Message: "must be a colour", MessageKey: "errors.colour", MessageArgs: []any{"x"}}

// A failed entry still unwraps to the setter's own failure, so errors.Is sees
// the sentinel through it the way it saw through the wrapping it replaced.
func TestEntryErrorUnwrapsToTheSetterFailure(t *testing.T) {
	t.Parallel()
	err := error(&entryError{index: 3, err: errNotInt})
	if !errors.Is(err, errNotInt) {
		t.Errorf("errors.Is(%v, errNotInt) = false, want the entry seen through", err)
	}
	if got := err.Error(); got != "entry 3 must be a valid integer" {
		t.Errorf("Error() = %q, want the position and no value", got)
	}
}
