#!/usr/bin/env bash
# yas installer: builds the cgo-free static agent from source and installs it,
# then stages the zsh integration files. Requires Go (version per go.mod; the
# Go toolchain auto-fetches a newer one if needed). Never edits your ~/.zshrc —
# it prints the two source lines to add instead.
#
#   ./scripts/install.sh                 # yas -> ~/.local/bin, hooks -> ~/.config/yas
#   BINDIR=/usr/local/bin ./scripts/install.sh
#   ./scripts/install.sh --with-server   # also install yas-server (sync hub)
#   ./scripts/install.sh --no-shell      # skip the zsh hook staging
set -euo pipefail

###############################################################################
# options
###############################################################################
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINDIR="${BINDIR:-$HOME/.local/bin}"
CONFDIR="${CONFDIR:-${XDG_CONFIG_HOME:-$HOME/.config}/yas}"
WITH_SERVER=0
WITH_SHELL=1

for arg in "$@"; do
    case "$arg" in
        --with-server) WITH_SERVER=1 ;;
        --no-shell)    WITH_SHELL=0 ;;
        -h|--help)     sed -n '2,10p' "${BASH_SOURCE[0]}"; exit 0 ;;
        *) echo "install.sh: unknown option $arg (see --help)" >&2; exit 2 ;;
    esac
done

command -v go >/dev/null 2>&1 || {
    echo "install.sh: Go is required to build yas (https://go.dev/dl/)" >&2
    exit 1
}

###############################################################################
# build + install binaries (static, cgo-free, version-stamped)
###############################################################################
VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
mkdir -p "$BINDIR"

echo "building yas $VERSION (CGO_ENABLED=0) -> $BINDIR/yas"
CGO_ENABLED=0 go build -C "$ROOT" -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$BINDIR/yas" ./cmd/yas

if [[ "$WITH_SERVER" == 1 ]]; then
    echo "building yas-server $VERSION -> $BINDIR/yas-server"
    CGO_ENABLED=0 go build -C "$ROOT" -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" -o "$BINDIR/yas-server" ./cmd/yas-server
fi

###############################################################################
# stage the zsh integration (copied, not sourced-in-place, so the repo clone
# can be deleted after install)
###############################################################################
if [[ "$WITH_SHELL" == 1 ]]; then
    mkdir -p "$CONFDIR"
    install -m 0644 "$ROOT/shell/yas.zsh" "$CONFDIR/hook.zsh"
    install -m 0644 "$ROOT/shell/yas-fzf.zsh" "$CONFDIR/ctrl-r.zsh"
    echo "staged zsh hooks -> $CONFDIR/{hook.zsh,ctrl-r.zsh}"
    echo
    echo "add to ~/.zshrc (capture hook; Ctrl-R line goes AFTER any fzf setup):"
    echo "  [[ -f $CONFDIR/hook.zsh ]] && source $CONFDIR/hook.zsh"
    echo "  [[ -f $CONFDIR/ctrl-r.zsh ]] && source $CONFDIR/ctrl-r.zsh"
fi

case ":$PATH:" in
    *":$BINDIR:"*) ;;
    *) echo "note: $BINDIR is not on your PATH" ;;
esac

echo "done. try: yas version && yas import --from zsh-history"
