package main

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
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
// If the loaded template is invalid, the returned error wraps [*templateInvalidError].
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

	return template{
		Name:       name,
		IsBuiltin:  isBuiltin,
		FS:         templateFS,
		Entrypoint: entrypoint,
	}, nil
}

// listTemplateNames returns all distinct built-in and user template names in alphabetical order.
func listTemplateNames(userTemplatesDir string) ([]string, error) {
	uniqueNames := map[string]bool{}

	builtinTemplateDirs, err := fs.ReadDir(builtinTemplatesFS, builtinTemplatesDirName)
	if err != nil {
		return nil, fmt.Errorf("listing template names: reading built-in templates: %s", err)
	}
	for _, dirEntry := range builtinTemplateDirs {
		uniqueNames[dirEntry.Name()] = true
	}

	userTemplateDirs, err := os.ReadDir(userTemplatesDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("listing template names: reading user templates: %s", err)
	}
	for _, dirEntry := range userTemplateDirs {
		uniqueNames[dirEntry.Name()] = true
	}

	return slices.Sorted(maps.Keys(uniqueNames)), nil
}
