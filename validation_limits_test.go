package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
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

// thread is a model that nests its own type, as deep as the client makes it.
type thread struct {
	Name  string  `json:"name"`
	Reply *thread `json:"reply"`
}

var threadValidations atomic.Int64

func (in *thread) Validate(v *Validation) {
	threadValidations.Add(1)
	v.String(&in.Name).Required()
	v.Nested(in.Reply)
}

// threadBody nests depth replies, each with the given name.
func threadBody(depth int, name string) string {
	level := `{"name":` + name + `,"reply":`
	return strings.Repeat(level, depth) + `{"name":` + name + `}` + strings.Repeat(`}`, depth)
}

// replyPath is the path of the model depth replies down.
func replyPath(depth int) string {
	return strings.TrimSuffix(strings.Repeat("reply.", depth), ".")
}

// TestRecursiveNestingIsBoundedInDepth is the regression test for a recursive
// model validated with Nested: ten thousand levels of it built a dotted path per
// level, reported the whole path at every level that failed, and cost a 300
// megabyte report. Past MaxNestedDepth the model is refused rather than
// followed.
//
// It is not parallel because it counts calls to thread.Validate through a
// package-level counter, and TestPooledValidationKeepsNothingFromTheRequest
// validates a thread of its own: run beside it, the count picked up that test's
// three calls and failed on a loaded machine.
func TestRecursiveNestingIsBoundedInDepth(t *testing.T) {
	app := New(quietOptions())
	app.Post("/threads", func(ctx *Context, in thread) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	// Every level fails, so the report holds one failure per level followed
	// and then the refusal of the level that was not.
	rec := do(t, app, "POST", "/threads", threadBody(9_990, `""`))
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if rec.Body.Len() > 16<<10 {
		t.Errorf("the report is %d bytes", rec.Body.Len())
	}
	details := decodeError(t, rec).Error.Details
	if len(details) != MaxNestedDepth+2 {
		t.Fatalf("details = %d entries, want %d", len(details), MaxNestedDepth+2)
	}
	for depth := range MaxNestedDepth + 1 {
		if want := joinPath(replyPath(depth), "name"); details[depth].Field != want {
			t.Fatalf("detail %d names %q, want %q", depth, details[depth].Field, want)
		}
	}
	refused := details[MaxNestedDepth+1]
	if refused.Field != replyPath(MaxNestedDepth+1) || refused.Issue != "must not be nested more than 32 levels deep" {
		t.Errorf("the last detail is %+v, want the refusal of the level past the limit", refused)
	}

	// A deep thread with nothing wrong in it is refused all the same, because
	// the part past the limit was never checked.
	before := threadValidations.Load()
	rec = do(t, app, "POST", "/threads", threadBody(9_990, `"a"`))
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if details := decodeError(t, rec).Error.Details; len(details) != 1 || details[0].Field != replyPath(MaxNestedDepth+1) {
		t.Errorf("details = %+v, want only the refusal", details)
	}
	if ran := threadValidations.Load() - before; ran != MaxNestedDepth+1 {
		t.Errorf("Validate ran %d times, want %d", ran, MaxNestedDepth+1)
	}

	// A thread as deep as the limit is validated all the way down.
	rec = do(t, app, "POST", "/threads", threadBody(MaxNestedDepth, `"a"`))
	assertStatus(t, rec, http.StatusOK)
}

// TestPooledValidationKeepsNothingFromTheRequest is the regression test for a
// pooled Validation that kept every nested model's Validation, and through it
// the request's body, alive after the request had finished.
func TestPooledValidationKeepsNothingFromTheRequest(t *testing.T) {
	t.Parallel()
	in := &thread{Reply: &thread{Reply: &thread{}}}
	typ := reflect.TypeFor[thread]()

	v := &Validation{
		plan:  planForType(typ),
		base:  reflect.ValueOf(in).Pointer(),
		size:  typ.Size(),
		value: reflect.ValueOf(in).Elem(),
	}
	v.run = &v.state
	in.Validate(v)
	v.Reject(&in.Name, "is refused")
	if details := v.details(); len(details) != 4 || details[3].Field != "reply.reply.name" {
		t.Fatalf("details = %+v", details)
	}
	v.reset()

	all := append([]*Validation{v}, v.state.nested...)
	if len(all) != 3 {
		t.Fatalf("%d Validations were kept for reuse, want one per level", len(all))
	}
	for depth, kept := range all {
		if kept.value.IsValid() || kept.plan != nil || kept.parent != nil || kept.run != nil || kept.path != "" {
			t.Errorf("level %d still points into the request: %+v", depth, kept)
		}
		for _, rules := range kept.freeStrings {
			if rules.Target() != nil {
				t.Errorf("level %d kept a rule set bound to a field", depth)
			}
		}
		if len(kept.rules) != 0 || cap(kept.rules) > 0 && kept.rules[:1][0] != nil {
			t.Errorf("level %d kept a rule set in its list", depth)
		}
		if cap(kept.rejected) > 0 && kept.rejected[:1][0].target != nil {
			t.Errorf("level %d kept a rejection", depth)
		}
	}
	if v.state.out != nil {
		t.Error("the report was kept")
	}
}

// panicky nests a model whose Validate panics partway through.
type panicky struct {
	Inner exploding `json:"inner"`
}

type exploding struct {
	Name string `json:"name"`
}

func (in *exploding) Validate(v *Validation) {
	v.String(&in.Name).Required()
	panic("the model's own bug")
}

func (in *panicky) Validate(v *Validation) { v.Nested(&in.Inner) }

// TestANestedModelThatPanicsLeavesNothingBehind covers the nested Validation
// kept for reuse when its model's Validate panics rather than returning.
func TestANestedModelThatPanicsLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	in := &panicky{}
	v := &Validation{value: reflect.ValueOf(in).Elem()}
	v.run = &v.state
	if recovered := catchPanic(func() { in.Validate(v) }); recovered == nil {
		t.Fatal("the model did not panic")
	}
	if child := v.state.nested[0]; child.value.IsValid() || child.freeStrings[0].Target() != nil {
		t.Error("the nested Validation still points into the request")
	}
}
