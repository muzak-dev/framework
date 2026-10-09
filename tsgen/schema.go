package tsgen

import (
	"fmt"
	"slices"
	"strings"

	"muzak.dev/framework"
)

// typeOf writes the TypeScript type a schema describes. blob says whether a
// binary string is a Blob, as it is in a form body and a response, rather
// than the text it is anywhere JSON carries it. level is how far an object
// written here is indented.
func (g *generator) typeOf(s *muzak.Schema, blob bool, level int) (string, error) {
	if s == nil {
		return "unknown", nil
	}
	g.depth++
	defer func() { g.depth-- }()
	if g.depth > maxDepth {
		return "", fmt.Errorf("a schema is nested more than %d levels deep, or contains itself other than by reference", maxDepth)
	}
	if err := g.spend(); err != nil {
		return "", err
	}
	if s.Ref != "" {
		return g.reference(s.Ref)
	}
	if s.Enum != nil {
		return g.enum(s.Enum)
	}
	var parts []string
	if len(s.AnyOf) > 0 {
		union, err := g.joined(s.AnyOf, blob, level)
		if err != nil {
			return "", err
		}
		parts = append(parts, union)
	}
	base, err := g.typed(s, blob, level)
	if err != nil {
		return "", err
	}
	if base != "unknown" {
		parts = append(parts, base)
	}
	for _, member := range s.AllOf {
		expr, err := g.typeOf(member, blob, level)
		if err != nil {
			return "", err
		}
		if expr != "unknown" {
			parts = append(parts, expr)
		}
	}
	switch len(parts) {
	case 0:
		return "unknown", nil
	case 1:
		return parts[0], nil
	}
	// Every keyword holds at once, so what they say is intersected. A union
	// among them is grouped, so that it is not read as part of another.
	for i := range parts {
		parts[i] = group(parts[i])
	}
	return strings.Join(slices.Compact(parts), " & "), nil
}

// joined writes the union of schemas, each grouped so that precedence is
// what it reads as. A member that may be anything makes the union anything.
func (g *generator) joined(schemas []*muzak.Schema, blob bool, level int) (string, error) {
	var parts []string
	for _, member := range schemas {
		expr, err := g.typeOf(member, blob, level)
		if err != nil {
			return "", err
		}
		if expr = group(expr); !slices.Contains(parts, expr) {
			parts = append(parts, expr)
		}
	}
	if slices.Contains(parts, "unknown") {
		return "unknown", nil
	}
	return strings.Join(parts, " | "), nil
}

// typed writes what a schema's type keyword describes, a union for a list of
// types, or what its other keywords imply when it has none.
func (g *generator) typed(s *muzak.Schema, blob bool, level int) (string, error) {
	types := typeNames(s.Type)
	if len(types) == 0 {
		switch {
		case s.Properties != nil || s.AdditionalProperties != nil:
			types = []string{"object"}
		case s.Items != nil:
			types = []string{"array"}
		default:
			return "unknown", nil
		}
	}
	var parts []string
	for _, name := range types {
		var expr string
		switch name {
		case "string":
			expr = "string"
			if blob && s.Format == "binary" {
				expr = "Blob"
			}
		case "integer", "number":
			expr = "number"
		case "boolean":
			expr = "boolean"
		case "null":
			expr = "null"
		case "array":
			item, err := g.typeOf(s.Items, blob, level)
			if err != nil {
				return "", err
			}
			expr = arrayOf(item)
		case "object":
			object, err := g.object(s, blob, level)
			if err != nil {
				return "", err
			}
			expr = object
		default:
			expr = "unknown"
		}
		if !slices.Contains(parts, expr) {
			parts = append(parts, expr)
		}
	}
	if slices.Contains(parts, "unknown") {
		return "unknown", nil
	}
	return strings.Join(parts, " | "), nil
}

// object writes an object type: its members, each optional unless the schema
// requires it, and an index signature for additionalProperties.
//
// A schema whose additional properties are typed and that also has members
// is given an index signature of unknown, since TypeScript requires every
// member to fit the signature and a member's type need not be the additional
// properties' type. One with no members at all is a Record, of never when it
// admits no member whatever.
func (g *generator) object(s *muzak.Schema, blob bool, level int) (string, error) {
	extra, hasExtra, err := g.additional(s.AdditionalProperties, blob, level)
	if err != nil {
		return "", err
	}
	if len(s.Properties) == 0 {
		switch {
		case s.AdditionalProperties == false:
			extra = "never"
		case !hasExtra:
			extra = "unknown"
		}
		return "Record<string, " + extra + ">", nil
	}
	indent := strings.Repeat("  ", level)
	var b strings.Builder
	b.WriteString("{\n")
	for _, name := range sortedKeys(s.Properties) {
		if err := g.spend(); err != nil {
			return "", err
		}
		member := s.Properties[name]
		expr, err := g.typeOf(member, blob, level+1)
		if err != nil {
			return "", err
		}
		writeComment(&b, indent+"  ", describe(member)...)
		optional := "?"
		if slices.Contains(s.Required, name) {
			optional = ""
		}
		b.WriteString(indent + "  " + propertyKey(name) + optional + ": " + expr + ";\n")
	}
	if hasExtra {
		b.WriteString(indent + "  [key: string]: unknown;\n")
	}
	b.WriteString(indent + "}")
	return b.String(), nil
}

// additional reads additionalProperties: the type of the extra members, and
// whether there may be any. Muzak writes a boolean or a *Schema; a document
// built in Go may hold a Schema by value, and anything else is read as
// admitting any member.
func (g *generator) additional(value any, blob bool, level int) (string, bool, error) {
	switch v := value.(type) {
	case nil:
		return "", false, nil
	case bool:
		return "unknown", v, nil
	case *muzak.Schema:
		expr, err := g.typeOf(v, blob, level)
		return expr, true, err
	case muzak.Schema:
		expr, err := g.typeOf(&v, blob, level)
		return expr, true, err
	}
	return "unknown", true, nil
}
