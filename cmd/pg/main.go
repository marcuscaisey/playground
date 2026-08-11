// Package main is the entrypoint to pg.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/marcuscaisey/playground/internal/session"
)

func main() {
	os.Exit(runCLI())
}

const completeSubcmd = "__complete"

// runCLI runs the pg command line interface and returns its exit status.
func runCLI() int {
	// SIGHUP is sent by tmux for the kill-pane, kill-window, kill-session, etc commands. e.g. we'll
	// receive it if the tmux pane that we're running in gets killed.
	ctx, stop := signalContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()
	args := os.Args[1:] // Drop program name
	if len(args) > 0 && args[0] == completeSubcmd {
		completeArgs := args[1:] // Drop subcommand
		return runCompleteCLI(completeArgs)
	}
	return runMainCLI(ctx, args)
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

//go:embed VERSION
var VERSION string

// runMainCLI runs the main command line interface and returns the exit status.
func runMainCLI(ctx context.Context, arguments []string) (status int) {
	args := args{}
	if status := parseArgs(arguments, &args, parseModeLoud); status >= 0 {
		return status
	}

	if args.Version {
		fmt.Println(strings.TrimSpace(VERSION))
		return 0
	}

	userTemplatesDir, err := userTemplatesDir()
	if err != nil {
		return errorExit(err)
	}

	ses, err := session.New(args.SessionName, args.TemplateName, args.SessionsDir, userTemplatesDir)
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

	if err := ses.Run(ctx, args.OutputPaneSize, args.Vertical, args.Editor, args.EditorArgs...); err != nil {
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
			return errorExitf("tmux not found in $PATH")
		}
		if errors.Is(err, session.ErrShNotFound) {
			return errorExitf("sh not found in $PATH")
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

type args struct {
	TemplateName   string
	SessionName    string
	Vertical       bool
	OutputPaneSize session.TmuxPaneSize
	Editor         string
	EditorArgs     []string
	SessionsDir    string
	Version        bool // mutually exclusive with the other args
}

// parseMode controls the behaviour of [parseArgs].
type parseMode int

const (
	// In parseModeLoud, errors and help are output to stdout and stderr.
	parseModeLoud parseMode = iota
	// In parseModeSilent, nothing is output to stdout or stderr.
	parseModeSilent
)

// parseArgs parses the provided arguments and stores the result in the value pointed to by args.
// It returns an exit status >= 0 if the program should exit.
// If -help is provided, a help message is printed.
// If -completion-script $shell is provided, the completion script for $shell is printed.
func parseArgs(arguments []string, args *args, mode parseMode) int {
	errorExitf := func(format string, a ...any) int {
		if mode == parseModeLoud {
			msg := fmt.Sprintf(format, a...)
			fmt.Fprintf(os.Stderr, "error: %s\n", msg)
		}
		return generalExitStatus
	}
	errorExit := func(err error) int {
		return errorExitf("%s", err)
	}

	// By default, [flag.Parse] emits parsing errors without:
	//   - "error: " before the error message
	//   - A blank line betweeen the error message and the usage text
	// These are minor annoyances but make the command line interface inconsistent. We therefore
	// construct our own flag set to control what is emitted.

	// Use [flag.ContinueOnError] error handling so that [flag.FlagSet.Parse] returns parsing errors
	// to us instead of exiting. We can then prefix them with "error: " and print a blank line where
	// required.
	flagSet := flag.NewFlagSet("pg", flag.ContinueOnError)
	// Discard all output until required (when we call [flag.FlagSet.PrintDefaults]).
	// [flag.FlagSet.Parse] emits parsing errors through the output, so we need to suppress this
	// since we're going to be emitting these errors ourself.
	flagSet.SetOutput(io.Discard)

	setEditorAndArgs := func(s string) error {
		fields := strings.Fields(strings.TrimSpace(s))
		if len(fields) == 0 {
			return fmt.Errorf("must not be empty")
		}
		args.Editor = fields[0]
		args.EditorArgs = fields[1:]
		return nil
	}

	completionScriptShell := new(shell)
	defaultVertical := false
	if value := os.Getenv("PG_VERTICAL"); value != "" {
		b, err := strconv.ParseBool(value)
		if err != nil {
			return errorExitf("invalid value %q for $PG_VERTICAL: must be one of 1, t, T, TRUE, true, True, 0, f, F, FALSE, false, False", value)
		}
		defaultVertical = b
	}
	args.Editor = "vi"
	if value := os.Getenv("EDITOR"); value != "" {
		if err := setEditorAndArgs(value); err != nil {
			return errorExitf("invalid value %q for $EDITOR: %s", value, err)
		}
	}
	args.OutputPaneSize = session.TmuxPaneSize("35%")
	if value := os.Getenv("PG_OUTPUT_PANE_SIZE"); value != "" {
		if err := args.OutputPaneSize.Set(value); err != nil {
			return errorExitf("invalid value %q for $PG_OUTPUT_PANE_SIZE: %s", value, err)
		}
	}
	defaultSessionsDir, err := defaultSessionsDir()
	if err != nil {
		return errorExit(err)
	}
	if value := os.Getenv("PG_SESSIONS_DIR"); value != "" {
		defaultSessionsDir = value
	}

	flagSet.Var(completionScriptShell, "completion-script", "generate a `shell` completion script for bash, zsh, or fish")
	flagSet.Func("editor", "`command` to open the editor (default \"vi\")", setEditorAndArgs)
	help := flagSet.Bool("help", false, "print this message")
	flagSet.Var(&args.OutputPaneSize, "output-pane-size", "output pane `size` in lines/columns, or a percentage")
	flagSet.StringVar(&args.SessionsDir, "sessions-dir", defaultSessionsDir, "named sessions `directory`")
	flagSet.BoolVar(&args.Version, "version", false, "print version")
	flagSet.BoolVar(&args.Vertical, "vertical", defaultVertical, "split the window vertically")

	printUsage := func(w io.Writer) {
		if mode == parseModeSilent {
			return
		}
		_, _ = fmt.Fprintln(w, "Usage:")
		_, _ = fmt.Fprintln(w, "    pg [options] template [session]")
		_, _ = fmt.Fprintln(w, "    pg -completion-script shell")
		_, _ = fmt.Fprintln(w, "    pg -version")
		_, _ = fmt.Fprintln(w, "    pg -help")
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "Options:")
		flagSet.SetOutput(w) // Unsuppress output for [flag.FlagSet.PrintDefaults]
		flagSet.PrintDefaults()
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "Docs: man pg")
		_, _ = fmt.Fprintln(w, "or:   https://github.com/marcuscaisey/playground/blob/main/docs/pg.md")
	}
	usageErrorf := func(msg string, a ...any) int {
		if mode == parseModeLoud {
			fmt.Fprintf(os.Stderr, "error: %s\n\n", fmt.Sprintf(msg, a...))
			printUsage(os.Stderr)
		}
		return usageErrorExitStatus
	}

	if err := flagSet.Parse(arguments); err != nil {
		return usageErrorf("%s", err)
	}

	if *help {
		printUsage(os.Stdout)
		return 0
	}

	if args.Version && flagSet.NArg() == 0 {
		return -1
	}

	if *completionScriptShell != 0 {
		if flagSet.NFlag() > 1 || flagSet.NArg() > 0 {
			return usageErrorf("-completion-script must be provided on its own")
		}
		completionDescs := map[string]string{}
		flagSet.VisitAll(func(f *flag.Flag) {
			_, usage := flag.UnquoteUsage(f)
			completionDescs[f.Name] = usage
		})
		completionScript, err := completionScript(completionDescs, *completionScriptShell)
		if err != nil {
			return errorExit(err)
		}
		if mode == parseModeLoud {
			fmt.Print(completionScript)
		}
		return 0
	}

	if args.TemplateName = flagSet.Arg(0); args.TemplateName == "" {
		return usageErrorf("template not provided")
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

// runCompleteCLI runs the command line interface for the __complete subcommand and returns the exit
// status.
// The __complete subcommand prints completions for templates and sessions.
func runCompleteCLI(arguments []string) int {
	flagSet := flag.NewFlagSet(completeSubcmd, flag.ExitOnError)
	shell := new(shell)
	flagSet.Var(shell, "shell", "Shell to generate completions for")
	printUsage := func() {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "    pg [-shell (bash|zsh|fish)] __complete templates")
		fmt.Fprintln(os.Stderr, "    pg [-shell (bash|zsh|fish)] __complete sessions <template> <current-args>")
	}
	usageError := func() int {
		printUsage()
		return usageErrorExitStatus
	}
	flagSet.Usage = printUsage

	_ = flagSet.Parse(arguments) // Parse exits on any error

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
		if flagSet.NArg() < 2 {
			return usageError()
		}
		templateName := flagSet.Arg(1)
		currentArguments := flagSet.Args()[2:]
		currentArgs := &args{}
		parseArgs(currentArguments, currentArgs, parseModeSilent)
		if currentArgs.SessionsDir == "" {
			return errorExitf("-sessions-dir not set")
		}
		if err := completeSessions(templateName, currentArgs.SessionsDir, *shell); err != nil {
			return errorExit(err)
		}

	default:
		return usageError()
	}

	return 0
}

// errorExit reports an error message and returns the general error exit status 1.
func errorExitf(format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	return generalExitStatus
}

// errorExit reports an error and returns the general error exit status 1.
func errorExit(err error) int {
	return errorExitf("%s", err)
}
