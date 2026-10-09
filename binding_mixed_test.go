package muzak

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The inputs below read some fields from the path, a header or the query and
// the rest from the body. A located field without json:"-" was still a member
// the decoder knew, so a body could carry it: a well-formed value was accepted
// and silently dropped, and a malformed one was a 422 about a member the
// documented schema does not list.

type MixedAuth struct {
	User string `header:"X-User"`
}

// mixedPage is unexported and embedded by value, so its body members are
// promoted into the input and have to be copied out one by one.
type mixedPage struct {
	Title string `json:"title"`
	Size  int    `json:"size" default:"20"`
}

type mixedIn struct {
	ID     int    `path:"id"`
	Tenant string `header:"X-Tenant" json:"tenant"`
	Debug  bool   `query:"debug" json:"debug,omitzero"`
	MixedAuth
	mixedPage
	Name string `json:"name"`
}

// mixedApp serves mixedIn, echoing what the handler was given.
func mixedApp(t *testing.T, opts ...RouteOption) *App {
	t.Helper()
	app := New(quietOptions())
	app.Post("/m/{id}", func(_ *Context, in mixedIn) (map[string]any, error) {
		return map[string]any{
			"id": in.ID, "tenant": in.Tenant, "debug": in.Debug, "user": in.User,
			"title": in.Title, "size": in.Size, "name": in.Name,
		}, nil
	}, opts...)
	return mustBuild(t, app)
}

// mixedRequest builds a request carrying the located parameters and a body.
func mixedRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/m/3?debug=true", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", "acme")
	req.Header.Set("X-User", "ada")
	return req
}

// TestLocatedFieldsAreNotBodyMembers covers the default: a body member naming
// a located field is unknown, like any other member the body does not have.
func TestLocatedFieldsAreNotBodyMembers(t *testing.T) {
	t.Parallel()
	app := mixedApp(t)
	for _, tc := range []struct{ body, field string }{
		{`{"name":"x","ID":999}`, "ID"},
		{`{"name":"x","ID":"zz"}`, "ID"},
		{`{"name":"x","tenant":"evil"}`, "tenant"},
		{`{"name":"x","debug":"yes"}`, "debug"},
		{`{"name":"x","User":"mallory"}`, "User"},
	} {
		rec := doRequest(t, app, mixedRequest(tc.body))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422\nbody: %s", tc.body, rec.Code, rec.Body.String())
			continue
		}
		details := decodeError(t, rec).Error.Details
		if len(details) != 1 || details[0].Field != tc.field || details[0].Issue != "is not a field this endpoint accepts" {
			t.Errorf("%s: details = %+v, want %s refused as unknown", tc.body, details, tc.field)
		}
	}

	// The body members, promoted ones and defaults included, still bind, and
	// the located fields come from where they are declared.
	rec := doRequest(t, app, mixedRequest(`{"name":"x","title":"t"}`))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"id":3,"tenant":"acme","debug":true,"user":"ada","title":"t","size":20,"name":"x"}`)
}

// TestLocatedFieldsAreIgnoredWhenUnknownMembersAre covers AllowUnknownFields:
// a member naming a located field is ignored like any other unknown member,
// whatever it holds, and never reaches the field.
func TestLocatedFieldsAreIgnoredWhenUnknownMembersAre(t *testing.T) {
	t.Parallel()
	app := mixedApp(t, AllowUnknownFields())
	rec := doRequest(t, app, mixedRequest(`{"name":"x","ID":"zz","tenant":"evil","User":7,"size":5}`))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"id":3,"tenant":"acme","debug":true,"user":"ada","title":"","size":5,"name":"x"}`)
}

// collidingIn embeds an unexported struct beside a field already holding the
// exported name it would be rebuilt under.
type collidingIn struct {
	ID int `path:"id"`
	mixedPage
	XmixedPage string `json:"x"`
}

func TestRebuiltBodyKeepsEveryMember(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/c/{id}", func(_ *Context, in collidingIn) (map[string]any, error) {
		return map[string]any{"id": in.ID, "title": in.Title, "size": in.Size, "x": in.XmixedPage}, nil
	})
	mustBuild(t, app)
	rec := do(t, app, http.MethodPost, "/c/4", `{"title":"t","x":"y","size":3}`)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"id":4,"title":"t","size":3,"x":"y"}`)

	// A member of the rebuilt embedded struct is named as the body names it.
	rec = do(t, app, http.MethodPost, "/c/4", `{"title":"t","size":"big"}`)
	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Field != "size" || details[0].Issue != "has the wrong type, a string is not accepted here" {
		t.Errorf("details = %+v, want size named", details)
	}
}

// selfDecodingMixed reads its own body, so the binder cannot narrow what the
// body reaches and leaves the decoding to it; the located field is still
// taken from the header afterwards.
type selfDecodingMixed struct {
	Tenant string `header:"X-Tenant"`
	Name   string `json:"name"`
}

func (s *selfDecodingMixed) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Name = strings.ToUpper(raw.Name)
	s.Tenant = "from the body"
	return nil
}

func TestMixedInputThatDecodesItself(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/s", func(_ *Context, in selfDecodingMixed) (map[string]string, error) {
		return map[string]string{"tenant": in.Tenant, "name": in.Name}, nil
	})
	mustBuild(t, app)
	req := httptest.NewRequest(http.MethodPost, "/s", bytes.NewReader([]byte(`{"name":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant", "acme")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"tenant":"acme","name":"X"}`)
}
