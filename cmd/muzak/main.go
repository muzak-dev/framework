// Command muzak is the command line for Muzak applications. It creates a
// project, runs one while it is being written, and reads the OpenAPI document
// one publishes:
//
//	go install muzak.dev/framework/cmd/muzak@latest
//
//	muzak new shop -module example.com/shop
//	cd shop && go mod tidy
//	muzak dev
//	muzak routes -url http://localhost:8080/openapi.json
//	muzak diff baseline.json current.json
//	muzak ts -file openapi.json -client -o web/src/api.ts
//
// Run "muzak help" for the list of commands and "muzak help <command>" for
// what one does. A command exits with status 0 when it did what it was asked,
// 1 when it could not, and 2 when the command line was wrong; diff, which a
// CI step gates on, exits 1 when it finds changes as serious as -fail-on
// names and 2 when a document cannot be read.
//
// Everything a command prints that came from a document, a server or a file
// is escaped for a terminal first, so a document someone else wrote cannot
// move the cursor, retitle the window or hide a line of the report it appears
// in.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], processConsole()))
}

// console is what a command reads, writes and runs in. main passes the
// process's own; a test passes buffers, a directory and an environment of its
// own, which is what lets every command be driven without starting a process.
type console struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// dir is the directory relative paths are resolved against and processes
	// are started in. Empty means the process's working directory.
	dir string
	// env is the environment every process a command starts runs with.
	env []string
	// signals subscribes to the interrupt and termination signals and returns
	// the function that ends the subscription. Only dev subscribes: every
	// other command is stopped by a signal the ordinary way, and an
	// application dev runs has to be stopped by dev rather than abandoned.
	signals func() (<-chan os.Signal, func())

	// version is the framework version a new project requires.
	version string
	// fetchTimeout bounds fetching a document from -url, from the first byte
	// sent to the last one read.
	fetchTimeout time.Duration
	// tempDir is where dev writes the binaries it builds. Empty means the
	// system's temporary directory.
	tempDir string
}

// defaultFetchTimeout bounds fetching a document. An application answers
// /openapi.json from memory, so this is time for a slow network rather than
// for a slow server.
const defaultFetchTimeout = 30 * time.Second

// processConsole is the console of this process.
func processConsole() *console {
	info, ok := debug.ReadBuildInfo()
	return &console{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		env:    os.Environ(),
		signals: func() (<-chan os.Signal, func()) {
			// Buffered, so a signal that arrives while dev is busy building is
			// kept until it looks rather than dropped by the runtime.
			ch := make(chan os.Signal, 2)
			signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
			return ch, func() { signal.Stop(ch) }
		},
		version:      frameworkVersion(info, ok),
		fetchTimeout: defaultFetchTimeout,
	}
}

// path resolves a path the command line named against the console's
// directory.
func (c *console) path(name string) string {
	if c.dir == "" || filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(c.dir, name)
}

// A command is one of the things muzak does.
type command struct {
	name string
	// args is what follows the name in the usage line.
	args string
	// summary is the one line "muzak help" lists the command with.
	summary string
	// about is the rest of its help, in paragraphs.
	about string
	// make returns a fresh runner, whose flags are declared on a flag set
	// before it runs.
	make func() runner
}

// A runner is one invocation of a command.
type runner interface {
	// flags declares the command's flags.
	flags(fs *flag.FlagSet)
	// run runs the command once its flags are parsed. args are the
	// arguments that were not flags, and passthrough is what followed a "--",
	// which only dev reads; any other command is given it as arguments.
	run(ctx context.Context, c *console, args, passthrough []string) error
}

// commands lists every command, in the order "muzak help" prints them.
var commands = []*command{newCmd, devCmd, routesCmd, diffCmd, tsCmd, versionCmd, helpCmd}

// lookup finds a command by name.
func lookup(name string) *command {
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd
		}
	}
	return nil
}

// Exit statuses.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// usageError is a command line that cannot be run as written. It exits with
// status 2, and its message is followed by where to read the usage.
type usageError struct {
	cmd *command
	msg string
}

func (e *usageError) Error() string { return e.msg }

// usagef reports a command line that cannot be run as written.
func usagef(cmd *command, format string, args ...any) error {
	return &usageError{cmd: cmd, msg: "muzak: " + fmt.Sprintf(format, args...)}
}

// exitError ends a command with a status other than 1. Its error, when there
// is one, is printed first.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

// run runs the command line args and returns the status to exit with.
func run(ctx context.Context, args []string, c *console) int {
	if len(args) == 0 {
		writeUsage(c.stderr)
		return exitUsage
	}
	switch args[0] {
	case "-h", "-help", "--help":
		writeUsage(c.stdout)
		return exitOK
	case "-version", "--version":
		args = append([]string{"version"}, args[1:]...)
	}
	cmd := lookup(args[0])
	if cmd == nil {
		return report(c, unknownCommand(args[0]))
	}
	return report(c, execute(ctx, c, cmd, args[1:]))
}

// unknownCommand explains a command name muzak does not have, and suggests
// the one that was probably meant.
func unknownCommand(name string) error {
	msg := fmt.Sprintf("muzak: there is no command %q", name)
	if strings.HasPrefix(name, "-") {
		msg = fmt.Sprintf("muzak: %q is a flag, and a command comes first, as in muzak new shop -module example.com/shop", name)
	} else if guess := closestCommand(name); guess != "" {
		msg += fmt.Sprintf("; did you mean %q?", guess)
	}
	return &usageError{msg: msg}
}

// closestCommand returns the command whose name is within two edits of name,
// or "" when none is. Names are short, so the quadratic distance is too.
func closestCommand(name string) string {
	best, bestDistance := "", 3
	for _, cmd := range commands {
		if d := editDistance(strings.ToLower(name), cmd.name); d < bestDistance {
			best, bestDistance = cmd.name, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between two strings, counted in
// bytes. A name longer than any command is not a typo of one, so it is not
// measured, which keeps the cost bounded whatever was typed.
func editDistance(a, b string) int {
	if len(a) > 32 {
		return len(a)
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

// execute parses a command's flags and runs it.
func execute(ctx context.Context, c *console, cmd *command, args []string) error {
	fs := newFlagSet(cmd)
	r := cmd.make()
	r.flags(fs)
	positional, passthrough, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		writeHelp(c.stdout, cmd)
		return nil
	}
	if err != nil {
		return usagef(cmd, "%s: %s", cmd.name, printable(err.Error()))
	}
	return r.run(ctx, c, positional, passthrough)
}

// newFlagSet returns the flag set a command's flags are declared on. It
// prints nothing itself: a mistake is reported once, by report, in the
// sentence form every other error has.
func newFlagSet(cmd *command) *flag.FlagSet {
	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseFlags parses flags wherever they are among the arguments, so that
// "muzak new shop -module example.com/shop" reads as it is written, which the
// flag package alone does not: it stops at the first argument that is not a
// flag. A "--" ends the flags, and what follows it is returned as
// passthrough.
func parseFlags(fs *flag.FlagSet, args []string) (positional, passthrough []string, err error) {
	for {
		if err := fs.Parse(args); err != nil {
			return nil, nil, err
		}
		rest := fs.Args()
		if endedByDashes(fs, args[:len(args)-len(rest)]) {
			return positional, rest, nil
		}
		if len(rest) == 0 {
			return positional, nil, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// endedByDashes reports whether the flag package stopped at a "--" among the
// arguments it consumed, rather than at an argument that is not a flag. It
// walks them as the flag package does, which is what tells "--" the
// terminator from "--" the value of a flag such as -module.
func endedByDashes(fs *flag.FlagSet, consumed []string) bool {
	for i := 0; i < len(consumed); i++ {
		arg := consumed[i]
		if arg == "--" {
			return true
		}
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) {
			i++ // the next argument is this flag's value
		}
	}
	return false
}

// isBoolFlag reports whether a flag takes no value, as -client does.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// report prints what an error says and returns the status it exits with.
func report(c *console, err error) int {
	if err == nil {
		return exitOK
	}
	var usage *usageError
	if errors.As(err, &usage) {
		_, _ = fmt.Fprintln(c.stderr, usage.msg)
		if usage.cmd != nil {
			_, _ = fmt.Fprintf(c.stderr, "Run \"muzak help %s\" for usage.\n", usage.cmd.name)
		} else {
			_, _ = fmt.Fprintln(c.stderr, "Run \"muzak help\" for the list of commands.")
		}
		return exitUsage
	}
	code := exitFailure
	var exit *exitError
	if errors.As(err, &exit) {
		code = exit.code
		if exit.err == nil {
			return code
		}
		err = exit.err
	}
	_, _ = fmt.Fprintln(c.stderr, printable(err.Error()))
	return code
}

// writeUsage prints the list of commands.
func writeUsage(w io.Writer) {
	var b strings.Builder
	b.WriteString("Muzak is the command line for Muzak applications.\n\nUsage:\n\n  muzak <command> [arguments]\n\nThe commands are:\n\n")
	for _, cmd := range commands {
		fmt.Fprintf(&b, "  %-9s %s\n", cmd.name, cmd.summary)
	}
	b.WriteString("\nRun \"muzak help <command>\" for more about a command.\n")
	_, _ = io.WriteString(w, b.String())
}

// writeHelp prints the help of one command: its usage line, what it does and
// its flags.
func writeHelp(w io.Writer, cmd *command) {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: muzak %s", cmd.name)
	if cmd.args != "" {
		b.WriteString(" " + cmd.args)
	}
	b.WriteString("\n\n" + cmd.about + "\n")
	fs := newFlagSet(cmd)
	cmd.make().flags(fs)
	hasFlags := false
	fs.VisitAll(func(*flag.Flag) { hasFlags = true })
	if hasFlags {
		b.WriteString("\nFlags:\n")
		fs.SetOutput(&b)
		fs.PrintDefaults()
	}
	_, _ = io.WriteString(w, b.String())
}

// helpCmd prints help.
var helpCmd = &command{
	name:    "help",
	args:    "[command]",
	summary: "print help about a command",
	about: `Help prints what a command does and the flags it takes, or, with no command
named, the list of commands. "muzak <command> -h" prints the same.`,
	make: func() runner { return &helpRunner{} },
}

type helpRunner struct{}

func (*helpRunner) flags(*flag.FlagSet) {}

func (*helpRunner) run(_ context.Context, c *console, args, passthrough []string) error {
	args = append(args, passthrough...)
	switch len(args) {
	case 0:
		writeUsage(c.stdout)
		return nil
	case 1:
		cmd := lookup(args[0])
		if cmd == nil {
			return unknownCommand(args[0])
		}
		writeHelp(c.stdout, cmd)
		return nil
	}
	return usagef(helpCmd, "help explains one command at a time, and was given %d", len(args))
}

// noArguments refuses arguments a command does not take.
func noArguments(cmd *command, args, passthrough []string) error {
	if extra := append(args, passthrough...); len(extra) > 0 {
		return usagef(cmd, "%s takes no arguments, and was given %q", cmd.name, strings.Join(extra, " "))
	}
	return nil
}
