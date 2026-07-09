package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/mjacobs/yas/internal/digest"
	"github.com/mjacobs/yas/internal/store"
)

// yas digest: a deterministic, cross-stream synthesis of "commands run in a
// window, grouped by host/project, with failures flagged." The computation and
// the JSON contract live in internal/digest (shared with GET /v1/digest); this
// file is the CLI shell: flag parsing, store access, and the human renderer.

// digestTimeLayout is the local-time window format for the human digest.
const digestTimeLayout = "2006-01-02 15:04"

// renderDigest writes d to w: the JSON contract (asJSON) or a human summary.
// styles color the human summary; it is ignored for JSON.
func renderDigest(w io.Writer, d digest.Digest, asJSON bool, styles cliStyles) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(digest.ToEnvelope(d))
	}
	return renderDigestHuman(w, d, styles)
}

// renderDigestHuman writes a readable grouped summary: a window header, then one
// block per (host, cwd) with its count and (when any) failure count and the
// sampled failing commands. With color on it accents the title, dims the
// path/counts, reds the failures, and adds a rule under the title; with color
// off the output is byte-for-byte the plain text (every style is a passthrough
// and the rule is omitted). An empty window prints a friendly line.
func renderDigestHuman(w io.Writer, d digest.Digest, styles cliStyles) error {
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

	since = digest.StartOfDay(now)
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
		Limit:     digest.ScanCap,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas digest:", err)
		os.Exit(1)
	}
	if err := renderDigest(os.Stdout, digest.Build(recs, since, until), asJSON, styles); err != nil {
		fmt.Fprintln(os.Stderr, "yas digest:", err)
		os.Exit(1)
	}
}
