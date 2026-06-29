package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(cli())
}

// cliError records an incorrect usage of the CLI.
type cliError string

func (e cliError) Error() string {
	return string(e)
}

func newCliErrorf(format string, a ...any) cliError {
	return cliError(fmt.Sprintf(format, a...))
}

// cli parses the CLI args, runs pg, reports any errors, and returns the process exit code.
//
// It returns 0 for success or help, 2 for incorrect CLI usage, and 1 for other errors.
func cli() int {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: pg [options] <template-name>")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
	}
	editor := flag.String("editor", "", fmt.Sprintf("Editor to open; falls back to $%s and then %q.", editorEnvVar, defaultEditor))
	printHelp := flag.Bool("help", false, "Print this message.")

	flag.Parse()

	if *printHelp {
		flag.Usage()
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := pg(ctx, flag.Args(), *editor); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		if _, ok := errors.AsType[cliError](err); ok {
			flag.Usage()
			return 2
		}
		return 1
	}

	return 0
}

const (
	editorEnvVar = "EDITOR"
	defaultEditor = "vi"
)

// TODO: fill in
func pg(ctx context.Context, args []string, editor string) (err error) {
	if len(args) < 1 {
		return newCliErrorf("template name not provided")
	}
	if len(args) > 1 {
		return newCliErrorf("unexpected arguments: %s", strings.Join(args, ", "))
	}

	// 1. Load template
	// 2. Create session directory
	// 3. CD to session directory
	// 4. Split pane
	// 5. Start nvim in top pane
	// 6. Start pg in runner mode in bottom pane

	templateName := args[0]
	template, err := loadTemplate(templateName)
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
		return err
	}

	sessionDir, err := anonSessionDir(template.Name)
	if err != nil {
		return err
	}
	if err := setupSessionDir(sessionDir, template.FS); err != nil {
		return err
	}
	if err := os.Chdir(sessionDir); err != nil {
		return err
	}

	closeResultsPane, err := runInNewTmuxPane(ctx, "watch date")
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := closeResultsPane(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing results pane: %s", closeErr))
		}
	}()

	if editor == "" {
		editor = os.Getenv(editorEnvVar)
	}
	if editor == "" {
		editor = defaultEditor
	}
	editorCmd := exec.CommandContext(ctx, editor, template.Entrypoint)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("running %q: %s", editor, cmdErrMsg(err))
	}

	return nil
}

//go:embed templates/*
var builtinTemplates embed.FS

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

// loadTemplate loads a template from the set of built-in templates.
// If the template does not exist, the returned error wraps [errTemplateNotFound].
// If the loaded template is invalid, the returned error wraps [*templateInvalidError].
func loadTemplate(name string) (template, error) {
	templateFS, err := fs.Sub(builtinTemplates, filepath.Join("templates", name))
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	}
	// [fs.Sub] doesn't check whether the dir exists, so we need to explicity check.
	if _, err := fs.Stat(templateFS, "."); err != nil {
		if os.IsNotExist(err) {
			return template{}, fmt.Errorf("loading template %q: %w", name, errTemplateNotFound)
		}
		return template{}, err
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

	const runScriptFilename = "run.sh"
	if _, err := fs.Stat(templateFS, runScriptFilename); err != nil {
		if os.IsNotExist(err) {
			templateInvalidErr := newTemplateInvalidErrorf(name, "", "run script (%q file) is missing", runScriptFilename)
			return template{}, fmt.Errorf("loading template %q: %w", name, templateInvalidErr)
		}
		return template{}, err
	}

	return template{
		Name:       name,
		FS:         templateFS,
		Entrypoint: entrypoint,
	}, nil
}

// runInNewTmuxPane splits the current tmux pane vertically and runs a command in the new pane,
// leaving the current pane selected.
// The returned function closes the new pane.
func runInNewTmuxPane(ctx context.Context, cmd string) (func() error, error) {
	paneID, ok := os.LookupEnv("TMUX_PANE")
	if !ok {
		return nil, fmt.Errorf("splitting tmux window: not currently in a tmux session")
	}

	tmuxCmd := exec.CommandContext(ctx, "tmux", "split-window", "-t", paneID, "-d", "-P", "-F", "#{pane_id}", cmd)
	output, err := tmuxCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("splitting tmux window: %s", cmdErrMsg(err))
	}
	newPaneID := strings.TrimSpace(string(output))

	return func() error {
		// We don't propagate the context to this command since we want to make sure that it can run
		// even if the context has been cancelled.
		cmd := exec.Command("tmux", "kill-pane", "-t", newPaneID)
		if _, err = cmd.Output(); err != nil {
			return fmt.Errorf("killing tmux pane %q: %s", newPaneID, cmdErrMsg(err))
		}
		return nil
	}, nil
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

func anonSessionDir(templateName string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("generating anonymous session directory name: %s", err)
	}
	sessionName := fmt.Sprintf("%s-%s", templateName, time.Now().Format(fmt.Sprintf("%s-%s", time.DateOnly, time.TimeOnly)))
	return filepath.Join(homeDir, ".pg", "sessions", "anonymous", sessionName), nil
}

// setupSessionDir sets up the directory for a session by creating it and copying the template files
// to the directory.
func setupSessionDir(dir string, fs fs.FS) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("setting up anonymous session directory: %s", err)
	}
	if err := os.CopyFS(dir, fs); err != nil {
		return fmt.Errorf("setting up anonymous session directory: %s", err)
	}
	return nil
}
