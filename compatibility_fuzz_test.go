package muzak

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"
)

// compatibilitySeeds returns documents to start fuzzing from: what Muzak
// generates, and the hand-written ones the rule tests compare.
func compatibilitySeeds(f *testing.F) [][]byte {
	f.Helper()
	a, b := kitchenSink()
	docs := []*Document{
		newIntegrationDocument(f), richDocument(f, false), richDocument(f, true), a, b,
		chainDocument(4, 2, 3, false), chainDocument(4, 2, 3, true),
		sharedDoc(compatObj(map[string]*Schema{"a": compatStr()}, "a")),
	}
	var seeds [][]byte
	for _, doc := range docs {
		data, err := doc.Marshal()
		if err != nil {
			f.Fatal(err)
		}
		seeds = append(seeds, data)
	}
	return append(seeds, []byte(minimalDocument(`{"type":["string","null"],"enum":["a",null],"additionalProperties":false}`)),
		[]byte(documentWithOperation(`{"operationId":"x","security":[{"k":["s"]}],"parameters":[{"name":"q","in":"query","schema":{"type":"integer","minimum":1}}],"responses":{"200":{"description":""}}}`)))
}

// FuzzReadDocument feeds ReadDocument arbitrary bytes. Whatever it accepts is
// the same document as a second reading of the same bytes, and as what it
// writes itself out as; whatever it refuses, it refuses with an error of its
// own and no document.
func FuzzReadDocument(f *testing.F) {
	for _, seed := range compatibilitySeeds(f) {
		f.Add(seed)
	}
	f.Add([]byte(`{"openapi":"3.1.0","paths":{"/a":{"get":{"operationId":"","responses":{"default":null}}}}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := ReadDocument(bytes.NewReader(data))
		if err != nil {
			if doc != nil || !strings.HasPrefix(err.Error(), "muzak: ") {
				t.Fatalf("ReadDocument = %v, %v", doc, err)
			}
			return
		}
		again, err := ReadDocument(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("a second reading failed: %v", err)
		}
		if changes := CompareDocuments(doc, again); len(changes) != 0 {
			t.Fatalf("a document differs from itself:\n%s", renderAPIChanges(changes))
		}
		out, err := doc.Marshal()
		if err != nil {
			t.Fatalf("a document read could not be written: %v", err)
		}
		if len(out) > maxDocumentBytes {
			return
		}
		written, err := ReadDocument(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("a document as written could not be read back: %v\n%s", err, out)
		}
		if changes := CompareDocuments(doc, written); len(changes) != 0 {
			t.Fatalf("a document differs from itself as written:\n%s", renderAPIChanges(changes))
		}
	})
}

// FuzzCompareDocuments compares two documents read from arbitrary bytes. The
// comparison never panics, gives the same result twice, is sorted, and is
// symmetric: every kind of change one way is matched by the opposite kind the
// other way.
func FuzzCompareDocuments(f *testing.F) {
	seeds := compatibilitySeeds(f)
	for i := range seeds {
		f.Add(seeds[i], seeds[(i+1)%len(seeds)])
		f.Add(seeds[i], seeds[i])
	}
	f.Fuzz(func(t *testing.T, a, b []byte) {
		old, err := ReadDocument(bytes.NewReader(a))
		if err != nil {
			return
		}
		cur, err := ReadDocument(bytes.NewReader(b))
		if err != nil {
			return
		}
		forward := CompareDocuments(old, cur)
		if again := CompareDocuments(old, cur); !slices.Equal(forward, again) {
			t.Fatal("the comparison is not deterministic")
		}
		for i, change := range forward {
			if change.Severity < Compatible || change.Severity > Breaking || change.Kind == "" || !strings.HasSuffix(change.Message, ".") {
				t.Fatalf("malformed change %+v", change)
			}
			if i > 0 && forward[i-1].Severity < change.Severity {
				t.Fatalf("changes are not sorted by severity: %+v before %+v", forward[i-1], change)
			}
		}
		if err := WriteChanges(io.Discard, forward); err != nil {
			t.Fatal(err)
		}
		backward := CompareDocuments(cur, old)
		if incomplete(forward) || incomplete(backward) {
			return
		}
		want := map[string]bool{}
		for _, change := range forward {
			want[inverseKind(change.Kind)] = true
		}
		have := map[string]bool{}
		for _, change := range backward {
			have[change.Kind] = true
		}
		if !mapsEqual(want, have) {
			t.Fatalf("the comparison is not symmetric:\nforward:\n%s\nbackward:\n%s", renderAPIChanges(forward), renderAPIChanges(backward))
		}
	})
}

func incomplete(changes []APIChange) bool {
	return slices.ContainsFunc(changes, func(c APIChange) bool { return c.Kind == "comparison-incomplete" })
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
