package muzak

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type settings struct {
	AppName      string        `env:"APP_NAME" default:"Awesome API"`
	AdminEmail   string        `env:"ADMIN_EMAIL" required:"true"`
	ItemsPerUser int           `env:"ITEMS_PER_USER" default:"50"`
	Debug        bool          `env:"DEBUG" default:"false"`
	Timeout      time.Duration `env:"TIMEOUT" default:"30s"`
	Hosts        []string      `env:"HOSTS" default:"a.example,b.example"`
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	got, err := LoadConfig[settings](
		WithoutEnvironment(),
		ConfigValues(map[string]string{
			"ADMIN_EMAIL":    "admin@example.com",
			"ITEMS_PER_USER": "7",
			"DEBUG":          "true",
		}),
	)
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.AppName != "Awesome API" {
		t.Errorf("AppName = %q, want the default", got.AppName)
	}
	if got.AdminEmail != "admin@example.com" {
		t.Errorf("AdminEmail = %q", got.AdminEmail)
	}
	if got.ItemsPerUser != 7 {
		t.Errorf("ItemsPerUser = %d, want 7", got.ItemsPerUser)
	}
	if !got.Debug {
		t.Error("Debug = false, want true")
	}
	if got.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", got.Timeout)
	}
	if len(got.Hosts) != 2 || got.Hosts[0] != "a.example" {
		t.Errorf("Hosts = %v", got.Hosts)
	}
}

func TestLoadConfigReportsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig[settings](
		WithoutEnvironment(),
		ConfigValues(map[string]string{"ITEMS_PER_USER": "many", "TIMEOUT": "soon"}),
	)
	if err == nil {
		t.Fatal("LoadConfig succeeded, want errors")
	}
	message := err.Error()
	for _, want := range []string{"ADMIN_EMAIL", "ITEMS_PER_USER", "TIMEOUT"} {
		if !strings.Contains(message, want) {
			t.Errorf("the error does not mention %s:\n%s", want, message)
		}
	}
}

func TestLoadConfigRejectsNonStructTypes(t *testing.T) {
	t.Parallel()
	if _, err := LoadConfig[int](WithoutEnvironment()); err == nil {
		t.Fatal("LoadConfig[int] succeeded, want an error")
	}
}

func TestLoadConfigSecretValuesStayOutOfErrors(t *testing.T) {
	t.Parallel()
	type withSecret struct {
		Port  int `env:"PORT"`
		Token int `env:"TOKEN" secret:"true"`
	}
	_, err := LoadConfig[withSecret](
		WithoutEnvironment(),
		ConfigValues(map[string]string{"PORT": "not-a-port", "TOKEN": "hunter2"}),
	)
	if err == nil {
		t.Fatal("LoadConfig succeeded, want errors")
	}
	message := err.Error()
	if !strings.Contains(message, "not-a-port") {
		t.Errorf("a non-secret value should appear in the error:\n%s", message)
	}
	if strings.Contains(message, "hunter2") {
		t.Errorf("the secret value leaked into the error:\n%s", message)
	}
	if !strings.Contains(message, "hidden because the field is marked secret") {
		t.Errorf("the error does not explain the omission:\n%s", message)
	}
}

func TestLoadConfigEnvironmentBeatsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("APP_NAME=from-file\nADMIN_EMAIL=file@example.com\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("APP_NAME", "from-environment")

	got, err := LoadConfig[settings](EnvFile(path))
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.AppName != "from-environment" {
		t.Errorf("AppName = %q, want the environment to win", got.AppName)
	}
	if got.AdminEmail != "file@example.com" {
		t.Errorf("AdminEmail = %q, want the file value", got.AdminEmail)
	}
}

func TestEnvFileMissingIsNotAnError(t *testing.T) {
	t.Parallel()
	_, err := LoadConfig[settings](
		WithoutEnvironment(),
		EnvFile(filepath.Join(t.TempDir(), "absent.env")),
		ConfigValues(map[string]string{"ADMIN_EMAIL": "a@example.com"}),
	)
	if err != nil {
		t.Fatalf("a missing dotenv file produced %v, want nil", err)
	}
}

func TestEnvFileParseErrorIsReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("this line has no equals sign\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := LoadConfig[settings](WithoutEnvironment(), EnvFile(path))
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("LoadConfig = %v, want a parse failure", err)
	}
}

func TestEnvFileUnreadableIsReported(t *testing.T) {
	t.Parallel()
	// A directory can be opened but not read as a file, which stands in for any
	// unreadable path without depending on file permissions.
	_, err := LoadConfig[settings](WithoutEnvironment(), EnvFile(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("LoadConfig = %v, want a read failure", err)
	}
}

func TestParseEnv(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"  SPACED  =  spaced  ",
		`QUOTED="  keeps spaces  "`,
		`ESCAPED="line\nbreak\ttab \"quoted\" back\\slash"`,
		`LITERAL='no \n escapes'`,
		"export EXPORTED=exported",
		"TRAILING=value # trailing comment",
		"HASHED=\"value # not a comment\"",
		"EMPTY=",
		"EQUALS=a=b=c",
	}, "\n")

	got, err := parseEnv(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseEnv = %v", err)
	}
	want := map[string]string{
		"PLAIN":    "value",
		"SPACED":   "spaced",
		"QUOTED":   "  keeps spaces  ",
		"ESCAPED":  "line\nbreak\ttab \"quoted\" back\\slash",
		"LITERAL":  `no \n escapes`,
		"EXPORTED": "exported",
		"TRAILING": "value",
		"HASHED":   "value # not a comment",
		"EMPTY":    "",
		"EQUALS":   "a=b=c",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("%s = %q, want %q", key, got[key], wantValue)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d keys, want %d: %v", len(got), len(want), got)
	}
}

func TestParseEnvQuotedValueWithTrailingComment(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`DOUBLE="v" # note`,
		`SINGLE='v' # note`,
		"TABBED=\"v\"\t# note",
		`HASH="a # b" # c`,
		`ESC="say \"hi\" # x" # c`,
		`LITERAL='a \n b' # c`,
		`EMPTY="" # c`,
		`STRAY="a" "b"`,
	}, "\n")
	got, err := parseEnv(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parseEnv = %v", err)
	}
	want := map[string]string{
		"DOUBLE": "v", "SINGLE": "v", "TABBED": "v", "HASH": "a # b",
		"ESC": `say "hi" # x`, "LITERAL": `a \n b`, "EMPTY": "",
		// Read as before: up to the last quote.
		"STRAY": `a" "b`,
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("%s = %q, want %q", key, got[key], wantValue)
		}
	}
	// A comment needs whitespace before it, and text after the closing quote
	// that is not a comment is still an unclosed value.
	for _, bad := range []string{`K="v"# c`, `K="v" x`, `K='v' x`} {
		if _, err := parseEnv(strings.NewReader(bad + "\n")); err == nil {
			t.Errorf("parseEnv(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseEnvErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no equals sign", "JUST_A_WORD\n", "not a KEY=VALUE pair"},
		{"empty key", "=value\n", "empty key"},
		{"unclosed double quote", `KEY="unclosed` + "\n", "never closed"},
		{"unclosed single quote", "KEY='unclosed\n", "never closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseEnv(strings.NewReader(tc.input))
			if err == nil {
				t.Fatalf("parseEnv(%q) succeeded, want an error", tc.input)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseEnvRejectsOverlongLines(t *testing.T) {
	t.Parallel()
	huge := "KEY=" + strings.Repeat("x", 2<<20) + "\n"
	if _, err := parseEnv(strings.NewReader(huge)); err == nil {
		t.Fatal("parseEnv accepted a line beyond the buffer limit, want an error")
	}
}

func TestDeriveEnvName(t *testing.T) {
	t.Parallel()
	tests := []struct{ field, want string }{
		{"AppName", "APP_NAME"},
		{"ItemsPerUser", "ITEMS_PER_USER"},
		{"URL", "URL"},
		{"HTTPPort", "HTTP_PORT"},
		{"Addr", "ADDR"},
		{"A", "A"},
		{"Port2", "PORT2"},
		{"MyURLValue", "MY_URL_VALUE"},
	}
	for _, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			if got := deriveEnvName(tc.field); got != tc.want {
				t.Errorf("deriveEnvName(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

func TestConfigUsesDerivedNames(t *testing.T) {
	t.Parallel()
	type derived struct {
		ItemsPerUser int
	}
	got, err := LoadConfig[derived](WithoutEnvironment(),
		ConfigValues(map[string]string{"ITEMS_PER_USER": "12"}))
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.ItemsPerUser != 12 {
		t.Errorf("ItemsPerUser = %d, want 12", got.ItemsPerUser)
	}
}

func TestConfigPrefix(t *testing.T) {
	t.Parallel()
	type prefixed struct {
		Name string `env:"NAME"`
	}
	got, err := LoadConfig[prefixed](WithoutEnvironment(), EnvPrefix("MYAPP_"),
		ConfigValues(map[string]string{"MYAPP_NAME": "prefixed", "NAME": "unprefixed"}))
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.Name != "prefixed" {
		t.Errorf("Name = %q, want the prefixed value", got.Name)
	}
}

func TestConfigSkipsIgnoredAndUnexportedFields(t *testing.T) {
	t.Parallel()
	type skipping struct {
		Kept    string `env:"KEPT"`
		Ignored string `env:"-"`
		hidden  string //nolint:unused // present to prove unexported fields are skipped
	}
	got, err := LoadConfig[skipping](WithoutEnvironment(),
		ConfigValues(map[string]string{"KEPT": "yes", "IGNORED": "no", "-": "no"}))
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.Kept != "yes" || got.Ignored != "" || got.hidden != "" {
		t.Errorf("loaded %+v, want only Kept populated", got)
	}
}

func TestConfigEmbeddedStructs(t *testing.T) {
	t.Parallel()
	type database struct {
		Host string `env:"DB_HOST" default:"localhost"`
		Port int    `env:"DB_PORT" default:"5432"`
	}
	type composed struct {
		database
		Name string `env:"NAME" default:"service"`
	}
	got, err := LoadConfig[composed](WithoutEnvironment(),
		ConfigValues(map[string]string{"DB_HOST": "db.internal"}))
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.Host != "db.internal" || got.Port != 5432 || got.Name != "service" {
		t.Errorf("loaded %+v", got)
	}
}

func TestConfigRejectsUnloadableFieldTypes(t *testing.T) {
	t.Parallel()
	type unloadable struct {
		Ch chan int `env:"CH"`
	}
	_, err := LoadConfig[unloadable](WithoutEnvironment(),
		ConfigValues(map[string]string{"CH": "anything"}))
	if err == nil || !strings.Contains(err.Error(), "cannot be loaded into field") {
		t.Fatalf("LoadConfig = %v, want a type error", err)
	}
}

func TestSplitConfigValue(t *testing.T) {
	t.Parallel()
	if got := splitConfigValue(reflect.TypeFor[string](), "a,b"); len(got) != 1 || got[0] != "a,b" {
		t.Errorf("a string value was split: %v", got)
	}
	if got := splitConfigValue(reflect.TypeFor[[]string](), "a, b ,c"); len(got) != 3 || got[1] != "b" {
		t.Errorf("splitConfigValue = %v, want three trimmed entries", got)
	}
	if got := splitConfigValue(reflect.TypeFor[[]string](), ""); got != nil {
		t.Errorf("an empty list produced %v, want nil", got)
	}
	if got := splitConfigValue(reflect.TypeFor[[]byte](), "raw"); len(got) != 1 {
		t.Errorf("a byte slice was split: %v", got)
	}
}

func TestCustomConfigSource(t *testing.T) {
	t.Parallel()
	type simple struct {
		Value string `env:"VALUE"`
	}
	got, err := LoadConfig[simple](WithoutEnvironment(), WithConfigSource(staticSource{}))
	if err != nil {
		t.Fatalf("LoadConfig = %v", err)
	}
	if got.Value != "from-custom-source" {
		t.Errorf("Value = %q", got.Value)
	}
}

// staticSource is a minimal custom ConfigSource.
type staticSource struct{}

func (staticSource) Name() string { return "the static source" }

func (staticSource) Lookup(key string) (string, bool) {
	if key == "VALUE" {
		return "from-custom-source", true
	}
	return "", false
}

func TestSourceNames(t *testing.T) {
	t.Parallel()
	empty := &configLoader{}
	if got := empty.sourceNames(); got != "any configured source" {
		t.Errorf("sourceNames with no sources = %q", got)
	}
	loaded := &configLoader{sources: []ConfigSource{envSource{}, staticSource{}}}
	if got := loaded.sourceNames(); got != "the environment or the static source" {
		t.Errorf("sourceNames = %q", got)
	}
}

func TestMustLoadConfig(t *testing.T) {
	t.Parallel()

	t.Run("returns the settings when loading succeeds", func(t *testing.T) {
		t.Parallel()
		got := MustLoadConfig[settings](WithoutEnvironment(),
			ConfigValues(map[string]string{"ADMIN_EMAIL": "a@example.com"}))
		if got.AppName != "Awesome API" {
			t.Errorf("AppName = %q", got.AppName)
		}
	})

	t.Run("panics when a required setting is missing", func(t *testing.T) {
		t.Parallel()
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("MustLoadConfig returned, want a panic")
			}
			if !strings.Contains(recovered.(string), "ADMIN_EMAIL") {
				t.Errorf("the panic does not name the missing setting: %v", recovered)
			}
		}()
		MustLoadConfig[settings](WithoutEnvironment())
	})
}

func TestEnvSourceReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("MUZAK_TEST_VALUE", "present")
	if got, ok := (envSource{}).Lookup("MUZAK_TEST_VALUE"); !ok || got != "present" {
		t.Errorf("Lookup = %q, %v", got, ok)
	}
	if _, ok := (envSource{}).Lookup("MUZAK_TEST_ABSENT"); ok {
		t.Error("Lookup reported an absent variable as present")
	}
	if got := (envSource{}).Name(); got != "the environment" {
		t.Errorf("Name = %q", got)
	}
}

func TestFailingSourceLooksUpNothing(t *testing.T) {
	t.Parallel()
	source := failingSource{name: "broken"}
	if got := source.Name(); got != "broken" {
		t.Errorf("Name = %q", got)
	}
	if _, ok := source.Lookup("anything"); ok {
		t.Error("a failing source reported a value")
	}
}
