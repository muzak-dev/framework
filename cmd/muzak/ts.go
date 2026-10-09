package main

import (
	"context"
	"flag"
	"fmt"

	"muzak.dev/framework/tsgen"
)

// tsCmd writes TypeScript declarations.
var tsCmd = &command{
	name:    "ts",
	args:    "[-url URL | -file openapi.json] [-client] [-o out.ts]",
	summary: "write TypeScript declarations for an OpenAPI document",
	about: `Ts writes the TypeScript declarations for the API a document describes, as
the tsgen package does: an interface for every component schema, and the
parameters, body, response and error types of every operation. With -client,
a small client built on fetch is written too.

The document is read from a file, from standard input with -file -, or from a
running application with -url, fetched as muzak routes fetches it. The output
goes to standard output, or to the file -o names, which is written to a
temporary file beside it and renamed into place, so a frontend build watching
it never reads half a file.`,
	make: func() runner { return &tsRunner{} },
}

type tsRunner struct {
	source documentSource
	client bool
	out    string
}

func (t *tsRunner) flags(fs *flag.FlagSet) {
	t.source.flags(fs)
	fs.BoolVar(&t.client, "client", false, "also write a client for the API, built on fetch")
	fs.StringVar(&t.out, "o", "", "write to this `file` rather than to standard output")
}

func (t *tsRunner) run(ctx context.Context, c *console, args, passthrough []string) error {
	if err := noArguments(tsCmd, args, passthrough); err != nil {
		return err
	}
	doc, err := t.source.load(ctx, c, tsCmd)
	if err != nil {
		return err
	}
	out, err := tsgen.Generate(doc, tsgen.Options{Client: t.client})
	if err != nil {
		return fmt.Errorf("muzak: the TypeScript could not be generated: %w", err)
	}
	return writeOutput(c, t.out, out)
}
