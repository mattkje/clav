// Package cli implements clav's command line interface.
//
// Commands are small and independent. Anything that touches the filesystem or
// clav's state lives in internal/archive, internal/project and internal/state,
// so new commands (export, import, move, clean, status) can be added here
// without reaching into those layers.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"clav/internal/project"
	"clav/internal/state"
)

// Version is set at build time with -ldflags "-X clav/internal/cli.Version=...".
var Version = "0.1.0"

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitAborted = 130
)

// usageError marks an error as a misuse of the CLI rather than a failure.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// App holds the streams and storage a command needs. It exists so tests can
// drive the CLI exactly as a user would.
type App struct {
	Out   io.Writer
	Err   io.Writer
	In    io.Reader
	Store *state.Store
	// Cwd is the directory a bare `clav park` resolves against. Empty means the
	// process working directory.
	Cwd string
	ui  *UI
}

// workingDir is the directory bare invocations resolve against.
//
// The result is cached: a command cannot change its own working directory, and
// os.Getwd fails once that directory has been deleted — which is exactly what
// park does. Resolving it once, up front, keeps the answer available afterwards.
func (a *App) workingDir() (string, error) {
	if a.Cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			// The directory has been deleted underneath the shell — which is
			// exactly the situation after a park. The shell still remembers
			// where it thinks it is, and for restore that is the useful answer.
			if pwd := os.Getenv("PWD"); pwd != "" {
				a.Cwd = pwd
				return a.Cwd, nil
			}
			return "", fmt.Errorf("cannot determine the current directory: %w", err)
		}
		a.Cwd = wd
	}
	return a.Cwd, nil
}

// Main is the process entry point. It returns an exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := &App{Out: os.Stdout, Err: os.Stderr, In: os.Stdin}
	err := app.Run(ctx, args)

	var ue usageError
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "clav: aborted; nothing was deleted")
		return ExitAborted
	case errors.As(err, &ue):
		fmt.Fprintf(os.Stderr, "clav: %v\n", err)
		return ExitUsage
	default:
		fmt.Fprintf(os.Stderr, "clav: %v\n", err)
		return ExitError
	}
}

// Run dispatches a single command invocation.
func (a *App) Run(ctx context.Context, args []string) error {
	if a.ui == nil {
		a.ui = newUI(a.Out, a.Err)
	}
	if len(args) == 0 {
		a.printUsage()
		return nil
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "-h", "--help", "help":
		a.printUsage()
		return nil
	case "--version", "version", "-V":
		a.ui.Line("clav %s", Version)
		return nil
	}
	if strings.HasPrefix(cmd, "-") {
		return usagef("unknown option %q", cmd)
	}

	if err := a.openStore(); err != nil {
		return err
	}

	switch cmd {
	case "park":
		return a.park(ctx, rest)
	case "restore":
		return a.restore(ctx, rest)
	case "sweep":
		return a.sweep(ctx, rest)
	case "list", "ls":
		return a.list(ctx, rest)
	case "inspect":
		return a.inspect(ctx, rest)
	case "remove", "rm":
		return a.remove(ctx, rest)
	default:
		return usagef("unknown command %q", cmd)
	}
}

func (a *App) openStore() error {
	if a.Store != nil {
		return nil
	}
	s, err := state.Open("")
	if err != nil {
		return err
	}
	a.Store = s
	return nil
}

// command bundles a flag set with the usage line shown when it is misused.
type command struct {
	fs      *flag.FlagSet
	usage   string
	verbose *bool
}

// newCommand builds a flag set that reports problems through clav's own error
// handling instead of printing Go's default output. Every command accepts
// --verbose.
func newCommand(name, usage string) *command {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	c := &command{fs: fs, usage: usage}
	verbose := fs.Bool("verbose", false, "print additional detail")
	fs.BoolVar(verbose, "v", false, "print additional detail")
	c.verbose = verbose
	return c
}

// permute moves flags ahead of operands so that `clav park ~/x --verbose`
// works as well as `clav park --verbose ~/x`. A flag that takes a value keeps
// its value with it, so `clav sweep ~/x --older-than 60d` is understood as the
// two-token flag it is rather than as a second path.
//
// A literal "--" ends flag parsing, as usual.
func (c *command) permute(args []string) []string {
	flags := make([]string, 0, len(args))
	operands := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			operands = append(operands, args[i+1:]...)
			return append(flags, append([]string{"--"}, operands...)...)
		case len(arg) > 1 && arg[0] == '-':
			flags = append(flags, arg)
			// A value-taking flag written as two tokens takes the next one
			// with it; "--name=value" already carries its own.
			if !strings.Contains(arg, "=") && c.takesValue(arg) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			operands = append(operands, arg)
		}
	}
	return append(flags, operands...)
}

// takesValue reports whether a flag expects a separate value token. Boolean
// flags do not: `--verbose true` would swallow an operand.
func (c *command) takesValue(arg string) bool {
	name := strings.TrimLeft(arg, "-")
	f := c.fs.Lookup(name)
	if f == nil {
		return false
	}
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !boolFlag.IsBoolFlag()
}

// parse applies args, translating -h and bad flags into usage errors.
func (c *command) parse(args []string) error {
	if err := c.fs.Parse(c.permute(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return usageError{errors.New(c.usage)}
		}
		return usageError{fmt.Errorf("%w\n\n%s", err, c.usage)}
	}
	return nil
}

// target returns the project a command should act on. With no argument it
// resolves the project you are standing in: the nearest enclosing project root,
// or the current directory if nothing marks one. The note explains how the path
// was chosen, so an inferred target is never silent.
func (a *App) target(cmd *command) (path, note string, err error) {
	if cmd.fs.NArg() > 0 {
		p, err := cmd.one("project path")
		return p, "", err
	}
	cwd, err := a.workingDir()
	if err != nil {
		return "", "", err
	}
	if root, marker, ok := project.FindRoot(cwd); ok {
		return root, "project root, found " + marker, nil
	}
	return cwd, "current directory", nil
}

// one returns the single path argument a command expects.
func (c *command) one(what string) (string, error) {
	switch c.fs.NArg() {
	case 1:
		return c.fs.Arg(0), nil
	case 0:
		return "", usagef("missing %s\n\n%s", what, c.usage)
	default:
		return "", usagef("expected one %s, got %d\n\n%s", what, c.fs.NArg(), c.usage)
	}
}

func (a *App) printUsage() {
	fmt.Fprint(a.Out, `clav — park git projects to reclaim disk space

Usage:
  clav park [path]       delete a project's tracked content, keep your own files
  clav sweep [path]      find projects worth parking under a directory
  clav restore [path]    clone the project back and put the kept files with it
  clav list              list parked projects
  clav inspect <path>    show details about a parked project
  clav remove <path>     forget a parked project

Flags:
  --push                 park pushes what the remote is missing first
  --rescue               park saves stashes and uncommitted changes into clav
  --dry-run              show what park or sweep would do, change nothing
  --force                park with unpushed work / replace an existing directory
  --keep-ignored         park keeps ignored build and dependency directories
  --verbose, -v          print each step
  --help, -h             show this help
  --version              print the clav version

park deletes what git can give back: tracked files, .git, and ignored build
and dependency directories (node_modules, target, .venv, dist and friends).
Everything else in the folder — notes, .env files, editor settings, anything
git does not know about — is left untouched.

Parking is refused when the remote does not already have your work: dirty
tracked files, unpushed commits or a stash. --push sends the commits, --rescue
saves the stashes and uncommitted changes into ~/.clav and puts them back on
the stash when you restore, and --force overrides the lot.

A repository with no remote has nowhere to be cloned back from, so clav
archives the whole directory into ~/.clav (override with CLAV_HOME).
`)
}
