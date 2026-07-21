package main

import (
	"bufio"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

//go:embed templates/*
var builtinTemplatesFS embed.FS

const builtinTemplatesDirName = "templates"

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
	IsBuiltin           bool   // Whether the template is built-in
	FS                  fs.FS  // File system containing the template's files
	Entrypoint          string // Entrypoint filename, such as "main.go"
	EntrypointStartLine int    // Line where the entrypoint should be opened
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
	if t.Name == "go" && t.IsBuiltin {
		path := filepath.Join(dir, "go.mod")
		data := []byte("module playground\n")
		if err := os.WriteFile(path, data, 0o666); err != nil {
			return fmt.Errorf("setting up session directory: writing built-in go template go.mod: %s", err)
		}
	}
	return nil
}

var errTemplateNotFound = fmt.Errorf("not found")

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
// If the loaded template is invalid, the returned error wraps [templateInvalidError].
func loadTemplate(name string, userTemplatesDir string) (template, error) {
	isBuiltin := false
	templatePath := ""
	var templateFS fs.FS
	userTemplatePath := filepath.Join(userTemplatesDir, name)
	if ok, err := fileExists(userTemplatePath); err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	} else if ok {
		templateFS = os.DirFS(userTemplatePath)
		templatePath = userTemplatePath
	} else if !ok {
		templateFS, err = fs.Sub(builtinTemplatesFS, path.Join(builtinTemplatesDirName, name))
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
		templateInvalidErr := newTemplateInvalidErrorf(name, templatePath, "entrypoint (%q file) is missing", entrypointPattern)
		return template{}, fmt.Errorf("loading template %q: %w", name, templateInvalidErr)
	default:
		quotedEntrypoints := make([]string, len(entrypoints))
		for i, entrypoint := range entrypoints {
			quotedEntrypoints[i] = strconv.Quote(entrypoint)
		}
		templateInvalidErr := newTemplateInvalidErrorf(name, templatePath, "multiple entrypoints: %s", strings.Join(quotedEntrypoints, ", "))
		return template{}, fmt.Errorf("loading template %q: %w", name, templateInvalidErr)
	}

	if ok, err := fileExistsFS(templateFS, runScriptFilename); err != nil {
		return template{}, fmt.Errorf("loading template %q: %s", name, err)
	} else if !ok {
		templateInvalidErr := newTemplateInvalidErrorf(name, templatePath, "run script (%q file) is missing", runScriptFilename)
		return template{}, fmt.Errorf("loading template %q: %w", name, templateInvalidErr)
	}

	f, err := templateFS.Open(entrypoint)
	if err != nil {
		return template{}, fmt.Errorf("loading template %q: reading entrypoint start line: %s", name, err)
	}
	defer f.Close() // nolint:errcheck // There's nothing we can do with this error
	scanner := bufio.NewScanner(f)
	entrypointStartLine := 0
	line := 1
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
		IsBuiltin:           isBuiltin,
		FS:                  templateFS,
		Entrypoint:          entrypoint,
		EntrypointStartLine: entrypointStartLine,
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

	builtinTemplateDirs, err := fs.ReadDir(builtinTemplatesFS, builtinTemplatesDirName)
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
