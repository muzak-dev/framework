package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
)

// update rewrites the golden files instead of comparing with them.
var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// TestMain lets a test run this binary as the muzak command itself, which is
// how the end-to-end tests deliver a real signal to a real process.
func TestMain(m *testing.M) {
	if os.Getenv("MUZAK_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// syncBuffer is a buffer a test reads while a command still writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// result is what one run of a command did.
type result struct {
	code   int
	stdout string
	stderr string
}

// testConsole returns a console of buffers working in dir.
func testConsole(dir string) (*console, *syncBuffer, *syncBuffer) {
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	return &console{
		stdin:  strings.NewReader(""),
		stdout: stdout,
		stderr: stderr,
		dir:    dir,
		env:    os.Environ(),
		signals: func() (<-chan os.Signal, func()) {
			return make(chan os.Signal), func() {}
		},
		version: "v0.3.0",
	}, stdout, stderr
}

// runIn runs a command line in dir and returns what it did.
func runIn(t *testing.T, dir string, args ...string) result {
	t.Helper()
	c, stdout, stderr := testConsole(dir)
	code := run(context.Background(), args, c)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// expect fails the test unless the command exited with code and its stderr
// matches every pattern.
func (r result) expect(t *testing.T, code int, stderr ...string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit status %d, want %d\nstdout: %s\nstderr: %s", r.code, code, r.stdout, r.stderr)
	}
	for _, pattern := range stderr {
		if !regexp.MustCompile(pattern).MatchString(r.stderr) {
			t.Errorf("stderr does not match %q:\n%s", pattern, r.stderr)
		}
	}
}

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden file: %v (run go test -update to create it)", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("output differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestNoArgumentsIsAUsageError(t *testing.T) {
	t.Parallel()
	r := runIn(t, t.TempDir())
	r.expect(t, exitUsage, `muzak <command> \[arguments\]`, `The commands are:`)
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

func TestHelpPrintsTheCommands(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-h"}, {"-help"}, {"--help"}, {"help"}} {
		r := runIn(t, t.TempDir(), args...)
		r.expect(t, exitOK)
		golden(t, "help.golden", r.stdout)
		if r.stderr != "" {
			t.Errorf("%v: stderr = %q", args, r.stderr)
		}
	}
}

func TestHelpForEveryCommand(t *testing.T) {
	t.Parallel()
	for _, cmd := range commands {
		t.Run(cmd.name, func(t *testing.T) {
			t.Parallel()
			r := runIn(t, t.TempDir(), "help", cmd.name)
			r.expect(t, exitOK)
			golden(t, "help-"+cmd.name+".golden", r.stdout)
			for _, flagName := range []string{"-h", "-help", "--help"} {
				again := runIn(t, t.TempDir(), cmd.name, flagName)
				again.expect(t, exitOK)
				if again.stdout != r.stdout {
					t.Errorf("muzak %s %s prints something other than muzak help %s", cmd.name, flagName, cmd.name)
				}
			}
		})
	}
}

func TestHelpRefusesWhatItCannotExplain(t *testing.T) {
	t.Parallel()
	runIn(t, t.TempDir(), "help", "serve").expect(t, exitUsage, `there is no command "serve"`)
	runIn(t, t.TempDir(), "help", "new", "dev").expect(t, exitUsage, `one command at a time, and was given 2`, `muzak help help`)
}

func TestUnknownCommandSuggestsTheNearestOne(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"nwe":                     `there is no command "nwe"; did you mean "new"\?`,
		"HELP":                    `did you mean "help"\?`,
		"DEV":                     `did you mean "dev"\?`,
		"version2":                `did you mean "version"\?`,
		"deploy":                  `there is no command "deploy"\n`,
		"-x":                      `"-x" is a flag, and a command comes first`,
		strings.Repeat("n", 1000): `there is no command "n+"\n`,
		"\x1b]0;owned\x07":        `there is no command "\\x1b\]0;owned\\a"`,
	}
	for arg, want := range cases {
		r := runIn(t, t.TempDir(), arg)
		r.expect(t, exitUsage, want, `Run "muzak help" for the list of commands`)
		if strings.ContainsAny(r.stderr, "\x1b\x07") {
			t.Errorf("%q: a control character reached the terminal: %q", arg, r.stderr)
		}
	}
}

func TestUnknownFlagsAreUsageErrors(t *testing.T) {
	t.Parallel()
	for _, cmd := range commands {
		r := runIn(t, t.TempDir(), cmd.name, "-nope")
		r.expect(t, exitUsage, `muzak: `+cmd.name+`: flag provided but not defined: -nope`, `Run "muzak help `+cmd.name+`" for usage`)
	}
	runIn(t, t.TempDir(), "dev", "-poll", "soon").expect(t, exitUsage, `invalid value "soon" for flag -poll`)
	runIn(t, t.TempDir(), "new", "shop", "-module").expect(t, exitUsage, `flag needs an argument: -module`)
	runIn(t, t.TempDir(), "new", "-\x1b[2J").expect(t, exitUsage, `flag provided but not defined: -\\x1b\[2J`)
}

func TestVersion(t *testing.T) {
	t.Parallel()
	want := "muzak v0.3.0 (" + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")\n"
	for _, args := range [][]string{{"version"}, {"-version"}, {"--version"}} {
		r := runIn(t, t.TempDir(), args...)
		r.expect(t, exitOK)
		if r.stdout != want {
			t.Errorf("%v printed %q, want %q", args, r.stdout, want)
		}
	}
	runIn(t, t.TempDir(), "version", "extra").expect(t, exitUsage, `version takes no arguments, and was given "extra"`)
}

func TestFrameworkVersionComesFromAReleaseBuildOnly(t *testing.T) {
	t.Parallel()
	build := func(path, version string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: path, Version: version}}
	}
	cases := []struct {
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{build(frameworkModule, "v0.3.4"), true, "v0.3.4"},
		{build(frameworkModule, "v1.12.0"), true, "v1.12.0"},
		{build(frameworkModule, "(devel)"), true, releaseVersion},
		{build(frameworkModule, "v0.3.1-0.20261010120000-abcdef123456"), true, releaseVersion},
		{build(frameworkModule, "v0.3.0+dirty"), true, releaseVersion},
		{build(frameworkModule, ""), true, releaseVersion},
		{build("example.com/fork", "v9.9.9"), true, releaseVersion},
		{nil, false, releaseVersion},
	}
	for _, c := range cases {
		if got := frameworkVersion(c.info, c.ok); got != c.want {
			t.Errorf("frameworkVersion(%+v) = %q, want %q", c.info, got, c.want)
		}
	}
}

func TestIsReleaseVersion(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"v0.3.0": true, "v10.20.30": true, "v0.0.0": true,
		"0.3.0": false, "v0.3": false, "v0.3.0.1": false, "v01.2.3": false, "v0.3.0-rc.1": false,
		"v0.3.0+meta": false, "v0..0": false, "v0.x.0": false, "v1234567890.0.0": false, "": false,
	} {
		if got := isReleaseVersion(v); got != want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestReleaseVersionIsNotBehindTheChangelog holds releaseVersion to the
// newest release CHANGELOG.md records, so a release cannot ship a muzak new
// that scaffolds projects requiring the release before it.
func TestReleaseVersionIsNotBehindTheChangelog(t *testing.T) {
	t.Parallel()
	changelog, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^## \[(\d+)\.(\d+)\.(\d+)\]`).FindSubmatch(changelog)
	if match == nil {
		t.Fatal("CHANGELOG.md names no release")
	}
	newest := "v" + string(match[1]) + "." + string(match[2]) + "." + string(match[3])
	if compareVersions(releaseVersion, newest) < 0 {
		t.Errorf("releaseVersion is %s, behind %s, the newest release in CHANGELOG.md", releaseVersion, newest)
	}
}

// compareVersions compares two release versions numerically.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a[1:], "."), strings.Split(b[1:], ".")
	for i := range 3 {
		if d := len(pa[i]) - len(pb[i]); d != 0 {
			return d
		}
		if c := strings.Compare(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return 0
}

func TestParseFlagsAnywhereAmongTheArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args        []string
		positional  []string
		passthrough []string
		module      string
		client      bool
	}{
		{[]string{"shop", "-module", "example.com/shop"}, []string{"shop"}, nil, "example.com/shop", false},
		{[]string{"-module=x", "a", "b"}, []string{"a", "b"}, nil, "x", false},
		{[]string{"a", "-client", "b"}, []string{"a", "b"}, nil, "", true},
		{[]string{"a", "--", "-module", "b"}, []string{"a"}, []string{"-module", "b"}, "", false},
		{[]string{"-module", "--", "a"}, []string{"a"}, nil, "--", false},
		{[]string{"-module", "--", "--", "a"}, nil, []string{"a"}, "--", false},
		{[]string{"--module", "m", "-", "--"}, []string{"-"}, []string{}, "m", false},
		{[]string{"-client=false", "x"}, []string{"x"}, nil, "", false},
		{nil, nil, nil, "", false},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		module := fs.String("module", "", "")
		client := fs.Bool("client", false, "")
		positional, passthrough, err := parseFlags(fs, c.args)
		if err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if !equalStrings(positional, c.positional) || !equalStrings(passthrough, c.passthrough) ||
			*module != c.module || *client != c.client {
			t.Errorf("%q: positional %q, passthrough %q, module %q, client %v; want %q, %q, %q, %v",
				c.args, positional, passthrough, *module, *client, c.positional, c.passthrough, c.module, c.client)
		}
	}
}

// equalStrings compares two lists, a nil one equal to an empty one.
func equalStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00") && len(a) == len(b)
}

func TestEditDistance(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"dev", "dev", 0}, {"dve", "dev", 2}, {"rotues", "routes", 2},
		{"", "new", 3}, {"diff", "", 4}, {"kitten", "sitting", 3},
		{strings.Repeat("x", 33), "new", 33},
	} {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// failingWriter refuses every write, as a closed pipe or a full disk does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the pipe is closed") }

// runFailingStdout runs a command line whose standard output refuses writes.
func runFailingStdout(t *testing.T, dir string, args ...string) result {
	t.Helper()
	c, _, stderr := testConsole(dir)
	c.stdout = failingWriter{}
	code := run(context.Background(), args, c)
	return result{code: code, stderr: stderr.String()}
}

func TestErrorsDescribeThemselves(t *testing.T) {
	t.Parallel()
	usage := usagef(newCmd, "a %s", "sentence")
	if usage.Error() != "muzak: a sentence" {
		t.Errorf("usage error reads %q", usage)
	}
	wrapped := &exitError{code: exitUsage, err: errors.New("muzak: the cause")}
	if wrapped.Error() != "muzak: the cause" || errors.Unwrap(wrapped) == nil {
		t.Errorf("exit error reads %q", wrapped)
	}
}

func TestAFailedWriteFailsTheCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"version"},
		{"new", "written", "-module", "example.com/written"},
	} {
		runFailingStdout(t, dir, args...).expect(t, exitFailure, `the pipe is closed`)
	}
}

func TestExitErrorWithoutAMessageExitsQuietly(t *testing.T) {
	t.Parallel()
	c, _, stderr := testConsole(t.TempDir())
	if code := report(c, &exitError{code: 3}); code != 3 {
		t.Errorf("status %d, want 3", code)
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
	if (&exitError{code: 3}).Error() != "exit status 3" {
		t.Error("an exitError without an error does not describe its status")
	}
}

func TestConsolePathResolvesAgainstItsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &console{dir: dir}
	if got := c.path("a.json"); got != filepath.Join(dir, "a.json") {
		t.Errorf("relative path resolved to %q", got)
	}
	abs := filepath.Join(t.TempDir(), "b.json")
	if got := c.path(abs); got != abs {
		t.Errorf("absolute path resolved to %q", got)
	}
	if got := (&console{}).path("a.json"); got != "a.json" {
		t.Errorf("without a directory, the path became %q", got)
	}
}

func TestProcessConsoleIsTheProcess(t *testing.T) {
	t.Parallel()
	c := processConsole()
	if c.stdout != os.Stdout || c.stderr != os.Stderr || c.stdin != os.Stdin || c.dir != "" {
		t.Error("the process console does not read and write the process's own streams")
	}
	if !isReleaseVersion(c.version) {
		t.Errorf("version %q", c.version)
	}
	signals, stop := c.signals()
	stop()
	if signals == nil {
		t.Error("no signal channel")
	}
}
