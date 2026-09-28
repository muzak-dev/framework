package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"muzak.dev/framework/validate"
)

// requiredTags reports every empty tag, which is one failure per element.
type requiredTags struct {
	Tags []string `json:"tags"`
}

func (in *requiredTags) Validate(v *Validation) {
	v.Slice(&in.Tags).Each(validate.String().Required())
}

// assertTruncated checks a report that reached the ceiling: exactly
// MaxValidationDetails failures, then the marker saying there were more.
func assertTruncated(t *testing.T, details []ErrorDetail) {
	t.Helper()
	if len(details) != MaxValidationDetails+1 {
		t.Fatalf("details = %d entries, want %d and the marker", len(details), MaxValidationDetails)
	}
	marker := details[len(details)-1]
	if marker.Field != "" || marker.Location != "body" || !strings.Contains(marker.Issue, "more problems than the 100 listed") {
		t.Errorf("the last detail is %+v, want the marker saying the report was cut short", marker)
	}
}

// TestEachReportIsCappedAtTheCeiling is the regression test for a collection
// rule turning a request into a report many times its size: a megabyte of empty
// strings was a 22 megabyte response.
func TestEachReportIsCappedAtTheCeiling(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/tags", func(ctx *Context, in requiredTags) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	n := (1<<20 - 20) / 3
	body := `{"tags":` + jsonArray(n, func(int) string { return `""` }) + `}`
	rec := do(t, app, "POST", "/tags", body)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if rec.Body.Len() > 16<<10 {
		t.Errorf("a %d byte request produced a %d byte report", len(body), rec.Body.Len())
	}

	details := decodeError(t, rec).Error.Details
	assertTruncated(t, details)
	for i, detail := range details[:MaxValidationDetails] {
		if want := fmt.Sprintf("tags[%d]", i); detail.Field != want {
			t.Fatalf("detail %d names %q, want %q: the report must list the first failures in order", i, detail.Field, want)
		}
	}
}

// countedTags counts how many elements its rule is asked about.
type countedTags struct {
	Tags  []string `json:"tags"`
	Notes []string `json:"notes"`
}

var countedTagChecks, countedNoteChecks atomic.Int64

func (in *countedTags) Validate(v *Validation) {
	v.Slice(&in.Tags).Each(validate.Value[string]().Must(func(string) error {
		countedTagChecks.Add(1)
		return errors.New("is refused")
	}))
	v.Slice(&in.Notes).Each(validate.Value[string]().Must(func(string) error {
		countedNoteChecks.Add(1)
		return errors.New("is refused")
	}))
}

// TestEachStopsEvaluatingAtTheCeiling checks that the ceiling bounds the work
// and not only the response: a report that is full stops evaluating the
// collection one element after it knows it is incomplete.
func TestEachStopsEvaluatingAtTheCeiling(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/tags", func(ctx *Context, in countedTags) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	// Measured as a difference, so the test holds when it is run repeatedly.
	tagsBefore, notesBefore := countedTagChecks.Load(), countedNoteChecks.Load()
	many := jsonArray(10_000, func(int) string { return `"x"` })
	rec := do(t, app, "POST", "/tags", `{"tags":`+many+`,"notes":`+many+`}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	assertTruncated(t, decodeError(t, rec).Error.Details)
	if got := countedTagChecks.Load() - tagsBefore; got != MaxValidationDetails+1 {
		t.Errorf("the rule ran %d times over 10000 elements, want %d", got, MaxValidationDetails+1)
	}
	// A rule declared after the report was known to be incomplete does not
	// run at all.
	if got := countedNoteChecks.Load() - notesBefore; got != 0 {
		t.Errorf("the second rule ran %d times, want none", got)
	}
}

// TestReportAtTheCeilingHasNoMarker pins the boundary: exactly as many failures
// as the report holds is a complete report, and one more is not.
func TestReportAtTheCeilingHasNoMarker(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/tags", func(ctx *Context, in requiredTags) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/tags", `{"tags":`+jsonArray(MaxValidationDetails, func(int) string { return `""` })+`}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if details := decodeError(t, rec).Error.Details; len(details) != MaxValidationDetails ||
		details[len(details)-1].Field != fmt.Sprintf("tags[%d]", MaxValidationDetails-1) {
		t.Errorf("a report of exactly %d failures was %d entries ending %+v",
			MaxValidationDetails, len(details), details[len(details)-1])
	}

	rec = do(t, app, "POST", "/tags", `{"tags":`+jsonArray(MaxValidationDetails+1, func(int) string { return `""` })+`}`)
	assertTruncated(t, decodeError(t, rec).Error.Details)
}

// perElement nests one model per element, which is the other way a report grew
// with the length of a collection.
type perElement struct {
	Items []countedItem `json:"items"`
}

type countedItem struct {
	Name string `json:"name"`
}

var countedItemValidations atomic.Int64

func (i *countedItem) Validate(v *Validation) {
	countedItemValidations.Add(1)
	v.String(&i.Name).Required()
}

func (in *perElement) Validate(v *Validation) {
	for i := range in.Items {
		v.Nested(&in.Items[i])
	}
}

// TestNestedPerElementStopsAtTheCeiling is the regression test for a model
// nested once per element: each element's failures used to be kept, and every
// element validated, however many the client sent.
func TestNestedPerElementStopsAtTheCeiling(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/items", func(ctx *Context, in perElement) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	before := countedItemValidations.Load()
	n := (1<<20 - 20) / 3
	rec := do(t, app, "POST", "/items", `{"items":`+jsonArray(n, func(int) string { return `{}` })+`}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if rec.Body.Len() > 16<<10 {
		t.Errorf("the report is %d bytes", rec.Body.Len())
	}
	assertTruncated(t, decodeError(t, rec).Error.Details)
	if got := countedItemValidations.Load() - before; got != MaxValidationDetails+1 {
		t.Errorf("%d elements were validated, want %d: nothing past the ceiling should be", got, MaxValidationDetails+1)
	}
}

// rejectEach rejects once per element from an ordinary loop.
type rejectEach struct {
	Codes []string `json:"codes"`
}

func (in *rejectEach) Validate(v *Validation) {
	for i := range in.Codes {
		v.Reject(&in.Codes, "must not hold "+in.Codes[i])
	}
	v.When(len(in.Codes) > 0).RejectKey(&in.Codes, "errors.codes.refused")
}

// TestRejectionsAreCappedToo covers a cross-field check written in a loop,
// which rejects as often as the client sent elements.
func TestRejectionsAreCappedToo(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/codes", func(ctx *Context, in rejectEach) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/codes", `{"codes":`+jsonArray(5_000, func(int) string { return `"x"` })+`}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	assertTruncated(t, decodeError(t, rec).Error.Details)
}

// ownAndNested declares a rule of its own before nesting a model.
type ownAndNested struct {
	Tags []string `json:"tags"`
	Ship address  `json:"ship_to"`
}

func (in *ownAndNested) Validate(v *Validation) {
	v.Slice(&in.Tags).Each(validate.String().Required())
	v.Nested(&in.Ship)
}

// TestOwnFailuresComeBeforeNestedOnes checks the order of a report that mixes a
// model's own failures with a nested model's. The nested model is validated
// first, when Nested is called, and the report is put back in the order it has
// always had.
func TestOwnFailuresComeBeforeNestedOnes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/mixed", func(ctx *Context, in ownAndNested) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/mixed", `{"tags":[""],"ship_to":{"street":"","city":"Ankara","postcode":"06000"}}`)
	details := decodeError(t, rec).Error.Details
	if len(details) != 2 || details[0].Field != "tags[0]" || details[1].Field != "ship_to.street" {
		t.Errorf("details = %+v, want the own failure before the nested one", details)
	}
}
