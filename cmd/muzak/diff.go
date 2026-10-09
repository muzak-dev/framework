package main

import (
	"context"
	"flag"
	"fmt"

	"muzak.dev/framework"
)

// diffCmd compares two documents.
var diffCmd = &command{
	name:    "diff",
	args:    "[-fail-on breaking|possibly|never] old.json new.json",
	summary: "report the changes between two OpenAPI documents that break clients",
	about: `Diff compares two OpenAPI documents, judged from the side of a client written
against the old one, and prints every change, grouped as breaking, possibly
breaking and compatible, each with where it is in the old document and its
kind. This is muzak.CompareDocuments and muzak.WriteChanges, so it reports
exactly what testclient.AssertCompatible does in a test.

Either document may be read from standard input by naming it -. Names in the
report that come from the documents are escaped for the terminal.

Exit status:

  0  no change as serious as -fail-on names
  1  at least one change that serious
  2  the command line is wrong, or a document cannot be read

-fail-on breaking, the default, fails on breaking changes; possibly also fails
on possibly breaking ones; never always succeeds once both documents are
read, for a report that only informs.`,
	make: func() runner { return &diffRunner{} },
}

type diffRunner struct {
	failOn string
}

func (d *diffRunner) flags(fs *flag.FlagSet) {
	fs.StringVar(&d.failOn, "fail-on", "breaking", "the least serious `level` of change that fails: breaking, possibly or never")
}

// failThresholds maps -fail-on to the least severity it fails on, where zero
// fails on nothing.
var failThresholds = map[string]muzak.ChangeSeverity{
	"breaking": muzak.Breaking,
	"possibly": muzak.PossiblyBreaking,
	"never":    0,
}

// failDescriptions name the changes each -fail-on fails on.
var failDescriptions = map[string]string{
	"breaking": "breaking",
	"possibly": "breaking or possibly breaking",
}

func (d *diffRunner) run(_ context.Context, c *console, args, passthrough []string) error {
	threshold, ok := failThresholds[d.failOn]
	if !ok {
		return usagef(diffCmd, "-fail-on is %q, and it is one of breaking, possibly or never", d.failOn)
	}
	args = append(args, passthrough...)
	if len(args) != 2 {
		return usagef(diffCmd, "diff compares two documents, the old and the new, and was given %d", len(args))
	}
	if args[0] == "-" && args[1] == "-" {
		return usagef(diffCmd, "only one of the documents can be read from standard input")
	}
	old, err := readDocumentFile(c, args[0])
	if err != nil {
		return &exitError{code: exitUsage, err: err}
	}
	cur, err := readDocumentFile(c, args[1])
	if err != nil {
		return &exitError{code: exitUsage, err: err}
	}
	changes := muzak.CompareDocuments(old, cur)
	if err := muzak.WriteChanges(c.stdout, changes); err != nil {
		return err
	}
	if threshold == 0 {
		return nil
	}
	failing := 0
	for _, change := range changes {
		if change.Severity >= threshold {
			failing++
		}
	}
	if failing == 0 {
		return nil
	}
	noun := "changes"
	if failing == 1 {
		noun = "change"
	}
	return &exitError{code: exitFailure, err: fmt.Errorf("muzak: the comparison found %d %s %s, and -fail-on %s fails on that",
		failing, failDescriptions[d.failOn], noun, d.failOn)}
}
