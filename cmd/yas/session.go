package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mjacobs/yas/internal/queryapi"
	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
)

// shortSession derives a compact, fixed-width display token from a full session
// id. Deterministic and stateless: fnv1a64 -> mod 36^7 -> base36 -> pad to 7.
// Empty session (imported/sessionless rows) maps to "".
func shortSession(id string) string {
	if id == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	n := h.Sum64() % 78364164096 // 36^7
	s := strconv.FormatUint(n, 36)
	if len(s) < 7 {
		s = strings.Repeat("0", 7-len(s)) + s
	}
	return s
}

// sessionStore is the slice of the local store that yas session needs.
type sessionStore interface {
	Search(ctx context.Context, q store.Query) ([]record.Record, error)
	Sessions(ctx context.Context) ([]string, error)
}

// resolveSession maps a CLI arg to a full session id: exact full-id first,
// else match the short token against the distinct live sessions.
func resolveSession(ctx context.Context, st sessionStore, arg string) (string, error) {
	if recs, err := st.Search(ctx, store.Query{Session: arg, Limit: 1}); err != nil {
		return "", err
	} else if len(recs) > 0 {
		return arg, nil // arg was a full id
	}
	sessions, err := st.Sessions(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, s := range sessions {
		if shortSession(s) == arg {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no session matching token %q", arg)
	default:
		cands := make([]string, len(matches))
		for i, m := range matches {
			cands[i] = fmt.Sprintf("%s (%s)", shortSession(m), m)
		}
		return "", fmt.Errorf("token %q is ambiguous across %d sessions: %s; re-run with a full id", arg, len(matches), strings.Join(cands, ", "))
	}
}

// sessionFetchLimit is an effectively-unbounded Search limit for `yas session`:
// a single shell's whole history must be shown, so we override the store's
// default page cap (which, being oldest-first, would otherwise keep only the
// OLDEST records and hide the most recent commands). No real session approaches
// this many commands.
const sessionFetchLimit = 1 << 30

// doSession resolves arg to a full session id, then prints that session's
// commands oldest-first via the history renderer (session column suppressed).
func doSession(ctx context.Context, st sessionStore, arg string, opts historyOpts, w io.Writer, loc *time.Location) error {
	full, err := resolveSession(ctx, st, arg)
	if err != nil {
		return err
	}
	recs, err := st.Search(ctx, store.Query{Session: full, Reverse: true, ExcludeID: opts.excludeID, Limit: sessionFetchLimit})
	if err != nil {
		return err
	}
	if opts.asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(queryapi.SearchResponse{Records: recs})
	}
	if _, err := fmt.Fprintf(w, "session %s (%s) · %d %s\n", shortSession(full), full, len(recs), plural(len(recs), "command", "commands")); err != nil {
		return err
	}
	opts.showSession = false // single session: redundant column
	styles := newCLIStyles(opts.color)
	if styles.color {
		return renderHistoryRich(w, recs, 1, loc, opts, styles)
	}
	return renderHistory(w, recs, 1, loc, opts, styles)
}

// parseSessionArgs parses `yas session` arguments: flags mirroring
// parseHistoryArgs (--json, --no-color, --time-format, --no-time, --no-exit,
// --no-duration) plus exactly one positional token or full session id.
func parseSessionArgs(args []string) (arg string, opts historyOpts, err error) {
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	layout := fs.String("time-format", defaultHistTimeLayout, "Go time layout for the timestamp column")
	noTime := fs.Bool("no-time", false, "omit the timestamp column")
	noExit := fs.Bool("no-exit", false, "omit the exit-code (result) column")
	noDuration := fs.Bool("no-duration", false, "omit the duration (TOOK) column")
	noColor := fs.Bool("no-color", false, "disable colorized output")
	asJSON := fs.Bool("json", false, "emit JSON (same envelope as the query API)")

	flags, operands := partitionArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return "", historyOpts{}, err
	}

	if len(operands) == 0 {
		return "", historyOpts{}, fmt.Errorf("usage: yas session <token|session-id>")
	}
	if len(operands) > 1 {
		// Unprefixed: cmdSession adds the "yas session:" prefix when printing.
		return "", historyOpts{}, fmt.Errorf("too many arguments")
	}
	if strings.TrimSpace(operands[0]) == "" {
		return "", historyOpts{}, fmt.Errorf("usage: yas session <token|session-id>")
	}

	opts = historyOpts{
		layout:       *layout,
		showTime:     !*noTime,
		showExit:     !*noExit,
		showDuration: !*noDuration,
		showSession:  false, // single-session view never needs the token column
		color:        !*noColor,
		asJSON:       *asJSON,
	}
	return operands[0], opts, nil
}

// cmdSession implements `yas session <token|session-id>`.
func cmdSession(args []string) {
	arg, opts, err := parseSessionArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas session:", err)
		os.Exit(2)
	}
	st, _, closeStore := openStore()
	defer closeStore()
	opts.color = opts.color && colorTerminal(os.Stdout)
	opts.excludeID = os.Getenv("YAS_RECORD_ID")
	if err := doSession(context.Background(), st, arg, opts, os.Stdout, time.Local); err != nil {
		fmt.Fprintln(os.Stderr, "yas session:", err)
		os.Exit(1)
	}
}
