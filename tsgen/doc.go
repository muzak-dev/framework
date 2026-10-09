// Package tsgen writes TypeScript declarations for the API a Muzak
// application describes, from its OpenAPI document, so that a frontend or a
// Node service types its requests and responses by the same models the Go
// handlers are written against:
//
//	doc, err := app.Document()
//	if err != nil {
//		log.Fatal(err)
//	}
//	ts, err := tsgen.Generate(doc, tsgen.Options{Client: true})
//	if err != nil {
//		log.Fatal(err)
//	}
//	err = os.WriteFile("web/src/api.ts", ts, 0o644)
//
// A document read with [muzak.ReadDocument], such as one another service
// publishes, works the same way.
//
// # What is written
//
// Every component schema becomes an exported interface, or a type alias when
// it is not an object, named after the component. Every operation becomes
// four types named after its operationId: Params, the path, query, header and
// cookie parameters grouped by where they go; Body, the request body; Response,
// what a success answers with; and Error, what any other status does. An
// Operations interface maps each operation to its method, path and those four
// types, for a client of your own to be typed by. With [Options.Client], a
// small client built on fetch is written too; see [Options].
//
// JSON Schema is translated as a TypeScript programmer would write it: an
// enum is a union of literals, a nullable type is "| null", a member the
// schema does not require is optional, additionalProperties is an index
// signature, allOf is an intersection and anyOf a union, and a string of
// format binary is a Blob in a form body or a response. A reference is always
// written as the name of the type it points at, never expanded, which is what
// makes a recursive type work and keeps the output linear in the document.
//
// # What it promises
//
// The output is deterministic: the same document always gives the same bytes,
// whatever order Go's maps are walked in, so the file can be committed and
// diffed. It is also safe to generate from a document someone else wrote.
// Every name in the document is reduced to an identifier made of ASCII
// letters, digits and "_", renamed when it is a reserved word or a global the
// output uses, and numbered when two names reduce to the same one. Every other
// string from the document is escaped where it is written: a description
// cannot end the comment it is written into, whatever "*" and "/" or line
// terminators it holds, U+2028 and U+2029 included; and an enum value, a
// property name or a path cannot end the string literal it is written into.
// Control characters, the Unicode controls that reorder text and bytes that
// are not UTF-8 are written as escapes rather than as themselves. Generation
// is bounded: a schema nested deeper than 64 levels, or a document whose
// schemas, shared in memory, would be walked more than a million times, is an
// error rather than a long wait.
package tsgen
