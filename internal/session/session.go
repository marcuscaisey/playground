// Package session provides functionality for managing playground sessions and inspecting playground
// sessions and templates.
//
// # Playground Session
//
// A playground session is an interactive terminal session where the user edits a file based on a
// template which is executed on save.
//
// More concretely, running a playground session:
//  1. Populates a directory (the session directory) with the contents of a playground template.
//  2. Opens the template's entrypoint using the user's editor in a tmux pane (the editor pane). If
//     the process is in a tmux pane, this is used; otherwise a new tmux session is started.
//  3. In another tmux pane (the results pane), split from the editor pane, the template's run
//     script is executed when either of the template's entrypoint or run script are saved.
//  4. Closing the editor ends the session.
//
// A session can either be named or anonymous. For a named session, the session directory is in the
// sessions directory (notice singular vs plural). For an anonymous session, the session directory
// is in a temporary directory. Named sessions can be resumed after they've been ended. After a
// session has ended, it can be saved as a named session.
//
// [Session] is the type which represents a playground session.
//
// # Playground Template
//
// A playground template is a directory containing the files used to run a playground session. The
// contents of the template are copied into the session directory when the session is started.
//
// A template contains:
//   - Exactly one "main.*" file, the entrypoint opened when the session starts.
//     If __CURSOR__ appears anywhere on a line, then the contents of the line (excluding leading
//     whitespace) are erased when the file is copied into the session directory and, if possible,
//     the cursor is placed on it when the entrypoint is opened for the first time. All __CURSOR__
//     appearances after the first one are ignored.
//   - A "run.sh" file, the run script executed as "bash run.sh".
//   - Any other files needed to run the session.
//
// A number of built-in templates are provided by the [templates] package. Users can use their own
// templates by adding them to the user templates directory. User templates take precedence over
// built-in templates.
package session

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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"
)

// Session represents a playground session.
type Session struct {
	Name string // Name of the session; if empty, the session is anonymous
	// Directory containing the session's files.
	// Dir is empty until its populated as a result of calling [session.Run].
	// Dir != "" implies that the directory has been created.
	Dir          string
	TemplateName string // Name of the session's template
	template     template
	sessionsDir  string // Directory where named sessions are stored
	pgPath       string
}

// InvalidTemplateNameError records an invalid template name and the reason it's invalid.
type InvalidTemplateNameError struct {
	Name   string
	Reason string
}

func (e *InvalidTemplateNameError) Error() string {
	return fmt.Sprintf("template name %q is invalid: %s", e.Name, e.Reason)
}

// New constructs a new [Session].
// If the name is invalid, the returned error wraps an [*InvalidNameError].
// If the template name is invalid, the returned error wraps an [*InvalidTemplateNameError].
// If the template is not found, the returned error wraps [ErrTemplateNotFound].
// If the template is invalid, the returned error wraps an [*InvalidTemplateError].
func New(name string, templateName string, sessionsDir string, userTemplatesDir string, pgPath string) (*Session, error) {
	if err := validateDirNameSafe(templateName); err != nil {
		return nil, fmt.Errorf("creating session: %w", &InvalidTemplateNameError{Name: templateName, Reason: err.Error()})
	}
	if name != "" {
		if err := validateSessionName(name); err != nil {
			return nil, fmt.Errorf("creating session: %w", err)
		}
	}
	template, err := loadTemplate(templateName, userTemplatesDir)
	if err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}
	absSessionsDir, err := filepath.Abs(sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("creating session: making sessions directory absolute: %s", err)
	}
	return &Session{
		Name:         name,
		TemplateName: template.Name,
		template:     template,
		sessionsDir:  absSessionsDir,
		pgPath:       pgPath,
	}, nil
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

// InvalidNameError records an invalid session name and the reason it's invalid.
type InvalidNameError struct {
	Name   string
	Reason string
}

func (e *InvalidNameError) Error() string {
	return fmt.Sprintf("session name %q is invalid: %s", e.Name, e.Reason)
}

// The returned error wraps an [*InvalidNameError].
func validateSessionName(name string) error {
	var reason string
	if strings.HasPrefix(name, namedSessionStagingDirPrefix) {
		reason = fmt.Sprintf("cannot use reserved prefix %q", namedSessionStagingDirPrefix)
	}
	if err := validateDirNameSafe(name); err != nil {
		reason = err.Error()
	}
	if reason != "" {
		return &InvalidNameError{Name: name, Reason: reason}
	}
	return nil
}

var (
	// ErrTmuxNotFound indicates that tmux was not found in $PATH.
	ErrTmuxNotFound = fmt.Errorf("tmux not found in $PATH")
	// ErrBashNotFound indicates that bash was not found in $PATH.
	ErrBashNotFound = fmt.Errorf("bash not found in $PATH")
)

// EditorNotFoundError records that an editor was not found in $PATH.
type EditorNotFoundError struct {
	Editor string
}

func (e *EditorNotFoundError) Error() string {
	return fmt.Sprintf("editor %q not found in $PATH", e.Editor)
}

// TmuxPaneSize represents the size of a tmux pane as either a number of lines/columns or a
// percentage if followed by %.
type TmuxPaneSize string

// Set implements [flag.Value].
func (s *TmuxPaneSize) Set(value string) error {
	if !regexp.MustCompile(`^\d+%?$`).MatchString(value) {
		return fmt.Errorf("must be a number, optionally followed by '%%'")
	}
	*s = TmuxPaneSize(value)
	return nil
}

func (s *TmuxPaneSize) String() string {
	return string(*s)
}

// File in the session directory which stores the sessions's last opened time as its mod time
const sessionLastOpenedMarker = ".pg-last-opened"

// Extra arguments passed to each editor when a new session is started.
// %d is replaced with the line where the template's entrypoint should be opened.
var editorNewSessionExtraArgs = map[string]string{
	"nvim":  "+%d +normal$",
	"vim":   "+%d +normal$",
	"vi":    "+%d +normal$",
	"emacs": "+%d:999",
	"hx":    "+%d",
	"kak":   "+%d:999",
	"nano":  "+%d",
	"pico":  "+%d",
}

// Run runs the session in either the process's tmux pane or a new tmux session.
//
// vertical determines whether the editor pane is split vertically (true) or horizontally (false) to
// create the results pane.
// editor is either a command in $PATH or an absolute path. editorArgs are any extra arguments to be
// passed to the editor.
//
// While this function is executing, the working directory of the process may be changed to the
// session directory. If so, then it's restored when the function returns.
//
// If the editor is not found in $PATH, the returned error wraps an [*EditorNotFoundError].
// If tmux is not found in $PATH, the returned error wraps [ErrTmuxNotFound].
// If bash is not found in $PATH, the returned error wraps [ErrBashNotFound].
// If the editor exits with a non-zero status, the returned error wraps [ErrEditorError].
func (s *Session) Run(ctx context.Context, resultsPaneSize TmuxPaneSize, vertical bool, editor string, editorArgs ...string) (err error) {
	// Check these here so that we don't find out after we've created:
	// - The results pane in the current tmux session -- killing it immediately because of the error
	//   is jarring.
	// - A new tmux session -- the error will be surfaced inside the session and then lost after the
	//   session is killed.
	if filepath.Base(editor) == editor {
		if _, err := exec.LookPath(editor); err != nil {
			return fmt.Errorf("running session: %w", &EditorNotFoundError{Editor: editor})
		}
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("running session: %w", ErrTmuxNotFound)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		return fmt.Errorf("running session: %w", ErrBashNotFound)
	}

	if s.Name != "" {
		s.Dir, err = s.setupNamedSessionDir()
	} else {
		s.Dir, err = s.setupAnonSessionDir(s.template)
	}
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}

	// Best effort as the last opened time is only depended upon during tab completion
	_ = os.WriteFile(filepath.Join(s.Dir, sessionLastOpenedMarker), nil, 0o666)

	// Ensure that commands which rely on the current directory (like tmux split-window -c
	// "#{pane_current_path}") work as expected
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("running session: changing to session directory: %s", err)
	}
	if err := os.Chdir(s.Dir); err != nil {
		return fmt.Errorf("running session: changing to session directory: %s", err)
	}
	defer func() {
		if chErr := os.Chdir(wd); chErr != nil {
			err = errors.Join(err, fmt.Errorf("running session: restoring working directory: %s", chErr))
		}
	}()

	editorArgs = slices.Clone(editorArgs)
	initialEntrypointContents, err := s.template.EntrypointContents()
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}
	entrypointPath := filepath.Join(s.Dir, s.template.Entrypoint)
	currentEntrypointContents, err := os.ReadFile(entrypointPath)
	if err != nil {
		return fmt.Errorf("running session: reading current entrypoint contents: %s", err)
	}
	if bytes.Equal(currentEntrypointContents, initialEntrypointContents) {
		editorName := filepath.Base(editor) // editor could be an absolute path
		if format, ok := editorNewSessionExtraArgs[editorName]; ok {
			extraArgs := strings.Fields(fmt.Sprintf(format, s.template.EntrypointStartLine))
			editorArgs = append(editorArgs, extraArgs...)
		}
	}
	editorArgs = append(editorArgs, s.template.Entrypoint)

	if currentPaneID := os.Getenv("TMUX_PANE"); currentPaneID != "" {
		return s.runFromTmuxPane(ctx, currentPaneID, resultsPaneSize, vertical, editor, editorArgs...)
	} else {
		return s.runInTmuxSession(ctx, resultsPaneSize, vertical, editor, editorArgs...)
	}
}

const namedSessionStagingDirPrefix = ".pg-tmp"

// setupNamedSessionDir creates and initialises a named session directory from the session's
// template if it doesn't already exist. The session directory is returned.
func (s *Session) setupNamedSessionDir() (string, error) {
	sessionDir := s.namedSessionDir(s.Name)
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
	if err := s.template.Initialise(stagingDir); err != nil {
		return "", err
	}
	if err := os.Rename(stagingDir, sessionDir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// If the directory already exists, there must have been a racing process trying to
			// create the same session. We can just behave as if the directory had already existed
			// when we checked and ignore the error.
			return "", nil
		}
		return "", fmt.Errorf("setting up session directory: moving into place: %s", err)
	}

	return sessionDir, nil
}

// namedSessionDir returns the directory where a named session is stored.
func (s *Session) namedSessionDir(name string) string {
	templateSessionsDir := templateSessionsDir(s.sessionsDir, s.template.Name)
	return filepath.Join(templateSessionsDir, name)
}

// templateSessionsDir returns the directory where named sessions for the given template are stored.
func templateSessionsDir(sessionsDir string, templateName string) string {
	return filepath.Join(sessionsDir, templateName)
}

// setupAnonSessionDir creates and initialises a session directory from template in a temporary
// directory. The session directory is returned.
func (s *Session) setupAnonSessionDir(template template) (string, error) {
	sessionDirParent := filepath.Join(os.TempDir(), "pg")
	if err := os.MkdirAll(sessionDirParent, 0755); err != nil {
		return "", fmt.Errorf("setting up session directory: creating temporary directory: %s", err)
	}
	namePattern := fmt.Sprintf("%s-*", template.Name)
	sessionDir, err := os.MkdirTemp(sessionDirParent, namePattern)
	if err != nil {
		return "", fmt.Errorf("setting up session directory: %s", err)
	}
	if err := template.Initialise(sessionDir); err != nil {
		return "", err
	}
	return sessionDir, nil
}

// ErrEditorError indicates that the editor exited with a non-zero status.
var ErrEditorError = fmt.Errorf("editor exited with non-zero status")

// runFromTmuxPane runs the session using the given pane as the editor pane.
func (s *Session) runFromTmuxPane(ctx context.Context, paneID string, resultsPaneSize TmuxPaneSize, vertical bool, editor string, editorArgs ...string) error {
	editorCmd := cmdWithStdio(ctx, editor, editorArgs...)
	editorCmd.Dir = s.Dir
	if err := editorCmd.Start(); err != nil {
		return fmt.Errorf("running session: opening editor: %s", err)
	}

	resultsPaneID, err := s.createResultsPane(ctx, paneID, resultsPaneSize, vertical)
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}
	defer func() {
		// Use a fresh context in case the main one has been cancelled
		_, killErr := cmdOutput(context.Background(), "tmux", "kill-pane", "-t", resultsPaneID)
		if killErr != nil && !strings.Contains(killErr.Error(), "can't find pane") {
			err = errors.Join(err, fmt.Errorf("running session: closing results pane: %s", killErr))
		}
	}()
	if err := s.configureResultsPane(ctx, resultsPaneID); err != nil {
		return fmt.Errorf("running session: %s", err)
	}

	if err := editorCmd.Wait(); err != nil {
		return fmt.Errorf("running session: %w", ErrEditorError)
	}

	return nil
}

// runFromTmuxPane runs the session in a new tmux session.
func (s *Session) runInTmuxSession(ctx context.Context, resultsPaneSize TmuxPaneSize, vertical bool, editor string, editorArgs ...string) error {
	// Running in a new tmux session is a bit trickier than just running from an existing pane. We
	// want to know when the editor exits and with which status so that we can kill the tmux session
	// and report to the caller whether the editor errored. In the existing pane case, we started
	// the editor process so we can just wait for it to exit. However, when we start a new session,
	// tmux is the one starting the editor process so we can't wait for it to exit in the same way
	// since it's not our child.
	//
	// Instead of starting the editor pane with just the editor command, we wrap it in a bash script
	// which uses "tmux wait-for -S $signal" to send a signal which can be received by a separate
	// call to "tmux wait-for $signal". We also populate a session option with the editor's exit
	// status once it exits which we can read once we receive the signal.
	const editorPaneExitedSignalPrefix = "pg-editor-pane-exited"
	const editorExitStatusOpt = "@pg-editor-exit-status"
	editorPaneCmds := []string{
		fmt.Sprintf(`trap 'tmux wait-for -S "%s-$TMUX_PANE"' EXIT`, editorPaneExitedSignalPrefix),
		"$@",
		// Only record the exit status if the editor exited on its own accord. If bash's process
		// group is terminated by a signal (e.g. when tmux sends SIGHUP after a kill-pane), then
		// this command won't be executed.
		fmt.Sprintf(`tmux set-option -t "$TMUX_PANE" %s "$?"`, editorExitStatusOpt),
	}
	editorPaneScript := strings.Join(editorPaneCmds, "; ")
	x, y, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("running session: starting tmux session: %s", err)
	}
	newSessionArgs := []string{
		"new-session",
		"-d", // Don't attach the session, otherwise the output of -P will go into the session
		"-c", s.Dir,
		"-x", strconv.Itoa(x), "-y", strconv.Itoa(y),
		"-P", "-F", "#{session_id} #{pane_id}",
		"bash", "-c", editorPaneScript, "pg-editor", editor,
	}
	newSessionArgs = append(newSessionArgs, editorArgs...)
	sessionIDEditorPaneID, err := cmdOutput(ctx, "tmux", newSessionArgs...)
	if err != nil {
		return fmt.Errorf("running session: starting tmux session: %s", err)
	}

	fields := strings.Fields(sessionIDEditorPaneID)
	if len(fields) != 2 {
		return fmt.Errorf("running session: expected new-session output to contain 2 fields: %q", sessionIDEditorPaneID)
	}
	sessionID := fields[0]
	editorPaneID := fields[1]
	defer func() {
		// Use a fresh context in case the main one has been cancelled
		_, killErr := cmdOutput(context.Background(), "tmux", "kill-session", "-t", sessionID)
		if killErr != nil && !strings.Contains(killErr.Error(), "can't find session") {
			err = errors.Join(err, fmt.Errorf("running session: killing tmux session: %s", killErr))
		}
	}()

	resultsPaneID, err := s.createResultsPane(ctx, editorPaneID, resultsPaneSize, vertical)
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}
	if err := s.configureResultsPane(ctx, resultsPaneID); err != nil {
		return fmt.Errorf("running session: %s", err)
	}

	attachCmd := cmdWithStdio(ctx, "tmux", "attach-session", "-t", sessionID)
	if err := attachCmd.Start(); err != nil {
		return fmt.Errorf("running session: attaching to tmux session: %s", err)
	}
	attachExited := make(chan struct{})
	go func() {
		defer close(attachExited)
		_ = attachCmd.Wait() // If there is an error, it should be logged out anyway
	}()

	type cmdOutputResult struct {
		Output string
		Err    error
	}
	editorExitStatusResult := make(chan cmdOutputResult)
	go func() {
		defer close(editorExitStatusResult)
		signal := fmt.Sprintf("%s-%s", editorPaneExitedSignalPrefix, editorPaneID)
		output, err := cmdOutput(ctx,
			"tmux",
			"wait-for", signal,
			";", "show-options", "-t", sessionID, "-q", "-v", editorExitStatusOpt)
		editorExitStatusResult <- cmdOutputResult{Output: output, Err: err}
	}()

	var editorExitStatus string
	select {
	case result := <-editorExitStatusResult:
		if result.Err != nil {
			return fmt.Errorf("running session: waiting for editor exit status: %s", err)
		}
		editorExitStatus = result.Output
	case <-attachExited:
	case <-ctx.Done():
		return fmt.Errorf("running session: waiting for session to end: %s", ctx.Err())
	}

	_, err = cmdOutput(ctx, "tmux", "kill-session", "-t", sessionID)
	if err != nil && !strings.Contains(err.Error(), "can't find session") {
		return fmt.Errorf("running session: killing tmux session: %s", err)
	}
	<-attachExited // If it hasn't already, killing the session will cause attach to exit

	// editorExitStatus == "" implies that the editor didn't exit on its own accord
	if editorExitStatus != "" && editorExitStatus != "0" {
		return fmt.Errorf("running session: %w", ErrEditorError)
	}

	return nil
}

// createResultsPane creates the results pane by splitting it from the given pane.
func (s *Session) createResultsPane(ctx context.Context, paneID string, size TmuxPaneSize, vertical bool) (string, error) {
	splitFlag := "-v"
	if vertical {
		splitFlag = "-h"
	}
	resultsPaneID, err := cmdOutput(ctx,
		"tmux", "split-window",
		"-t", paneID,
		"-d",
		splitFlag,
		"-l", string(size),
		"-P", "-F", "#{pane_id}",
		// See [resultsCLI] for the results subcommand
		s.pgPath, "__results", s.Dir, s.template.Entrypoint)
	if err != nil {
		return "", fmt.Errorf("creating results pane: %s", err)
	}
	return resultsPaneID, nil
}

// configureResultsPane configures the results pane for use in a session. This is separate from
// [Session.createResultsPane] to allow for cleanup of the pane to be scheduled before it's
// configured.
func (s *Session) configureResultsPane(ctx context.Context, id string) error {
	_, err := cmdOutput(ctx,
		"tmux",
		// Protect the user from accidentally killing the process with Ctrl-C
		"select-pane", "-t", id, "-d",
		// Keep the results pane open if the results command exits with a non-zero status
		";", "set-option", "-p", "-t", id, "remain-on-exit", "failed",
		";", "set-option", "-p", "-t", id, "remain-on-exit-format",
		`pg: results command has exited unexpectedly. Restart it with "tmux respawn-pane -t #{pane_id}".`,
	)
	if err != nil {
		return fmt.Errorf("configuring results pane: %s", err)
	}
	return nil
}

// AlreadyExistsError records that a session with the same name already exists and where its session
// directory is.
type AlreadyExistsError struct {
	Name string
	Dir  string
}

func (e *AlreadyExistsError) Error() string {
	return fmt.Sprintf("session %q already exists", e.Name)
}

// Save saves the session as a named session.
// If the name is invalid, the returned error wraps an [*InvalidNameError].
// If a session with the same name already exists, the returned error wraps an
// [*AlreadyExistsError].
func (s *Session) Save(name string) error {
	if s.Dir == "" {
		return fmt.Errorf("saving session: session must be run first")
	}
	if err := validateSessionName(name); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}
	newSessionDir := s.namedSessionDir(name)
	if err := os.MkdirAll(filepath.Dir(newSessionDir), 0755); err != nil {
		return fmt.Errorf("saving session: %s", err)
	}
	if err := os.Rename(s.Dir, newSessionDir); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("saving session: %w", &AlreadyExistsError{Name: name, Dir: newSessionDir})
		}
		return fmt.Errorf("saving session: %s", err)
	}
	s.Name = name
	s.Dir = newSessionDir
	return nil
}

// Info describes an available session.
type Info struct {
	Name       string    // Session name
	LastOpened time.Time // Last time session was opened; zero value if unknown
}

// TemplateSessions returns all sessions using the given template.
func TemplateSessions(templateName string, sessionsDir string) ([]Info, error) {
	templateSessionsDir := templateSessionsDir(sessionsDir, templateName)
	sessionDirs, err := os.ReadDir(templateSessionsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("listing sessions: reading %q template sessions directory: %s", templateName, err)
	}
	var sessions []Info
	for _, dir := range sessionDirs {
		name := dir.Name()
		if strings.HasPrefix(name, namedSessionStagingDirPrefix) {
			continue
		}
		var lastOpened time.Time
		lastOpenedPath := filepath.Join(templateSessionsDir, name, sessionLastOpenedMarker)
		// The file may have been removed by the user, this is fine
		if info, err := os.Stat(lastOpenedPath); err == nil {
			lastOpened = info.ModTime()
		}
		session := Info{Name: name, LastOpened: lastOpened}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// cmdOutput executes a command and returns its stdout output.
func cmdOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("executing %q: %s", cmd, string(bytes.TrimSpace(exitErr.Stderr)))
		}
		return "", fmt.Errorf("executing %q: %s", cmd, err)
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
