package validate

import (
	"slices"
	"testing"
	"time"
)

// TestKindsCoversEveryRule checks the enumeration against the table it is
// derived from, and that it is stable.
func TestKindsCoversEveryRule(t *testing.T) {
	t.Parallel()
	kinds := Kinds()
	if len(kinds) == 0 {
		t.Fatal("Kinds returned nothing")
	}
	if !slices.IsSorted(kinds) {
		t.Errorf("Kinds returned %v, want them sorted so that the order is the same on every run", kinds)
	}
	for _, kind := range kindsOf {
		if !slices.Contains(kinds, kind) {
			t.Errorf("Kinds omits %q, which a rule reports", kind)
		}
	}
	// Reported before any rule runs, so it is not in the table but is still a
	// failure the framework has to have a message for.
	if !slices.Contains(kinds, KindNotANumber) {
		t.Error("Kinds omits not_a_number")
	}
	// A rule appearing twice in the table is listed once.
	seen := map[Kind]bool{}
	for _, kind := range kinds {
		if seen[kind] {
			t.Errorf("Kinds lists %q more than once", kind)
		}
		seen[kind] = true
	}
}

// TestFailuresCarryTheirRule checks what each rule reports beyond its English,
// which is what a translation of it is looked up and filled in with.
func TestFailuresCarryTheirRule(t *testing.T) {
	t.Parallel()
	epoch := time.Unix(0, 0).UTC()
	later := epoch.Add(time.Hour)

	cases := []struct {
		name string
		got  []Problem
		kind Kind
		args []any
	}{
		{name: "required", got: String().Required().For(ptr("")).Evaluate(), kind: KindBlank},
		{name: "min length", got: String().MinLen(5).For(ptr("ab")).Evaluate(),
			kind: KindTooShort, args: []any{"count", 5}},
		{name: "max length", got: String().MaxLen(1).For(ptr("ab")).Evaluate(),
			kind: KindTooLong, args: []any{"count", 1}},
		{name: "exact length", got: String().Len(4).For(ptr("ab")).Evaluate(),
			kind: KindWrongLength, args: []any{"count", 4}},
		{name: "email", got: String().Email().For(ptr("nope")).Evaluate(), kind: KindEmail},
		{name: "url", got: String().URL().For(ptr("nope")).Evaluate(), kind: KindURL},
		{name: "uuid", got: String().UUID().For(ptr("nope")).Evaluate(), kind: KindUUID},
		{name: "pattern", got: String().Matches(`^[0-9]+$`).For(ptr("ab")).Evaluate(), kind: KindInvalid},
		{name: "one of", got: String().OneOf("red").For(ptr("blue")).Evaluate(),
			kind: KindInclusion, args: []any{"list", `"red"`}},
		{name: "not one of", got: String().NotOneOf("taken").For(ptr("taken")).Evaluate(), kind: KindExclusion},
		{name: "equal", got: String().Equal("a").For(ptr("b")).Evaluate(), kind: KindConfirmation},
		{name: "prefix", got: String().Prefix("pre").For(ptr("no")).Evaluate(),
			kind: KindPrefix, args: []any{"value", `"pre"`}},
		{name: "suffix", got: String().Suffix("post").For(ptr("no")).Evaluate(),
			kind: KindSuffix, args: []any{"value", `"post"`}},
		{name: "contains", got: String().Contains("mid").For(ptr("no")).Evaluate(),
			kind: KindContains, args: []any{"value", `"mid"`}},
		{name: "minimum", got: Number().Min(10).For(ptr(1)).Evaluate(),
			kind: KindGreaterThanOrEqualTo, args: []any{"count", float64(10)}},
		{name: "maximum", got: Number().Max(1).For(ptr(5)).Evaluate(),
			kind: KindLessThanOrEqualTo, args: []any{"count", float64(1)}},
		{name: "between", got: Number().Between(10, 20).For(ptr(50)).Evaluate(),
			kind: KindBetween, args: []any{"min", "10", "max", "20"}},
		{name: "positive", got: Number().Positive().For(ptr(-1)).Evaluate(), kind: KindPositive},
		{name: "negative", got: Number().Negative().For(ptr(1)).Evaluate(), kind: KindNegative},
		{name: "multiple of", got: Number().MultipleOf(7).For(ptr(5)).Evaluate(),
			kind: KindMultipleOf, args: []any{"count", float64(7)}},
		{name: "not a number", got: Number().For(ptr(mustNaN())).Evaluate(), kind: KindNotANumber},
		{name: "too few items", got: Slice[string]().MinItems(3).For(&[]string{"a"}).Evaluate(),
			kind: KindTooFewItems, args: []any{"count", 3}},
		{name: "too many items", got: Slice[string]().MaxItems(1).For(&[]string{"a", "b"}).Evaluate(),
			kind: KindTooManyItems, args: []any{"count", 1}},
		{name: "repeated", got: Slice[string]().Unique().For(&[]string{"a", "a"}).Evaluate(),
			kind: KindTaken, args: []any{"value", "a"}},
		{name: "before", got: Time().Before(epoch).For(&later).Evaluate(),
			kind: KindBefore, args: []any{"time", epoch.Format(time.RFC3339)}},
		{name: "after", got: Time().After(later).For(&epoch).Evaluate(),
			kind: KindAfter, args: []any{"time", later.Format(time.RFC3339)}},
		{name: "between times", got: Time().Between(epoch, later).For(ptr(later.Add(time.Hour))).Evaluate(),
			kind: KindTimeBetween,
			args: []any{"earliest", epoch.Format(time.RFC3339), "latest", later.Format(time.RFC3339)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.got) != 1 {
				t.Fatalf("the rule reported %d problems, want exactly one", len(tc.got))
			}
			problem := tc.got[0]
			if problem.Issue == "" {
				t.Error("the problem carries no English, which every caller relies on")
			}
			if problem.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", problem.Kind, tc.kind)
			}
			if !sameArgs(problem.Args, tc.args) {
				t.Errorf("Args = %v, want %v", problem.Args, tc.args)
			}
		})
	}
}

// sameArgs compares two alternating name and value lists.
func sameArgs(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestMessageOverridesClearTheRule checks the two ways an override behaves.
//
// A literal message replaces the words and drops the rule, because a caller who
// wrote a sentence asked for that sentence. A key keeps the rule, because the
// key still has to be filled in with what the rule knows.
func TestMessageOverridesClearTheRule(t *testing.T) {
	t.Parallel()

	literal := String().MinLen(5).Message("too wee").For(ptr("ab")).Evaluate()
	if len(literal) != 1 || literal[0].Issue != "too wee" {
		t.Fatalf("Message gave %v, want the sentence it was handed", literal)
	}
	if literal[0].Kind != KindNone || literal[0].Key != "" {
		t.Errorf("Message left %q and %q behind, want both cleared", literal[0].Kind, literal[0].Key)
	}

	keyed := String().MinLen(5).MessageKey("my.key", "extra", "value").For(ptr("ab")).Evaluate()
	if len(keyed) != 1 {
		t.Fatalf("MessageKey gave %v, want one problem", keyed)
	}
	if keyed[0].Key != "my.key" {
		t.Errorf("Key = %q, want the key that was named", keyed[0].Key)
	}
	if keyed[0].Kind != KindTooShort {
		t.Errorf("Kind = %q, want the rule kept so its values can fill the key", keyed[0].Kind)
	}
	if keyed[0].Issue != "must be at least 5 characters" {
		t.Errorf("Issue = %q, want the rule's English left as the fallback", keyed[0].Issue)
	}
	if !sameArgs(keyed[0].Args, []any{"count", 5, "extra", "value"}) {
		t.Errorf("Args = %v, want the rule's own followed by the ones named", keyed[0].Args)
	}
}

// TestMessageKeyOnEveryRuleSet checks that the override exists on all five
// families, since a rule set that lacked it would be the one an application
// happened to need.
func TestMessageKeyOnEveryRuleSet(t *testing.T) {
	t.Parallel()
	epoch := time.Unix(0, 0).UTC()

	cases := map[string][]Problem{
		"string":     String().Required().MessageKey("k").For(ptr("")).Evaluate(),
		"number":     Number().Min(5).MessageKey("k").For(ptr(1)).Evaluate(),
		"slice":      Slice[string]().MinItems(2).MessageKey("k").For(&[]string{"a"}).Evaluate(),
		"value":      Value[string]().OneOf("a").MessageKey("k").For(ptr("b")).Evaluate(),
		"time bound": Time().After(epoch.Add(time.Hour)).MessageKey("k").For(&epoch).Evaluate(),
	}
	for name, problems := range cases {
		if len(problems) != 1 || problems[0].Key != "k" {
			t.Errorf("%s: MessageKey gave %v, want the key recorded", name, problems)
		}
	}
}

// TestMessageKeySkipsTransforms checks that an override attaches to the check
// written before it rather than to a transform, which is the same rule
// [StringRules.Message] follows.
func TestMessageKeySkipsTransforms(t *testing.T) {
	t.Parallel()
	problems := String().Trim().Required().MessageKey("k").For(ptr("   ")).Evaluate()
	if len(problems) != 1 || problems[0].Key != "k" {
		t.Errorf("the override landed on %v, want the presence check", problems)
	}
}

// ptr is the address of a value, for a rule set bound to a literal.
func ptr[T any](v T) *T { return &v }

// mustNaN returns a value no numeric field legitimately holds.
func mustNaN() float64 {
	var zero float64
	return zero / zero
}

// TestFormatFailuresCarryTheirRule is [TestFailuresCarryTheirRule] for the
// rules added alongside the formats, held to the same contract: English that is
// always there, the rule it came from, and the values that rule's message
// interpolates.
func TestFormatFailuresCarryTheirRule(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  []Problem
		kind Kind
		args []any
	}{
		{name: "https", got: String().HTTPS().For(ptr("http://x.dev")).Evaluate(), kind: KindHTTPS},
		{name: "url scheme", got: String().URLWithSchemes("s3").For(ptr("https://x.dev")).Evaluate(),
			kind: KindURLScheme, args: []any{"list", `"s3"`}},
		{name: "host", got: String().Host().For(ptr("-bad.example")).Evaluate(), kind: KindHost},
		{name: "ip", got: String().IP().For(ptr("nope")).Evaluate(), kind: KindIP},
		{name: "ipv4", got: String().IPv4().For(ptr("::1")).Evaluate(), kind: KindIPv4},
		{name: "ipv6", got: String().IPv6().For(ptr("127.0.0.1")).Evaluate(), kind: KindIPv6},
		{name: "cidr", got: String().CIDR().For(ptr("10.0.0.0")).Evaluate(), kind: KindCIDR},
		{name: "mac", got: String().MAC().For(ptr("nope")).Evaluate(), kind: KindMAC},
		{name: "not matches", got: String().MatchesNot(`^tmp-`).For(ptr("tmp-1")).Evaluate(),
			kind: KindNotMatches},
		{name: "not blank", got: String().NotBlank().For(ptr("   ")).Evaluate(), kind: KindNotBlank},
		{name: "no control", got: String().NoControl().For(ptr("a\rb")).Evaluate(), kind: KindNoControl},
		{name: "alpha", got: String().Alpha().For(ptr("a1")).Evaluate(), kind: KindAlpha},
		{name: "alphanumeric", got: String().Alphanumeric().For(ptr("a 1")).Evaluate(),
			kind: KindAlphanumeric},
		{name: "numeric", got: String().Numeric().For(ptr("12a")).Evaluate(), kind: KindNumeric},
		{name: "ascii", got: String().ASCII().For(ptr("caf\u00e9")).Evaluate(), kind: KindASCII},
		{name: "slug", got: String().Slug().For(ptr("Not A Slug")).Evaluate(), kind: KindSlug},
		{name: "hex", got: String().Hex().For(ptr("ghij")).Evaluate(), kind: KindHex},
		{name: "hex colour", got: String().HexColour().For(ptr("fff")).Evaluate(), kind: KindHexColour},
		{name: "base64", got: String().Base64().For(ptr("***")).Evaluate(), kind: KindBase64},
		{name: "json", got: String().JSON().For(ptr("{")).Evaluate(), kind: KindJSON},
		{name: "semver", got: String().Semver().For(ptr("1.4")).Evaluate(), kind: KindSemver},
		{name: "e164", got: String().E164().For(ptr("0555")).Evaluate(), kind: KindE164},
		{name: "language tag", got: String().LanguageTag().For(ptr("pt_BR")).Evaluate(),
			kind: KindLanguageTag},
		{name: "timezone", got: String().Timezone().For(ptr("Mars/Olympus")).Evaluate(),
			kind: KindTimezone},
		{name: "country code", got: String().CountryCode().For(ptr("tr")).Evaluate(),
			kind: KindCountryCode},
		{name: "currency code", got: String().CurrencyCode().For(ptr("try")).Evaluate(),
			kind: KindCurrencyCode},
		{name: "equal fold", got: String().EqualFold("secret").For(ptr("other")).Evaluate(),
			kind: KindConfirmation},
		{name: "min bytes", got: String().MinBytes(5).For(ptr("ab")).Evaluate(),
			kind: KindMinBytes, args: []any{"count", 5}},
		{name: "max bytes", got: String().MaxBytes(2).For(ptr("abcd")).Evaluate(),
			kind: KindMaxBytes, args: []any{"count", 2}},

		{name: "greater than", got: Number().GreaterThan(10).For(ptr(1)).Evaluate(),
			kind: KindGreaterThan, args: []any{"count", float64(10)}},
		{name: "less than", got: Number().LessThan(1).For(ptr(5)).Evaluate(),
			kind: KindLessThan, args: []any{"count", float64(1)}},
		{name: "non negative", got: Number().NonNegative().For(ptr(-1)).Evaluate(),
			kind: KindNonNegative},
		{name: "non positive", got: Number().NonPositive().For(ptr(1)).Evaluate(),
			kind: KindNonPositive},
		{name: "whole", got: Number().Whole().For(ptr(1.5)).Evaluate(), kind: KindWhole},
		{name: "port", got: Number().Port().For(ptr(70000)).Evaluate(), kind: KindPort},
		{name: "one of", got: Number().OneOf(1, 2).For(ptr(5)).Evaluate(),
			kind: KindInclusion, args: []any{"list", "1 or 2"}},

		{name: "items", got: Slice[string]().Items(3).For(&[]string{"a"}).Evaluate(),
			kind: KindWrongItems, args: []any{"count", 3}},
		{name: "not empty", got: Slice[string]().NotEmpty().For(&[]string{}).Evaluate(),
			kind: KindNotEmpty},
		{name: "contains", got: Slice[string]().Contains("read").For(&[]string{"write"}).Evaluate(),
			kind: KindContainsItem, args: []any{"value", "read"}},
		{name: "excludes", got: Slice[string]().Excludes("*").For(&[]string{"*"}).Evaluate(),
			kind: KindExcludesItem, args: []any{"value", "*"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.got) != 1 {
				t.Fatalf("the rule reported %d problems, want exactly one", len(tc.got))
			}
			problem := tc.got[0]
			if problem.Issue == "" {
				t.Error("the problem carries no English, which every caller relies on")
			}
			if problem.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", problem.Kind, tc.kind)
			}
			if !sameArgs(problem.Args, tc.args) {
				t.Errorf("Args = %v, want %v", problem.Args, tc.args)
			}
		})
	}
}

// TestTimeFailuresCarryTheirRule covers the bounds against now, which need a
// moment set up relative to the clock rather than a literal.
func TestTimeFailuresCarryTheirRule(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-2 * time.Hour)
	future := time.Now().Add(2 * time.Hour)

	cases := []struct {
		name string
		got  []Problem
		kind Kind
		args []any
	}{
		{name: "past", got: Time().Past().For(&future).Evaluate(), kind: KindPast},
		{name: "future", got: Time().Future().For(&past).Evaluate(), kind: KindFuture},
		{name: "within", got: Time().Within(time.Minute).For(&past).Evaluate(),
			kind: KindWithin, args: []any{"duration", "1m0s"}},
	}
	for _, tc := range cases {
		if len(tc.got) != 1 {
			t.Errorf("%s reported %d problems, want one", tc.name, len(tc.got))
			continue
		}
		if tc.got[0].Kind != tc.kind {
			t.Errorf("%s: Kind = %q, want %q", tc.name, tc.got[0].Kind, tc.kind)
		}
		if !sameArgs(tc.got[0].Args, tc.args) {
			t.Errorf("%s: Args = %v, want %v", tc.name, tc.got[0].Args, tc.args)
		}
	}

	// And the values each of them accepts, so that a rule which rejected
	// everything would fail here rather than look correct.
	if got := Time().Past().For(&past).Evaluate(); len(got) != 0 {
		t.Errorf("Past rejected a moment already gone: %v", got)
	}
	if got := Time().Future().For(&future).Evaluate(); len(got) != 0 {
		t.Errorf("Future rejected a moment still to come: %v", got)
	}
	near := time.Now().Add(-time.Second)
	if got := Time().Within(time.Minute).For(&near).Evaluate(); len(got) != 0 {
		t.Errorf("Within rejected a moment inside the window: %v", got)
	}
	if got := Time().Within(time.Minute).For(&future).Evaluate(); len(got) != 1 {
		t.Errorf("Within accepted a moment ahead of the window: %v", got)
	}
}

// TestNewRulesDescribeThemselves checks what the added rules contribute to the
// generated document.
//
// A rule that enforces something and describes nothing leaves a client guessing;
// one that describes something it does not enforce is worse. Both are easy to
// get wrong when the mapping is a switch arm written apart from the check.
func TestNewRulesDescribeThemselves(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		got   Constraints
		check func(*testing.T, Constraints)
	}{
		{name: "https", got: String().HTTPS().Describe(), check: wantFormat("uri")},
		{name: "url scheme", got: String().URLWithSchemes("s3").Describe(), check: wantFormat("uri")},
		{name: "host", got: String().Host().Describe(), check: wantFormat("hostname")},
		{name: "ip", got: String().IP().Describe(), check: wantFormat("ip")},
		{name: "ipv4", got: String().IPv4().Describe(), check: wantFormat("ipv4")},
		{name: "ipv6", got: String().IPv6().Describe(), check: wantFormat("ipv6")},
		{name: "cidr", got: String().CIDR().Describe(), check: wantFormat("cidr")},
		{name: "mac", got: String().MAC().Describe(), check: wantFormat("mac")},
		{name: "base64", got: String().Base64().Describe(), check: wantFormat("byte")},

		{name: "alpha", got: String().Alpha().Describe(), check: wantPattern(patternAlpha)},
		{name: "alphanumeric", got: String().Alphanumeric().Describe(), check: wantPattern(patternAlphanumeric)},
		{name: "numeric", got: String().Numeric().Describe(), check: wantPattern(patternNumeric)},
		{name: "ascii", got: String().ASCII().Describe(), check: wantPattern(patternASCII)},
		{name: "slug", got: String().Slug().Describe(), check: wantPattern(patternSlug)},
		{name: "hex", got: String().Hex().Describe(), check: wantPattern(patternHex)},
		{name: "hex colour", got: String().HexColour().Describe(), check: wantPattern(patternHexColour)},
		{name: "e164", got: String().E164().Describe(), check: wantPattern(patternE164)},
		{name: "country code", got: String().CountryCode().Describe(), check: wantPattern(patternCountryCode)},
		{name: "currency code", got: String().CurrencyCode().Describe(), check: wantPattern(patternCurrencyCode)},

		{
			name: "greater than is exclusive",
			got:  Number().GreaterThan(10).Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.ExclusiveMinimum == nil || *c.ExclusiveMinimum != 10 {
					t.Errorf("ExclusiveMinimum = %v, want 10", c.ExclusiveMinimum)
				}
				if c.Minimum != nil {
					t.Errorf("Minimum = %v, want nothing: the bound itself is refused", c.Minimum)
				}
			},
		},
		{
			name: "less than is exclusive",
			got:  Number().LessThan(10).Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.ExclusiveMaximum == nil || *c.ExclusiveMaximum != 10 {
					t.Errorf("ExclusiveMaximum = %v, want 10", c.ExclusiveMaximum)
				}
				if c.Maximum != nil {
					t.Errorf("Maximum = %v, want nothing", c.Maximum)
				}
			},
		},
		{
			name: "non negative admits its bound",
			got:  Number().NonNegative().Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.Minimum == nil || *c.Minimum != 0 {
					t.Errorf("Minimum = %v, want 0", c.Minimum)
				}
				if c.ExclusiveMinimum != nil {
					t.Errorf("ExclusiveMinimum = %v, want nothing: zero is allowed", c.ExclusiveMinimum)
				}
			},
		},
		{
			name: "non positive admits its bound",
			got:  Number().NonPositive().Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.Maximum == nil || *c.Maximum != 0 {
					t.Errorf("Maximum = %v, want 0", c.Maximum)
				}
			},
		},
		{
			name: "whole",
			got:  Number().Whole().Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.MultipleOf == nil || *c.MultipleOf != 1 {
					t.Errorf("MultipleOf = %v, want 1", c.MultipleOf)
				}
			},
		},
		{
			name: "port",
			got:  Number().Port().Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.Minimum == nil || *c.Minimum != 1 || c.Maximum == nil || *c.Maximum != 65535 {
					t.Errorf("bounds = %v to %v, want 1 to 65535", c.Minimum, c.Maximum)
				}
			},
		},
		{
			name: "numeric enum",
			got:  Number().OneOf(1, 2).Describe(),
			check: func(t *testing.T, c Constraints) {
				if len(c.Enum) != 2 || c.Enum[0] != float64(1) || c.Enum[1] != float64(2) {
					t.Errorf("Enum = %v, want the permitted numbers", c.Enum)
				}
			},
		},
		{
			name: "exact item count",
			got:  Slice[string]().Items(3).Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.MinItems == nil || *c.MinItems != 3 || c.MaxItems == nil || *c.MaxItems != 3 {
					t.Errorf("items = %v to %v, want exactly 3", c.MinItems, c.MaxItems)
				}
			},
		},
		{
			name: "not empty",
			got:  Slice[string]().NotEmpty().Describe(),
			check: func(t *testing.T, c Constraints) {
				if c.MinItems == nil || *c.MinItems != 1 {
					t.Errorf("MinItems = %v, want 1", c.MinItems)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.check(t, tc.got)
		})
	}

	// The rules that deliberately describe nothing, because JSON Schema has no
	// way to say what they enforce.
	for name, got := range map[string]Constraints{
		"not blank":   String().NotBlank().Describe(),
		"no control":  String().NoControl().Describe(),
		"not matches": String().MatchesNot(`^a`).Describe(),
		"json":        String().JSON().Describe(),
		"timezone":    String().Timezone().Describe(),
		"min bytes":   String().MinBytes(4).Describe(),
		"max bytes":   String().MaxBytes(4).Describe(),
		"equal fold":  String().EqualFold("a").Describe(),
	} {
		if !got.IsZero() {
			t.Errorf("%s described %+v, want nothing: JSON Schema cannot state it", name, got)
		}
	}
}

// wantFormat builds a check for a rule that contributes a format.
func wantFormat(format string) func(*testing.T, Constraints) {
	return func(t *testing.T, c Constraints) {
		if c.Format != format {
			t.Errorf("Format = %q, want %q", c.Format, format)
		}
	}
}

// wantPattern builds a check for a rule that contributes an expression.
func wantPattern(pattern string) func(*testing.T, Constraints) {
	return func(t *testing.T, c Constraints) {
		if c.Pattern != pattern {
			t.Errorf("Pattern = %q, want %q", c.Pattern, pattern)
		}
	}
}
