package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

type runSessionOptions struct {
	TemplateName     string // Template name.
	SessionName      string // Session name; if empty, session is anonymous with generated name.
	Vertical         bool   // Whether to split the window vertically
	ResultsPaneSize  string // Number of lines, or a percentage if followed by %
	Editor           string // Editor to open.
	SessionsDir      string // Absolute path to sessions directory.
	UserTemplatesDir string // Absolute path to user's templates directory.
	PgPath           string // Absolute path to pg executable; used to start the results command.
}

// runSession runs a session using the given template.
// It sets up the session directory, starts the results command in a new tmux pane, and opens the
// template's entrypoint in the given editor.
// If the session is named, then its directory is set up in the sessions directory, named after the
// session. Otherwise, the session is anonymous and its directory is set up in a temporary directory
// with a generated name.
func runSession(ctx context.Context, opts runSessionOptions) (err error) {
	if err := validateDirNameSafe(opts.TemplateName); err != nil {
		return fmt.Errorf("template name %q is invalid: %s", opts.TemplateName, err)
	}
	if opts.SessionName != "" {
		if err := validateSessionName(opts.SessionName); err != nil {
			return fmt.Errorf("session name %q is invalid: %s", opts.SessionName, err)
		}
	}
	if !regexp.MustCompile(`\d+%?`).MatchString(opts.ResultsPaneSize) {
		return fmt.Errorf("results pane size must be a number, optionally followed by '%%'")
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

	var sessionDir string
	if opts.SessionName != "" {
		sessionDir, err = ensureNamedSessionDir(opts.SessionName, opts.SessionsDir, template)
	} else {
		sessionDir, err = setupAnonSessionDir(template)
		defer os.RemoveAll(sessionDir) // nolint:errcheck // It's fine if this fails as it's a temporary directory
	}
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}

	// Ensure that commands which rely on the current directory (like tmux split-window -c
	// "#{pane_current_path}") work as expected.
	if err := os.Chdir(sessionDir); err != nil {
		return fmt.Errorf("running session: changing to session directory: %s", err)
	}

	// See [resultsCLI] for the __results command
	resultsPaneCmd := fmt.Sprintf("%s __results %s %s", shellQuote(opts.PgPath), shellQuote(sessionDir), shellQuote(template.Entrypoint))
	resultsPaneID, err := tmuxSplitPane(ctx, resultsPaneCmd, opts.Vertical, opts.ResultsPaneSize)
	if err != nil {
		if errors.Is(err, errTmuxNotFound) {
			return fmt.Errorf("tmux not found in $PATH")
		}
		return fmt.Errorf("running session: creating results pane: %s", err)
	}
	defer func() {
		// We don't propagate the context through this call since we want it to run even if our
		// context got cancelled.
		if closeErr := killTmuxPane(context.Background(), resultsPaneID); closeErr != nil && !errors.Is(closeErr, errTmuxPaneNotFound) {
			err = errors.Join(err, fmt.Errorf("running session: closing results pane: %s", closeErr))
		}
	}()
	if err := disableTmuxPaneInput(ctx, resultsPaneID); err != nil {
		return fmt.Errorf("running session: configuring results pane: %s", err)
	}
	// Keeps the results pane open if the results command exits with a non-zero code
	if err := setTmuxPaneOption(ctx, resultsPaneID, "remain-on-exit", "failed"); err != nil {
		return fmt.Errorf("running session: configuring results pane: %s", err)
	}
	resultsExitMsg := fmt.Sprintf(`playground: results command has exited unexpectedly. Restart it with "tmux respawn-pane -t %s".`, resultsPaneID)
	if err := setTmuxPaneOption(ctx, resultsPaneID, "remain-on-exit-format", resultsExitMsg); err != nil {
		return fmt.Errorf("running session: configuring results pane: %s", err)
	}

	// FIXME: This fails when editor contains args like "nvim -n".
	editorCmd := exec.CommandContext(ctx, opts.Editor, template.Entrypoint)
	editorCmd.Dir = sessionDir
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("editor %q not found in $PATH", opts.Editor)
		}
		// TODO: make this error message more user friendly
		return fmt.Errorf("running session with editor %q: %s", opts.Editor, cmdErrMsg(err))
	}

	return nil
}

// validateDirNameSafe reports whether name is safe to use as a single directory path element.
func validateDirNameSafe(name string) error {
	if name == "" {
		return fmt.Errorf("cannot be empty")
	}
	if strings.ContainsRune(name, os.PathSeparator) {
		return fmt.Errorf("cannot contain path separator %q", os.PathSeparator)
	}
	if name == "." || name == ".." {
		return fmt.Errorf(`cannot be "." or ".."`)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("cannot contain unicode control characters (%q)", r)
		}
	}
	return nil
}

func validateSessionName(name string) error {
	if strings.HasPrefix(name, namedSessionStagingDirPrefix) {
		return fmt.Errorf("cannot use reserved prefix %q", namedSessionStagingDirPrefix)
	}
	return validateDirNameSafe(name)
}

// shellQuote returns arg quoted so that it can be used as a literal shell command argument.
func shellQuote(arg string) string {
	return fmt.Sprintf("'%s'", strings.ReplaceAll(arg, "'", `'\''`))
}

const namedSessionStagingDirPrefix = ".pg-tmp"

// ensureNamedSessionDir sets up a named session directory in the sessions directory if it doesn't
// already exist and returns the directory name.
func ensureNamedSessionDir(sessionName string, sessionsDir string, template template) (string, error) {
	templateSessionsDir := templateSessionsDir(sessionsDir, template.Name)
	sessionDir := filepath.Join(templateSessionsDir, sessionName)
	if ok, err := fileExists(sessionDir); err != nil {
		return "", fmt.Errorf("setting up session directory: %s", err)
	} else if ok {
		return sessionDir, nil
	}

	// We set up the session in a temporary staging directory first and then move it into place.
	// This way, if the set up fails, we're not left with a partially set up directory which would
	// get used by future sessions.
	sessionDirParent := filepath.Dir(sessionDir)
	if err := os.MkdirAll(sessionDirParent, 0755); err != nil {
		return "", fmt.Errorf("setting up session directory: %s", err)
	}
	stagingDirNamePattern := fmt.Sprintf("%s-*", namedSessionStagingDirPrefix)
	stagingDir, err := os.MkdirTemp(sessionDirParent, stagingDirNamePattern)
	if err != nil {
		return "", fmt.Errorf("setting up session directory: creating staging directory: %s", err)
	}
	defer os.RemoveAll(stagingDir) // nolint:errcheck // It's fine if this fails as it's a hidden directory
	if err := setupSessionDir(stagingDir, template); err != nil {
		return "", err
	}
	if err := os.Rename(stagingDir, sessionDir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// If the directory already exists, there must have been a racing process trying to
			// create the same session. We can just behave as if the directory had already existed
			// when we checked and ignore the error.
			return sessionDir, nil
		}
		return "", fmt.Errorf("setting up session directory: moving into place: %s", err)
	}

	return sessionDir, nil
}

// templateSessionsDir returns the directory where named sessions for the given template are stored.
func templateSessionsDir(sessionsDir string, templateName string) string {
	return filepath.Join(sessionsDir, templateName)
}

// listSessionNames returns the names of all sessions using the given template in alphabetical
// order.
func listSessionNames(templateName string, sessionsDir string) ([]string, error) {
	templateSessionsDir := templateSessionsDir(sessionsDir, templateName)
	sessionDirs, err := os.ReadDir(templateSessionsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("listing sessions names: reading %q template sessions directory: %s", templateName, err)
	}
	var names []string
	for _, dirEntry := range sessionDirs {
		if strings.HasPrefix(dirEntry.Name(), namedSessionStagingDirPrefix) {
			continue
		}
		names = append(names, dirEntry.Name())
	}
	return names, nil
}

// setupAnonSessionDir sets up an anonymous session directory in a temporary directory and returns
// the directory name.
func setupAnonSessionDir(template template) (string, error) {
	sessionDirParent := filepath.Join(os.TempDir(), subdirName)
	if err := os.MkdirAll(sessionDirParent, 0755); err != nil {
		return "", fmt.Errorf("setting up session directory: creating temporary directory: %s", err)
	}
	namePattern := fmt.Sprintf("%s-*", template.Name)
	sessionDir, err := os.MkdirTemp(sessionDirParent, namePattern)
	if err != nil {
		return "", fmt.Errorf("setting up session directory: %s", err)
	}
	if err := setupSessionDir(sessionDir, template); err != nil {
		return "", err
	}
	return sessionDir, nil
}

// setupSessionDir copies a template's files into a session directory.
func setupSessionDir(dir string, template template) error {
	if err := os.CopyFS(dir, template.FS); err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}
	// From the [embed] docs: "Patterns must not match files outside the package's module, such as
	// ‘.git/*’, symbolic links, 'vendor/', or any directories containing go.mod (these are separate
	// modules).". This prevents us from including a go.mod file in the built-in Go template
	// directory so we just write one to the session directory ourselves.
	if template.Name == "go" && template.IsBuiltin {
		path := filepath.Join(dir, "go.mod")
		data := []byte("module playground\n")
		if err := os.WriteFile(path, data, 0o666); err != nil {
			return fmt.Errorf("setting up session directory: writing built-in go template go.mod: %s", err)
		}
	}
	return nil
}

var errTmuxNotFound = fmt.Errorf("tmux not found in $PATH")

// tmuxSplitPane splits the current tmux pane and runs a command in the new pane, leaving the
// current pane selected.
// size is passed as the -l option to tmux split-window.
// The ID of the new pane is returned.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
func tmuxSplitPane(ctx context.Context, cmd string, vertical bool, size string) (string, error) {
	paneID, ok := os.LookupEnv("TMUX_PANE")
	if !ok {
		return "", fmt.Errorf("running command in new tmux pane: not currently in a tmux session")
	}
	// The split-window flags direction flags aren't the way around you'd expect. -v means the panes
	// will stacked vertically (what you'd expect is horizontal) and -h means they'll be
	// side-by=side (what you'd expect)
	directionFlag := "-v"
	if vertical {
		directionFlag = "-h"
	}
	newPaneID, err := tmux(ctx, "split-window", "-t", paneID, "-d", directionFlag, "-l", size, "-P", "-F", "#{pane_id}", cmd)
	if err != nil {
		return "", fmt.Errorf("running command in new tmux pane: %w", err)
	}
	return newPaneID, nil
}

var errTmuxPaneNotFound = fmt.Errorf("tmux pane not found")

// killTmuxPane kills the given tmux pane.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
// If the pane is not found, the returned error wraps [errTmuxPaneNotFound].
func killTmuxPane(ctx context.Context, id string) error {
	_, err := tmux(ctx, "kill-pane", "-t", id)
	if err != nil {
		if strings.Contains(err.Error(), "can't find pane") {
			return fmt.Errorf("killing tmux pane %q: %w", id, errTmuxPaneNotFound)
		}
		return fmt.Errorf("killing tmux pane: %w", err)
	}
	return err
}

// disableTmuxPaneInput disables input to the given tmux pane.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
func disableTmuxPaneInput(ctx context.Context, id string) error {
	_, err := tmux(ctx, "select-pane", "-t", id, "-d")
	if err != nil {
		return fmt.Errorf("disabling tmux pane input: %w", err)
	}
	return nil
}

// setTmuxPaneOption sets an option in a tmux pane.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
func setTmuxPaneOption(ctx context.Context, id string, option string, value string) error {
	_, err := tmux(ctx, "set-option", "-p", "-t", id, option, value)
	if err != nil {
		return fmt.Errorf("setting tmux pane option: %w", err)
	}
	return nil
}

// tmux runs a tmux command and returns its output.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
func tmux(ctx context.Context, args ...string) (string, error) {
	tmuxCmd := exec.CommandContext(ctx, "tmux", args...)
	output, err := tmuxCmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("executing %s: %w", tmuxCmd, errTmuxNotFound)
		}
		return "", fmt.Errorf("executing %q: %s", tmuxCmd, cmdErrMsg(err))
	}
	return strings.TrimSpace(string(output)), nil
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
	return fileExistsResult(err)
}

func fileExistsFS(f fs.FS, name string) (bool, error) {
	_, err := fs.Stat(f, name)
	return fileExistsResult(err)
}

func fileExistsResult(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return true, err
}
