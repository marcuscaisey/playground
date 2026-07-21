package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type runSessionOptions struct {
	TemplateName         string // Template name
	SessionName          string // Session name; if empty, session is anonymous with generated name
	Vertical             bool   // Whether to split the window vertically
	ResultsPaneSize      string // Number of lines, or a percentage if followed by %
	Editor               string // Shell command to open editor
	SessionsDir          string // Path to sessions directory
	SessionsDirIsDefault bool   // Whether the sessions directory is the default; used when printing example commands
	UserTemplatesDir     string // Absolute path to user's templates directory
	PgPath               string // Absolute path to pg executable; used to start the results command
}

// Extra arguments passed to each editor when a new session is started.
// {start_line} is replaced with the line where the template's entrypoint should be opened.
var editorNewSessionExtraArgs = map[string]string{
	"nvim":  "+{start_line} +normal$",
	"vim":   "+{start_line} +normal$",
	"vi":    "+{start_line} +normal$",
	"emacs": "+{start_line}:999",
	"hx":    "+{start_line}",
	"kak":   "+{start_line}:999",
	"nano":  "+{start_line}",
	"pico":  "+{start_line}",
}

// runSession runs a session using the given template.
// The template's entrypoint is opened in the given editor and the results are displayed in a
// results pane below or to the side.
//
// Named sessions are run in a persistent directory under the sessions directory.
// Anonymous sessions are run in a temporary directory and, after the editor exits successfully, may
// be saved as a named session.
func runSession(ctx context.Context, opts runSessionOptions) (err error) {
	if err := validateDirNameSafe(opts.TemplateName); err != nil {
		return fmt.Errorf("template name %q is invalid: %s", opts.TemplateName, err)
	}
	if opts.SessionName != "" {
		if err := validateSessionName(opts.SessionName); err != nil {
			return err
		}
	}
	if !regexp.MustCompile(`\d+%?`).MatchString(opts.ResultsPaneSize) {
		return fmt.Errorf("results pane size %q is invalid: must be a number, optionally followed by '%%'", opts.ResultsPaneSize)
	}

	template, err := loadTemplate(opts.TemplateName, opts.UserTemplatesDir)
	if err != nil {
		if errors.Is(err, errTemplateNotFound) {
			return fmt.Errorf("template %q not found", opts.TemplateName)
		}
		if invalidErr, ok := errors.AsType[*templateInvalidError](err); ok {
			return fmt.Errorf("template %q (%s) is invalid: %s", invalidErr.Name, invalidErr.Source, invalidErr.Reason)
		}
		return fmt.Errorf("running session: %s", err)
	}

	absSessionsDir, err := filepath.Abs(opts.SessionsDir)
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}
	var sessionDir string
	sessionIsNew := true
	if opts.SessionName != "" {
		sessionDir, sessionIsNew, err = ensureNamedSessionDir(opts.SessionName, absSessionsDir, template)
	} else {
		sessionDir, err = setupAnonSessionDir(template)
		defer func() {
			// Keep the directory around if there's an error to allow the user to recover the
			// contents of the session
			if err == nil {
				_ = os.RemoveAll(sessionDir) // It's fine if this fails as it's a temporary directory
			}
		}()
	}
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}

	// Best effort as the last opened time is only depended upon during tab completion
	_ = updateSessionLastOpened(sessionDir)

	// Ensure that commands which rely on the current directory (like tmux split-window -c
	// "#{pane_current_path}") work as expected.
	if err := os.Chdir(sessionDir); err != nil {
		return fmt.Errorf("running session: changing to session directory: %s", err)
	}

	// See [resultsCLI] for the results subcommand
	resultsPaneCmd := fmt.Sprintf("%s %s %s %s", shellQuote(opts.PgPath), resultsSubcmd, shellQuote(sessionDir), shellQuote(template.Entrypoint))
	resultsPaneID, err := tmuxSplitPane(ctx, resultsPaneCmd, opts.Vertical, opts.ResultsPaneSize)
	if err != nil {
		if errors.Is(err, errTmuxNotFound) {
			return fmt.Errorf("tmux not found in $PATH")
		}
		if errors.Is(err, errNotInTmuxSession) {
			return fmt.Errorf("not in a tmux session")
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
	// Protect the user from accidentally killing the process with Ctrl-C
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

	editor := strings.TrimSpace(opts.Editor)
	editorCmdName, editorArgsString, _ := strings.Cut(editor, " ")
	editorArgs := strings.Fields(editorArgsString)
	if sessionIsNew {
		editorName := filepath.Base(editorCmdName) // editorCmdName could be an absolute path
		if format := editorNewSessionExtraArgs[editorName]; format != "" {
			extraArgsString := strings.ReplaceAll(format, "{start_line}", strconv.Itoa(template.EntrypointStartLine))
			extraArgs := strings.Fields(extraArgsString)
			editorArgs = append(editorArgs, extraArgs...)
		}
	}
	editorArgs = append(editorArgs, template.Entrypoint)
	editorCmd := cmdWithStdio(ctx, editorCmdName, editorArgs...)
	editorCmd.Dir = sessionDir
	if err := editorCmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("editor %q not found in $PATH", editorCmdName)
		}
		// This is expected behaviour when the user doesn't want to be prompted to save their
		// anonymous session. Also, if there is an actual error, it should be logged out by the
		// editor anyway.
		return nil
	}

	if opts.SessionName != "" {
		return nil
	}
	// We still want to save the user's session if this fails and we're going to try killing the
	// pane again when we exit this function anyway (where the error will also be recorded).
	_ = killTmuxPane(ctx, resultsPaneID)

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("Enter a name to save this session (or Ctrl-D to abort): ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("saving session: reading line from stdin: %s", err)
			}
			fmt.Println()
			fmt.Println("Exit editor with a non-zero exit code to skip this prompt")
			return nil
		}
		sessionName := scanner.Text()
		if sessionName == "" {
			continue
		}
		if err := validateSessionName(sessionName); err != nil {
			fmt.Println(err)
			continue
		}

		newSessionDir := namedSessionDir(sessionName, absSessionsDir, template.Name)
		if err := os.MkdirAll(filepath.Dir(newSessionDir), 0755); err != nil {
			return fmt.Errorf("saving session: %s", err)
		}
		if err := os.Rename(sessionDir, newSessionDir); err != nil {
			if errors.Is(err, fs.ErrExist) {
				fmt.Printf("A %q session named %q already exists:\n", template.Name, sessionName)
				fmt.Printf("  %s\n", newSessionDir)
				continue
			}
			return fmt.Errorf("saving session: %s", err)
		}

		fmt.Printf("Saved %q session %q to:\n", template.Name, sessionName)
		fmt.Printf("  %s\n", newSessionDir)
		fmt.Printf("To resume, run:\n")
		sessionsDirFlag := ""
		if !opts.SessionsDirIsDefault {
			sessionsDirFlag = fmt.Sprintf("-sessions-dir %s ", shellQuote(opts.SessionsDir))
		}
		fmt.Printf("  pg %s%s %s\n", sessionsDirFlag, shellQuote(template.Name), shellQuote(sessionName))
		break
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

// validateSessionName validates that a session name is safe to use as a single directory path
// element and doesn't use the prefix .pg-tmp reserved for temporary staging directories.
func validateSessionName(name string) error {
	if strings.HasPrefix(name, namedSessionStagingDirPrefix) {
		return fmt.Errorf("session name %q is invalid: cannot use reserved prefix %q", name, namedSessionStagingDirPrefix)
	}
	if err := validateDirNameSafe(name); err != nil {
		return fmt.Errorf("session name %q is invalid: %s", name, err)
	}
	return nil
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

const namedSessionStagingDirPrefix = ".pg-tmp"

// ensureNamedSessionDir sets up a named session directory in the sessions directory if it doesn't
// already exist. It returns the directory name and whether it was created by this call.
func ensureNamedSessionDir(sessionName string, sessionsDir string, template template) (sessionDir string, created bool, err error) {
	sessionDir = namedSessionDir(sessionName, sessionsDir, template.Name)
	if ok, err := fileExists(sessionDir); err != nil {
		return "", false, fmt.Errorf("setting up session directory: %s", err)
	} else if ok {
		return sessionDir, false, nil
	}

	// We set up the session in a temporary staging directory first and then move it into place.
	// This way, if the set up fails, we're not left with a partially set up directory which would
	// get used by future sessions.
	sessionDirParent := filepath.Dir(sessionDir)
	if err := os.MkdirAll(sessionDirParent, 0755); err != nil {
		return "", false, fmt.Errorf("setting up session directory: %s", err)
	}
	stagingDirNamePattern := fmt.Sprintf("%s-*", namedSessionStagingDirPrefix)
	stagingDir, err := os.MkdirTemp(sessionDirParent, stagingDirNamePattern)
	if err != nil {
		return "", false, fmt.Errorf("setting up session directory: creating staging directory: %s", err)
	}
	defer os.RemoveAll(stagingDir) // nolint:errcheck // It's fine if this fails as it's a hidden directory
	if err := template.SetupSessionDir(stagingDir); err != nil {
		return "", false, err
	}
	if err := os.Rename(stagingDir, sessionDir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// If the directory already exists, there must have been a racing process trying to
			// create the same session. We can just behave as if the directory had already existed
			// when we checked and ignore the error.
			return sessionDir, false, nil
		}
		return "", false, fmt.Errorf("setting up session directory: moving into place: %s", err)
	}

	return sessionDir, true, nil
}

// namedSessionDir returns the directory where a named session is stored.
func namedSessionDir(sessionName string, sessionsDir string, templateName string) string {
	templateSessionsDir := templateSessionsDir(sessionsDir, templateName)
	return filepath.Join(templateSessionsDir, sessionName)
}

// templateSessionsDir returns the directory where named sessions for the given template are stored.
func templateSessionsDir(sessionsDir string, templateName string) string {
	return filepath.Join(sessionsDir, templateName)
}

// sessionInfo describes an available session.
type sessionInfo struct {
	Name       string    // Session name
	LastOpened time.Time // Last time session was opened; zero value if unknown
}

// listSessions returns all sessions using the given template.
func listSessions(templateName string, sessionsDir string) ([]sessionInfo, error) {
	templateSessionsDir := templateSessionsDir(sessionsDir, templateName)
	sessionDirs, err := os.ReadDir(templateSessionsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("listing sessions: reading %q template sessions directory: %s", templateName, err)
	}
	var sessions []sessionInfo
	for _, dir := range sessionDirs {
		name := dir.Name()
		if strings.HasPrefix(name, namedSessionStagingDirPrefix) {
			continue
		}
		var lastOpened time.Time
		lastOpenedPath := filepath.Join(templateSessionsDir, name, sessionLastOpenedFilename)
		// The file may have been removed by the user, this is fine
		if info, err := os.Stat(lastOpenedPath); err == nil {
			lastOpened = info.ModTime()
		}
		session := sessionInfo{Name: name, LastOpened: lastOpened}
		sessions = append(sessions, session)
	}
	return sessions, nil
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
	if err := template.SetupSessionDir(sessionDir); err != nil {
		return "", err
	}
	return sessionDir, nil
}

const sessionLastOpenedFilename = ".pg-last-opened"

// updateSessionLastOpened updates the session's last opened time.
// This is stored as the modification time of the .pg-last-opened file in the session directory.
func updateSessionLastOpened(sessionDir string) error {
	if err := os.WriteFile(filepath.Join(sessionDir, sessionLastOpenedFilename), nil, 0o666); err != nil {
		return fmt.Errorf("updating session last opened: %s", err)
	}
	return nil
}

// tmuxSplitPane splits the current tmux pane and runs a command in the new pane, leaving the
// current pane selected.
// size is passed as the -l option to tmux split-window.
// The ID of the new pane is returned.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
// If not in a tmux session, the returned error wraps [errNotInTmuxSession].
func tmuxSplitPane(ctx context.Context, cmd string, vertical bool, size string) (string, error) {
	// The split-window flags direction flags aren't the way around you'd expect. -v means the panes
	// will stacked vertically (what you'd expect is horizontal) and -h means they'll be
	// side-by=side (what you'd expect)
	directionFlag := "-v"
	if vertical {
		directionFlag = "-h"
	}
	paneID := os.Getenv("TMUX_PANE")
	newPaneID, err := tmux(ctx, "split-window", "-t", paneID, "-d", directionFlag, "-l", size, "-P", "-F", "#{pane_id}", cmd)
	if err != nil {
		return "", fmt.Errorf("running command in new tmux pane: %w", err)
	}
	return newPaneID, nil
}

var errTmuxPaneNotFound = fmt.Errorf("tmux pane not found")

// killTmuxPane kills the given tmux pane.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
// If not in a tmux session, the returned error wraps [errNotInTmuxSession].
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
// If not in a tmux session, the returned error wraps [errNotInTmuxSession].
func disableTmuxPaneInput(ctx context.Context, id string) error {
	_, err := tmux(ctx, "select-pane", "-t", id, "-d")
	if err != nil {
		return fmt.Errorf("disabling tmux pane input: %w", err)
	}
	return nil
}

// setTmuxPaneOption sets an option in a tmux pane.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
// If not in a tmux session, the returned error wraps [errNotInTmuxSession].
func setTmuxPaneOption(ctx context.Context, id string, option string, value string) error {
	_, err := tmux(ctx, "set-option", "-p", "-t", id, option, value)
	if err != nil {
		return fmt.Errorf("setting tmux pane option: %w", err)
	}
	return nil
}

var (
	errTmuxNotFound     = fmt.Errorf("tmux not found in $PATH")
	errNotInTmuxSession = fmt.Errorf("not in a tmux session")
)

// tmux runs a tmux command and returns its output.
// If tmux is not found, the returned error wraps [errTmuxNotFound].
// If not in a tmux session, the returned error wraps [errNotInTmuxSession].
func tmux(ctx context.Context, args ...string) (string, error) {
	tmuxCmd := exec.CommandContext(ctx, "tmux", args...)
	if _, ok := os.LookupEnv("TMUX"); !ok {
		return "", fmt.Errorf("executing %q: %w", tmuxCmd, errNotInTmuxSession)
	}
	output, err := tmuxCmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("executing %q: %w", tmuxCmd, errTmuxNotFound)
		}
		return "", fmt.Errorf("executing %q: %s", tmuxCmd, cmdErrMsg(err))
	}
	return strings.TrimSpace(string(output)), nil
}

// cmdWithStdio returns an [exec.Cmd] which uses the standard input, ouput, and error of the
// current process.
func cmdWithStdio(ctx context.Context, cmd string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, cmd, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command
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
