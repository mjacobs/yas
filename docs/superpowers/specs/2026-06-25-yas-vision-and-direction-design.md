# yas — Grand Vision & Product Direction

**Date:** 2026-06-25
**Status:** draft, pending review
**Type:** vision / positioning (sets direction; does not change behavior yet)

## Why this doc

yas began as a deliberate *refusal of a vision*: where atuin ships an opinionated
TUI + sync + stats product, yas ships "no UI — just a recorder, a local store, a
sync hub, and a JSON contract; bring your own front-end." That refusal is sound,
but it left the project without a *grander* story at a moment when every neighbor
in the space is claiming one (atuin: "magical shell history" → runbooks → an AI
agent; suvadu: "Not just history. Memory. / Shared memory for your AI agents";
warp: an agentic terminal). This doc settles what yas's grand vision *is* — and,
just as importantly, what it deliberately is **not** — so the roadmap has a spine.

The positioning below is grounded in a competitive + ecosystem review (atuin,
mcfly, suvadu, hishtory, zsh-histdb, resh, warp, amazon q, wave) and a read of the
author's adjacent projects (agentsview, roborev, memex). The load-bearing findings
are summarized inline; the resolution is a sequence of locked decisions.

## The one-line vision

> **yas is a deterministic *execution substrate*: a queryable, self-hosted record
> of everything you — and your agents — have run, exposed as a stable JSON/MCP
> contract that any UI or agent reasons over. The value is the data and the
> contract, never a bundled UI or model.**

**Lead tagline (the "promise" register):**
*"A queryable record of everything you and your agents have run — that you
actually own."*

Alternate registers, same vision (kept for marketing/README use):
- Architect: *"Own the history API, not the history UI."*
- Grand/ecosystem: *"The execution layer of your dev life — every command, every
  machine, every agent, self-hosted end to end."*

The playful subtitle stays: *"you want a shell search? yas."*

## The white space (why this vision is yas's and only yas's)

The category fractures on two axes, and every rival sits at a corner that leaves
yas's corner nearly empty:

- **Privacy/sync:** atuin & hishtory are sync-first (but pull toward a SaaS / hosted
  cloud); mcfly & suvadu are 100%-local single-machine (sync is manual
  export/import — suvadu's roadmap *explicitly excludes* sync); warp is
  cloud/team-first.
- **Surface:** *all* of them ship a coupled, opinionated TUI and treat their SQLite
  schema as either private-and-unreachable or the de-facto integration point.
  **None offers a stable machine-facing HTTP+JSON query contract** a third-party
  UI/agent/dashboard can target.

yas already holds, simultaneously, the three corners nobody else combines:
schema-private / **JSON-contract-public**, **genuinely self-hosted cross-machine
sync** (local SQLite replica → your own homelab Postgres, no SaaS), shipped as a
**cgo-free static binary**. The grand vision is to *own that intersection on
purpose* and add the one thing the agentic era needs — agent-readiness baked into
the data model rather than bolted on.

The deepest, least-copyable part of the moat is **composability**: yas is the
command/execution instance of the author's wider personal-dev-telemetry pattern —
the same local-first-capture → SQLite/FTS replica → idempotent-upsert-by-client-id
→ monotonic-`seq` change feed → one shared homelab Postgres → private-schema /
public-JSON-or-MCP-contract shape shared by **agentsview** (agent sessions),
**roborev** (commit-review verdicts), and **memex** (thoughts). Competitors
structurally cannot follow there: they have only the command stream.

## Locked decisions

These were settled through brainstorming and are the spine of everything below.

### D1 — Identity: "The History API," grandeur via composability

yas's identity is the **stable JSON/MCP contract over a private, refactorable
store** — legible and composable enough to **stand alone *and* compose** into a
wider ecosystem. The grandeur comes from being a first-class, MCP-exposed citizen
of that ecosystem (agentsview now; a kenn.io toolkit / personal telemetry mesh as
it grows), **not** from climbing the stack into an AI app or bundling a UI. A
personal mesh-node alone is illegible to outsiders; a stable contract is precisely
what lets yas be a first-class tool *and* a mesh node at once.

### D2 — Character: a pure deterministic substrate (no model, ever)

yas runs **zero inference**. All reasoning — natural-language/semantic search,
"what failed and why," risk judgement — is done by whatever agent queries yas over
MCP (this is, notably, suvadu's *actual* design despite its loud "AI" framing). This:

- turns the **cgo-free invariant from a constraint into a feature** — a
  deterministic substrate behind a stable contract is the most composable thing
  you can hand a toolkit, and the opposite of atuin's hosted-AI lock-in;
- makes **"agentic" a property of the clients, not of yas** — so yas is
  agent-native without being an "AI product" that rots as models change;
- does **not** lose NL/semantic search — it moves to the client (the agent
  translates NL → structured `/v1/search` calls), exactly as `agentsview-mcp`
  already works.

The one honest tradeoff — **no server-side semantic/vector recall** — has a
pre-approved escape hatch: an **optional, out-of-process sidecar**, applying the
existing precedent (*"DuckDB stays an optional out-of-process analytics tool
only"*, ADR 0001). The core stays cgo-free and offline-first regardless.

### D3 — Contract posture: lean v1 now, reserve the seams

Because the contract *is* the product, it cannot be casually churned — but it is
still young. Resolution: **freeze and version `/v1` now** with today's proven
fields plus the one clearly-needed agent-era field, and **reserve** the ecosystem
join seam as nullable so it can light up later with no premature cross-repo
coupling.

```
v1 record (frozen, versioned):
  id, command, host, cwd, session, exit_code,
  duration_ms, start/finish timestamps, deleted   # proven today
  + executor    # 'human' | 'claude-code' | 'codex' | 'ci' | ...
  + corr_id     # nullable; reserves the agentsview session-join seam
```

`corr_id` is a **cross-tool correlation key** (which agent *session* a command
belongs to), deliberately distinct from the existing per-shell `session` field —
it is the seam that will later join a yas command to an agentsview session. It
stays nullable and unused until Chapter 4, so reserving it costs nothing now.

`seq` remains sync-transport-only and never appears in record JSON (unchanged
invariant). Everything beyond this arrives as **additive `v1.x`**. "Stable
contract" becomes a true, testable promise immediately (contract test +
`/v1/version`), and the `executor` wedge — the one axis suvadu leads and atuin
fumbles — ships in the same move.

## What yas is, and stays

- **Still refuses, now as vision features (not just non-goals):** no model bundled
  in core; no SaaS; and — the key nuance — **the core never seizes your keys or
  screen.**
- **A face is allowed, strictly as a client.** A dashboard or even a TUI is fair
  game *as a client over the contract*. A "hi"-style **web dashboard** over a
  future *global* query API (the existing stretch goal) is explicitly in-bounds —
  a detachable client, never a terminal takeover. (Reference: an internal Google
  tool, "hi", that piped history to common infra and exposed a good web view; the
  lesson is *central infra + a good web view*, not a per-terminal widget.)
- **Agent commands are first-class, not hidden.** Where atuin hides agent rows in
  its UI by default, yas makes `executor` a queryable field; **clients choose** to
  filter (`/v1/search?executor=$all-human`), the substrate never decides for them.
- **Capture policy:** keep the minimal redaction yas has (`ignore_patterns`); treat
  **multiline-command correctness** as a substrate requirement (both suvadu and
  mcfly botch this — a quiet quality moat); push everything else to clients.

### Evolved non-goal: ctrl-R and up-history

The README's blunt *"no Ctrl-R hijack"* non-goal is refined to **"no *forced*
hijack by the core."** The genuinely offensive pattern is atuin putting a
full-screen TUI dialog on the **up key** — a keystroke pressed hundreds of times a
day. That is out, always: **yas never touches the up key.**

A client that binds ctrl-R, an inline autosuggestion, or a TUI is fair game and
*supported* — the user opts in by installing it. The practical consequence for the
core: the **local query path must be fast enough to back an interactive widget**
(suvadu advertises <10ms at 1M rows; that's the bar a ctrl-R client sets).

## The two seams (the proof of the vision)

yas has **two user seams hitting one contract**, and that duality *is* the
demonstration that "own the contract, not the UI" works:

> **Up = local & recent** (native zsh, untouched).
> **Ctrl-R = global & searchable** (the user's own fzf, fed by yas's cross-machine
> store).

### Human seam — dogfood the author's real zsh workflow

The author runs `zsh-history-substring-search` on up/down (inline, no dialog) and
fzf's `fzf-history-widget` on ctrl-R (with custom `FZF_CTRL_R_OPTS`). The
integration honors this exactly:

1. **Up/Down: yas keeps its hands off entirely.** Native zsh + the substring-search
   plugin over `$HISTFILE`, unchanged. yas records in the background; it does not
   touch the up key.
2. **Ctrl-R: re-point the existing fzf widget at yas.** A small (~10-line) zsh
   override sources fzf's candidate list from `yas` (e.g. `yas search --json`)
   instead of `fc -rl`, keeping the user's `FZF_CTRL_R_OPTS` verbatim. **Same fzf
   UX, but ctrl-R now searches every command from every machine** — not just local
   `$HISTFILE`.

This is "bring your own UI" proven in the most literal way: **fzf is the UI, yas is
the data, `yas search --json` is the seam.** The reference recall client is
therefore *not a TUI yas builds* — it is a tiny shim wiring an existing, loved
client to yas. Anyone wanting a full atuin-style widget builds it as their own
client.

**Guardrail:** the reference recall shim stays minimal (wire fzf to yas, nothing
more). Anything fancier — theming, preview panes, custom keybindings — is someone
else's client over the same API, by design. This is the line that keeps yas from
drifting back into the atuin-TUI it defines itself against.

### Agent seam — `yas-mcp`

A read-only MCP server (`cmd/yas-mcp`) that is a thin client of a running `yas
serve`, never touching the DB — copying the proven `agentsview-mcp` shape verbatim,
including the self-reference guard that excludes the in-flight session. It exposes
the contract as MCP tools (`search_commands`, `recent_commands`, `command_status`,
`what_failed`, …) so any coding agent reasons over the same store the human's
ctrl-R hits.

**The punchline:** the human's ctrl-R recall and the agent's MCP query are the
**same records, the same `/v1/search`** — two clients, one execution memory.

### MCP posture

- **Pull-only core** (locked): the substrate/contract never pushes; it only answers
  queries. Minimal, no privacy surface.
- **Opt-in context injection** lives one layer up, *in `yas-mcp` only*, **off by
  default**: the suvadu "context before the first prompt" move (auto-loading
  curated MCP *resources* — e.g. "recent failures across the fleet" — into an
  agent's context at session start). It is off by default because it decides *what
  every agent sees* and is a privacy/noise surface. The vision merely commits to
  **not walling it off**; shipping it is later/optional (possibly a stretch).

## Ecosystem composition (the north star)

yas is the command/execution node of the author's unified personal-dev-telemetry
mesh. The destination is reached **by landing one consumer, not by building the
mesh up front** (that up-front build is exactly atuin's scope-sprawl failure mode):

- yas records already sync to the shared homelab Postgres.
- The end-state proof is **one** cross-stream synthesis read: a deterministic SQL +
  JSON-contract query contributing a "commands I ran today, grouped by
  host/project, with failures flagged" section to the author's existing
  `auto-review` daily check-in note — joining to agentsview sessions where the
  reserved `corr_id`/session key matches.

A kenn.io-toolkit future (yas as the execution layer beneath kata's task layer) is
left open by the contract identity, at zero cost, and not pursued further yet.

## Rollout (sequence, not scope)

Ordered to **prove both seams over the one contract, human-first** (the author will
daily-drive the human seam, which is the best forcing function for quality). kata
remains source-of-truth for status; these become new milestones after M6-packaging
and are not renumbered here.

1. **Agent-aware records + frozen `/v1`.** Add `executor` + nullable `corr_id` to
   the canonical Record; plumb through SQLite store, sync (LWW/idempotent upsert
   intact), and `/v1/search?executor=`. Version the contract (`/v1/version` +
   contract test). Small, sacred-path-safe, additive. Source `executor` from the
   zsh hook env (detect agent env vars) and an explicit `--author` flag on `yas
   record start` so agent hooks self-tag.
2. **Human seam / dogfood.** Finish M6 import so the author's real `~/.zsh_history`
   (and atuin) lands; ensure live capture is complete; ship the **fzf-ctrl-R-over-
   yas** shim. *This is the human-seam demo.*
3. **`yas-mcp`.** The agent-seam demo, same contract.
4. **Agentsview seam → mesh.** When a shared session-identity contract is ready,
   light up `prompt → session → command → exit` joins; then the one synthesis
   consumer (above) and, optionally, the "hi"-style global web dashboard over a
   global query API.

## Open / deferred decisions (not blocking the vision)

- **Cross-repo session-identity contract with agentsview** — the highest-leverage
  Chapter-4 move, but a standing coupling tax. Reserved via `corr_id`; the actual
  shared-id semantics are designed only when Chapter 4 starts. Until then, streams
  can join loosely by time/host.
- **Web dashboard timing** — in-bounds as a client; scheduled no earlier than
  Chapter 4 (needs the global query API). Not core.
- **Opt-in MCP context injection** — design exists in posture; build is later/
  optional/off-by-default. May remain a stretch.
- **Capture-policy depth** — multiline correctness is in scope (substrate quality);
  richer redaction / agent-noise handling beyond `executor`-at-query-time stays
  client-side unless proven otherwise.
- **Sidecar for semantic recall** — only if server-side semantic/vector recall
  becomes essential; out-of-process, per ADR 0001 precedent.

## Relationship to existing invariants

This vision **reinforces** every load-bearing invariant in `CLAUDE.md` rather than
bending one: cgo-free static binary (D2 makes it a feature), SQLite WAL multi-
process local store, JSON-surface-as-contract / schema-private (D1/D3 are the
sharpest statement of it), the sacred fast/offline record path (Chapter-1 changes
are additive and must not slow it), and `[]`-not-`null` empty results. The only
revision is softening the README's "no Ctrl-R hijack" non-goal to "no *forced*
hijack by the core" (above).
