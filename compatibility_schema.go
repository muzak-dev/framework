package muzak

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// This file compares two schemas at one position of two documents, in the
// direction the value travels.
//
// A position holds a list of conjuncts rather than one schema: JSON Schema
// 2020-12 applies a $ref together with what is written beside it, and allOf
// lists schemas a value satisfies every one of, so the schema at a position is
// everything it reaches that way, which is what [schemaView] collects. That is
// how a rule Muzak writes beside a reference, for one use of a type, or a
// second pattern it moves into allOf, is compared as the constraint it is and
// not as a member or a keyword that came and went.
//
// A position that is nothing but a reference, as almost every use of a named
// type is, is not walked into. The pair of components the two documents refer
// to there is queued instead, and compared once, in components, however many
// positions lead to it. That is what keeps a type every operation shares, or
// one that refers to itself, from being compared once per path through it.

// oneSchema is the list of conjuncts a position with one schema holds, which
// is empty for a position with none, a value of any kind.
func oneSchema(s *Schema) []*Schema {
	if s == nil {
		return nil
	}
	return []*Schema{s}
}

// compareParts compares what one position of each document holds.
func (c *docComparer) compareParts(location string, old, cur []*Schema, dir apiDirection, textual bool) {
	if !c.step(location) {
		return
	}
	if c.depth >= compareMaxDepth {
		c.incomplete = true
		return
	}
	c.depth++
	defer func() { c.depth-- }()
	if len(old) == 1 && len(cur) == 1 {
		oldName, oldNull, oldRef := pureReference(c.oldSchemas, old[0])
		curName, curNull, curRef := pureReference(c.curSchemas, cur[0])
		if oldRef && curRef {
			if !c.spend(c.readCost(old[0]) + c.readCost(cur[0])) {
				return
			}
			c.compareNullable(location, oldNull, curNull, dir)
			c.compareAnnotations(location, old, cur, dir)
			c.enqueue(oldName, curName, dir, textual)
			return
		}
	}
	c.compareViews(location, c.view(c.oldSchemas, old), c.view(c.curSchemas, cur), dir, textual)
}

// schemaFacts is what a comparison reads from one schema every time a position
// reaches it, worked out the first time: the canonical JSON of its enum values
// other than null and of its default, and what reading the schema costs.
type schemaFacts struct {
	enum       []string
	def        string
	hasDefault bool
	cost       int
}

// facts returns the facts of a schema, working them out the first time.
//
// Reading a schema costs a step, one more for each value, member, required
// name and alternative it lists, and one for every compareBytesPerStep bytes
// of the text in them, which is what reading it again at another position
// takes. A comparison charges that every time it reads the schema, and a
// document's budget is the sum of it over its schemas, so that a schema many
// positions reach through allOf, or beside a rule of their own, costs its size
// each time rather than one step, and a large one reached everywhere ends the
// comparison at its bound rather than taking time quadratic in the documents.
func (c *docComparer) facts(s *Schema) *schemaFacts {
	if known, ok := c.schemaFacts[s]; ok {
		return known
	}
	f := &schemaFacts{}
	text := len(s.Ref) + len(s.Format) + len(s.Pattern) + len(s.ContentEncoding)
	for _, value := range s.Enum {
		if value != nil {
			canonical := canonicalValue(value)
			f.enum = append(f.enum, canonical)
			text += len(canonical)
		}
	}
	if s.Default != nil {
		f.def, f.hasDefault = canonicalValue(s.Default), true
		text += len(f.def)
	}
	for _, name := range s.Required {
		text += len(name)
	}
	for name := range s.Properties {
		text += len(name)
	}
	f.cost = 1 + len(s.Enum) + len(s.Required) + len(s.Properties) + len(s.AnyOf) + len(s.AllOf) + text/compareBytesPerStep
	c.schemaFacts[s] = f
	return f
}

// readCost is what reading a position that holds nothing but a reference
// costs: the schema there and, when it is a choice between a schema and null,
// that schema too.
func (c *docComparer) readCost(s *Schema) int {
	cost := c.facts(s).cost
	if inner, wrapped := nullableCore(s); wrapped {
		cost += c.facts(inner).cost
	}
	return cost
}

// enqueue queues a pair of components for comparison, unless it already was
// in this direction. Comparing two different components is how a renamed one,
// or the Input copy Muzak makes of a type a response also uses, is followed.
func (c *docComparer) enqueue(old, cur string, dir apiDirection, textual bool) {
	pair := schemaPair{old: old, cur: cur, dir: dir, textual: textual}
	if c.pairs[pair] {
		return
	}
	c.pairs[pair] = true
	if old != cur {
		c.add(Compatible, dir.prefix()+"schema-renamed", "/components/schemas/"+pointerToken(old),
			fmt.Sprintf("%sthe schema %q is now %q.", dir.where(), old, cur))
	}
	c.queue = append(c.queue, pair)
}

// refTarget returns the component a reference names, or nil for one that
// leads anywhere else or nowhere.
func refTarget(schemas map[string]*Schema, ref string) (string, *Schema) {
	name, local := strings.CutPrefix(ref, componentPrefix)
	if !local {
		return "", nil
	}
	return name, schemas[name]
}

// pureReference reports whether a schema says nothing but which component it
// is, perhaps also admitting null as [nullable] spells it, and annotations
// such as a description or a default, which a position compares for itself.
func pureReference(schemas map[string]*Schema, s *Schema) (name string, admitsNull, pure bool) {
	core := s
	if inner, wrapped := nullableCore(s); wrapped {
		if s.Ref != "" || constrains(s, true) {
			return "", false, false
		}
		core, admitsNull = inner, true
	}
	if core.Ref == "" || constrains(core, false) {
		return "", false, false
	}
	name, target := refTarget(schemas, core.Ref)
	if target == nil {
		return "", false, false
	}
	return name, admitsNull, true
}

// constrains reports whether a schema says anything about a value beside the
// reference it may hold. Annotations do not count; neither does anyOf when
// skipUnion is set, for the caller that is looking at it.
func constrains(s *Schema, skipUnion bool) bool {
	return s.Type != nil || s.Format != "" || s.ContentEncoding != "" || s.Properties != nil || s.Required != nil ||
		s.Items != nil || (s.AnyOf != nil && !skipUnion) || s.AllOf != nil || s.AdditionalProperties != nil ||
		s.Enum != nil || s.Pattern != "" || s.MinLength != nil || s.MaxLength != nil || s.Minimum != nil ||
		s.Maximum != nil || s.ExclusiveMinimum != nil || s.ExclusiveMaximum != nil || s.MultipleOf != nil ||
		s.MinItems != nil || s.MaxItems != nil || s.UniqueItems
}

// nullableCore returns what a schema admits besides null, when it is written
// as a choice between that and null, which is how [nullable] widens a
// reference.
func nullableCore(s *Schema) (*Schema, bool) {
	if len(s.AnyOf) != 2 {
		return nil, false
	}
	first, second := s.AnyOf[0], s.AnyOf[1]
	switch {
	case first != nil && isNullOnly(second):
		return first, true
	case second != nil && isNullOnly(first):
		return second, true
	}
	return nil, false
}

// isNullOnly reports whether a schema admits null and nothing else.
func isNullOnly(s *Schema) bool {
	if s == nil {
		return false
	}
	names, typed := typeNames(s.Type)
	if !typed || len(names) != 1 || names[0] != "null" {
		return false
	}
	rest := *s
	rest.Type = nil
	return rest.Ref == "" && !constrains(&rest, false)
}

// typeNames reads the type keyword, which is one name or a list of them. It
// reports false for a schema with no type, which admits values of every type,
// and for a value it cannot read, which a Document built in Go may hold and
// which is then taken to say nothing.
func typeNames(t any) ([]string, bool) {
	switch v := t.(type) {
	case string:
		return []string{v}, true
	case []string:
		return v, true
	case []any:
		names := make([]string, 0, len(v))
		for _, name := range v {
			if text, ok := name.(string); ok {
				names = append(names, text)
			}
		}
		return names, true
	}
	return nil, false
}

// jsonTypes is a set of JSON types. A number is an integer or a fraction, so
// that an integer is the narrower of the two.
type jsonTypes uint8

const (
	typeString jsonTypes = 1 << iota
	typeInteger
	typeFraction
	typeBoolean
	typeObject
	typeArray

	allTypes = typeString | typeInteger | typeFraction | typeBoolean | typeObject | typeArray
)

// String names the types for a message.
func (t jsonTypes) String() string {
	switch t {
	case allTypes:
		return "any type"
	case 0:
		return "null alone"
	}
	var names []string
	if t&typeString != 0 {
		names = append(names, "string")
	}
	switch t & (typeInteger | typeFraction) {
	case typeInteger:
		names = append(names, "integer")
	case typeInteger | typeFraction, typeFraction:
		names = append(names, "number")
	}
	if t&typeBoolean != 0 {
		names = append(names, "boolean")
	}
	if t&typeObject != 0 {
		names = append(names, "object")
	}
	if t&typeArray != 0 {
		names = append(names, "array")
	}
	return strings.Join(names, " or ")
}

// typesOf reads the types a schema admits other than null, reporting false
// when it does not say.
func typesOf(s *Schema) (jsonTypes, bool) {
	names, typed := typeNames(s.Type)
	if !typed {
		return allTypes, false
	}
	var set jsonTypes
	for _, name := range names {
		switch name {
		case "string":
			set |= typeString
		case "integer":
			set |= typeInteger
		case "number":
			set |= typeInteger | typeFraction
		case "boolean":
			set |= typeBoolean
		case "object":
			set |= typeObject
		case "array":
			set |= typeArray
		}
	}
	return set, true
}

// typeAdmitsNull reports whether a type keyword admits null.
func typeAdmitsNull(t any) bool {
	names, typed := typeNames(t)
	return !typed || slices.Contains(names, "null")
}

// partAdmitsNull reports whether one conjunct admits null. The answer for each
// schema is worked out once and kept, so a union that many positions reach
// costs as much as it is large once rather than once per position. While it is
// being worked out a schema is taken not to admit null, so that a Document
// built in Go whose union holds itself as an alternative gets an answer
// instead of recursing until the stack runs out, which no recover survives.
func (c *docComparer) partAdmitsNull(s *Schema) bool {
	if known, ok := c.admitsNull[s]; ok {
		return known
	}
	c.admitsNull[s] = false
	admits := typeAdmitsNull(s.Type) && (s.Enum == nil || slices.Contains(s.Enum, nil))
	if admits && len(s.AnyOf) > 0 {
		_, wrapped := nullableCore(s)
		admits = wrapped || slices.ContainsFunc(s.AnyOf, func(alternative *Schema) bool {
			return alternative == nil || c.partAdmitsNull(alternative)
		})
	}
	c.admitsNull[s] = admits
	return admits
}

// schemaView is what a position holds, gathered from everything it reaches
// through references and allOf: parts are the conjuncts, unions the parts
// that also hold a choice between alternatives, and nullable whether null
// satisfies all of it.
type schemaView struct {
	parts    []*Schema
	unions   []*Schema
	nullable bool
}

// view gathers what a position holds. A part reached through a choice between
// a schema and null is held to its keywords only when the value is not null,
// so it has no say in whether null is admitted.
func (c *docComparer) view(schemas map[string]*Schema, nodes []*Schema) schemaView {
	v := schemaView{nullable: true}
	seen := map[*Schema]bool{}
	var gather func(s *Schema, core bool, depth int)
	gather = func(s *Schema, core bool, depth int) {
		if s == nil || seen[s] {
			return
		}
		if depth >= compareMaxDepth {
			c.incomplete = true
			return
		}
		if !c.spend(c.facts(s).cost) {
			return
		}
		seen[s] = true
		v.parts = append(v.parts, s)
		if !core && !c.partAdmitsNull(s) {
			v.nullable = false
		}
		if s.Ref != "" {
			if _, target := refTarget(schemas, s.Ref); target != nil {
				gather(target, core, depth+1)
			}
		}
		for _, also := range s.AllOf {
			gather(also, core, depth+1)
		}
		if len(s.AnyOf) > 0 {
			if inner, wrapped := nullableCore(s); wrapped {
				gather(inner, true, depth+1)
			} else {
				v.unions = append(v.unions, s)
			}
		}
	}
	for _, node := range nodes {
		gather(node, false, 0)
	}
	return v
}

// types returns the types other than null the view admits. A value that
// travels as text, as a parameter or a form value does, is a string whatever
// else it is described as, so a string there admits every scalar.
func (v schemaView) types(textual bool) jsonTypes {
	set := allTypes
	for _, part := range v.parts {
		if types, typed := typesOf(part); typed {
			set &= types
		}
	}
	if textual && set&typeString != 0 {
		set |= typeInteger | typeFraction | typeBoolean
	}
	return set
}

// enum returns the values other than null the view is limited to, by their
// canonical JSON, and whether it is limited at all. Null is left to
// nullability, which is compared on its own.
func (c *docComparer) enum(v schemaView) (map[string]bool, bool) {
	var allowed map[string]bool
	for _, part := range v.parts {
		if part.Enum == nil {
			continue
		}
		values := map[string]bool{}
		for _, value := range c.facts(part).enum {
			values[value] = true
		}
		if allowed == nil {
			allowed = values
			continue
		}
		maps.DeleteFunc(allowed, func(value string, _ bool) bool { return !values[value] })
	}
	return allowed, allowed != nil
}

// strings gathers one string keyword from every part, sorted and distinct.
func (v schemaView) strings(keyword func(*Schema) string) []string {
	var out []string
	for _, part := range v.parts {
		if value := keyword(part); value != "" {
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// schemaBound is a lower or an upper bound, which may exclude its value.
type schemaBound struct {
	value     float64
	exclusive bool
	set       bool
}

// stricter reports whether a is a stricter bound of its kind than b, both set.
func (a schemaBound) stricter(b schemaBound, lower bool) bool {
	if a.value != b.value {
		return (a.value > b.value) == lower
	}
	return a.exclusive && !b.exclusive
}

// String describes the bound for a message.
func (a schemaBound) String() string {
	if !a.set {
		return "unset"
	}
	text := strconv.FormatFloat(a.value, 'g', -1, 64)
	if a.exclusive {
		text += " (exclusive)"
	}
	return text
}

// bound gathers the strictest of one kind of bound over every part.
func (v schemaView) bound(lower bool, read func(*Schema) (float64, bool, bool)) schemaBound {
	var out schemaBound
	for _, part := range v.parts {
		value, exclusive, set := read(part)
		if !set {
			continue
		}
		next := schemaBound{value: value, exclusive: exclusive, set: true}
		if !out.set || next.stricter(out, lower) {
			out = next
		}
	}
	return out
}

// floatBound reads a pair of inclusive and exclusive number keywords as one
// bound, the stricter of the two when both are set.
func floatBound(inclusive, exclusive *float64, lower bool) (float64, bool, bool) {
	switch {
	case inclusive == nil && exclusive == nil:
		return 0, false, false
	case inclusive == nil:
		return *exclusive, true, true
	case exclusive == nil:
		return *inclusive, false, true
	}
	a := schemaBound{value: *inclusive, set: true}
	b := schemaBound{value: *exclusive, exclusive: true, set: true}
	if a.stricter(b, lower) {
		return a.value, false, true
	}
	return b.value, true, true
}

// intBound reads an integer keyword as a bound.
func intBound(n *int) (float64, bool, bool) {
	if n == nil {
		return 0, false, false
	}
	return float64(*n), false, true
}

// boundKinds lists every bound, how to read it and how a message names it.
var boundKinds = []struct {
	kind, label string
	lower       bool
	read        func(*Schema) (float64, bool, bool)
}{
	{"minimum", "the minimum", true, func(s *Schema) (float64, bool, bool) { return floatBound(s.Minimum, s.ExclusiveMinimum, true) }},
	{"maximum", "the maximum", false, func(s *Schema) (float64, bool, bool) { return floatBound(s.Maximum, s.ExclusiveMaximum, false) }},
	{"min-length", "the minimum length", true, func(s *Schema) (float64, bool, bool) { return intBound(s.MinLength) }},
	{"max-length", "the maximum length", false, func(s *Schema) (float64, bool, bool) { return intBound(s.MaxLength) }},
	{"min-items", "the minimum number of items", true, func(s *Schema) (float64, bool, bool) { return intBound(s.MinItems) }},
	{"max-items", "the maximum number of items", false, func(s *Schema) (float64, bool, bool) { return intBound(s.MaxItems) }},
}

// multiples gathers every multipleOf over the parts, distinct.
func (v schemaView) multiples() []float64 {
	var out []float64
	for _, part := range v.parts {
		if part.MultipleOf != nil {
			out = append(out, *part.MultipleOf)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// members gathers every member the parts describe or require, each with the
// schemas the parts give it; a member only required has none.
func (v schemaView) members() map[string][]*Schema {
	out := map[string][]*Schema{}
	for _, part := range v.parts {
		// The order members are met in does not matter: each gathers its
		// schemas in the order of the parts, and the caller sorts the names.
		for name, property := range part.Properties {
			if property != nil {
				out[name] = append(out[name], property)
			} else if _, known := out[name]; !known {
				out[name] = nil
			}
		}
		for _, name := range part.Required {
			if _, known := out[name]; !known {
				out[name] = nil
			}
		}
	}
	return out
}

// required gathers the members any part requires.
func (v schemaView) required() map[string]bool {
	out := map[string]bool{}
	for _, part := range v.parts {
		for _, name := range part.Required {
			out[name] = true
		}
	}
	return out
}

// closed reports whether some part refuses members it does not name.
func (v schemaView) closed() bool {
	return slices.ContainsFunc(v.parts, func(part *Schema) bool { return part.AdditionalProperties == false })
}

// additional gathers the schemas the parts hold members they do not name to.
func (v schemaView) additional() []*Schema {
	var out []*Schema
	for _, part := range v.parts {
		if values := additionalSchema(part.AdditionalProperties); values != nil {
			out = append(out, values)
		}
	}
	return out
}

// additionalSchema reads additionalProperties as a schema, which it is for a
// map; false and true, and a value a Document built in Go may hold that is
// neither, are not.
func additionalSchema(ap any) *Schema {
	switch v := ap.(type) {
	case *Schema:
		return v
	case Schema:
		return &v
	}
	return nil
}

// items gathers the schemas the parts give the elements of an array.
func (v schemaView) items() []*Schema {
	var out []*Schema
	for _, part := range v.parts {
		if part.Items != nil {
			out = append(out, part.Items)
		}
	}
	return out
}

// compareViews compares what two positions hold.
func (c *docComparer) compareViews(location string, old, cur schemaView, dir apiDirection, textual bool) {
	c.compareNullable(location, old.nullable, cur.nullable, dir)
	c.compareAnnotations(location, old.parts, cur.parts, dir)
	if c.compareTypes(location, old.types(textual), cur.types(textual), dir) {
		// The types have nothing in common, so no value either side admits is
		// admitted by the other, and what the keywords for those types say
		// would only restate the one change that matters.
		return
	}
	c.compareEnums(location, old, cur, dir)
	c.compareFormats(location, old.strings(func(s *Schema) string { return s.Format }), cur.strings(func(s *Schema) string { return s.Format }), dir)
	c.comparePatterns(location, old.strings(func(s *Schema) string { return s.Pattern }), cur.strings(func(s *Schema) string { return s.Pattern }), dir)
	oldEncoding := old.strings(func(s *Schema) string { return s.ContentEncoding })
	curEncoding := cur.strings(func(s *Schema) string { return s.ContentEncoding })
	if !slices.Equal(oldEncoding, curEncoding) {
		c.add(Breaking, dir.prefix()+"content-encoding-changed", location+"/contentEncoding",
			fmt.Sprintf("%sthe content encoding is now %s instead of %s.", dir.where(), orNone(curEncoding), orNone(oldEncoding)))
	}
	for _, b := range boundKinds {
		c.compareBound(location, b.kind, b.label, old.bound(b.lower, b.read), cur.bound(b.lower, b.read), b.lower, dir)
	}
	c.compareMultiples(location, old.multiples(), cur.multiples(), dir)
	oldUnique := slices.ContainsFunc(old.parts, func(s *Schema) bool { return s.UniqueItems })
	curUnique := slices.ContainsFunc(cur.parts, func(s *Schema) bool { return s.UniqueItems })
	switch {
	case !oldUnique && curUnique:
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"unique-items-added", location+"/uniqueItems",
			dir.where()+"the items must now be unique.")
	case oldUnique && !curUnique:
		c.add(dir.pick(Compatible, PossiblyBreaking), dir.prefix()+"unique-items-removed", location+"/uniqueItems",
			dir.where()+"the items no longer have to be unique.")
	}
	c.compareObjects(location, old, cur, dir, textual)
	if oldItems, curItems := old.items(), cur.items(); len(oldItems) > 0 || len(curItems) > 0 {
		c.compareParts(location+"/items", oldItems, curItems, dir, textual)
	}
	c.compareUnions(location, old.unions, cur.unions, dir, textual)
}

// orNone quotes a list of values for a message, or says there are none.
func orNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return quotedList(values)
}

// compareNullable compares whether null is admitted.
func (c *docComparer) compareNullable(location string, old, cur bool, dir apiDirection) {
	switch {
	case !old && cur:
		c.add(dir.pick(Compatible, Breaking), dir.prefix()+"nullable-added", location, dir.where()+"null is now allowed here.")
	case old && !cur:
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"nullable-removed", location, dir.where()+"null is no longer allowed here.")
	}
}

// compareAnnotations compares the default a position gives and whether it is
// deprecated. A default changes what the server assumes of a request that
// leaves the value out, which a client may have relied on; in a response it is
// documentation.
func (c *docComparer) compareAnnotations(location string, old, cur []*Schema, dir apiDirection) {
	oldDefault, oldHas := c.firstDefault(old)
	curDefault, curHas := c.firstDefault(cur)
	severity := dir.pick(PossiblyBreaking, Compatible)
	switch {
	case !oldHas && curHas:
		c.add(severity, dir.prefix()+"default-added", location+"/default", fmt.Sprintf("%sthe default is now %s.", dir.where(), curDefault))
	case oldHas && !curHas:
		c.add(severity, dir.prefix()+"default-removed", location+"/default", fmt.Sprintf("%sthe default %s is gone.", dir.where(), oldDefault))
	case oldHas && oldDefault != curDefault:
		c.add(severity, dir.prefix()+"default-changed", location+"/default",
			fmt.Sprintf("%sthe default is now %s instead of %s.", dir.where(), curDefault, oldDefault))
	}
	oldDeprecated, curDeprecated := anyDeprecated(old), anyDeprecated(cur)
	switch {
	case !oldDeprecated && curDeprecated:
		c.add(Compatible, dir.prefix()+"deprecated-added", location, dir.where()+"the value is now deprecated.")
	case oldDeprecated && !curDeprecated:
		c.add(Compatible, dir.prefix()+"deprecated-removed", location, dir.where()+"the value is no longer deprecated.")
	}
}

// firstDefault returns the first default the parts give, as canonical JSON.
func (c *docComparer) firstDefault(parts []*Schema) (string, bool) {
	for _, part := range parts {
		if part.Default != nil {
			return c.facts(part).def, true
		}
	}
	return "", false
}

// anyDeprecated reports whether any part is deprecated.
func anyDeprecated(parts []*Schema) bool {
	return slices.ContainsFunc(parts, func(s *Schema) bool { return s.Deprecated })
}

// compareTypes compares the types admitted, reporting whether they have
// nothing in common.
func (c *docComparer) compareTypes(location string, old, cur jsonTypes, dir apiDirection) bool {
	added, removed := cur&^old, old&^cur
	if added == 0 && removed == 0 {
		return false
	}
	kind, severity := "type-changed", Breaking
	switch {
	case removed == 0:
		kind, severity = "type-widened", dir.pick(Compatible, Breaking)
	case added == 0:
		kind, severity = "type-narrowed", dir.pick(Breaking, Compatible)
	}
	c.add(severity, dir.prefix()+kind, location+"/type",
		fmt.Sprintf("%sthe type is now %s instead of %s.", dir.where(), cur, old))
	return old&cur == 0 && old != 0 && cur != 0
}

// compareEnums compares the values a position is limited to.
func (c *docComparer) compareEnums(location string, old, cur schemaView, dir apiDirection) {
	oldValues, oldLimited := c.enum(old)
	curValues, curLimited := c.enum(cur)
	location += "/enum"
	switch {
	case !oldLimited && !curLimited:
		return
	case !oldLimited:
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"enum-added", location,
			fmt.Sprintf("%sthe value is now limited to %s.", dir.where(), valueList(slices.Sorted(maps.Keys(curValues)))))
		return
	case !curLimited:
		c.add(dir.pick(Compatible, PossiblyBreaking), dir.prefix()+"enum-removed", location,
			dir.where()+"the value is no longer limited to a list.")
		return
	}
	removed, added := setDifference(slices.Collect(maps.Keys(oldValues)), slices.Collect(maps.Keys(curValues)))
	if len(added) > 0 {
		c.add(dir.pick(Compatible, PossiblyBreaking), dir.prefix()+"enum-values-added", location,
			fmt.Sprintf("%sthe allowed values now include %s.", dir.where(), valueList(added)))
	}
	if len(removed) > 0 {
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"enum-values-removed", location,
			fmt.Sprintf("%sthe allowed values no longer include %s.", dir.where(), valueList(removed)))
	}
}

// valueList renders canonical JSON values for a message, shortened after a
// few.
func valueList(values []string) string {
	if len(values) == 0 {
		return "no value at all"
	}
	return shortList(values, false)
}

// formatWidens reports whether every value of one format is a value of
// another, as an int32 is an int64. A client generator picks the width of a
// number by its format, so the difference is one a client feels.
func formatWidens(from, to string) bool {
	return (from == "int32" && to == "int64") || (from == "float" && to == "double")
}

// compareFormats compares the formats a value is held to.
func (c *docComparer) compareFormats(location string, old, cur []string, dir apiDirection) {
	removed, added := setDifference(old, cur)
	location += "/format"
	switch {
	case len(removed) == 0 && len(added) == 0:
	case len(removed) == 1 && len(added) == 1 && formatWidens(removed[0], added[0]):
		c.add(dir.pick(Compatible, Breaking), dir.prefix()+"format-widened", location,
			fmt.Sprintf("%sthe format is now %q instead of %q.", dir.where(), added[0], removed[0]))
	case len(removed) == 1 && len(added) == 1 && formatWidens(added[0], removed[0]):
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"format-narrowed", location,
			fmt.Sprintf("%sthe format is now %q instead of %q.", dir.where(), added[0], removed[0]))
	case len(removed) > 0 && len(added) > 0:
		c.add(Breaking, dir.prefix()+"format-changed", location,
			fmt.Sprintf("%sthe format is now %s instead of %s.", dir.where(), quotedList(added), quotedList(removed)))
	case len(added) > 0:
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"format-added", location,
			fmt.Sprintf("%sthe value now has the format %s.", dir.where(), quotedList(added)))
	default:
		c.add(dir.pick(Compatible, Breaking), dir.prefix()+"format-removed", location,
			fmt.Sprintf("%sthe value no longer has the format %s.", dir.where(), quotedList(removed)))
	}
}

// comparePatterns compares the patterns a string is held to. Whether one
// pattern admits everything another does cannot be decided in general, so a
// changed pattern is taken to narrow a request and to possibly widen a
// response.
func (c *docComparer) comparePatterns(location string, old, cur []string, dir apiDirection) {
	removed, added := setDifference(old, cur)
	location += "/pattern"
	switch {
	case len(removed) == 0 && len(added) == 0:
	case len(removed) > 0 && len(added) > 0:
		c.add(dir.pick(Breaking, PossiblyBreaking), dir.prefix()+"pattern-changed", location,
			fmt.Sprintf("%sthe value must now match %s instead of %s.", dir.where(), quotedList(added), quotedList(removed)))
	case len(added) > 0:
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"pattern-added", location,
			fmt.Sprintf("%sthe value must now match %s.", dir.where(), quotedList(added)))
	default:
		c.add(dir.pick(Compatible, PossiblyBreaking), dir.prefix()+"pattern-removed", location,
			fmt.Sprintf("%sthe value no longer has to match %s.", dir.where(), quotedList(removed)))
	}
}

// compareBound compares one kind of bound. A response bound that was relaxed
// is possibly breaking: only a client that validates what it reads refuses
// the values it now admits.
func (c *docComparer) compareBound(location, kind, label string, old, cur schemaBound, lower bool, dir apiDirection) {
	tightened := cur.set && (!old.set || cur.stricter(old, lower))
	relaxed := old.set && (!cur.set || old.stricter(cur, lower))
	message := fmt.Sprintf("%s%s is now %s instead of %s.", dir.where(), label, cur, old)
	switch {
	case tightened:
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+kind+"-tightened", location+"/"+boundKeyword(kind), message)
	case relaxed:
		c.add(dir.pick(Compatible, PossiblyBreaking), dir.prefix()+kind+"-relaxed", location+"/"+boundKeyword(kind), message)
	}
}

// boundKeyword is the keyword a bound is written with, for its location.
func boundKeyword(kind string) string {
	switch kind {
	case "min-length":
		return "minLength"
	case "max-length":
		return "maxLength"
	case "min-items":
		return "minItems"
	case "max-items":
		return "maxItems"
	}
	return kind
}

// isMultipleOf reports whether a is a whole multiple of b, allowing for the
// rounding of a decimal fraction such as 0.3 over 0.1.
func isMultipleOf(a, b float64) bool {
	if b == 0 {
		return false
	}
	ratio := a / b
	if math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		return false
	}
	return math.Abs(ratio-math.Round(ratio)) <= 1e-9*math.Max(1, math.Abs(ratio))
}

// compareMultiples compares the multiples a number is held to. A new multiple
// narrows only when no old one already implied it: a multiple of 4 is always
// a multiple of 2.
func (c *docComparer) compareMultiples(location string, old, cur []float64, dir apiDirection) {
	implied := func(m float64, by []float64) bool {
		return slices.ContainsFunc(by, func(o float64) bool { return isMultipleOf(o, m) })
	}
	// Every multiple of one side is checked against every multiple of the
	// other, which a schema holding a great many of them pays for.
	if !c.spend(len(old) * len(cur)) {
		return
	}
	location += "/multipleOf"
	for _, m := range cur {
		if !implied(m, old) {
			c.add(dir.pick(Breaking, Compatible), dir.prefix()+"multiple-of-tightened", location,
				fmt.Sprintf("%sthe value must now be a multiple of %s.", dir.where(), strconv.FormatFloat(m, 'g', -1, 64)))
			break
		}
	}
	for _, m := range old {
		if !implied(m, cur) {
			c.add(dir.pick(Compatible, PossiblyBreaking), dir.prefix()+"multiple-of-relaxed", location,
				fmt.Sprintf("%sthe value no longer has to be a multiple of %s.", dir.where(), strconv.FormatFloat(m, 'g', -1, 64)))
			break
		}
	}
}

// compareObjects compares the members of two objects, and what they hold
// members they do not name to.
func (c *docComparer) compareObjects(location string, old, cur schemaView, dir apiDirection, textual bool) {
	oldMembers, curMembers := old.members(), cur.members()
	oldRequired, curRequired := old.required(), cur.required()
	oldClosed, curClosed := old.closed(), cur.closed()
	names := slices.Sorted(maps.Keys(oldMembers))
	for name := range curMembers {
		if _, known := oldMembers[name]; !known {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if !c.step(location) {
			return
		}
		at := location + "/properties/" + pointerToken(name)
		oldParts, inOld := oldMembers[name]
		curParts, inCur := curMembers[name]
		switch {
		case !inOld:
			c.addedMember(at, name, curRequired[name], oldClosed, dir)
		case !inCur:
			c.removedMember(at, name, oldRequired[name], curClosed, anyDeprecated(oldParts), dir)
		default:
			switch {
			case !oldRequired[name] && curRequired[name]:
				c.add(dir.pick(Breaking, Compatible), dir.prefix()+"property-became-required", at,
					fmt.Sprintf("%sthe member %q is now required.", dir.where(), name))
			case oldRequired[name] && !curRequired[name]:
				c.add(dir.pick(Compatible, Breaking), dir.prefix()+"property-became-optional", at,
					fmt.Sprintf("%sthe member %q is now optional.", dir.where(), name))
			}
			c.compareParts(at, oldParts, curParts, dir, textual)
		}
	}
	switch {
	case oldClosed && !curClosed:
		c.add(dir.pick(Compatible, PossiblyBreaking), dir.prefix()+"unknown-members-allowed", location+"/additionalProperties",
			dir.where()+"members the schema does not name are now allowed.")
	case !oldClosed && curClosed:
		c.add(dir.pick(Breaking, Compatible), dir.prefix()+"unknown-members-refused", location+"/additionalProperties",
			dir.where()+"members the schema does not name are now refused.")
	case !oldClosed:
		if oldValues, curValues := old.additional(), cur.additional(); len(oldValues) > 0 || len(curValues) > 0 {
			c.compareParts(location+"/additionalProperties", oldValues, curValues, dir, textual)
		}
	}
}

// addedMember reports a member only the newer object has. A request member is
// breaking when it is required, since an older client does not send it; a
// response member is not, since a response is open, unless the object said it
// held no other members.
func (c *docComparer) addedMember(location, name string, required, wasClosed bool, dir apiDirection) {
	kind := dir.prefix() + "property-added"
	switch {
	case dir == towardServer && required:
		c.add(Breaking, kind, location,
			fmt.Sprintf("%sthe member %q is new and required, which a client written against the old document does not send.", dir.where(), name))
	case dir == towardServer:
		c.add(Compatible, kind, location, fmt.Sprintf("%sthe member %q is new and optional.", dir.where(), name))
	case wasClosed:
		c.add(PossiblyBreaking, kind, location,
			fmt.Sprintf("%sthe member %q is new, in an object that said it held no other members.", dir.where(), name))
	default:
		c.add(Compatible, kind, location, fmt.Sprintf("%sthe member %q is new.", dir.where(), name))
	}
}

// removedMember reports a member only the older object has. In a request it is
// breaking when the object refuses members it does not know, as every object
// Muzak reads a request into does, and possibly breaking when what a client
// sends in it is only ignored. In a response it is breaking when clients could
// rely on it being there. A member that was deprecated is possibly breaking
// either way, since clients were told to stop using it.
func (c *docComparer) removedMember(location, name string, wasRequired, nowClosed, deprecated bool, dir apiDirection) {
	severity, why := PossiblyBreaking, ""
	switch {
	case dir == towardServer && nowClosed:
		severity, why = Breaking, ", so a client that still sends it is refused"
	case dir == towardServer:
		why = ", so what a client still sends in it is ignored"
	case wasRequired:
		severity, why = Breaking, ", though clients could rely on it"
	}
	if deprecated {
		severity, why = PossiblyBreaking, why+", but it was deprecated"
	}
	c.add(severity, dir.prefix()+"property-removed", location, fmt.Sprintf("%sthe member %q is gone%s.", dir.where(), name, why))
}

// compareUnions compares the choices between alternatives two positions hold,
// alternative by alternative in the order they are written. A choice is a
// conjunct like any other, so one that appears narrows the value and one that
// disappears widens it; within one, an alternative that appears widens it.
func (c *docComparer) compareUnions(location string, old, cur []*Schema, dir apiDirection, textual bool) {
	location += "/anyOf"
	for i := range max(len(old), len(cur)) {
		switch {
		case i >= len(cur):
			c.add(dir.pick(Compatible, Breaking), dir.prefix()+"any-of-removed", location,
				dir.where()+"the value no longer has to match one of several alternatives.")
			continue
		case i >= len(old):
			c.add(dir.pick(Breaking, Compatible), dir.prefix()+"any-of-added", location,
				dir.where()+"the value must now also match one of several alternatives.")
			continue
		}
		oldAlternatives, curAlternatives := old[i].AnyOf, cur[i].AnyOf
		for j := range max(len(oldAlternatives), len(curAlternatives)) {
			if !c.step(location) {
				return
			}
			at := location + "/" + strconv.Itoa(j)
			switch {
			case j >= len(curAlternatives):
				c.add(dir.pick(Breaking, Compatible), dir.prefix()+"any-of-alternative-removed", at,
					dir.where()+"an alternative the value could match is gone.")
			case j >= len(oldAlternatives):
				c.add(dir.pick(Compatible, Breaking), dir.prefix()+"any-of-alternative-added", at,
					dir.where()+"the value may now match a new alternative.")
			default:
				c.compareParts(at, oneSchema(oldAlternatives[j]), oneSchema(curAlternatives[j]), dir, textual)
			}
		}
	}
}

// canonicalValue renders a value of an enum or a default as canonical JSON, so
// that a value a Go program put in a Document compares equal to the same value
// read back from JSON: the integer 3 and the float 3, a named string type and
// a string. Numbers are read back as JSON reads them, so two integers JSON
// cannot tell apart are not told apart here either.
func canonicalValue(v any) string {
	switch v.(type) {
	case string, bool, float64:
		if data, err := json.Marshal(v); err == nil {
			return string(data)
		}
	}
	data, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		// A value with no JSON form cannot appear in a served document, and
		// is told apart from every other by its Go form instead.
		return fmt.Sprintf("%T(%v)", v, v)
	}
	var generic any
	if json.Unmarshal(data, &generic) == nil {
		if again, err := json.Marshal(generic, json.Deterministic(true)); err == nil {
			return string(again)
		}
	}
	return string(data)
}

// shortList renders values for a message, quoted when asked, and shortened
// after a few so that a message stays one readable sentence.
func shortList(values []string, quote bool) string {
	const shown = 5
	out := make([]string, 0, min(len(values), shown))
	for i, v := range values {
		if i == shown {
			break
		}
		if quote {
			v = strconv.Quote(v)
		}
		out = append(out, v)
	}
	text := strings.Join(out, ", ")
	if len(values) > shown {
		text += fmt.Sprintf(" and %d more", len(values)-shown)
	}
	return text
}
