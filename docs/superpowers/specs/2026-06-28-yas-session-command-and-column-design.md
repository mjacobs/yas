# yas session — group a shell's history; session-token column

Date: 2026-06-28
Status: Approved design (pre-implementation)

## Problem

You can find a command with `yas search` (e.g. that one `rsync` invocation), but you
can't easily see what you ran **before and after it in that same terminal**.
Each record already carries a per-shell `session` id, but it's only visible via
`--json` and there's no command to pull one shell's history. The goal is to make
the session both **visible** in the listings and **directly queryable** as a
linear timeline.

## Goals

- Surface a compact per-shell session id as a column in **both** `yas search`
  and `yas history` human output.
- Add `yas session <token | full-id>` that prints one shell's commands as a
  linear, oldest-first history (so you can read the neighbours of a found
  command).

## Non-goals (YAGNI)

- No ±N windowing around a specific record — `yas session` lists the whole
  session and you eyeball the neighbours.
- No grouping of imported history (those rows have an empty session — see
  Caveats).
- No cross-terminal / time-window grouping — that is already `--since`/`--until`.
- No change to the stored DB schema and no change to the record JSON contract.

## Background (current state, verified)

- **Capture:** `shell/yas.zsh` sets `YAS_SESSION="${HOST}-$$-${EPOCHSECONDS}"`
  once per interactive shell — stable for that shell's life; `host-pid-epoch`,
  ~22 chars.
- **Store/query:** `store.Query` has a `Session` exact filter (wired into the
  SQLite `WHERE r.session = ?`) and `Reverse` ordering. Already exposed via the
  CLI `--session`, the query API `?session=`, and the MCP `search_commands`
  tool.
- **Rendering:** the human output of `yas search` (`doSearch`/`renderSearchRich`)
  and `yas history` (`renderHistory`/`renderHistoryRich`) shows
  `time · exit · command`; the session id appears only in `--json`.
- **Importer:** `internal/histimport` leaves `session` empty, so back-imported
  `~/.zsh_history` rows have no session.

## Design

### 1. `shortSession` token

A pure helper in `cmd/yas`:

```
shortSession(id string) string
```

- Value: take a 64-bit **FNV-1a hash** of the full session id, reduce to its low
  `36^7` (`h mod 36^7`), render **base36** lowercase, and zero-pad to exactly
  **7 characters**. This is fixed-width by construction (no truncation
  ambiguity). `shortSession("") == ""`.
- Deterministic, fixed-width, derivable anywhere, no stored state.
- 7 base36 chars ≈ 7.8×10¹⁰ space. Collisions are **handled at resolution**
  (§3), not engineered away.

### 2. Session-token column in `search` and `history`

- Add a `SESS` column (the 7-char token) to the human renderers of **both**
  commands: `renderSearch`(plain)/`renderSearchRich` and
  `renderHistory`(plain)/`renderHistoryRich`. Placed **between the exit field and
  the command** so the variable-width command stays last.
- **Blank** when the row's session is empty (imported rows).
- Shown by default; **`--no-session`** suppresses it (mirrors the existing
  `--no-exit`/`--no-time`). Implemented as a `showSession bool` on the shared
  render opts (default `true`).
- **`--json` is unchanged** for both commands: the records envelope already
  carries the full `session` field; the short token is display-only and is
  **never** added to the record JSON (the JSON surface is the contract).

### 3. `yas session <token | full-id>`

The new subcommand.

- Requires one argument; empty/whitespace → usage error (exit 2).
- **Resolution:**
  1. **Exact full id:** `Search(Query{Session: arg, Reverse: true})`. If it
     returns ≥1 record, that's the session.
  2. **Else short token:** call `store.Sessions(ctx)` for the distinct non-empty
     session ids and select those whose `shortSession(s) == arg`:
     - exactly 1 → use it;
     - 0 → error `no session matching token "<arg>"` (exit 1);
     - >1 (rare hash collision) → error listing each candidate `token (full-id)`
       and asking the user to re-run with a full id (exit 1).
- **Output:** the session's commands as **linear history, oldest-first**
  (`Reverse: true`), rendered through the existing **history renderer** with the
  session column suppressed (`showSession=false`, since every row shares the one
  session). A header line precedes it:
  `session <token> (<full-id>) · <N> commands`.
- **Flags:** `--json` (records envelope, full `session` field), `--no-color`, and
  the same `--time-format`/`--no-time`/`--no-exit` flags as `history`.
- Excludes the in-flight `yas session` command via `YAS_RECORD_ID`, consistent
  with `history`/`search` self-exclusion (harmless; it's almost always a
  different session anyway).

### 4. `store.Sessions`

Add to the SQLite store and the store interface the CLI uses:

```
Sessions(ctx context.Context) ([]string, error)
```

- `SELECT DISTINCT session FROM records WHERE deleted = 0 AND session != ''`,
  ordered by `max(start_time) DESC` (newest session first).
- Used **only** by token resolution. No schema change, no migration.
- Postgres store: **not required for v1** — all queries (and thus resolution)
  run against the local SQLite replica. Add a Postgres `Sessions` only if/when a
  server-side global query API lands.

### 5. CLI wiring

- New `session` case in the `main.go` subcommand dispatch and the usage text.
- `parseSessionArgs` for the positional arg plus the shared rendering flags.

## Error behaviour

| Situation | stderr | exit |
| --- | --- | --- |
| no arg / blank arg | usage | 2 |
| token resolves to 0 sessions | `no session matching token "x"` | 1 |
| token resolves to >1 (collision) | ambiguous + candidate list | 1 |
| store/query error | the error | 1 |

## Testing (TDD, vertical slices)

- **`shortSession`:** determinism; width == 7; `""` → `""`; distinct tokens for
  distinct sample ids; base36 charset only.
- **`store.Sessions`:** distinct; excludes empty and deleted; newest-session
  first.
- **Renderers (search + history):** `SESS` column present and aligned; blank for
  sessionless rows; `--no-session` hides it; `--json` output byte-unchanged.
- **`doSession`:** resolve by token; resolve by full id; oldest-first ordering;
  header line; 0-match error; collision error; empty-arg usage; `--json`
  envelope.

## Caveats & follow-ups

- **Live-only:** only live-hook-captured commands have a session. Imported rows
  (`session=""`) show a blank token and can't be grouped — same import/live split
  tracked by h4t6/qzs4. Documented, not fixed here.
- **Collisions:** 7-char base36 makes them very unlikely; resolution handles them
  rather than the token format.
- **MCP:** no change in v1 (the full `session` is already exposed); a
  session-grouping MCP verb is a candidate later recall verb (tcnp).
- **Postgres `Sessions`:** add when/if a global server-side query API lands.
