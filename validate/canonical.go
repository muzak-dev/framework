package validate

import (
	"encoding/binary"
	"math"
	"reflect"
	"slices"
	"time"
)

// Tags that open a variable-shaped part of a canonical key. A part whose shape
// is fixed by its type, an integer or a boolean, needs none; one that can be
// absent, or can be one of several types, says which it is, so that no two
// different values write the same bytes.
const (
	tagAbsent  byte = iota // a nil pointer, slice, map, interface or func
	tagPresent             // the same, and not nil
	tagBackref             // a pointer met again inside itself
	tagInstant             // a time.Time
	tagNaN                 // a value that equals nothing, not even itself
)

var timeType = reflect.TypeFor[time.Time]()

// canonicalizer turns a value into bytes that are equal for two values exactly
// when [reflect.DeepEqual] calls them equal, so that a collection of values that
// cannot be compared with == can still be searched for repeats with a hash set
// instead of pair by pair.
//
// DeepEqual is the reference rather than a shortcut to it, so the encoding
// reads what DeepEqual reads. A pointer is what it points at and a nil one is
// not an empty one. A slice or map is its contents, a nil one is not an empty
// one, and a map is the same map in whatever order its entries were written. An
// interface is its dynamic type and value. Every field of a struct counts,
// unexported and blank ones too. A NaN, and a func that is not nil, equal
// nothing, so each gets a mark of its own that no other value shares. Two
// exceptions follow the rest of [SliceRules.Unique]: a time.Time is the instant
// it names, wherever it sits, and -0 is 0.
//
// A structure that points back into itself is written once, with the point of
// return marked, so a cycle ends the encoding instead of extending it forever.
//
// A canonicalizer keeps what it learns between values, which is what makes the
// bytes of one comparable with the bytes of another, and so serves one search
// on one goroutine.
type canonicalizer struct {
	buf   []byte
	types map[reflect.Type]uint64
	open  map[openKey]int
	depth int
	marks uint64
}

// openKey names something being encoded that another part of the value can
// point back to. The type is part of it because a pointer to a struct and a
// pointer to its first field share an address.
type openKey struct {
	addr uintptr
	typ  reflect.Type
	len  int
}

// key returns the map key for one element.
func (c *canonicalizer) key(e any) any {
	c.buf = c.buf[:0]
	c.depth = 0
	v := reflect.ValueOf(e)
	if !v.IsValid() {
		c.buf = append(c.buf, tagAbsent)
		return string(c.buf)
	}
	// The dynamic type leads, because an element that is an interface says
	// which type it holds: int8(1) and uint8(1) are not the same element.
	c.buf = append(c.buf, tagPresent)
	c.typeID(v.Type())
	c.encode(v)
	return string(c.buf)
}

// typeID writes a number for a type that is the same wherever the type is met
// during this search.
func (c *canonicalizer) typeID(t reflect.Type) {
	if c.types == nil {
		c.types = make(map[reflect.Type]uint64)
	}
	id, ok := c.types[t]
	if !ok {
		id = uint64(len(c.types))
		c.types[t] = id
	}
	c.buf = binary.AppendUvarint(c.buf, id)
}

// mark writes a value that no other value can equal.
func (c *canonicalizer) mark() {
	c.marks++
	c.buf = append(c.buf, tagNaN)
	c.buf = binary.AppendUvarint(c.buf, c.marks)
}

// enter records that a value is being encoded and reports how far back it
// began, when it is already being encoded further up.
func (c *canonicalizer) enter(k openKey) (back int, cycle bool) {
	if c.open == nil {
		c.open = make(map[openKey]int)
	}
	if at, found := c.open[k]; found {
		return c.depth - at, true
	}
	c.open[k] = c.depth
	return 0, false
}

func (c *canonicalizer) leave(k openKey) { delete(c.open, k) }

func (c *canonicalizer) encode(v reflect.Value) {
	c.depth++
	defer func() { c.depth-- }()

	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			c.buf = append(c.buf, 1)
		} else {
			c.buf = append(c.buf, 0)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		c.buf = binary.AppendVarint(c.buf, v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		c.buf = binary.AppendUvarint(c.buf, v.Uint())
	case reflect.Float32, reflect.Float64:
		c.float(v.Float())
	case reflect.Complex64, reflect.Complex128:
		z := v.Complex()
		c.float(real(z))
		c.float(imag(z))
	case reflect.String:
		s := v.String()
		c.buf = binary.AppendUvarint(c.buf, uint64(len(s)))
		c.buf = append(c.buf, s...)
	case reflect.Array:
		for i := range v.Len() {
			c.encode(v.Index(i))
		}
	case reflect.Slice:
		c.slice(v)
	case reflect.Map:
		c.mapping(v)
	case reflect.Pointer:
		c.pointer(v)
	case reflect.Interface:
		if v.IsNil() {
			c.buf = append(c.buf, tagAbsent)
			return
		}
		c.buf = append(c.buf, tagPresent)
		c.typeID(v.Elem().Type())
		c.encode(v.Elem())
	case reflect.Struct:
		c.structure(v)
	case reflect.Func:
		// DeepEqual: equal only when both are nil.
		if v.IsNil() {
			c.buf = append(c.buf, tagAbsent)
			return
		}
		c.mark()
	case reflect.Chan, reflect.UnsafePointer:
		// Compared with ==, which is the address.
		c.buf = binary.AppendUvarint(c.buf, uint64(v.Pointer()))
	default:
		// coverage: every kind reflect has is handled above.
		c.mark()
	}
}

// float writes a floating-point number the way == compares it: -0 is 0, and
// NaN is not equal to anything. A number leads with a tag of its own, so that
// the mark a NaN writes cannot be read as the digits of one.
func (c *canonicalizer) float(f float64) {
	switch {
	case f != f:
		c.mark()
	case f == 0:
		c.buf = append(c.buf, tagPresent)
		c.buf = binary.BigEndian.AppendUint64(c.buf, 0)
	default:
		c.buf = append(c.buf, tagPresent)
		c.buf = binary.BigEndian.AppendUint64(c.buf, math.Float64bits(f))
	}
}

func (c *canonicalizer) slice(v reflect.Value) {
	if v.IsNil() {
		c.buf = append(c.buf, tagAbsent)
		return
	}
	c.buf = append(c.buf, tagPresent)
	n := v.Len()
	c.buf = binary.AppendVarint(c.buf, int64(n))
	if n == 0 {
		return
	}
	k := openKey{v.Pointer(), v.Type(), n}
	if back, cycle := c.enter(k); cycle {
		c.buf = append(c.buf, tagBackref)
		c.buf = binary.AppendVarint(c.buf, int64(back))
		return
	}
	defer c.leave(k)
	for i := range n {
		c.encode(v.Index(i))
	}
}

// mapping writes a map's entries in the order their own encodings sort in, so
// that two maps with the same entries write the same bytes.
func (c *canonicalizer) mapping(v reflect.Value) {
	if v.IsNil() {
		c.buf = append(c.buf, tagAbsent)
		return
	}
	c.buf = append(c.buf, tagPresent)
	c.buf = binary.AppendVarint(c.buf, int64(v.Len()))
	if v.Len() == 0 {
		return
	}
	k := openKey{v.Pointer(), v.Type(), 0}
	if back, cycle := c.enter(k); cycle {
		c.buf = append(c.buf, tagBackref)
		c.buf = binary.AppendVarint(c.buf, int64(back))
		return
	}
	defer c.leave(k)

	entries := make([]string, 0, v.Len())
	start := len(c.buf)
	for it := v.MapRange(); it.Next(); {
		c.encode(it.Key())
		c.encode(it.Value())
		entries = append(entries, string(c.buf[start:]))
		c.buf = c.buf[:start]
	}
	slices.Sort(entries)
	for _, entry := range entries {
		c.buf = append(c.buf, entry...)
	}
}

func (c *canonicalizer) pointer(v reflect.Value) {
	if v.IsNil() {
		c.buf = append(c.buf, tagAbsent)
		return
	}
	c.buf = append(c.buf, tagPresent)
	k := openKey{v.Pointer(), v.Type(), 0}
	if back, cycle := c.enter(k); cycle {
		c.buf = append(c.buf, tagBackref)
		c.buf = binary.AppendVarint(c.buf, int64(back))
		return
	}
	defer c.leave(k)
	c.encode(v.Elem())
}

func (c *canonicalizer) structure(v reflect.Value) {
	// A time.Time is compared by instant everywhere Unique compares one. It
	// can only be read as a time when it was not reached through an unexported
	// field; there it is encoded as the struct it is, which distinguishes
	// zones and is the strictest reading, never a looser one.
	if v.Type() == timeType && v.CanInterface() {
		at := v.Interface().(time.Time)
		c.buf = append(c.buf, tagInstant)
		c.buf = binary.AppendVarint(c.buf, at.Unix())
		c.buf = binary.AppendVarint(c.buf, int64(at.Nanosecond()))
		return
	}
	for i := range v.NumField() {
		c.encode(v.Field(i))
	}
}
