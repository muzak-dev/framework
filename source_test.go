package badele

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// disallowedRunes are characters that must not appear anywhere in the project's
// sources, documentation or generated output.
//
// They are written as escapes rather than as literals so that this file passes
// its own check. Em dashes and their typographic relatives are the ones that
// matter: they are hard to tell apart from a hyphen in most editors, break
// alignment in fixed-width contexts such as the console log format, and cannot
// be typed on every keyboard, which makes them a poor fit for text a developer
// may need to grep for or retype.
var disallowedRunes = map[rune]string{
	'\u2014': "em dash",
	'\u2013': "en dash",
	'\u2012': "figure dash",
	'\u2015': "horizontal bar",
	'\u2018': "left single quotation mark",
	'\u2019': "right single quotation mark",
	'\u201C': "left double quotation mark",
	'\u201D': "right double quotation mark",
	'\u2026': "horizontal ellipsis",
	'\u00A0': "non-breaking space",
	'\u00B7': "middle dot",
}

// finding is one offending rune, located for a readable failure message.
type finding struct {
	line int
	rune rune
	why  string
}

// String renders a finding as the message the test reports.
func (f finding) String() string {
	if f.why != "" {
		return fmt.Sprintf("line %d contains a %s (%q); use plain ASCII punctuation instead", f.line, f.why, string(f.rune))
	}
	return fmt.Sprintf("line %d contains the non-ASCII rune %q (U+%04X)", f.line, string(f.rune), f.rune)
}

// findNonASCII returns one finding per non-ASCII rune in content.
func findNonASCII(content string) []finding {
	var found []finding
	line := 1
	for _, r := range content {
		if r == '\n' {
			line++
			continue
		}
		if r < utf8.RuneSelf {
			continue
		}
		found = append(found, finding{line: line, rune: r, why: disallowedRunes[r]})
	}
	return found
}

// TestSourcesAreASCII walks the whole project and fails on any file carrying a
// non-ASCII rune.
//
// Keeping every source and document file to plain ASCII means what is written
// is what can be typed, searched for and diffed, with no invisible characters
// hiding in comments or in log output.
func TestSourcesAreASCII(t *testing.T) {
	t.Parallel()
	walkProject(t, func(name, content string) {
		for _, f := range findNonASCII(content) {
			t.Errorf("%s:%s", name, f)
		}
	})
}

// TestNoUnsafeImports enforces the rule that the module uses no unsafe code.
func TestNoUnsafeImports(t *testing.T) {
	t.Parallel()
	walkProject(t, func(name, content string) {
		if filepath.Ext(name) != ".go" {
			return
		}
		for number, line := range strings.Split(content, "\n") {
			// Match an import line rather than any mention, so that the check
			// does not trip over its own source.
			trimmed := strings.TrimSpace(line)
			if trimmed == `"unsafe"` || trimmed == `_ "unsafe"` || trimmed == `import "unsafe"` {
				t.Errorf("%s:%d imports unsafe, which the module does not permit", name, number+1)
			}
		}
	})
}

// TestCoverageExemptionsAreJustified checks that every coverage exemption
// carries a real explanation, so that a bare marker cannot be used to hide
// untested logic behind an unexplained exemption.
func TestCoverageExemptionsAreJustified(t *testing.T) {
	t.Parallel()
	const marker = "// coverage:"
	walkProject(t, func(name, content string) {
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return
		}
		for number, line := range strings.Split(content, "\n") {
			index := strings.Index(line, marker)
			if index < 0 {
				continue
			}
			reason := strings.TrimSpace(line[index+len(marker):])
			if len(strings.Fields(reason)) < 4 {
				t.Errorf("%s:%d has a coverage exemption with no real justification: %q",
					name, number+1, reason)
			}
		}
	})
}

// walkProject calls visit for every checked file in the repository, with the
// path rendered relative to the repository root.
func walkProject(t *testing.T, visit func(name, content string)) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := filepath.Dir(wd)

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !checkedExtension(filepath.Ext(entry.Name())) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, relErr := filepath.Rel(root, path)
		if relErr != nil {
			// coverage: both paths come from the same walk, so they always
			// share a root and Rel cannot fail here.
			name = path
		}
		visit(name, string(content))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the project: %v", err)
	}
}

// skipDir reports whether a directory should not be walked.
//
// The panels directory holds the front-end workspace, whose dependencies and
// vendored documentation are not this module's to police; the rules enforced
// here are about Go sources and the documents written alongside them.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "testdata", "vendor", "panels":
		return true
	}
	return false
}

// checkedExtension reports whether a file's contents are subject to the rules
// enforced here.
func checkedExtension(ext string) bool {
	switch ext {
	case ".go", ".md", ".html", ".css", ".js", ".json", ".yaml", ".yml", ".mod", ".txt":
		return true
	}
	return false
}

// TestFindNonASCII exercises the checker itself, so that a silent pass cannot
// be mistaken for a clean project.
func TestFindNonASCII(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		want    int
		message string
	}{
		{name: "clean ASCII", content: "package main\n// a plain comment\n", want: 0},
		{name: "em dash", content: "a \u2014 b\n", want: 1, message: "em dash"},
		{name: "smart quotes", content: "\u201Cquoted\u201D\n", want: 2, message: "quotation mark"},
		{name: "unclassified non-ascii", content: "caf\u00E9\n", want: 1, message: "non-ASCII rune"},
		{name: "counts every occurrence", content: "\u2014\u2014\u2014\n", want: 3, message: "em dash"},
		{name: "reports the line number", content: "ok\nok\n\u2014\n", want: 1, message: "line 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			found := findNonASCII(tc.content)
			if len(found) != tc.want {
				t.Fatalf("findNonASCII(%q) = %d findings, want %d", tc.content, len(found), tc.want)
			}
			if tc.message != "" && !strings.Contains(found[0].String(), tc.message) {
				t.Errorf("message = %q, want it to mention %q", found[0], tc.message)
			}
		})
	}
}

func TestSkipDirAndCheckedExtension(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".git", "node_modules", "testdata", "vendor", "panels"} {
		if !skipDir(name) {
			t.Errorf("skipDir(%q) = false, want true", name)
		}
	}
	if skipDir("internal") {
		t.Error("skipDir(internal) = true, want false")
	}
	for _, ext := range []string{".go", ".md", ".html", ".css", ".js", ".json", ".yaml", ".yml", ".mod", ".txt"} {
		if !checkedExtension(ext) {
			t.Errorf("checkedExtension(%q) = false, want true", ext)
		}
	}
	if checkedExtension(".png") {
		t.Error("checkedExtension(.png) = true, want false")
	}
}
