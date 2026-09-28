package muzak

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

// Struct tags recognised by the configuration loader.
const (
	tagEnv    = "env"
	tagSecret = "secret"
)

// ConfigSource supplies configuration values by name.
//
// Sources are consulted in the order they are given to [LoadConfig], and the
// first one holding a name wins. Implement it to read from somewhere Muzak
// does not know about, such as a secret manager or a configuration service.
type ConfigSource interface {
	// Name identifies the source in error messages, as "the environment" or
	// the path of a file.
	Name() string
	// Lookup returns the value stored under key and reports whether it was
	// present. A present but empty value must be reported as present, so that
	// an explicitly blank setting can override a default.
	Lookup(key string) (string, bool)
}

// ConfigOption configures how [LoadConfig] resolves values.
type ConfigOption func(*configLoader)

// configLoader accumulates the sources and settings for one load.
type configLoader struct {
	sources []ConfigSource
	skipEnv bool
	prefix  string

	// secretNames records every variable name a secret field reads, and
	// valueErrors every parse failure that would otherwise show its value.
	// Both are settled only after every field has been visited, because two
	// fields may read the same variable and the secret one may come second.
	secretNames map[string]bool
	valueErrors []*configValueError
}

// envSource reads from the process environment.
type envSource struct{}

func (envSource) Name() string { return "the environment" }

func (envSource) Lookup(key string) (string, bool) { return os.LookupEnv(key) }

// mapSource reads from an in-memory map, which is what a dotenv file and
// [ConfigValues] both produce.
type mapSource struct {
	name   string
	values map[string]string
}

func (m mapSource) Name() string { return m.name }

func (m mapSource) Lookup(key string) (string, bool) {
	v, ok := m.values[key]
	return v, ok
}

// EnvFile adds a dotenv file as a configuration source.
//
// The file holds one KEY=VALUE pair per line. Blank lines and lines beginning
// with '#' are ignored, an optional leading "export " is stripped, and a value
// may be wrapped in single or double quotes to preserve surrounding spaces or
// a '#'. Escape sequences are interpreted only inside double quotes.
//
// The real environment takes precedence over the file, so a value exported by
// a container runtime overrides the one checked into a development .env. A
// missing file is not an error, which lets the same code run in development,
// where the file exists, and in production, where the environment supplies
// everything. A file that exists but cannot be parsed is an error, reported
// with the file name, the line number and, where there is one, the key, but
// never the text of the line: a malformed line is often part of a secret, such
// as the second line of an unquoted PEM key.
func EnvFile(path string) ConfigOption {
	return func(l *configLoader) {
		values, err := readEnvFile(path)
		if err != nil {
			l.sources = append(l.sources, failingSource{name: path, err: err})
			return
		}
		l.sources = append(l.sources, mapSource{name: path, values: values})
	}
}

// ConfigValues adds an explicit set of values as a source, which is what a
// test uses to supply configuration without touching the environment.
func ConfigValues(values map[string]string) ConfigOption {
	return func(l *configLoader) {
		l.sources = append(l.sources, mapSource{name: "the supplied values", values: values})
	}
}

// WithConfigSource adds a custom source, consulted in the order added.
func WithConfigSource(source ConfigSource) ConfigOption {
	return func(l *configLoader) { l.sources = append(l.sources, source) }
}

// EnvPrefix requires every variable name to carry the given prefix, so that
// "APP_NAME" is read as "MYAPP_APP_NAME" under EnvPrefix("MYAPP_"). It keeps
// one service's settings from colliding with another's on a shared host.
func EnvPrefix(prefix string) ConfigOption {
	return func(l *configLoader) { l.prefix = prefix }
}

// WithoutEnvironment stops the process environment from being consulted,
// leaving only the sources given explicitly. It exists so that a test can
// pin configuration exactly, without inheriting whatever the developer has
// exported.
func WithoutEnvironment() ConfigOption {
	return func(l *configLoader) { l.skipEnv = true }
}

// failingSource records a source that could not be opened, so that the failure
// is reported alongside every other configuration problem rather than
// separately.
type failingSource struct {
	name string
	err  error
}

func (f failingSource) Name() string { return f.name }

func (f failingSource) Lookup(string) (string, bool) { return "", false }

// LoadConfig reads a configuration struct from the environment and any
// additional sources.
//
// Each exported field is read from the variable named by its env tag, falling
// back to the field name upper-cased with underscores between words. A default
// tag supplies the value used when no source holds the variable, and
// required:"true" turns an absent or empty variable into an error. Field
// types are converted with the same rules the request binder uses, so strings,
// booleans, numbers, durations, slices and any [encoding.TextUnmarshaler] all
// work:
//
//	type Settings struct {
//		AppName      string `env:"APP_NAME" default:"Awesome API"`
//		AdminEmail   string `env:"ADMIN_EMAIL" required:"true"`
//		ItemsPerUser int    `env:"ITEMS_PER_USER" default:"50"`
//	}
//
//	settings, err := muzak.LoadConfig[Settings](muzak.EnvFile(".env"))
//
// Every problem found is reported together, so a first run in a new
// environment lists all the missing variables at once instead of one per
// attempt. Mark a field secret:"true" to keep its value out of the error
// messages produced when it fails to parse; the value is hidden from every
// field that reads the same variable, not only the one marked. On an embedded
// struct, secret:"true" marks every field inside it, at any depth.
func LoadConfig[T any](opts ...ConfigOption) (T, error) {
	var out T
	loader := &configLoader{}
	for _, opt := range opts {
		opt(loader)
	}
	if !loader.skipEnv {
		// The real environment is consulted first, so a deployed value always
		// beats a checked-in file.
		loader.sources = append([]ConfigSource{envSource{}}, loader.sources...)
	}

	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		return out, fmt.Errorf("muzak: LoadConfig needs a struct type, but %s is not one", t)
	}

	var problems []error
	for _, source := range loader.sources {
		if failing, broken := source.(failingSource); broken {
			problems = append(problems, fmt.Errorf("muzak: configuration source %s could not be read: %w", failing.name, failing.err))
		}
	}
	value := reflect.ValueOf(&out).Elem()
	loader.fill(t, value, nil, false, &problems)
	for _, bad := range loader.valueErrors {
		if loader.secretNames[bad.name] {
			bad.shared = true
		}
	}
	if len(problems) > 0 {
		return out, errors.Join(problems...)
	}
	return out, nil
}

// MustLoadConfig is [LoadConfig] for a program that cannot run without its
// configuration.
//
// It panics when loading fails, which is the right behaviour in a main
// function: a service missing a required setting should stop immediately and
// visibly rather than start in an undefined state. Prefer LoadConfig anywhere
// the failure can be handled.
func MustLoadConfig[T any](opts ...ConfigOption) T {
	settings, err := LoadConfig[T](opts...)
	if err != nil {
		panic("muzak: configuration could not be loaded:\n" + err.Error())
	}
	return settings
}

// fill populates a struct from the configured sources, recursing into embedded
// structs so that shared settings can be composed.
//
// secret reports that an enclosing embedded struct was marked secret, which
// marks every field inside it. The tag used to be read only on the field that
// is assigned, so one written on an embedded group of credentials, the
// natural place to put it, hid nothing and a value that failed to parse was
// echoed into the error.
func (l *configLoader) fill(t reflect.Type, value reflect.Value, prefix []int, secret bool, problems *[]error) {
	for i := range t.NumField() {
		field := t.Field(i)
		if !usableField(field) {
			continue
		}
		index := append(append([]int(nil), prefix...), i)
		name, tagged := field.Tag.Lookup(tagEnv)
		if name == "-" {
			continue
		}
		marked := secret || field.Tag.Get(tagSecret) == "true"
		if field.Anonymous && field.Type.Kind() == reflect.Struct && !tagged {
			l.fill(field.Type, value, index, marked, problems)
			continue
		}
		if !tagged {
			name = deriveEnvName(field.Name)
		}
		l.assign(field, l.prefix+name, fieldByIndex(value, index), marked, problems)
	}
}

// assign resolves one field's value and writes it, recording a problem instead
// of stopping when the value is missing or unusable. secret reports that the
// field, or an embedded struct enclosing it, is marked secret.
func (l *configLoader) assign(field reflect.StructField, name string, target reflect.Value, secret bool, problems *[]error) {
	if secret {
		if l.secretNames == nil {
			l.secretNames = map[string]bool{}
		}
		l.secretNames[name] = true
	}
	raw, found := l.lookup(name)
	required := field.Tag.Get(tagRequired) == "true"
	if required && found && raw == "" {
		// A variable that is set to nothing is what a Compose or Kubernetes
		// expansion of an unset one produces, so "API_KEY=" must not satisfy a
		// setting that exists to be supplied: an empty signing key or token
		// would load without a word. Anything else, a default included,
		// treats it as absent.
		found = false
	}
	if !found {
		if def, hasDefault := field.Tag.Lookup(tagDefault); hasDefault {
			raw = def
		} else {
			if required {
				*problems = append(*problems, fmt.Errorf("muzak: %s is required but was not set, or is empty, in %s", name, l.sourceNames()))
			}
			return
		}
	}
	set, err := setterFor(field.Type)
	if err != nil {
		*problems = append(*problems, fmt.Errorf("muzak: %s cannot be loaded into field %s: %w", name, field.Name, err))
		return
	}
	if err := set(target, splitConfigValue(field.Type, raw)); err != nil {
		bad := &configValueError{name: name, err: err, raw: raw, secret: secret}
		l.valueErrors = append(l.valueErrors, bad)
		*problems = append(*problems, bad)
	}
}

// configValueError is a value that was found but could not be converted to
// its field's type.
//
// It is a type rather than a formatted string because whether the value may
// be shown is not known when the failure happens: a secret field that reads
// the same variable may not have been visited yet, and LoadConfig marks the
// error shared once every field has been.
type configValueError struct {
	name   string
	err    error
	raw    string
	secret bool // the failing field itself is marked secret
	shared bool // another field reading the same variable is marked secret
}

// Error reports the failure, with the value only when no secret field reads
// it.
//
// A type implementing encoding.TextUnmarshaler writes its own error, which
// this package does not control and which may well echo the text it was given
// back into the message (a custom credential type reporting "invalid key %q"
// is a natural thing to write). Secret means secret from the setter's own
// errors too, so a hidden value hides the setter's error along with it.
func (e *configValueError) Error() string {
	switch {
	case e.secret:
		return fmt.Sprintf("muzak: %s could not be parsed (value hidden because the field is marked secret)", e.name)
	case e.shared:
		return fmt.Sprintf("muzak: %s could not be parsed (value hidden because another field reading it is marked secret)", e.name)
	default:
		return fmt.Sprintf("muzak: %s %v%s", e.name, e.err, describeBadValue(e.raw))
	}
}

// Unwrap exposes the setter's error only when the message shows it, so that
// errors.As cannot reach a hidden value through the back door.
func (e *configValueError) Unwrap() error {
	if e.secret || e.shared {
		return nil
	}
	return e.err
}

// lookup consults the sources in order and returns the first value found.
func (l *configLoader) lookup(name string) (string, bool) {
	for _, source := range l.sources {
		if value, ok := source.Lookup(name); ok {
			return value, true
		}
	}
	return "", false
}

// sourceNames lists the configured sources for an error message.
func (l *configLoader) sourceNames() string {
	names := make([]string, 0, len(l.sources))
	for _, source := range l.sources {
		names = append(names, source.Name())
	}
	if len(names) == 0 {
		return "any configured source"
	}
	return strings.Join(names, " or ")
}

// splitConfigValue turns one variable into the list of values a setter
// expects, splitting a slice-typed field on commas so that a list can be
// written as "a,b,c".
func splitConfigValue(t reflect.Type, raw string) []string {
	if t.Kind() != reflect.Slice || t.Elem().Kind() == reflect.Uint8 {
		return []string{raw}
	}
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// describeBadValue appends the offending value to an error message. The
// caller has already turned a failure on a variable any secret field reads
// into a fixed message before reaching here, so this always has a value safe
// to show.
func describeBadValue(raw string) string {
	return fmt.Sprintf(" (got %q)", raw)
}

// deriveEnvName converts a Go field name into the conventional environment
// variable spelling, so that ItemsPerUser becomes ITEMS_PER_USER.
func deriveEnvName(field string) string {
	var b strings.Builder
	b.Grow(len(field) + 4)
	runes := []rune(field)
	for i, r := range runes {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && i > 0 {
			previous := runes[i-1]
			previousLower := previous >= 'a' && previous <= 'z' || previous >= '0' && previous <= '9'
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if previousLower || nextLower {
				b.WriteByte('_')
			}
		}
		if isUpper {
			b.WriteRune(r)
			continue
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r - ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// readEnvFile parses a dotenv file into a map. A missing file yields an empty
// map and no error, because the environment alone is a complete source.
func readEnvFile(path string) (map[string]string, error) {
	// The caller names the file, which is the whole point of EnvFile; there is
	// no fixed path to open instead.
	file, err := os.Open(path) // #nosec G304 -- the path is the API's argument

	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return parseEnv(file)
}

// parseEnv reads KEY=VALUE lines from r.
//
// Its errors name the line number and, where the line has one that looks like
// a variable name, the key, and never the text of the line or its value. A
// line that fails to parse is exactly where a secret tends to be: the second
// line of an unquoted multi-line PEM key, or "API_KEY: sk-..." typed with a
// colon. The error travels into startup logs and crash reports, which are read
// by more people than the file is.
func parseEnv(r io.Reader) (map[string]string, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(r)
	// A configuration line longer than this is a malformed file rather than a
	// setting, and refusing it keeps a hostile file from exhausting memory.
	scanner.Buffer(make([]byte, 0, 4<<10), 1<<20)

	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, raw, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("line %d is not a KEY=VALUE pair (its text is not shown, since it may "+
				"hold a secret); a value cannot span lines, so write a line break as \\n inside double quotes", line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d has an empty key", line)
		}
		value, err := parseEnvValue(strings.TrimSpace(raw))
		if err != nil {
			if envKeyShown(key) {
				return nil, fmt.Errorf("line %d (%s): %w", line, key, err)
			}
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		// The line being read when the scanner gave up is the one after the
		// last it returned. Its text is not included, for the reason above.
		return nil, fmt.Errorf("line %d: %w", line+1, err)
	}
	return values, nil
}

// envKeyShown reports whether a key is safe to repeat in an error. A real
// variable name is made of letters, digits and underscores and is not secret;
// anything else before an '=' may be a fragment of a value, such as a line of
// base64 ending in padding, and is left out.
func envKeyShown(key string) bool {
	if len(key) > 64 {
		return false
	}
	for _, r := range key {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// parseEnvValue unwraps a quoted value and strips a trailing comment from an
// unquoted one.
func parseEnvValue(raw string) (string, error) {
	if len(raw) >= 2 {
		quote := raw[0]
		if quote == '"' || quote == '\'' {
			if raw[len(raw)-1] != quote {
				return "", errors.New("the value opens with a quote that is never closed")
			}
			inner := raw[1 : len(raw)-1]
			if quote == '\'' {
				// Single quotes are literal, as in a POSIX shell.
				return inner, nil
			}
			return strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`).Replace(inner), nil
		}
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return raw, nil
}
