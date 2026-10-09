package muzak

import (
	"fmt"
	"math"
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
//
// The request is timed beside the same body sent to a route that reads it
// without asking for Unique, rather than held to a deadline: a deadline
// generous enough for the race detector was still crossed on a loaded
// machine, where both requests are slowed alike. Keying twenty thousand ids
// adds a pass about as long as the reading, and comparing every pair of them
// thousands of times as much. It is not parallel, so that what it times is
// its own work and not that of the tests alongside it.
func TestUniqueOverALargeBodyIsLinear(t *testing.T) {
	app := New(quietOptions())
	app.Post("/bulk", func(ctx *Context, in uniqueIDsIn) (map[string]int, error) {
		return map[string]int{"n": len(in.IDs)}, nil
	}, MaxBodySize(4<<20))
	app.Post("/read", func(ctx *Context, in struct {
		IDs []int `json:"ids"`
	}) (map[string]int, error) {
		return map[string]int{"n": len(in.IDs)}, nil
	}, MaxBodySize(4<<20))
	mustBuild(t, app)
	ids := func(n int) string {
		return `{"ids":` + jsonArray(n, func(i int) string { return fmt.Sprint(i) }) + `}`
	}

	const n, maxOverhead = 20_000, 100
	body := ids(n)
	read := func() { do(t, app, "POST", "/read", body) }
	unique := func() { do(t, app, "POST", "/bulk", body) }
	if overhead := costRatio(read, unique, maxOverhead); overhead > maxOverhead {
		t.Fatalf("%d distinct ids took %.0f times as long with Unique as without, want at most %d",
			n, overhead, maxOverhead)
	}

	rec := do(t, app, "POST", "/bulk", ids(200_000))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"n":200000}`)

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

// costRatio returns how many times as long as yardstick run takes.
//
// Each is timed several times, in turn, and the fastest timing of each is
// compared. Time the machine spends elsewhere only ever adds to a timing, so
// the fastest of several is the closest to the cost of the work itself, where
// one timing on a loaded machine can be stretched many times over.
//
// It stops as soon as the ratio is within limit, because one pair of timings
// that shows the work can be done that fast is the answer, and once two
// rounds leave it more than ten times over, which no stall makes of work
// within it, so that a search comparing every pair fails after two of its
// long calls rather than five.
func costRatio(yardstick, run func(), limit float64) float64 {
	best := [2]time.Duration{math.MaxInt64, math.MaxInt64}
	var ratio float64
	for round := range 5 {
		best[0] = min(best[0], perCall(yardstick))
		best[1] = min(best[1], perCall(run))
		ratio = float64(best[1]) / float64(best[0])
		if ratio <= limit || (round > 0 && ratio > 10*limit) {
			break
		}
	}
	return ratio
}

// perCall returns how long one call to run takes, averaged over as many calls
// as fill 50ms, so that a fast call is still timed in steps a coarse clock can
// see: the one on Windows can move in steps of 15.6ms.
func perCall(run func()) time.Duration {
	const span = 50 * time.Millisecond
	start := time.Now()
	for calls := 1; ; calls++ {
		run()
		if elapsed := time.Since(start); elapsed >= span {
			return elapsed / time.Duration(calls)
		}
	}
}
