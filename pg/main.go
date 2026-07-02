package main

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
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

	const (
		editorEnvVar  = "EDITOR"
		defaultEditor = "vi"
	)
	editorFlag := flagSet.String("editor", "", fmt.Sprintf("Editor to open; falls back to $%s, then %q.", editorEnvVar, defaultEditor))
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
	editor := cmp.Or(*editorFlag, os.Getenv(editorEnvVar), defaultEditor)
	if err := runSession(ctx, pgPath, templateName, sessionName, editor); err != nil {
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

const pgDir = ".pg"

// runSession runs a session using the given template.
// It sets up the session directory (creates it and copies in the template's files), starts the
// results command in a new tmux pane, and opens the template's entrypoint in the given editor.
// If the session is named (sessionName != ""), then its directory is set up in the named sessions
// directory if it doesn't already exist. Otherwise, the session is anonymous and it's set up in the
// anonymous sessions directory with a generated name.
// pgPath must be the absolute path to the pg executable. This is used to start the results command.
func runSession(ctx context.Context, pgPath string, templateName string, sessionName string, editor string) (err error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("running session: setting up session directory: %s", err)
	}

	template, err := loadTemplate(homeDir, templateName)
	if err != nil {
		if errors.Is(err, errTemplateNotFound) {
			return fmt.Errorf("template %q not found", templateName)
		}
		if templateInvalidErr, ok := errors.AsType[*templateInvalidError](err); ok {
			templateSrc := fmt.Sprintf("built-in template %q", templateInvalidErr.Name)
			if templateInvalidErr.Path != "" {
				templateSrc = fmt.Sprintf("template %q (%q)", templateInvalidErr.Name, templateInvalidErr.Path)
			}
			return fmt.Errorf("%s is invalid: %s", templateSrc, templateInvalidErr.Reason)
		}
		return fmt.Errorf("running session: %s", err)
	}

	sessionType := "named"
	if sessionName == "" {
		sessionType = "anonymous"
		sessionName = fmt.Sprintf("%s-%s", templateName, time.Now().Format(fmt.Sprintf("%s-%s", time.DateOnly, time.TimeOnly)))
	}
	sessionDir := filepath.Join(homeDir, pgDir, "sessions", sessionType, sessionName)
	if ok, err := fileExists(sessionDir); err != nil {
		return fmt.Errorf("running session: %s", err)
	} else if !ok {
		if err := setupSessionDir(sessionDir, template.FS); err != nil {
			return fmt.Errorf("running session: %s", err)
		}
	}

	resultsCmd := fmt.Sprintf("%q %s %q %q", pgPath, resultsCmd, sessionDir, template.Entrypoint)
	closeResultsPane, err := runInNewTmuxPane(ctx, resultsCmd)
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}
	defer func() {
		if closeErr := closeResultsPane(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("running session: closing results pane: %s", closeErr))
		}
	}()

	editorCmd := exec.CommandContext(ctx, editor, template.Entrypoint)
	editorCmd.Dir = sessionDir
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("running session with editor %q: %s", editor, cmdErrMsg(err))
	}

	return nil
}

// printSessionResults watches a session directory for changes to the entrypoint, executing the
// run script and printing the results when changes occur.
func printSessionResults(ctx context.Context, sessionDir string, entrypoint string) (err error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("printing session results: %s", err)
	}
	defer func() {
		if closeErr := watcher.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("printing session results: closing session directory watcher: %s", closeErr))
		}
	}()

	if err := watcher.Add(sessionDir); err != nil {
		return fmt.Errorf("printing session results: adding session directory watcher: %s", err)
	}

	entrypointPath := filepath.Join(sessionDir, entrypoint)
	var startTimerC <-chan time.Time
	stopCurrentRun := func() {}
	defer stopCurrentRun()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("printing session results: watcher events channel closed")
			}
			if event.Name != entrypointPath {
				continue
			}
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			const debounceDuration = 100 * time.Millisecond
			startTimerC = time.After(debounceDuration)

		case <-startTimerC:
			stopCurrentRun()
			stopCurrentRun, err = startRunScript(ctx, sessionDir, entrypoint)
			if err != nil {
				return fmt.Errorf("printing session results: %s", err)
			}

		case <-ctx.Done():
			return fmt.Errorf("printing session results: %s", ctx.Err())

		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("printing session results: watcher error channel closed")
			}
			return fmt.Errorf("printing session results: watching session directory: %s", err)
		}
	}
}

//go:embed templates/*
var builtinTemplatesFS embed.FS

const runScriptFilename = "run.sh"

// template represents a playground template.
//
// A playground template is a directory containing the files used to run a playground session. The
// contents of the template are copied into the session directory.
//
// A template contains:
//   - Exactly one "main.*" file, the entrypoint opened when the session starts
//   - A "run.sh" file, the run script executed as "bash run.sh $entrypoint"
//   - Any other files needed to run the session
type template struct {
	Name       string // Template name; matches the template directory name.
	FS         fs.FS  // File system containing the template's files.
	Entrypoint string // Entrypoint filename, such as "main.go".
}

var errTemplateNotFound = errors.New("not found")

// templateInvalidError records an invalid template and the reason it's invalid.
//
// Path is empty for built-in templates.
type templateInvalidError struct {
	Name   string
	Path   string
	Reason string
}

func (e *templateInvalidError) Error() string {
	return e.Reason
}

func newTemplateInvalidErrorf(name string, path string, reason string, a ...any) *templateInvalidError {
	return &templateInvalidError{
		Name:   name,
		Path:   path,
		Reason: fmt.Sprintf(reason, a...),
	}
}

// loadTemplate loads a template by searching the user's templates directory first, then the set of
// built-in templates.
// If the template does not exist, the returned error wraps [errTemplateNotFound].
// If the loaded template is invalid, the returned error wraps [*templateInvalidError].
func loadTemplate(homeDir string, name string) (template, error) {
	userTemplatePath := filepath.Join(homeDir, pgDir, "templates", name)
	var templateFS fs.FS
	if ok, err := fileExists(userTemplatePath); err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	} else if ok {
		templateFS = os.DirFS(userTemplatePath)
	} else if !ok {
		templateFS, err = fs.Sub(builtinTemplatesFS, filepath.Join("templates", name))
		if err != nil {
			return template{}, fmt.Errorf("loading template %q: %s", name, err)
		}
		// [fs.Sub] doesn't check whether the dir exists, so we need to explicity check.
		if ok, err := fileExistsFS(templateFS, "."); err != nil {
			return template{}, fmt.Errorf("loading template %q: %s", name, err)
		} else if !ok {
			return template{}, fmt.Errorf("loading template %q: %w", name, errTemplateNotFound)
		}
	}

	const entrypointPattern = "main.*"
	entrypoints, err := fs.Glob(templateFS, entrypointPattern)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: identifying entrypoint: %s", name, err)
	}
	var entrypoint string
	switch len(entrypoints) {
	case 1:
		entrypoint = entrypoints[0]
	case 0:
		templateInvalidErr := newTemplateInvalidErrorf(name, "", "entrypoint (%q file) is missing", entrypointPattern)
		return template{}, fmt.Errorf("loading template %q: %w", name, templateInvalidErr)
	default:
		quotedEntrypoints := make([]string, len(entrypoints))
		for i, entrypoint := range entrypoints {
			quotedEntrypoints[i] = strconv.Quote(entrypoint)
		}
		templateInvalidErr := newTemplateInvalidErrorf(name, "", "multiple entrypoints: %s", strings.Join(quotedEntrypoints, ", "))
		return template{}, fmt.Errorf("loading template %q: %w", name, templateInvalidErr)
	}

	if ok, err := fileExistsFS(templateFS, runScriptFilename); err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	} else if !ok {
		templateInvalidErr := newTemplateInvalidErrorf(name, "", "run script (%q file) is missing", runScriptFilename)
		return template{}, fmt.Errorf("loading template %q: %w", name, templateInvalidErr)
	}

	return template{
		Name:       name,
		FS:         templateFS,
		Entrypoint: entrypoint,
	}, nil
}

// setupSessionDir creates the directory for a session and copies the template files into it.
func setupSessionDir(dir string, templateFS fs.FS) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}
	if err := os.CopyFS(dir, templateFS); err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}
	return nil
}

// runInNewTmuxPane splits the current tmux pane vertically and runs a command in the new pane,
// leaving the current pane selected.
// The returned function closes the new pane.
func runInNewTmuxPane(ctx context.Context, cmd string) (func() error, error) {
	paneID, ok := os.LookupEnv("TMUX_PANE")
	if !ok {
		return nil, fmt.Errorf("running %q in new tmux pane: not currently in a tmux session", cmd)
	}

	tmuxCmd := exec.CommandContext(ctx, "tmux", "split-window", "-t", paneID, "-d", "-P", "-F", "#{pane_id}", cmd)
	output, err := tmuxCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running %q in new tmux pane: splitting tmux window: %s", cmd, cmdErrMsg(err))
	}
	newPaneID := strings.TrimSpace(string(output))

	return func() error {
		// We don't propagate the context to this command since we want it to run even if the
		// context gets cancelled.
		cmd := exec.Command("tmux", "kill-pane", "-t", newPaneID)
		if _, err = cmd.Output(); err != nil {
			return fmt.Errorf("killing tmux pane %q: %s", newPaneID, cmdErrMsg(err))
		}
		return nil
	}, nil
}

// startRunScript starts execution of a session's run script and returns a function to stop it.
// The run script is executed as "bash run.sh $entrypoint" from the session directory.
// The returned function blocks until execution has been stopped and is safe to be called after
// execution has stopped.
func startRunScript(ctx context.Context, sessionDir string, entrypoint string) (stop func(), err error) {
	cmdCtx, cancelCmdCtx := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancelCmdCtx()
		}
	}()

	cmd := exec.CommandContext(cmdCtx, "bash", runScriptFilename, entrypoint)
	cmd.Dir = sessionDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Run bash in a process group so that we can kill it and any child processes spawned by run.sh.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	fmt.Print(ansiClearScreen)
	fmt.Print(ansiMoveCursorHome)
	styledPrintf(ansiBoldGreen, "Executing %q\n\n", cmd.String())
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("executing %q in %q: %s", entrypoint, sessionDir, err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)
		exitStatus := 0
		exitStatusStyle := ansiBoldGreen
		if err := cmd.Wait(); err != nil {
			exitErr, ok := errors.AsType[*exec.ExitError](err)
			if !ok {
				return
			}
			exitStatus = exitErr.ExitCode()
			exitStatusStyle = ansiBoldRed
		}
		styledPrintf(exitStatusStyle, "\nExited with status %d\n", exitStatus)
	}()

	return func() {
		cancelCmdCtx()
		<-done
	}, nil
}

type style string

const (
	ansiClearScreen    style = "\x1b[2J"
	ansiMoveCursorHome style = "\x1b[H"
	ansiBoldGreen      style = "\x1b[1;32m"
	ansiBoldRed        style = "\x1b[1;31m"
	ansiReset          style = "\x1b[0m"
)

// styledPrintf prints text styled using the given ANSI escape sequence(s).
func styledPrintf(style style, format string, a ...any) {
	fmt.Printf("%s%s%s", style, fmt.Sprintf(format, a...), ansiReset)
}

// cmdErrMsg returns the appropriate error message for an [error] returned by [exec.Cmd.Output].
// If possible, the stderr output is extracted from the error. Otherwise, the value of [error.Error]
// is returned.
func cmdErrMsg(err error) string {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && len(exitErr.Stderr) > 0 {
		return string(bytes.TrimSpace(exitErr.Stderr))
	}
	return err.Error()
}

func fileExists(name string) (bool, error) {
	_, err := os.Stat(name)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func fileExistsFS(f fs.FS, name string) (bool, error) {
	_, err := fs.Stat(f, name)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}
