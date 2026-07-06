# yas query API — v1 contract

The localhost HTTP+JSON query API served by `yas serve` is the stable contract
UIs and agents target. **The JSON record shape — not the SQLite/Postgres schema —
is the contract.** This document is v1; it is frozen and enforced by
`TestRecordJSON_ContractFields` (a key drift fails the build).

`GET /v1/version` → `{"version":"v1","record_fields":[...]}` lets a client detect
the contract version and field set at runtime.

## Record JSON

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | UUIDv7, client-generated, global dedup key. Always present. |
| `command` | string | The command line. Always present. |
| `cwd` | string | Working directory. Omitted when empty. |
| `hostname` | string | Machine that ran it. Omitted when empty. |
| `session` | string | Per-shell session id. Omitted when empty. |
| `shell` | string | zsh \| bash \| fish. Omitted when empty. |
| `username` | string | OS user. Omitted when empty. |
| `exit_code` | int\|null | Null/absent until the command finishes. |
| `start_time` | RFC3339 | When the command began. Always present. |
| `duration_ms` | int\|null | Null/absent until the command finishes. |
| `created_at` | RFC3339 | When the record was first written. Always present. |
| `deleted` | bool | Tombstone. Omitted when false. |
| `executor` | string | Who/what ran it: `human` \| `claude-code` \| `codex` \| `ci` \| ... Empty = human. Omitted when empty. |
| `corr_id` | string | Cross-tool correlation key (e.g. an agentsview session). Reserved; populated in a later milestone. Omitted when empty. |
| `repo_root` | string | Git repo root of `cwd`, derived at capture time. Empty off-repo and on imported history (unrecoverable after the fact). Omitted when empty. |
| `branch` | string | Git branch at capture time. Empty on a detached HEAD, off-repo, and on imported history. Omitted when empty. |

These are additive, back-compatible v1 fields (like `executor`/`corr_id`
before them): a fully-populated record gains the keys, old records simply lack
them. `seq` is sync-transport metadata and **never** appears in record JSON.

## Endpoints

- `GET /v1/search` — newest-first matching records. Params: `q` (FTS), `host`,
  `cwd`, `session`, `exit`, `executor` (a name, or `$all-agent` / `$all-human`; `human` is treated the same as `$all-human`, i.e. includes untagged rows),
  `since`, `until` (RFC3339), `limit`, `offset`, `reverse`. Response:
  `{"records":[...]}`. Empty result → `{"records":[]}` (never `null`).
- `GET /v1/version` — `{"version":"v1","record_fields":[...]}`.
- `GET /v1/healthz` — `{"status":"ok"}`.

Malformed params → `400`; non-GET → `405`.

## Stability promise

Within v1, fields are only **added** (additively, behind `omitempty`). Removing or
renaming a field, or changing its type, is a breaking change requiring a new
version (`/v2`) and a bump of `queryapi.ContractVersion`.
