# Rename: `yass` → `yas`

**Date:** 2026-06-24
**Status:** approved, in progress
**Type:** mechanical rename / rebrand (no behavior change)

## Goal

Drop one character from the project/CLI name: `yass` → `yas`. Shorter is better
for a common CLI tool, and the intended meme reference is spelled both ways, so
the single-`s` form is fine. The name was previously a backronym ("Yet Another
Shell-history Service"); it is now just the name.

## Scope decisions (approved 2026-06-24)

- **Backward compat:** clean break + migrate. Switch to `YAS_*` env vars and
  `~/.config/yas` + `~/.local/share/yas`; migrate existing local data in place.
  No permanent fallback / compat shims (pre-1.0, single user).
- **Live deployment:** full cutover now — rename the central Postgres database,
  reinstall renamed binaries + systemd units, restart.
- **Repo reach:** everything → `yas`: Go module path, Gitea repo, git remote URL,
  and the local working directory.

## The substitution is total and mechanical

A footprint scan found the name only as two case variants (`yass`, `YASS`) — no
title-case, and no occurrence is embedded in a larger word. Every match is either
the standalone name or an identifier/path that must change (`cmd/yass`,
`github.com/mjacobs/yass`, `YASS_*`, `yass-pause`, `_yass_precmd`, `5432/yass`,
`/opt/yass`, `yass.lan`). So the entire content rename is two blanket
substitutions over the matched text files:

- `yass` → `yas`
- `YASS` → `YAS`

plus path renames via `git mv`.

## Phases

### Phase 1 — Code & docs rename (one atomic commit)

Done on branch `rename-yass-to-yas`. A half-renamed module does not build, so this
is genuinely one commit, green throughout.

- `go.mod` module path + all `github.com/mjacobs/yass/...` imports → `.../yas`.
- `git mv` directories/files:
  - `cmd/yass` → `cmd/yas`
  - `cmd/yass-server` → `cmd/yas-server`
  - `shell/yass.zsh` → `shell/yas.zsh`
  - `deploy/systemd/yass-server.service` → `yas-server.service`
  - `deploy/systemd/yass-sync.service` → `yas-sync.service`
  - `deploy/systemd/yass-sync.timer` → `yas-sync.timer`
- Content subst (the two rules above) over: all `.go`, `shell/yas.zsh`,
  `scripts/smoke.sh`, `Makefile`, `.gitignore`, `.kata.toml`, `deploy/systemd/*`,
  and all docs (`README.md`, `AGENTS.md`, `BUILD.md`, `deploy/README.md`,
  `docs/decisions/0001-*.md`). This covers env vars (`YASS_*`→`YAS_*`), shell
  funcs (`yass-pause/resume/status`, `_yass_*`), CLI/help text, binary names, and
  connection-string examples (`…/yass`→`…/yas`).
- `gofmt` + `go build ./...` green before committing.

The pre-existing uncommitted `README.md` rebrand (header → `# yass`, tagline
*"you want a shell search? yass."*) flows through the substitution and rides in
this commit.

### Phase 2 — Exhaustive verification

`make build test vet lint`, `make cross` (cgo-free static-binary guard),
`make smoke` (end-to-end record→search→serve→curl, now with `YAS_*`). Plus a
multi-angle sweep: assert zero remaining `yass`/`YASS` in tracked files, and an
adversarial check that no substitution changed semantics (no external dependency
is named `yass`; the module path is self-owned). Commit only when fully green.

### Phase 3 — Live homelab cutover (stateful, reversible-first)

The live instance runs as systemd `--user`: `yass-server.service` (Postgres-backed
hub, holds live DB connections), `yass-sync.timer`. Central DB `yass` on the
homelab Postgres host; local replica `~/.local/share/yass/history.db`.

1. Stop `yass-server.service` and `yass-sync.timer` (no writes during migration).
2. Postgres: `ALTER DATABASE yass RENAME TO yas;` run from the `postgres` db with
   no active connections (instant; no data copy). Old DB not dropped yet.
3. `make install` → `yas`, `yas-server`. `mv ~/.config/yass ~/.config/yas` and
   `mv ~/.local/share/yass ~/.local/share/yas`. Fix `server.json`
   `database_url` `…/yass` → `…/yas`.
4. Install `yas-*` systemd units, disable/remove old `yass-*`, `daemon-reload`,
   enable + start.
5. Update `~/.zshrc` source line + the `github.com/mjacobs/yass` URL comment
   (yadm-managed); commit via yadm.
6. Verify live: server healthy, `/v1/search` round-trips a fresh record, sync
   push/pull works against `yas`. **Only then** remove old `~/.local/bin/yass*`
   and (with confirmation) `DROP DATABASE yass`.

### Phase 4 — Repo rename

- `git remote set-url origin <homelab-git-host>/mj/yas.git`.
- Push the rename (confirm first).
- Gitea-side repo rename `mj/yass` → `mj/yas` is done in the Gitea web UI (no API
  token available locally) — or via API if a token is provided.
- **Last:** `mv ~/dev/projects/yass ~/dev/projects/yas` (changes this session's
  cwd — done dead-last, with a heads-up to `cd`).

### Phase 5 — Bookkeeping

- `.kata.toml` project name already renamed in Phase 1; confirm `kata` still
  resolves the project.
- Update agent memory files referencing the old name and paths.

## Risk handling

- Destructive steps (drop old DB, delete old binaries/dirs) run **only after**
  live verification, each behind an explicit confirmation.
- The local SQLite replica can be rebuilt from a Postgres pull, so the local
  migration is low-risk even if a step is missed.
- Working-directory rename is dead-last to avoid breaking in-flight tooling.
