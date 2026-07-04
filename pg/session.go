package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type runSessionOptions struct {
	TemplateName     string // Template name.
	SessionName      string // Session name; if empty, session is anonymous with generated name.
	Editor           string // Editor to open.
	SessionsDir      string // Absolute path to sessions directory.
	UserTemplatesDir string // Absolute path to user's templates directory.
	PgPath           string // Absolute path to pg executable; used to start the results command.
}

// runSession runs a session using the given template.
// It sets up the session directory (creates it and copies in the template's files) if it doesn't
// already exist, starts the results command in a new tmux pane, and opens the template's entrypoint
// in the given editor.
// If the session is named, then its directory is set up in the sessions directory, named after the
// session. Otherwise, the session is anonymous and its directory is set up in a temporary directory
// with a generated name.
func runSession(ctx context.Context, opts runSessionOptions) (err error) {
	if strings.ContainsRune(opts.TemplateName, os.PathSeparator) {
		return fmt.Errorf("template name %q is invalid: must not contain %q", opts.TemplateName, os.PathSeparator)
	}
	if strings.ContainsRune(opts.SessionName, os.PathSeparator) {
		return fmt.Errorf("session name %q is invalid: must not contain %q", opts.SessionName, os.PathSeparator)
	}

	template, err := loadTemplate(opts.TemplateName, opts.UserTemplatesDir)
	if err != nil {
		if errors.Is(err, errTemplateNotFound) {
			return fmt.Errorf("template %q not found", opts.TemplateName)
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

	sessionDir := filepath.Join(opts.SessionsDir, opts.TemplateName, opts.SessionName)
	if opts.SessionName == "" {
		sessionName := fmt.Sprintf("%s-%s", opts.TemplateName, time.Now().Format(fmt.Sprintf("%s-%s", time.DateOnly, time.TimeOnly)))
		sessionDir = filepath.Join(os.TempDir(), "playground", sessionName)
		// We can ignore this error since the temp directory will typically be cleared out by the OS
		// anyway.
		defer os.RemoveAll(sessionDir) // nolint:errcheck
	}
	if ok, err := fileExists(sessionDir); err != nil {
		return fmt.Errorf("running session: %s", err)
	} else if !ok {
		if err := setupSessionDir(sessionDir, template); err != nil {
			return fmt.Errorf("running session: %s", err)
		}
	}

	resultsCmd := fmt.Sprintf("%q %s %q %q", opts.PgPath, resultsCmd, sessionDir, template.Entrypoint)
	closeResultsPane, err := runInNewTmuxPane(ctx, resultsCmd)
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}
	defer func() {
		if closeErr := closeResultsPane(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("running session: closing results pane: %s", closeErr))
		}
	}()

	editorCmd := exec.CommandContext(ctx, opts.Editor, template.Entrypoint)
	editorCmd.Dir = sessionDir
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("running session with editor %q: %s", opts.Editor, cmdErrMsg(err))
	}

	return nil
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
	IsBuiltin  bool   // Whether the template is built-in.
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
func loadTemplate(name string, userTemplatesDir string) (template, error) {
	userTemplatePath := filepath.Join(userTemplatesDir, name)
	var templateFS fs.FS
	isBuiltin := false
	if ok, err := fileExists(userTemplatePath); err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	} else if ok {
		templateFS = os.DirFS(userTemplatePath)
	} else if !ok {
		templateFS, err = fs.Sub(builtinTemplatesFS, filepath.Join("templates", name))
		if err != nil {
			return template{}, fmt.Errorf("loading template %q: %s", name, err)
		}
		isBuiltin = true
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
		IsBuiltin:  isBuiltin,
		FS:         templateFS,
		Entrypoint: entrypoint,
	}, nil
}

// setupSessionDir creates the directory for a session and copies the template's files into it.
func setupSessionDir(dir string, template template) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}
	if err := os.CopyFS(dir, template.FS); err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}
	// From the [embed] docs: "Patterns must not match files outside the package's module, such as
	// ‘.git/*’, symbolic links, 'vendor/', or any directories containing go.mod (these are separate
	// modules).". This prevents us from including a go.mod file in the built-in Go template.
	// To work around this, we just write one ourselves at this point.
	if template.Name == "go" && template.IsBuiltin {
		path := filepath.Join(dir, "go.mod")
		data := []byte("module playground\n")
		if err := os.WriteFile(path, data, 0o666); err != nil {
			return fmt.Errorf("setting up session directory: writing built-in go template go.mod: %s", err)
		}
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
