package muzak

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Dep is a field of a route's input type that receives a value dependency,
// which the handler reads with [Dep.Get]:
//
//	type ReadItem struct {
//		ID   string                 `path:"id"`
//		User muzak.Dep[CurrentUser]
//	}
//
//	r.Get("/items/{id}", func(ctx *muzak.Context, in ReadItem) (ItemOut, error) {
//		return ItemOut{ID: in.ID, Owner: in.User.Get().Username}, nil
//	}, muzak.Needs(GetCurrentUser))
//
// It is the declarative counterpart of [From]: the input type now says what
// the handler depends on, and a route that forgets to declare the provider is
// refused when the application is built rather than failing with a 500 on its
// first request. Every Dep[T] field must be satisfied by a provider of exactly
// type T declared on the route or on a router above it, with [Needs],
// [Acquire], [Transaction], [Singleton] or [WithSingleton]; otherwise the
// build error names the route, the field and T. A provider of *T does not
// satisfy a Dep[T], nor the other way round. When T is declared more than once
// along the chain, the field receives the innermost value, as From does, and
// under [App.Override] it receives the override's.
//
// A Dep is never read from the request. It is not a JSON body member (a body
// naming it is refused as an unknown member, or ignored under
// [AllowUnknownFields]), it cannot carry a path, query, header, cookie, form or
// file tag, and it does not appear in the OpenAPI document. It is filled after
// the dependencies have resolved and before the input is bound and validated,
// so a Validate method may read it.
//
// It must be a named, exported field at the top level of the input, or of a
// struct embedded in it by value, which is how a set of dependencies is shared
// between inputs. A Dep behind a pointer, inside a slice, an array or a map,
// or in a field of a nested struct would be left empty, so each of those is a
// build error. Inputs without a Dep pay nothing for the feature.
type Dep[T any] struct {
	value T
}

// Get returns the value the route's provider resolved for this request.
//
// Every Dep of the input a handler receives has been filled, because a route
// whose Dep has no provider is not built and a provider that fails ends the
// request before the handler runs. Outside that, Get returns T's zero value:
// in an input built by hand, and in the zero input Muzak hands a Validate
// method when it compiles the route's rules at build time, so a Validate that
// reads a Dep must cope with the zero value there.
func (d Dep[T]) Get() T { return d.value }

// fill stores a resolved value. It is reached through [depFiller], because a
// Dep's field is unexported and the binder only has a reflect.Value of it.
// The comma-ok assertion turns a nil interface value, which a provider of an
// interface type may legitimately return, into T's zero value, as [TryFrom]
// does.
func (d *Dep[T]) fill(v any) { d.value, _ = v.(T) }

// depFiller is what every *Dep[T] implements, whatever T is.
type depFiller interface{ fill(v any) }

var (
	depFillerType = reflect.TypeFor[depFiller]()
	depPkgPath    = reflect.TypeFor[Dep[struct{}]]().PkgPath()
)

// depValueType reports whether t is an instantiation of [Dep], and the type T
// it carries.
//
// The type is recognised exactly, by its package and generic name, rather than
// by the method set: a struct of the caller's own that embeds a Dep has the
// method too, and treating it as a Dep would fill the embedded field while the
// handler read the outer one.
func depValueType(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() != reflect.Struct || t.PkgPath() != depPkgPath || !strings.HasPrefix(t.Name(), "Dep[") ||
		!reflect.PointerTo(t).Implements(depFillerType) {
		return nil, false
	}
	return t.Field(0).Type, true
}

// isDepField reports whether a struct field is a [Dep], which the body plan
// and the document leave out.
func isDepField(f reflect.StructField) bool {
	_, ok := depValueType(f.Type)
	return ok
}

// depBinder is one Dep field of an input type: where it is, what it is called
// in an error, and the type of the value it receives.
type depBinder struct {
	index []int
	name  string
	typ   reflect.Type
}

// collectDep handles the field at position i of a struct being walked by
// collectFields. It reports true for a Dep field, which it records or refuses,
// so that the caller treats it as neither a parameter nor body content. For
// every other field it looks for a Dep somewhere inside the field's type,
// which would never be filled, and refuses it.
//
// A struct embedded by value is not searched here, because collectFields
// walks its fields itself and so finds a Dep in it at the level where one is
// allowed.
func (p *bindPlan) collectDep(f reflect.StructField, prefix []int, i int) (bool, error) {
	if valueType, ok := depValueType(f.Type); ok {
		switch {
		case f.Anonymous:
			return true, fmt.Errorf("field %s embeds a muzak.Dep[%s], which would promote Get onto the input; declare it as a named field, such as `User muzak.Dep[%s]`",
				f.Name, valueType, valueType)
		case !f.IsExported():
			return true, fmt.Errorf("field %s is a muzak.Dep[%s] but is unexported, so the binder cannot fill it; export the field", f.Name, valueType)
		}
		if location, declared := declaredLocation(f); declared {
			return true, fmt.Errorf("field %s is a muzak.Dep[%s], which is filled by a provider and never read from the request, but it declares a %s parameter; remove the tag",
				f.Name, valueType, location)
		}
		index := append(append([]int(nil), prefix...), i)
		p.deps = append(p.deps, depBinder{index: index, name: f.Name, typ: valueType})
		return true, nil
	}
	if f.Anonymous && f.Type.Kind() == reflect.Struct {
		if _, located := declaredLocation(f); !located {
			return false, nil
		}
	}
	if path, valueType, found := depWithin(f.Type, map[reflect.Type]bool{}); found {
		where := f.Name
		if path != "" {
			where += "." + path
		}
		return false, fmt.Errorf("field %s holds a muzak.Dep[%s] at %s, behind a pointer, inside a slice, an array or a map, or in a nested struct, where it would never be filled; "+
			"a Dep is filled only as a field of the input itself or of a struct embedded in it by value", f.Name, valueType, where)
	}
	return false, nil
}

// depWithin searches a type for a [Dep], looking through pointers, slices,
// arrays, map values and the fields of structs, exported or not. It returns
// the dotted path to the first one, empty when the type itself leads straight
// to it, and the type that Dep carries. The seen set stops a recursive type
// from being walked forever; see [locatedWithin], which this mirrors.
//
// It runs once per field when a route is compiled, never on a request.
func depWithin(t reflect.Type, seen map[reflect.Type]bool) (path string, valueType reflect.Type, found bool) {
	for {
		if valueType, ok := depValueType(t); ok {
			return "", valueType, true
		}
		if t.Kind() != reflect.Pointer && t.Kind() != reflect.Slice && t.Kind() != reflect.Array && t.Kind() != reflect.Map {
			break
		}
		if seen[t] {
			return "", nil, false
		}
		seen[t] = true
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return "", nil, false
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if path, valueType, found := depWithin(f.Type, seen); found {
			if path == "" {
				return f.Name, valueType, true
			}
			return f.Name + "." + path, valueType, true
		}
	}
	return "", nil, false
}

// fillDeps sets every Dep field of the input from the values the route's
// providers resolved.
//
// It is linear in the number of Dep fields times the number of resolved
// dependencies, both fixed by the route's declaration, and allocates nothing:
// the field is reached in place and its address is pointer-shaped, so the
// conversion to an interface does not box it.
func (p *bindPlan) fillDeps(c *Context, dst reflect.Value) error {
	for i := range p.deps {
		d := &p.deps[i]
		v, ok := c.dependency(d.typ)
		if !ok {
			// A route is built only when every Dep has a provider of its exact
			// type on the route's chain, and every provider on the chain has
			// resolved before the input is bound, so the value is always there.
			// This fails the request closed rather than handing the handler an
			// empty value if that ever stopped holding.
			return fmt.Errorf("muzak: no value of type %s was resolved for field %s", d.typ, d.name)
		}
		fieldByIndex(dst, d.index).Addr().Interface().(depFiller).fill(v)
	}
	return nil
}

// dependency returns the innermost resolved value of the given type, which is
// the one [TryFrom] returns.
func (c *Context) dependency(t reflect.Type) (any, bool) {
	for i := len(c.deps) - 1; i >= 0; i-- {
		if c.deps[i].typ == t {
			return c.deps[i].val, true
		}
	}
	return nil, false
}

// checkDeps reports every Dep field of the route's input that no provider on
// the route's chain satisfies, naming the route, the field and the type.
func (rt *Route) checkDeps() error {
	var errs []error
	for _, d := range rt.plan.deps {
		if hasProvider(rt.providers, d.typ) {
			continue
		}
		hint := ""
		switch {
		case hasProvider(rt.providers, reflect.PointerTo(d.typ)):
			hint = fmt.Sprintf("; a provider of *%s is declared, which a muzak.Dep[*%s] would receive", d.typ, d.typ)
		case d.typ.Kind() == reflect.Pointer && hasProvider(rt.providers, d.typ.Elem()):
			hint = fmt.Sprintf("; a provider of %s is declared, which a muzak.Dep[%s] would receive", d.typ.Elem(), d.typ.Elem())
		}
		errs = append(errs, fmt.Errorf("muzak: %s %s: field %s is a muzak.Dep[%s], but no provider of exactly %s is declared on the route or a router above it; "+
			"declare one with muzak.Needs, muzak.Acquire, muzak.Transaction, muzak.Singleton or muzak.WithSingleton%s",
			rt.Method, rt.Path, d.name, d.typ, d.typ, hint))
	}
	return errors.Join(errs...)
}

// hasProvider reports whether any provider produces exactly the given type.
func hasProvider(providers []*provider, t reflect.Type) bool {
	for _, p := range providers {
		if p.typ == t {
			return true
		}
	}
	return false
}
