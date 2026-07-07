# Design: yas web UI — a fleet-wide dashboard client over the query API

Date: 2026-07-07
Status: Approved (brainstorm), pending implementation plan
Tracking: kata#meex (epic)

## Goal

Give yas a web face: a search/timeline dashboard in the spirit of the best
fleet-history dashboards — instant search over all history, per-host/per-dir
slicing, timeline browsing, shareable links — built **strictly as a client
over the frozen `/v1` JSON contract**. The vision spec
([2026-06-25](2026-06-25-yas-vision-and-direction-design.md)) already ruled
this in-bounds: "a face is allowed, strictly as a client," never a terminal
takeover, never a schema consumer.

## Grounding: the hishtory check (2026-07-07)

Before designing, we evaluated [ddworken/hishtory](https://github.com/ddworken/hishtory)
(3.1k stars) as a possible reason to stop yas development. Verdict:
**inspiration, not killer** (full analysis on kata#meex):

- hishtory's E2E encryption means its server only holds encrypted blobs, so a
  rich server-side global dashboard is architecturally impossible for it; its
  web UI is a temporary client-served sharing page behind HTTP Basic Auth. The
  central-infra-serves-the-view property this epic is chasing is exactly what
  it traded away. yas's self-hosted plaintext Postgres keeps that door open.
- hishtory has no machine-facing contract (TUI + raw SQLite only): no JSON
  API, no MCP, no executor/corr_id, no mesh joins.
- Steals adopted here: its search-token query language, and (as follow-ons)
  duplicate-collapsing and default filters. Its custom-columns feature
  reinforces kata#xvt6 (capture git repo-root per record).

## Decisions (settled in brainstorm)

- **Data plane: the local replica.** The UI targets the existing localhost
  query API (`yas serve`). Because sync already lands every machine's records
  in every replica, the "local" UI is fleet-wide in content — the global
  view without building the server-side global query API. That API (an
  always-on dashboard at a homelab URL) stays a filed follow-on, not v1.
- **Delivery: embedded, one binary.** UI assets ship inside `yas` via
  `go:embed` (pure Go — `make cross` unaffected) and are served by the
  existing `yas serve` at `/ui/`, always on. No new artifact, no install step.
  "Client, not core" is enforced by contract, not process separation: the UI's
  only data path is same-origin `fetch('/v1/...')`.
- **Stack: no-build vanilla JS.** Hand-written HTML/CSS/JS (ES modules),
  no Node toolchain, no framework, no external assets (works offline).
  `make build` stays go-only.
- **v1 scope:** search+timeline page, session detail, record detail, plus a
  duration column in the CLI (see below). Digest view, stats, and the global
  query API are follow-on children of the epic, not v1.

## Architecture

```
cmd/yas serve ──► internal/queryapi   GET /v1/search|version|healthz  (unchanged)
                                      GET /ui/*  ──► go:embed static assets
                                                      │
browser ◄── HTML/CSS/JS (vanilla) ◄───────────────────┘
   │
   └── fetch('/v1/search?...')  ← the ONLY data path; same-origin, no CORS
```

- Assets live in a new `internal/webui/` package: `webui.go` (embed + handler)
  plus `static/` (index.html, app.css, JS modules).
- `internal/queryapi` mounts the webui handler at `/ui/`. `/` redirects to
  `/ui/` (nothing else claims the root today).
- **Contract impact: one additive param.** Search+timeline and session detail
  both ride the existing `GET /v1/search` (session detail is `?session=<id>`;
  `duration_ms` is already in record JSON). The only gap: the CLI's `--failed`
  filter (`store.Query.FailedOnly`) is not exposed over HTTP, so `/v1/search`
  gains a `failed=true` boolean param — additive, no version bump. No other
  endpoint or contract changes.

## The pages

**Search + timeline (home).** One search box over everything, hishtory-style
tokens parsed client-side into `/v1/search` params:

| Token | Maps to |
| --- | --- |
| free text | `q` (FTS) |
| `host:pine` | `host=pine` |
| `cwd:/home/mj/dev` | `cwd=...` |
| `exit:127` | `exit=127` |
| `failed` | `failed=true` (new additive param, see above) |
| `executor:$all-human` / `executor:claude` | `executor=...` |
| `session:<id>` | `session=<id>` |
| `before:2026-07-01` / `after:2026-06-01` | `until=` / `since=` |

Results render as a chronological timeline: command (monospace), host, cwd,
exit badge, duration, relative time. Infinite scroll via `limit`/`offset`.
**All view state lives in the URL query string** — every search is a
shareable, bookmarkable link.

**Session detail.** Clicking a record's session id navigates to
`/ui/?session=<id>` — the same timeline filtered to that session, ordered
oldest-first: the web render of `yas session`. Deep-linkable.

**Record detail.** Clicking a command expands it inline to show the full
record fields (id, session, executor, corr_id, duration, exact timestamps).
No separate page; the expanded state is not URL-persisted in v1.

## CLI duration column (non-UI slice)

Runtime is already captured end-to-end (the zsh hook computes
`--duration-ms` from `EPOCHREALTIME`; `duration_ms` is in the JSON contract,
nil until finish and for imported rows). Only display is missing:

- `yas history` and `yas search` human renders gain a duration column,
  humanized (`85ms`, `1.2s`, `3m40s`), blank when nil.
- A `--no-duration` flag matches the existing `--no-time`/`--no-exit` pattern.
- JSON output is already correct — untouched.

## Aesthetics

**Terminal-native, refined.** Dark-first palette drawn from catppuccin mocha
(matching the owner's tmux/ecosystem), with a light theme via
`prefers-color-scheme`. Commands render in a real monospace stack; UI chrome
in a clean sans. Color is semantic: green/red exit badges, muted host/dir
tints, one accent color. It should feel like a polished tool, not a website.

**The cat nod — small, never the meme photo in the chrome:** a `=^..^=`
kaomoji beside the "yas" wordmark in the header, a cat favicon, and the cat
appearing in the empty-state ("no matching history") message.

## Security posture

`yas serve` binds 127.0.0.1 by default and has no auth; the UI inherits that.
Document plainly: binding to a non-loopback address exposes your full history
unauthenticated — don't, until the global query API (with auth) exists. No new
attack surface beyond what `/v1` already serves; the UI adds only static
assets.

## Testing

- **Go (`httptest`):** `/ui/` serves index.html with the right content types;
  `/` redirects; `/v1/*` behavior unchanged.
- **Token parser:** the one real unit of JS logic lives in a single pure ES
  module; a shared spec table (query string → expected `/v1/search` params)
  is golden-tested from Go so the repo stays Node-free.
- **Smoke:** `make smoke` grows a curl check that `/ui/` returns HTML
  alongside the existing `/v1/search` checks.
- **Visual:** screenshot passes with browser tooling during development;
  not CI-gated.

## Epic breakdown (kata children of meex, in order)

1. **webui-skeleton** — `internal/webui` embed + `/ui/` serving + smoke.
2. **duration column** — CLI history/search renders (tiny, independent; can
   land first).
3. **search + timeline page** — token query box, URL-synced state, infinite
   scroll, the visual system.
4. **session + record detail** — session-filtered view, inline record expand.

Follow-ons (filed under the epic, not v1): digest view (`GET /v1/digest` +
page), stats/sparklines, duplicate-collapsing and default filters, the
server-side global query API + auth (the true always-on central dashboard).

## Non-goals (v1)

- No server-side rendering, no Node/npm, no framework, no CDN assets.
- No auth (localhost-only posture documented instead).
- No new `/v1` endpoints; no contract version bump.
- No write operations from the UI (redaction/deletion stays in the CLI).
