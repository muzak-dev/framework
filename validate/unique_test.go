package validate

import (
	"fmt"
	"math"
	"net/netip"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// pairwiseRepeat is the search Unique used to run, kept as the reference the
// hashed search has to agree with.
func pairwiseRepeat[E any](list []E) (int, bool) {
	for i := range list {
		for j := i + 1; j < len(list); j++ {
			if reflect.DeepEqual(list[i], list[j]) {
				return i, true
			}
		}
	}
	return 0, false
}

// agreesWithPairwise checks firstRepeat against the reference on one input.
func agreesWithPairwise[E any](t *testing.T, name string, list []E) {
	t.Helper()
	gotIndex, gotFound := firstRepeat(list)
	wantIndex, wantFound := pairwiseRepeat(list)
	if gotFound != wantFound || (wantFound && gotIndex != wantIndex) {
		t.Errorf("%s: firstRepeat = (%d, %v), want (%d, %v)", name, gotIndex, gotFound, wantIndex, wantFound)
	}
}

type flatPoint struct {
	X, Y int
	Tag  string
}

type pointerPoint struct {
	X *int
}

type blankPoint struct {
	X int
	_ int
}

func TestFirstRepeatAgreesWithDeepEqual(t *testing.T) {
	t.Parallel()
	one, otherOne, two := 1, 1, 2

	agreesWithPairwise(t, "empty", []int{})
	agreesWithPairwise(t, "single", []int{1})
	agreesWithPairwise(t, "distinct", []int{1, 2, 3})
	// The repeat found first belongs to b, but a appears earlier, and the
	// pairwise search reported a.
	agreesWithPairwise(t, "earliest first occurrence", []string{"a", "b", "b", "a"})
	agreesWithPairwise(t, "late repeat", []string{"a", "b", "c", "d", "c"})
	agreesWithPairwise(t, "NaN equals nothing", []float64{math.NaN(), math.NaN()})
	agreesWithPairwise(t, "signed zeros are equal", []float64{0, math.Copysign(0, -1)})
	agreesWithPairwise(t, "flat structs", []flatPoint{{1, 2, "a"}, {1, 2, "b"}, {1, 2, "a"}})
	agreesWithPairwise(t, "arrays", [][2]uint8{{1, 2}, {2, 1}, {1, 2}})
	agreesWithPairwise(t, "complex", []complex128{1i, 2, 1i})
	// Pointers are compared by what they point at, which a map keyed on the
	// pointer itself would get wrong.
	agreesWithPairwise(t, "pointers to equal values", []*int{&one, &two, &otherOne})
	agreesWithPairwise(t, "structs holding pointers", []pointerPoint{{&one}, {&otherOne}})
	agreesWithPairwise(t, "interfaces", []any{[]int{1}, "x", []int{1}})
	agreesWithPairwise(t, "nested slices", [][]string{{"a"}, {"b"}})
	agreesWithPairwise(t, "blank fields", []blankPoint{{X: 1}, {X: 1}})
	agreesWithPairwise(t, "times", []time.Time{time.Unix(1, 0).UTC(), time.Unix(1, 0).UTC()})
}

// instants builds n distinct timestamps, all in one zone, the way a client
// sends them.
func instants(n int, loc *time.Location) []time.Time {
	out := make([]time.Time, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	for i := range out {
		out[i] = base.Add(time.Duration(i) * time.Second)
	}
	return out
}

// addresses builds n distinct IPv4 addresses.
func addresses(n int) []netip.Addr {
	out := make([]netip.Addr, n)
	for i := range out {
		out[i] = netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
	}
	return out
}

// Not parallel, like every test that calls assertLinear.
func TestUniqueIsLinearForTimesAndAddresses(t *testing.T) {
	someTimes, timeRules := instants(uniqueSize, time.UTC), Slice[time.Time]().Unique()
	assertLinear(t, "times", someTimes, func() { _ = timeRules.Check(someTimes) })
	someAddrs, addrRules := addresses(uniqueSize), Slice[netip.Addr]().Unique()
	assertLinear(t, "addresses", someAddrs, func() { _ = addrRules.Check(someAddrs) })

	times := instants(200_000, time.UTC)
	if err := Slice[time.Time]().Unique().Check(times); err != nil {
		t.Errorf("distinct times gave %v", err)
	}
	times[len(times)-1] = times[3]
	if err := Slice[time.Time]().Unique().Check(times); err == nil {
		t.Error("a late repeated time was accepted")
	}
	addrs := addresses(200_000)
	if err := Slice[netip.Addr]().Unique().Check(addrs); err != nil {
		t.Errorf("distinct addresses gave %v", err)
	}
	addrs[len(addrs)-1] = addrs[7]
	if err := Slice[netip.Addr]().Unique().Check(addrs); err == nil {
		t.Error("a late repeated address was accepted")
	}
}

// Two times are the same element when they are the same instant, whatever
// zone they are written in, which is what time.Time.Equal says and what a
// client that sends 12:00Z and 13:00+01:00 means by repeating itself.
func TestUniqueComparesTimesByInstant(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("", 3600)
	noon := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	if index, found := firstRepeat([]time.Time{noon, noon.Add(time.Hour), noon.In(zone)}); !found || index != 0 {
		t.Errorf("the same instant in another zone: firstRepeat = (%d, %v), want (0, true)", index, found)
	}
	if _, found := firstRepeat([]time.Time{noon, noon.Add(time.Nanosecond)}); found {
		t.Error("instants a nanosecond apart were reported as one")
	}
	if _, found := firstRepeat([]time.Time{{}, {}}); !found {
		t.Error("two zero times were not reported as a repeat")
	}
	// A reading of the monotonic clock is not part of the instant.
	now := time.Now()
	if _, found := firstRepeat([]time.Time{now, now.Round(0)}); !found {
		t.Error("a time and its wall-clock reading were reported as distinct")
	}
	// Far outside the range UnixNano can represent.
	far := time.Date(9999, 12, 31, 0, 0, 0, 1, time.UTC)
	if _, found := firstRepeat([]time.Time{far, far.Add(time.Nanosecond), far}); !found {
		t.Error("a repeated time beyond the year 2262 was missed")
	}
	if _, found := firstRepeat([]time.Time{far, far.Add(time.Nanosecond)}); found {
		t.Error("distinct times beyond the year 2262 were reported as one")
	}
}

func TestHashable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ  reflect.Type
		want bool
	}{
		{reflect.TypeFor[string](), true},
		{reflect.TypeFor[bool](), true},
		{reflect.TypeFor[uintptr](), true},
		{reflect.TypeFor[complex64](), true},
		{reflect.TypeFor[[16]byte](), true},
		{reflect.TypeFor[flatPoint](), true},
		{reflect.TypeFor[struct{}](), true},
		{reflect.TypeFor[*int](), false},
		{reflect.TypeFor[any](), false},
		{reflect.TypeFor[[]int](), false},
		{reflect.TypeFor[map[string]int](), false},
		{reflect.TypeFor[[2]*int](), false},
		{reflect.TypeFor[pointerPoint](), false},
		{reflect.TypeFor[blankPoint](), false},
		{reflect.TypeFor[time.Time](), false},
		{reflect.TypeFor[chan int](), false},
	} {
		if got := hashable(tc.typ); got != tc.want {
			t.Errorf("hashable(%s) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

// uniqueDeadline is far above what a search bounded to a hundred elements
// needs even under the race detector, and far below what the pairwise one
// would take over the twenty thousand elements it is given below.
const uniqueDeadline = 5 * time.Second

// uniqueSize is how many elements the cost of Unique is measured over. Keying
// each of them is twenty thousand steps, and comparing every pair two hundred
// million.
const uniqueSize = 20_000

// maxOverhead is how many times as long as a yardstick pass over the same
// elements Unique may take. Keying an element costs about what formatting it
// does, give or take a few times, and comparing every pair of twenty thousand
// costs thousands of times as much.
const maxOverhead = 100

// assertLinear fails the test unless unique, a Unique check over list, costs
// about what one pass over the elements of list does.
//
// The yardstick formats every element and indexes it in a map, work done once
// for each element and of the same kind as keying it, and it is timed beside
// unique on the same machine at the same moment. A deadline in its place,
// generous enough for the race detector, was still crossed on a loaded
// machine; timing two sizes of input and comparing them was thrown off by the
// larger falling out of a cache the load was sharing. Over the same elements
// both are slowed alike, and only work that grows faster than the input can
// open a gap of a hundred times between them.
//
// A test that calls it does not run in parallel, so that what it times is
// its own work and not that of the tests alongside it.
func assertLinear[E any](t *testing.T, name string, list []E, unique func()) {
	t.Helper()
	yardstick := func() {
		index := make(map[string]int, len(list))
		for i, e := range list {
			index[fmt.Sprint(e)] = i
		}
	}
	if overhead := costRatio(yardstick, unique, maxOverhead); overhead > maxOverhead {
		t.Fatalf("%s: Unique over %d elements took %.0f times as long as formatting each of them once, want at most %d",
			name, len(list), overhead, maxOverhead)
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

// distinctInts and distinctStrings build n distinct elements of each kind.
func distinctInts(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func distinctStrings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(i)
	}
	return out
}

// Not parallel, like every test that calls assertLinear.
func TestUniqueIsLinearForHashableElements(t *testing.T) {
	someInts, intRules := distinctInts(uniqueSize), Slice[int]().Unique()
	assertLinear(t, "ints", someInts, func() { _ = intRules.For(&someInts).Evaluate() })
	someStrs, strRules := distinctStrings(uniqueSize), Slice[string]().Unique()
	assertLinear(t, "strings", someStrs, func() { _ = strRules.Check(someStrs) })

	ints, strs := distinctInts(200_000), distinctStrings(200_000)
	if problems := Slice[int]().Unique().For(&ints).Evaluate(); len(problems) != 0 {
		t.Errorf("distinct ints gave %v", problems)
	}
	if err := Slice[string]().Unique().Check(strs); err != nil {
		t.Errorf("distinct strings gave %v", err)
	}
	strs[len(strs)-1] = "3"
	if err := Slice[string]().Unique().Check(strs); err == nil || err.Error() != "must not repeat 3" {
		t.Errorf("a late repeat gave %v, want must not repeat 3", err)
	}
}

func TestUniqueDefersToACountBound(t *testing.T) {
	t.Parallel()
	// Elements that cannot be hashed, so Unique would compare them pair by
	// pair: twenty thousand of them is two hundred million comparisons.
	values := make([][]int, 20_000)
	for i := range values {
		values[i] = []int{i}
	}

	start := time.Now()
	for _, tc := range []struct {
		name  string
		rules *SliceRules[[]int]
		want  string
	}{
		{"max declared after", Slice[[]int]().Unique().MaxItems(100), "must have at most 100 items"},
		{"max declared before", Slice[[]int]().MaxItems(100).Unique(), "must have at most 100 items"},
		{"exact count", Slice[[]int]().Unique().Items(3), "must have exactly 3 items"},
	} {
		problems := tc.rules.For(&values).Evaluate()
		if len(problems) != 1 || problems[0].Issue != tc.want || problems[0].Kind == KindTaken {
			t.Errorf("%s: problems = %+v, want %q", tc.name, problems, tc.want)
		}
		if err := tc.rules.Check(values); err == nil || err.Error() != tc.want {
			t.Errorf("%s: Check = %v, want %q", tc.name, err, tc.want)
		}
	}
	if elapsed := time.Since(start); elapsed > uniqueDeadline {
		t.Errorf("a bounded Unique over 20000 elements took %v", elapsed)
	}
}

func TestUniqueStillRunsWithinTheBound(t *testing.T) {
	t.Parallel()
	repeated := [][]int{{1}, {2}, {1}}
	for _, rules := range []*SliceRules[[]int]{
		Slice[[]int]().Unique().MaxItems(100),
		Slice[[]int]().MaxItems(3).Unique(),
		Slice[[]int]().Unique().Items(5),
	} {
		if err := rules.Check(repeated); err == nil || err.Error() != "must not repeat [1]" {
			t.Errorf("Check = %v, want must not repeat [1]", err)
		}
	}

	// A lower bound costs nothing to exceed, so it does not change which
	// failure is reported: declaration order decides, as it always did.
	if err := Slice[string]().Unique().MinItems(5).Check([]string{"a", "a"}); err == nil || err.Error() != "must not repeat a" {
		t.Errorf("Check = %v, want must not repeat a", err)
	}
	if err := Slice[string]().MinItems(5).Unique().Check([]string{"a", "a"}); err == nil || err.Error() != "must have at least 5 items" {
		t.Errorf("Check = %v, want must have at least 5 items", err)
	}
}
