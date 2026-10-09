package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeApp is the source of the application the dev tests run. It appends a
// line to the file FAKE_MARKER names for everything a test needs to see:
// that it started, with its generation, process id and arguments, and each
// signal it receives. FAKE_MODE changes what it does: "ignore" ignores every
// signal, "exit" exits at once with status 3, "quit" exits at once with
// status 0, and "grandchild" starts a copy of itself that ignores every
// signal and records its process id.
const fakeApp = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const generation = %d

func note(format string, args ...any) {
	f, err := os.OpenFile(os.Getenv("FAKE_MARKER"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(f, format+"\n", args...)
	f.Close()
}

func main() {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	if os.Getenv("FAKE_ROLE") == "grandchild" {
		note("grandchild %%d", os.Getpid())
		for range signals {
		}
	}
	switch os.Getenv("FAKE_MODE") {
	case "exit":
		note("start %%d %%d %%q", generation, os.Getpid(), os.Args[1:])
		os.Exit(3)
	case "quit":
		note("start %%d %%d %%q", generation, os.Getpid(), os.Args[1:])
		return
	case "grandchild":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "FAKE_ROLE=grandchild")
		if err := child.Start(); err != nil {
			panic(err)
		}
	}
	fmt.Println("fake app says hello on stdout")
	note("start %%d %%d %%q", generation, os.Getpid(), os.Args[1:])
	for s := range signals {
		note("signal %%d %%d %%s", generation, os.Getpid(), s)
		if os.Getenv("FAKE_MODE") != "ignore" {
			return
		}
	}
}
`

// project is a module holding the fake application, in a directory whose
// name has a space in it, as do the directories dev builds into.
type project struct {
	dir    string
	marker string
	temp   string
}

// newProject writes the fake application at generation 1.
func newProject(t *testing.T) *project {
	t.Helper()
	requireGo(t)
	base := t.TempDir()
	p := &project{
		dir:    filepath.Join(base, "fake app"),
		marker: filepath.Join(base, "marker log"),
		temp:   filepath.Join(base, "build dir"),
	}
	if err := os.Mkdir(p.temp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(p.dir, "go.mod"), "module fakeapp\n\ngo 1.27.0\n")
	p.generation(t, 1)
	return p
}

// generation rewrites the application as generation n.
func (p *project) generation(t *testing.T, n int) {
	t.Helper()
	writeFile(t, filepath.Join(p.dir, "main.go"), fmt.Sprintf(fakeApp, n))
}

// breakBuild rewrites the application so that it does not compile.
func (p *project) breakBuild(t *testing.T) {
	t.Helper()
	writeFile(t, filepath.Join(p.dir, "main.go"), "package main\n\nfunc main() { undefinedName() }\n")
}

// lines returns what the application has recorded.
func (p *project) lines() []string {
	data, _ := os.ReadFile(p.marker)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// waitFor waits for a recorded line matching pattern, and returns its
// submatches.
func (p *project) waitFor(t *testing.T, pattern string) []string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range p.lines() {
			if m := re.FindStringSubmatch(line); m != nil {
				return m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the application never recorded %q; it recorded:\n%s", pattern, strings.Join(p.lines(), "\n"))
	return nil
}

// recorded reports whether a line matching pattern has been recorded.
func (p *project) recorded(pattern string) bool {
	re := regexp.MustCompile(pattern)
	for _, line := range p.lines() {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// devRun is dev running on a goroutine of the test.
type devRun struct {
	signals chan os.Signal
	cancel  context.CancelFunc
	done    chan int
	stdout  *syncBuffer
	stderr  *syncBuffer
}

// startDev runs dev in the project with the given arguments, and the fake
// application's environment with extra added.
func startDev(t *testing.T, p *project, args []string, extra ...string) *devRun {
	t.Helper()
	c, stdout, stderr := testConsole(p.dir)
	c.env = append(goEnv(), append([]string{"FAKE_MARKER=" + p.marker}, extra...)...)
	c.tempDir = p.temp
	d := &devRun{signals: make(chan os.Signal, 1), done: make(chan int, 1), stdout: stdout, stderr: stderr}
	c.signals = func() (<-chan os.Signal, func()) { return d.signals, func() {} }
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go func() { d.done <- run(ctx, append([]string{"dev"}, args...), c) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-d.done:
		case <-time.After(30 * time.Second):
			t.Error("dev did not stop")
		}
	})
	return d
}

// wait waits for dev to return, and returns its status.
func (d *devRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-d.done:
		d.done <- code
		return code
	case <-time.After(30 * time.Second):
		t.Fatalf("dev did not stop\nstderr:\n%s", d.stderr)
		return 0
	}
}

// waitStderr waits for dev's standard error to match pattern.
func (d *devRun) waitStderr(t *testing.T, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if re.MatchString(d.stderr.String()) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stderr never matched %q:\n%s", pattern, d.stderr)
}

// pidOf reads a process id from a submatch.
func pidOf(t *testing.T, s string) int {
	t.Helper()
	pid, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// requireSignals skips a test of signal delivery where there are no signals
// to deliver.
func requireSignals(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot deliver an interrupt to another process; dev kills the application instead")
	}
}

// expectNoBuilds fails unless dev removed every directory it built into.
func expectNoBuilds(t *testing.T, p *project) {
	t.Helper()
	entries, err := os.ReadDir(p.temp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dev left %d entries in its temporary directory", len(entries))
	}
}

func TestDevRestartsTheApplicationWhenAFileChanges(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms", "-grace", "5s", "--", "-flag", "two words"})
	first := p.waitFor(t, `^start 1 (\d+) \["-flag" "two words"\]$`)
	firstPID := pidOf(t, first[1])

	p.generation(t, 2)
	second := p.waitFor(t, `^start 2 (\d+) `)
	if runtime.GOOS != "windows" && !p.recorded(`^signal 1 `+first[1]+` interrupt$`) {
		t.Errorf("the first build was not interrupted before the second started:\n%s", strings.Join(p.lines(), "\n"))
	}
	expectGone(t, firstPID)
	expectAlive(t, pidOf(t, second[1]))

	d.cancel()
	if code := d.wait(t); code != exitOK {
		t.Errorf("dev exited with %d", code)
	}
	expectGone(t, pidOf(t, second[1]))
	expectNoBuilds(t, p)
	for _, want := range []string{`muzak dev: building \.`, `muzak dev: started \. \(pid \d+\)`, `muzak dev: stopping the application`, `muzak dev: stopped`} {
		if !regexp.MustCompile(want).MatchString(d.stderr.String()) {
			t.Errorf("stderr does not say %q:\n%s", want, d.stderr)
		}
	}
	if !strings.Contains(d.stdout.String(), "fake app says hello on stdout") {
		t.Errorf("the application's standard output was not passed on: %q", d.stdout)
	}
}

func TestDevKeepsTheRunningApplicationWhenABuildFails(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"})
	first := p.waitFor(t, `^start 1 (\d+) `)

	p.breakBuild(t)
	d.waitStderr(t, `undefinedName`)
	d.waitStderr(t, `muzak dev: the build failed; the application keeps running the previous build`)
	if p.recorded(`^signal 1 `) {
		t.Error("the application was stopped although the new build failed")
	}
	expectAlive(t, pidOf(t, first[1]))

	p.generation(t, 3)
	p.waitFor(t, `^start 3 `)
	expectGone(t, pidOf(t, first[1]))
}

func TestDevWaitsForAChangeWhenTheFirstBuildFails(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	p.breakBuild(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"})
	d.waitStderr(t, `muzak dev: the build failed; waiting for a change`)
	p.generation(t, 4)
	p.waitFor(t, `^start 4 `)
}

func TestDevWaitsForAChangeAfterTheApplicationExits(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"}, "FAKE_MODE=exit")
	p.waitFor(t, `^start 1 `)
	d.waitStderr(t, `muzak dev: the application exited \(exit status 3\); waiting for a change`)
	time.Sleep(200 * time.Millisecond)
	if strings.Count(strings.Join(p.lines(), "\n"), "start 1") != 1 {
		t.Error("an application that exited was started again without a change")
	}
	p.generation(t, 2)
	p.waitFor(t, `^start 2 `)
}

func TestDevReportsAnApplicationThatFinished(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"}, "FAKE_MODE=quit")
	p.waitFor(t, `^start 1 `)
	d.waitStderr(t, `muzak dev: the application exited; waiting for a change`)
}

func TestDevForwardsTheSignalItReceives(t *testing.T) {
	t.Parallel()
	requireSignals(t)
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			p := newProject(t)
			d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"})
			started := p.waitFor(t, `^start 1 (\d+) `)
			d.signals <- sig
			if code := d.wait(t); code != exitOK {
				t.Errorf("dev exited with %d", code)
			}
			if !p.recorded(`^signal 1 ` + started[1] + ` ` + sig.String() + `$`) {
				t.Errorf("the application did not receive %s:\n%s", sig, strings.Join(p.lines(), "\n"))
			}
			expectGone(t, pidOf(t, started[1]))
			expectNoBuilds(t, p)
		})
	}
}

func TestDevKillsAnApplicationThatIgnoresTheSignal(t *testing.T) {
	t.Parallel()
	requireSignals(t)
	p := newProject(t)
	grace := 300 * time.Millisecond
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms", "-grace", grace.String()}, "FAKE_MODE=ignore")
	started := p.waitFor(t, `^start 1 (\d+) `)
	begun := time.Now()
	d.signals <- syscall.SIGTERM
	d.wait(t)
	if elapsed := time.Since(begun); elapsed < grace {
		t.Errorf("the application was killed after %s, before its grace of %s", elapsed, grace)
	}
	if !p.recorded(`^signal 1 ` + started[1] + ` terminated$`) {
		t.Error("the application was killed without first being asked to stop")
	}
	expectGone(t, pidOf(t, started[1]))
}

func TestDevStopsWhatTheApplicationStarted(t *testing.T) {
	t.Parallel()
	requireSignals(t)
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms", "-grace", "300ms"}, "FAKE_MODE=grandchild")
	started := p.waitFor(t, `^start 1 (\d+) `)
	grandchild := p.waitFor(t, `^grandchild (\d+)$`)
	d.cancel()
	d.wait(t)
	expectGone(t, pidOf(t, started[1]))
	expectGone(t, pidOf(t, grandchild[1]))
}

func TestDevStopsWhenSignalledDuringABuild(t *testing.T) {
	t.Parallel()
	requireSignals(t)
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"})
	// The first build takes far longer than this, so the signal arrives
	// while the go command runs.
	d.signals <- os.Interrupt
	if code := d.wait(t); code != exitOK {
		t.Errorf("dev exited with %d", code)
	}
	if p.recorded(`^start `) {
		t.Error("an application was started after dev was told to stop")
	}
	expectNoBuilds(t, p)
}

func TestDevStopsWhenItsContextEndsDuringABuild(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"})
	d.cancel()
	if code := d.wait(t); code != exitOK {
		t.Errorf("dev exited with %d", code)
	}
	expectNoBuilds(t, p)
}

func TestDevStopsWhenTheWatchedTreeCannotBeRead(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	watched := filepath.Join(p.dir, "watched")
	writeFile(t, filepath.Join(watched, "x.go"), "package x\n")
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms", "-watch", "watched"})
	started := p.waitFor(t, `^start 1 (\d+) `)
	if err := os.RemoveAll(watched); err != nil {
		t.Fatal(err)
	}
	if code := d.wait(t); code != exitFailure {
		t.Errorf("dev exited with %d", code)
	}
	expectGone(t, pidOf(t, started[1]))
	if !strings.Contains(d.stderr.String(), "the directory dev watches cannot be read") {
		t.Errorf("stderr:\n%s", d.stderr)
	}
	expectNoBuilds(t, p)
}

func TestDevReportsAnApplicationThatCannotStart(t *testing.T) {
	t.Parallel()
	p := newProject(t)
	// A package that is not a command builds, and leaves nothing to run.
	writeFile(t, filepath.Join(p.dir, "lib", "lib.go"), "package lib\n")
	d := startDev(t, p, []string{"-pkg", "./lib", "-poll", "20ms"})
	d.waitStderr(t, `muzak dev: the application could not be started`)
}

func TestDevCommandLineErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runIn(t, dir, "dev", "./cmd/server").expect(t, exitUsage, `dev passes arguments to the application only after --, as in muzak dev -- -verbose, and was given "./cmd/server"`)
	runIn(t, dir, "dev", "-poll", "1ms").expect(t, exitUsage, `-poll is 1ms, and dev polls at most every 10ms`)
	runIn(t, dir, "dev", "-grace", "0s").expect(t, exitUsage, `-grace is 0s`)
	runIn(t, dir, "dev", "-pkg", "-toolexec=x").expect(t, exitUsage, `-pkg is "-toolexec=x", and it names a package`)
	runIn(t, dir, "dev", "-pkg", "").expect(t, exitUsage, `-pkg is ""`)
	runIn(t, dir, "dev", "-ext", "go,,mod").expect(t, exitUsage, `-ext lists ""`)
}

func TestDevRefusesAWatchedTreeItCannotRead(t *testing.T) {
	t.Parallel()
	requireGo(t)
	c, _, stderr := testConsole(t.TempDir())
	c.tempDir = t.TempDir()
	code := run(context.Background(), []string{"dev", "-watch", "missing"}, c)
	if code != exitFailure || !strings.Contains(stderr.String(), "the directory dev watches cannot be read") {
		t.Errorf("status %d, stderr %q", code, stderr)
	}
	if entries, _ := os.ReadDir(c.tempDir); len(entries) != 0 {
		t.Error("dev left its build directory behind")
	}
}

func TestDevNeedsADirectoryToBuildInto(t *testing.T) {
	t.Parallel()
	requireGo(t)
	c, _, stderr := testConsole(t.TempDir())
	c.tempDir = filepath.Join(t.TempDir(), "missing")
	if code := run(context.Background(), []string{"dev"}, c); code != exitFailure {
		t.Errorf("status %d", code)
	}
	if !strings.Contains(stderr.String(), "dev could not create a directory to build into") {
		t.Errorf("stderr %q", stderr)
	}
}

// TestDevNeedsTheGoCommand changes PATH, which no other test may see, so it
// does not run in parallel.
func TestDevNeedsTheGoCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runIn(t, t.TempDir(), "dev").expect(t, exitFailure, `dev builds the application with the go command, and there is none on PATH`)
}

// TestDevLeavesNothingRunning runs dev through a restart and a stop, then
// asks the runtime whether any goroutine was left that can never finish.
// It does not run in parallel, so that what it finds is its own.
func TestDevLeavesNothingRunning(t *testing.T) {
	p := newProject(t)
	d := startDev(t, p, []string{"-pkg", ".", "-poll", "20ms"})
	first := p.waitFor(t, `^start 1 (\d+) `)
	p.generation(t, 2)
	second := p.waitFor(t, `^start 2 (\d+) `)
	d.cancel()
	d.wait(t)
	expectGone(t, pidOf(t, first[1]))
	expectGone(t, pidOf(t, second[1]))

	profile := pprof.Lookup("goroutineleak")
	if profile == nil {
		t.Fatal("the goroutineleak profile is unavailable; Go 1.27 or later is required")
	}
	for range 5 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	var buf bytes.Buffer
	if err := profile.WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "goroutineleak profile: total 0") {
		t.Errorf("goroutines leaked:\n%s", buf.String())
	}
}
