# yas zsh integration — two-phase capture.
#
#   source /path/to/yas.zsh
#
# preexec fires with the command line just before it runs (we record the start
# and stash the new record id); precmd fires before the next prompt (we read the
# exit status first thing and finalize the record). This captures command, cwd,
# exit code, duration, and a per-shell session id — without touching Ctrl-R.
#
# Recording is synchronous but tiny (one local SQLite insert), so history is
# captured even when the server is unreachable. Requires `yas` on PATH; if it
# is missing the hooks no-op so your shell still works.
#
# Pause capture in the current shell with `yas-pause` and turn it back on with
# `yas-resume` (`yas-status` shows the state). The pause is a per-shell
# variable, so it clears when the terminal exits; `export YAS_PAUSED=1` silences
# a whole terminal or a script.
#
# Set YAS_EXECUTOR to self-tag who ran these commands (default "human"); an
# agent wrapper can export e.g. YAS_EXECUTOR=claude-code so its commands are
# queryable as agent activity. This is the generic, non-rotting tagging seam.

# EPOCHSECONDS / EPOCHREALTIME come from zsh/datetime; load it so the hook is
# self-contained even if the interactive config doesn't already.
zmodload zsh/datetime 2>/dev/null

# One session id per interactive shell (host-pid-epoch is unique enough).
typeset -g YAS_SESSION="${HOST}-$$-${EPOCHSECONDS}"
# Exported so a `yas history`/`search` child can exclude its own in-flight
# record (the command currently printing the listing) from the output.
typeset -gx YAS_RECORD_ID=""
typeset -g YAS_START=""
# Non-empty pauses capture for this shell. Honor an inherited/exported value so
# `export YAS_PAUSED=1` (or a parent shell) silences this terminal from the start.
typeset -g YAS_PAUSED="${YAS_PAUSED:-}"

_yas_has() { command -v yas >/dev/null 2>&1 }

_yas_preexec() {
    _yas_has || return
    [[ -n "$YAS_PAUSED" ]] && return   # capture paused for this shell
    YAS_START="${EPOCHREALTIME}"
    # `yas record start` prints the new record id on stdout.
    YAS_RECORD_ID="$(yas record start \
        --command "$1" \
        --cwd "$PWD" \
        --session "$YAS_SESSION" \
        --author "${YAS_EXECUTOR:-human}" \
        --shell zsh 2>/dev/null)"
}

_yas_precmd() {
    local exit=$?               # MUST be the first statement
    _yas_has || return
    [[ -n "$YAS_RECORD_ID" ]] || return
    # Build the optional --duration-ms as an array: zsh does NOT word-split
    # unquoted ${x:+a b} the way bash does, so a flag+value must be array elements.
    local -a dur_arg
    if [[ -n "$YAS_START" ]]; then
        # (EPOCHREALTIME - start) seconds -> integer milliseconds
        local dur_ms=$(( (EPOCHREALTIME - YAS_START) * 1000 ))
        dur_ms=${dur_ms%.*}
        [[ -n "$dur_ms" ]] && dur_arg=(--duration-ms "$dur_ms")
    fi
    yas record finish \
        --id "$YAS_RECORD_ID" \
        --exit "$exit" \
        "${dur_arg[@]}" >/dev/null 2>&1
    YAS_RECORD_ID=""
    YAS_START=""
}

autoload -Uz add-zsh-hook
add-zsh-hook preexec _yas_preexec
add-zsh-hook precmd _yas_precmd

# --- pause / resume capture for this shell ---------------------------------
# These are shell functions (not `yas` subcommands) because only the shell can
# flip state for the running shell — a subprocess can't change its parent's env.

yas-pause() {
    YAS_PAUSED=1
    print -r -- "yas: tracking paused for this shell (yas-resume to re-enable)"
}

yas-resume() {
    YAS_PAUSED=""
    print -r -- "yas: tracking resumed for this shell"
}

yas-status() {
    if [[ -n "$YAS_PAUSED" ]]; then
        print -r -- "yas: tracking paused (this shell)"
    else
        print -r -- "yas: tracking active (this shell)"
    fi
}
