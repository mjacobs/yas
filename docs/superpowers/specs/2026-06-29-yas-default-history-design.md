# Design: `yas` / `yas [n]` as a shortcut for `yas history`

Date: 2026-06-29
Status: Approved (brainstorm), pending implementation plan

## Goal

Make `yas history` the default command, so the two most common reads are
shorter to type:

- `yas` (no args) lists recent history (the existing default of 100 entries).
- `yas <n>` lists the last `n` entries — a shortcut for `yas history <n>`.

This is purely a CLI-dispatch convenience. It changes no storage, no JSON
contract, and no `yas history` semantics; it only changes how the first
argument to `yas` is routed.

## Routing rule (Option C: number + flags)

A pure function maps the raw argument list to a normalized
`(subcommand, remaining-args)` pair; `main()` is a thin switch over the result.

```
route(args []string) (cmd string, rest []string)
```

Precedence, top to bottom (first match wins):

1. **No args** → `("history", nil)`. Lists the default 100.
2. **Known subcommand word** — `record`, `search`, `history`, `serve`, `sync`,
   `import`, `session`, `mcp` → `(word, args[1:])`. Unchanged from today.
3. **Reserved meta-flags** (intercepted before any fallthrough):
   `version` / `--version` / `-v` → `("version", nil)`;
   `help` / `-h` / `--help` → `("help", nil)`.
4. **First token is a non-negative integer** → `("history", args)` — the whole
   arg list, count included.
5. **First token is flag-like** (length > 1 and starts with `-`, e.g. `--json`,
   `-d`, `--no-time`) → `("history", args)`. History's own flag set validates
   it, so an unknown flag such as `yas --failed` errors cleanly with
   `flag provided but not defined: -failed`.
6. **Anything else** (a bare unknown word, e.g. `serch`) → `("unknown", args)`.
   `main()` prints `yas: unknown command "<word>"`, then usage, then exits 2 —
   identical to today's behavior.

Step 3 is the only bookkeeping the rule requires: a short, fixed list of
top-level meta-flags must be intercepted before step 5, so they never leak into
the history flag set. Any future top-level flag must be added there too.

### Why this rule does not create a latent grammar trap

- Command names are always words; a history count is always a non-negative
  integer. The two namespaces are disjoint and stay that way, so the number
  shortcut (step 4) can never become ambiguous.
- Flags (step 5) can be neither a command name nor a history *positional*, so
  routing them to history is grammatically safe.
- Bare unknown words still error (step 6), which preserves typo diagnostics and
  keeps the command-name namespace decoupled from history's positional grammar.
  (The rejected "history as true default" option routed bare words to history
  too, which both degraded typo errors and welded those two namespaces together
  — a trap that would spring if `history` ever gained a non-numeric positional.)

## Resolved edge cases (no new special-casing)

All of these fall out of the rule plus existing `yas history` semantics:

| Invocation        | Routes to            | Result                                              |
| ----------------- | -------------------- | --------------------------------------------------- |
| `yas`             | `history`            | last 100 (empty store → prints nothing)             |
| `yas 20`          | `history 20`         | last 20                                             |
| `yas 0`           | `history 0`          | last 100 — `n==0` is the existing default sentinel  |
| `yas 20 --json`   | `history 20 --json`  | last 20 as the query-API JSON envelope              |
| `yas --json`      | `history --json`     | last 100 as JSON                                    |
| `yas -d 3`        | `history -d 3`       | delete entry #3 (same as `yas history -d 3`)        |
| `yas -c --yes`    | `history -c --yes`   | clear all — still guarded by `--yes`                |
| `yas -3` / `yas -`| `history` flag set   | clean "flag provided but not defined" / parse error |
| `yas serch git`   | `unknown`            | `yas: unknown command "serch"`, usage, exit 2       |
| `yas search foo`  | `search foo`         | unchanged                                           |
| `yas -v`          | `version`            | prints version (intercepted before step 5)          |

## Decisions

- **No empty-store fallback.** `yas` is *exactly* `yas history` — on an empty
  store it prints nothing rather than falling back to usage. This keeps the
  mental model clean ("`yas` = your recent history"). Help stays discoverable
  via `yas help` / `yas -h` / `yas --help`.
- **`route` is pure** and returns a normalized command string (including a
  synthetic `"unknown"` for the typo path), so `main()` performs no parsing
  logic of its own. This matches the repo idiom of unit-testing pure helpers
  (`parseHistoryArgs`, `parseSearchArgs`) rather than `main()`.
- **`-d` / `-c` reachable via the shortcut** is intentional and consistent;
  destructive `-c` remains gated behind `--yes`, unchanged.

## Implementation shape

- Add `route(args []string) (cmd string, rest []string)` to `cmd/yas/main.go`.
- Refactor `main()` to call `route(os.Args[1:])` and switch over `cmd`,
  including a `"history"` case, a `"version"`/`"help"` case, and an `"unknown"`
  case that reproduces today's unknown-command error + usage + exit 2.
- No change to `parseHistoryArgs`, `cmdHistory`, `doHistory`, or any store code.

## Testing (TDD)

A table-driven `TestRoute` is the new red test, asserting `(cmd, rest)` for:

- `[]` → `("history", nil)`
- `["20"]` → `("history", ["20"])`; `["0"]` → `("history", ["0"])`
- `["20","--json"]` → `("history", ["20","--json"])`
- `["--json"]` → `("history", ["--json"])`; `["-d","3"]` → `("history", ["-d","3"])`
- `["history","20"]` → `("history", ["20"])` (explicit form still works)
- `["search","foo"]` → `("search", ["foo"])`; `["session","tok"]` → `("session", ["tok"])`
- `["-v"]` / `["--version"]` / `["version"]` → `("version", …)`
- `["-h"]` / `["--help"]` / `["help"]` → `("help", …)`
- `["serch"]` → `("unknown", ["serch"])`; `["serch","git"]` → `("unknown", ["serch","git"])`

Existing `yas history` / search / session tests are unaffected.

## Docs

- Update `usage()` in `cmd/yas/main.go` to document `yas [n]` as the shortcut.
- Update the README usage block to show the shortcut.

## Out of scope

- No changes to `yas history` flags, output, numbering, or JSON.
- No "did you mean …?" suggestions for unknown commands (step 6 keeps today's
  plain error; suggestions can be layered on later without affecting the rule).
- No empty-store usage fallback (see Decisions).
