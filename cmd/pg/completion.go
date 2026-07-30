package main

import (
	"cmp"
	_ "embed"
	"fmt"
	"slices"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/marcuscaisey/playground/internal/session"
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
	//go:embed completion/_pg.tmpl
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
		return "", fmt.Errorf("completionScript: invalid shell %q", shell)
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
// For zsh, the format is that expected by the zshcompsys _describe command name1 argument.
// For fish, the format is that expected by the complete command --arguments flag.
func completeTemplates(userTemplatesDir string, shell shell) error {
	templates, err := session.AllTemplates(userTemplatesDir)
	if err != nil {
		return fmt.Errorf("completing templates: %s", err)
	}
	for _, template := range templates {
		description := "user"
		if template.IsBuiltin {
			description = "built-in"
		}
		fmt.Println(formatCompletion(template.Name, description, shell))
	}
	return nil
}

// completeSessions prints all session names for a given template in descending last opened order,
// one per line.
// For zsh, the format is that expected by the zshcompsys _describe command name1 argument.
// For fish, the format is that expected by the complete command --arguments flag.
func completeSessions(templateName string, sessionsDir string, shell shell) error {
	sessions, err := session.TemplateSessions(templateName, sessionsDir)
	if err != nil {
		return fmt.Errorf("completing sessions: %s", err)
	}
	slices.SortFunc(sessions, func(a session.Info, b session.Info) int {
		if c := a.LastOpened.Compare(b.LastOpened); c != 0 {
			return -c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	for _, session := range sessions {
		var description string
		if !session.LastOpened.IsZero() {
			sinceLastOpen := time.Since(session.LastOpened)
			description = fmt.Sprintf("%s ago", humanDuration(sinceLastOpen))
		} else {
			description = "unknown"
		}
		fmt.Println(formatCompletion(session.Name, description, shell))
	}
	return nil
}

// humanDuration returns a human-readable approximation of a duration (eg. "About a minute",
// "4 hours ago", etc.).
// Copied from github.com/docker/go-units.
func humanDuration(d time.Duration) string {
	if seconds := int(d.Seconds()); seconds < 1 {
		return "Less than a second"
	} else if seconds == 1 {
		return "1 second"
	} else if seconds < 60 {
		return fmt.Sprintf("%d seconds", seconds)
	} else if minutes := int(d.Minutes()); minutes == 1 {
		return "About a minute"
	} else if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	} else if hours := int(d.Hours() + 0.5); hours == 1 {
		return "About an hour"
	} else if hours < 48 {
		return fmt.Sprintf("%d hours", hours)
	} else if hours < 24*7*2 {
		return fmt.Sprintf("%d days", hours/24)
	} else if hours < 24*30*2 {
		return fmt.Sprintf("%d weeks", hours/24/7)
	} else if hours < 24*365*2 {
		return fmt.Sprintf("%d months", hours/24/30)
	}
	return fmt.Sprintf("%d years", int(d.Hours())/24/365)
}

// formatCompletion formats a completion for the given shell.
// value is the proposed text snippet, this is always shown.
// description describes the snippet, this is only shown for supporting shells.
// For zsh, the format is that expected by the zshcompsys _describe command name1 argument.
// For fish, the format is that expected by the complete command --arguments flag.
// For other shells, value is returned unchanged.
func formatCompletion(value string, description string, shell shell) string {
	switch shell {
	case shellZsh:
		value := strings.ReplaceAll(value, ":", `\:`)
		// Empty description results in an empty description shown. No description results in no
		// description shown.
		if description != "" {
			return value + ":" + description
		} else {
			return value
		}
	case shellFish:
		value := strings.ReplaceAll(value, "\t", strings.Repeat(" ", 4))
		return value + "\t" + description
	case shellBash:
	default:
	}
	return value
}
