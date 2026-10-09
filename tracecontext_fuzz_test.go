package muzak

import (
	"regexp"
	"strings"
	"testing"
)

// The fuzz targets below check the parsers against an oracle written straight
// from the recommendation's grammar as regular expressions, which share no code
// with the parsers, so a mistake cannot hide by being made the same way twice.
var (
	oracleTraceparentV00    = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$`)
	oracleTraceparentFuture = regexp.MustCompile(`(?s)^(?:0[1-9a-f]|[1-9a-e][0-9a-f]|f[0-9a-e])-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}(?:-.*)?$`)
	oracleTracestateMember  = regexp.MustCompile(`^([a-z][a-z0-9_\-*/]{0,255}|[a-z0-9][a-z0-9_\-*/]{0,240}@[a-z][a-z0-9_\-*/]{0,13})=[\x20-\x2b\x2d-\x3c\x3e-\x7e]{0,255}[\x21-\x2b\x2d-\x3c\x3e-\x7e]$`)
)

// oracleTraceparent reports whether the recommendation accepts value.
func oracleTraceparent(value string) bool {
	match := oracleTraceparentV00.FindStringSubmatch(value)
	if match == nil {
		match = oracleTraceparentFuture.FindStringSubmatch(value)
	}
	return match != nil && strings.Trim(match[1], "0") != "" && strings.Trim(match[2], "0") != ""
}

// oracleTracestate returns what the recommendation and the bounds make of a
// list of header values, and whether it is accepted.
func oracleTracestate(values []string) (string, bool) {
	raw := 0
	for _, v := range values {
		raw += len(v) + 1
	}
	if raw > maxTracestateInput {
		return "", false
	}
	var members []string
	keys := map[string]bool{}
	for _, v := range values {
		for _, member := range strings.Split(v, ",") {
			member = strings.Trim(member, " \t")
			if member == "" {
				continue
			}
			match := oracleTracestateMember.FindStringSubmatch(member)
			if match == nil || keys[match[1]] {
				return "", false
			}
			keys[match[1]] = true
			members = append(members, member)
		}
	}
	joined := strings.Join(members, ",")
	if len(members) > maxTracestateMembers || len(joined) > maxTracestateBytes {
		return "", false
	}
	return joined, true
}

func FuzzTraceparent(f *testing.F) {
	for _, seed := range []string{
		testParent,
		"00-" + testTraceIDHex + "-" + testSpanIDHex + "-00",
		"cc-" + testTraceIDHex + "-" + testSpanIDHex + "-01-future",
		"cc-" + testTraceIDHex + "-" + testSpanIDHex + "-01.future",
		"ff-" + testTraceIDHex + "-" + testSpanIDHex + "-01",
		"00-00000000000000000000000000000000-" + testSpanIDHex + "-01",
		strings.ToUpper(testParent),
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		sc, ok := parseTraceparent(value)
		if want := oracleTraceparent(value); ok != want {
			t.Fatalf("parseTraceparent(%q) ok = %v, the grammar says %v", value, ok, want)
		}
		if !ok {
			if sc != (SpanContext{}) {
				t.Fatalf("a refused value returned %+v", sc)
			}
			return
		}
		if !sc.IsValid() || !sc.Remote || sc.TraceState != "" {
			t.Fatalf("accepted %q as %+v", value, sc)
		}
		written := sc.Traceparent()
		if strings.HasPrefix(value, "00-") && written != value {
			t.Fatalf("%q was written back as %q", value, written)
		}
		again, ok := parseTraceparent(written)
		if !ok || again != sc {
			t.Fatalf("%q round-tripped to %q, which parses as %+v, %v", value, written, again, ok)
		}
	})
}

func FuzzTracestate(f *testing.F) {
	for _, seed := range []string{
		"rojo=00f067aa0ba902b7,congo=t61rcWkgMzE",
		" rojo=1 ,\tcongo=2\t,,",
		"fw529a3039@dt=xyz",
		"rojo=1\ncongo=2",
		"rojo=1\nrojo=2",
		"a=b c",
		"a=b=c",
		"Rojo=1",
		"a=" + strings.Repeat("v", 256),
		strings.Repeat("k=v,", 40),
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		// A newline separates header values, which lets one fuzzed string
		// stand for a request carrying several tracestate headers.
		values := strings.Split(input, "\n")
		got, ok := parseTracestate(values)
		want, wantOK := oracleTracestate(values)
		if ok != wantOK || got != want {
			t.Fatalf("parseTracestate(%q) = %q, %v; the grammar says %q, %v", values, got, ok, want, wantOK)
		}
		if !ok {
			if got != "" {
				t.Fatalf("a refused header returned %q", got)
			}
			return
		}
		if len(got) > maxTracestateBytes || strings.Count(got, ",") >= maxTracestateMembers {
			t.Fatalf("accepted %d bytes and %d members", len(got), strings.Count(got, ",")+1)
		}
		again, ok := parseTracestate([]string{got})
		if !ok || again != got {
			t.Fatalf("%q did not survive a second parse: %q, %v", got, again, ok)
		}
	})
}
