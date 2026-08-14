#!/usr/bin/env sh
#
# Installs a pg release from GitHub

set -eu

#######################################
# Prints an error message and exits with status 1
# Arguments:
#     $1: error message
#######################################
error() {
    printf 'error: %b\n' "$1" >&2
    exit 1
}

script_name=$0

#######################################
# Prints an optional error message followed by the usage text and exits with 2
# Arguments:
#     $1: optional error message
#######################################
usage_error() {
    [ -n "${1+x}" ] && printf 'error: %b\n' "$1" >&2
    printf 'Usage: %s [-d directory] [-v version]\n' "$script_name" >&2
    printf 'Options:\n' >&2
    printf '  -d directory\n' >&2
    printf '    \tinstall directory (default "/usr/local")\n' >&2
    printf '  -v version\n' >&2
    printf '    \tversion to install (default "latest")\n' >&2
    exit 2
}

install_dir=/usr/local
version=
while getopts v:d: name; do
    case $name in
    d)
        install_dir=$OPTARG
        ;;
    v)
        version=$OPTARG
        ;;
    ?)
        usage_error
        ;;
    esac
done
shift $((OPTIND - 1))
[ $# -gt 0 ] && usage_error "unexpected arguments: $*"

for cmd in uname curl mktemp tar install; do
    command -v $cmd >/dev/null || error "need '$cmd' (command not found)"
done

issue_msg="Open an issue to request support:\n  https://github.com/marcuscaisey/playground/issues/new"
os=$(uname -s)
case $os in
Darwin)
    goos=darwin
    ;;
Linux)
    goos=linux
    ;;
*)
    error "unsupported operating system: $os\n$issue_msg"
    ;;
esac
arch=$(uname -m)
case $arch in
arm64 | aarch64)
    goarch=arm64
    ;;
x86_64 | amd64)
    goarch=amd64
    ;;
*)
    error "unsupported architecture: $arch\n$issue_msg"
    ;;
esac

releases_url="https://github.com/marcuscaisey/playground/releases"

if [ -z "$version" ]; then
    latest_url=$(
        curl \
            --location \
            --fail \
            --silent \
            --show-error \
            --output /dev/null \
            --write-out '%{url_effective}' \
            "$releases_url/latest"
    )
    version=${latest_url##*/}
    version=${version#v}
    [ -n "$version" ] || error "could not determine latest version"
fi

archive="pg_${version}_${goos}_${goarch}.tar.gz"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

printf 'Downloading pg %s\n' "$version"
url="$releases_url/download/v${version}/${archive}"
curl \
    --location \
    --fail \
    --silent \
    --show-error \
    --remote-name \
    --output-dir "$tmpdir" \
    "$url" || error "failed to download release archive from:\n  $url"

tar -xzf "$tmpdir/$archive" -C "$tmpdir"

bin_dir="$install_dir/bin"
man_dir="$install_dir/share/man/man1"
bash_completions_dir="$install_dir/share/bash-completion/completions"
zsh_completions_dir="$install_dir/share/zsh/site-functions"
fish_completions_dir="$install_dir/share/fish/vendor_completions.d"
install -d -m 755 "$bin_dir" "$man_dir" "$bash_completions_dir" "$zsh_completions_dir" "$fish_completions_dir"

printf 'Installing pg to %s\n' "$bin_dir"
install -m 755 "$tmpdir/pg" "$bin_dir"
printf 'Installing man page to %s\n' "$man_dir"
install -m 644 "$tmpdir/docs/pg.1" "$man_dir"
printf 'Installing bash completions to %s\n' "$bash_completions_dir"
install -m 644 "$tmpdir/completions/pg.bash" "$bash_completions_dir"
printf 'Installing zsh completions to %s\n' "$zsh_completions_dir"
install -m 644 "$tmpdir/completions/_pg" "$zsh_completions_dir"
printf 'Installing fish completions to %s\n' "$fish_completions_dir"
install -m 644 "$tmpdir/completions/pg.fish" "$fish_completions_dir"

printf 'Installation complete\n'
