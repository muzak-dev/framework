package muzak

import (
	"strings"
	"testing"
)

type embeddedDBSecrets struct {
	Port  int        `env:"DB_PORT"`
	Token leakyToken `env:"DB_TOKEN"`
	embeddedDeeper
}

type embeddedDeeper struct {
	Retries int `env:"DB_RETRIES"`
}

type embeddedSecretSettings struct {
	embeddedDBSecrets `secret:"true"`
	Workers           int `env:"WORKERS"`
}

// TestConfigEmbeddedSecretTagMarksItsFields is the regression test for
// secret:"true" written on an embedded struct, the natural place to mark a
// group of credentials, which was ignored: a field inside it that failed to
// parse echoed its value into the error.
func TestConfigEmbeddedSecretTagMarksItsFields(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig[embeddedSecretSettings](WithoutEnvironment(), ConfigValues(map[string]string{
		"DB_PORT":    "s3cr3t-in-the-wrong-variable",
		"DB_TOKEN":   "tok-s3cr3t",
		"DB_RETRIES": "deep-s3cr3t",
		"WORKERS":    "not-a-secret",
	}))
	if err == nil {
		t.Fatal("LoadConfig accepted values that do not parse")
	}
	msg := err.Error()
	for _, leaked := range []string{"s3cr3t-in-the-wrong-variable", "tok-s3cr3t", "deep-s3cr3t"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("the error echoes %q from a field inside a secret embedded struct:\n%s", leaked, msg)
		}
	}
	for _, name := range []string{"DB_PORT", "DB_TOKEN", "DB_RETRIES"} {
		if !strings.Contains(msg, name+" could not be parsed (value hidden because the field is marked secret)") {
			t.Errorf("the error does not report %s as a hidden secret:\n%s", name, msg)
		}
	}
	// A field beside the embedded struct is not marked by it.
	if !strings.Contains(msg, "not-a-secret") {
		t.Errorf("the error hides WORKERS, which is not secret:\n%s", msg)
	}
}
