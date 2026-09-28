package muzak

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errLeakyToken is what leakyToken wraps, so a test can tell whether the
// setter's own error is reachable through errors.Is.
var errLeakyToken = errors.New("the token is malformed")

// leakyToken is a credential type whose parse error quotes its input, which is
// the natural way to write one and exactly why a secret field hides it.
type leakyToken string

func (l *leakyToken) UnmarshalText(text []byte) error {
	return fmt.Errorf("invalid token %q: %w", text, errLeakyToken)
}

// TestEnvFileErrorsNeverEchoTheLine covers the lines a parse failure is most
// likely to land on, which are also the lines most likely to hold a secret.
func TestEnvFileErrorsNeverEchoTheLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		file   string
		secret string
		want   []string
	}{
		{
			name:   "an unquoted multi-line PEM key",
			file:   "APP=demo\nTLS_KEY=-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n",
			secret: "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC",
			want:   []string{"line 3 is not a KEY=VALUE pair", "inside double quotes"},
		},
		{
			name:   "a colon typed for an equals sign",
			file:   "API_KEY: sk-live-abcdef123456\n",
			secret: "sk-live-abcdef123456",
			want:   []string{"line 1 is not a KEY=VALUE pair"},
		},
		{
			name:   "an unclosed quote names the key but not the value",
			file:   "\nDB_PASSWORD=\"correct horse battery\n",
			secret: "correct horse",
			want:   []string{"line 2 (DB_PASSWORD)", "never closed"},
		},
		{
			name:   "an implausibly long key is not repeated",
			file:   strings.Repeat("K", 65) + "=\"unclosed\n",
			secret: strings.Repeat("K", 65),
			want:   []string{"line 1: the value opens"},
		},
		{
			name:   "a key that is not a variable name is not repeated",
			file:   "ab+cd/ef=\"unclosed\n",
			secret: "ab+cd/ef",
			want:   []string{"line 1: the value opens"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), ".env")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			type settings struct {
				App string `env:"APP"`
			}
			_, err := LoadConfig[settings](WithoutEnvironment(), EnvFile(path))
			if err == nil {
				t.Fatal("LoadConfig succeeded, want a parse error")
			}
			message := err.Error()
			if strings.Contains(message, tc.secret) {
				t.Errorf("the error repeats the secret:\n%s", message)
			}
			for _, want := range append(tc.want, path, "muzak: ") {
				if !strings.Contains(message, want) {
					t.Errorf("error = %q, want it to mention %q", message, want)
				}
			}
		})
	}
}

func TestEnvFileOverlongLineNamesTheLineOnly(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("s", 2<<20)
	_, err := parseEnv(strings.NewReader("A=1\nKEY=" + secret + "\n"))
	if err == nil {
		t.Fatal("parseEnv accepted a line beyond the buffer limit, want an error")
	}
	if !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "sss") {
		t.Errorf("error = %.200q, want the line number and none of its text", err.Error())
	}
}

// TestSecretHidesAValueEveryFieldReads covers a variable read by two fields,
// only one of them secret. The other field failing to parse it must not print
// what the secret field protects, whichever of the two is declared first.
func TestSecretHidesAValueEveryFieldReads(t *testing.T) {
	t.Parallel()
	type secretFirst struct {
		Token string     `env:"TOKEN" secret:"true"`
		Count leakyToken `env:"TOKEN"`
	}
	type secretSecond struct {
		Count leakyToken `env:"TOKEN"`
		Token string     `env:"TOKEN" secret:"true"`
	}
	values := ConfigValues(map[string]string{"TOKEN": "hunter2"})
	_, errFirst := LoadConfig[secretFirst](WithoutEnvironment(), values)
	_, errSecond := LoadConfig[secretSecond](WithoutEnvironment(), values)
	for i, err := range []error{errFirst, errSecond} {
		if err == nil {
			t.Fatalf("case %d: LoadConfig succeeded, want an error", i)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("case %d: the secret leaked:\n%s", i, err)
		}
		if !strings.Contains(err.Error(), "another field reading it is marked secret") {
			t.Errorf("case %d: the error does not explain the omission:\n%s", i, err)
		}
		// The setter's own error may quote the value, so it is not reachable
		// either.
		if errors.Is(err, errLeakyToken) {
			t.Errorf("case %d: the hidden setter error is reachable", i)
		}
	}
}

func TestConfigValueErrorUnwrapsWhenShown(t *testing.T) {
	t.Parallel()
	type settings struct {
		Token leakyToken `env:"TOKEN"`
	}
	_, err := LoadConfig[settings](WithoutEnvironment(), ConfigValues(map[string]string{"TOKEN": "eighty"}))
	if !errors.Is(err, errLeakyToken) {
		t.Errorf("error = %v, want the setter's error reachable for a value that is shown", err)
	}
	if !strings.Contains(err.Error(), `"eighty"`) {
		t.Errorf("error = %v, want the non-secret value shown", err)
	}
}
