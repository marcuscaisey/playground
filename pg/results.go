package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// printSessionResults watches a session directory for changes to the entrypoint or run script,
// executing the run script and printing the results when changes occur.
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
	runScriptPath := filepath.Join(sessionDir, runScriptFilename)
	var startTimerC <-chan time.Time
	stopCurrentRun := func() {}
	defer stopCurrentRun()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("printing session results: watcher events channel closed")
			}
			if event.Name != entrypointPath && event.Name != runScriptPath {
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
