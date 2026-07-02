# yas-fzf.zsh — bind Ctrl-R to a fuzzy search over yas's (cross-machine) history.
#
#   source /path/to/yas-fzf.zsh
#
# This is the HUMAN recall seam: your existing fzf UX, but the candidate list
# comes from yas instead of $HISTFILE — so Ctrl-R searches every command from
# every synced machine. Source it AFTER the fzf plugin so this ^R binding wins.
# It reuses your $FZF_CTRL_R_OPTS / $FZF_DEFAULT_OPTS verbatim.
#
# Deliberately scoped to Ctrl-R only: the up arrow stays vanilla zsh (yas never
# touches it). Anything fancier than this thin shim is a different client's job.
#
# Tunables:
#   YAS_FZF_LIMIT     how many recent records to pull as candidates (default 5000)
#   YAS_FZF_EXECUTOR  recall scope; default '$all-human' hides agent-run commands.
#                     Set to '' to include everything, or a name (e.g. claude-code).

command -v yas >/dev/null 2>&1 || return

: ${YAS_FZF_LIMIT:=5000}
# No-colon default: assign only when UNSET, so an explicit empty value means
# "no executor filter" (recall everything) rather than being reset to the default.
: ${YAS_FZF_EXECUTOR='$all-human'}

# _yas_fzf_source emits recall candidates NUL-separated, de-duplicated,
# newest-first. NUL separation (jq --raw-output0 + gawk RS=NUL) keeps multiline
# commands intact as single candidates. Factored out so it is testable without
# an interactive fzf.
_yas_fzf_source() {
    command -v jq >/dev/null 2>&1 || return
    # Build the optional flag as an array: zsh does NOT word-split an unquoted
    # ${x:+--flag "$v"} into two words the way bash does, so a flag+value must be
    # separate array elements (same gotcha the capture hook documents).
    local -a exec_arg
    [[ -n "$YAS_FZF_EXECUTOR" ]] && exec_arg=(--executor "$YAS_FZF_EXECUTOR")
    yas search "${exec_arg[@]}" --limit "$YAS_FZF_LIMIT" --json 2>/dev/null \
        | jq --raw-output0 '.records[].command' \
        | awk 'BEGIN { RS = "\0"; ORS = "\0" } length($0) && !seen[$0]++'
}

# The interactive widget only exists when fzf is available; the source function
# above works regardless (and is what tests exercise).
if command -v fzf >/dev/null 2>&1; then
    _yas_ctrl_r() {
        local sel
        sel="$(
            _yas_fzf_source \
            | FZF_DEFAULT_OPTS="${FZF_DEFAULT_OPTS} ${FZF_CTRL_R_OPTS}" \
              fzf --read0 --print0 --query "$LBUFFER" +m
        )" || true
        if [[ -n "$sel" ]]; then
            BUFFER="${sel%$'\0'}"   # drop the trailing NUL from --print0
            CURSOR=${#BUFFER}
        fi
        zle reset-prompt
    }
    zle -N _yas_ctrl_r
    bindkey '^R' _yas_ctrl_r
fi
