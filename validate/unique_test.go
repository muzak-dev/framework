package validate

import (
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

func TestUniqueIsLinearForTimesAndAddresses(t *testing.T) {
	t.Parallel()
	times := instants(200_000, time.UTC)
	addrs := make([]netip.Addr, 200_000)
	for i := range addrs {
		addrs[i] = netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
	}

	start := time.Now()
	if err := Slice[time.Time]().Unique().Check(times); err != nil {
		t.Errorf("distinct times gave %v", err)
	}
	times[len(times)-1] = times[3]
	if err := Slice[time.Time]().Unique().Check(times); err == nil {
		t.Error("a late repeated time was accepted")
	}
	if err := Slice[netip.Addr]().Unique().Check(addrs); err != nil {
		t.Errorf("distinct addresses gave %v", err)
	}
	addrs[len(addrs)-1] = addrs[7]
	if err := Slice[netip.Addr]().Unique().Check(addrs); err == nil {
		t.Error("a late repeated address was accepted")
	}
	if elapsed := time.Since(start); elapsed > uniqueDeadline {
		t.Errorf("Unique over 200000 times and addresses took %v", elapsed)
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

// uniqueDeadline is far above what the linear search needs even under the race
// detector, and far below what the pairwise one would: on the inputs below it
// runs for minutes.
const uniqueDeadline = 5 * time.Second

func TestUniqueIsLinearForHashableElements(t *testing.T) {
	t.Parallel()
	ints := make([]int, 200_000)
	strs := make([]string, 200_000)
	for i := range ints {
		ints[i] = i
		strs[i] = strconv.Itoa(i)
	}

	start := time.Now()
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
	if elapsed := time.Since(start); elapsed > uniqueDeadline {
		t.Errorf("Unique over 200000 elements took %v", elapsed)
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
