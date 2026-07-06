# yas mcp — command history for AI agents

`yas mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io)
server exposing **read-only** tools over your shell-command history. It is the
agent seam: the same records your Ctrl-R recall hits, queryable by a coding
agent. It reads the local replica through the same query contract the HTTP API
uses (`store.Query`/record), never the database schema, and needs no separate
`yas serve` running.

## Tools (all read-only)

| Tool | What it answers |
| --- | --- |
| `search_commands` | Full-text search with filters (host, cwd, session, exit, `executor`, failed-only, RFC3339 `since`/`until`), newest-first. "Have I run this before?", "how did I do X here?" |
| `recent_commands` | The most recent commands, optionally scoped by host/cwd/executor. |
| `what_failed` | Recent commands that exited non-zero, optionally by host/cwd/since. |
| `command_status` | One command by `id`: its exit code, duration, cwd, host, executor. |
| `failure_summary` | **Rollup:** top recurring failing commands with counts + last-seen, scoped by host/cwd/`since`. "What keeps breaking?" — the aggregate to `what_failed`'s list. |
| `how_did_i_run` | **Recall:** distinct argument patterns for a given program (`git`, `ssh`, …), newest-first, near-duplicates that differ only inside quotes collapsed. "What flags/args did I use with X?" |

`failure_summary` and `how_did_i_run` fold results in the agent over a recent
scan window (exact within it); a top-level `scan_truncated` flag signals older
matches may exist beyond the window (distinct from a per-item `truncated`, which
flags a single command string that was shortened). `how_did_i_run` scopes its
full-text match to the command column, so a directory path containing the
program name never crowds real invocations out of the window.

`executor` accepts a name (`human`, `claude-code`, `codex`, `ci`, …) or the
convenience tokens `$all-agent` / `$all-human` — the same vocabulary as the
query API and `yas search --executor`.

## Add it to an MCP client

**Claude Code:**

```bash
claude mcp add yas -- yas mcp
```

**Generic MCP client config** (Claude Desktop, etc.):

```json
{
  "mcpServers": {
    "yas": { "command": "yas", "args": ["mcp"] }
  }
}
```

The client launches `yas mcp` and speaks MCP over **stdio** (the default).

## StreamableHTTP (optional)

To serve over HTTP instead of stdio:

```bash
yas mcp --http 127.0.0.1:8770        # loopback (no listener auth)
```

Bare ports (`8770`, `:8770`) bind loopback. A **non-loopback** bind is a
network-reachable read surface over your whole history, so it is refused unless
you pass `--http-allow-insecure` **and** have a token configured (`token` /
`YAS_TOKEN`); the server then enforces `Authorization: Bearer <token>` on every
request. The loopback handler also keeps the SDK's DNS-rebinding protection.

### Self-reference guard

`yas mcp` can exclude the querying agent's own session's commands from
results via `--exclude-corr-id <id>` (default `$YAS_CORR_ID`).

Because yas core is agent-**agnostic**, it does not read
`CLAUDE_CODE_SESSION_ID` itself — that mapping lives only in the zsh capture
hook (`shell/yas.zsh`). So the guard is **inert unless you supply the id**:
launch with `--exclude-corr-id "$CLAUDE_CODE_SESSION_ID"` (or set
`YAS_CORR_ID` in the mcp server's environment to the same value the record
hook uses), so the excluded id agrees with the `corr_id` the hook records.
With `YAS_CORR_ID` unset, records may carry `corr_id=CLAUDE_CODE_SESSION_ID`
while the guard excludes nothing — a documented no-op, not a bug.

It is additionally inert until agent-run commands are themselves captured
with a corr_id (a later milestone) — there is nothing to exclude until then.

## Notes

- The server is **pull-only**: an agent calls a tool; yas answers. Auto-injected
  "context before the first prompt" resources are a deliberate non-feature for
  now (a privacy/noise surface) — the seam is left open for later, off by
  default.
