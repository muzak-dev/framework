package muzak

import (
	"strings"
	"testing"
)

// TestRequiredRefusesAnEmptyValue is the regression test for a required
// setting that an empty variable satisfied. Compose and Kubernetes expansions
// of an unset variable produce exactly "API_KEY=", so a required signing key
// or token loaded as the empty string without any error.
func TestRequiredRefusesAnEmptyValue(t *testing.T) {
	t.Parallel()
	type keyed struct {
		Key string `env:"API_KEY" required:"true" secret:"true"`
	}
	_, err := LoadConfig[keyed](WithoutEnvironment(), ConfigValues(map[string]string{"API_KEY": ""}))
	if err == nil {
		t.Fatal("LoadConfig accepted an empty value for a required setting")
	}
	if !strings.Contains(err.Error(), "API_KEY") || !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %q, want it to name the required variable", err)
	}
}

// TestRequiredStillAcceptsAValue and the cases beside it pin what the change
// must not touch: an empty value is still a value for a setting that is not
// required, and a required setting that has a value still loads it.
func TestRequiredStillAcceptsAValue(t *testing.T) {
	t.Parallel()
	type mixed struct {
		Key      string `env:"API_KEY" required:"true"`
		Optional string `env:"OPTIONAL" default:"fallback"`
		Plain    string `env:"PLAIN"`
	}
	got, err := LoadConfig[mixed](WithoutEnvironment(), ConfigValues(map[string]string{
		"API_KEY":  "s3cret",
		"OPTIONAL": "",
		"PLAIN":    "",
	}))
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.Key != "s3cret" {
		t.Errorf("Key = %q", got.Key)
	}
	if got.Optional != "" || got.Plain != "" {
		t.Errorf("an empty value for a setting that is not required was replaced: %+v", got)
	}
}
