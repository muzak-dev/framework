package muzak

import (
	"cmp"
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
)

// jsonMember is one member of the object a struct encodes as, with the Go field
// it is read into and written from, as encoding/json/v2 decides them.
//
// The decoder, the generated document and the names validation failures are
// reported under all have to agree about which members a body has and what
// each is called, and the decoder is the one that is right. Each used to work
// it out for itself, and they parted ways wherever json/v2 has a rule of its
// own: a field an outer one of the same name shadows, the embed option, a
// number written as a string. [jsonMembers] follows json/v2's own resolution,
// so the others ask it instead of guessing.
type jsonMember struct {
	jsonTag
	// field is the Go field, and index the path to it from the outer struct.
	field reflect.StructField
	index []int
	// offset is where the field sits within the outer struct. It means nothing
	// when indirect is set: a field promoted through an embedded pointer lives
	// wherever that pointer leads, and is absent while the pointer is nil.
	offset   uintptr
	indirect bool
}

// jsonTag is what a field's json tag says, read the way json/v2 reads it.
type jsonTag struct {
	name    string
	hasName bool
	// omitzero and omitempty leave the member out of what is written, under
	// conditions [jsonMember.mayBeAbsent] spells out.
	omitzero  bool
	omitempty bool
	// stringify is the string option, which writes a number as a string and
	// reads it back only from one.
	stringify bool
	// embed promotes the members of the field's type into the object holding
	// it, which is what a Go embedded field does by default.
	embed bool
}

// jsonObject is the object a struct type encodes as.
type jsonObject struct {
	members []jsonMember
	// fallback is the embedded map, or jsontext.Value, that json/v2 reads every
	// member the struct does not name into, or nil when there is none.
	fallback *jsonMember
}

var (
	// jsontextValueType is the raw value json/v2 can collect unknown members
	// in.
	jsontextValueType = reflect.TypeFor[jsontext.Value]()
	// jsonMarshalers are the methods a type writes its own JSON or text with,
	// any of which json/v2 prefers to the type's shape.
	jsonMarshalers = []reflect.Type{
		reflect.TypeFor[json.Marshaler](),
		reflect.TypeFor[json.MarshalerTo](),
		reflect.TypeFor[encoding.TextMarshaler](),
		reflect.TypeFor[encoding.TextAppender](),
	}
	// jsonUnmarshalers are the methods a type reads its own JSON or text with.
	jsonUnmarshalers = []reflect.Type{
		reflect.TypeFor[json.Unmarshaler](),
		reflect.TypeFor[json.UnmarshalerFrom](),
		reflect.TypeFor[encoding.TextUnmarshaler](),
	}
)

// parseJSONTag reads a field's json tag, reporting whether json/v2 ignores the
// field altogether, which it does for one tagged "-" and for an unexported one
// that is not embedded.
func parseJSONTag(field reflect.StructField) (tag jsonTag, ignored bool) {
	raw := field.Tag.Get(tagJSON)
	if raw == "-" || (!field.IsExported() && !field.Anonymous) {
		return jsonTag{}, true
	}
	tag.name = field.Name
	options := raw
	if raw != "" && !strings.HasPrefix(raw, ",") {
		// json/v2 refuses a type whose tag names a member in any form but
		// the plain one up to the first comma, so that is the only one read.
		tag.name, options, _ = strings.Cut(raw, ",")
		tag.hasName = true
	}
	for option := range strings.SplitSeq(options, ",") {
		switch option {
		case "omitzero":
			tag.omitzero = true
		case "omitempty":
			tag.omitempty = true
		case "string":
			tag.stringify = true
		case "embed":
			tag.embed = true
		}
	}
	return tag, false
}

// jsonMembers lists the members of the object a struct type encodes as.
//
// It is json/v2's own resolution. Embedded structs, and fields tagged with the
// embed option, are searched breadth first, so the shallower of two fields of
// one name is the member; of two at the same depth, the one whose tag names it
// is; and two that still tie are both dropped, so neither is the member and
// json/v2 neither writes nor reads that name. A struct json/v2 refuses to
// encode at all, as one whose embedded field carries other options is, is
// listed the way json/v2 resolves it before it refuses.
func jsonMembers(t reflect.Type) jsonObject {
	type queued struct {
		typ      reflect.Type
		index    []int
		offset   uintptr
		indirect bool
		// visit is false for a struct already met, whose embedded structs are
		// not searched again, which is what ends a type that embeds itself.
		visit bool
	}
	queue := []queued{{typ: t, visit: true}}
	seen := map[reflect.Type]bool{t: true}
	var all, fallbacks []jsonMember
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for i := range next.typ.NumField() {
			field := next.typ.Field(i)
			tag, ignored := parseJSONTag(field)
			if ignored {
				continue
			}
			member := jsonMember{
				jsonTag:  tag,
				field:    field,
				index:    append(slices.Clip(next.index), i),
				offset:   next.offset + field.Offset,
				indirect: next.indirect,
			}
			target := embeddedType(field.Type)
			if field.Anonymous && !member.hasName && target.Kind() == reflect.Struct {
				member.embed = true
			}
			if member.embed {
				if member.hasName {
					// The embed option takes no name, and json/v2 treats a
					// field given both as an ordinary member.
					member.embed = false
				} else {
					member.jsonTag = jsonTag{name: member.name, embed: true}
				}
			}
			if member.embed {
				switch {
				case target.Kind() == reflect.Struct:
					if next.visit {
						queue = append(queue, queued{
							typ:      target,
							index:    member.index,
							offset:   member.offset,
							indirect: next.indirect || field.Type.Kind() == reflect.Pointer,
							visit:    !seen[target],
						})
					}
					seen[target] = true
					continue
				case !field.IsExported():
					continue
				case target == jsontextValueType || (target.Kind() == reflect.Map && target.Key().Kind() == reflect.String):
					fallbacks = append(fallbacks, member)
					continue
				}
			}
			// An unexported field is written only when it is an embedded struct
			// given a name of its own, which makes it an ordinary member.
			if !field.IsExported() && (!field.Anonymous || target.Kind() != reflect.Struct) {
				continue
			}
			all = append(all, member)
		}
	}

	slices.SortStableFunc(all, func(x, y jsonMember) int {
		return cmp.Or(
			strings.Compare(x.name, y.name),
			cmp.Compare(len(x.index), len(y.index)),
			cmp.Compare(boolRank(!x.hasName), boolRank(!y.hasName)))
	})
	var object jsonObject
	for len(all) > 0 {
		n := 1
		for n < len(all) && all[n].name == all[0].name {
			n++
		}
		if n == 1 || len(all[0].index) != len(all[1].index) || all[0].hasName != all[1].hasName {
			object.members = append(object.members, all[0])
		}
		all = all[n:]
	}
	slices.SortFunc(object.members, func(x, y jsonMember) int { return slices.Compare(x.index, y.index) })
	if n := len(fallbacks); n == 1 || (n > 1 && len(fallbacks[0].index) != len(fallbacks[1].index)) {
		object.fallback = &fallbacks[0]
	}
	return object
}

// embeddedType looks through the one unnamed pointer an embedded field may be.
func embeddedType(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer && t.Name() == "" {
		return t.Elem()
	}
	return t
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mayBeAbsent reports whether a response can leave the member out.
//
// omitzero leaves out a zero value, which every type has. omitempty leaves out
// what encodes as null, "", {} or [], which a number, a bool, an array of at
// least one element and a time never do, so a member of one of those is
// always written whatever its tag says. A member promoted through an embedded
// pointer is absent while the pointer is nil. A pointer, a member with a
// default and one marked required:"false" are described as optional as they
// always have been.
func (m jsonMember) mayBeAbsent() bool {
	t := m.field.Type
	switch {
	case m.indirect, m.omitzero, t.Kind() == reflect.Pointer, m.field.Tag.Get(tagRequired) == "false":
		return true
	case m.omitempty && canEncodeEmpty(t):
		return true
	}
	_, defaulted := m.field.Tag.Lookup(tagDefault)
	return defaulted
}

// canEncodeEmpty reports whether a value of the type can encode as what
// omitempty leaves out.
func canEncodeEmpty(t reflect.Type) bool {
	if t == timeType {
		// json/v2 writes a time as RFC 3339 text, which is never empty.
		return false
	}
	if encodesItself(t) {
		return true
	}
	switch t.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return false
	case reflect.Array:
		return t.Len() == 0
	}
	return true
}

// encodesItself reports whether a type writes its own JSON or text, so what it
// writes cannot be known from its shape.
func encodesItself(t reflect.Type) bool {
	return implementsAny(t, jsonMarshalers)
}

// decodesItself reports whether a type reads its own JSON or text, so what it
// accepts cannot be known from its shape.
func decodesItself(t reflect.Type) bool {
	return implementsAny(t, jsonUnmarshalers)
}

// implementsAny reports whether a type, or a pointer to it, implements any of
// the given interfaces.
func implementsAny(t reflect.Type, interfaces []reflect.Type) bool {
	for _, i := range interfaces {
		if t.Implements(i) || reflect.PointerTo(t).Implements(i) {
			return true
		}
	}
	return false
}

// isRawByte reports whether a type is byte itself, which json/v2 encodes a
// slice or array of as base64. A named byte type is not: a slice of one is an
// array of numbers.
func isRawByte(t reflect.Type) bool {
	return t.Kind() == reflect.Uint8 && t.PkgPath() == ""
}
