package validate

import (
	"strings"
	"testing"
)

// TestDescribeCoversEveryKeyword walks the rules whose only job is to
// contribute a constraint, so that a rule which stops describing itself is
// caught rather than silently dropping out of the generated document.
func TestDescribeCoversEveryKeyword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		got    Constraints
		verify func(Constraints) bool
	}{
		{"min", Number().Min(3).Describe(), func(c Constraints) bool { return c.Minimum != nil && *c.Minimum == 3 }},
		{"max", Number().Max(9).Describe(), func(c Constraints) bool { return c.Maximum != nil && *c.Maximum == 9 }},
		{"url", String().URL().Describe(), func(c Constraints) bool { return c.Format == "uri" }},
		{"uuid", String().UUID().Describe(), func(c Constraints) bool { return c.Format == "uuid" }},
		{"value required", Value[string]().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"string required", String().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"number required", Number().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"slice required", Slice[string]().Required().Describe(), func(c Constraints) bool { return c.Required }},
		{"time required", Time().Required().Describe(), func(c Constraints) bool { return c.Required }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !tc.verify(tc.got) {
				t.Errorf("constraints = %+v, want the keyword described", tc.got)
			}
		})
	}
}

// TestRequiredAfterAnotherCheckSkipsIt covers the ordering where a check is
// written before Required and the value turns out to be empty. The earlier
// check has nothing to say, and Required is left to report the emptiness on its
// own rather than both complaining at once.
func TestRequiredAfterAnotherCheckSkipsIt(t *testing.T) {
	t.Parallel()
	err := String().MinLen(5).Required().Check("")
	if err == nil {
		t.Fatal("an empty required value was accepted")
	}
	if err.Error() != "is required" {
		t.Errorf("Check = %q, want only the presence failure", err)
	}
}

// TestBoundTargets checks that each rule set reports the field it was bound to,
// which is how the framework works out the name to report a failure under.
func TestBoundTargets(t *testing.T) {
	t.Parallel()

	text := "value"
	if got := String().For(&text).Target(); got != any(&text) {
		t.Errorf("string target = %v", got)
	}

	number := 1
	if got := Number().For(&number).Target(); got != any(&number) {
		t.Errorf("number target = %v", got)
	}

	values := []string{"a"}
	if got := Slice[string]().For(&values).Target(); got == nil {
		t.Error("slice target is nil after For")
	}

	role := "admin"
	if got := Value[string]().For(&role).Target(); got != any(&role) {
		t.Errorf("value target = %v", got)
	}
}

// TestResetReturnsARuleSetToTheStart covers the recycling that lets a
// Validation hand the same rule set to one request after another.
func TestResetReturnsARuleSetToTheStart(t *testing.T) {
	t.Parallel()
	text := "value"

	t.Run("string", func(t *testing.T) {
		t.Parallel()
		rules := String().As("label").Required().MinLen(3).For(&text)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Errorf("Reset left state behind: target %v, label %q, %+v",
				rules.Target(), rules.Label(), rules.Describe())
		}
		// The rule set is usable again, and its capacity survived.
		if err := rules.For(&text).MaxLen(1).Evaluate(); len(err) != 1 {
			t.Errorf("a reset rule set did not work again: %v", err)
		}
	})

	t.Run("number", func(t *testing.T) {
		t.Parallel()
		number := 5
		rules := Number().As("label").Required().Min(3).For(&number)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Error("Reset left state behind")
		}
	})

	t.Run("slice", func(t *testing.T) {
		t.Parallel()
		values := []string{"a"}
		rules := Slice[string]().As("label").Required().MaxItems(1).Each(String()).For(&values)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Error("Reset left state behind")
		}
		if !rules.DescribeElement().IsZero() {
			t.Error("Reset left the element rules behind")
		}
	})

	t.Run("value", func(t *testing.T) {
		t.Parallel()
		role := "admin"
		rules := Value[string]().As("label").Required().For(&role)
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" || !rules.Describe().IsZero() {
			t.Error("Reset left state behind")
		}
	})

	t.Run("time", func(t *testing.T) {
		t.Parallel()
		rules := Time().As("label").Required()
		rules.Reset()
		if rules.Target() != nil || rules.Label() != "" {
			t.Error("Reset left state behind")
		}
	})
}

// TestOptionalCollectionSkipsItsChecks covers the generic runner's skip, which
// is what makes an absent collection acceptable to a rule set that bounds its
// size.
func TestOptionalCollectionSkipsItsChecks(t *testing.T) {
	t.Parallel()
	if err := Slice[string]().MinItems(2).Check(nil); err != nil {
		t.Errorf("Check(nil) on an optional collection = %v, want it accepted", err)
	}
	if err := Slice[string]().MinItems(2).Check([]string{"a"}); err == nil {
		t.Error("a supplied collection skipped its check")
	}
	if err := Value[string]().Equal("x").Check(""); err != nil {
		t.Errorf("Check on an optional value = %v, want it accepted", err)
	}
}

// A value a rule approves is used as it stands, so a rule is no better than the
// characters it lets a "valid" value carry.
func TestFormatsRejectWhatOnlyLooksValid(t *testing.T) {
	t.Parallel()
	const (
		canonical = "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"
		bidi      = "\u202e"
		zeroWidth = "\u200b"
	)

	accepts(t, "isBase64", isBase64,
		[]string{"aGk=", "aGVsbG8="},
		[]string{"aGk=\r\n", "aG\nk=", "aGk=\n", "\raGk="})

	accepts(t, "isCanonicalUUID", isCanonicalUUID,
		[]string{canonical, strings.ToUpper(canonical)},
		[]string{"urn:uuid:" + canonical, "{" + canonical + "}", strings.ReplaceAll(canonical, "-", ""),
			canonical + "\n", "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b3", "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b3g"})

	accepts(t, "URL", func(v string) bool { return String().URL().Check(v) == nil },
		[]string{"https://example.com/", "http://example.com:8080/x", "https://[::1]:443/", "http://example.com:65535/"},
		[]string{"http://[::1]:99999/", "http://example.com:65536/", "https://example.com:99999",
			"http://ex" + bidi + "ample.com/", "http://example.com/a" + zeroWidth + "b", "https://example.com/" + bidi})
	accepts(t, "HTTPS", func(v string) bool { return String().HTTPS().Check(v) == nil },
		[]string{"https://example.com:8443/"},
		[]string{"https://example.com:99999/", "https://exa" + zeroWidth + "mple.com/"})
	accepts(t, "URLWithSchemes", func(v string) bool { return String().URLWithSchemes("ftp").Check(v) == nil },
		[]string{"ftp://example.com:21/"},
		[]string{"ftp://example.com:70000/", "ftp://example.com/a" + bidi})

	// A field of nothing that renders is as blank as one of spaces.
	accepts(t, "isBlank", isBlank,
		[]string{"", " ", "\u200b", "\u200d\u200b", "\u3164", " \u200b\u00a0\u2800 ", "\u115f\u1160", "\ufeff", "\u2028"},
		[]string{"a", "a\u200b", "\u200ba", "0", "\u4f60"})
}
