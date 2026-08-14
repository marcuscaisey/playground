#!/usr/bin/env sh
#
# Uninstalls files installed by install.sh

set -eu

script_name=$0

#######################################
# Prints an optional error message followed by the usage text and exits with 2
# Arguments:
#     $1: optional error message
#######################################
usage_error() {
    [ -n "${1+x}" ] && printf 'error: %b\n' "$1" >&2
    printf 'Usage: %s [-d directory]\n' "$script_name" >&2
    printf 'Options:\n' >&2
    printf '  -d directory\n' >&2
    printf '    \tinstall directory (default "/usr/local")\n' >&2
    exit 2
}

install_dir=/usr/local
while getopts d: name; do
    case $name in
    d)
        install_dir=$OPTARG
        ;;
    ?)
        usage_error
        ;;
    esac
done
shift $((OPTIND - 1))
[ $# -gt 0 ] && usage_error "unexpected arguments: $*"

printf 'Uninstalling pg from %s\n' "$install_dir"

set -- \
    "$install_dir/bin/pg" \
    "$install_dir/share/man/man1/pg.1" \
    "$install_dir/share/bash-completion/completions/pg.bash" \
    "$install_dir/share/zsh/site-functions/_pg" \
    "$install_dir/share/fish/vendor_completions.d/pg.fish"

for file; do
    printf 'Removing %s\n' "$file"
    rm -f "$file"
done

printf 'Uninstallation complete\n'
