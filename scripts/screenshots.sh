#!/usr/bin/env bash
###############################################################################
# screenshots.sh — regenerate the README terminal screenshots with `freeze`.
#
# Renders yas' colorized output over a THROWAWAY store seeded with SYNTHETIC
# data. The data MUST be fake: a PNG can't be scanned by the publish safety
# check, so a screenshot of your real history would leak straight to the public
# mirror. Everything here is invented (hosts, paths, commands).
#
#   ./scripts/screenshots.sh            # write into docs/img/
#   ./scripts/screenshots.sh /tmp/shots # write elsewhere (preview)
#
# Needs: freeze (go install github.com/charmbracelet/freeze@latest) and go.
###############################################################################
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/docs/img}"

command -v freeze >/dev/null 2>&1 || {
  echo "screenshots: 'freeze' not found — go install github.com/charmbracelet/freeze@latest" >&2
  exit 1
}

BIN="$(mktemp -d)"; DATA="$(mktemp -d)"
trap 'rm -rf "$BIN" "$DATA"' EXIT

go build -o "$BIN/yas" "$ROOT/cmd/yas"
# CLICOLOR_FORCE makes yas emit ANSI even though freeze captures through a pipe.
# NO_COLOR wins over CLICOLOR_FORCE in yas' color gate, so a caller that exports
# it globally would otherwise get plain (non-color) screenshots — clear it here.
unset NO_COLOR
export PATH="$BIN:$PATH" YAS_DATA_DIR="$DATA" CLICOLOR_FORCE=1

### Seed synthetic data ######################################################
# rec HOST CWD EXIT COMMAND DURATION_MS [AUTHOR]
rec() {
  local args=(record start --command "$4" --cwd "$2" --session demo --shell zsh)
  [[ -n "${6:-}" ]] && args+=(--author "$6")
  local id; id="$(YAS_HOSTNAME="$1" yas "${args[@]}")"
  yas record finish --id "$id" --exit "$3" --duration-ms "$5"
  sleep 0.05
}

rec workstation /work/projects/api 0 "go build ./..."                900
rec workstation /work/projects/api 0 "go test ./..."                3200
rec workstation /work/projects/api 1 "make lint"                     410
rec workstation /work/projects/api 0 "git commit -m 'add limiter'"   120
rec workstation /work/projects/api 0 "rg TODO internal/"              80 claude-code
rec laptop      /work/projects/web 0 "pnpm build"                    5400
rec laptop      /work/projects/web 1 "pnpm test"                     2100
rec laptop      /work/projects/web 0 "git push origin main"           640

### Render ###################################################################
mkdir -p "$OUT"
shot() { # OUTFILE  "yas subcommand…"
  freeze --window --margin 16 --padding 26 --border.radius 8 --border.width 1 \
         --shadow.blur 24 --font.size 14 \
         --execute "$2" --output "$OUT/$1" >/dev/null
  echo "  wrote $OUT/$1"
}
shot history.png "yas history"
shot digest.png  "yas digest"
shot search.png  "yas search git"

echo "screenshots: done -> $OUT"
