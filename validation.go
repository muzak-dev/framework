package badele

import (
	"reflect"
	"strings"
	"sync"
	"time"

	"badele/validate"
)

// Validatable is implemented by an input model that declares validation rules.
//
// Badele runs Validate after binding and after every guard, so a model never
// gets to tell an unauthenticated caller what is wrong with its request. The
// method belongs on the pointer type, which is what lets rules name fields by
// address:
//
//	func (in *CreateUser) Validate(v *badele.Validation) {
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
// rather than failing them.
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
	prefix   string
	rules    []validate.Evaluator
	rejected []rejection
	children []*Validation

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
func (v *Validation) reset() {
	v.plan = nil
	v.base = 0
	v.size = 0
	v.value = reflect.Value{}
	v.prefix = ""
	for i := range v.rules {
		v.rules[i] = nil
	}
	v.rules = v.rules[:0]
	for i := range v.rejected {
		v.rejected[i] = rejection{}
	}
	v.rejected = v.rejected[:0]
	v.children = v.children[:0]
	v.usedStrings, v.usedNumbers, v.usedTimes = 0, 0, 0
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
}

// String binds rules to a string field.
//
//	v.String(&in.Email).Trim().Lower().Required().Email()
//
// The field may be a string or a pointer to one; a nil pointer skips its rules.
// A field of any other type does not compile.
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
	v.rejected = append(v.rejected, rejection{target: target, issue: issue})
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

// Nested validates a model held inside another, reporting its failures under a
// dotted path:
//
//	v.Nested(&in.Address)
//
// A failure on the nested model's City field is reported as "address.city". A
// nil pointer is skipped, so an optional nested model needs no guard of its
// own.
func (v *Validation) Nested(model Validatable) {
	if model == nil {
		return
	}
	pointer := reflect.ValueOf(model)
	if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
		return
	}

	child := &Validation{
		plan:   planForType(pointer.Type().Elem()),
		base:   pointer.Pointer(),
		size:   pointer.Type().Elem().Size(),
		value:  pointer.Elem(),
		prefix: joinPath(v.prefix, v.nameOfNested(model, pointer)),
	}
	model.Validate(child)
	v.children = append(v.children, child)
}

// nameOfNested works out which field of this model holds a nested one.
//
// A model embedded by value sits inside its parent, so its address resolves
// through the offset map like any other field. A model held behind a pointer
// does not: its address is wherever it was allocated, which says nothing about
// the field pointing at it. For that case the parent's pointer fields are
// compared against the address, which is a short scan over a handful of fields
// and only happens when the direct lookup fails.
func (v *Validation) nameOfNested(model Validatable, pointer reflect.Value) string {
	if origin, found := v.originOf(model); found {
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
	return ""
}

// details turns everything collected into error details, naming each field the
// way the binder named it and saying where it came from.
func (v *Validation) details() []ErrorDetail {
	var out []ErrorDetail
	for _, rules := range v.rules {
		name, location := v.describe(rules.Target(), rules.Label())
		for _, problem := range rules.Evaluate() {
			out = append(out, ErrorDetail{
				Field:    joinPath(v.prefix, name) + problem.Path,
				Location: location,
				Issue:    problem.Issue,
			})
		}
	}
	for _, rejected := range v.rejected {
		name, location := v.describe(rejected.target, "")
		out = append(out, ErrorDetail{
			Field:    joinPath(v.prefix, name),
			Location: location,
			Issue:    rejected.issue,
		})
	}
	for _, child := range v.children {
		out = append(out, child.details()...)
	}
	return out
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
}

// newValidationPlan records where every field of an input type came from,
// starting from the JSON names and then correcting the ones the binder read
// from somewhere other than the body.
func newValidationPlan(t reflect.Type, plan *bindPlan) *validationPlan {
	vp := &validationPlan{fields: map[uintptr]fieldOrigin{}}
	collectOrigins(t, 0, vp.fields)
	for i := range plan.params {
		p := &plan.params[i]
		if offset, ok := offsetOf(t, p.index); ok {
			vp.fields[offset] = fieldOrigin{name: p.name, location: p.source.String()}
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
	vp := &validationPlan{fields: map[uintptr]fieldOrigin{}}
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
	model.Validate(v)

	details := v.details()
	if len(failed) == 0 {
		return details
	}
	kept := details[:0]
	for _, detail := range details {
		if !failed[detail.Field] {
			kept = append(kept, detail)
		}
	}
	return kept
}
