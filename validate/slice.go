package validate

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// SliceRules collects the checks applied to a slice field.
//
// Rules about the collection itself, such as how many elements it may hold, sit
// alongside rules about each element, which [SliceRules.Each] supplies:
//
//	v.Slice(&in.Tags).MaxItems(10).Each(validate.String().MaxLen(20))
//
// A failure inside an element is reported against that position, as
// "tags[2]", so a client can tell which one to fix.
type SliceRules[E any] struct {
	target   *[]E
	label    string
	required bool
	steps    []step[[]E]
	element  ElementRules[E]
}

// Slice returns an unbound rule set for a collection of E.
func Slice[E any]() *SliceRules[E] { return &SliceRules[E]{} }

// For binds the rule set to a field, identified by its address. See
// [StringRules.For].
func (r *SliceRules[E]) For(target *[]E) *SliceRules[E] {
	r.target = target
	return r
}

// Reset returns the rule set to its unbound, ruleless state while keeping the
// memory it has already claimed. See [StringRules.Reset].
func (r *SliceRules[E]) Reset() {
	r.target = nil
	r.label = ""
	r.required = false
	clear(r.steps)
	r.steps = r.steps[:0]
	r.element = nil
}

// Target implements [Evaluator].
//
// An unbound rule set reports no target at all rather than a typed nil pointer,
// so a caller can test the result against nil and get the answer it expects.
func (r *SliceRules[E]) Target() any {
	if r.target == nil {
		return nil
	}
	return r.target
}

// Label implements [Evaluator].
func (r *SliceRules[E]) Label() string { return r.label }

// Describe implements [Evaluator].
func (r *SliceRules[E]) Describe() Constraints {
	c := describeAll(r.steps)
	c.Required = r.required
	return c
}

// DescribeElement reports the constraints on each element, so the generated
// document can describe the items of an array as well as the array itself. It
// returns the zero Constraints when no element rules were declared.
func (r *SliceRules[E]) DescribeElement() Constraints {
	if r.element == nil {
		return Constraints{}
	}
	return r.element.describe()
}

// Evaluate implements [Evaluator].
//
// It reports every element that fails, so the length of what it returns is
// chosen by whoever chose the length of the collection. Muzak itself evaluates
// through [SliceRules.EvaluateUpTo], and a caller running rule sets against a
// client's input on its own should do the same.
func (r *SliceRules[E]) Evaluate() []Problem {
	return r.evaluate(0)
}

// EvaluateUpTo is [SliceRules.Evaluate] with a ceiling. It stops at the
// limit'th problem and leaves the remaining elements unchecked, so the work as
// well as the report is bounded by what the caller is prepared to send back
// rather than by how many elements a client sent: a megabyte of empty strings
// is a third of a million failures under Each(String().Required()), and
// without the ceiling every one of them is found, described and returned.
//
// A limit below one is treated as one. Asking for one more problem than will
// be reported is how a caller learns that there was more to say without paying
// to find out how much.
func (r *SliceRules[E]) EvaluateUpTo(limit int) []Problem {
	return r.evaluate(max(limit, 1))
}

// evaluate is the body of both, with a limit of zero meaning none.
func (r *SliceRules[E]) evaluate(limit int) []Problem {
	if r.target == nil {
		// coverage: the entry point always binds a non-nil field pointer.
		return nil
	}
	values := *r.target
	if problems := runSlice(&values, r.steps); len(problems) > 0 {
		return problems
	}
	if r.element == nil {
		return nil
	}

	// Elements are validated in place, so a transform declared on the element
	// rules reaches the slice the handler receives.
	var problems []Problem
	for i := range values {
		for _, problem := range r.element.applyTo(&values[i]) {
			problem.Path = "[" + strconv.Itoa(i) + "]" + problem.Path
			problems = append(problems, problem)
			if len(problems) == limit {
				return problems
			}
		}
	}
	return problems
}

// runSlice is the collection counterpart to [run], written out for the one
// thing a collection needs that no other family does: [SliceRules.Unique]
// defers to a count bound, whatever order the two were declared in.
//
// Checks otherwise run in the order written and stop at the first failure,
// and for most rules that order is only a matter of which message a client
// reads first. For Unique it is a matter of cost. Its comparison is linear for
// elements that can be hashed, but a collection of elements that cannot, such
// as structs holding pointers or slices, is compared pair by pair, and a
// quarter of a megabyte of JSON is enough elements to spend seconds of CPU on.
// Unique().MaxItems(100) reads as bounded, so it has to be: a collection over
// a MaxItems or Items bound is never searched for repeats, and the bound
// reports it instead when its turn comes.
func runSlice[E any](values *[]E, steps []step[[]E]) []Problem {
	for i := range steps {
		s := &steps[i]
		if !runsOnEmpty(s.kind) && len(*values) == 0 {
			// An optional collection that was not supplied has nothing to
			// check, and a required one has already been reported.
			continue
		}
		if s.kind == kindUnique && exceedsCountBound(steps, len(*values)) {
			continue
		}
		if err := applySliceStep(s, values); err != nil {
			return []Problem{problemFor(s, err)}
		}
	}
	return nil
}

// exceedsCountBound reports whether a collection of n elements is longer than
// a MaxItems or Items rule in the set allows.
func exceedsCountBound[E any](steps []step[[]E], n int) bool {
	for i := range steps {
		switch steps[i].kind {
		case kindMaxItems, kindItems:
			if n > steps[i].n {
				return true
			}
		}
	}
	return false
}

// add appends a step and returns the rule set for chaining.
func (r *SliceRules[E]) add(s step[[]E]) *SliceRules[E] {
	r.steps = append(r.steps, s)
	return r
}

// As renames the field in the messages this rule set produces.
func (r *SliceRules[E]) As(name string) *SliceRules[E] {
	r.label = name
	return r
}

// Message overrides the wording of the check written immediately before it.
func (r *SliceRules[E]) Message(message string) *SliceRules[E] {
	setMessage(r.steps, message)
	return r
}

// MessageKey overrides the wording of the check written immediately before it
// with a translation key, so that an override is translated like every built-in
// rule rather than fixed in one language.
//
//	v.String(&in.Password).MinLen(12).MessageKey("errors.password.too_short")
//
// Arguments are alternating names and values, and are interpolated alongside
// the ones the rule supplies itself, so the key may use %{count} exactly as
// the built-in message does. The rule's English wording still reaches
// [Problem.Issue], so an application that does not translate reads the same
// sentence it always did.
//
// Use [SliceRules.Message] instead when the wording is fixed in one language on
// purpose.
func (r *SliceRules[E]) MessageKey(key string, args ...any) *SliceRules[E] {
	setMessageKey(r.steps, key, args)
	return r
}

// Required rejects an empty or absent collection.
func (r *SliceRules[E]) Required() *SliceRules[E] {
	r.required = true
	return r.add(step[[]E]{kind: kindRequired})
}

// MinItems requires at least n elements.
func (r *SliceRules[E]) MinItems(n int) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindMinItems, n: n})
}

// MaxItems requires at most n elements.
func (r *SliceRules[E]) MaxItems(n int) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindMaxItems, n: n})
}

// Unique requires every element to differ from every other.
//
// Elements are compared the way reflect.DeepEqual compares them, so a slice of
// structs is deduplicated by content rather than by identity. The exception is
// time.Time, whose elements are the same when they are the same instant, as
// time.Time.Equal has it, in whatever zone they were written. The failure
// names the earliest element that appears again later.
//
// The cost depends on the element type. Strings, numbers, booleans, arrays
// and structs built only from those, times and netip addresses are hashed, so
// a collection of any length is checked in linear time. Anything else, a
// struct holding a pointer, a slice, a map or an interface, cannot be hashed
// the way DeepEqual compares it and is compared pair by pair, which is quadratic: a few hundred kilobytes
// of JSON is enough elements to cost seconds. Declare [SliceRules.MaxItems]
// alongside Unique for such a collection. Unique defers to MaxItems and
// [SliceRules.Items] in either order of declaration: a collection over the
// bound is reported as too long and never searched for repeats.
func (r *SliceRules[E]) Unique() *SliceRules[E] {
	return r.add(step[[]E]{kind: kindUnique})
}

// Each applies a rule set to every element.
//
// Only a rule set for the element's own type satisfies the parameter, so
// Each(validate.String()) on a slice of integers does not compile. Use
// [Value] for element types the typed rule sets do not cover:
//
//	v.Slice(&in.Scores).Each(validate.Value[int]().Must(positive))
func (r *SliceRules[E]) Each(rules ElementRules[E]) *SliceRules[E] {
	r.element = rules
	return r
}

// Items requires an exact number of elements.
//
// It is the collection counterpart of [StringRules.Len], for a field whose
// length is fixed rather than bounded: a pair of coordinates, a set of answers
// to a fixed set of questions.
func (r *SliceRules[E]) Items(n int) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindItems, n: n})
}

// NotEmpty requires at least one element.
//
// Like [StringRules.NotBlank], and unlike almost every other rule, it runs even
// when the field was not supplied, because an absent collection is empty too.
//
// It therefore differs from Required by wording rather than by effect, and the
// wording is the point: a client told that a list "must not be empty" knows
// what to do, where one told it "is required" will go looking for whether it
// sent the field at all.
func (r *SliceRules[E]) NotEmpty() *SliceRules[E] {
	return r.add(step[[]E]{kind: kindNotEmpty})
}

// Contains requires an element to be present.
//
//	v.Slice(&in.Scopes).Contains("read")
//
// Elements are compared the way [SliceRules.Unique] compares them, so a
// collection of structs works as well as one of strings.
func (r *SliceRules[E]) Contains(wanted E) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindContainsItem, enum: []any{wanted}})
}

// Excludes requires an element to be absent, for the value that is legal
// everywhere else but not here: a wildcard scope, a reserved tag.
func (r *SliceRules[E]) Excludes(rejected E) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindExcludesItem, enum: []any{rejected}})
}

// Must applies a rule of your own to the collection as a whole.
func (r *SliceRules[E]) Must(check func([]E) error) *SliceRules[E] {
	return r.add(step[[]E]{kind: kindCustom, check: check})
}

// Check applies the rule set to a value and returns the first failure.
func (r *SliceRules[E]) Check(values []E) error {
	problems := runSlice(&values, r.steps)
	if len(problems) == 0 && r.element != nil {
		for i := range values {
			if elementProblems := r.element.applyTo(&values[i]); len(elementProblems) > 0 {
				return fmt.Errorf("item %d %s", i+1, elementProblems[0].Issue)
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(problems[0].Issue)
}

// joinWithOr renders a list of alternatives as prose.
func joinWithOr(values []string) string {
	switch len(values) {
	case 0:
		return "no permitted value"
	case 1:
		return values[0]
	default:
		return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
	}
}

// applySliceStep runs one collection rule. See [applyStringStep] for why the
// rules are dispatched rather than closed over.
func applySliceStep[E any](s *step[[]E], values *[]E) error {
	switch s.kind {
	case kindRequired:
		if len(*values) == 0 {
			return errRequired
		}
	case kindMinItems:
		if len(*values) < s.n {
			return fmt.Errorf("must have at least %d %s", s.n, plural(s.n, "item"))
		}
	case kindMaxItems:
		if len(*values) > s.n {
			return fmt.Errorf("must have at most %d %s", s.n, plural(s.n, "item"))
		}
	case kindUnique:
		list := *values
		if i, repeated := firstRepeat(list); repeated {
			return element{
				text:  fmt.Sprintf("must not repeat %v", list[i]),
				value: fmt.Sprintf("%v", list[i]),
			}
		}
	case kindItems:
		if len(*values) != s.n {
			return fmt.Errorf("must have exactly %d %s", s.n, plural(s.n, "item"))
		}
	case kindNotEmpty:
		if len(*values) == 0 {
			return errors.New("must not be empty")
		}
	case kindContainsItem:
		if !holdsElement(*values, s.enum) {
			return element{
				text:  fmt.Sprintf("must contain %v", s.enum[0]),
				value: fmt.Sprintf("%v", s.enum[0]),
			}
		}
	case kindExcludesItem:
		if holdsElement(*values, s.enum) {
			return element{
				text:  fmt.Sprintf("must not contain %v", s.enum[0]),
				value: fmt.Sprintf("%v", s.enum[0]),
			}
		}
	default:
		return s.check(*values)
	}
	return nil
}

// holdsElement reports whether a collection contains the wanted element.
func holdsElement[E any](values []E, wanted []any) bool {
	if len(wanted) == 0 {
		// coverage: Contains and Excludes each record exactly one element, so
		// the set is never empty by the time it is searched.
		return false
	}
	for i := range values {
		if reflect.DeepEqual(any(values[i]), wanted[0]) {
			return true
		}
	}
	return false
}

// firstRepeat returns the index of the earliest element that appears again
// later in the collection, which is the element Unique reports.
//
// The pairwise search this replaces found the same element, and the hashed
// search is written to: a repeat found late may belong to an element earlier
// than one found sooner, as in [a b b a], so the scan keeps the lowest first
// occurrence it meets rather than stopping at the first repeat.
func firstRepeat[E any](list []E) (int, bool) {
	if len(list) < 2 {
		return 0, false
	}
	key := keyFor[E]()
	if key == nil {
		for i := range list {
			for j := i + 1; j < len(list); j++ {
				if reflect.DeepEqual(list[i], list[j]) {
					return i, true
				}
			}
		}
		return 0, false
	}
	seen := make(map[any]int, len(list))
	first := -1
	for j := range list {
		k := key(list[j])
		if i, found := seen[k]; found {
			if first < 0 || i < first {
				first = i
			}
			continue
		}
		seen[k] = j
	}
	return first, first >= 0
}

// instant identifies a point in time and nothing else about a time.Time.
type instant struct {
	sec  int64
	nsec int
}

// keyFor returns a function that maps an element to a map key which is equal
// for two elements exactly when [SliceRules.Unique] counts them as the same,
// or nil when the type has no such key and elements have to be compared pair
// by pair.
//
// Two element types are common enough, and slow enough under the pairwise
// search, to be given a key of their own. A time.Time holds a *Location, so
// it is not [hashable], yet it is what a list of timestamps is made of, and
// the pairwise search cost a quarter of a megabyte of JSON twelve seconds of
// CPU. Two times are one element when they are the same instant, the way
// time.Time.Equal compares them: the zone a client wrote the timestamp in and
// a reading of the monotonic clock are not part of what it says, and
// 12:00Z and 13:00+01:00 are a repeat, where DeepEqual, comparing the zone
// pointers, once called them distinct. The key is the seconds and nanoseconds
// since the epoch rather than UnixNano, which cannot represent a year past
// 2262. A netip.Addr, or an address with a port or a prefix, is comparable
// and made only of values the standard library interns, so == on it and
// DeepEqual on it are the same function and it is used as its own key.
func keyFor[E any]() func(E) any {
	t := reflect.TypeFor[E]()
	switch t {
	case reflect.TypeFor[time.Time]():
		return func(e E) any {
			at := any(e).(time.Time)
			return instant{at.Unix(), at.Nanosecond()}
		}
	case reflect.TypeFor[netip.Addr](), reflect.TypeFor[netip.AddrPort](), reflect.TypeFor[netip.Prefix]():
		return func(e E) any { return any(e) }
	}
	if hashable(t) {
		return func(e E) any { return any(e) }
	}
	return nil
}

// hashable reports whether values of a type can be told apart with a map
// lookup and get the answer reflect.DeepEqual would give.
//
// Being comparable is not enough. == on a pointer compares addresses where
// DeepEqual compares what they point at, an interface compares its dynamic
// value the same way, and == on a struct ignores blank fields that DeepEqual
// reads. What is left is the types whose == and DeepEqual are the same
// function: the basic kinds and arrays and structs made only of them. NaN is
// consistent too, since it equals nothing under either.
func hashable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	case reflect.Array:
		return hashable(t.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			field := t.Field(i)
			if field.Name == "_" || !hashable(field.Type) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
