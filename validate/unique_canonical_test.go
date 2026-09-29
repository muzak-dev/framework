package validate

import (
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
)

type withSlice struct {
	Name string
	Tags []string
}

type withMap struct {
	Labels map[string]int
}

type withInterface struct {
	Value any
}

type withFunc struct {
	Name string
	Run  func()
}

type withTime struct {
	At *time.Time
}

type node struct {
	Next *node
	N    int
}

// Elements that cannot be hashed the way DeepEqual compares them used to be
// searched pair by pair, and twenty thousand of them, which fits in a few
// hundred kilobytes of JSON, is two hundred million comparisons. Every one of
// them is now keyed by a canonical encoding of its value, so the work follows
// the size of the body and a MaxItems bound is no longer the only thing between
// a client and the CPU.
func TestUniqueIsLinearForElementsThatCannotBeHashed(t *testing.T) {
	t.Parallel()
	const n = 20_000

	ptrs := make([]*int, n)
	anys := make([]any, n)
	nested := make([][]string, n)
	structs := make([]withSlice, n)
	maps := make([]map[string]int, n)
	ifaces := make([]withInterface, n)
	pointed := make([]*withSlice, n)
	for i := range n {
		v := i
		ptrs[i] = &v
		anys[i] = strconv.Itoa(i)
		nested[i] = []string{"a", strconv.Itoa(i)}
		structs[i] = withSlice{Name: "x", Tags: []string{strconv.Itoa(i)}}
		maps[i] = map[string]int{"k": i, "j": -i}
		ifaces[i] = withInterface{Value: []int{i}}
		pointed[i] = &withSlice{Tags: []string{strconv.Itoa(i)}}
	}

	// check runs distinct and then repeated input against one rule set, and
	// bounds the time both take together.
	type checker func() (distinct, repeated error)
	cases := map[string]checker{
		"pointers": func() (error, error) {
			r := Slice[*int]().Unique()
			a := r.Check(ptrs)
			ptrs[n-1] = ptrs[3]
			return a, r.Check(ptrs)
		},
		"interfaces": func() (error, error) {
			r := Slice[any]().Unique()
			a := r.Check(anys)
			anys[n-1] = anys[3]
			return a, r.Check(anys)
		},
		"nested slices": func() (error, error) {
			r := Slice[[]string]().Unique()
			a := r.Check(nested)
			nested[n-1] = []string{"a", "3"}
			return a, r.Check(nested)
		},
		"structs holding a slice": func() (error, error) {
			r := Slice[withSlice]().Unique()
			a := r.Check(structs)
			structs[n-1] = withSlice{Name: "x", Tags: []string{"3"}}
			return a, r.Check(structs)
		},
		"maps": func() (error, error) {
			r := Slice[map[string]int]().Unique()
			a := r.Check(maps)
			maps[n-1] = map[string]int{"j": -3, "k": 3}
			return a, r.Check(maps)
		},
		"structs holding an interface": func() (error, error) {
			r := Slice[withInterface]().Unique()
			a := r.Check(ifaces)
			ifaces[n-1] = withInterface{Value: []int{3}}
			return a, r.Check(ifaces)
		},
		"pointers to structs": func() (error, error) {
			r := Slice[*withSlice]().Unique()
			a := r.Check(pointed)
			pointed[n-1] = &withSlice{Tags: []string{"3"}}
			return a, r.Check(pointed)
		},
	}

	for name, run := range cases {
		start := time.Now()
		distinct, repeated := run()
		elapsed := time.Since(start)
		if distinct != nil {
			t.Errorf("%s: distinct elements gave %v", name, distinct)
		}
		if repeated == nil {
			t.Errorf("%s: a late repeat was accepted", name)
		}
		if elapsed > uniqueDeadline {
			t.Errorf("%s: Unique over %d elements took %v", name, n, elapsed)
		}
	}
}

// The cost of a collection has to follow its size in memory as well as in
// time, or the key itself becomes the lever.
func TestUniqueAllocatesInProportionToTheCollection(t *testing.T) {
	values := make([][]string, 10_000)
	for i := range values {
		values[i] = []string{strconv.Itoa(i)}
	}
	rules := Slice[[]string]().Unique()
	allocs := testing.AllocsPerRun(3, func() {
		if err := rules.Check(values); err != nil {
			t.Fatal(err)
		}
	})
	// A handful per element for the key and the map entry; a pairwise search
	// allocates almost nothing, so this is a guard against per-comparison
	// garbage, which a quadratic implementation of the key would produce.
	if limit := float64(len(values)) * 8; allocs > limit {
		t.Errorf("Unique made %.0f allocations for %d elements, want at most %.0f", allocs, len(values), limit)
	}
}

// The canonical key must call two elements the same exactly when
// reflect.DeepEqual does, and pick out the same earliest element.
func TestCanonicalKeyAgreesWithDeepEqual(t *testing.T) {
	t.Parallel()
	one, otherOne, two := 1, 1, 2
	var nilPtr *int
	nan := math.NaN()
	at := time.Unix(5, 0).UTC()
	sameAt := time.Unix(5, 0).In(time.FixedZone("", 3600))
	cycleA := &node{N: 1}
	cycleA.Next = cycleA
	cycleB := &node{N: 1}
	cycleB.Next = cycleB

	agreesWithPairwise(t, "pointers", []*int{&one, &two, nilPtr, &otherOne, nilPtr})
	agreesWithPairwise(t, "pointers to pointers", []**int{ptrTo(&one), ptrTo(&otherOne)})
	agreesWithPairwise(t, "any of mixed dynamic types", []any{1, int64(1), "1", 1.0, nil, nil, []int{1}, []int{1}})
	agreesWithPairwise(t, "any of equal numbers of different types", []any{int8(1), int16(1), uint8(1)})
	agreesWithPairwise(t, "nil slice is not an empty one", [][]int{nil, {}, nil})
	agreesWithPairwise(t, "empty slices", [][]int{{}, {}})
	agreesWithPairwise(t, "nested slices", [][][]string{{{"a"}, {"b"}}, {{"a", "b"}}, {{"a"}, {"b"}}})
	agreesWithPairwise(t, "boundaries between strings", [][]string{{"ab", "c"}, {"a", "bc"}, {"ab", "c"}})
	agreesWithPairwise(t, "maps", []map[string]int{{"a": 1, "b": 2}, {"b": 2, "a": 1}, {"a": 1}})
	agreesWithPairwise(t, "nil map is not an empty one", []map[string]int{nil, {}, {}})
	agreesWithPairwise(t, "structs holding a slice", []withSlice{{"a", []string{"x"}}, {"a", []string{"y"}}, {"a", []string{"x"}}})
	agreesWithPairwise(t, "structs holding a map", []withMap{{map[string]int{"a": 1}}, {map[string]int{"a": 1}}})
	agreesWithPairwise(t, "structs holding an interface", []withInterface{{1}, {int64(1)}, {1}})
	agreesWithPairwise(t, "NaN equals nothing", []any{nan, nan})
	agreesWithPairwise(t, "NaN inside a slice", [][]float64{{nan}, {nan}})
	agreesWithPairwise(t, "signed zeros in a slice", [][]float64{{0}, {math.Copysign(0, -1)}})
	agreesWithPairwise(t, "arrays of slices", [][2][]int{{{1}, {2}}, {{1}, {2}}})
	agreesWithPairwise(t, "earliest first occurrence", [][]int{{1}, {2}, {2}, {1}})
	agreesWithPairwise(t, "funcs", []withFunc{{Name: "a"}, {Name: "a"}})
	agreesWithPairwise(t, "cyclic structures", []*node{cycleA, cycleB})
	agreesWithPairwise(t, "no elements", [][]int{})

	// A time inside a value is the instant it names, as a time on its own is.
	if _, found := firstRepeat([]withTime{{&at}, {&sameAt}}); !found {
		t.Error("the same instant in another zone, held by a pointer in a struct, was reported as distinct")
	}
	later := at.Add(time.Nanosecond)
	if _, found := firstRepeat([]withTime{{&at}, {&later}}); found {
		t.Error("instants a nanosecond apart, held by a pointer in a struct, were reported as one")
	}
	if _, found := firstRepeat([]withTime{{nil}, {&at}, {nil}}); !found {
		t.Error("two nil times were reported as distinct")
	}
}

func ptrTo(p *int) **int { return &p }

// A func is equal to nothing but a nil func, and a channel to itself, as
// DeepEqual and == have it. None of them can come out of a request body, but a
// rule set is also handed values built in code.
func TestCanonicalKeyHandlesKindsAMapCannotKey(t *testing.T) {
	t.Parallel()
	run := func() {}
	if _, found := firstRepeat([]withFunc{{"a", run}, {"a", run}}); found {
		t.Error("two non-nil funcs were reported as a repeat")
	}
	if _, found := firstRepeat([]withFunc{{"a", nil}, {"a", nil}}); !found {
		t.Error("two nil funcs were reported as distinct")
	}
	ch := make(chan int)
	if _, found := firstRepeat([]struct{ C chan int }{{ch}, {ch}}); !found {
		t.Error("one channel held twice was reported as distinct")
	}
	if _, found := firstRepeat([]struct{ C chan int }{{ch}, {make(chan int)}}); found {
		t.Error("two channels were reported as one")
	}
}

// randomValue builds a small value out of the kinds the encoding has to tell
// apart, from an alphabet small enough that repeats are common.
func randomValue(r *rand.Rand, depth int) any {
	kinds := 9
	if depth == 0 {
		kinds = 5
	}
	switch r.IntN(kinds) {
	case 0:
		return nil
	case 1:
		return r.IntN(3)
	case 2:
		return int64(r.IntN(3))
	case 3:
		return string(rune('a' + r.IntN(3)))
	case 4:
		return []float64{0, math.Copysign(0, -1), 1.5, math.NaN()}[r.IntN(4)]
	case 5:
		n := r.IntN(3)
		list := make([]any, n)
		for i := range list {
			list[i] = randomValue(r, depth-1)
		}
		if r.IntN(4) == 0 {
			return []any(nil)
		}
		return list
	case 6:
		m := map[string]any{}
		for range r.IntN(3) {
			m[string(rune('a'+r.IntN(3)))] = randomValue(r, depth-1)
		}
		if r.IntN(4) == 0 {
			return map[string]any(nil)
		}
		return m
	case 7:
		v := randomValue(r, depth-1)
		return &v
	default:
		return withInterface{randomValue(r, depth-1)}
	}
}

// Against the search Unique used to run, on values built to collide, the key
// picks out the same element every time.
func TestCanonicalKeyMatchesDeepEqualOnRandomValues(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(7, 11))
	for round := range 3000 {
		list := make([]any, 2+r.IntN(6))
		for i := range list {
			list[i] = randomValue(r, 3)
		}
		agreesWithPairwise(t, "random round "+strconv.Itoa(round), list)
		if t.Failed() {
			t.Logf("input: %#v", list)
			return
		}
	}
}
