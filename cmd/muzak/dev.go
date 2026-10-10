package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// devCmd runs an application while it is being written.
var devCmd = &command{
	name:    "dev",
	args:    "[-pkg ./cmd/server] [-watch dir] [-poll 300ms] [-grace 5s] [-ext list] [-- arguments]",
	summary: "run an application, rebuilding and restarting it when a file changes",
	about: `Dev builds the package with go build, runs it, and watches the files under
-watch. When one changes, it waits for the editing to settle, builds again,
and, if the build succeeds, stops the running application and starts the new
build. A build that fails prints what the compiler said and leaves the
application that was running alone, so the server stays up while the mistake
is fixed. Arguments after -- are passed to the application, which runs in the
current directory, with this environment and no standard input.

Changes are found by polling: every -poll, the tree is walked and each
watched file's size and modification time compared, which works the same on
every platform and file system. Only files with one of the -ext extensions
are watched, so an application that writes a database, a log or an upload
into its own directory does not restart itself, and directories whose name
starts with a dot, node_modules, vendor and testdata are not walked. A tree
with more than 10000 watched files is refused rather than polled slowly.

Ctrl-C or SIGTERM stops dev. The application is sent the same signal and
given -grace to shut down before it is killed. On Unix the application runs
in a process group of its own and the signal goes to the whole group, so a
process it started is stopped with it. On Windows there is no signal to send:
the application is killed at once, and a process it started is left running.`,
	make: func() runner { return &devRunner{} },
}

// Bounds on dev's timing.
const (
	// minPoll is the shortest -poll, below which polling a large tree would
	// keep a core busy for nothing an editor needs.
	minPoll = 10 * time.Millisecond
	// maxSettlePolls is how many polls in a row may each find a change
	// before dev builds anyway: a rebuild waits for one quiet poll, so a save
	// that touches several files builds once, but a file that changes
	// continuously cannot put the build off for ever.
	maxSettlePolls = 10
	// appWaitDelay bounds the wait for an application's output after it has
	// exited, which a process it started may still hold open.
	appWaitDelay = 2 * time.Second
)

type devRunner struct {
	pkg   string
	watch string
	ext   string
	poll  time.Duration
	grace time.Duration
}

func (d *devRunner) flags(fs *flag.FlagSet) {
	fs.StringVar(&d.pkg, "pkg", "./cmd/server", "the `package` to build and run")
	fs.StringVar(&d.watch, "watch", ".", "the `directory` to watch for changes")
	fs.DurationVar(&d.poll, "poll", 300*time.Millisecond, "how often to look for changes")
	fs.DurationVar(&d.grace, "grace", 5*time.Second, "how long a stopping application has before it is killed")
	fs.StringVar(&d.ext, "ext", defaultWatchExtensions, "the comma-separated `list` of extensions of the files whose change rebuilds the application")
}

func (d *devRunner) run(ctx context.Context, c *console, args, passthrough []string) error {
	switch {
	case len(args) > 0:
		return usagef(devCmd, "dev passes arguments to the application only after --, as in muzak dev -- -verbose, and was given %q", strings.Join(args, " "))
	case d.poll < minPoll:
		return usagef(devCmd, "-poll is %s, and dev polls at most every %s", d.poll, minPoll)
	case d.grace <= 0:
		return usagef(devCmd, "-grace is %s, and an application needs some time to shut down", d.grace)
	case d.pkg == "" || strings.HasPrefix(d.pkg, "-"):
		return usagef(devCmd, "-pkg is %q, and it names a package, such as ./cmd/server", d.pkg)
	}
	root, err := filepath.Abs(c.path(d.watch))
	if err != nil {
		// coverage: Abs fails only when the working directory has been
		// removed, which a test cannot arrange without pulling it from under
		// every other test of the package.
		return fmt.Errorf("muzak: the directory to watch cannot be resolved: %w", err)
	}
	watcher, err := newWatcher(root, d.ext, "", maxWatchedFiles)
	if err != nil {
		return usagef(devCmd, "%s", err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("muzak: dev builds the application with the go command, and there is none on PATH: %w", err)
	}
	temp, err := os.MkdirTemp(c.tempDir, "muzak-dev-")
	if err != nil {
		return fmt.Errorf("muzak: dev could not create a directory to build into: %w", err)
	}
	defer func() { _ = os.RemoveAll(temp) }()
	watcher.skip = resolveLinks(temp)

	signals, unsubscribe := c.signals()
	defer unsubscribe()
	status := &lockedWriter{w: c.stderr}
	output := &lockedWriter{w: c.stdout}
	s := &devSession{
		c:       c,
		status:  status,
		stdout:  childOutput(c.stdout, output),
		stderr:  childOutput(c.stderr, status),
		goTool:  goTool,
		pkg:     d.pkg,
		args:    passthrough,
		poll:    d.poll,
		grace:   d.grace,
		temp:    temp,
		watcher: watcher,
	}
	return s.loop(ctx, signals)
}

// devSession is one run of dev. Only the goroutine running loop touches it.
type devSession struct {
	c *console
	// status is where dev says what it is doing.
	status io.Writer
	// stdout and stderr are what the build and the application write to.
	stdout io.Writer
	stderr io.Writer

	goTool string
	pkg    string
	args   []string
	poll   time.Duration
	grace  time.Duration
	// temp is the directory builds are written to, removed when dev ends.
	temp    string
	watcher *watcher

	// builds counts the builds, which name the binaries: each build is
	// written to a new file, since Windows refuses to replace the binary of
	// a process that is running, and the previous one runs until the new
	// one has built.
	builds int
	// app is the application running, or nil, and binary is its file.
	app    *process
	binary string
}

// logf says what dev is doing.
func (s *devSession) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.status, "muzak dev: "+format+"\n", args...)
}

// loop builds and runs the application, rebuilds it on every change, and
// stops it when dev is told to stop, which a signal or ctx does.
func (s *devSession) loop(ctx context.Context, signals <-chan os.Signal) error {
	if err := s.watcher.prime(); err != nil {
		return err
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	// settling counts the polls since a change that has not been built yet,
	// and is zero when there is none. build is set when the tree has
	// settled, and to begin with, so the first build does not wait a poll.
	settling, build := 0, true
	for {
		if build {
			build = false
			if sig := s.rebuild(ctx, signals); sig != nil {
				s.shutdown(sig)
				return nil
			}
		}
		var exited <-chan struct{}
		if s.app != nil {
			exited = s.app.done
		}
		select {
		case sig := <-signals:
			s.shutdown(sig)
			return nil
		case <-ctx.Done():
			s.shutdown(os.Interrupt)
			return nil
		case <-exited:
			s.reportExit()
		case <-ticker.C:
			changed, err := s.watcher.poll()
			if err != nil {
				s.shutdown(os.Interrupt)
				return err
			}
			if changed {
				settling++
				if settling < maxSettlePolls {
					continue
				}
			} else if settling == 0 {
				continue
			}
			settling, build = 0, true
		}
	}
}

// rebuild builds the package and, when the build succeeds, replaces the
// running application with the new build. It returns the signal that
// interrupted the build, or os.Interrupt when ctx did, and nil otherwise.
func (s *devSession) rebuild(ctx context.Context, signals <-chan os.Signal) os.Signal {
	s.builds++
	binary := filepath.Join(s.temp, fmt.Sprintf("app-%d%s", s.builds, executableSuffix()))
	s.logf("building %s", printable(s.pkg))
	cmd := exec.Command(s.goTool, "build", "-o", binary, s.pkg) //nolint:gosec // the go command found on PATH, building the package the developer named
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = s.c.dir, s.c.env, s.stdout, s.stderr
	build, err := startProcess(cmd)
	if err != nil {
		// coverage: the go command was found on PATH a moment ago, and
		// starting it fails only if it was removed or the system is out of
		// processes since.
		s.logf("the build could not be started: %s", printable(err.Error()))
		return nil
	}
	select {
	case <-build.done:
	case sig := <-signals:
		build.kill()
		return sig
	case <-ctx.Done():
		build.kill()
		return os.Interrupt
	}
	if build.err != nil {
		_ = os.Remove(binary)
		if s.app != nil {
			// An application that exited meanwhile is reported by the loop as
			// soon as this returns.
			s.logf("the build failed; the application keeps running the previous build")
		} else {
			s.logf("the build failed; waiting for a change")
		}
		return nil
	}
	if s.app != nil {
		s.stopApp(os.Interrupt)
	}
	s.startApp(binary)
	return nil
}

// startApp runs a build.
func (s *devSession) startApp(binary string) {
	cmd := exec.Command(binary, s.args...) //nolint:gosec // the binary dev has just built, with the arguments the developer gave
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = s.c.dir, s.c.env, s.stdout, s.stderr
	cmd.WaitDelay = appWaitDelay
	app, err := startProcess(cmd)
	if err != nil {
		_ = os.Remove(binary)
		s.logf("the application could not be started: %s", printable(err.Error()))
		return
	}
	s.app, s.binary = app, binary
	s.logf("started %s (pid %d)", printable(s.pkg), app.cmd.Process.Pid)
}

// stopApp stops the running application and removes its binary.
func (s *devSession) stopApp(sig os.Signal) {
	s.app.stop(sig, s.grace)
	s.app = nil
	_ = os.Remove(s.binary)
}

// reportExit deals with an application that exited by itself: whatever it
// started and left behind is stopped at once, and dev waits for a change
// rather than starting it again, which a program that fails as it starts
// would only repeat.
func (s *devSession) reportExit() {
	err := s.app.err
	s.app.kill()
	s.app = nil
	_ = os.Remove(s.binary)
	if err != nil {
		s.logf("the application exited (%s); waiting for a change", printable(err.Error()))
		return
	}
	s.logf("the application exited; waiting for a change")
}

// shutdown stops the application, if one is running, with sig.
func (s *devSession) shutdown(sig os.Signal) {
	if s.app != nil {
		s.logf("stopping the application")
		s.stopApp(sig)
	}
	s.logf("stopped")
}

// executableSuffix is what the name of an executable ends with.
func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
