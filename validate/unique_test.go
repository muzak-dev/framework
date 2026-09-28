package validate

import (
	"math"
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
