package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	os.Exit(cli())
}

// cliError describes an incorrect usage of the CLI.
type cliError string

func (e cliError) Error() string {
	return string(e)
}

// newCliErrorf creates a [cliError].
// The error message is constructed from the given format string and arguments, as in [fmt.Sprintf].
func newCliErrorf(format string, a ...any) cliError {
	return cliError(fmt.Sprintf(format, a...))
}

// cli parses the CLI args, runs pg, and returns the appropriate exit code:
//   - If the -help flag is passed, then the usage is printed and 0 is returned.
//   - If the CLI is used incorrectly (e.g. invalid flag, not enough args), then the error and usage
//     are printed and 2 is returned.
//   - If any other error occurs, then the error is printed and 1 is returned.
func cli() int {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: pg [options] <template-name>")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
	}
	printHelp := flag.Bool("help", false, "Print this message")

	flag.Parse()

	if *printHelp {
		flag.Usage()
		return 0
	}

	if err := pg(flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if _, ok := errors.AsType[cliError](err); ok {
			flag.Usage()
			return 2
		}
		return 1
	}

	return 0
}

func pg(args []string) error {
	if len(args) < 1 {
		return newCliErrorf("template name not provided")
	}
	if len(args) > 1 {
		return newCliErrorf("unexpected arguments: %s", strings.Join(args, ", "))
	}

	// 1. Load template
	// 2. Split pane
	// 3. Start nvim in top pane
	// 4. Start pg in runner mode in bottom pane

	templateName := args[0]
	_, err := loadTemplate(templateName)
	if err != nil {
		return err
	}

	closeResultsPane, err := runInNewTmuxPane("watch date")
	if err != nil {
		return err
	}
	defer func() {
		if err := closeResultsPane(); err != nil {
			fmt.Fprintf(os.Stderr, "closing results tmux pane: %s", err)
		}
	}()

	time.Sleep(10 * time.Second)

	return nil
}

//go:embed templates/*
var builtinTemplates embed.FS

type template struct {
	EntrypointContents []byte
	RunScriptContents  []byte
}

// loadTemplate loads a template from the set of built-in templates.
func loadTemplate(name string) (template, error) {
	templateDir := filepath.Join("templates", name)
	templateDirContents, err := builtinTemplates.ReadDir(templateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return template{}, fmt.Errorf("loading template %q: does not exist", name)
		}
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	}

	var entrypointName string
	for _, dirEntry := range templateDirContents {
		if dirEntry.IsDir() {
			continue
		}
		filename := strings.TrimSuffix(dirEntry.Name(), filepath.Ext(dirEntry.Name()))
		if filename == "main" {
			if entrypointName != "" {
				return template{}, fmt.Errorf("loading template %q: multiple entrypoints defined: %q and %q", name, entrypointName, dirEntry.Name())
			}
			entrypointName = dirEntry.Name()
		}
	}
	if entrypointName == "" {
		return template{}, fmt.Errorf(`loading template %q: entrypoint ("main.*") is missing`, name)
	}
	entrypointContents, err := builtinTemplates.ReadFile(filepath.Join(templateDir, entrypointName))
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	}

	runScriptContents, err := builtinTemplates.ReadFile(filepath.Join(templateDir, "run.sh"))
	if err != nil {
		if os.IsNotExist(err) {
			return template{}, fmt.Errorf(`loading template %q: run script ("run.sh") is missing`, name)
		}
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	}

	return template{
		EntrypointContents: entrypointContents,
		RunScriptContents:  runScriptContents,
	}, nil
}

// runInNewTmuxPane splits the current tmux pane vertically and runs a command in the new pane,
// leaving the current pane selected.
// The returned function closes the new pane.
func runInNewTmuxPane(cmd string) (func() error, error) {
	if _, ok := os.LookupEnv("TMUX"); !ok {
		return nil, fmt.Errorf("splitting tmux window: not currently in a tmux session")
	}

	tmuxCmd := exec.Command("tmux", "split-window", "-d", "-P", "-F", "#{pane_id}", cmd)
	output, err := tmuxCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("splitting tmux window: %s", cmdErrMsg(err))
	}
	paneID := strings.TrimSpace(string(output))

	return func() error {
		cmd := exec.Command("tmux", "kill-pane", "-t", paneID)
		if _, err = cmd.Output(); err != nil {
			return fmt.Errorf("killing tmux pane %q: %s", paneID, cmdErrMsg(err))
		}
		return nil
	}, nil
}

// cmdErrMsg returns the appropriate error message for an [error] returned by [exec.Cmd.Output].
// If possible, the stderr output is extracted from the error. Otherwise, the value of [error.Error]
// is returned.
func cmdErrMsg(err error) string {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return string(exitErr.Stderr)
	}
	return err.Error()
}
