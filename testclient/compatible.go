package testclient

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"muzak.dev/framework"
)

// updateBaselineVariable is the environment variable that makes
// [AssertCompatible] record the current document rather than compare with the
// stored one.
const updateBaselineVariable = "MUZAK_UPDATE_OPENAPI"

// baselineReadLimit is one byte more than the largest document
// [muzak.ReadDocument] reads, so that reading a larger file stops there and
// the document is refused for its size rather than read whole.
const baselineReadLimit = 16<<20 + 1

// AssertCompatible fails the test when the application's API would break a
// client written against the baseline document stored at baselinePath, and
// lists what breaks it. It is how a pull request that removes an operation,
// adds a required member to a request or takes a member out of a response is
// caught by the test suite instead of by a client.
//
//	func TestAPICompatibility(t *testing.T) {
//		testclient.AssertCompatible(t, buildApp(), "testdata/openapi.json")
//	}
//
// The comparison is [muzak.CompareDocuments]. A breaking change fails the
// test; a change that may break some clients, such as a new value in a
// response enum, is logged for a person to judge; a compatible one passes,
// with a note when the baseline no longer matches the document, so that it can
// be refreshed. The application is built to describe it, so register
// everything first.
//
// Run the test with MUZAK_UPDATE_OPENAPI=1 in the environment to record the
// current document as the baseline instead: when the baseline is first
// created, and when a change it reports is intended. The changes a new
// baseline accepts are logged as it is written. The document is written to
// baselinePath alone, creating the directories it needs, through a temporary
// file in the same directory that is renamed into place, so an interrupted run
// never leaves half a document behind; a link at baselinePath is replaced, not
// followed. The file is readable by everyone (0644), as a file committed to a
// repository is.
//
// A baseline that does not exist fails the test with these instructions, so a
// suite never passes by comparing against nothing.
func AssertCompatible(tb testing.TB, app *muzak.App, baselinePath string) {
	tb.Helper()
	if baselinePath == "" {
		tb.Fatalf("testclient: AssertCompatible needs the path of the baseline document")
		return
	}
	doc, err := app.Document()
	if err != nil {
		tb.Fatalf("testclient: the application could not be built to describe it: %v", err)
		return
	}
	if doc == nil {
		tb.Fatalf("testclient: the application publishes no OpenAPI document, since AppOptions.DisableDocs is set, so there is nothing to compare with the baseline at %s", baselinePath)
		return
	}
	current, err := doc.Marshal()
	if err != nil {
		tb.Fatalf("testclient: the application's OpenAPI document could not be written: %v", err)
		return
	}
	current = append(current, '\n')
	stored, readErr := readBaseline(baselinePath)

	if os.Getenv(updateBaselineVariable) == "1" {
		if readErr == nil {
			if baseline, err := muzak.ReadDocument(bytes.NewReader(stored)); err == nil {
				if accepted := notCompatible(muzak.CompareDocuments(baseline, doc)); len(accepted) > 0 {
					tb.Logf("testclient: the new baseline at %s accepts these changes:\n%s", baselinePath, report(accepted))
				}
			}
		}
		if err := writeBaseline(baselinePath, current); err != nil {
			tb.Fatalf("testclient: the OpenAPI baseline could not be written to %s: %v", baselinePath, err)
			return
		}
		tb.Logf("testclient: recorded the application's OpenAPI document as the baseline at %s; commit it, and run without %s to compare against it", baselinePath, updateBaselineVariable)
		return
	}

	if errors.Is(readErr, fs.ErrNotExist) {
		tb.Fatalf("testclient: there is no OpenAPI baseline at %s; run the test once with %s=1 to record the application's current document there, and commit the file", baselinePath, updateBaselineVariable)
		return
	}
	if readErr != nil {
		tb.Fatalf("testclient: the OpenAPI baseline at %s could not be read: %v", baselinePath, readErr)
		return
	}
	baseline, err := muzak.ReadDocument(bytes.NewReader(stored))
	if err != nil {
		tb.Fatalf("testclient: the OpenAPI baseline at %s is not a document it can compare with: %v; run the test with %s=1 to record the current document in its place", baselinePath, err, updateBaselineVariable)
		return
	}
	changes := muzak.CompareDocuments(baseline, doc)
	if possibly := withSeverity(changes, muzak.PossiblyBreaking); len(possibly) > 0 {
		tb.Logf("testclient: the API changed in ways that may break some clients of the baseline at %s:\n%s", baselinePath, report(possibly))
	}
	if breaking := withSeverity(changes, muzak.Breaking); len(breaking) > 0 {
		tb.Errorf("testclient: the API breaks clients written against the baseline at %s:\n%sIf the change is intended, run the test with %s=1 to record the new document as the baseline.",
			baselinePath, report(breaking), updateBaselineVariable)
		return
	}
	// A checkout that turns line endings into CRLF, as Git on Windows may,
	// has not changed the document.
	if !bytes.Equal(bytes.TrimSpace(bytes.ReplaceAll(stored, []byte("\r\n"), []byte("\n"))), bytes.TrimSpace(current)) {
		tb.Logf("testclient: the API is compatible with the baseline at %s, which no longer matches it; run the test with %s=1 to record the current document", baselinePath, updateBaselineVariable)
	}
}

// readBaseline reads a stored baseline, stopping one byte past the largest
// document that is read, which the reading then refuses for its size.
func readBaseline(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the test's own baseline, named by the test that asserts against it
	if err != nil {
		return nil, err
	}
	// The file is only read, so closing it has nothing left to report.
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, baselineReadLimit))
}

// writeBaseline writes a document to path through a temporary file beside it,
// renamed into place once it is complete, so that the path holds the old
// document or the new one and never part of either. Nothing is written
// anywhere else: the temporary file is removed when any step fails, and a link
// at path is replaced rather than followed.
func writeBaseline(path string, data []byte) error {
	dir := filepath.Dir(path)
	// The directories hold a file every collaborator reads, as the rest of a
	// repository's directories do.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a repository directory, not a private one
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		// A baseline is committed and read by every collaborator and by CI,
		// so it is readable by all rather than private to its writer.
		err = tmp.Chmod(0o644) //nolint:gosec // a document meant to be committed, which holds no secret
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// withSeverity returns the changes of one severity.
func withSeverity(changes []muzak.APIChange, severity muzak.ChangeSeverity) []muzak.APIChange {
	var out []muzak.APIChange
	for _, change := range changes {
		if change.Severity == severity {
			out = append(out, change)
		}
	}
	return out
}

// notCompatible returns the changes that break, or may break, a client.
func notCompatible(changes []muzak.APIChange) []muzak.APIChange {
	return append(withSeverity(changes, muzak.Breaking), withSeverity(changes, muzak.PossiblyBreaking)...)
}

// report renders changes as [muzak.WriteChanges] does.
func report(changes []muzak.APIChange) string {
	var b strings.Builder
	// A strings.Builder never fails a write.
	_ = muzak.WriteChanges(&b, changes)
	return b.String()
}
