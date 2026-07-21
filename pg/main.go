package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	os.Exit(cli(os.Args))
}

const (
	resultsSubcmd  = "__results"
	completeSubcmd = "__complete"
)

// cli parses its args, runs one of the session, complete, or results commands, and returns the
// corresponding exit code.
func cli(args []string) int {
	// SIGUP is sent by tmux for the kill-pane, kill-window, kill-session, etc commands
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cmdArgs := args[1:] // Drop program name
	if len(cmdArgs) > 1 {
		subcmd := cmdArgs[0]
		subcmdArgs := cmdArgs[1:] // Drop subcommand
		switch subcmd {
		case resultsSubcmd:
			return resultsCLI(ctx, subcmdArgs)
		case completeSubcmd:
			return completeCLI(subcmdArgs)
		}
	}
	return sessionCLI(ctx, cmdArgs)
}

const usageErrorExitCode = 2

const sessionsDirEnvVar = "PG_SESSIONS_DIR"

// sessionCLI parses the args for the session command, runs a session, reports any errors, and
// returns an exit code.
// It returns 0 for success or help, 2 for incorrect CLI usage, and 1 for other errors.
func sessionCLI(ctx context.Context, args []string) int {
	// By default, [flag.Parse] emits parsing errors without:
	//   - "error: " before the error message
	//   - A blank line betweeen the error message and the usage text
	// These are minor annoyances but make the command line interface inconsistent. We there
	// construct our own flag set to control what is emitted.

	// Use [flag.ContinueOnError] error handling so that [flagSet.Parse] returns parsing errors to
	// us instead of exiting. We can then prefix them with "error: " and print a blank line where
	// required.
	flagSet := newFlagSet("pg", flag.ContinueOnError)
	// Discard all output until required (when we call [flagSet.PrintDefaults]). [flagSet.Parse]
	// emits parsing errors through [flagSet.Output], so we need to suppress this since
	// we're going to be emitting these errors ourself.
	flagSet.SetOutput(io.Discard)

	defaultSessionsDir, err := defaultSessionsDir()
	if err != nil {
		return errorExit(err)
	}

	vertical := flagSet.Bool("vertical", false, "Split the window vertically instead of horizontally.")
	resultsPaneSize := flagSet.StringWithEnvVar("results-pane-size", "PG_RESULTS_PANE_SIZE", "35%", "Results pane `size` in lines, or as a percentage if followed by '%'.\n")
	editor := flagSet.StringWithEnvVar("editor", "EDITOR", "vi", "Shell `command` to open the editor."+`
For nvim, vim, vi, emacs, helix, kakoune, nano, and pico, the template
entrypoint is opened at the start line defined by the template.
`)
	sessionsDir := flagSet.StringWithEnvVar("sessions-dir", sessionsDirEnvVar, defaultSessionsDir, "Named sessions `directory`.\n")
	completionScriptShell := new(shell)
	flagSet.Var(completionScriptShell, "completion-script", "Generate a `shell` completion script for bash, zsh, or fish."+`
Example usage:
    source <(pg -completion-script bash)
    source <(pg -completion-script zsh)
    pg -completion-script fish | source`)
	help := flagSet.Bool("help", false, "Print help message")

	usage := func() {
		fmt.Fprintln(os.Stderr, "Usage: pg [options] <template-name> [<session-name>]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flagSet.SetOutput(os.Stderr) // Unsuppress output for [flagSet.PrintDefaults]
		flagSet.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Environment variables in brackets are used as defaults when set.")
	}
	usageErrorf := func(msg string, a ...any) int {
		fmt.Fprintf(os.Stderr, "error: %s\n\n", fmt.Sprintf(msg, a...))
		usage()
		return usageErrorExitCode
	}

	if err := flagSet.Parse(args); err != nil {
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
		flagDescriptions := flagSet.CompletionDescriptions()
		completionScript, err := completionScript(flagDescriptions, *completionScriptShell)
		if err != nil {
			return errorExit(err)
		}
		fmt.Print(completionScript)
		return 0
	}

	templateName := flagSet.Arg(0)
	if templateName == "" {
		return usageErrorf("template name not provided")
	}
	sessionName := flagSet.Arg(1)
	if flagSet.NArg() > 2 {
		return usageErrorf("unexpected arguments: %s", strings.Join(flagSet.Args()[2:], ", "))
	}

	userTemplatesDir, err := userTemplatesDir()
	if err != nil {
		return errorExit(err)
	}
	pgPath, err := os.Executable()
	if err != nil {
		return errorExit(err)
	}
	opts := runSessionOptions{
		TemplateName:         templateName,
		SessionName:          sessionName,
		Vertical:             *vertical,
		ResultsPaneSize:      *resultsPaneSize,
		Editor:               *editor,
		SessionsDir:          *sessionsDir,
		SessionsDirIsDefault: *sessionsDir == defaultSessionsDir,
		UserTemplatesDir:     userTemplatesDir,
		PgPath:               pgPath,
	}
	if err := runSession(ctx, opts); err != nil {
		return errorExit(err)
	}

	return 0
}

const subdirName = "pg" // Subdirectory name used when storing pg specific files in another directory

func defaultSessionsDir() (string, error) {
	dataDir, err := xdgDataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, subdirName, "sessions"), nil
}

func userTemplatesDir() (string, error) {
	configDir, err := xdgConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, subdirName, "templates"), nil
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

// resultsCLI parses the args for the results command, prints the results for a session, reports any
// errors, and returns an exit code.
// It returns 0 for success or help, 2 for incorrect CLI usage, and 1 for other errors.
func resultsCLI(ctx context.Context, args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "Usage: pg __results <session-dir> <entrypoint>")
		return usageErrorExitCode
	}
	sessionDir := args[0]
	entrypoint := args[1]
	if err := printSessionResults(ctx, sessionDir, entrypoint); err != nil {
		return errorExit(err)
	}
	return 0
}

// completeCLI parses the args for the complete command, prints the requested completions, reports
// any errors, and returns an exit code.
// It returns 0 for success or help, 2 for incorrect CLI usage, and 1 for other errors.
func completeCLI(args []string) int {
	flagSet := flag.NewFlagSet(completeSubcmd, flag.ExitOnError)
	shell := new(shell)
	flagSet.Var(shell, "shell", "Shell to generate completions for")
	usage := func() {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "    pg [-shell (bash|zsh|fish)] __complete templates")
		fmt.Fprintln(os.Stderr, "    pg [-shell (bash|zsh|fish)] __complete sessions <template-name>")
	}
	usageError := func() int {
		usage()
		return usageErrorExitCode
	}
	flagSet.Usage = usage

	_ = flagSet.Parse(args) // Parse will exit on any error

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

// errorExit reports an error and returns the general error exit code 1.
func errorExit(err error) int {
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	return 1
}
