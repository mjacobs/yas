#!/usr/bin/env bash
# End-to-end smoke test: record -> search -> serve -> curl, including a
# concurrent write while `yas serve` holds the DB open (the WAL multi-process
# property). Uses a throwaway data dir. Requires: go, curl, jq.
set -euo pipefail

###############################################################################
# setup
###############################################################################
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ADDR="${YAS_SMOKE_ADDR:-127.0.0.1:8799}"
WORK="$(mktemp -d)"
BIN="$WORK/yas"
export YAS_DATA_DIR="$WORK/data"
SERVE_PID=""

cleanup() {
    [[ -n "$SERVE_PID" ]] && kill "$SERVE_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

for tool in curl jq; do
    command -v "$tool" >/dev/null || { echo "smoke: '$tool' is required" >&2; exit 1; }
done

assert_eq() { # assert_eq <desc> <actual> <expected>
    if [[ "$2" != "$3" ]]; then
        echo "  FAIL: $1 (got '$2', want '$3')" >&2
        exit 1
    fi
    echo "  ok: $1"
}

###############################################################################
# record
###############################################################################
echo "### build"
( cd "$ROOT" && "${GO:-go}" build -o "$BIN" ./cmd/yas )

echo "### record three commands"
for c in "git status" "docker ps -a" "go test ./..."; do
    id="$("$BIN" record start --command "$c" --cwd "$ROOT" --session smoke --shell zsh)"
    "$BIN" record finish --id "$id" --exit 0 --duration-ms 5
done
echo "### yas search (human)"
"$BIN" search

###############################################################################
# serve + query API
###############################################################################
echo "### serve"
"$BIN" serve --addr "$ADDR" 2>/dev/null &
SERVE_PID=$!
curl -fsS --retry 20 --retry-connrefused --retry-delay 1 "http://$ADDR/v1/healthz" >/dev/null
echo "  ok: healthz"

docker_count="$(curl -fsS "http://$ADDR/v1/search?q=docker" | jq '.records | length')"
assert_eq "search q=docker -> 1 result" "$docker_count" "1"

echo "### concurrent write while serving (WAL multi-process)"
id="$("$BIN" record start --command "echo concurrent-write" --cwd /tmp --session smoke --shell zsh)"
"$BIN" record finish --id "$id" --exit 0 --duration-ms 1
conc_count="$(curl -fsS "http://$ADDR/v1/search?q=concurrent" | jq '.records | length')"
assert_eq "new record visible via API" "$conc_count" "1"

echo "### contract checks"
empty_body="$(curl -fsS "http://$ADDR/v1/search?q=nomatchxyz")"
case "$empty_body" in
    *'"records":[]'*) echo "  ok: empty result is []" ;;
    *) echo "  FAIL: empty result not [] -> $empty_body" >&2; exit 1 ;;
esac
assert_eq "bad param -> 400" \
    "$(curl -s -o /dev/null -w '%{http_code}' "http://$ADDR/v1/search?exit=abc")" "400"
assert_eq "non-GET -> 405" \
    "$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://$ADDR/v1/search")" "405"

###############################################################################
# yas history (bash-style listing + result tracking + delete) — also writes
# while serve holds the DB open, exercising the WAL multi-process property.
###############################################################################
echo "### record a failing command (result tracking)"
fid="$("$BIN" record start --command "false" --cwd "$ROOT" --session smoke --shell zsh)"
"$BIN" record finish --id "$fid" --exit 1 --duration-ms 1

echo "### yas history"
# 5 records: git status, docker ps -a, go test ./..., echo concurrent-write, false.
hist_count="$("$BIN" history --json | jq '.records | length')"
assert_eq "history lists all 5" "$hist_count" "5"

# Capture full listings once. Piping `yas` straight into head/grep lets the
# reader close the pipe early, which SIGPIPEs yas and trips `set -o pipefail`;
# reading from a captured string (here-string) avoids that.
# --no-session keeps these result/ordering checks focused on the columns they
# assert; the SESS token column gets its own coverage below.
hist_full="$("$BIN" history --no-time --no-session)"
hist_bare="$("$BIN" history --no-time --no-exit --no-session)"

# The default listing surfaces each command's result; the failing one shows [1].
if grep -q '\[1\]  false' <<<"$hist_full"; then
    echo "  ok: failing command shows its exit code"
else
    echo "  FAIL: exit column missing for the failing command" >&2
    printf '%s\n' "$hist_full" >&2; exit 1
fi

# Numbered oldest-first, with the result column: entry 1 is the first recorded.
hist_first="$(head -1 <<<"$hist_full" | sed 's/^[[:space:]]*//')"
assert_eq "entry 1 is the oldest, with result" "$hist_first" "1  [0]  git status"

# --no-exit returns the bare bash look.
bare_first="$(head -1 <<<"$hist_bare" | sed 's/^[[:space:]]*//')"
assert_eq "--no-exit drops the result column" "$bare_first" "1  git status"

# The SESS token column shows a 7-char per-shell token by default (between the
# result and the command) and --no-session hides it.
if grep -Eq '\[1\]  [0-9a-z]{7}  false' <<<"$("$BIN" history --no-time)"; then
    echo "  ok: SESS token column shown by default"
else
    echo "  FAIL: SESS token column missing from default history" >&2
    "$BIN" history --no-time >&2; exit 1
fi

# `yas session <id>` groups one shell's commands oldest-first, with a header
# line "session <token> (<full-id>) · <N> commands".
sess_view="$("$BIN" session smoke --no-time)"
sess_header="$(head -1 <<<"$sess_view")"
if grep -Eq '^session [0-9a-z]{7} \(smoke\) · 5 commands$' <<<"$sess_header" \
   && grep -q 'git status' <<<"$sess_view"; then
    echo "  ok: yas session lists one shell oldest-first with a header"
else
    echo "  FAIL: yas session output unexpected" >&2
    printf '%s\n' "$sess_view" >&2; exit 1
fi

# `yas search --failed` finds only the non-zero exits.
failed_cmds="$("$BIN" search --failed --json | jq -r '.records[].command')"
assert_eq "search --failed -> only the failing command" "$failed_cmds" "false"

# Delete the oldest entry; it drops out and the deletion sticks.
"$BIN" history -d 1 2>/dev/null
after_del="$("$BIN" history --json | jq '.records | length')"
assert_eq "history after -d 1" "$after_del" "4"
gone="$("$BIN" history --json | jq '[.records[].command] | index("git status")')"
assert_eq "deleted entry is gone" "$gone" "null"

# -c refuses without the explicit guard.
if "$BIN" history -c >/dev/null 2>&1; then
    echo "  FAIL: 'history -c' must refuse without --yes" >&2; exit 1
fi
echo "  ok: -c refused without --yes"

# -c --yes wipes the rest.
"$BIN" history -c --yes 2>/dev/null
cleared="$("$BIN" history --json | jq '.records | length')"
assert_eq "history cleared" "$cleared" "0"

# A query hides its own in-flight record: the zsh hook exports YAS_RECORD_ID,
# and history/search exclude it so the running command doesn't list itself.
self_id="$("$BIN" record start --command "self-marker-cmd" --cwd "$ROOT" --session smoke --shell zsh)"
self_hit="$(YAS_RECORD_ID="$self_id" "$BIN" history --json | jq -r '[.records[].command] | index("self-marker-cmd")')"
assert_eq "in-flight self record excluded from its own output" "$self_hit" "null"

###############################################################################
# zsh hook: live capture + yas-pause / yas-resume (optional; needs zsh)
###############################################################################
if command -v zsh >/dev/null; then
    echo "### zsh hook + pause"
    # Source the real hook in a clean zsh and drive preexec/precmd by hand around
    # a few commands, toggling pause in the middle. Same data dir + binary.
    hook="$WORK/hook_test.zsh"
    hook_session_file="$WORK/hook_session"
    # Echo the hook's own generated session id out so the assertions below can
    # scope to it explicitly, rather than relying on the store being otherwise
    # empty (from the `history -c --yes` above) to isolate these records.
    cat >"$hook" <<EOF
source "$ROOT/shell/yas.zsh"
_yas_preexec "smoke-tracked-one"; _yas_precmd
yas-pause
_yas_preexec "smoke-secret-paused"; _yas_precmd
yas-resume
_yas_preexec "smoke-tracked-two"; _yas_precmd
export YAS_CORR_ID="smoke-corr-sentinel"
_yas_preexec "smoke-corr-tagged"; _yas_precmd
unset YAS_CORR_ID
unset CLAUDE_CODE_SESSION_ID
_yas_preexec "smoke-corr-absent"; _yas_precmd
echo "\$YAS_SESSION" >"$hook_session_file"
EOF
    PATH="$WORK:$PATH" zsh -f "$hook" >/dev/null 2>&1
    hook_session="$(<"$hook_session_file")"
    captured="$("$BIN" search --json --session "$hook_session" | jq -r '.records[].command')"
    case "$captured" in
        *smoke-secret-paused*)
            echo "  FAIL: a yas-pause'd command was captured" >&2
            printf '%s\n' "$captured" >&2; exit 1 ;;
    esac
    if grep -q 'smoke-tracked-one' <<<"$captured" && grep -q 'smoke-tracked-two' <<<"$captured"; then
        echo "  ok: hook captures; yas-pause skips; yas-resume restores"
    else
        echo "  FAIL: tracked commands were not captured" >&2
        printf '%s\n' "$captured" >&2; exit 1
    fi
    tracked_exec="$("$BIN" search --json --session "$hook_session" | jq -r '.records[] | select(.command=="smoke-tracked-one") | .executor')"
    assert_eq "hook tags human executor" "$tracked_exec" "human"

    # corr_id seam: YAS_CORR_ID (an explicit override, or the CLAUDE_CODE_SESSION_ID
    # fallback) rides the hook's --corr-id flag into the record; with neither set,
    # corr_id stays empty/absent rather than being invented.
    corr_tagged="$("$BIN" search --json --session "$hook_session" | jq -r '.records[] | select(.command=="smoke-corr-tagged") | .corr_id')"
    assert_eq "hook maps YAS_CORR_ID into corr_id" "$corr_tagged" "smoke-corr-sentinel"
    corr_absent="$("$BIN" search --json --session "$hook_session" | jq -r '.records[] | select(.command=="smoke-corr-absent") | .corr_id')"
    assert_eq "hook leaves corr_id empty with no YAS_CORR_ID/CLAUDE_CODE_SESSION_ID" "$corr_absent" "null"
else
    echo "### zsh hook + pause (skipped: zsh not found)"
fi

echo "### executor provenance + contract version"
aid="$("$BIN" record start --command "agent-deploy" --cwd "$ROOT" --session smoke --shell zsh --author claude-code)"
"$BIN" record finish --id "$aid" --exit 0 --duration-ms 1
# Scoped to the smoke session so this doesn't silently depend on the store
# being otherwise empty (from the earlier `history -c --yes`) to isolate it.
agent_hit="$(curl -fsS "http://$ADDR/v1/search?executor=\$all-agent&session=smoke" | jq -r '.records[].command')"
assert_eq "executor=\$all-agent finds the agent command" "$agent_hit" "agent-deploy"
ver="$(curl -fsS "http://$ADDR/v1/version" | jq -r .version)"
assert_eq "/v1/version reports v1" "$ver" "v1"

echo "### record repeated failures (failure_summary rollup)"
# The store was cleared above, so seed a recurring failure for the MCP rollup.
for _ in 1 2; do
    frid="$("$BIN" record start --command "flaky-cmd --once" --cwd "$ROOT" --session smoke --shell zsh)"
    "$BIN" record finish --id "$frid" --exit 1 --duration-ms 1
done

###############################################################################
# yas digest: deterministic per-(host,cwd) synthesis over the local replica.
# --json is the contract; groups is always an array, seq never surfaces, and an
# empty window still serializes groups as [] (the empty-list invariant).
###############################################################################
echo "### yas digest"
digest_json="$("$BIN" digest --json)"
assert_eq "digest --json groups is an array" \
    "$(jq -r '.groups | type' <<<"$digest_json")" "array"
if jq -e '.. | objects | has("seq")' <<<"$digest_json" >/dev/null 2>&1; then
    echo "  FAIL: digest --json leaked seq -> $digest_json" >&2; exit 1
fi
echo "  ok: digest --json never emits seq"
empty_digest="$("$BIN" digest --since 2000-01-01T00:00:00Z --until 2000-01-02T00:00:00Z --json)"
case "$empty_digest" in
    *'"groups": []'*) echo "  ok: empty digest window serializes groups as []" ;;
    *) echo "  FAIL: empty digest not [] -> $empty_digest" >&2; exit 1 ;;
esac

###############################################################################
# yas mcp (agent seam): JSON-RPC round-trip over the real stdio transport —
# initialize, list the tools, call search_commands with an executor token, and
# call failure_summary (the rollup verb). stdin stays open briefly after each
# batch so the server can flush replies before EOF tears the transport down.
###############################################################################
echo "### yas mcp e2e (stdio)"
mcp_out="$({
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
    sleep 1
    printf '%s\n' \
        '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
        '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
        '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_commands","arguments":{"query":"agent-deploy","executor":"$all-agent","session":"smoke"}}}' \
        '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"failure_summary","arguments":{}}}'
    sleep 1
} | "$BIN" mcp 2>/dev/null)"

mcp_tools="$(jq -rs '[.[] | select(.id==2) | .result.tools[].name] | sort | join(",")' <<<"$mcp_out")"
assert_eq "mcp tools/list exposes the six read tools" \
    "$mcp_tools" "command_status,failure_summary,how_did_i_run,recent_commands,search_commands,what_failed"
mcp_hit="$(jq -rs '[.[] | select(.id==3) | .result.structuredContent.commands[].command] | join(",")' <<<"$mcp_out")"
assert_eq "mcp search_commands honors \$all-agent" "$mcp_hit" "agent-deploy"
# failure_summary rolls the two flaky-cmd failures into one group with count 2.
mcp_fail_cmd="$(jq -rs '[.[] | select(.id==4) | .result.structuredContent.failures[0].command] | join(",")' <<<"$mcp_out")"
assert_eq "mcp failure_summary rolls up the recurring failure" "$mcp_fail_cmd" "flaky-cmd --once"
mcp_fail_count="$(jq -rs '[.[] | select(.id==4) | .result.structuredContent.failures[0].count] | join(",")' <<<"$mcp_out")"
assert_eq "mcp failure_summary counts repeats" "$mcp_fail_count" "2"

###############################################################################
# Ctrl-R recall source (shell/yas-fzf.zsh): dedup + human-only default scope.
# Exercises the real _yas_fzf_source function (no interactive fzf needed).
###############################################################################
if command -v zsh >/dev/null; then
    echo "### ctrl-R recall candidates"
    for _ in 1 2; do
        rid="$("$BIN" record start --command "recall-dup" --cwd "$ROOT" --session smoke --shell zsh)"
        "$BIN" record finish --id "$rid" --exit 0 --duration-ms 1
    done
    # Default scope is $all-human, so repeats collapse and the agent-run
    # "agent-deploy" (recorded above with --author claude-code) is excluded.
    cands="$(PATH="$WORK:$PATH" zsh -fc "source '$ROOT/shell/yas-fzf.zsh'; _yas_fzf_source" | tr '\0' '\n')"
    assert_eq "recall dedups repeats" "$(grep -c '^recall-dup$' <<<"$cands")" "1"
    assert_eq "recall hides agent commands (default \$all-human)" "$(grep -c '^agent-deploy$' <<<"$cands")" "0"
else
    echo "### ctrl-R recall candidates (skipped: zsh not found)"
fi

echo "ALL SMOKE CHECKS PASSED"
