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

const resultsCmd = "__results"

// cli parses its args, runs one of the session or results commands, and returns the corresponding
// exit code.
func cli(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if len(args) > 1 && args[1] == resultsCmd {
		return resultsCLI(ctx, args[2:]) // Drop program name and command.
	} else {
		return sessionCLI(ctx, args[1:]) // Drop program name.
	}
}

const usageErrorExitCode = 2

// sessionCLI parses the args for the session command, runs a session, reports any errors, and
// returns an exit code.
// It returns 0 for success or help, 2 for incorrect CLI usage, and 1 for other errors.
func sessionCLI(ctx context.Context, args []string) int {
	// [flag.Parse] emits parsing errors without:
	//   - "error: " before the error message
	//   - A blank line betweeen the error message and the usage text
	// We use our own [flag.FlagSet] so that we can format the output how we want.
	flagSet := flag.NewFlagSet("pg", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return errorExit(err)
	}
	dataDir := filepath.Join(homeDir, ".pg")

	const (
		editorEnvVar      = "EDITOR"
		defaultEditor     = "vi"
		sessionsDirEnvVar = "PG_SESSIONS_DIR"
	)
	defaultSessionsDir := filepath.Join(dataDir, "sessions")
	editorFlag := flagSet.String("editor", "", fmt.Sprintf("Editor to open; falls back to $%s, then %q.", editorEnvVar, defaultEditor))
	sessionsDirFlag := flagSet.String("sessions-dir", "", fmt.Sprintf("Where named sessions are stored; falls back to $%s, then %q.", sessionsDirEnvVar, defaultSessionsDir))
	printHelp := flagSet.Bool("help", false, "Print this message.")

	usage := func() {
		fmt.Fprintln(os.Stderr, "Usage: pg [options] <template-name> [<session-name>]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flagSet.SetOutput(nil)
		defer flagSet.SetOutput(io.Discard)
		flagSet.PrintDefaults()
	}
	usageErrorf := func(msg string, a ...any) int {
		fmt.Fprintf(os.Stderr, "error: %s\n\n", fmt.Sprintf(msg, a...))
		usage()
		return usageErrorExitCode
	}

	if err := flagSet.Parse(args); err != nil {
		return usageErrorf("%s", err)
	}

	if *printHelp {
		usage()
		return 0
	}

	templateName := flagSet.Arg(0)
	if templateName == "" {
		return usageErrorf("template name not provided")
	}
	sessionName := flagSet.Arg(1)
	if args := flagSet.Args(); len(args) > 2 {
		return usageErrorf("unexpected arguments: %s", strings.Join(args[1:], ", "))
	}

	pgPath, err := os.Executable()
	if err != nil {
		return errorExit(err)
	}
	opts := runSessionOptions{
		UserTemplatesDir: filepath.Join(dataDir, "templates"),
		PgPath:           pgPath,
		TemplateName:     templateName,
		SessionName:      sessionName,
		Editor:           cmp.Or(*editorFlag, os.Getenv(editorEnvVar), defaultEditor),
		SessionsDir:      cmp.Or(*sessionsDirFlag, os.Getenv(sessionsDirEnvVar), defaultSessionsDir),
	}
	if err := runSession(ctx, opts); err != nil {
		return errorExit(err)
	}

	return 0
}

// resultsCLI parses the args for the results command, prints the results for a session, reports any
// errors, and returns an exit code.
// It returns 0 for success or help, 2 for incorrect CLI usage, and 1 for other errors.
func resultsCLI(ctx context.Context, args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: pg %s <session-dir> <entrypoint>\n", resultsCmd)
		return usageErrorExitCode
	}
	sessionDir := args[0]
	entrypoint := args[1]
	if err := printSessionResults(ctx, sessionDir, entrypoint); err != nil {
		return errorExit(err)
	}
	return 0
}

// errorExit reports an error and returns the general error exit code 1.
func errorExit(err error) int {
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	return 1
}
