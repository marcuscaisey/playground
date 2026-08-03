package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/marcuscaisey/playground/internal/session/templates"
)

const runScriptFilename = "run.sh"

// template represents a playground template.
// Use [loadTemplate] to load a template from disk.
type template struct {
	Name                string // Template name; matches the template directory name
	Entrypoint          string // Entrypoint filename, such as "main.go"
	EntrypointStartLine int    // Line where the entrypoint should be opened
	fs                  fs.FS  // File system containing the template's files
	isBuiltIn           bool   // Whether the template is built-in
}

// Initialise populates a directory with the template's files, ready for a session to be run.
func (t template) Initialise(dir string) error {
	if err := os.CopyFS(dir, t.fs); err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}

	entrypointContents, err := t.EntrypointContents()
	if err != nil {
		return fmt.Errorf("setting up session directory: %s", err)
	}
	if err := os.WriteFile(filepath.Join(dir, t.Entrypoint), entrypointContents, 0o666); err != nil {
		return fmt.Errorf("setting up session directory: writing entrypoint: %s", err)
	}

	// From the [embed] docs: "Patterns must not match files outside the package's module, such as
	// ‘.git/*’, symbolic links, 'vendor/', or any directories containing go.mod (these are separate
	// modules).". This prevents us from including a go.mod file in the built-in Go template
	// directory so we just write one here.
	if t.Name == "go" && t.isBuiltIn {
		path := filepath.Join(dir, "go.mod")
		data := []byte("module playground\n")
		if err := os.WriteFile(path, data, 0o666); err != nil {
			return fmt.Errorf("setting up session directory: writing built-in go template go.mod: %s", err)
		}
	}
	return nil
}

// EntrypointContents returns the contents of the template's entrypoint. The contents of the first
// line (excluding leading whitespace) that __CURSOR__ appears on are erased.
func (t template) EntrypointContents() ([]byte, error) {
	f, err := t.fs.Open(t.Entrypoint)
	if err != nil {
		return nil, fmt.Errorf("making entrypoint contents: %s", err)
	}
	defer func() { _ = f.Close() }() // Error not useful after reading
	contents, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("making entrypoint contents: %s", err)
	}
	// Matches a whole line containing __CURSOR__ excluding the leading whitespace
	placeholderRe := regexp.MustCompile(`(?m)^\s*((?:[^\s].*)?__CURSOR__.*)$`)
	if locs := placeholderRe.FindSubmatchIndex(contents); locs != nil {
		// Replace with ' ' instead of '' so that when the editor is opened on the last character of
		// the line (e.g. +normal$ passed to vim), it's on the first character after the leading
		// whitespace.
		contents = slices.Concat(contents[:locs[2]], []byte{' '}, contents[locs[3]:])
	}
	return contents, nil
}

// ErrTemplateNotFound indicates that a template was not found in either the built-in or user
// templates.
var ErrTemplateNotFound = fmt.Errorf("not found")

// InvalidTemplateError records an invalid template and the reason it's invalid.
//
// Source describes where the template came from (built-in or path under user templates directory).
type InvalidTemplateError struct {
	Name   string
	Source string
	Reason string
}

func (e *InvalidTemplateError) Error() string {
	return e.Reason
}

// loadTemplate loads a template by searching the user templates directory first, then the set of
// built-in templates.
// If the template does not exist, the returned error wraps [ErrTemplateNotFound].
// If the loaded template is invalid, the returned error wraps an [*InvalidTemplateError].
func loadTemplate(name string, userTemplatesDir string) (template, error) {
	var templateFS fs.FS
	source := "built-in"
	isBuiltin := true
	userTemplatePath := filepath.Join(userTemplatesDir, name)
	userTemplateExists, err := fileExists(userTemplatePath)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: checking user templates directory: %s", name, err)
	}
	if userTemplateExists {
		templateFS = os.DirFS(userTemplatePath)
		source = userTemplatePath
		isBuiltin = false
	} else {
		templateFS, err = fs.Sub(templates.FS, name)
		if err != nil {
			return template{}, fmt.Errorf("loading template %q: checking built-in templates directory: %s", name, err)
		}
		// [fs.Sub] doesn't check whether the dir exists, so we need to
		builtinTemplateExists, err := fileExistsFS(templateFS, ".")
		if err != nil {
			return template{}, fmt.Errorf("loading template %q: checking built-in templates directory: %s", name, err)
		}
		if !builtinTemplateExists {
			return template{}, fmt.Errorf("loading template %q: %w", name, ErrTemplateNotFound)
		}
	}

	const entrypointPattern = "main.*"
	entrypoints, err := fs.Glob(templateFS, entrypointPattern)
	if err != nil {
		// [fs.Glob] can only return [path.ErrBadPattern] so this should never happen
		return template{}, fmt.Errorf("loading template %q: globbing for entrypoint: %s", name, err)
	}
	var entrypoint string
	switch len(entrypoints) {
	case 1:
		entrypoint = entrypoints[0]
	case 0:
		return template{}, fmt.Errorf("loading template %q: %w", name, &InvalidTemplateError{
			Name:   name,
			Source: source,
			Reason: fmt.Sprintf("entrypoint (%q file) is missing", entrypointPattern),
		})
	default:
		return template{}, fmt.Errorf("loading template %q: %w", name, &InvalidTemplateError{
			Name:   name,
			Source: source,
			Reason: fmt.Sprintf("multiple entrypoints: %q", entrypoints),
		})
	}

	runScriptExists, err := fileExistsFS(templateFS, runScriptFilename)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	}
	if !runScriptExists {
		return template{}, fmt.Errorf("loading template %q: %w", name, &InvalidTemplateError{
			Name:   name,
			Source: source,
			Reason: fmt.Sprintf("run script (%q file) is missing", runScriptFilename),
		})
	}

	f, err := templateFS.Open(entrypoint)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: reading entrypoint start line: %s", name, err)
	}
	defer func() { _ = f.Close() }() // Error not useful after reading
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
		Entrypoint:          entrypoint,
		EntrypointStartLine: entrypointStartLine,
		fs:                  templateFS,
		isBuiltIn:           isBuiltin,
	}, nil
}

// TemplateInfo describes an available template.
type TemplateInfo struct {
	Name      string // Template name
	IsBuiltin bool   // Whether the template is built-in
}

// AllTemplates returns all distinct built-in and user templates.
func AllTemplates(userTemplatesDir string) ([]TemplateInfo, error) {
	infos := map[string]TemplateInfo{}

	builtinTemplateDirs, err := fs.ReadDir(templates.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("listing templates: reading built-in templates: %s", err)
	}
	for _, dirEntry := range builtinTemplateDirs {
		if dirEntry.Name() == "templates.go" {
			continue
		}
		infos[dirEntry.Name()] = TemplateInfo{Name: dirEntry.Name(), IsBuiltin: true}
	}

	userTemplateDirs, err := os.ReadDir(userTemplatesDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("listing templates: reading user templates: %s", err)
	}
	for _, dirEntry := range userTemplateDirs {
		infos[dirEntry.Name()] = TemplateInfo{Name: dirEntry.Name(), IsBuiltin: false}
	}

	return slices.Collect(maps.Values(infos)), nil
}
