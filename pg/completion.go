package main

import (
	"cmp"
	_ "embed"
	"fmt"
	"slices"
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

// completion represents a text snippet proposed to complete text being typed.
type completion struct {
	Value       string // Proposed text snippet; always shown
	Description string // Description of snippet; only shown for supporting shells
}

// Format formats the completion for the given shell.
// For zsh, the format is that expected by the zshcompsys _describe command name1 argument.
// For fish, the format is that expected by the complete command --arguments flag.
// For other shell, the value is returned unchanged.
func (c completion) Format(shell shell) string {
	switch shell {
	case shellZsh:
		value := strings.ReplaceAll(c.Value, ":", `\:`)
		// Empty description results in an empty description shown. No description results in no
		// description shown.
		if c.Description != "" {
			return value + ":" + c.Description
		} else {
			return value
		}
	case shellFish:
		value := strings.ReplaceAll(c.Value, "\t", strings.Repeat(" ", 4))
		return value + "\t" + c.Description
	default:
		return c.Value
	}
}

// completeTemplates prints all available template names, one per line.
// For zsh, the format is that expected by the zshcompsys _describe command name1 argument.
// For fish, the format is that expected by the complete command --arguments flag.
func completeTemplates(userTemplatesDir string, shell shell) error {
	templates, err := listTemplates(userTemplatesDir)
	if err != nil {
		return fmt.Errorf("completing templates: %s", err)
	}
	slices.SortFunc(templates, func(a templateInfo, b templateInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})
	for _, template := range templates {
		description := "user"
		if template.IsBuiltin {
			description = "built-in"
		}
		completion := completion{Value: template.Name, Description: description}
		fmt.Println(completion.Format(shell))
	}
	return nil
}

// completeSessions prints all session names for a given template, one per line.
// For zsh, the format is that expected by the zshcompsys _describe command name1 argument.
// For fish, the format is that expected by the complete command --arguments flag.
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
