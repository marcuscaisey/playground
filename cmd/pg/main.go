// Package main is the entrypoint to pg.
package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/marcuscaisey/playground/internal/session"
)

func main() {
	os.Exit(runCLI())
}

const (
	resultsSubcmd  = "__results"
	completeSubcmd = "__complete"
)

// runCLI runs the pg command line interface and returns its exit status.
func runCLI() int {
	// SIGHUP is sent by tmux for the kill-pane, kill-window, kill-session, etc commands. e.g. we'll
	// receive it if the tmux pane that we're running in gets killed.
	ctx, stop := signalContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	cmdArgs := os.Args[1:] // Drop program name
	if len(cmdArgs) > 1 {
		subcmd := cmdArgs[0]
		subcmdArgs := cmdArgs[1:] // Drop subcommand
		switch subcmd {
		case resultsSubcmd:
			return runResultsCLI(ctx, subcmdArgs)
		case completeSubcmd:
			return runCompleteCLI(subcmdArgs)
		}
	}
	return runMainCLI(ctx, cmdArgs)
}

// signalContext is like [signal.NotifyContext] except the stop function sends the process the
// received signal (if any) so that the parent process can detect that the process was killed by a
// signal and by which one.
func signalContext(parent context.Context, signals ...os.Signal) (ctx context.Context, stop context.CancelFunc) {
	// [signal.NotifyContext] cancels the context with a cause error describing the received signal,
	// but it can't be handled programatically because it's not exported. Instead, we use
	// [signal.Notify] and store the received signal for later.
	// See: https://github.com/golang/go/issues/60756
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, signals...)

	ctx, cancel := context.WithCancelCause(parent)
	var receivedSignal os.Signal
	go func() {
		select {
		case receivedSignal = <-sigc:
			cancel(errors.New(receivedSignal.String()))
		case <-ctx.Done():
		}
	}()

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		panic(fmt.Errorf("failed to find process: %s", err))
	}
	return ctx, func() {
		cancel(nil)
		signal.Stop(sigc)
		if receivedSignal == nil {
			return
		}
		if err := proc.Signal(receivedSignal); err != nil {
			fmt.Fprintf(os.Stderr, "error: sending process signal %q: %s\n", receivedSignal, err)
		}
		select {}
	}
}

// runMainCLI runs the main command line interface and returns the exit status.
// The main command line interface is responsible for running sessions.
func runMainCLI(ctx context.Context, a []string) (status int) {
	args := args{}
	if status := parseArgs(a, &args); status >= 0 {
		return status
	}

	userTemplatesDir, err := userTemplatesDir()
	if err != nil {
		return errorExit(err)
	}
	pgPath, err := os.Executable()
	if err != nil {
		return errorExit(err)
	}

	ses, err := session.New(args.SessionName, args.TemplateName, args.SessionsDir, userTemplatesDir, pgPath)
	if err != nil {
		if errors.Is(err, session.ErrTemplateNotFound) {
			return errorExitf("template %q not found", args.TemplateName)
		}
		if invalidErr, ok := errors.AsType[*session.InvalidTemplateError](err); ok {
			return errorExitf("template %q (%s) is invalid: %s", invalidErr.Name, invalidErr.Source, invalidErr.Reason)
		}
		if invalidErr, ok := errors.AsType[*session.InvalidTemplateNameError](err); ok {
			return errorExitf("template name %q is invalid: %s", invalidErr.Name, invalidErr.Reason)
		}
		if invalidErr, ok := errors.AsType[*session.InvalidNameError](err); ok {
			return errorExitf("session name %q is invalid: %s", invalidErr.Name, invalidErr.Reason)
		}
		return errorExit(err)
	}
	defer func() {
		// If there was an error, keep the anonymous session directory around so its contents can be
		// recovered
		if status == 0 && ses.Name == "" && ses.Dir != "" {
			// Anonymous session directory is temporary so even if we fail to clean it up, it should
			// get cleaned up by the OS at some point.
			_ = os.RemoveAll(ses.Dir)
		}
	}()

	if err := ses.Run(ctx, args.ResultsPaneSize, args.Vertical, args.Editor, args.EditorArgs...); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			fmt.Fprintln(os.Stderr, cause)
			return generalExitStatus
		}
		if errors.Is(err, session.ErrEditorError) {
			// This is expected when the user doesn't want to be prompted to save their anonymous
			// session. If there was an actual error, the editor will log it to stdout/stderr
			// anyway.
			return 0
		}
		if notFoundErr, ok := errors.AsType[*session.EditorNotFoundError](err); ok {
			return errorExitf("editor %q not found in $PATH", notFoundErr.Editor)
		}
		if errors.Is(err, session.ErrTmuxNotFound) {
			return errorExitf("tmux not found on $PATH")
		}
		return errorExit(err)
	}

	if ses.Name != "" {
		return 0
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("Enter a name to save this session (or Ctrl-D to abort): ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return errorExitf("saving session: reading line from stdin: %s", err)
			}
			fmt.Println()
			fmt.Println("Exit editor with a non-zero status to skip this prompt")
			return 0
		}
		sessionName := scanner.Text()
		if sessionName == "" {
			continue
		}
		if err := ses.Save(sessionName); err != nil {
			if alreadyExistsErr, ok := errors.AsType[*session.AlreadyExistsError](err); ok {
				fmt.Printf("A %q session named %q already exists:\n", ses.TemplateName, alreadyExistsErr.Name)
				fmt.Printf("  %s\n", alreadyExistsErr.Dir)
				continue
			}
			if invalidErr, ok := errors.AsType[*session.InvalidNameError](err); ok {
				fmt.Printf("session name %q is invalid: %s\n", invalidErr.Name, invalidErr.Reason)
				continue
			}
			return errorExit(err)
		}
		fmt.Printf("Saved %q session %q to:\n", ses.TemplateName, ses.Name)
		fmt.Printf("  %s\n", ses.Dir)
		fmt.Printf("To resume, run:\n")
		sessionsDirFlag := ""
		if defaultSessionsDir, err := defaultSessionsDir(); err != nil {
			return errorExit(err)
		} else if args.SessionsDir != defaultSessionsDir {
			sessionsDirFlag = fmt.Sprintf("-sessions-dir %s ", shellQuote(args.SessionsDir))
		}
		fmt.Printf("  pg %s%s %s\n", sessionsDirFlag, shellQuote(ses.TemplateName), shellQuote(ses.Name))
		return 0
	}
}

const (
	generalExitStatus    = 1
	usageErrorExitStatus = 2
)

const sessionsDirEnvVar = "PG_SESSIONS_DIR"

type args struct {
	TemplateName    string
	SessionName     string
	Vertical        bool
	ResultsPaneSize session.TmuxPaneSize
	Editor          string
	EditorArgs      []string
	SessionsDir     string
}

// parseArgs parses the provided arguments and stores the result in the value pointed to by args.
// It returns an exit status >= 0 if the program should exit.
// If -help is provided, a help message is printed.
// If -completion-script $shell is provided, the completion script for $shell is printed.
func parseArgs(arguments []string, args *args) int {
	// By default, [flag.Parse] emits parsing errors without:
	//   - "error: " before the error message
	//   - A blank line betweeen the error message and the usage text
	// These are minor annoyances but make the command line interface inconsistent. We therefore
	// construct our own flag set to control what is emitted.

	// Use [flag.ContinueOnError] error handling so that [flagSet.Parse] returns parsing errors to
	// us instead of exiting. We can then prefix them with "error: " and print a blank line where
	// required.
	flagSet := newFlagSet("pg", flag.ContinueOnError)
	// Discard all output until required (when we call [flagSet.PrintDefaults]). [flagSet.Parse]
	// emits parsing errors through the output, so we need to suppress this since we're going to be
	// emitting these errors ourself.
	flagSet.SetOutput(io.Discard)

	defaultSessionsDir, err := defaultSessionsDir()
	if err != nil {
		return errorExit(err)
	}

	setEditorAndArgs := func(s string) error {
		fields := strings.Fields(strings.TrimSpace(s))
		if len(fields) == 0 {
			return fmt.Errorf("must not be empty")
		}
		args.Editor = fields[0]
		args.EditorArgs = fields[1:]
		return nil
	}

	const (
		verticalDesc        = "Split the window vertically instead of horizontally."
		resultsPaneSizeDesc = "Results pane `size` in lines/columns, or a percentage if followed by '%'.\n"
		editorDesc          = "Shell `command` to open the editor." + `
For nvim, vim, vi, emacs, helix, kakoune, nano, and pico, the template
entrypoint is opened at the start line defined by the template.
(default "vi")`
		sessionsDirDesc      = "Named sessions `directory`.\n"
		completionScriptDesc = "Generate a `shell` completion script for bash, zsh, or fish." + `
Example usage:
    source <(pg -completion-script bash)
    source <(pg -completion-script zsh)
    pg -completion-script fish | source`
		helpDesc = "Print help message."
	)

	flagSet.BoolVar(&args.Vertical, "vertical", false, verticalDesc)
	args.ResultsPaneSize = session.TmuxPaneSize("35%")
	flagSet.VarWithEnvVar(&args.ResultsPaneSize, "results-pane-size", "PG_RESULTS_PANE_SIZE", resultsPaneSizeDesc)
	args.Editor = "vi"
	flagSet.FuncWithEnvVar("editor", "EDITOR", editorDesc, setEditorAndArgs)
	flagSet.StringVarWithEnvVar(&args.SessionsDir, "sessions-dir", sessionsDirEnvVar, defaultSessionsDir, sessionsDirDesc)
	completionScriptShell := new(shell)
	flagSet.Var(completionScriptShell, "completion-script", completionScriptDesc)
	help := flagSet.Bool("help", false, helpDesc)

	printUsage := func() {
		fmt.Fprintln(os.Stderr, "Usage: pg [options] <template-name> [<session-name>]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flagSet.SetOutput(os.Stderr) // Unsuppress output for [flagSet.PrintDefaults]
		flagSet.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Environment variables in brackets are used as defaults when set and valid.")
	}
	usageErrorf := func(msg string, a ...any) int {
		fmt.Fprintf(os.Stderr, "error: %s\n\n", fmt.Sprintf(msg, a...))
		printUsage()
		return usageErrorExitStatus
	}

	if err := flagSet.Parse(arguments); err != nil {
		return usageErrorf("%s", err)
	}

	if *help {
		printUsage()
		return 0
	}

	if *completionScriptShell != 0 {
		if flagSet.NFlag() > 1 || flagSet.NArg() > 0 {
			return usageErrorf("-completion-script flag must be provided on its own")
		}
		completionDescs := flagSet.CompletionDescriptions()
		completionScript, err := completionScript(completionDescs, *completionScriptShell)
		if err != nil {
			return errorExit(err)
		}
		fmt.Print(completionScript)
		return 0
	}

	if args.TemplateName = flagSet.Arg(0); args.TemplateName == "" {
		return usageErrorf("template name not provided")
	}
	args.SessionName = flagSet.Arg(1)
	if flagSet.NArg() > 2 {
		return usageErrorf("unexpected arguments: %s", strings.Join(flagSet.Args()[2:], ", "))
	}

	return -1
}

func defaultSessionsDir() (string, error) {
	dataDir, err := xdgDataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "pg", "sessions"), nil
}

func userTemplatesDir() (string, error) {
	configDir, err := xdgConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "pg", "templates"), nil
}

func xdgDataHome() (string, error) {
	return xdgDir("XDG_DATA_HOME", filepath.Join(".local", "share"))
}

func xdgConfigHome() (string, error) {
	return xdgDir("XDG_CONFIG_HOME", ".config")
}

func xdgDir(envVar string, homeSubDir string) (string, error) {
	if dir := os.Getenv(envVar); dir != "" && filepath.IsAbs(dir) {
		return dir, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, homeSubDir), nil
}

// shellQuote returns arg quoted so that it can be used as a literal shell command argument.
// If arg consists entirely of "safe" characters, then it's returned unchanged.
func shellQuote(arg string) string {
	safeCharsRe := regexp.MustCompile(`^[\w\-./]+$`)
	if safeCharsRe.MatchString(arg) {
		return arg
	}
	return fmt.Sprintf("'%s'", strings.ReplaceAll(arg, "'", `'\''`))
}

// runResultsCLI runs the command line interface for the __results subcommand and returns the exit
// status.
// The __results subcommand runs in the results pane and executes the entrypoint on save.
func runResultsCLI(ctx context.Context, args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "Usage: pg __results <session-dir> <entrypoint>")
		return usageErrorExitStatus
	}
	sessionDir := args[0]
	entrypoint := args[1]
	if err := session.PrintResults(ctx, sessionDir, entrypoint); err != nil {
		return errorExit(err)
	}
	return 0
}

// runCompleteCLI runs the command line interface for the __complete subcommand and returns the exit
// status.
// The __complete subcommand prints completions for templates and sessions.
func runCompleteCLI(args []string) int {
	flagSet := flag.NewFlagSet(completeSubcmd, flag.ExitOnError)
	shell := new(shell)
	flagSet.Var(shell, "shell", "Shell to generate completions for")
	printUsage := func() {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "    pg [-shell (bash|zsh|fish)] __complete templates")
		fmt.Fprintln(os.Stderr, "    pg [-shell (bash|zsh|fish)] __complete sessions <template-name>")
	}
	usageError := func() int {
		printUsage()
		return usageErrorExitStatus
	}
	flagSet.Usage = printUsage

	_ = flagSet.Parse(args) // Parse exits on any error

	if flagSet.NArg() < 1 {
		return usageError()
	}

	switch flagSet.Arg(0) {
	case "templates":
		if flagSet.NArg() != 1 {
			return usageError()
		}
		userTemplatesDir, err := userTemplatesDir()
		if err != nil {
			return errorExit(err)
		}
		if err := completeTemplates(userTemplatesDir, *shell); err != nil {
			return errorExit(err)
		}

	case "sessions":
		if flagSet.NArg() != 2 {
			return usageError()
		}
		templateName := flagSet.Arg(1)
		defaultSessionsDir, err := defaultSessionsDir()
		if err != nil {
			return errorExit(err)
		}
		sessionsDir := cmp.Or(os.Getenv(sessionsDirEnvVar), defaultSessionsDir)
		if err := completeSessions(templateName, sessionsDir, *shell); err != nil {
			return errorExit(err)
		}

	default:
		return usageError()
	}

	return 0
}

// errorExit reports an error and returns the general error exit status 1.
func errorExit(err error) int {
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	return generalExitStatus
}

// errorExit reports an error message and returns the general error exit status 1.
func errorExitf(format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	return generalExitStatus
}
