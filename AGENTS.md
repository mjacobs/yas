# AGENTS.md — working in yas

yas is a local-first, homelab-scale shell-history service in Go. See
[README.md](README.md) for the full design (vision, architecture, sync protocol,
query API). This file is the working contract for agents — and humans — in this
repo.

## Load-bearing invariants (do not break these)

- **cgo-free static binary.** The agent must build with `CGO_ENABLED=0` and
  cross-compile to any `GOOS`/`GOARCH` by swapping env vars. The local store is
  `modernc.org/sqlite` (pure Go) precisely for this. Never introduce a cgo
  dependency. `make cross` guards it.
- **Local store is SQLite in WAL mode.** A long-lived `yas serve` reader runs
  concurrently with short-lived `yas record` writer processes on the same file.
  Preserve multi-process WAL — don't funnel writes through a single shared
  connection in a way that breaks it.
- **The JSON surface is the contract; the DB schema is private.** UIs target the
  HTTP+JSON query API (`{"records":[...]}`), never the SQLite/Postgres schema, so
  storage stays free to refactor. `seq` is sync-transport metadata and never
  appears in record JSON.
- **The record path is sacred.** `yas record start` must be fast, synchronous,
  work offline, and print ONLY the UUIDv7 to stdout — the zsh hook captures
  stdout as the id. Errors go to stderr + a non-zero exit, never a bogus id on
  stdout.
- **Empty result lists serialize as `[]`, not `null`** (a query-API contract;
  the store returns a non-nil empty slice).

## Decisions (don't re-litigate)

- **Local store: SQLite, not DuckDB** — adversarially verified 2026-06-22 (cgo +
  single-writer file lock + OLTP-write/FTS fit). DuckDB stays an optional
  out-of-process analytics tool only. See
  [docs/decisions/0001](docs/decisions/0001-local-store-sqlite-not-duckdb.md).
- **Topology / language / stores:** local-first + sync; Go; SQLite local replica;
  Postgres central source-of-record (see the README "Why these choices" table).
- **The query API is served by the local agent** over the local replica (v1). A
  server-side global query API is a stretch goal, not v1.

## Workflow

- **Issue tracking: GitHub Issues.** Search existing issues before filing a new
  one; prefer commenting on an existing issue over opening a duplicate. Close
  only verified work, citing the verification (e.g. `go test ./...`) and the
  commit; if work is incomplete, say what remains rather than closing.
- **TDD.** Build features test-first in vertical slices (one test → one minimal
  impl → repeat), testing through public interfaces. Don't write all tests then
  all code.
- **Small, focused commits**, each green. Agent-authored commits carry a
  `Co-Authored-By` trailer; wrap body text at 80 columns.
- **Before closing a milestone:** `make build test vet` green, `make cross` (the
  cgo-free guard), `make smoke` (end-to-end), and an adversarial review of the
  diff.

## Build / test / run

```bash
make build        # go build ./...
make test         # go test ./...
make vet lint     # go vet + golangci-lint (gosec, misspell, gofmt)
make cross        # CGO_ENABLED=0 cross-compile matrix — proves the static-binary property
make smoke        # end-to-end: record -> search -> serve -> curl (throwaway data dir; needs curl+jq)
make install      # cgo-free static `yas` -> $BINDIR (default ~/.local/bin), version stamped
```

See [BUILD.md](BUILD.md) for `make install`/version-stamping details (the version
comes from `git describe`; the linker symbol is `main.version`, not the import
path), cross-compiling, and releases.

The Postgres store tests (`internal/store/postgres`) are integration tests gated on
`$YAS_TEST_DATABASE_URL`; they skip unless it points at a reachable Postgres (e.g. a
throwaway container). `go test ./...` stays green either way.

Run the agent directly (data dir via `YAS_DATA_DIR`, default
`~/.local/share/yas`):

```bash
go run ./cmd/yas record start --command "git status" --cwd "$PWD" --session s --shell zsh
go run ./cmd/yas search git --limit 20            # or --json (same envelope as the API)
go run ./cmd/yas serve --addr 127.0.0.1:8765      # then curl /v1/search?q=...
source shell/yas.zsh                              # live capture in an interactive zsh
# then: yas-pause / yas-resume / yas-status      # toggle capture for this shell
```

Config: JSON at `~/.config/yas/config.json`, overridable via env — client:
`YAS_SERVER_URL`, `YAS_TOKEN`, `YAS_DATA_DIR`, `YAS_HOSTNAME`; server:
`YAS_DATABASE_URL`, `YAS_ADDR`, `YAS_CONFIG`.

## Layout

```
cmd/yas/                 agent CLI: record, search, serve, sync
cmd/yas-server/          sync hub: Postgres-backed push/pull sync API + bearer auth
internal/record/          canonical Record + dependency-free UUIDv7
internal/config/          client + server config (JSON file + env overrides)
internal/syncproto/       push/pull wire types (seq is transport-only)
internal/syncapi/         server-side sync HTTP API (push/pull) + bearer-token guard
internal/syncclient/      client side of the sync protocol (push/pull HTTP client)
internal/store/           Store/SyncSource/Cursor interfaces + SQLite & Postgres schemas
internal/store/sqlite/    local replica (modernc.org/sqlite): WAL, upsert-LWW, FTS5
internal/store/postgres/  server source-of-record (pgx): upsert, seq cursor, bump-seq re-pull
internal/queryapi/        localhost HTTP+JSON query API (the UI contract)
shell/yas.zsh            two-phase preexec/precmd capture hook
deploy/systemd/           systemd --user units (server service + sync timer)
docs/decisions/           architecture decision records (ADRs)
```

## Status

M1–M6 done: agent, server, sync loop, query API, and packaging (static release
matrix, install script, import from ~/.zsh_history + atuin) are all in. A
reference deployment runs via `deploy/` (systemd `--user`) against Postgres.
GitHub Issues is the source of truth for live status — don't duplicate it here.
