// Package digest computes a deterministic, cross-stream synthesis of
// "commands run in a window, grouped by host/project, with failures flagged."
// It is the shared core behind `yas digest` (CLI) and GET /v1/digest (query
// API): a pure value computation over records plus the stable JSON envelope
// both surfaces emit.
package digest

import (
	"sort"
	"time"

	"github.com/mjacobs/yas/internal/record"
)

const (
	// ScanCap bounds the window scan so a huge window can't pull an unbounded
	// result set into memory. Any realistic window — even a heavy, multi-host
	// synced day — stays far under this. A window that exceeds it keeps the
	// most-recent ScanCap commands (the store returns newest-first, so this is
	// deterministic) and older ones fall out of the digest.
	ScanCap = 100_000
	// maxFailedPerGroup caps how many distinct failing commands a group surfaces.
	// The Failures count still reflects every failing record; this only bounds the
	// displayed sample so a group that fails the same thing hundreds of times (or
	// many distinct things) stays readable and the JSON stays bounded.
	maxFailedPerGroup = 10
	// MaxFailedCommandRunes truncates each surfaced failing command for display so
	// a giant heredoc/paste can't blow up the digest.
	MaxFailedCommandRunes = 120
)

// Digest is the computed window summary: the [Since, Until) window and the
// per-(host, location) groups, deterministically ordered. It is a pure value
// with no I/O so it can be built and asserted in isolation.
type Digest struct {
	Since  time.Time
	Until  time.Time
	Groups []Group
}

// Group summarizes every command in one (host, location) group over the
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
type Group struct {
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

// Build groups recs by (host, location) over [since, until) (Until exclusive),
// counts commands and failures per group, and collects each group's distinct
// failing commands most-recent-first. Groups come out sorted by host asc then
// cwd asc; the returned Groups slice (and every FailedCommands slice) is
// non-nil so an empty digest serializes as [] rather than null. Pure: no I/O.
func Build(recs []record.Record, since, until time.Time) Digest {
	d := Digest{Since: since, Until: until, Groups: []Group{}}

	// Keep only records inside the window. Build re-applies the window even
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
		group *Group
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
				group: &Group{Host: r.Hostname, CWD: loc, RepoRoot: r.RepoRoot, FailedCommands: []string{}},
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
				a.group.FailedCommands = append(a.group.FailedCommands, truncateRunes(r.Command, MaxFailedCommandRunes))
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

// Envelope is the stable JSON contract for a digest, shared byte-for-byte by
// `yas digest --json` and GET /v1/digest. The window times are RFC3339
// strings and groups/failed_commands are always emitted (never null),
// matching yas's empty-list-as-[] contract invariant. seq never appears
// because the digest is shaped from these fields alone.
type Envelope struct {
	Since  string      `json:"since"`
	Until  string      `json:"until"`
	Groups []GroupWire `json:"groups"`
}

// GroupWire is one group in the JSON envelope.
type GroupWire struct {
	Host           string   `json:"host"`
	CWD            string   `json:"cwd"`
	RepoRoot       string   `json:"repo_root,omitempty"` // set only for git-project groups; a bare-cwd group omits it
	Count          int      `json:"count"`
	Failures       int      `json:"failures"`
	FailedCommands []string `json:"failed_commands"`
}

// ToEnvelope shapes d into the JSON contract. Non-nil slices are built
// explicitly so an empty digest serializes as {"...","groups":[]}.
func ToEnvelope(d Digest) Envelope {
	env := Envelope{
		Since:  d.Since.Format(time.RFC3339),
		Until:  d.Until.Format(time.RFC3339),
		Groups: make([]GroupWire, 0, len(d.Groups)),
	}
	for _, g := range d.Groups {
		fc := g.FailedCommands
		if fc == nil {
			fc = []string{}
		}
		env.Groups = append(env.Groups, GroupWire{
			Host:           g.Host,
			CWD:            g.CWD,
			RepoRoot:       g.RepoRoot,
			Count:          g.Count,
			Failures:       g.Failures,
			FailedCommands: fc,
		})
	}
	return env
}

// StartOfDay returns local midnight of t (in t's own location) — the default
// window start for a "today" digest.
func StartOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
