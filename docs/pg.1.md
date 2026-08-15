PG(1) - General Commands Manual

# NAME

**pg** - run a coding playground in your terminal

# SYNOPSIS

**pg**
\[options]
*template*
\[*session*]  
**pg**
**-completion-script**&nbsp;*shell*  
**pg**
**-version**  
**pg**
**-help**

# DESCRIPTION

Playground
(**pg**)
runs a coding playground in your terminal.

A coding
*playground*
is an environment for quickly writing and executing a program in any language.
The program is opened in your editor and automatically executed on save.
The output is displayed in a separate
tmux(1)
pane
(the *output pane*)
, under or to the side of the editor.
When
**pg**
is not started from within tmux, it creates a temporary tmux session.

When
**pg**
is started, it opens a
*session*
&#8212; a playground created from the
*template*
specified by the
*template*
argument.
If the
*session*
argument is given, a
*named*
session is created or resumed if it already exists.
Otherwise, an
*anonymous*
session is created which can be saved after it ends.
The session ends when the editor is closed.
Sessions are created in the
*sessions directory*
,
which defaults to
*$XDG\_DATA\_HOME/pg/sessions*
(or
*$HOME/.local/share/pg/sessions*
if $XDG\_DATA\_HOME is not set).
See the
*OPTIONS*
section for all of the options that
**pg**
accepts.

A template is a directory containing the files used to run a session.
When a session is created, the contents of the template are copied into the session's working directory
(the *session directory*)
.
**pg**
provides a number of built-in templates but you can use your own by adding them to the
*user templates directory*
*$XDG\_CONFIG\_HOME/pg/templates*
(or
*$HOME/.config/pg/templates*
if $XDG\_CONFIG\_HOME is not set).
User templates override built-in templates with the same name.
The structure of a template is described in the
*TEMPLATE STRUCTURE*
section.

The built-in templates are: bash, c, dart, fish, go, java, lua, mdoc, node, php, python, rust, sh, typescript, zsh.

## TEMPLATE STRUCTURE

A template must contain at least the following files:

main.\*

> The
> *entrypoint*
> opened when the session starts.
> Exactly one file matching this pattern must exist.
> If \_\_CURSOR\_\_ appears anywhere on a line, then the contents of the line (excluding leading whitespace) are erased when the file is copied into the session directory and, if possible, the cursor is placed on it when the entrypoint is opened.
> All \_\_CURSOR\_\_ appearances after the first one are ignored.

run.sh

> The
> *run script*
> ,
> executed as
> '`./run.sh`'
> from the session directory when the program is saved.

See the built-in templates for some examples:
[https://github.com/marcuscaisey/playground/blob/main/internal/session/templates](https://github.com/marcuscaisey/playground/blob/main/internal/session/templates)
.

## OPTIONS

**pg**
accepts the following options:

**-completion-script** *shell*

	Generate a shell completion script for bash, zsh, or fish.

**-editor** *command*

	Command to open the editor.
	For nvim, vim, vi, emacs, helix, kakoune, nano, and pico, the template entrypoint is opened at the start line defined by the template.
	Defaults to $EDITOR if set, otherwise vi.

**-help**

	Print a help message.

**-output-pane-size** *size*

	Output pane size in lines/columns, or a percentage if followed by '%'.
	Defaults to 35%.

**-sessions-dir** *directory*

	Directory where sessions are created.
	Defaults to $PG_SESSIONS_DIR if set, otherwise
	*$XDG_DATA_HOME/pg/sessions*
	.

**-version**

	Print the version of
	**pg**
	.

**-vertical**

	Split the window vertically instead of horizontally to create the output pane.
	Defaults to $PG_VERTICAL if set.
	Accepted values: 1, t, T, TRUE, true, True, 0, f, F, FALSE, false, False.

# EXAMPLES

To start an anonymous Go session:

> $ pg go

To start or resume a Python session named banana:

> $ pg python banana

To move the output pane to the side and have it take up half the screen:

> $ pg -vertical -output-pane-size 50% python

To configure bash tab completion, add the following to
*~/.bashrc*
:

> source <(pg -completion-script bash)

To configure zsh tab completion, add the following to
*~/.zshrc*
:

> source <(pg -completion-script zsh)

To configure fish tab completion, add the following to
*~/.config/fish/config.fish*
:

> pg -completion-script fish | source

# SEE ALSO

tmux(1)

# AUTHORS

Marcus Caisey <marcus@teckna.com>

macOS 26.4 - August 11, 2026
