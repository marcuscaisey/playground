// Package session provides functionality for managing sessions and inspecting sessions and
// templates. See the manual (man docs/pg.1) for a description of sessions and templates.
package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/term"
)

// Session represents a playground session.
type Session struct {
	Name string // Name of the session; if empty, the session is anonymous
	// Directory containing the session's files.
	// Dir is empty until its populated as a result of calling [Session.Run].
	// Dir != "" implies that the directory has been created.
	Dir          string
	TemplateName string // Name of the session's template
	template     template
	sessionsDir  string // Directory where sessions are created
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
func New(name string, templateName string, sessionsDir string, userTemplatesDir string) (*Session, error) {
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
	if strings.HasPrefix(name, sessionStagingDirPrefix) {
		reason = fmt.Sprintf("cannot use reserved prefix %q", sessionStagingDirPrefix)
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
	// ErrShNotFound indicates that sh was not found in $PATH.
	ErrShNotFound = fmt.Errorf("sh not found in $PATH")
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
// create the output pane.
// editor is either a command in $PATH or an absolute path. editorArgs are any extra arguments to be
// passed to the editor.
//
// While this function is executing, the working directory of the process may be changed to the
// session directory. If so, then it's restored when the function returns.
//
// If the editor is not found in $PATH, the returned error wraps an [*EditorNotFoundError].
// If tmux is not found in $PATH, the returned error wraps [ErrTmuxNotFound].
// If sh is not found in $PATH, the returned error wraps [ErrShNotFound].
// If the editor exits with a non-zero status, the returned error wraps [ErrEditorError].
func (s *Session) Run(
	ctx context.Context,
	outputPaneSize TmuxPaneSize,
	vertical bool,
	editor string,
	editorArgs ...string,
) (err error) {
	if s.Name != "" {
		s.Dir, err = s.setupNamedSessionDir()
	} else {
		s.Dir, err = s.setupAnonSessionDir()
	}
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}

	// Write is best effort as the last opened time is only depended upon during shell completion
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
		return s.runFromTmuxPane(ctx, currentPaneID, outputPaneSize, vertical, editor, editorArgs...)
	} else {
		return s.runInTmuxSession(ctx, outputPaneSize, vertical, editor, editorArgs...)
	}
}

const sessionStagingDirPrefix = ".pg-tmp"

// setupNamedSessionDir creates and initialises a named session directory from the session's
// template if it doesn't already exist. The session directory is returned.
func (s *Session) setupNamedSessionDir() (string, error) {
	templateSessionsDir := templateSessionsDir(s.sessionsDir, s.template.Name)
	sessionDir := filepath.Join(templateSessionsDir, s.Name)
	if ok, err := fileExists(sessionDir); err != nil {
		return "", fmt.Errorf("setting up session directory: %s", err)
	} else if ok {
		return sessionDir, nil
	}

	// Set up in a staging directory first so that if there's an error, we're not left with a
	// partially set up directory which would be used by future sessions.
	if err := os.MkdirAll(templateSessionsDir, 0755); err != nil {
		return "", fmt.Errorf("setting up session directory: %s", err)
	}
	stagingDirNamePattern := fmt.Sprintf("%s-*", sessionStagingDirPrefix)
	stagingDir, err := os.MkdirTemp(templateSessionsDir, stagingDirNamePattern)
	if err != nil {
		return "", fmt.Errorf("setting up session directory: creating staging directory: %s", err)
	}
	// We can tolerate this directory hanging around since it's hidden
	defer func() { _ = os.RemoveAll(stagingDir) }()
	if err := s.template.Initialise(stagingDir); err != nil {
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

// setupAnonSessionDir creates and initialises an anonymous session directory from the session's
// template. The session directory is returned.
func (s *Session) setupAnonSessionDir() (string, error) {
	// Set up in a staging directory first so that if there's an error, we're not left with a
	// partially set up directory which would be used by future sessions.
	templateSessionsDir := templateSessionsDir(s.sessionsDir, s.template.Name)
	if err := os.MkdirAll(templateSessionsDir, 0755); err != nil {
		return "", fmt.Errorf("setting up session directory: %s", err)
	}
	stagingDirNamePattern := fmt.Sprintf("%s-*", sessionStagingDirPrefix)
	stagingDir, err := os.MkdirTemp(templateSessionsDir, stagingDirNamePattern)
	if err != nil {
		return "", fmt.Errorf("setting up session directory: creating staging directory: %s", err)
	}
	// We can tolerate this directory hanging around since it's hidden
	defer func() { _ = os.RemoveAll(stagingDir) }()
	if err := s.template.Initialise(stagingDir); err != nil {
		return "", err
	}

	try := 0
	for {
		name := fmt.Sprintf("anonymous-%d", rand.Uint32())
		sessionDir := filepath.Join(templateSessionsDir, name)
		if err := os.Rename(stagingDir, sessionDir); err != nil {
			if errors.Is(err, fs.ErrExist) {
				if try++; try < 10000 {
					continue
				}
				return "", fmt.Errorf("setting up session directory: failed to create temporary directory after %d attempts", try)
			}
			return "", fmt.Errorf("setting up session directory: moving into place: %s", err)
		}
		return sessionDir, nil
	}
}

// templateSessionsDir returns the directory where sessions for the given template are created.
func templateSessionsDir(sessionsDir string, templateName string) string {
	return filepath.Join(sessionsDir, templateName)
}

// ErrEditorError indicates that the editor exited with a non-zero status.
var ErrEditorError = fmt.Errorf("editor exited with non-zero status")

// runFromTmuxPane runs the session using the given pane as the editor pane.
// If the editor is not found in $PATH, the returned error wraps an [*EditorNotFoundError].
// If tmux is not found in $PATH, the returned error wraps [ErrTmuxNotFound].
// If the editor exits with a non-zero status, the returned error wraps [ErrEditorError].
func (s *Session) runFromTmuxPane(
	ctx context.Context,
	paneID string,
	outputPaneSize TmuxPaneSize,
	vertical bool,
	editor string,
	editorArgs ...string,
) (err error) {
	// Check this before we open the editor since closing it immediately because of the error could
	// be jarring.
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("running session: %w", ErrTmuxNotFound)
	}

	wg := new(sync.WaitGroup)
	wgCtx, cancelWgCtx := context.WithCancel(ctx)
	defer func() {
		cancelWgCtx()
		wg.Wait()
	}()

	editorCmd := exec.CommandContext(wgCtx, editor, editorArgs...)
	editorCmd.Dir = s.Dir
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	editorCmd.Cancel = func() error {
		return editorCmd.Process.Signal(syscall.SIGTERM) // Allow the editor to shutdown gracefully
	}
	if err := editorCmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("running session: %w", &EditorNotFoundError{Editor: editor})
		}
		return fmt.Errorf("running session: opening editor: %s", err)
	}

	outputPaneID, err := s.createOutputPane(ctx, paneID, outputPaneSize, vertical)
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}
	defer func() {
		// Use a fresh context in case the main one has been cancelled
		_, killErr := cmdOutput(context.Background(), "tmux", "kill-pane", "-t", outputPaneID)
		if killErr != nil && !strings.Contains(killErr.Error(), "can't find pane") {
			err = errors.Join(err, fmt.Errorf("running session: closing output pane: %s", killErr))
		}
	}()

	watchErr := make(chan error, 1)
	wg.Go(func() {
		watchErr <- s.watchSessionDir(wgCtx, outputPaneID)
	})

	editorErr := make(chan error, 1)
	wg.Go(func() {
		editorErr <- editorCmd.Wait()
	})

	select {
	case err := <-watchErr:
		if err != nil {
			return fmt.Errorf("running session: %s", err)
		}
	case err := <-editorErr:
		if err != nil {
			return fmt.Errorf("running session: %w", ErrEditorError)
		}
	case <-ctx.Done():
		return fmt.Errorf("running session: waiting for session to end: %s", ctx.Err())
	}

	return nil
}

// runFromTmuxPane runs the session in a new tmux session.
// If the editor is not found in $PATH, the returned error wraps an [*EditorNotFoundError].
// If tmux is not found in $PATH, the returned error wraps [ErrTmuxNotFound].
// If sh is not found in $PATH, the returned error wraps [ErrShNotFound].
// If the editor exits with a non-zero status, the returned error wraps [ErrEditorError].
func (s *Session) runInTmuxSession(
	ctx context.Context,
	outputPaneSize TmuxPaneSize,
	vertical bool,
	editor string,
	editorArgs ...string,
) (err error) {
	// Check these before we create the tmux session since the error will be surfaced inside the
	// session and then lost after it's killed.
	if filepath.Base(editor) == editor {
		if _, err := exec.LookPath(editor); err != nil {
			return fmt.Errorf("running session: %w", &EditorNotFoundError{Editor: editor})
		}
	}
	if _, err := exec.LookPath("sh"); err != nil {
		return fmt.Errorf("running session: %w", ErrShNotFound)
	}

	// Running in a new tmux session is a bit trickier than just running from an existing pane. We
	// want to know when the editor exits and with which status so that we can kill the tmux session
	// and report to the caller whether the editor errored. In the existing pane case, we started
	// the editor process so we can just wait for it to exit. However, when we start a new session,
	// tmux is the one starting the editor process so we can't wait for it to exit in the same way
	// since it's not our child.
	//
	// Instead of starting the editor pane with just the editor command, we wrap it in a shell
	// script which uses "tmux wait-for -S $signal" to send a signal which can be received by a
	// separate call to "tmux wait-for $signal". We also populate a session option with the editor's
	// exit status once it exits which we can read once we receive the signal.
	const editorPaneExitedSignalPrefix = "pg-editor-pane-exited"
	const editorExitStatusOpt = "@pg-editor-exit-status"
	editorPaneCmds := []string{
		fmt.Sprintf(`trap 'tmux wait-for -S "%s-$TMUX_PANE"' EXIT`, editorPaneExitedSignalPrefix),
		"$@",
		// Only record the exit status if the editor exited on its own accord. If the foreground
		// process group is terminated by a signal (e.g. when tmux sends SIGHUP after a kill-pane),
		// then this command won't be executed.
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
		"sh", "-c", editorPaneScript, "pg-editor", editor,
	}
	newSessionArgs = append(newSessionArgs, editorArgs...)
	sessionIDEditorPaneID, err := cmdOutput(ctx, "tmux", newSessionArgs...)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("running session: starting tmux session: %w", ErrTmuxNotFound)
		}
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

	outputPaneID, err := s.createOutputPane(ctx, editorPaneID, outputPaneSize, vertical)
	if err != nil {
		return fmt.Errorf("running session: %s", err)
	}

	wg := new(sync.WaitGroup)
	wgCtx, cancelWgCtx := context.WithCancel(ctx)
	defer func() {
		cancelWgCtx()
		wg.Wait()
	}()

	watchErr := make(chan error, 1)
	wg.Go(func() {
		watchErr <- s.watchSessionDir(wgCtx, outputPaneID)
	})

	attachCmd := exec.CommandContext(wgCtx, "tmux", "attach-session", "-t", sessionID)
	attachCmd.Stdin = os.Stdin
	attachCmd.Stdout = os.Stdout
	attachCmd.Stderr = os.Stderr
	if err := attachCmd.Start(); err != nil {
		return fmt.Errorf("running session: attaching to tmux session: %s", err)
	}
	attachExited := make(chan struct{})
	wg.Go(func() {
		defer close(attachExited)
		_ = attachCmd.Wait() // Any error we're ignoring should have been logged to stderr
	})

	type cmdOutputResult struct {
		Output string
		Err    error
	}
	editorExitStatusResult := make(chan cmdOutputResult, 1)
	wg.Go(func() {
		signal := fmt.Sprintf("%s-%s", editorPaneExitedSignalPrefix, editorPaneID)
		output, err := cmdOutput(wgCtx,
			"tmux",
			"wait-for", signal,
			";", "show-options", "-t", sessionID, "-q", "-v", editorExitStatusOpt)
		editorExitStatusResult <- cmdOutputResult{Output: output, Err: err}
	})

	var editorExitStatus string
	select {
	case err := <-watchErr:
		if err != nil {
			return fmt.Errorf("running session: %s", err)
		}
	case <-attachExited:
	case result := <-editorExitStatusResult:
		if result.Err != nil {
			return fmt.Errorf("running session: waiting for editor exit status: %s", result.Err)
		}
		editorExitStatus = result.Output
	case <-ctx.Done():
		return fmt.Errorf("running session: waiting for session to end: %s", ctx.Err())
	}

	_, err = cmdOutput(ctx, "tmux", "kill-session", "-t", sessionID)
	if err != nil && !strings.Contains(err.Error(), "can't find session") {
		return fmt.Errorf("running session: killing tmux session: %s", err)
	}
	// If it hasn't already, killing the session will cause attach to exit. We wait for it to exit
	// since it's in the foreground.
	<-attachExited

	// editorExitStatus == "" implies that the editor didn't exit on its own accord, so we don't
	// class this as an editor error.
	if editorExitStatus != "" && editorExitStatus != "0" {
		return fmt.Errorf("running session: %w", ErrEditorError)
	}

	return nil
}

// createOutputPane splits the given pane to create the output pane and returns its ID.
func (s *Session) createOutputPane(
	ctx context.Context,
	paneID string,
	size TmuxPaneSize,
	vertical bool,
) (string, error) {
	splitFlag := "-v"
	if vertical {
		splitFlag = "-h"
	}
	outputPaneID, err := cmdOutput(ctx,
		"tmux", "split-window",
		"-t", paneID,
		"-d",
		"-E",
		splitFlag,
		"-l", size.String(),
		"-P", "-F", "#{pane_id}")
	if err != nil {
		return "", fmt.Errorf("creating output pane: %s", err)
	}
	return outputPaneID, nil
}

// watchSessionDir watches the session directory for changes to the run script or any file with the
// entrypoint's extension, executing the run script and writing the output to the given pane when
// changes occur.
func (s *Session) watchSessionDir(ctx context.Context, paneID string) (err error) {
	displayCmd := exec.CommandContext(ctx, "tmux", "display-message", "-t", paneID, "-I")
	outputPipe, err := displayCmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("watching session directory: creating pipe to output pane: %s", err)
	}
	if err := displayCmd.Start(); err != nil {
		return fmt.Errorf("watching session directory: creating pipe to output pane: executing %q: %s", displayCmd, cmdErr(err))
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watching session directory: %s", err)
	}
	defer func() {
		if closeErr := watcher.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("watching session directory: closing session directory watcher: %s", closeErr))
		}
	}()

	if err := watcher.Add(s.Dir); err != nil {
		return fmt.Errorf("watching session directory: adding session directory watcher: %s", err)
	}

	entrypointExt := filepath.Ext(s.template.Entrypoint)
	runScriptPath := filepath.Join(s.Dir, runScriptFilename)
	var startTimerC <-chan time.Time
	stopCurrentRun := func() {}
	defer stopCurrentRun()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("watching session directory: watcher events channel closed")
			}
			if filepath.Ext(event.Name) != entrypointExt && event.Name != runScriptPath {
				continue
			}
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			const debounceDuration = 100 * time.Millisecond
			startTimerC = time.After(debounceDuration)

		case <-startTimerC:
			stopCurrentRun()
			stopCurrentRun, err = s.startRunScript(ctx, outputPipe, paneID)
			if err != nil {
				return fmt.Errorf("watching session directory: %s", err)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("watching session directory: watcher error channel closed")
			}
			return fmt.Errorf("watching session directory: watching session directory: %s", err)

		case <-ctx.Done():
			return fmt.Errorf("watching session directory: %s", ctx.Err())
		}
	}
}

// startRunScript starts execution of the session's run script and returns a function to stop it.
// The run script is executed as "./run.sh" from the session directory.
// The script's output is written to w.
// TMUX_PANE is set to paneID in the script's environment.
// The returned function blocks until execution has been stopped and is safe to be called after
// execution has stopped.
func (s *Session) startRunScript(ctx context.Context, w io.Writer, paneID string) (stop func(), err error) {
	cmdCtx, cancelCmdCtx := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancelCmdCtx()
		}
	}()

	cmd := exec.CommandContext(cmdCtx, "./"+runScriptFilename)
	// Without this, if TMUX_PANE is set, it will be set to the pane of the current process rather
	// than the pane that the script output is being sent to which would probably be unexpected.
	cmd.Env = append(cmd.Environ(), "TMUX_PANE="+paneID)
	cmd.Dir = s.Dir
	cmd.Stdout = w
	cmd.Stderr = w
	// Run the script in a process group so that we can signal it and any spawned child processes as
	// long as they're also in the process group (which should be the case for any commands executed
	// by the script).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}

	_, _ = fmt.Fprint(w, ansiClearScreen)
	_, _ = fmt.Fprint(w, ansiMoveCursorHome)
	_, _ = styledFprintf(w, ansiBoldGreen, "Executing %s\n\n", cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("executing %q: %s", cmd, err)
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
		_, _ = styledFprintf(w, exitStatusStyle, "\nExited with status %d\n", exitStatus)
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

// styledFprintf prints text styled using the given ANSI escape sequence(s).
func styledFprintf(w io.Writer, style style, format string, a ...any) (n int, err error) {
	return fmt.Fprintf(w, "%s%s%s", style, fmt.Sprintf(format, a...), ansiReset)
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

// Save saves the session with the given name.
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
	templateSessionsDir := templateSessionsDir(s.sessionsDir, s.template.Name)
	newSessionDir := filepath.Join(templateSessionsDir, name)
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
		if strings.HasPrefix(name, sessionStagingDirPrefix) {
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
		return "", fmt.Errorf("executing %q: %w", cmd, cmdErr(err))
	}
	return strings.TrimSpace(string(output)), nil
}

// cmdErr returns the appropriate error for an [error] returned by [exec.Cmd.Output].
// If possible, the stderr output is extracted from the error. Otherwise, the error is returned
// unchanged.
func cmdErr(err error) error {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("%s", bytes.TrimSpace(exitErr.Stderr))
	}
	return err
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
