package muzak

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"muzak.dev/framework/validate"
)

// Validatable is implemented by an input model that declares validation rules.
//
// Muzak runs Validate after binding and after every guard, so a model never
// gets to tell an unauthenticated caller what is wrong with its request. The
// method belongs on the pointer type, which is what lets rules name fields by
// address:
//
//	func (in *CreateUser) Validate(v *muzak.Validation) {
//		v.String(&in.Email).Trim().Lower().Required().Email()
//		v.Number(&in.Age).Between(18, 120)
//	}
//
// Implementing it is the only thing needed: there is no option to remember and
// no pipe to install, so a model cannot be left unvalidated by omission. Use
// [SkipValidation] on a route that must not validate.
type Validatable interface {
	// Validate declares this model's rules. It is called once per request, on
	// the bound value, and should declare rules rather than do work of its own.
	Validate(v *Validation)
}

// StringField matches a string field or an optional pointer to one.
//
// The union is what lets one entry point serve both `Name string` and
// `Nickname *string`. Rules bind to the field's address either way, so the
// field is still identified by position, and a nil pointer skips its rules
// rather than failing them, except Required, which a nil pointer fails.
type StringField interface {
	~string | *string
}

// NumberField matches a numeric field or an optional pointer to one.
type NumberField interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64 |
		*int | *int8 | *int16 | *int32 | *int64 |
		*uint | *uint8 | *uint16 | *uint32 | *uint64 |
		*float32 | *float64
}

// TimeField matches a time field or an optional pointer to one.
type TimeField interface {
	time.Time | *time.Time
}

// Validation collects the rules a model declares and turns what they find into
// error details.
//
// A model receives one during its Validate method and uses it to bind rule sets
// to its own fields. It is not safe for concurrent use and must not be retained
// after Validate returns.
type Validation struct {
	plan *validationPlan
	base uintptr
	size uintptr
	// value is the model being validated. It is kept so that a nested model
	// reached through a pointer field can be traced back to the field holding
	// it, which its address alone cannot reveal.
	value    reflect.Value
	rules    []validate.Evaluator
	rejected []rejection

	// parent is the Validation of the model this one is nested in, and depth
	// how many models deep that is. A nested model's dotted path is not built
	// when it is nested but the first time one of its failures needs it, and
	// then kept in path, so a model that fails nothing costs no path at all
	// and one that does costs a single join with its parent's.
	parent    *Validation
	depth     int
	path      string
	pathKnown bool
	// lastElement is the position of the element of a collection a model
	// nested in this one was last found at, where the next search begins.
	lastElement int

	// run is the report every Validation of one request writes into, the
	// model's own and each nested one's alike, so that one ceiling covers
	// them all. It is nil while a model is only being described, which is
	// what tells Nested not to descend. state is where the outermost
	// Validation keeps it, so a pooled one brings its report along.
	run   *validationRun
	state validationRun
	// dry records the models nested while the route is compiled, for
	// checkRulesBindToFields to vet. It is nil on every Validation a request
	// uses.
	dry *dryRun
	// start is where this Validation's own failures belong in the report:
	// ahead of those of the models nested in it, which are evaluated first.
	start int

	// The free lists below hold the rule sets this Validation has already
	// built. A model declares the same shape on every request, so handing a
	// recycled rule set back and resetting it means redeclaring the rules
	// costs nothing after the first request through a route. Each list is
	// typed, so a recycled set is always the right kind; only which field it
	// binds to changes.
	freeStrings []*validate.StringRules
	freeNumbers []*validate.NumberRules
	freeTimes   []*validate.TimeRules
	usedStrings int
	usedNumbers int
	usedTimes   int
}

// reset returns a Validation to the pool's idea of empty, keeping the rule sets
// and slices it has already built.
//
// Everything that points into the request is cleared, not only truncated: a
// slice cut to length zero still holds what it held, and a pooled Validation
// that kept the last request's rule sets bound to its fields, or the models
// nested in it, would keep that request's body alive until the pool happened
// to drop it.
func (v *Validation) reset() {
	v.plan = nil
	v.base = 0
	v.size = 0
	v.value = reflect.Value{}
	clear(v.rules)
	v.rules = v.rules[:0]
	clear(v.rejected)
	v.rejected = v.rejected[:0]
	v.parent = nil
	v.depth = 0
	v.path, v.pathKnown = "", false
	v.lastElement = 0
	for _, rules := range v.freeStrings[:v.usedStrings] {
		rules.Reset()
	}
	for _, rules := range v.freeNumbers[:v.usedNumbers] {
		rules.Reset()
	}
	for _, rules := range v.freeTimes[:v.usedTimes] {
		rules.Reset()
	}
	v.usedStrings, v.usedNumbers, v.usedTimes = 0, 0, 0
	// The Validations kept for nested models are reset as each model
	// finishes, a panicking one included, so only the list is kept here.
	v.run = nil
	v.state = validationRun{nested: v.state.nested}
	v.start = 0
}

// MaxValidationDetails is the most failures the validation of one request
// reports.
//
// A rule over a collection reports each element that fails, and a model nested
// once per element does the same, so without a ceiling the length of the report
// is chosen by the client: a megabyte of empty strings against
// Each(validate.String().Required()) was a 22 megabyte response and more than
// half a gigabyte of allocation to build it. A report that reaches the ceiling
// ends with one further detail, of kind "too_many_problems" and with an empty
// field, saying that more was found than is listed. Nothing is evaluated after
// that point, not the rest of a collection and not a model nested after it, so
// the ceiling bounds the work as well as the response.
//
// A hundred is far more than a person corrects in one pass, and a client that
// sends more mistakes than that learns about the rest by sending again.
const MaxValidationDetails = 100

// validationRun is the report of one request's validation.
type validationRun struct {
	out []ErrorDetail
	// failed names the fields that already failed to bind, whose validation
	// failures are left out because an unparseable value has nothing further
	// to say.
	failed map[string]bool
	// truncated records that a failure was found after the report was full,
	// and is what stops everything that would have been evaluated after it.
	truncated bool
	// nested holds one Validation per level of nesting, reused by every model
	// nested at that level. A model is validated completely before the next
	// one at its level begins, so one per level is all a request needs, and
	// a model nested once per element of a collection reuses the rule sets
	// the previous element built.
	nested []*Validation
}

// child hands out the Validation for a model nested at the given depth.
func (r *validationRun) child(depth int) *Validation {
	for len(r.nested) < depth {
		r.nested = append(r.nested, new(Validation))
	}
	return r.nested[depth-1]
}

// room is how many more failures the report takes.
func (r *validationRun) room() int {
	return MaxValidationDetails - len(r.out)
}

// add records one failure, or notes that there was no room for it.
func (r *validationRun) add(detail ErrorDetail) {
	switch {
	case r.truncated, r.failed[detail.Field]:
	case len(r.out) >= MaxValidationDetails:
		r.truncated = true
	default:
		r.out = append(r.out, detail)
	}
}

// report returns what was found, closed by the marker when there was more.
func (r *validationRun) report() []ErrorDetail {
	if !r.truncated {
		return r.out
	}
	return append(r.out, ErrorDetail{
		Location: "body",
		Issue: fmt.Sprintf("has more problems than the %d listed; correct these and send it again to see the rest",
			MaxValidationDetails),
		Kind: "too_many_problems",
		Args: []any{"count", MaxValidationDetails},
	})
}

// boundedEvaluator is a rule set that can stop partway, which is what
// [validate.SliceRules] offers for a collection a client chose the length of.
type boundedEvaluator interface {
	EvaluateUpTo(limit int) []validate.Problem
}

// nextString hands out a recycled string rule set, building one only the first
// time a model reaches that position.
func (v *Validation) nextString() *validate.StringRules {
	if v.usedStrings < len(v.freeStrings) {
		rules := v.freeStrings[v.usedStrings]
		rules.Reset()
		v.usedStrings++
		return rules
	}
	rules := validate.String()
	v.freeStrings = append(v.freeStrings, rules)
	v.usedStrings++
	return rules
}

// nextNumber hands out a recycled numeric rule set.
func (v *Validation) nextNumber() *validate.NumberRules {
	if v.usedNumbers < len(v.freeNumbers) {
		rules := v.freeNumbers[v.usedNumbers]
		rules.Reset()
		v.usedNumbers++
		return rules
	}
	rules := validate.Number()
	v.freeNumbers = append(v.freeNumbers, rules)
	v.usedNumbers++
	return rules
}

// nextTime hands out a recycled time rule set.
func (v *Validation) nextTime() *validate.TimeRules {
	if v.usedTimes < len(v.freeTimes) {
		rules := v.freeTimes[v.usedTimes]
		rules.Reset()
		v.usedTimes++
		return rules
	}
	rules := validate.Time()
	v.freeTimes = append(v.freeTimes, rules)
	v.usedTimes++
	return rules
}

// rejection is a failure recorded directly rather than through a rule set.
type rejection struct {
	target any
	issue  string
	// key names a translation to render the failure from, set by RejectKey.
	key string
	// args are the values that translation interpolates.
	args []any
}

// String binds rules to a string field.
//
//	v.String(&in.Email).Trim().Lower().Required().Email()
//
// The field may be a string or a pointer to one; a nil pointer skips its rules,
// except Required, which reports it missing. A field of any other type does not compile.
func (v *Validation) String[T StringField](ptr *T) *validate.StringRules {
	rules := v.nextString().For(ptr)
	v.rules = append(v.rules, rules)
	return rules
}

// Number binds rules to a numeric field.
//
//	v.Number(&in.Age).Between(18, 120)
//
// Bounds are written as ordinary constants whatever the field's own numeric
// type. A pointer field is optional.
func (v *Validation) Number[T NumberField](ptr *T) *validate.NumberRules {
	rules := v.nextNumber().For(ptr)
	v.rules = append(v.rules, rules)
	return rules
}

// Time binds rules to a time field.
func (v *Validation) Time[T TimeField](ptr *T) *validate.TimeRules {
	rules := v.nextTime().For(ptr)
	v.rules = append(v.rules, rules)
	return rules
}

// Slice binds rules to a collection, both about the collection itself and,
// through [validate.SliceRules.Each], about each element:
//
//	v.Slice(&in.Tags).MaxItems(10).Each(validate.String().MaxLen(20))
func (v *Validation) Slice[E any](ptr *[]E) *validate.SliceRules[E] {
	rules := validate.Slice[E]().For(ptr)
	v.rules = append(v.rules, rules)
	return rules
}

// Value binds rules to a field of any type, for what the typed rule sets do not
// cover. Must and OneOf take the field's own type, so the values written at the
// call site are checked by the compiler.
func (v *Validation) Value[T any](ptr *T) *validate.ValueRules[T] {
	rules := validate.Value[T]().For(ptr)
	v.rules = append(v.rules, rules)
	return rules
}

// Reject records a failure against a field without declaring a rule for it.
//
// It is the direct form of a cross-field check, for a condition that reads
// better as an ordinary if than as a rule:
//
//	if in.Start.After(in.End) {
//		v.Reject(&in.End, "must not be before the start")
//	}
func (v *Validation) Reject(target any, issue string) {
	v.reject(rejection{target: target, issue: issue})
}

// reject records a rejection, keeping no more than the report could use: a
// check written once per element of a collection rejects as often as the client
// sent elements, and past [MaxValidationDetails] and the one more that says the
// report is incomplete, another would only be discarded.
func (v *Validation) reject(r rejection) {
	if len(v.rejected) <= MaxValidationDetails {
		v.rejected = append(v.rejected, r)
	}
}

// RejectKey records a failure against a field and names a translation to render
// it from, so that a cross-field check reads in the caller's language the way
// every built-in rule does:
//
//	if in.Start.After(in.End) {
//		v.RejectKey(&in.End, "errors.booking.ends_before_it_starts")
//	}
//
// Arguments are alternating names and values, interpolated into the
// translation. When nothing translates the key, the key itself is reported,
// which names what has to be added and where.
func (v *Validation) RejectKey(target any, key string, args ...any) {
	v.reject(rejection{target: target, issue: key, key: key, args: args})
}

// Condition is a pending cross-field check produced by [Validation.When].
type Condition struct {
	validation *Validation
	holds      bool
}

// When begins a check that applies only when a condition holds:
//
//	v.When(in.Role == "admin" && in.Age < 21).
//		Reject(&in.Role, "an admin must be at least 21")
//
// The condition is an ordinary Go expression over the model's own fields, which
// is all a cross-field rule needs to be.
func (v *Validation) When(condition bool) *Condition {
	return &Condition{validation: v, holds: condition}
}

// Reject records the failure when the condition held, and does nothing
// otherwise. It returns the condition so that several failures can hang off one
// test.
func (c *Condition) Reject(target any, issue string) *Condition {
	if c.holds {
		c.validation.Reject(target, issue)
	}
	return c
}

// RejectKey records a translated failure when the condition held, and does
// nothing otherwise. It is [Validation.RejectKey] hung off a condition, as
// [Condition.Reject] is [Validation.Reject].
func (c *Condition) RejectKey(target any, key string, args ...any) *Condition {
	if c.holds {
		c.validation.RejectKey(target, key, args...)
	}
	return c
}

// Nested validates a model held inside another, reporting its failures under a
// dotted path:
//
//	v.Nested(&in.Address)
//
// A failure on the nested model's City field is reported as "address.city". A
// nil pointer is skipped, so an optional nested model needs no guard of its
// own.
//
// The argument has to be a pointer, and the model's Validate has to be
// declared on the pointer. A value would be skipped, and a Validate with a
// value receiver would bind its rules to a copy, so a route whose model does
// either is refused when it is compiled, in the way a rule bound to a value
// is.
//
// A collection of models is validated by nesting each element, by its address
// in the collection:
//
//	for i := range in.Items {
//		v.Nested(&in.Items[i])
//	}
//
// A failure on the third item's Name is then reported as "items[2].name", and
// the same holds for a collection of pointers, nested as v.Nested(in.Items[i]).
// The address has to be the element's own. Ranging over the values, as in
// `for _, item := range in.Items { v.Nested(&item) }`, validates a copy, whose
// transforms never reach the collection and whose position cannot be traced,
// so its failures are reported without it. The collection has to be a field of
// the model calling Nested, as any nested model does.
//
// The nested model is validated there and then, transforms included, rather
// than after the outer model's Validate returns: that is what lets a report
// that has reached [MaxValidationDetails] stop nesting, so a model nested once
// per element of a long collection costs nothing past the ceiling. Its
// failures are still listed after the outer model's own. A model nested more
// than [MaxNestedDepth] levels deep is refused rather than validated.
func (v *Validation) Nested(model Validatable) {
	if model == nil {
		return
	}
	if v.dry != nil {
		v.dry.record(model)
	}
	if v.run == nil || v.run.truncated {
		return
	}
	pointer := reflect.ValueOf(model)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return
	}
	if v.depth >= MaxNestedDepth {
		v.run.add(ErrorDetail{
			Field:    joinPath(v.prefix(), v.nameOfNested(pointer)),
			Location: "body",
			Issue:    fmt.Sprintf("must not be nested more than %d levels deep", MaxNestedDepth),
			Kind:     "too_deep",
			Args:     []any{"count", MaxNestedDepth},
		})
		return
	}

	child := v.run.child(v.depth + 1)
	child.plan = planForType(pointer.Type().Elem())
	child.base = pointer.Pointer()
	child.size = pointer.Type().Elem().Size()
	child.value = pointer.Elem()
	child.parent = v
	child.depth = v.depth + 1
	child.run = v.run
	child.start = len(v.run.out)
	defer child.reset()
	model.Validate(child)
	child.evaluate()
}

// MaxNestedDepth is how many models deep [Validation.Nested] follows.
//
// A model that nests its own type, a tree or a thread of replies, is as deep as
// the client makes it, and every level is a Validate call on the stack and a
// segment on the path of every failure beneath it: nine thousand levels of a
// recursive model once cost a 300 megabyte report and gigabytes of allocation.
// A model nested deeper than this is not validated. It is reported, as a
// failure of kind "too_deep" on the field that holds it, so the request is
// refused rather than passed on with part of it unchecked.
//
// Thirty-two is well past the depth of any model a person designs, and a
// recursive one that must go deeper should bound its own depth with a rule
// before it nests.
const MaxNestedDepth = 32

// prefix is the dotted path of the model this Validation validates, empty for
// the outermost one, built the first time a failure needs it.
func (v *Validation) prefix() string {
	if v.parent == nil || v.pathKnown {
		return v.path
	}
	v.path = joinPath(v.parent.prefix(), v.parent.nameOfNested(v.value.Addr()))
	v.pathKnown = true
	return v.path
}

// nameOfNested works out which field of this model holds a nested one.
//
// A model embedded by value sits inside its parent, so its address resolves
// through the offset map like any other field. A model held behind a pointer
// does not: its address is wherever it was allocated, which says nothing about
// the field pointing at it. For that case the parent's pointer fields are
// compared against the address, which is a short scan over a handful of fields
// and only happens when the direct lookup fails. A model held in an element of
// one of the parent's collections is found last, and named with its position,
// as "items[2]".
func (v *Validation) nameOfNested(pointer reflect.Value) string {
	if origin, found := v.originOf(pointer.Interface()); found {
		return origin.name
	}
	if !v.value.IsValid() || v.value.Kind() != reflect.Struct {
		return ""
	}
	address := pointer.Pointer()
	for i := range v.value.NumField() {
		field := v.value.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() || field.Pointer() != address {
			continue
		}
		if origin, found := v.plan.fields[v.value.Type().Field(i).Offset]; found {
			return origin.name
		}
	}
	for _, list := range v.plan.lists {
		if i, found := v.elementIndex(v.value.FieldByIndex(list.index), pointer); found {
			return v.plan.fields[list.offset].name + "[" + strconv.Itoa(i) + "]"
		}
	}
	return ""
}

// elementIndex reports which element of a collection a nested model is.
//
// An element held by value is found by arithmetic on its address, since the
// elements sit side by side. One held behind a pointer has to be searched for.
// The search starts after the element found last, because a model nested once
// per element is nested in order, and the name is only worked out for a model
// that failed, of which a report holds at most [MaxValidationDetails]; so a
// long collection is searched from the start at most that many times, and in
// the usual order hardly at all.
func (v *Validation) elementIndex(list, pointer reflect.Value) (int, bool) {
	n := list.Len()
	address := pointer.Pointer()
	switch list.Type().Elem() {
	case pointer.Type().Elem():
		size := pointer.Type().Elem().Size()
		first := list.Pointer()
		if n == 0 || address < first || address >= first+uintptr(n)*size || (address-first)%size != 0 {
			return 0, false
		}
		return int((address - first) / size), true
	case pointer.Type():
		for step := range n {
			i := (v.lastElement + 1 + step) % n
			if list.Index(i).Pointer() == address {
				v.lastElement = i
				return i, true
			}
		}
	}
	return 0, false
}

// details evaluates the outermost model's own rules, after everything nested in
// it already has been, and returns the whole report.
func (v *Validation) details() []ErrorDetail {
	if v.run == nil {
		v.run = &v.state
	}
	v.evaluate()
	return v.run.report()
}

// evaluate turns this Validation's own rules and rejections into error
// details, naming each field the way the binder named it and saying where it
// came from, and moves them ahead of the failures of the models nested in it.
//
// Each rule is asked for one failure more than the report has room for, so a
// full report learns that it is incomplete without evaluating the rest of a
// collection to find out by how much.
func (v *Validation) evaluate() {
	run := v.run
	own := len(run.out)
	for _, rules := range v.rules {
		if run.truncated {
			break
		}
		var problems []validate.Problem
		if bounded, ok := rules.(boundedEvaluator); ok {
			problems = bounded.EvaluateUpTo(run.room() + 1)
		} else {
			problems = rules.Evaluate()
		}
		if len(problems) == 0 {
			continue
		}
		name, location := v.describe(rules.Target(), rules.Label())
		field := joinPath(v.prefix(), name)
		for _, problem := range problems {
			run.add(ErrorDetail{
				Field:    field + problem.Path,
				Location: location,
				Issue:    problem.Issue,
				Kind:     string(problem.Kind),
				Key:      problem.Key,
				Args:     problem.Args,
			})
		}
	}
	for _, rejected := range v.rejected {
		name, location := v.describe(rejected.target, "")
		run.add(ErrorDetail{
			Field:    joinPath(v.prefix(), name),
			Location: location,
			Issue:    rejected.issue,
			Key:      rejected.key,
			Args:     rejected.args,
		})
	}
	// Rotating the two runs in place keeps the report in the order it has
	// always had, a model's own failures before its nested models', at a cost
	// bounded by the ceiling rather than by the depth of the nesting.
	slices.Reverse(run.out[v.start:own])
	slices.Reverse(run.out[own:])
	slices.Reverse(run.out[v.start:])
}

// describe resolves a field pointer to the name and location it should be
// reported under. A rule set's own label wins when it set one, which is what
// [validate.StringRules.As] is for.
func (v *Validation) describe(target any, label string) (name, location string) {
	origin, found := v.originOf(target)
	switch {
	case label != "" && found:
		return label, origin.location
	case label != "":
		return label, "body"
	case found:
		return origin.name, origin.location
	default:
		// A pointer outside the model, which only a hand-written rejection can
		// produce, is reported against the request rather than against a field.
		return "", "body"
	}
}

// originOf looks a field pointer up by its offset within the model.
func (v *Validation) originOf(target any) (fieldOrigin, bool) {
	if target == nil || v.plan == nil || v.base == 0 {
		return fieldOrigin{}, false
	}
	pointer := reflect.ValueOf(target)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return fieldOrigin{}, false
	}
	address := pointer.Pointer()
	if address < v.base || address >= v.base+v.size {
		return fieldOrigin{}, false
	}
	origin, found := v.plan.fields[address-v.base]
	return origin, found
}

// joinPath appends a field name to a prefix, leaving out the separator when
// either side is empty.
func joinPath(prefix, name string) string {
	switch {
	case prefix == "":
		return name
	case name == "":
		return prefix
	default:
		return prefix + "." + name
	}
}

// fieldOrigin is how one field should be reported: the name the client knows it
// by, and the part of the request it arrived in.
type fieldOrigin struct {
	name     string
	location string
}

// validationPlan maps a field's offset within a model to how it should be
// reported.
//
// Offsets are computed once, when the route is registered, so validating a
// request costs a subtraction and a map lookup per rule rather than a walk over
// the type.
type validationPlan struct {
	fields map[uintptr]fieldOrigin
	// lists are the collections a model nested per element can be held in,
	// which is how its failures are given the element's position.
	lists []listField
}

// listField is a collection field whose elements are models or pointers to
// them: where it sits, so its elements can be found, and the offset its name
// is recorded under.
type listField struct {
	index  []int
	offset uintptr
}

// newValidationPlan records where every field of an input type came from,
// starting from the JSON names and then correcting the ones the binder read
// from somewhere other than the body.
func newValidationPlan(t reflect.Type, plan *bindPlan) *validationPlan {
	vp := &validationPlan{fields: map[uintptr]fieldOrigin{}, lists: collectLists(t, nil, 0, nil)}
	collectOrigins(t, 0, vp.fields)
	for _, binders := range [][]paramBinder{plan.params, plan.form} {
		for i := range binders {
			p := &binders[i]
			if offset, ok := offsetOf(t, p.index); ok {
				vp.fields[offset] = fieldOrigin{name: p.name, location: p.source.String()}
			}
		}
	}
	for i := range plan.files {
		f := &plan.files[i]
		if offset, ok := offsetOf(t, f.index); ok {
			vp.fields[offset] = fieldOrigin{name: f.name, location: srcFile.String()}
		}
	}
	return vp
}

// nestedPlans caches the plans for models reached through [Validation.Nested],
// which are not routes of their own and so have no binding plan to derive from.
var nestedPlans sync.Map

// planForType returns the plan for a nested model, building it once per type.
func planForType(t reflect.Type) *validationPlan {
	if cached, found := nestedPlans.Load(t); found {
		return cached.(*validationPlan)
	}
	vp := &validationPlan{fields: map[uintptr]fieldOrigin{}, lists: collectLists(t, nil, 0, nil)}
	collectOrigins(t, 0, vp.fields)
	actual, _ := nestedPlans.LoadOrStore(t, vp)
	return actual.(*validationPlan)
}

// collectOrigins walks a struct, recording each field's offset and the name it
// is encoded under. Every field starts out as body content; the caller corrects
// the located ones afterwards.
func collectOrigins(t reflect.Type, base uintptr, into map[uintptr]fieldOrigin) {
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		field := t.Field(i)
		if !usableField(field) {
			continue
		}
		offset := base + field.Offset
		name, _, _ := strings.Cut(field.Tag.Get(tagJSON), ",")
		if name == "" {
			name = field.Name
		}
		if name != "-" {
			into[offset] = fieldOrigin{name: name, location: "body"}
		}
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			collectOrigins(field.Type, offset, into)
		}
	}
}

// collectLists finds the collections of structs, or of pointers to them, that
// collectOrigins names, walking embedded structs the way it does.
func collectLists(t reflect.Type, index []int, base uintptr, into []listField) []listField {
	if t.Kind() != reflect.Struct {
		return into
	}
	for i := range t.NumField() {
		field := t.Field(i)
		if !usableField(field) {
			continue
		}
		path := append(slices.Clip(index), i)
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			into = collectLists(field.Type, path, base+field.Offset, into)
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get(tagJSON), ",")
		if field.Type.Kind() != reflect.Slice || name == "-" {
			continue
		}
		elem := field.Type.Elem()
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Struct && elem.Size() > 0 {
			into = append(into, listField{index: path, offset: base + field.Offset})
		}
	}
	return into
}

// offsetOf resolves a field index path to a byte offset within the struct.
func offsetOf(t reflect.Type, index []int) (uintptr, bool) {
	var offset uintptr
	current := t
	for _, i := range index {
		if current.Kind() != reflect.Struct || i >= current.NumField() {
			// coverage: index paths come from the binding plan, which built
			// them by walking this very type, so they always resolve.
			return 0, false
		}
		field := current.Field(i)
		offset += field.Offset
		current = field.Type
	}
	return offset, true
}

// describeConstraints reports what a model's rules demand of each field, keyed
// by the name the field is reported under.
//
// The rules are collected by running Validate once against a zero value, which
// is safe because declaring a rule set has no effect beyond recording it. Only
// the rules that map onto JSON Schema keywords contribute; a Must rule is
// opaque by nature and adds nothing.
func (p *bindPlan) describeConstraints() map[string]validate.Constraints {
	if p.validation == nil {
		return nil
	}
	scratch := reflect.New(p.typ)
	model, ok := scratch.Interface().(Validatable)
	if !ok {
		// coverage: the plan only carries a validation plan for a type that
		// implements Validatable, which is checked when the route is compiled.
		return nil
	}

	v := &Validation{plan: p.validation, base: scratch.Pointer(), size: p.typ.Size(), value: scratch.Elem()}
	model.Validate(v)

	out := make(map[string]validate.Constraints, len(v.rules))
	for _, rules := range v.rules {
		name, _ := v.describe(rules.Target(), rules.Label())
		if name == "" {
			continue
		}
		out[name] = rules.Describe()
	}
	return out
}

// elementConstraints reports the rules a model applies to the elements of each
// of its collections, so an array's items can be described as precisely as the
// array itself.
func (p *bindPlan) elementConstraints() map[string]validate.Constraints {
	if p.validation == nil {
		return nil
	}
	scratch := reflect.New(p.typ)
	model, ok := scratch.Interface().(Validatable)
	if !ok {
		// coverage: guarded by the same check as describeConstraints.
		return nil
	}

	v := &Validation{plan: p.validation, base: scratch.Pointer(), size: p.typ.Size(), value: scratch.Elem()}
	model.Validate(v)

	out := map[string]validate.Constraints{}
	for _, rules := range v.rules {
		describer, ok := rules.(interface{ DescribeElement() validate.Constraints })
		if !ok {
			continue
		}
		name, _ := v.describe(rules.Target(), rules.Label())
		if name == "" {
			continue
		}
		if constraints := describer.DescribeElement(); !constraints.IsZero() {
			out[name] = constraints
		}
	}
	return out
}

// constraintsForDocs reports the field constraints for the OpenAPI document,
// or nothing when the route skips validation, because a document should
// describe what the route actually enforces.
func (rt *Route) constraintsForDocs() map[string]validate.Constraints {
	if rt.skipValidation {
		return nil
	}
	return rt.plan.describeConstraints()
}

// elementConstraintsForDocs reports the constraints on collection elements.
func (rt *Route) elementConstraintsForDocs() map[string]validate.Constraints {
	if rt.skipValidation {
		return nil
	}
	return rt.plan.elementConstraints()
}

// validationPool recycles Validation values, and with them the rule sets they
// have already built.
//
// A model declares the same shape on every request, so after the first request
// through a route the rule sets, their step slices and the detail slice are all
// already the right size. Nothing request-specific survives a reset, which is
// what makes sharing them safe.
var validationPool = sync.Pool{
	New: func() any { return new(Validation) },
}

// runValidation validates a bound model and returns what failed.
//
// Fields that already failed to bind are left alone: telling a client that
// "limit" is both unparseable and below the minimum says nothing the first
// message did not.
func (p *bindPlan) runValidation(dst reflect.Value, failed map[string]bool) []ErrorDetail {
	if p.validation == nil {
		return nil
	}
	model, ok := dst.Addr().Interface().(Validatable)
	if !ok {
		// coverage: the plan only carries a validation plan for types that
		// implement Validatable, which is checked when the route is compiled.
		return nil
	}

	v := validationPool.Get().(*Validation)
	defer func() {
		v.reset()
		validationPool.Put(v)
	}()
	v.plan = p.validation
	v.base = dst.Addr().Pointer()
	v.size = p.typ.Size()
	v.value = dst
	v.run = &v.state
	v.run.failed = failed
	model.Validate(v)
	return v.details()
}

// checkRulesBindToFields refuses a model whose rules are bound to something
// that is not one of its fields.
//
// A rule set is matched to its field by that field's address, so
// `v.String(&in.Name)` is matched and `v.String(in.Name)` -- the pointer the
// field holds rather than the field -- is not. Both compile, because
// StringField and NumberField admit the pointer type so that one entry point
// can serve `Name string` and `Nickname *string` alike.
//
// The wrong one fails in the two ways that look most like working:
//
//   - the request is still refused, and the detail carries an empty field, so
//     a client is told that something is wrong and not what;
//   - the rule contributes nothing to the generated document, because
//     describeConstraints skips a rule it cannot name, so the published schema
//     quietly loses the constraint the code still enforces.
//
// Neither shows up in a test that asserts a status. In the service this check
// was written for it had gone unnoticed across nineteen call sites and five
// models -- every optional field on five resources.
//
// It runs once, when the route is compiled, against a zero value: declaring a
// rule set only records it, which is what makes that safe and is the same trick
// describeConstraints uses. A rule the model only declares under a condition is
// not seen here, and that is the residue -- but the condition is usually a nil
// check that is itself redundant, because a nil pointer field already skips its
// rules.
//
// A rule that named itself with As() is left alone. Binding to something
// outside the model is then deliberate, and the name it reports under is the
// one the caller chose.
func (p *bindPlan) checkRulesBindToFields() error {
	if p.validation == nil {
		return nil
	}
	return checkModelRules(p.typ, p.validation, map[reflect.Type]bool{})
}

// dryRun collects what the models nested by one model's Validate looked like
// when it was called against a zero value.
type dryRun struct {
	nested []reflect.Type
	// problem is the first misuse of Nested seen, worded for the developer.
	problem string
}

// record notes a call to Nested, and what is wrong with it if anything is.
//
// Nested only ever validates the model a pointer leads to, and does nothing
// for anything else, which is how a mistake in it looks like working: it
// compiles, the request is accepted, and the rules that were meant to run
// never did.
//
//   - A model handed over by value, `v.Nested(in.Address)`, is a value
//     rather than a pointer, and is skipped without a word. That compiles
//     only because a Validate declared with a value receiver is in the method
//     set of the value too.
//   - A pointer to a model whose Validate has a value receiver is validated,
//     but the rules bind to the receiver, which is a copy: their failures are
//     reported against the parent field rather than the one that failed, and
//     their transforms never reach the request.
func (d *dryRun) record(model Validatable) {
	rv := reflect.ValueOf(model)
	if rv.Kind() != reflect.Pointer {
		if d.problem == "" {
			d.problem = fmt.Sprintf(
				"v.Nested was given a %s rather than a pointer to one, so the model is never validated; "+
					"pass its address (v.Nested(&in.Field))", rv.Type())
		}
		return
	}
	elem := rv.Type().Elem()
	if elem.Implements(reflect.TypeFor[Validatable]()) {
		if d.problem == "" {
			d.problem = fmt.Sprintf(
				"the Validate method of %s has a value receiver, so the rules it declares bind to a copy of the model: "+
					"their failures would be reported against the wrong field and their transforms would be lost; "+
					"declare it on the pointer, func (m *%s) Validate", elem, elem.Name())
		}
		return
	}
	d.nested = append(d.nested, elem)
}

// checkModelRules is [bindPlan.checkRulesBindToFields] for one model type, and
// then for each model that one nests, since a model is nested by the same
// mistakes as a request is. seen keeps a model that nests its own type from
// being followed for ever.
func checkModelRules(typ reflect.Type, plan *validationPlan, seen map[reflect.Type]bool) error {
	if seen[typ] {
		return nil
	}
	seen[typ] = true

	scratch := reflect.New(typ)
	model, ok := scratch.Interface().(Validatable)
	if !ok {
		// coverage: the caller only reaches this for a type implementing it.
		return nil
	}

	v := &Validation{
		plan:  plan,
		base:  scratch.Pointer(),
		size:  typ.Size(),
		value: scratch.Elem(),
		dry:   &dryRun{},
	}
	model.Validate(v)

	for _, rules := range v.rules {
		if rules.Label() != "" {
			continue
		}
		if _, found := v.originOf(rules.Target()); found {
			continue
		}
		return fmt.Errorf(
			"a %s rule is bound to a value rather than to a field of %s, so its failures would name no field "+
				"and its constraints would be missing from the generated document; "+
				"pass the field's address (v.Rule(&in.Field), not v.Rule(in.Field)), or name it with As()",
			ruleSetKind(rules), typ)
	}
	if v.dry.problem != "" {
		return fmt.Errorf("%s: %s", typ, v.dry.problem)
	}
	for _, nested := range v.dry.nested {
		if err := checkModelRules(nested, planForType(nested), seen); err != nil {
			return err
		}
	}
	return nil
}

// ruleSetKind names a rule set for the error above, so it says "a String rule"
// rather than a package-qualified type nobody wrote.
func ruleSetKind(rules validate.Evaluator) string {
	name := reflect.TypeOf(rules).String()
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, "Rules")
}
