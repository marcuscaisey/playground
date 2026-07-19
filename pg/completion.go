package main

import (
	_ "embed"
	"fmt"
	"strings"
	texttemplate "text/template"
)

// shell is a shell that completions can be generated for.
type shell int

const (
	shellsStart shell = iota

	shellBash
	shellZsh
	shellFish

	shellsEnd
)

func (s shell) String() string {
	switch s {
	case shellBash:
		return "bash"
	case shellZsh:
		return "zsh"
	case shellFish:
		return "fish"
	default:
		return fmt.Sprintf("shell(%d)", s)
	}
}

// Set implements [flag.Value].
func (s *shell) Set(value string) error {
	allShells := allShells()
	shellStrings := make([]string, len(allShells))
	for i, shell := range allShells {
		if shell.String() == value {
			*s = shell
			return nil
		}
		shellString := shell.String()
		if i == len(allShells)-1 {
			shellString = fmt.Sprintf("or %s", shellString)
		}
		shellStrings[i] = shellString
	}
	return fmt.Errorf("must be one of %s", strings.Join(shellStrings, ", "))
}

// allShells returns all possible [shell] values.
func allShells() []shell {
	numShells := shellsEnd - shellsStart - 1
	shells := make([]shell, numShells)
	for i := range numShells {
		shells[i] = shell(shellsStart + 1 + i)
	}
	return shells
}

var (
	//go:embed completion/pg.bash.tmpl
	bashCompletionScriptTmpl string
	//go:embed completion/pg.zsh.tmpl
	zshCompletionScriptTmpl string
	//go:embed completion/pg.fish.tmpl
	fishCompletionScriptTmpl string
)

// completionScript returns the completion script for the given shell.
func completionScript(flagDescriptions map[string]string, shell shell) (string, error) {
	var scriptTemplate string
	switch shell {
	case shellBash:
		scriptTemplate = bashCompletionScriptTmpl
	case shellZsh:
		scriptTemplate = zshCompletionScriptTmpl
	case shellFish:
		scriptTemplate = fishCompletionScriptTmpl
	default:
		panic(fmt.Sprintf("completionScript() not implemented for shell %q", shell))
	}
	tmpl, err := texttemplate.New("script").Option("missingkey=error").Parse(scriptTemplate)
	if err != nil {
		return "", fmt.Errorf("generating completion script for shell %q: %s", shell, err)
	}

	data := map[string]string{}
	for flag, description := range flagDescriptions {
		key := fmt.Sprintf("%s_description", strings.ReplaceAll(flag, "-", "_"))
		escapedDescription := strings.ReplaceAll(description, `"`, `\"`)
		data[key] = escapedDescription
	}
	allShells := allShells()
	shellStrings := make([]string, len(allShells))
	for i, shell := range allShells {
		shellStrings[i] = shell.String()
	}
	data["completion_script_shell_values"] = strings.Join(shellStrings, " ")

	b := new(strings.Builder)
	if err := tmpl.Execute(b, data); err != nil {
		return "", fmt.Errorf("generating completion script for shell %q: %s", shell, err)
	}
	return b.String(), nil
}

// completeTemplates prints all available template names, one per line.
// When shell is zsh, the template names are escaped so that the output can be safely used with the
// zsh _describe function.
// When shell is fish, the template names are escaped so that the output can be safely used with the
// fish complete function.
func completeTemplates(userTemplatesDir string, shell shell) error {
	names, err := listTemplateNames(userTemplatesDir)
	if err != nil {
		return fmt.Errorf("completing templates: %s", err)
	}
	printCompletions(names, shell)
	return nil
}

// completeSessions prints all session names for a given template, one per line.
// When shell is zsh, the template names are escaped so that the output can be safely used with the
// zsh _describe function.
// When shell is fish, the template names are escaped so that the output can be safely used with the
// fish complete function.
func completeSessions(templateName string, sessionsDir string, shell shell) error {
	names, err := listSessionNames(templateName, sessionsDir)
	if err != nil {
		return fmt.Errorf("completing sessions: %s", err)
	}
	printCompletions(names, shell)
	return nil
}

func printCompletions(completions []string, shell shell) {
	for _, completion := range completions {
		switch shell {
		case shellZsh:
			// : is the separator between the completion and its description
			completion = strings.ReplaceAll(completion, ":", `\:`)
		case shellFish:
			// \t is the separator between the completion and its description
			completion = strings.ReplaceAll(completion, "\t", strings.Repeat(" ", 4))
		}
		fmt.Println(completion)
	}
}
