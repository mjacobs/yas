# 1. Local store is SQLite (modernc.org/sqlite), not DuckDB

- **Status:** Accepted
- **Date:** 2026-06-22
- **Deciders:** project owner, informed by adversarially-verified research

## Context

The per-machine agent needs an embedded local store for the full history
replica. The workload: one tiny single-row `INSERT` per command (`record start`)
plus a single-row `UPDATE` (`record finish`), driven by short-lived CLI
processes, with a long-lived `yas serve` process reading the **same file**
concurrently, full-text command search, and per-command durability. DuckDB was
proposed as an alternative to SQLite and evaluated.

## Decision

Use **SQLite via the pure-Go `modernc.org/sqlite` driver** as the embedded local
store. Do **not** adopt DuckDB as the store.

## Why not DuckDB (verified 2026-06-22)

Two project invariants are disqualifying, plus a workload mismatch. Both decisive
claims were checked by independent adversarial verifiers.

1. **cgo / static cross-compile.** Every Go DuckDB driver requires
   `CGO_ENABLED=1` and links the DuckDB C++ library (the canonical driver is now
   `duckdb/duckdb-go`). Prebuilt static libs cover only ~5 platform tuples,
   cross-compiling needs a per-target C/C++ toolchain, and the binary is ~40 MB.
   `modernc.org/sqlite` is pure Go, `CGO_ENABLED=0`, single-digit MB — the "one
   static binary, trivial cross-compile" property is a core project value.
2. **Multi-process read-while-write.** DuckDB takes an exclusive file lock in
   read-write mode; two processes may share a file only if **all** are read-only.
   yas's design (a `yas serve` reader concurrent with per-command `yas record`
   writer processes on one file) has no supported DuckDB mode — it fails with a
   lock conflict. SQLite WAL is built for exactly this (one writer + many readers
   across processes).
3. **Workload + search fit.** DuckDB is a columnar OLAP engine — its docs warn
   against row-by-row inserts (storage is organized in ~122k-row groups) and its
   FTS extension cannot update incrementally. yas's hot path is OLTP single-row
   writes with live full-text search — SQLite + FTS5 territory.

## Consequences

- The agent stays a cgo-free, trivially cross-compiled static binary; `make
  cross` guards the property.
- DuckDB is **not** lost — it remains available as an optional, out-of-process
  analytics tool. A user can `ATTACH '~/.local/share/yas/history.db' (TYPE
  sqlite, READ_ONLY)` from the standalone DuckDB CLI (or `ATTACH` the central
  Postgres) for top-N commands, frequency-over-time, per-dir stats, and window
  functions — at zero cost to the yas binary.
- Revisit only in a v2/v3 if rich built-in analytics become a priority.
