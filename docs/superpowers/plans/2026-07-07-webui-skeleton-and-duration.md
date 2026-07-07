# Web UI slice 1 — /ui/ skeleton + CLI duration column — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the first two children of the web UI epic (kata#meex): a
humanized duration column in `yas history` / `yas search` (kata#zh7h), and the
embedded `/ui/` skeleton served by `yas serve` (kata#y2c9).

**Architecture:** Duration is display-only — `duration_ms` is already captured
and in the JSON contract; we add a render column + `--no-duration` flags. The
UI skeleton is a new `internal/webui` package embedding static assets via
`go:embed` (pure Go), mounted at `/ui/` by `internal/queryapi.NewHandler`,
with `/` redirecting to `/ui/`.

**Tech Stack:** Go stdlib (`embed`, `net/http`), lipgloss (already used for
CLI styling). No new dependencies, no Node, no cgo.

**Spec:** `docs/superpowers/specs/2026-07-07-yas-web-ui-design.md`

## Global Constraints

- cgo-free static binary: nothing here may break `CGO_ENABLED=0` / `make cross`.
- The JSON surface is the contract: `--json` output and all `/v1/*` responses
  are byte-for-byte unchanged by this plan.
- Empty result lists serialize as `[]`, never `null` (don't disturb).
- The record path is sacred: no changes under `yas record`.
- Gate for every commit: `make build test vet lint` green; `make cross` and
  `make smoke` before finishing (smoke output changes in Task 6).
- Commits: small and focused, `Co-Authored-By: Claude Fable 5
  <noreply@anthropic.com>` trailer, body wrapped at 80 columns, NO
  `Claude-Session:` trailer.
- Work on branch `webui/design` (already exists, has the spec commits).

---

### Task 1: `durationField` helper

**Files:**
- Modify: `cmd/yas/main.go` (near `exitField`, ~line 258)
- Test: `cmd/yas/main_test.go`

**Interfaces:**
- Produces: `durationField(r record.Record) string` — humanized duration
  (`"85ms"`, `"1.2s"`, `"3m40s"`, `"1h02m"`), `""` when `r.DurationMS` is nil.
  Tasks 2 and 3 call it.

- [ ] **Step 1: Write the failing test**

Add to `cmd/yas/main_test.go`:

```go
func TestDurationField(t *testing.T) {
	ms := func(n int64) *int64 { return &n }
	cases := []struct {
		name string
		in   *int64
		want string
	}{
		{"nil (unfinished or imported)", nil, ""},
		{"zero", ms(0), "0ms"},
		{"millis", ms(85), "85ms"},
		{"seconds one decimal", ms(1234), "1.2s"},
		{"just under a minute", ms(59949), "59.9s"},
		{"minutes", ms(220000), "3m40s"},
		{"minutes pads seconds", ms(180000), "3m00s"},
		{"hours", ms(3720000), "1h02m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := durationField(record.Record{DurationMS: tc.in})
			if got != tc.want {
				t.Fatalf("durationField(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/yas/ -run TestDurationField -v`
Expected: FAIL — `undefined: durationField`

- [ ] **Step 3: Write minimal implementation**

Add to `cmd/yas/main.go`, right after `exitField`:

```go
// durationField renders a record's wall-clock runtime for the human listings:
// humanized ("85ms", "1.2s", "3m40s", "1h02m"), or "" when no duration was
// captured (still running, or an imported entry). JSON output is untouched —
// duration_ms is already in the contract.
func durationField(r record.Record) string {
	if r.DurationMS == nil {
		return ""
	}
	ms := *r.DurationMS
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms%60_000)/1000)
	default:
		return fmt.Sprintf("%dh%02dm", ms/3_600_000, (ms%3_600_000)/60_000)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/yas/ -run TestDurationField -v`
Expected: PASS (all subtests)

- [ ] **Step 5: Commit**

```bash
git add cmd/yas/main.go cmd/yas/main_test.go
git commit -m "feat(cli): add durationField humanizer for the listing renders"
```

---

### Task 2: duration column in `yas history`

**Files:**
- Modify: `cmd/yas/main.go` — `historyOpts` (~line 609), `parseHistoryArgs`
  (~line 641), `renderHistory` (~line 777), `renderHistoryRich` (~line 376),
  usage text (~line 1486)
- Test: `cmd/yas/main_test.go`

**Interfaces:**
- Consumes: `durationField` (Task 1).
- Produces: `historyOpts.showDuration bool` (default true; `--no-duration`
  clears it). Column order: `# WHEN <exit> TOOK SESS COMMAND`; rich header
  label is `TOOK`, right-padded like the others.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/yas/main_test.go`:

```go
func TestHistoryDurationColumn(t *testing.T) {
	ms := func(n int64) *int64 { return &n }
	exit0 := 0
	recs := []record.Record{
		{Command: "sleep 2", StartTime: time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC),
			ExitCode: &exit0, DurationMS: ms(2100), Session: "abc123def456"},
		{Command: "true", StartTime: time.Date(2026, 7, 7, 10, 0, 5, 0, time.UTC),
			ExitCode: &exit0, DurationMS: nil, Session: "abc123def456"},
	}
	styles := newCLIStyles(false)

	t.Run("shown by default, blank when nil", func(t *testing.T) {
		var b strings.Builder
		opts := historyOpts{layout: defaultHistTimeLayout, showTime: true,
			showExit: true, showSession: true, showDuration: true}
		if err := renderHistory(&b, recs, 1, time.UTC, opts, styles); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if !strings.Contains(out, "2.1s") {
			t.Fatalf("expected duration 2.1s in output:\n%s", out)
		}
		// The nil-duration row still aligns: the command column follows padding.
		if !strings.Contains(out, "true") {
			t.Fatalf("expected nil-duration row rendered:\n%s", out)
		}
	})

	t.Run("--no-duration parses", func(t *testing.T) {
		opts, err := parseHistoryArgs([]string{"--no-duration"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.showDuration {
			t.Fatal("expected showDuration=false with --no-duration")
		}
	})

	t.Run("omitted when disabled", func(t *testing.T) {
		var b strings.Builder
		opts := historyOpts{layout: defaultHistTimeLayout, showTime: true,
			showExit: true, showSession: true, showDuration: false}
		if err := renderHistory(&b, recs, 1, time.UTC, opts, styles); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "2.1s") {
			t.Fatalf("expected no duration column:\n%s", b.String())
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/yas/ -run TestHistoryDurationColumn -v`
Expected: FAIL — `unknown field showDuration in struct literal` (compile error)

- [ ] **Step 3: Implement**

In `historyOpts`, after `showSession`:

```go
	showDuration bool // list: include the humanized duration (TOOK) column
```

In `parseHistoryArgs`, after the `noSession` flag:

```go
	noDuration := fs.Bool("no-duration", false, "omit the duration (TOOK) column")
```

and in the `opts := historyOpts{...}` literal, after `showSession`:

```go
		showDuration: !*noDuration,
```

In `renderHistory` (plain path): compute a width and add the cell between the
exit and session columns. After the `exitWidth` block:

```go
	durWidth := 0
	if opts.showDuration {
		for _, r := range recs {
			if n := len(durationField(r)); n > durWidth {
				durWidth = n
			}
		}
	}
```

and in the row loop, after the `opts.showExit` block:

```go
		if opts.showDuration && durWidth > 0 {
			b.WriteString("  ")
			b.WriteString(styles.dim.Render(fmt.Sprintf("%*s", durWidth, durationField(r))))
		}
```

(The `durWidth > 0` guard keeps output byte-identical when no record in the
batch has a duration — e.g. pure imported history.)

In `renderHistoryRich`: extend the width scan loop:

```go
		if opts.showDuration {
			if n := lipgloss.Width(durationField(r)); n > durW {
				durW = n
			}
		}
```

(declare `durW` alongside `timeW, glyphW`: `timeW, glyphW, durW := 0, 0, 0`).
Add the header column after the exit column, only when the column will render:

```go
	if opts.showDuration && durW > 0 {
		if n := lipgloss.Width("TOOK"); n > durW {
			durW = n
		}
		cols = append(cols, padTo("TOOK", durW))
	}
```

and the row cell after the `opts.showExit` block:

```go
		if opts.showDuration && durW > 0 {
			b.WriteString("  ")
			b.WriteString(styles.dim.Render(fmt.Sprintf("%*s", durW, durationField(r))))
		}
```

NOTE: the `durW > 0` header guard must use the same condition as the row cell —
compute `durW` from records first, and only then max it with the header width
inside the guard (as shown), otherwise an all-nil batch renders a header for an
empty column.

Update the history usage line (~line 1486) to include `[--no-duration]`:

```
  yas history [n]                       list the last n entries (default 100),
              [--time-format <layout>] [--no-time] [--no-exit] [--no-duration] [--no-session] [--no-color] [--json]   numbered, oldest first
```

- [ ] **Step 4: Run the tests**

Run: `go test ./cmd/yas/ -run 'TestHistoryDurationColumn' -v` then the whole
package: `go test ./cmd/yas/`
Expected: PASS. If existing history-render tests assert exact output and now
fail, update their expectations to include the new column (that's the feature),
NOT by disabling the column.

- [ ] **Step 5: Commit**

```bash
git add cmd/yas/main.go cmd/yas/main_test.go
git commit -m "feat(history): humanized duration (TOOK) column + --no-duration"
```

---

### Task 3: duration column in `yas search`

**Files:**
- Modify: `cmd/yas/main.go` — `parseSearchArgs` (~line 497), `cmdSearch`
  (~line 474), `doSearch` (~line 232), `renderSearchRich` (~line 438),
  usage text (~line 1484)
- Test: `cmd/yas/main_test.go`

**Interfaces:**
- Consumes: `durationField` (Task 1).
- Produces: `searchOpts` struct replacing the loose bools threaded through
  search rendering:

```go
// searchOpts carries the search listing's presentation flags.
type searchOpts struct {
	asJSON       bool
	noColor      bool
	showSession  bool
	showDuration bool
}
```

  New signatures (update ALL call sites, including existing tests):
  `parseSearchArgs(args []string) (store.Query, searchOpts, error)`,
  `doSearch(ctx context.Context, s recordStore, q store.Query, w io.Writer, opts searchOpts, styles cliStyles) error`,
  `renderSearchRich(w io.Writer, recs []record.Record, styles cliStyles, opts searchOpts) error`.

- [ ] **Step 1: Write the failing tests**

```go
func TestSearchDurationColumn(t *testing.T) {
	t.Run("--no-duration parses", func(t *testing.T) {
		_, opts, err := parseSearchArgs([]string{"--no-duration", "git"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.showDuration {
			t.Fatal("expected showDuration=false with --no-duration")
		}
	})
	t.Run("default shows duration", func(t *testing.T) {
		_, opts, err := parseSearchArgs([]string{"git"})
		if err != nil {
			t.Fatal(err)
		}
		if !opts.showDuration {
			t.Fatal("expected showDuration=true by default")
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/yas/ -run TestSearchDurationColumn -v`
Expected: FAIL (compile error — `parseSearchArgs` returns 5 values today, and
`searchOpts` is undefined)

- [ ] **Step 3: Implement**

1. Add the `searchOpts` struct (near `doSearch`).
2. `parseSearchArgs`: add `noDuration := fs.Bool("no-duration", false, "omit the duration column")`,
   change the returns to `(store.Query, searchOpts, error)` where the opts are
   `searchOpts{asJSON: *asJSON, noColor: *noColor, showSession: !*noSession, showDuration: !*noDuration}`.
   All error returns become `return store.Query{}, searchOpts{}, err`.
3. `cmdSearch`: adapt —

```go
	q, opts, err := parseSearchArgs(args)
	...
	styles := newCLIStyles(!opts.noColor && colorTerminal(os.Stdout))
	if err := doSearch(context.Background(), st, q, os.Stdout, opts, styles); err != nil {
```

4. `doSearch`: signature `(ctx, s, q, w, opts searchOpts, styles cliStyles)`;
   `asJSON` → `opts.asJSON`, `showSession` → `opts.showSession`; the plain
   (non-color) loop gains the duration cell between exit and session:

```go
	durWidth := 0
	if opts.showDuration {
		for _, r := range recs {
			if n := len(durationField(r)); n > durWidth {
				durWidth = n
			}
		}
	}
	for _, r := range recs {
		ts := styles.dim.Render(r.StartTime.UTC().Format("2006-01-02 15:04:05Z"))
		dur := ""
		if opts.showDuration && durWidth > 0 {
			dur = fmt.Sprintf("%*s", durWidth, durationField(r)) + "  "
		}
		sess := ""
		if opts.showSession {
			sess = sessCell(r.Session) + "  "
		}
		if _, err := fmt.Fprintf(w, "%s  %s  %s%s%s\n", ts, styles.exit(r), dur, sess, r.Command); err != nil {
			return err
		}
	}
```

5. `renderSearchRich`: signature `(w, recs, styles, opts searchOpts)`; add the
   `TOOK` column exactly as in `renderHistoryRich` (width scan over
   `durationField`, header `padTo("TOOK", durW)` guarded by
   `opts.showDuration && durW > 0`, dim right-aligned cell between the glyph
   and SESS columns). `showSession` reads from `opts.showSession`.
6. Update the search usage line (~line 1484) to include `[--no-duration]`.
7. Fix every caller the compiler flags (existing tests construct these calls).

- [ ] **Step 4: Run the package tests**

Run: `go test ./cmd/yas/`
Expected: PASS after updating existing call sites/expectations.

- [ ] **Step 5: Full gate + commit**

Run: `make build test vet lint`
Expected: all green.

```bash
git add cmd/yas/main.go cmd/yas/main_test.go
git commit -m "feat(search): duration (TOOK) column + --no-duration; fold search flags into searchOpts"
```

Then comment + close kata#zh7h citing the commits and `make build test vet lint`.

---

### Task 4: `internal/webui` package — embedded assets + handler

**Files:**
- Create: `internal/webui/webui.go`
- Create: `internal/webui/static/index.html`
- Create: `internal/webui/static/app.css`
- Test: `internal/webui/webui_test.go`

**Interfaces:**
- Produces: `webui.Handler() http.Handler` — serves the embedded `static/`
  tree; callers mount it under `/ui/` (it strips that prefix itself).
  Task 5 consumes it from `internal/queryapi`.

- [ ] **Step 1: Write the failing test**

`internal/webui/webui_test.go`:

```go
package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

func TestHandlerServesIndex(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/ui/", "/ui/index.html"} {
		res := get(t, h, path)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s Content-Type = %q, want text/html", path, ct)
		}
		body, _ := io.ReadAll(res.Body)
		if !strings.Contains(string(body), "yas") {
			t.Fatalf("GET %s body missing wordmark:\n%s", path, body)
		}
	}
}

func TestHandlerServesCSS(t *testing.T) {
	res := get(t, Handler(), "/ui/app.css")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/app.css = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("Content-Type = %q, want text/css", ct)
	}
}

func TestHandlerUnknownPathIs404(t *testing.T) {
	if res := get(t, Handler(), "/ui/nope.js"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /ui/nope.js = %d, want 404", res.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webui/ -v`
Expected: FAIL — package does not exist / `undefined: Handler`

- [ ] **Step 3: Implement**

`internal/webui/webui.go`:

```go
// Package webui serves the embedded web UI: a browser client of the /v1 query
// API. Assets are embedded (go:embed, pure Go — the cgo-free static binary is
// preserved) and served under /ui/. The UI's only data path is same-origin
// fetch('/v1/...'): it is a client of the JSON contract, never of the store.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler serves the embedded UI. Mount it under /ui/ — the handler strips
// that prefix itself.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("webui: embedded static tree missing: " + err.Error()) // unreachable: compiled in
	}
	return http.StripPrefix("/ui/", http.FileServer(http.FS(sub)))
}
```

`internal/webui/static/index.html` (skeleton: wordmark + cat + palette; the
search page arrives in kata#stg4):

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>yas =^..^=</title>
  <link rel="icon" href="data:image/svg+xml,<svg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%220 0 100 100%22><text y=%22.9em%22 font-size=%2290%22>🐱</text></svg>">
  <link rel="stylesheet" href="app.css">
</head>
<body>
  <header>
    <h1 class="wordmark">yas <span class="cat" aria-hidden="true">=^..^=</span></h1>
    <p class="tagline">a queryable record of everything you run</p>
  </header>
  <main>
    <p class="empty-state">=^..^= &nbsp;search is on its way — the skeleton is live.</p>
  </main>
</body>
</html>
```

`internal/webui/static/app.css` (catppuccin-mocha-adjacent, dark-first, light
via `prefers-color-scheme`; semantic colors reserved for later slices):

```css
/* yas web UI — terminal-native, refined. Dark-first (catppuccin mocha
   -adjacent); light theme via prefers-color-scheme. Commands render in the
   mono stack; chrome in the sans stack. */
:root {
  --bg: #1e1e2e;        /* mocha base */
  --surface: #313244;   /* mocha surface0 */
  --text: #cdd6f4;      /* mocha text */
  --subtext: #a6adc8;   /* mocha subtext0 */
  --accent: #89b4fa;    /* mocha blue */
  --ok: #a6e3a1;        /* mocha green  (exit 0) */
  --fail: #f38ba8;      /* mocha red    (exit != 0) */
  --font-mono: ui-monospace, "JetBrains Mono", "Fira Code", Menlo, monospace;
  --font-sans: system-ui, -apple-system, "Segoe UI", sans-serif;
}
@media (prefers-color-scheme: light) {
  :root {
    --bg: #eff1f5;      /* latte base */
    --surface: #ccd0da; /* latte surface0 */
    --text: #4c4f69;    /* latte text */
    --subtext: #6c6f85; /* latte subtext0 */
    --accent: #1e66f5;  /* latte blue */
    --ok: #40a02b;      /* latte green */
    --fail: #d20f39;    /* latte red */
  }
}
* { box-sizing: border-box; }
body {
  margin: 0 auto;
  max-width: 72rem;
  padding: 2rem 1.5rem;
  background: var(--bg);
  color: var(--text);
  font-family: var(--font-sans);
}
.wordmark {
  font-family: var(--font-mono);
  font-size: 1.5rem;
  margin: 0;
}
.wordmark .cat { color: var(--accent); font-size: 1rem; }
.tagline { color: var(--subtext); margin: 0.25rem 0 0; }
.empty-state {
  margin-top: 3rem;
  color: var(--subtext);
  font-family: var(--font-mono);
  text-align: center;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webui/ -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/webui/
git commit -m "feat(webui): embedded static UI skeleton (internal/webui)"
```

---

### Task 5: mount `/ui/` in the query API + root redirect

**Files:**
- Modify: `internal/queryapi/queryapi.go` (`NewHandler`, ~line 40)
- Test: `internal/queryapi/queryapi_test.go`

**Interfaces:**
- Consumes: `webui.Handler()` (Task 4).
- Produces: `yas serve` responds on `GET /ui/*` (the UI) and redirects
  `GET /` and `GET /ui` to `/ui/`. `/v1/*` untouched.

- [ ] **Step 1: Write the failing tests**

Add to `internal/queryapi/queryapi_test.go` (match the file's existing test
helper style — it already builds `NewHandler` around a fake/real store; reuse
that setup):

```go
func TestUIMountedAndRootRedirects(t *testing.T) {
	h := NewHandler(emptySearcher{}) // reuse/introduce the file's stub Searcher

	res := doGet(t, h, "/ui/") // reuse the file's request helper, or httptest inline
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /ui/ Content-Type = %q, want text/html", ct)
	}

	for _, path := range []string{"/", "/ui"} {
		res := doGet(t, h, path)
		if res.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("GET %s = %d, want 301", path, res.StatusCode)
		}
		if loc := res.Header.Get("Location"); loc != "/ui/" {
			t.Fatalf("GET %s Location = %q, want /ui/", path, loc)
		}
	}
}
```

If the test file has no stub `Searcher`, add one:

```go
type emptySearcher struct{}

func (emptySearcher) Search(context.Context, store.Query) ([]record.Record, error) {
	return []record.Record{}, nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/queryapi/ -run TestUIMountedAndRootRedirects -v`
Expected: FAIL — `GET /ui/ = 404, want 200`

- [ ] **Step 3: Implement**

In `internal/queryapi/queryapi.go`, import
`"github.com/mjacobs/yas/internal/webui"` and extend `NewHandler`:

```go
	// The embedded web UI is a client of this same /v1 contract, served on the
	// same listener for zero-config dogfooding. GET /{$} matches the bare root
	// only (Go 1.22 pattern), so /v1/* routing is unaffected.
	mux.Handle("GET /ui/", webui.Handler())
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/queryapi/ ./internal/webui/`
Expected: PASS, including all pre-existing queryapi tests (405/400 behavior
unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/queryapi/queryapi.go internal/queryapi/queryapi_test.go
git commit -m "feat(serve): mount the embedded web UI at /ui/, redirect / to it"
```

---

### Task 6: smoke check, docs, full gate, close-out

**Files:**
- Modify: `scripts/smoke.sh` (serve section, after the `/v1/version` check ~line 236)
- Modify: `README.md` (serve bullet — mention the UI at /ui/ + the
  localhost-only security note from the spec)
- Modify: `cmd/yas/main.go` usage line for `serve` (~line 1487)

**Interfaces:**
- Consumes: everything above. Produces: the shipped gate.

- [ ] **Step 1: Add the smoke check**

In `scripts/smoke.sh`, after the `/v1/version` assertion:

```bash
ui_ct="$(curl -fsS -o /dev/null -w '%{content_type}' "http://$ADDR/ui/")"
case "$ui_ct" in text/html*) ;; *) echo "FAIL: /ui/ content-type $ui_ct"; exit 1;; esac
curl -fsS "http://$ADDR/ui/" | grep -q 'yas' || { echo "FAIL: /ui/ missing wordmark"; exit 1; }
```

(Match the script's existing assertion helper style — if it uses `assert_eq`,
use that instead of the inline case/grep.)

- [ ] **Step 2: Update docs**

- `yas serve` usage line: `yas serve  [--addr 127.0.0.1:8765]   localhost HTTP+JSON query API + web UI at /ui/`
- README serve/API section: one sentence that `yas serve` now also serves the
  embedded web UI at `http://127.0.0.1:8765/ui/`, plus the spec's security
  note verbatim in spirit: no auth — binding to a non-loopback address exposes
  your full history; keep it on 127.0.0.1.

- [ ] **Step 3: Full gate**

Run: `make build test vet lint && make cross && make smoke`
Expected: all green, including the new /ui/ smoke check.

- [ ] **Step 4: Visual check**

Run `go run ./cmd/yas serve --addr 127.0.0.1:8765` and screenshot
`http://127.0.0.1:8765/ui/` with the browser tooling (dark + light). Confirm
wordmark, kaomoji, tagline, palette. Kill the server.

- [ ] **Step 5: Commit + close out**

```bash
git add scripts/smoke.sh README.md cmd/yas/main.go
git commit -m "feat(webui): smoke /ui/, document the embedded UI + localhost-only note"
```

- Comment + close kata#y2c9 citing the gate (`make build test vet lint cross
  smoke`) and the commits.
- Comment on kata#meex: slice 1 landed; next is kata#stg4 (search + timeline).
- Run the whole-branch roborev review per repo workflow before any push:
  `roborev review --branch --base main --panel default_security` (skip if the
  hook already covered every commit and you're not pushing yet).
