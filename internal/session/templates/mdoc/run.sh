#!/usr/bin/env sh
width=$(tmux display-message -t "$TMUX_PANE" -p "#{pane_width}")
mandoc -O "width=$((width-2))" main.0 | ul
