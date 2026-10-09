package muzak

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type headerListIn struct {
	IDs  []int    `header:"X-Ids"`
	Tags []string `header:"X-Tags"`
	Name string   `header:"X-Name"`
}

// TestHeaderListIsSplitOnCommas covers a slice bound from a header. RFC 9110
// lets a sender write a list header as one line with comma-separated elements
// or as several lines, and requires the two to mean the same thing; the binder
// read only the lines, so "X-Ids: 1, 2" was one element, "1, 2", and a 422 for
// a []int. Each line is now split on the commas outside quoted strings, with
// the whitespace around each element dropped and empty elements skipped. A
// scalar header is read whole, commas included.
func TestHeaderListIsSplitOnCommas(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/h", func(_ *Context, in headerListIn) (headerListIn, error) { return in, nil })
	mustBuild(t, app)

	for _, tc := range []struct {
		name  string
		lines map[string][]string
		want  string
	}{
		{"one line", map[string][]string{"X-Ids": {"1, 2"}},
			`{"IDs":[1,2],"Tags":[],"Name":""}`},
		{"lines and commas together", map[string][]string{"X-Ids": {"1,2", "3"}},
			`{"IDs":[1,2,3],"Tags":[],"Name":""}`},
		{"empty elements and tabs", map[string][]string{"X-Ids": {" ,1,,\t2 , "}},
			`{"IDs":[1,2],"Tags":[],"Name":""}`},
		{"nothing but separators", map[string][]string{"X-Ids": {" , "}},
			`{"IDs":[],"Tags":[],"Name":""}`},
		{"quoted strings keep their commas", map[string][]string{"X-Tags": {`"a,b", c, "d\",e"`}},
			`{"IDs":[],"Tags":["\"a,b\"","c","\"d\\\",e\""],"Name":""}`},
		{"a scalar is read whole", map[string][]string{"X-Name": {"Lovelace, Ada"}},
			`{"IDs":[],"Tags":[],"Name":"Lovelace, Ada"}`},
	} {
		req := httptest.NewRequest(http.MethodGet, "/h", nil)
		for name, lines := range tc.lines {
			for _, line := range lines {
				req.Header.Add(name, line)
			}
		}
		rec := doRequest(t, app, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200\nbody: %s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		assertJSON(t, rec, tc.want)
	}

	// An element that does not parse is named by its place in the list.
	req := httptest.NewRequest(http.MethodGet, "/h", nil)
	req.Header.Set("X-Ids", "1, x")
	details := decodeError(t, doRequest(t, app, req)).Error.Details
	if len(details) != 1 || details[0].Issue != "entry 2 must be a valid integer" {
		t.Errorf("details = %+v, want the second entry named", details)
	}
}
