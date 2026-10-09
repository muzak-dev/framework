package muzak

import (
	"strings"
	"testing"
)

// TestParseEnvCommentAfterAnEmptyValue is the regression test for a comment
// that became the value. "API_KEY= # fill me in" is how a template .env leaves
// a setting for its reader to supply, and it loaded "# fill me in", which
// satisfied required:"true" and so defeated the guard that refuses an empty
// required value. A tab before the '#' was not read as starting a comment
// either, although the quoted form already accepted one.
func TestParseEnvCommentAfterAnEmptyValue(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		"API_KEY= # fill me in",
		"TABBED_EMPTY=\t# fill me in",
		"SPACED = # around the equals sign",
		"TABBED=x\t# a tab before the comment",
		"MIXED=x \t # both",
		// A '#' with nothing but the '=' before it is part of the value, as
		// it is in a POSIX shell, which is what keeps a colour or a channel
		// name intact.
		"COLOR=#336699",
		"CHANNEL=#alerts # where pages go",
		"INSIDE=a#b",
	}, "\n")
	got, err := parseEnv(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseEnv = %v", err)
	}
	want := map[string]string{
		"API_KEY":      "",
		"TABBED_EMPTY": "",
		"SPACED":       "",
		"TABBED":       "x",
		"MIXED":        "x",
		"COLOR":        "#336699",
		"CHANNEL":      "#alerts",
		"INSIDE":       "a#b",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("%s = %q, want %q", key, got[key], wantValue)
		}
	}
}

// TestRequiredRefusesACommentedOutValue checks the consequence that mattered:
// a required setting left as a commented placeholder is reported missing
// rather than loaded as the text of the comment.
func TestRequiredRefusesACommentedOutValue(t *testing.T) {
	t.Parallel()
	values, err := parseEnv(strings.NewReader("API_KEY= # fill me in\n"))
	if err != nil {
		t.Fatalf("parseEnv = %v", err)
	}
	type keyed struct {
		APIKey string `env:"API_KEY" required:"true"`
	}
	got, err := LoadConfig[keyed](WithoutEnvironment(), ConfigValues(values))
	if err == nil {
		t.Fatalf("LoadConfig accepted a commented placeholder for a required setting: %q", got.APIKey)
	}
	if !strings.Contains(err.Error(), "API_KEY") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
}
