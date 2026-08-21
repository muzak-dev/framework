package validate

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// check is the shape every rule test takes: a value, and the issue the rule set
// should report about it, or the empty string when it should accept the value.
type check struct {
	name  string
	value string
	issue string
}

// runStringCases applies a rule set to each case and compares the outcome.
func runStringCases(t *testing.T, build func() *StringRules, cases []check) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := build().Check(tc.value)
			switch {
			case tc.issue == "" && err != nil:
				t.Errorf("Check(%q) = %v, want it accepted", tc.value, err)
			case tc.issue != "" && err == nil:
				t.Errorf("Check(%q) was accepted, want %q", tc.value, tc.issue)
			case tc.issue != "" && err.Error() != tc.issue:
				t.Errorf("Check(%q) = %q, want %q", tc.value, err, tc.issue)
			}
		})
	}
}

func TestStringRequired(t *testing.T) {
	t.Parallel()
	runStringCases(t, func() *StringRules { return String().Required() }, []check{
		{"present", "value", ""},
		{"empty", "", "is required"},
		{"whitespace is not empty on its own", " ", ""},
	})
}

func TestStringLength(t *testing.T) {
	t.Parallel()
	runStringCases(t, func() *StringRules { return String().Required().MinLen(3) }, []check{
		{"long enough", "abc", ""},
		{"too short", "ab", "must be at least 3 characters"},
	})
	runStringCases(t, func() *StringRules { return String().Required().MaxLen(3) }, []check{
		{"short enough", "abc", ""},
		{"too long", "abcd", "must be at most 3 characters"},
	})
	runStringCases(t, func() *StringRules { return String().Required().Len(3) }, []check{
		{"exact", "abc", ""},
		{"too short", "ab", "must be exactly 3 characters"},
		{"too long", "abcd", "must be exactly 3 characters"},
	})
	// Lengths are counted in characters rather than bytes.
	runStringCases(t, func() *StringRules { return String().Required().MaxLen(3) }, []check{
		{"multibyte counted as characters", "\u00e9\u00e9\u00e9", ""},
		{"multibyte over the limit", "\u00e9\u00e9\u00e9\u00e9", "must be at most 3 characters"},
	})
	// A limit of one reads in the singular.
	runStringCases(t, func() *StringRules { return String().Required().MinLen(1).MaxLen(1).Len(1) }, []check{
		{"singular wording", "ab", "must be at most 1 character"},
	})
}

func TestStringFormats(t *testing.T) {
	t.Parallel()
	runStringCases(t, func() *StringRules { return String().Required().Email() }, []check{
		{"plain", "rick@example.test", ""},
		{"no at sign", "rick.example.test", "must be a valid email address"},
		{"display name is not a bare address", "Rick <rick@example.test>", "must be a valid email address"},
		{"empty local part", "@example.test", "must be a valid email address"},
	})
	runStringCases(t, func() *StringRules { return String().Required().URL() }, []check{
		{"absolute", "https://example.test/path", ""},
		{"no host", "https://", "must be a valid absolute URL"},
		{"relative", "/just/a/path", "must be a valid absolute URL"},
		{"not a url at all", "not a url", "must be a valid absolute URL"},
	})
	runStringCases(t, func() *StringRules { return String().Required().UUID() }, []check{
		{"canonical", "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31", ""},
		{"nonsense", "not-a-uuid", "must be a valid UUID"},
	})
}

func TestStringMatches(t *testing.T) {
	t.Parallel()
	runStringCases(t, func() *StringRules { return String().Required().Matches(`^[a-z]+$`) }, []check{
		{"matching", "abc", ""},
		{"not matching", "ABC", "is not in the expected format"},
	})
}

func TestStringMatchesRejectsABadPatternAtDeclaration(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("a malformed pattern was accepted, want a panic when the rule is declared")
		}
	}()
	String().Matches(`([`)
}

func TestStringSets(t *testing.T) {
	t.Parallel()
	runStringCases(t, func() *StringRules { return String().Required().OneOf("admin", "editor", "viewer") }, []check{
		{"permitted", "editor", ""},
		{"not permitted", "wizard", `must be one of "admin", "editor" or "viewer"`},
	})
	runStringCases(t, func() *StringRules { return String().Required().OneOf("only") }, []check{
		{"single alternative reads plainly", "other", `must be one of "only"`},
	})
	runStringCases(t, func() *StringRules { return String().Required().OneOf() }, []check{
		{"an empty set permits nothing", "anything", "must be one of no permitted value"},
	})
	runStringCases(t, func() *StringRules { return String().Required().NotOneOf("root", "admin") }, []check{
		{"allowed", "rick", ""},
		{"reserved", "root", "is not available"},
	})
}

func TestStringComparisons(t *testing.T) {
	t.Parallel()
	runStringCases(t, func() *StringRules { return String().Required().Equal("secret") }, []check{
		{"same", "secret", ""},
		{"different", "guess", "does not match"},
	})
	runStringCases(t, func() *StringRules { return String().Required().Prefix("api-") }, []check{
		{"has it", "api-key", ""},
		{"lacks it", "key", `must begin with "api-"`},
	})
	runStringCases(t, func() *StringRules { return String().Required().Suffix(".test") }, []check{
		{"has it", "example.test", ""},
		{"lacks it", "example.com", `must end with ".test"`},
	})
	runStringCases(t, func() *StringRules { return String().Required().Contains("-") }, []check{
		{"has it", "a-b", ""},
		{"lacks it", "ab", `must contain "-"`},
	})
}

func TestStringMust(t *testing.T) {
	t.Parallel()
	shouty := func(s string) error {
		if s != strings.ToUpper(s) {
			return errors.New("must be shouted")
		}
		return nil
	}
	runStringCases(t, func() *StringRules { return String().Required().Must(shouty) }, []check{
		{"shouted", "LOUD", ""},
		{"quiet", "quiet", "must be shouted"},
	})
}

func TestStringTransforms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules *StringRules
		in    string
		want  string
	}{
		{"trim", String().Trim(), "  padded  ", "padded"},
		{"lower", String().Lower(), "SHOUTED", "shouted"},
		{"upper", String().Upper(), "quiet", "QUIET"},
		{"chained", String().Trim().Lower(), "  MiXeD  ", "mixed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value := tc.in
			if problems := tc.rules.applyTo(&value); len(problems) != 0 {
				t.Fatalf("transforms reported %v", problems)
			}
			if value != tc.want {
				t.Errorf("value = %q, want %q", value, tc.want)
			}
		})
	}
}

// TestStringTransformsRunBeforeChecks is what makes Trim().Required() reject a
// field holding nothing but spaces.
func TestStringTransformsRunBeforeChecks(t *testing.T) {
	t.Parallel()
	err := String().Trim().Required().Check("   ")
	if err == nil || err.Error() != "is required" {
		t.Errorf("Check = %v, want the trimmed value to count as empty", err)
	}
}

// TestStringOptionalSkipsItsChecks is what makes a field optional: the rules
// have nothing to say about a value nobody sent.
func TestStringOptionalSkipsItsChecks(t *testing.T) {
	t.Parallel()
	if err := String().MinLen(10).Email().Check(""); err != nil {
		t.Errorf("Check on an empty optional value = %v, want it accepted", err)
	}
	if err := String().MinLen(10).Check("short"); err == nil {
		t.Error("a supplied value skipped its checks")
	}
}

// TestStringStopsAtTheFirstFailure keeps one empty field from producing a
// paragraph of complaints.
func TestStringStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	err := String().Required().MinLen(10).Email().Check("")
	if err == nil || err.Error() != "is required" {
		t.Errorf("Check = %v, want only the presence failure", err)
	}
}

func TestStringMessageOverridesTheLastCheck(t *testing.T) {
	t.Parallel()
	rules := String().Required().Email().Message("we need a real address")

	if err := rules.Check("nonsense"); err == nil || err.Error() != "we need a real address" {
		t.Errorf("Check = %v, want the override", err)
	}
	// The override belongs to the email check, so the presence failure keeps
	// its own wording.
	if err := rules.Check(""); err == nil || err.Error() != "is required" {
		t.Errorf("Check = %v, want the presence check untouched", err)
	}
}

// TestMessageSkipsTransforms checks that a message written after a transform
// still lands on the check before it, since a transform cannot fail.
func TestMessageSkipsTransforms(t *testing.T) {
	t.Parallel()
	rules := String().Required().Trim().Message("say something")
	if err := rules.Check(""); err == nil || err.Error() != "say something" {
		t.Errorf("Check = %v, want the message on the presence check", err)
	}
}

// TestMessageWithNoCheckIsIgnored covers a message written before any check.
func TestMessageWithNoCheckIsIgnored(t *testing.T) {
	t.Parallel()
	rules := String().Trim().Message("nothing to attach to").Required()
	if err := rules.Check(""); err == nil || err.Error() != "is required" {
		t.Errorf("Check = %v, want the default wording", err)
	}
}

func TestStringLabel(t *testing.T) {
	t.Parallel()
	if got := String().As("date_of_birth").Label(); got != "date_of_birth" {
		t.Errorf("Label = %q", got)
	}
	if got := String().Label(); got != "" {
		t.Errorf("Label = %q, want empty by default", got)
	}
}

func TestStringConstraints(t *testing.T) {
	t.Parallel()
	c := String().Required().MinLen(3).MaxLen(30).Matches(`^x`).Email().Describe()

	if !c.Required {
		t.Error("Required is not described")
	}
	if c.MinLength == nil || *c.MinLength != 3 {
		t.Errorf("MinLength = %v", c.MinLength)
	}
	if c.MaxLength == nil || *c.MaxLength != 30 {
		t.Errorf("MaxLength = %v", c.MaxLength)
	}
	if c.Pattern != `^x` {
		t.Errorf("Pattern = %q", c.Pattern)
	}
	if c.Format != "email" {
		t.Errorf("Format = %q", c.Format)
	}

	enum := String().OneOf("a", "b").Describe()
	if len(enum.Enum) != 2 {
		t.Errorf("Enum = %v", enum.Enum)
	}

	exact := String().Len(4).Describe()
	if exact.MinLength == nil || exact.MaxLength == nil || *exact.MinLength != 4 || *exact.MaxLength != 4 {
		t.Errorf("Len did not describe both bounds: %+v", exact)
	}

	// A rule set of nothing but Must says nothing a document can carry.
	if !String().Must(func(string) error { return nil }).Describe().IsZero() {
		t.Error("an opaque rule contributed a constraint")
	}
}

func TestStringEvaluateWithoutATarget(t *testing.T) {
	t.Parallel()
	// An unbound rule set has nothing to read, so it reports nothing rather
	// than dereferencing a pointer it does not have.
	if problems := String().Required().Evaluate(); len(problems) != 0 {
		t.Errorf("Evaluate on an unbound rule set = %v", problems)
	}
	if got := String().Target(); got != nil {
		t.Errorf("Target = %v, want nil", got)
	}
}

func TestStringEvaluateThroughAPointerField(t *testing.T) {
	t.Parallel()
	text := "  Padded  "
	field := &text
	rules := String().Trim().Required().For(&field)

	if problems := rules.Evaluate(); len(problems) != 0 {
		t.Fatalf("Evaluate = %v", problems)
	}
	if text != "Padded" {
		t.Errorf("the transform did not reach the value: %q", text)
	}

	var absent *string
	if problems := String().Required().For(&absent).Evaluate(); len(problems) != 0 {
		t.Errorf("a nil pointer field reported %v, want it skipped", problems)
	}
}

func TestStringEvaluateReportsAFailure(t *testing.T) {
	t.Parallel()
	value := ""
	problems := String().Required().For(&value).Evaluate()
	if len(problems) != 1 || problems[0].Issue != "is required" {
		t.Errorf("Evaluate = %v", problems)
	}
}

func TestResolveRejectsWhatItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target any
	}{
		{"nil interface", nil},
		{"not a pointer", "plain"},
		{"nil pointer", (*string)(nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := resolve(tc.target); ok {
				t.Errorf("resolve(%v) succeeded, want it refused", tc.target)
			}
		})
	}
	value := "x"
	if _, ok := resolve(&value); !ok {
		t.Error("resolve refused a plain field pointer")
	}
}

func TestTimeRules(t *testing.T) {
	t.Parallel()
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	middle := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		rules *TimeRules
		value time.Time
		ok    bool
	}{
		{"required present", Time().Required(), middle, true},
		{"required absent", Time().Required(), time.Time{}, false},
		{"before", Time().Required().Before(late), middle, true},
		{"not before", Time().Required().Before(early), middle, false},
		{"after", Time().Required().After(early), middle, true},
		{"not after", Time().Required().After(late), middle, false},
		{"between", Time().Required().Between(early, late), middle, true},
		{"outside", Time().Required().Between(middle, late), early, false},
		{"must", Time().Required().Must(func(time.Time) error { return errors.New("no") }), middle, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.rules.Check(tc.value)
			if tc.ok != (err == nil) {
				t.Errorf("Check = %v, want ok = %v", err, tc.ok)
			}
		})
	}

	if got := Time().Describe().Format; got != "date-time" {
		t.Errorf("Format = %q, want date-time", got)
	}
	if got := Time().As("when").Label(); got != "when" {
		t.Errorf("Label = %q", got)
	}
	if problems := Time().Required().Evaluate(); len(problems) != 0 {
		t.Errorf("an unbound time rule set reported %v", problems)
	}

	moment := middle
	if problems := Time().Required().Before(early).For(&moment).Evaluate(); len(problems) != 1 {
		t.Errorf("Evaluate = %v, want one failure", problems)
	}
	if got := Time().Required().Message("pick a date").Check(time.Time{}); got == nil || got.Error() != "pick a date" {
		t.Errorf("Check = %v, want the override", got)
	}
	if got := Time().Required().For(&moment).Target(); got == nil {
		t.Error("Target = nil after For")
	}
}
