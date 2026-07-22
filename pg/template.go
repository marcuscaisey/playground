package main

import (
	"bufio"
	"cmp"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const runScriptFilename = "run.sh"

// template represents a playground template.
//
// A playground template is a directory containing the files used to run a playground session. The
// contents of the template are copied into the session directory when the session is started.
//
// A template contains:
//   - Exactly one "main.*" file, the entrypoint opened when the session starts.
//     If __CURSOR__ appears anywhere on a line, then the contents of the line (excluding leading
//     whitespace) are erased when the template is copied into the session directory and, if
//     possible, the cursor is placed on it when the entrypoint is opened for the first time. All
//     __CURSOR__ appearances after the first one are ignored.
//   - A "run.sh" file, the run script executed as "bash run.sh"
//   - Any other files needed to run the session
type template struct {
	Name                string // Template name; matches the template directory name
	FS                  fs.FS  // File system containing the template's files
	Entrypoint          string // Entrypoint filename, such as "main.go"
	EntrypointStartLine int    // Line where the entrypoint should be opened
	isBuiltIn           bool   // Whether the template is built-in
}

// SetupSessionDir copies the template's files into a session directory and erases the contents of
// the first line of the entrypoint that the placeholder __CURSOR__ appears on.
func (t template) SetupSessionDir(dir string) error {
	if err := os.CopyFS(dir, t.FS); err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}

	entrypointPath := filepath.Join(dir, t.Entrypoint)
	entrypointContents, err := os.ReadFile(entrypointPath)
	if err != nil {
		return fmt.Errorf("setting up session directory: removing __CURSOR__ placeholder from entrypoint: %s", err)
	}
	// Matches a whole line containing __CURSOR__ excluding the leading whitespace
	placeholderRe := regexp.MustCompile(`(?m)^\s*((?:[^\s].*)?__CURSOR__.*)$`)
	if locs := placeholderRe.FindSubmatchIndex(entrypointContents); locs != nil {
		// Replace with ' ' instead of '' so that when the editor is opened on the last character of
		// the line (+normal$ arg passed to vim), it's on the first character after the leading
		// whitespace.
		entrypointContents = slices.Concat(entrypointContents[:locs[2]], []byte{' '}, entrypointContents[locs[3]:])
		if err := os.WriteFile(entrypointPath, entrypointContents, 0o666); err != nil {
			return fmt.Errorf("setting up session directory: removing __CURSOR__ placeholder from entrypoint: %s", err)
		}
	}

	// From the [embed] docs: "Patterns must not match files outside the package's module, such as
	// ‘.git/*’, symbolic links, 'vendor/', or any directories containing go.mod (these are separate
	// modules).". This prevents us from including a go.mod file in the built-in Go template
	// directory so we just write one to the session directory ourselves.
	if t.Name == "go" && t.isBuiltIn {
		path := filepath.Join(dir, "go.mod")
		data := []byte("module playground\n")
		if err := os.WriteFile(path, data, 0o666); err != nil {
			return fmt.Errorf("setting up session directory: writing built-in go template go.mod: %s", err)
		}
	}
	return nil
}

var errTemplateNotFound = fmt.Errorf("not found")

// invalidTemplateError records an invalid template and the reason it's invalid.
//
// Source describes where the template came from (built-in or path under user templates directory).
type invalidTemplateError struct {
	Name   string
	Source string
	Reason string
}

func (e *invalidTemplateError) Error() string {
	return e.Reason
}

// newInvalidTemplateErrorf constructs a new [invalidTemplateErrorf].
// Empty path is used for built-in templates.
func newInvalidTemplateErrorf(name string, path string, reason string, a ...any) *invalidTemplateError {
	return &invalidTemplateError{
		Name:   name,
		Source: cmp.Or(path, "built-in"),
		Reason: fmt.Sprintf(reason, a...),
	}
}

// File system containing the built-in templates directory
//
//go:embed templates/*
var builtinTemplatesRootFS embed.FS

// File system containing the built-in templates
var builtinTemplatesFS fs.FS = func() fs.FS {
	templatesFS, err := fs.Sub(builtinTemplatesRootFS, "templates")
	if err != nil {
		panic(fmt.Sprintf("failed to construct built-in templates file system: %s", err))
	}
	return templatesFS
}()

// loadTemplate loads a template by searching the user templates directory first, then the set of
// built-in templates.
// If the template does not exist, the returned error wraps [errTemplateNotFound].
// If the loaded template is invalid, the returned error wraps [invalidTemplateErrorf].
func loadTemplate(name string, userTemplatesDir string) (template, error) {
	var templateFS fs.FS
	path := ""
	userTemplatePath := filepath.Join(userTemplatesDir, name)
	userTemplateExists, err := fileExists(userTemplatePath)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: checking user templates directory: %s", name, err)
	}
	if userTemplateExists {
		templateFS = os.DirFS(userTemplatePath)
		path = userTemplatePath
	} else {
		templateFS, err = fs.Sub(builtinTemplatesFS, name)
		if err != nil {
			return template{}, fmt.Errorf("loading template %q: checking built-in templates directory: %s", name, err)
		}
		// [fs.Sub] doesn't check whether the dir exists, so we need to
		builtinTemplateExists, err := fileExistsFS(templateFS, ".")
		if err != nil {
			return template{}, fmt.Errorf("loading template %q: checking built-in templates directory: %s", name, err)
		}
		if !builtinTemplateExists {
			return template{}, fmt.Errorf("loading template %q: %w", name, errTemplateNotFound)
		}
	}

	const entrypointPattern = "main.*"
	entrypoints, err := fs.Glob(templateFS, entrypointPattern)
	if err != nil {
		// [fs.Glob] can only return [path.ErrBadPattern] so this should never happen
		panic(fmt.Sprintf("failed to glob for entrypoint: %s", err))
	}
	var entrypoint string
	switch len(entrypoints) {
	case 1:
		entrypoint = entrypoints[0]
	case 0:
		return template{}, fmt.Errorf("loading template %q: %w", name, newInvalidTemplateErrorf(name, path, "entrypoint (%q file) is missing", entrypointPattern))
	default:
		return template{}, fmt.Errorf("loading template %q: %w", name, newInvalidTemplateErrorf(name, path, "multiple entrypoints: %q", entrypoints))
	}

	runScriptExists, err := fileExistsFS(templateFS, runScriptFilename)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	}
	if !runScriptExists {
		return template{}, fmt.Errorf("loading template %q: %w", name, newInvalidTemplateErrorf(name, path, "run script (%q file) is missing", runScriptFilename))
	}

	f, err := templateFS.Open(entrypoint)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: reading entrypoint start line: %s", name, err)
	}
	defer f.Close() // nolint:errcheck // There's nothing we can do with this error
	scanner := bufio.NewScanner(f)
	line := 1
	entrypointStartLine := 0
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "__CURSOR__") {
			entrypointStartLine = line
			break
		}
		line++
	}
	if err := scanner.Err(); err != nil {
		return template{}, fmt.Errorf("reading template entrypoint start line: %s", err)
	}

	return template{
		Name:                name,
		FS:                  templateFS,
		Entrypoint:          entrypoint,
		EntrypointStartLine: entrypointStartLine,
		isBuiltIn:           path == "",
	}, nil
}

// templateInfo describes an available template.
type templateInfo struct {
	Name      string // Template name
	IsBuiltin bool   // Whether the template is built-in
}

// listTemplates returns all distinct built-in and user templates.
func listTemplates(userTemplatesDir string) ([]templateInfo, error) {
	templates := map[string]templateInfo{}

	builtinTemplateDirs, err := fs.ReadDir(builtinTemplatesFS, ".")
	if err != nil {
		return nil, fmt.Errorf("listing templates: reading built-in templates: %s", err)
	}
	for _, dirEntry := range builtinTemplateDirs {
		templates[dirEntry.Name()] = templateInfo{Name: dirEntry.Name(), IsBuiltin: true}
	}

	userTemplateDirs, err := os.ReadDir(userTemplatesDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("listing templates: reading user templates: %s", err)
	}
	for _, dirEntry := range userTemplateDirs {
		templates[dirEntry.Name()] = templateInfo{Name: dirEntry.Name(), IsBuiltin: false}
	}

	return slices.Collect(maps.Values(templates)), nil
}
