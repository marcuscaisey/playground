package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

// flagSet wraps [flag.FlagSet] and adds the following methods:
//   - [flagSet.PrintDefaults] prints flags in the order they were defined.
//   - [flagSet.StringVarWithEnvVar]
//   - [flagSet.VarWithEnvVar]
//   - [flagSet.CompletionDescriptions]
type flagSet struct {
	base            *flag.FlagSet
	defOrderedFlags []*flag.Flag
	stringFlags     map[string]bool
	flagEnvVars     map[string]string
}

// newFlagSet is the same as [flag.NewFlagSet].
func newFlagSet(name string, errorHandling flag.ErrorHandling) *flagSet {
	return &flagSet{
		base:        flag.NewFlagSet(name, errorHandling),
		stringFlags: map[string]bool{},
		flagEnvVars: map[string]string{},
	}
}

// SetOutput is the same as [flag.FlagSet.SetOutput].
func (fs *flagSet) SetOutput(output io.Writer) { fs.base.SetOutput(output) }

// PrintDefaults is like [flag.FlagSet.PrintDefaults] except flags are printed in the order they
// were defined.
// Adapted from Go's flag.FlagSet.PrintDefaults:
// https://github.com/golang/go/blob/go1.26.5/src/flag/flag.go.
// Copyright 2009 The Go Authors.
// BSD 3-Clause License; see LICENSES/github.com/golang/go/LICENSE.
func (fs *flagSet) PrintDefaults() {
	var isZeroValueErrs []error
	for _, f := range fs.defOrderedFlags {
		var b strings.Builder
		fmt.Fprintf(&b, "  -%s", f.Name) // Two spaces before -; see next two comments.
		name, usage := flag.UnquoteUsage(f)
		if len(name) > 0 {
			b.WriteString(" ")
			b.WriteString(name)
		}
		// Boolean flags of one ASCII letter are so common we
		// treat them specially, putting their usage on the same line.
		if b.Len() <= 4 { // space, space, '-', 'x'.
			b.WriteString("\t")
		} else {
			// Four spaces before the tab triggers good alignment
			// for both 4- and 8-space tab stops.
			b.WriteString("\n    \t")
		}
		b.WriteString(strings.ReplaceAll(usage, "\n", "\n    \t"))

		// Print the default value only if it differs to the zero value
		// for this flag type.
		if isZero, err := isZeroValue(f, f.DefValue); err != nil {
			isZeroValueErrs = append(isZeroValueErrs, err)
		} else if !isZero {
			if usage[len(usage)-1] != '\n' {
				fmt.Fprintf(&b, " ")
			}
			if fs.stringFlags[f.Name] {
				// put quotes on the value
				fmt.Fprintf(&b, "(default %q)", f.DefValue)
			} else {
				fmt.Fprintf(&b, "(default %v)", f.DefValue)
			}
		}
		if envVar, ok := fs.flagEnvVars[f.Name]; ok {
			fmt.Fprintf(&b, " [$%s]", envVar)
		}
		_, _ = fmt.Fprint(fs.base.Output(), b.String(), "\n")
	}
	// If calling String on any zero flag.Values triggered a panic, print
	// the messages after the full set of defaults so that the programmer
	// knows to fix the panic.
	if errs := isZeroValueErrs; len(errs) > 0 {
		_, _ = fmt.Fprintln(fs.base.Output())
		for _, err := range errs {
			_, _ = fmt.Fprintln(fs.base.Output(), err)
		}
	}
}

// isZeroValue determines whether the string represents the zero
// value for a flag.
// Adapted from Go's flag.isZeroValue:
// https://github.com/golang/go/blob/go1.26.5/src/flag/flag.go.
// Copyright 2009 The Go Authors.
// BSD 3-Clause License; see LICENSES/github.com/golang/go/LICENSE.
func isZeroValue(f *flag.Flag, value string) (ok bool, err error) {
	// Build a zero value of the flag's Value type, and see if the
	// result of calling its String method equals the value passed in.
	// This works unless the Value type is itself an interface type.
	typ := reflect.TypeOf(f.Value)
	var z reflect.Value
	if typ.Kind() == reflect.Pointer {
		z = reflect.New(typ.Elem())
	} else {
		z = reflect.Zero(typ)
	}
	// Catch panics calling the String method, which shouldn't prevent the
	// usage message from being printed, but that we should report to the
	// user so that they know to fix their code.
	defer func() {
		if e := recover(); e != nil {
			if typ.Kind() == reflect.Pointer {
				typ = typ.Elem()
			}
			err = fmt.Errorf("panic calling String method on zero %v for flag %s: %v", typ, f.Name, e)
		}
	}()
	return value == z.Interface().(flag.Value).String(), nil
}

// NFlag is the same as [flag.FlagSet.NFlag].
func (fs *flagSet) NFlag() int { return fs.base.NFlag() }

// Arg is the same as [flag.FlagSet.Arg].
func (fs *flagSet) Arg(i int) string { return fs.base.Arg(i) }

// NArg is the same as [flag.FlagSet.NArg].
func (fs *flagSet) NArg() int { return fs.base.NArg() }

// Args is the same as [flag.FlagSet.Args].
func (fs *flagSet) Args() []string { return fs.base.Args() }

// BoolVar is the same as [flag.FlagSet.BoolVar].
func (fs *flagSet) BoolVar(p *bool, name string, value bool, usage string) {
	fs.base.BoolVar(p, name, value, usage)
	fs.defOrderedFlags = append(fs.defOrderedFlags, fs.base.Lookup(name))
}

// Bool is the same as [flag.FlagSet.Bool].
func (fs *flagSet) Bool(name string, value bool, usage string) *bool {
	p := fs.base.Bool(name, value, usage)
	fs.defOrderedFlags = append(fs.defOrderedFlags, fs.base.Lookup(name))
	return p
}

// StringVarWithEnvVar is like [flag.FlagSet.StringVar] but uses the given environment variable as a
// default value when set.
func (fs *flagSet) StringVarWithEnvVar(p *string, name string, envVar string, value string, usage string) {
	fs.base.StringVar(p, name, value, usage)
	*p = cmp.Or(os.Getenv(envVar), value)
	fs.defOrderedFlags = append(fs.defOrderedFlags, fs.base.Lookup(name))
	fs.stringFlags[name] = true
	fs.flagEnvVars[name] = envVar
}

// FuncWithEnvVar is like [flag.FlagSet.Func] but uses the given environment variable as a default
// value when set and valid.
func (fs *flagSet) FuncWithEnvVar(name string, envVar string, usage string, fn func(string) error) {
	fs.base.Func(name, usage, fn)
	if envValue := os.Getenv(envVar); envValue != "" {
		_ = fn(envValue)
	}
	fs.defOrderedFlags = append(fs.defOrderedFlags, fs.base.Lookup(name))
	fs.flagEnvVars[name] = envVar
}

// Var is the same as [flag.FlagSet.Var].
func (fs *flagSet) Var(value flag.Value, name string, usage string) {
	fs.base.Var(value, name, usage)
	fs.defOrderedFlags = append(fs.defOrderedFlags, fs.base.Lookup(name))
}

// VarWithEnvVar is like [flag.FlagSet.Var] but uses the given environment variable as a default
// value when set and valid.
func (fs *flagSet) VarWithEnvVar(value flag.Value, name string, envVar string, usage string) {
	fs.Var(value, name, usage)
	if envValue := os.Getenv(envVar); envValue != "" {
		_ = value.Set(envValue)
	}
	fs.flagEnvVars[name] = envVar
}

// Parse is the same as [flag.FlagSet.Parse].
func (fs *flagSet) Parse(arguments []string) error { return fs.base.Parse(arguments) }

// CompletionDescriptions returns a map from flag name to shell completion description.
// The description is generated by taking usage text and:
//   - Removing the back-quotes from the parameter type (first back-quoted name)
//   - Removing all lines after the first
//   - Extracting the first sentence
//   - Mapping the first letter to lowercase
func (fs *flagSet) CompletionDescriptions() map[string]string {
	descriptions := map[string]string{}
	fs.base.VisitAll(func(f *flag.Flag) {
		_, usage := flag.UnquoteUsage(f)
		usage, _, _ = strings.Cut(usage, "\n")
		usage, _, _ = strings.Cut(usage, ".")
		usage = lowerFirstLetter(usage)
		descriptions[f.Name] = usage
	})
	return descriptions
}

func lowerFirstLetter(s string) string {
	firstLetter, size := utf8.DecodeRuneInString(s)
	if firstLetter == utf8.RuneError && size <= 1 {
		return s
	}
	firstLetterLower := unicode.ToLower(firstLetter)
	if firstLetterLower == firstLetter {
		return s
	}
	return string(firstLetterLower) + s[size:]
}
