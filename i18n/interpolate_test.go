package i18n

import (
	"strings"
	"testing"
)

func TestInterpolate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		args []any
		want string
	}{
		{name: "nothing to fill", text: "is required", want: "is required"},
		{name: "one value", text: "must be at least %{count} characters",
			args: []any{"count", 12}, want: "must be at least 12 characters"},
		{name: "several values", text: "%{a} and %{b}",
			args: []any{"a", "one", "b", "two"}, want: "one and two"},
		{name: "the same value twice", text: "%{a}%{a}", args: []any{"a", "x"}, want: "xx"},
		{name: "value at the start", text: "%{a} trails", args: []any{"a", "x"}, want: "x trails"},
		{name: "escaped percent", text: "100%% sure", want: "100% sure"},
		{name: "explicit verb", text: "%<n>03d", args: []any{"n", 7}, want: "007"},
		{name: "explicit float verb", text: "%<n>.2f", args: []any{"n", 1.5}, want: "1.50"},
		{name: "a strftime pattern is left alone", text: "%Y-%m-%d", want: "%Y-%m-%d"},
		{name: "a currency format is left alone", text: "%u%n", want: "%u%n"},
		{name: "a trailing percent", text: "50%", want: "50%"},
		{name: "an unclosed placeholder is literal", text: "%{unclosed", want: "%{unclosed"},
		{name: "an unclosed verb is literal", text: "%<unclosed", want: "%<unclosed"},
		{name: "a verb with no letter is literal", text: "%<n>123", args: []any{"n", 1}, want: "%<n>123"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Interpolate(tc.text, tc.args...)
			if err != nil {
				t.Fatalf("Interpolate(%q) returned %v, want no error", tc.text, err)
			}
			if got != tc.want {
				t.Errorf("Interpolate(%q, %v) = %q, want %q", tc.text, tc.args, got, tc.want)
			}
		})
	}
}

// TestInterpolateValueTypes covers each type written out in appendValue, which
// exists so that the common ones never reach reflection.
func TestInterpolateValueTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{name: "string", value: "text", want: "text"},
		{name: "int", value: 42, want: "42"},
		{name: "int32", value: int32(42), want: "42"},
		{name: "int64", value: int64(42), want: "42"},
		{name: "uint", value: uint(42), want: "42"},
		{name: "uint64", value: uint64(42), want: "42"},
		{name: "float64", value: 1.5, want: "1.5"},
		{name: "bool", value: true, want: "true"},
		{name: "a type with no fast path", value: []int{1, 2}, want: "[1 2]"},
		{name: "nil", value: nil, want: "<nil>"},
	}
	for _, tc := range cases {
		got, err := Interpolate("%{v}", "v", tc.value)
		if err != nil {
			t.Errorf("%s: Interpolate returned %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: Interpolate = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestInterpolateRefuses(t *testing.T) {
	t.Parallel()
	if _, err := Interpolate("needs %{value}"); err == nil {
		t.Error("Interpolate with a value missing returned no error, want one")
	}
	if _, err := Interpolate("names %{scope}", "scope", "x"); err == nil {
		t.Error("Interpolate of a reserved name returned no error, want one")
	}
}

// TestCompileSkipsWhatItCan is the assertion behind the fast path: a message
// with nothing to fill in must compile to nothing, so that rendering it copies
// no bytes.
func TestCompileSkipsWhatItCan(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"is required", "", "%Y-%m-%d", "%n %u", "50%", "must be a valid email address",
	} {
		if parts := compile(text); parts != nil {
			t.Errorf("compile(%q) produced %d parts, want none", text, len(parts))
		}
	}
	for _, text := range []string{"%{a}", "a %{b} c", "%<n>d", "100%%"} {
		if parts := compile(text); parts == nil {
			t.Errorf("compile(%q) produced nothing, want parts", text)
		}
	}
}

// TestErrorMessages checks what each failure says, because these are read by
// whoever has to fix the locale file and a message that does not name the key
// sends them looking through all of them.
func TestErrorMessages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		mentions []string
	}{
		{
			name:     "missing translation",
			err:      &MissingTranslationError{Locale: "fr", Key: "store.title", Tried: []string{"fr", "en"}},
			mentions: []string{"store.title", "fr", "en"},
		},
		{
			name:     "invalid locale",
			err:      &InvalidLocaleError{Locale: "de", Available: []string{"en", "fr"}},
			mentions: []string{"de", "en", "fr"},
		},
		{
			name:     "invalid locale with nothing available",
			err:      &InvalidLocaleError{Locale: "de"},
			mentions: []string{"de"},
		},
		{
			name:     "a count against a single string",
			err:      &InvalidPluralizationDataError{Locale: "en", Key: "a", Count: 2},
			mentions: []string{"a", "en", "2"},
		},
		{
			name: "a form the entry does not define",
			err: &InvalidPluralizationDataError{
				Locale: "ru", Key: "files", Count: 3, Category: Few, Have: []PluralCategory{One, Other}},
			mentions: []string{"files", "ru", "few", "one, other"},
		},
		{
			name: "missing interpolation argument",
			err: &MissingInterpolationArgumentError{
				Locale: "en", Key: "a", Placeholder: "count", Text: "at least %{count}"},
			mentions: []string{"a", "en", "count", "at least"},
		},
		{
			name:     "reserved interpolation key",
			err:      &ReservedInterpolationKeyError{Locale: "en", Key: "a", Placeholder: "scope"},
			mentions: []string{"a", "en", "scope"},
		},
		{
			name:     "unknown file type",
			err:      &UnknownFileTypeError{Name: "en.toml", Ext: ".toml"},
			mentions: []string{"en.toml", ".toml", ".yml"},
		},
		{
			name:     "an argument where a name was expected",
			err:      &ArgumentError{Key: "a", Index: 2, Value: 42},
			mentions: []string{"a", "2", "42"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			message := tc.err.Error()
			if !strings.HasPrefix(message, "i18n: ") {
				t.Errorf("the message does not begin with the package name: %q", message)
			}
			for _, mention := range tc.mentions {
				if !strings.Contains(message, mention) {
					t.Errorf("the message does not mention %q: %q", mention, message)
				}
			}
		})
	}
}

func TestExceptionHandlers(t *testing.T) {
	t.Parallel()
	missing := &MissingTranslationError{Locale: "fr", Key: "a"}

	text, err := DefaultExceptionHandler(missing, "fr", "a")
	if err != nil || text != "translation missing: fr.a" {
		t.Errorf("DefaultExceptionHandler on a missing translation = %q, %v, want the marker and no error", text, err)
	}

	other := &ReservedInterpolationKeyError{Locale: "fr", Key: "a", Placeholder: "scope"}
	if _, err := DefaultExceptionHandler(other, "fr", "a"); err == nil {
		t.Error("DefaultExceptionHandler swallowed a mistake in the code, want it reported")
	}

	if _, err := StrictExceptionHandler(missing, "fr", "a"); err == nil {
		t.Error("StrictExceptionHandler swallowed a missing translation, want it reported")
	}
}

// TestInterpolateBoundsTheWidthOfADirective is the regression test for a
// "%<name>d" placeholder whose width was handed to fmt as written: a hundred
// "%<n>1000000d", twelve hundred bytes of translation, rendered ninety-five
// megabytes.
func TestInterpolateBoundsTheWidthOfADirective(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("%<n>1000000d", 100)
	out, err := Interpolate(huge, "n", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > len(huge) {
		t.Errorf("a %d byte translation rendered %d bytes, want a directive with an unbounded width left as text", len(huge), len(out))
	}
	for _, text := range []string{"%<n>.1000000f", "%<n>99999999999999999999d", "%<n>1000000.1000000f", "%<n>*d", "%<n>[1]d"} {
		out, err := Interpolate(text, "n", 7)
		if err != nil || out != text {
			t.Errorf("Interpolate(%q) = %q, %v; want the directive left as literal text", text, out, err)
		}
	}
}

// TestInterpolateKeepsOrdinaryDirectives pins the directives translations use,
// including the widest one the bound allows.
func TestInterpolateKeepsOrdinaryDirectives(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text  string
		value any
		want  string
	}{
		{"%<n>d", 7, "7"},
		{"%<n>03d", 7, "007"},
		{"%<n>5d", 7, "    7"},
		{"%<n>-5d|", 7, "7    |"},
		{"%<n>+d", 7, "+7"},
		{"%<n>x", 7, "7"},
		{"%<n>v", 7, "7"},
		{"%<n>#x", 7, "0x7"},
		{"%<n>064d", 7, strings.Repeat("0", 63) + "7"},
		{"%<n>.2f", 7.0, "7.00"},
		{"%<n>8.3f", 7.0, "   7.000"},
		{"%<n>.64f", 7.0, "7." + strings.Repeat("0", 64)},
		{"%<n>65d", 7, "%<n>65d"},
		{"%<n>.65f", 7.0, "%<n>.65f"},
		{"%<n>10.65f", 7.0, "%<n>10.65f"},
	} {
		got, err := Interpolate(tc.text, "n", tc.value)
		if err != nil || got != tc.want {
			t.Errorf("Interpolate(%q) = %q, %v; want %q", tc.text, got, err, tc.want)
		}
	}
}
