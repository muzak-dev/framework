package main

import (
	"context"
	"flag"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"muzak.dev/framework"
)

// routesCmd lists the operations of a document.
var routesCmd = &command{
	name:    "routes",
	args:    "[-url URL | -file openapi.json]",
	summary: "list the operations an OpenAPI document describes",
	about: `Routes prints a table of every operation the document describes: its method,
its path, its operationId, its summary and the security requirements a client
satisfies one of.

A requirement lists the schemes a client satisfies together, joined by "+",
each with the scopes it needs in brackets, and alternatives are separated by
"|". "none" is an operation that says it needs no credentials, and "-" one
that says nothing at all. A deprecated operation's summary is marked.

The document is read from a file, from standard input with -file -, or from a
running application with -url, such as the one muzak dev runs. Fetching
connects to loopback and private addresses, since that is where a developer's
own application listens, but never to a cloud metadata address; it gives up
after 30 seconds and reads at most 16 MiB. Everything printed that comes from
the document is escaped for the terminal.`,
	make: func() runner { return &routesRunner{} },
}

type routesRunner struct {
	source documentSource
}

func (r *routesRunner) flags(fs *flag.FlagSet) { r.source.flags(fs) }

func (r *routesRunner) run(ctx context.Context, c *console, args, passthrough []string) error {
	if err := noArguments(routesCmd, args, passthrough); err != nil {
		return err
	}
	doc, err := r.source.load(ctx, c, routesCmd)
	if err != nil {
		return err
	}
	return writeRoutes(c.stdout, doc)
}

// maxSummary is the most of a summary the table shows, in runes, so that one
// long sentence does not push every row past the width of a terminal.
const maxSummary = 72

// routeMethods are the methods a path item holds, in the order the table
// lists them.
var routeMethods = []struct {
	name string
	op   func(*muzak.PathItem) *muzak.Operation
}{
	{"GET", func(p *muzak.PathItem) *muzak.Operation { return p.Get }},
	{"HEAD", func(p *muzak.PathItem) *muzak.Operation { return p.Head }},
	{"POST", func(p *muzak.PathItem) *muzak.Operation { return p.Post }},
	{"PUT", func(p *muzak.PathItem) *muzak.Operation { return p.Put }},
	{"PATCH", func(p *muzak.PathItem) *muzak.Operation { return p.Patch }},
	{"DELETE", func(p *muzak.PathItem) *muzak.Operation { return p.Delete }},
	{"OPTIONS", func(p *muzak.PathItem) *muzak.Operation { return p.Options }},
}

// writeRoutes prints the table of operations, sorted by path and then by
// method, so the same document always prints the same table. Every cell taken
// from the document is escaped, which also keeps a tab or a line break in one
// from splitting the table, and the work is linear in the size of the
// document.
func writeRoutes(w io.Writer, doc *muzak.Document) error {
	var b strings.Builder
	table := tabwriter.NewWriter(&b, 0, 8, 2, ' ', 0)
	rows := 0
	for _, path := range slices.Sorted(maps.Keys(doc.Paths)) {
		item := doc.Paths[path]
		if item == nil {
			continue
		}
		for _, method := range routeMethods {
			op := method.op(item)
			if op == nil {
				continue
			}
			if rows == 0 {
				_, _ = io.WriteString(table, "METHOD\tPATH\tOPERATION\tSUMMARY\tSECURITY\n")
			}
			rows++
			cells := []string{method.name, cell(path), cell(op.OperationID), summary(op), security(op.Security)}
			_, _ = io.WriteString(table, strings.Join(cells, "\t")+"\n")
		}
	}
	_ = table.Flush()
	if rows == 0 {
		b.WriteString("The document describes no operations.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// cell escapes a value for the table, writing "-" for an empty one so that
// every row has every column.
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return printable(s)
}

// summary is an operation's summary, marked when the operation is deprecated
// and cut to maxSummary runes once escaped.
func summary(op *muzak.Operation) string {
	text := printable(op.Summary)
	if op.Deprecated {
		text = strings.TrimSpace("(deprecated) " + text)
	}
	if utf8.RuneCountInString(text) > maxSummary {
		runes := []rune(text)
		text = string(runes[:maxSummary-3]) + "..."
	}
	return cell(text)
}

// security renders an operation's security requirements: "-" when it
// declares none, "none" for a requirement that needs nothing, and otherwise
// each alternative, its schemes joined by "+" with their scopes in brackets.
func security(requirements []muzak.SecurityRequirement) string {
	if requirements == nil {
		return "-"
	}
	if len(requirements) == 0 {
		return "none"
	}
	alternatives := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		if len(requirement) == 0 {
			alternatives = append(alternatives, "none")
			continue
		}
		schemes := make([]string, 0, len(requirement))
		for _, name := range slices.Sorted(maps.Keys(requirement)) {
			scheme := printable(name)
			if scopes := requirement[name]; len(scopes) > 0 {
				escaped := make([]string, len(scopes))
				for i, scope := range scopes {
					escaped[i] = printable(scope)
				}
				scheme += "[" + strings.Join(escaped, ",") + "]"
			}
			schemes = append(schemes, scheme)
		}
		alternatives = append(alternatives, strings.Join(schemes, "+"))
	}
	return strings.Join(alternatives, " | ")
}
