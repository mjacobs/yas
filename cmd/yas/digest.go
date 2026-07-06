package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
)

// yas digest: a deterministic, cross-stream synthesis of "commands run in a
// window, grouped by host/project, with failures flagged." It reads the local
// replica (like the other CLI commands) and emits a stable JSON contract an
// external consumer can call, plus a human summary.

const (
	// digestScanCap bounds the window scan so a huge --since window can't pull an
	// unbounded result set into memory. Any realistic window — even a heavy,
	// multi-host synced day — stays far under this. A window that exceeds it keeps
	// the most-recent digestScanCap commands (the store returns newest-first, so
	// this is deterministic) and older ones fall out of the digest.
	digestScanCap = 100_000
	// maxFailedPerGroup caps how many distinct failing commands a group surfaces.
	// The Failures count still reflects every failing record; this only bounds the
	// displayed sample so a group that fails the same thing hundreds of times (or
	// many distinct things) stays readable and the JSON stays bounded.
	maxFailedPerGroup = 10
	// maxFailedCommandRunes truncates each surfaced failing command for display so
	// a giant heredoc/paste can't blow up the digest.
	maxFailedCommandRunes = 120
	// digestTimeLayout is the local-time window format for the human digest.
	digestTimeLayout = "2006-01-02 15:04"
)

// Digest is the computed window summary: the [Since, Until) window and the
// per-(host, cwd) groups, deterministically ordered. It is a pure value with no
// I/O so it can be built and asserted in isolation.
type Digest struct {
	Since  time.Time
	Until  time.Time
	Groups []DigestGroup
}

// DigestGroup summarizes every command in one (host, location) group over the
// window: the total Count, how many Failures (non-nil, non-zero exit), and a
// deduped, capped, truncated sample of the distinct failing command strings,
// most-recent-first.
//
// Grouping is by PROJECT when a record carries a git repo root (xvt6): all
// commands under one repo — however deep their cwd — collapse into a single
// group. Records without a repo root (off-repo, or imported before the field
// existed) fall back to grouping by exact cwd. CWD holds the group's display
// location (the repo root for a project group, else the cwd); RepoRoot is set
// only for project groups, so a consumer can tell the two apart.
type DigestGroup struct {
	Host           string
	CWD            string
	RepoRoot       string
	Count          int
	Failures       int
	FailedCommands []string
}

// groupLocation returns the location a record groups under: its git repo root
// when present, else its exact cwd. Empty repo root (off-repo/imported) falls
// back to cwd so those records still group deterministically.
func groupLocation(r record.Record) string {
	if r.RepoRoot != "" {
		return r.RepoRoot
	}
	return r.CWD
}

// isFailure reports whether a record is a finished, non-zero exit. A nil exit
// (still running / never finalized) and an exit of 0 are both NOT failures.
func isFailure(r record.Record) bool {
	return r.ExitCode != nil && *r.ExitCode != 0
}

// buildDigest groups recs by (host, cwd) over [since, until) (Until exclusive),
// counts commands and failures per group, and collects each group's distinct
// failing commands most-recent-first. Groups come out sorted by host asc then
// cwd asc; the returned Groups slice (and every FailedCommands slice) is
// non-nil so an empty digest serializes as [] rather than null. Pure: no I/O.
func buildDigest(recs []record.Record, since, until time.Time) Digest {
	d := Digest{Since: since, Until: until, Groups: []DigestGroup{}}

	// Keep only records inside the window. buildDigest re-applies the window even
	// though the store already filters, so the pure core is correct on any input.
	inWindow := make([]record.Record, 0, len(recs))
	for _, r := range recs {
		if r.StartTime.Before(since) {
			continue
		}
		if !until.IsZero() && !r.StartTime.Before(until) {
			continue // Until is exclusive
		}
		inWindow = append(inWindow, r)
	}

	// Most-recent-first (start_time desc, id desc as a deterministic tiebreak) so
	// each group's failing-command sample is newest-first and dedups stably.
	sort.Slice(inWindow, func(i, j int) bool {
		a, b := inWindow[i], inWindow[j]
		if !a.StartTime.Equal(b.StartTime) {
			return a.StartTime.After(b.StartTime)
		}
		return a.ID > b.ID
	})

	type agg struct {
		group *DigestGroup
		seen  map[string]struct{} // full failing commands already sampled (dedup)
	}
	groups := map[string]*agg{}
	for i := range inWindow {
		r := inWindow[i]
		loc := groupLocation(r)
		// The grouping mode is part of the key: a project group (repo root) and a
		// bare-cwd group must stay distinct even when their location strings are
		// equal (an imported record whose cwd was /repo vs a live record inside the
		// /repo checkout). Without this they'd merge and the emitted repo_root would
		// depend on record order.
		mode := "c"
		if r.RepoRoot != "" {
			mode = "p"
		}
		key := r.Hostname + "\x00" + mode + "\x00" + loc
		a := groups[key]
		if a == nil {
			a = &agg{
				group: &DigestGroup{Host: r.Hostname, CWD: loc, RepoRoot: r.RepoRoot, FailedCommands: []string{}},
				seen:  map[string]struct{}{},
			}
			groups[key] = a
		}
		a.group.Count++
		if isFailure(r) {
			a.group.Failures++
			// Dedup on the full command, then store a truncated copy for display.
			if _, dup := a.seen[r.Command]; !dup && len(a.group.FailedCommands) < maxFailedPerGroup {
				a.seen[r.Command] = struct{}{}
				a.group.FailedCommands = append(a.group.FailedCommands, truncateRunes(r.Command, maxFailedCommandRunes))
			}
		}
	}

	for _, a := range groups {
		d.Groups = append(d.Groups, *a.group)
	}
	// Order by (host, cwd, repo_root). RepoRoot is the final tiebreak so a
	// project group and a bare-cwd group that share a display location ("" sorts
	// before the repo path) still order deterministically — the grouping key
	// already keeps them as separate groups.
	sort.Slice(d.Groups, func(i, j int) bool {
		if d.Groups[i].Host != d.Groups[j].Host {
			return d.Groups[i].Host < d.Groups[j].Host
		}
		if d.Groups[i].CWD != d.Groups[j].CWD {
			return d.Groups[i].CWD < d.Groups[j].CWD
		}
		return d.Groups[i].RepoRoot < d.Groups[j].RepoRoot
	})
	return d
}

// truncateRunes shortens s to at most max runes, appending an ellipsis when it
// had to cut. Rune-based so a multibyte command is never split mid-character.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	// The ellipsis counts toward the budget, so keep max-1 content runes: the
	// result is at most max runes, as documented.
	return string(runes[:max-1]) + "…"
}

// digestEnvelope is the JSON contract for `yas digest --json`. The window times
// are RFC3339 strings and groups/failed_commands are always emitted (never
// null), matching yas's empty-list-as-[] contract invariant. seq never appears
// because the digest is shaped from these fields alone.
type digestEnvelope struct {
	Since  string            `json:"since"`
	Until  string            `json:"until"`
	Groups []digestGroupWire `json:"groups"`
}

type digestGroupWire struct {
	Host           string   `json:"host"`
	CWD            string   `json:"cwd"`
	RepoRoot       string   `json:"repo_root,omitempty"` // set only for git-project groups; a bare-cwd group omits it
	Count          int      `json:"count"`
	Failures       int      `json:"failures"`
	FailedCommands []string `json:"failed_commands"`
}

// renderDigest writes d to w: the JSON contract (asJSON) or a human summary.
// styles color the human summary; it is ignored for JSON.
func renderDigest(w io.Writer, d Digest, asJSON bool, styles cliStyles) error {
	if asJSON {
		return renderDigestJSON(w, d)
	}
	return renderDigestHuman(w, d, styles)
}

// renderDigestJSON emits the digest envelope. Non-nil slices are built
// explicitly so an empty digest serializes as {"...","groups":[]}.
func renderDigestJSON(w io.Writer, d Digest) error {
	env := digestEnvelope{
		Since:  d.Since.Format(time.RFC3339),
		Until:  d.Until.Format(time.RFC3339),
		Groups: make([]digestGroupWire, 0, len(d.Groups)),
	}
	for _, g := range d.Groups {
		fc := g.FailedCommands
		if fc == nil {
			fc = []string{}
		}
		env.Groups = append(env.Groups, digestGroupWire{
			Host:           g.Host,
			CWD:            g.CWD,
			RepoRoot:       g.RepoRoot,
			Count:          g.Count,
			Failures:       g.Failures,
			FailedCommands: fc,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

// renderDigestHuman writes a readable grouped summary: a window header, then one
// block per (host, cwd) with its count and (when any) failure count and the
// sampled failing commands. With color on it accents the title, dims the
// path/counts, reds the failures, and adds a rule under the title; with color
// off the output is byte-for-byte the plain text (every style is a passthrough
// and the rule is omitted). An empty window prints a friendly line.
func renderDigestHuman(w io.Writer, d Digest, styles cliStyles) error {
	since := d.Since.Format(digestTimeLayout)
	until := d.Until.Format(digestTimeLayout)
	if len(d.Groups) == 0 {
		_, err := fmt.Fprintf(w, "yas digest: no commands from %s to %s\n", since, until)
		return err
	}
	title := styles.accent.Render("yas · digest") + "  " + styles.dim.Render(since+" → "+until)
	if _, err := fmt.Fprintln(w, title); err != nil {
		return err
	}
	if styles.color {
		if _, err := fmt.Fprintln(w, styles.dim.Render(strings.Repeat("─", lipgloss.Width(title)))); err != nil {
			return err
		}
	}
	for _, g := range d.Groups {
		meta := styles.dim.Render(fmt.Sprintf("%s — %d %s", g.CWD, g.Count, plural(g.Count, "command", "commands")))
		line := "\n" + g.Host + "  " + meta
		if g.Failures > 0 {
			line += styles.fail.Render(fmt.Sprintf(", %d failed", g.Failures))
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		for _, c := range g.FailedCommands {
			if _, err := fmt.Fprintf(w, "  %s %s\n", styles.fail.Render("✗"), c); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseDigestArgs parses `yas digest` flags against now: --since/--until
// (RFC3339) and --json. The default window is today — local midnight of now
// through now. It rejects an inverted window and any stray positional argument.
func parseDigestArgs(args []string, now time.Time) (since, until time.Time, asJSON, noColor bool, err error) {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinceStr := fs.String("since", "", "window start, inclusive (RFC3339); default: local midnight today")
	untilStr := fs.String("until", "", "window end, exclusive (RFC3339); default: now")
	jsonOut := fs.Bool("json", false, "emit the JSON digest contract instead of the human summary")
	noColorOut := fs.Bool("no-color", false, "disable colorized output")
	if perr := fs.Parse(args); perr != nil {
		return time.Time{}, time.Time{}, false, false, perr
	}
	if fs.NArg() > 0 {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("digest takes no positional arguments; got %q", fs.Arg(0))
	}

	since = startOfDay(now)
	if *sinceStr != "" {
		if since, err = time.Parse(time.RFC3339, *sinceStr); err != nil {
			return time.Time{}, time.Time{}, false, false, fmt.Errorf("invalid --since %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", *sinceStr)
		}
	}
	until = now
	if *untilStr != "" {
		if until, err = time.Parse(time.RFC3339, *untilStr); err != nil {
			return time.Time{}, time.Time{}, false, false, fmt.Errorf("invalid --until %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", *untilStr)
		}
	}
	if until.Before(since) {
		return time.Time{}, time.Time{}, false, false, fmt.Errorf("--until %s is before --since %s", until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	return since, until, *jsonOut, *noColorOut, nil
}

// startOfDay returns local midnight of t (in t's own location).
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// cmdDigest implements `yas digest [--since t] [--until t] [--json]`: it reads
// the window from the local replica (like the other CLI commands), builds the
// deterministic per-(host, cwd) digest, and renders it.
func cmdDigest(args []string) {
	since, until, asJSON, noColor, err := parseDigestArgs(args, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas digest:", err)
		os.Exit(2)
	}
	styles := newCLIStyles(!noColor && colorTerminal(os.Stdout))
	st, _, closeStore := openStore()
	defer closeStore()

	recs, err := st.Search(context.Background(), store.Query{
		Since: since,
		Until: until,
		// Don't count the in-flight `yas digest` command itself (the zsh hook
		// exports its record id before this process runs).
		ExcludeID: os.Getenv("YAS_RECORD_ID"),
		Limit:     digestScanCap,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas digest:", err)
		os.Exit(1)
	}
	if err := renderDigest(os.Stdout, buildDigest(recs, since, until), asJSON, styles); err != nil {
		fmt.Fprintln(os.Stderr, "yas digest:", err)
		os.Exit(1)
	}
}
