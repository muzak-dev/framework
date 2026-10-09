package main

import (
	"bytes"
	"strings"
	"testing"
)

// diffDocuments writes the documents the diff tests compare into dir:
// old.json; breaking.json, which removes an operation, removes a deprecated
// one and adds one; possibly.json, which only removes the deprecated one;
// compatible.json, which only adds one; and same.json, a copy of old.json.
func diffDocuments(t *testing.T, dir string) {
	t.Helper()
	old := documentOf(t, diffApp(nil, false))
	writeDocument(t, dir, "old.json", old)
	writeDocument(t, dir, "same.json", old)
	writeDocument(t, dir, "breaking.json", documentOf(t, diffApp(map[string]bool{"create": true, "delete": true}, true)))
	writeDocument(t, dir, "possibly.json", documentOf(t, diffApp(map[string]bool{"delete": true}, false)))
	writeDocument(t, dir, "compatible.json", documentOf(t, diffApp(nil, true)))
}

func TestDiffExitsByTheSeverityItFailsOn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	diffDocuments(t, dir)
	cases := []struct {
		failOn string
		newer  string
		code   int
	}{
		{"", "breaking.json", exitFailure},
		{"breaking", "breaking.json", exitFailure},
		{"possibly", "breaking.json", exitFailure},
		{"never", "breaking.json", exitOK},
		{"", "possibly.json", exitOK},
		{"possibly", "possibly.json", exitFailure},
		{"never", "possibly.json", exitOK},
		{"", "compatible.json", exitOK},
		{"possibly", "compatible.json", exitOK},
		{"possibly", "same.json", exitOK},
	}
	for _, c := range cases {
		args := []string{"diff", "old.json", c.newer}
		if c.failOn != "" {
			args = append(args, "-fail-on", c.failOn)
		}
		r := runIn(t, dir, args...)
		if r.code != c.code {
			t.Errorf("%v: exit status %d, want %d\n%s%s", args, r.code, c.code, r.stdout, r.stderr)
		}
		if (r.code == exitFailure) != strings.Contains(r.stderr, "and -fail-on") {
			t.Errorf("%v: stderr %q", args, r.stderr)
		}
	}
}

func TestDiffReportsEveryChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	diffDocuments(t, dir)
	r := runIn(t, dir, "diff", "-fail-on", "possibly", "old.json", "breaking.json")
	r.expect(t, exitFailure, `^muzak: the comparison found 2 breaking or possibly breaking changes, and -fail-on possibly fails on that\n$`)
	golden(t, "diff.golden", r.stdout)

	r = runIn(t, dir, "diff", "old.json", "possibly.json", "-fail-on", "possibly")
	r.expect(t, exitFailure, `found 1 breaking or possibly breaking change, and`)
	r = runIn(t, dir, "diff", "old.json", "breaking.json")
	r.expect(t, exitFailure, `^muzak: the comparison found 1 breaking change, and -fail-on breaking fails on that\n$`)

	same := runIn(t, dir, "diff", "old.json", "same.json")
	same.expect(t, exitOK)
	if same.stdout != "No changes.\n" {
		t.Errorf("stdout = %q", same.stdout)
	}
}

func TestDiffReadsOneDocumentFromStandardInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	diffDocuments(t, dir)
	for _, args := range [][]string{{"diff", "-", "breaking.json"}, {"diff", "old.json", "-"}} {
		c, _, _ := testConsole(dir)
		source := "old.json"
		if args[2] == "-" {
			source = "breaking.json"
		}
		data := documentOf(t, diffApp(nil, false))
		if source == "breaking.json" {
			data = documentOf(t, diffApp(map[string]bool{"create": true, "delete": true}, true))
		}
		c.stdin = bytes.NewReader(data)
		runWith(c, args...).expect(t, exitFailure, `found 1 breaking change`)
	}
}

func TestDiffUsageAndReadErrorsExitTwo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	diffDocuments(t, dir)
	writeDocument(t, dir, "bad.json", []byte(`{"openapi": "3.1.0"`))
	runIn(t, dir, "diff").expect(t, exitUsage, `diff compares two documents, the old and the new, and was given 0`)
	runIn(t, dir, "diff", "old.json").expect(t, exitUsage, `was given 1`)
	runIn(t, dir, "diff", "a", "b", "c").expect(t, exitUsage, `was given 3`)
	runIn(t, dir, "diff", "-", "-").expect(t, exitUsage, `only one of the documents can be read from standard input`)
	runIn(t, dir, "diff", "-fail-on", "minor", "old.json", "same.json").expect(t, exitUsage, `-fail-on is "minor", and it is one of breaking, possibly or never`)
	runIn(t, dir, "diff", "missing.json", "old.json").expect(t, exitUsage, `muzak: the document cannot be opened`)
	runIn(t, dir, "diff", "old.json", "missing.json").expect(t, exitUsage, `muzak: the document cannot be opened`)
	runIn(t, dir, "diff", "old.json", "bad.json").expect(t, exitUsage, `muzak: bad\.json: the OpenAPI document is malformed`)
	// A file named like a flag is read after --.
	writeDocument(t, dir, "-new.json", documentOf(t, diffApp(nil, false)))
	runIn(t, dir, "diff", "--", "old.json", "-new.json").expect(t, exitOK)
}

func TestDiffFailsWhenTheReportCannotBeWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	diffDocuments(t, dir)
	runFailingStdout(t, dir, "diff", "-fail-on", "never", "old.json", "breaking.json").expect(t, exitFailure, `the pipe is closed`)
}

func TestDiffEscapesWhatTheDocumentsHold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDocument(t, dir, "old.json", []byte(hostileDocument))
	writeDocument(t, dir, "new.json", []byte(`{"openapi": "3.1.0", "info": {"title": "x", "version": "1"}, "paths": {}}`))
	r := runIn(t, dir, "diff", "old.json", "new.json")
	r.expect(t, exitFailure)
	checkPrintable(t, r.stdout, true)
	if !strings.Contains(r.stdout, `/a\x1b[2J\x1b[Hb`) {
		t.Errorf("the report does not name the path, escaped:\n%s", r.stdout)
	}
}
