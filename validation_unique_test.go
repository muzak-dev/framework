package muzak

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type uniqueIDsIn struct {
	IDs []int `json:"ids"`
}

func (in *uniqueIDsIn) Validate(v *Validation) {
	v.Slice(&in.IDs).Unique()
}

type uniqueCappedIn struct {
	Tags   []string `json:"tags"`
	Groups [][]int  `json:"groups"`
}

func (in *uniqueCappedIn) Validate(v *Validation) {
	v.Slice(&in.Tags).Unique().MaxItems(100)
	v.Slice(&in.Groups).Unique().MaxItems(100)
}

// jsonArray renders n elements produced by one function as a JSON array.
func jsonArray(n int, element func(int) string) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(element(i))
	}
	sb.WriteByte(']')
	return sb.String()
}

// requestDeadline is generous for the race detector and still orders of
// magnitude below what the pairwise search spent on these bodies.
const requestDeadline = 5 * time.Second

// TestUniqueOverALargeBodyIsLinear is the review's proof of concept: 228 KB
// of distinct integers cost 24 seconds of CPU when Unique compared every pair.
func TestUniqueOverALargeBodyIsLinear(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/bulk", func(ctx *Context, in uniqueIDsIn) (map[string]int, error) {
		return map[string]int{"n": len(in.IDs)}, nil
	}, MaxBodySize(4<<20))
	mustBuild(t, app)

	body := `{"ids":` + jsonArray(200_000, func(i int) string { return fmt.Sprint(i) }) + `}`
	start := time.Now()
	rec := do(t, app, "POST", "/bulk", body)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"n":200000}`)
	if elapsed := time.Since(start); elapsed > requestDeadline {
		t.Errorf("200000 distinct ids took %v", elapsed)
	}

	rec = do(t, app, "POST", "/bulk", `{"ids":[1,2,3,2,1]}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "must not repeat 1") {
		t.Errorf("body = %s, want the earliest repeat named", rec.Body.String())
	}
}

// TestUniqueBeforeMaxItemsIsBounded checks that the bound written after Unique
// still bounds it, including for elements that cannot be hashed and so would
// be compared pair by pair.
func TestUniqueBeforeMaxItemsIsBounded(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/tags", func(ctx *Context, in uniqueCappedIn) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	for _, body := range []string{
		`{"tags":` + jsonArray(20_000, func(i int) string { return fmt.Sprintf("%q", fmt.Sprint(i)) }) + `,"groups":[]}`,
		`{"tags":[],"groups":` + jsonArray(20_000, func(i int) string { return fmt.Sprintf("[%d]", i) }) + `}`,
	} {
		start := time.Now()
		rec := do(t, app, "POST", "/tags", body)
		assertStatus(t, rec, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), "must have at most 100 items") {
			t.Errorf("body = %.200s, want the bound reported", rec.Body.String())
		}
		if elapsed := time.Since(start); elapsed > requestDeadline {
			t.Errorf("a bounded Unique over 20000 elements took %v", elapsed)
		}
	}
}
