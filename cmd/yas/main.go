// Command yas is the per-machine agent: it records shell history into a local
// SQLite replica, serves a localhost HTTP+JSON query API for UIs, and syncs with
// the central yas-server.
//
// As of M2 the two-phase recording path persists into the local SQLite replica
// (used by shell/yas.zsh); search, serve, and sync land in later milestones.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mjacobs/yas/internal/config"
	"github.com/mjacobs/yas/internal/histimport"
	"github.com/mjacobs/yas/internal/queryapi"
	"github.com/mjacobs/yas/internal/record"
	"github.com/mjacobs/yas/internal/store"
	sqlitestore "github.com/mjacobs/yas/internal/store/sqlite"
	"github.com/mjacobs/yas/internal/syncclient"
	"github.com/mjacobs/yas/internal/syncproto"
	"github.com/muesli/termenv"
)

var version = "0.0.0-dev"

func main() {
	cmd, rest := route(os.Args[1:])
	switch cmd {
	case "record":
		cmdRecord(rest)
	case "search":
		cmdSearch(rest)
	case "history":
		cmdHistory(rest)
	case "serve":
		cmdServe(rest)
	case "sync":
		cmdSync(rest)
	case "import":
		cmdImport(rest)
	case "session":
		cmdSession(rest)
	case "digest":
		cmdDigest(rest)
	case "mcp":
		cmdMCP(rest)
	case "completion":
		cmdCompletion(rest)
	case "version":
		fmt.Println("yas", version)
	case "help":
		usage()
	default: // "unknown"
		fmt.Fprintf(os.Stderr, "yas: unknown command %q\n", rest[0])
		usage()
		os.Exit(2)
	}
}

// route resolves the args after the program name into a normalized
// (command, rest) pair. history is the default command: a bare `yas`, a
// leading count (`yas 20`), and a leading flag (`yas --json`) all route to
// history, so they are shortcuts for the matching `yas history ...`. Known
// subcommands and the version/help meta-flags dispatch as named; a bare
// unknown word resolves to "unknown" (rest[0] is the offending token).
func route(args []string) (cmd string, rest []string) {
	if len(args) == 0 {
		return "history", nil
	}
	first := args[0]
	switch first {
	case "record", "search", "history", "serve", "sync", "import", "session", "digest", "mcp", "completion":
		return first, args[1:]
	case "version", "--version", "-v":
		return "version", nil
	case "help", "-h", "--help":
		return "help", nil
	}
	// Default command: a count or any flag is a `yas history` shortcut, so pass
	// the whole arg list through to history's own parser, which validates it.
	if isCount(first) || isFlagLike(first) {
		return "history", args
	}
	return "unknown", args
}

// isCount reports whether s is a non-negative integer — a history count
// operand. It mirrors what parseHistoryArgs accepts, so route never sends
// history something its parser would then reject as "invalid count".
func isCount(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0
}

// isFlagLike reports whether s looks like a flag ("-x" / "--long"). A lone "-"
// is not flag-like.
func isFlagLike(s string) bool {
	return len(s) > 1 && s[0] == '-'
}

// recordStore is the slice of the local store the recording path needs.
type recordStore interface {
	Put(ctx context.Context, recs ...record.Record) error
	Search(ctx context.Context, q store.Query) ([]record.Record, error)
}

// doRecordStart assigns the record its identity (UUIDv7) and timestamps, then
// persists it unfinished, returning the new id for the hook to stash.
func doRecordStart(ctx context.Context, st recordStore, now time.Time, rec record.Record) (string, error) {
	rec.ID = record.NewID()
	rec.StartTime = now
	rec.CreatedAt = now
	if err := st.Put(ctx, rec); err != nil {
		return "", err
	}
	return rec.ID, nil
}

// doRecordFinish loads the started record by id, stamps the now-known exit code
// and duration, and re-persists it. Immutable fields are preserved by the
// store's upsert. An unknown id is an error.
func doRecordFinish(ctx context.Context, st recordStore, id string, exit int, durationMS *int64) error {
	recs, err := st.Search(ctx, store.Query{ID: id, Limit: 1})
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("unknown record id %q", id)
	}
	r := recs[0]
	r.ExitCode = &exit
	r.DurationMS = durationMS
	return st.Put(ctx, r)
}

// cmdRecord handles `yas record start` and `yas record finish`, the contract
// the zsh hook depends on.
func cmdRecord(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: yas record <start|finish> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "start":
		fs := flag.NewFlagSet("record start", flag.ExitOnError)
		command := fs.String("command", "", "the command line as entered")
		cwd := fs.String("cwd", "", "working directory")
		session := fs.String("session", "", "shell session id")
		shell := fs.String("shell", "", "shell name (zsh|bash|fish)")
		author := fs.String("author", "", `who/what ran it; empty = human ("claude-code", "codex", "ci", ...)`)
		_ = fs.Parse(args[1:])

		st, cfg, closeStore := openStore()
		defer closeStore()

		id, err := doRecordStart(context.Background(), st, time.Now(), record.Record{
			Command:  *command,
			CWD:      *cwd,
			Session:  *session,
			Shell:    *shell,
			Executor: *author,
			Hostname: cfg.Hostname,
			Username: currentUsername(),
		})
		if err != nil {
			// Never print a bogus id to stdout — the hook reads stdout as the id.
			fmt.Fprintln(os.Stderr, "yas record start:", err)
			os.Exit(1)
		}
		// The hook captures stdout as the record id, so print ONLY the id.
		fmt.Println(id)

	case "finish":
		fs := flag.NewFlagSet("record finish", flag.ExitOnError)
		id := fs.String("id", "", "record id from `record start`")
		exit := fs.Int("exit", 0, "command exit code")
		durMS := fs.Int64("duration-ms", -1, "command duration in milliseconds")
		_ = fs.Parse(args[1:])
		if *id == "" {
			fmt.Fprintln(os.Stderr, "yas record finish: --id is required")
			os.Exit(2)
		}

		var durationMS *int64
		if *durMS >= 0 {
			durationMS = durMS
		}

		st, _, closeStore := openStore()
		defer closeStore()

		if err := doRecordFinish(context.Background(), st, *id, *exit, durationMS); err != nil {
			fmt.Fprintln(os.Stderr, "yas record finish:", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "yas record: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

// doSearch runs q against the store and renders the results to w: as the same
// JSON envelope the HTTP API serves (asJSON), else a compact human line each.
// JSON is never styled — it's the UI contract.
func doSearch(ctx context.Context, s recordStore, q store.Query, w io.Writer, asJSON bool, styles cliStyles, showSession bool) error {
	recs, err := s.Search(ctx, q)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(queryapi.SearchResponse{Records: recs})
	}
	if styles.color {
		return renderSearchRich(w, recs, styles, showSession)
	}
	for _, r := range recs {
		ts := styles.dim.Render(r.StartTime.UTC().Format("2006-01-02 15:04:05Z"))
		sess := ""
		if showSession {
			sess = sessCell(r.Session) + "  "
		}
		if _, err := fmt.Fprintf(w, "%s  %s  %s%s\n", ts, styles.exit(r), sess, r.Command); err != nil {
			return err
		}
	}
	return nil
}

// exitField renders a record's result for the human listings: the exit code, or
// "-" while the command is still unfinished (no exit captured yet).
func exitField(r record.Record) string {
	if r.ExitCode == nil {
		return "-"
	}
	return strconv.Itoa(*r.ExitCode)
}

// sessCellWidth is the fixed visible width of the SESS token column.
const sessCellWidth = 7

// sessCell renders the fixed-width SESS column for a record: its short session
// token, or sessCellWidth spaces for a sessionless (imported) row so the command
// column stays aligned. shortSession always yields exactly sessCellWidth chars
// for a non-empty session, so no truncation or extra padding is needed.
func sessCell(session string) string {
	if s := shortSession(session); s != "" {
		return s
	}
	return strings.Repeat(" ", sessCellWidth)
}

// colorTerminal reports whether f is a color-capable TTY with NO_COLOR unset.
// Combined with the --no-color flag, it gates colorized output so pipes,
// redirects, and NO_COLOR users get plain text automatically.
func colorTerminal(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return termenv.NewOutput(f).Profile != termenv.Ascii
}

// cliStyles holds the lipgloss styles for the human listings. When color is off
// every style is a no-op passthrough, so callers can render unconditionally and
// the output is byte-identical to the old plain text.
type cliStyles struct {
	dim    lipgloss.Style // timestamps + header labels
	ok     lipgloss.Style // exit 0
	fail   lipgloss.Style // non-zero exit
	wait   lipgloss.Style // still running ("-")
	accent lipgloss.Style // title (bold pink)
	num    lipgloss.Style // entry number (cyan)
	color  bool           // true on a TTY -> use the rich (styled) layout
}

// newCLIStyles builds the palette. The color profile is set explicitly (rather
// than auto-detected from a writer) so callers decide on/off and tests are
// deterministic regardless of whether the test writer is a TTY.
func newCLIStyles(color bool) cliStyles {
	r := lipgloss.NewRenderer(io.Discard)
	if color {
		r.SetColorProfile(termenv.ANSI256)
	} else {
		r.SetColorProfile(termenv.Ascii)
	}
	return cliStyles{
		dim:    r.NewStyle().Faint(true),
		ok:     r.NewStyle().Foreground(lipgloss.Color("2")),            // green
		fail:   r.NewStyle().Foreground(lipgloss.Color("1")).Bold(true), // red
		wait:   r.NewStyle().Foreground(lipgloss.Color("3")),            // yellow
		accent: r.NewStyle().Foreground(lipgloss.Color("212")).Bold(true),
		num:    r.NewStyle().Foreground(lipgloss.Color("6")), // cyan
		color:  color,
	}
}

// exit renders the bracketed result token, colored by outcome: green for 0, bold
// red for a non-zero exit, yellow while the command is still running. Used by the
// plain (piped / --no-color) listings.
func (s cliStyles) exit(r record.Record) string {
	field := "[" + exitField(r) + "]"
	switch {
	case r.ExitCode == nil:
		return s.wait.Render(field)
	case *r.ExitCode == 0:
		return s.ok.Render(field)
	default:
		return s.fail.Render(field)
	}
}

// glyph renders the outcome as a status symbol for the rich (TTY) listings:
// green ✓ for success, bold red ✗ + the code for a failure, yellow ○ while the
// command is still running.
func (s cliStyles) glyph(r record.Record) string {
	switch {
	case r.ExitCode == nil:
		return s.wait.Render("○")
	case *r.ExitCode == 0:
		return s.ok.Render("✓")
	default:
		return s.fail.Render("✗ " + strconv.Itoa(*r.ExitCode))
	}
}

// padTo right-pads a (possibly ANSI-styled) cell to width visible columns.
func padTo(cell string, width int) string {
	if pad := width - lipgloss.Width(cell); pad > 0 {
		return cell + strings.Repeat(" ", pad)
	}
	return cell
}

// richHeader renders a title line, a rule, and a dim column-header row for the
// styled listings. cols are the already-width-padded header labels.
func richHeader(title string, styles cliStyles, cols ...string) string {
	header := strings.Join(cols, "  ")
	width := lipgloss.Width(header)
	if w := lipgloss.Width(title); w > width {
		width = w
	}
	rule := styles.dim.Render(strings.Repeat("─", width))
	return styles.accent.Render(title) + "\n" + rule + "\n" + styles.dim.Render(header) + "\n"
}

// renderHistoryRich is the TTY listing: a title, column headers, then numbered
// rows with a cyan index, dim timestamp, status glyph, and the command.
func renderHistoryRich(w io.Writer, recs []record.Record, startNum int, loc *time.Location, opts historyOpts, styles cliStyles) error {
	if len(recs) == 0 {
		return nil // empty history prints nothing, same as the plain path
	}
	const numW = 5
	timeW, glyphW := 0, 0
	for _, r := range recs {
		if opts.showTime {
			if n := lipgloss.Width(r.StartTime.In(loc).Format(opts.layout)); n > timeW {
				timeW = n
			}
		}
		if opts.showExit {
			if n := lipgloss.Width(styles.glyph(r)); n > glyphW {
				glyphW = n
			}
		}
	}

	cols := []string{fmt.Sprintf("%*s", numW, "#")} // right-aligned to match the numbers
	if opts.showTime {
		cols = append(cols, padTo("WHEN", timeW))
	}
	if opts.showExit {
		cols = append(cols, padTo("", glyphW))
	}
	if opts.showSession {
		cols = append(cols, padTo("SESS", sessCellWidth))
	}
	cols = append(cols, "COMMAND")
	if _, err := io.WriteString(w, richHeader("yas · history", styles, cols...)); err != nil {
		return err
	}

	for i, r := range recs {
		var b strings.Builder
		b.WriteString(styles.num.Render(fmt.Sprintf("%*d", numW, startNum+i)))
		if opts.showTime {
			b.WriteString("  ")
			b.WriteString(styles.dim.Render(padTo(r.StartTime.In(loc).Format(opts.layout), timeW)))
		}
		if opts.showExit {
			b.WriteString("  ")
			b.WriteString(padTo(styles.glyph(r), glyphW))
		}
		if opts.showSession {
			b.WriteString("  ")
			b.WriteString(sessCell(r.Session))
		}
		b.WriteString("  ")
		b.WriteString(r.Command)
		b.WriteByte('\n')
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
	}
	return nil
}

// renderSearchRich is the TTY search listing: a title, column headers, then rows
// of dim UTC timestamp, status glyph, optional SESS token, and command (newest
// first, as searched).
func renderSearchRich(w io.Writer, recs []record.Record, styles cliStyles, showSession bool) error {
	if len(recs) == 0 {
		return nil // no matches -> no chrome, same as the plain path
	}
	timeW, glyphW := 0, 0
	for _, r := range recs {
		if n := lipgloss.Width(r.StartTime.UTC().Format("2006-01-02 15:04:05Z")); n > timeW {
			timeW = n
		}
		if n := lipgloss.Width(styles.glyph(r)); n > glyphW {
			glyphW = n
		}
	}
	headerCols := []string{padTo("WHEN", timeW), padTo("", glyphW)}
	if showSession {
		headerCols = append(headerCols, padTo("SESS", sessCellWidth))
	}
	headerCols = append(headerCols, "COMMAND")
	if _, err := io.WriteString(w, richHeader("yas · search", styles, headerCols...)); err != nil {
		return err
	}
	for _, r := range recs {
		ts := styles.dim.Render(padTo(r.StartTime.UTC().Format("2006-01-02 15:04:05Z"), timeW))
		sess := ""
		if showSession {
			sess = sessCell(r.Session) + "  "
		}
		if _, err := fmt.Fprintf(w, "%s  %s  %s%s\n", ts, padTo(styles.glyph(r), glyphW), sess, r.Command); err != nil {
			return err
		}
	}
	return nil
}

// cmdSearch implements `yas search [text...] [filters]`, a built-in client of
// the local store. Free args form the full-text query.
func cmdSearch(args []string) {
	q, asJSON, noColor, showSession, err := parseSearchArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas search:", err)
		os.Exit(2)
	}
	st, _, closeStore := openStore()
	defer closeStore()
	// Don't list the in-flight `yas search` command itself (the zsh hook exports
	// its record id before this process runs).
	q.ExcludeID = os.Getenv("YAS_RECORD_ID")
	styles := newCLIStyles(!noColor && colorTerminal(os.Stdout))
	if err := doSearch(context.Background(), st, q, os.Stdout, asJSON, styles, showSession); err != nil {
		fmt.Fprintln(os.Stderr, "yas search:", err)
		os.Exit(1)
	}
}

// parseSearchArgs turns `search` args into a store.Query, the --json flag, the
// --no-color flag, and the showSession flag (true unless --no-session is passed).
// Flags may appear before OR after the free-text query (the stdlib flag package
// stops at the first operand, so we partition ourselves).
func parseSearchArgs(args []string) (store.Query, bool, bool, bool, error) {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	host := fs.String("host", "", "filter by hostname")
	cwd := fs.String("cwd", "", "filter by working directory")
	session := fs.String("session", "", "filter by session id")
	exit := fs.Int("exit", 0, "filter by exit code")
	failed := fs.Bool("failed", false, "only commands that exited non-zero")
	limit := fs.Int("limit", 0, "max results (0 = default)")
	offset := fs.Int("offset", 0, "results to skip (pagination)")
	reverse := fs.Bool("reverse", false, "oldest first (default newest first)")
	since := fs.String("since", "", "only commands at/after this RFC3339 time")
	until := fs.String("until", "", "only commands before this RFC3339 time")
	executor := fs.String("executor", "", "filter by who/what ran it: a name, or $all-agent / $all-human")
	asJSON := fs.Bool("json", false, "emit JSON (same shape as the HTTP API)")
	noColor := fs.Bool("no-color", false, "disable colorized output")
	noSession := fs.Bool("no-session", false, "hide the session token column")

	flags, operands := partitionArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return store.Query{}, false, false, false, err
	}

	q := store.Query{
		Text:       strings.Join(operands, " "),
		Host:       *host,
		CWD:        *cwd,
		Session:    *session,
		FailedOnly: *failed,
		Limit:      *limit,
		Offset:     *offset,
		Reverse:    *reverse,
	}
	// --exit only filters when actually passed (0 is a meaningful exit code).
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "exit" {
			q.ExitCode = exit
		}
	})
	q.ApplyExecutorToken(*executor)
	var err error
	if q.Since, err = parseOptRFC3339(*since); err != nil {
		return store.Query{}, false, false, false, fmt.Errorf("invalid --since %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", *since)
	}
	if q.Until, err = parseOptRFC3339(*until); err != nil {
		return store.Query{}, false, false, false, fmt.Errorf("invalid --until %q: want RFC3339 (e.g. 2006-01-02T15:04:05Z)", *until)
	}
	return q, *asJSON, *noColor, !*noSession, nil
}

// partitionArgs splits args into flag tokens (with their values) and operands,
// so flags and free text can be interspersed. A value-taking flag in spaced form
// (--host h) consumes the following token; bool flags and --k=v forms do not.
func partitionArgs(fs *flag.FlagSet, args []string) (flags, operands []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {
				continue // value embedded in the token
			}
			if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		operands = append(operands, a)
	}
	return flags, operands
}

func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

// parseOptRFC3339 parses an optional RFC3339 timestamp; "" yields the zero time.
func parseOptRFC3339(val string) (time.Time, error) {
	if val == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, val)
}

// --- yas history: the shell `history` builtin, over the local replica ---

// historyMode is the operation `yas history` performs.
type historyMode int

const (
	histList   historyMode = iota // list entries (default)
	histDelete                    // -d: tombstone an offset or range
	histClear                     // -c: tombstone every entry
)

const (
	// defaultHistoryLimit bounds a no-argument `yas history` so it can't dump a
	// huge synced store to the terminal (bash lists all; zsh lists the last few).
	defaultHistoryLimit = 100
	// defaultHistTimeLayout is the Go layout for the timestamp column, shown in
	// local time. Override with --time-format; suppress with --no-time.
	defaultHistTimeLayout = "2006-01-02 15:04:05"
	// clearBatch caps how many records each -c round tombstones at once.
	clearBatch = 500
)

// historyOpts is the parsed form of `yas history` arguments.
type historyOpts struct {
	mode        historyMode
	n           int    // list: number of most-recent entries (0 = default)
	delSpec     string // delete: offset or start-end
	layout      string // list: timestamp layout
	showTime    bool   // list: include the timestamp column
	showExit    bool   // list: include the exit-code (result) column
	showSession bool   // list: include the SESS token column (default true from parse)
	color       bool   // list: colorize the output (gated by --no-color + TTY)
	asJSON      bool   // list: emit the query-API JSON envelope
	yes         bool   // clear: confirmation guard
	excludeID   string // omit this record (the in-flight `yas history` command itself)
}

// historyStore is the slice of the local store `yas history` needs: read the
// timeline (Search), number it absolutely (Count), and tombstone (Put).
type historyStore interface {
	Search(ctx context.Context, q store.Query) ([]record.Record, error)
	Count(ctx context.Context, q store.Query) (int, error)
	Put(ctx context.Context, recs ...record.Record) error
}

// parseHistoryArgs turns `history` args into historyOpts. It mirrors the bash
// builtin's surface: a bare count lists the last n, -d deletes, -c clears. Flags
// may interleave with the count operand (we partition them like search does).
func parseHistoryArgs(args []string) (historyOpts, error) {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clear := fs.Bool("c", false, "clear all history (tombstone every entry; requires --yes)")
	yes := fs.Bool("yes", false, "confirm a destructive operation (-c)")
	del := fs.String("d", "", "delete the entry at an offset, or a start-end range")
	layout := fs.String("time-format", defaultHistTimeLayout, "Go time layout for the timestamp column")
	noTime := fs.Bool("no-time", false, "omit the timestamp column")
	noExit := fs.Bool("no-exit", false, "omit the exit-code (result) column")
	noSession := fs.Bool("no-session", false, "hide the session token column")
	noColor := fs.Bool("no-color", false, "disable colorized output")
	asJSON := fs.Bool("json", false, "emit JSON (same envelope as the query API)")

	flags, operands := partitionArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return historyOpts{}, err
	}

	opts := historyOpts{
		layout:      *layout,
		showTime:    !*noTime,
		showExit:    !*noExit,
		showSession: !*noSession,
		color:       !*noColor, // refined against the TTY/NO_COLOR in cmdHistory
		asJSON:      *asJSON,
		yes:         *yes,
		delSpec:     *del,
	}

	delSet := *del != ""
	switch {
	case *clear && delSet:
		return historyOpts{}, fmt.Errorf("-c and -d are mutually exclusive")
	case (*clear || delSet) && len(operands) > 0:
		return historyOpts{}, fmt.Errorf("a count cannot be combined with -c or -d")
	case len(operands) > 1:
		return historyOpts{}, fmt.Errorf("history takes at most one count argument")
	case *clear:
		if !*yes {
			return historyOpts{}, fmt.Errorf("refusing to clear all history without --yes")
		}
		opts.mode = histClear
	case delSet:
		opts.mode = histDelete
	default:
		opts.mode = histList
		if len(operands) == 1 {
			n, err := strconv.Atoi(operands[0])
			if err != nil || n < 0 {
				return historyOpts{}, fmt.Errorf("invalid count %q: want a non-negative integer", operands[0])
			}
			opts.n = n
		}
	}
	return opts, nil
}

// deletePlan is a single ordered Search that addresses the records a -d argument
// targets, without a separate Count: positive offsets count from the oldest
// (ascending), negative offsets from the newest (descending). Resolving the
// target in one query keeps the delete free of the Count→Search skew a
// concurrent `yas record` write could otherwise open on the WAL file.
type deletePlan struct {
	ascending bool // true: rank from the oldest (ASC); false: from the newest (DESC)
	offset    int
	limit     int
}

// planDelete turns a -d argument ("offset" or "start-end", each endpoint a
// positive or negative integer) into a deletePlan. Positive offsets are 1-based
// from the oldest entry (matching the absolute numbers `history` prints);
// negative offsets count back from the newest (-1 = newest). An out-of-range
// offset isn't an error here — it simply matches no rows, which the caller
// reports — so planDelete never needs to know the history size.
func planDelete(spec string) (deletePlan, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return deletePlan{}, fmt.Errorf("missing offset")
	}
	// A separator is a '-' immediately preceded by a digit; this disambiguates a
	// range ("2-4", "-3--1") from a single negative offset ("-2").
	sep := -1
	for i := 1; i < len(spec); i++ {
		if spec[i] == '-' && spec[i-1] >= '0' && spec[i-1] <= '9' {
			sep = i
			break
		}
	}
	if sep == -1 {
		v, err := strconv.Atoi(spec)
		if err != nil {
			return deletePlan{}, fmt.Errorf("invalid offset %q: want an integer or a start-end range", spec)
		}
		return offsetPlan(v)
	}
	a, errA := strconv.Atoi(strings.TrimSpace(spec[:sep]))
	b, errB := strconv.Atoi(strings.TrimSpace(spec[sep+1:]))
	if errA != nil || errB != nil {
		return deletePlan{}, fmt.Errorf("invalid range %q: want start-end integers", spec)
	}
	return rangePlan(a, b, spec)
}

// offsetPlan addresses a single entry by offset.
func offsetPlan(v int) (deletePlan, error) {
	switch {
	case v > 0:
		return deletePlan{ascending: true, offset: v - 1, limit: 1}, nil
	case v < 0:
		return deletePlan{ascending: false, offset: -v - 1, limit: 1}, nil
	default:
		return deletePlan{}, fmt.Errorf("offset 0 is invalid: entries are numbered from 1")
	}
}

// rangePlan addresses an inclusive range. Both endpoints must share a sign:
// positive ranks count from the oldest, negative from the newest. A range mixing
// signs (or touching 0) has no single contiguous ordering and is rejected.
func rangePlan(a, b int, spec string) (deletePlan, error) {
	switch {
	case a > 0 && b > 0:
		if a > b {
			return deletePlan{}, fmt.Errorf("invalid range %q: start is after end", spec)
		}
		return deletePlan{ascending: true, offset: a - 1, limit: b - a + 1}, nil
	case a < 0 && b < 0:
		// -j..-k spans the newest |b| up through the newest |a|; the start must be
		// the older endpoint, i.e. a <= b (more negative is older).
		if a > b {
			return deletePlan{}, fmt.Errorf("invalid range %q: start is after end", spec)
		}
		return deletePlan{ascending: false, offset: -b - 1, limit: -a - -b + 1}, nil
	default:
		return deletePlan{}, fmt.Errorf("invalid range %q: endpoints must both be positive or both negative", spec)
	}
}

// renderHistory writes records (oldest-first) as numbered history lines starting
// at startNum: the absolute number, then optional local-time timestamp and exit
// (result) columns, then the command. The result token is padded to the widest
// in the batch so the command column lines up even when exit-code widths differ
// ([0] vs [128] vs [-]); the timestamp layout is already fixed-width. Padding is
// computed from the plain (un-styled) width so it's correct even when the result
// token carries ANSI color codes.
func renderHistory(w io.Writer, recs []record.Record, startNum int, loc *time.Location, opts historyOpts, styles cliStyles) error {
	exitWidth := 0
	if opts.showExit {
		for _, r := range recs {
			if n := len(exitField(r)) + len("[]"); n > exitWidth {
				exitWidth = n
			}
		}
	}
	for i, r := range recs {
		var b strings.Builder
		b.WriteString(styles.dim.Render(fmt.Sprintf("%5d", startNum+i)))
		if opts.showTime {
			b.WriteString("  ")
			b.WriteString(styles.dim.Render(r.StartTime.In(loc).Format(opts.layout)))
		}
		if opts.showExit {
			pad := exitWidth - (len(exitField(r)) + len("[]"))
			b.WriteString("  ")
			b.WriteString(styles.exit(r))
			b.WriteString(strings.Repeat(" ", pad))
		}
		if opts.showSession {
			b.WriteString("  ")
			b.WriteString(sessCell(r.Session))
		}
		b.WriteString("  ")
		b.WriteString(r.Command)
		b.WriteByte('\n')
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
	}
	return nil
}

// doHistory executes the parsed history operation against the store, writing any
// listing to w. It returns the number of entries deleted (0 for a listing).
func doHistory(ctx context.Context, st historyStore, opts historyOpts, w io.Writer, loc *time.Location) (int, error) {
	switch opts.mode {
	case histDelete:
		plan, err := planDelete(opts.delSpec)
		if err != nil {
			return 0, err
		}
		// One ordered query resolves the target rows, so there's no Count→Search
		// window for a concurrent record write to shift the offset under us.
		// ExcludeID keeps the in-flight `yas history -d` command out of the ranks.
		recs, err := st.Search(ctx, store.Query{ExcludeID: opts.excludeID, Reverse: plan.ascending, Offset: plan.offset, Limit: plan.limit})
		if err != nil {
			return 0, err
		}
		if len(recs) == 0 {
			return 0, fmt.Errorf("no history entry at offset %q", opts.delSpec)
		}
		return tombstone(ctx, st, recs)

	case histClear:
		if !opts.yes {
			return 0, fmt.Errorf("refusing to clear all history without --yes")
		}
		total := 0
		for {
			// As records are tombstoned they leave the result set, so repeatedly
			// draining the head clears the whole store without tracking offsets.
			// ExcludeID spares the in-flight `yas history -c` command itself.
			recs, err := st.Search(ctx, store.Query{ExcludeID: opts.excludeID, Limit: clearBatch})
			if err != nil {
				return total, err
			}
			if len(recs) == 0 {
				return total, nil
			}
			n, err := tombstone(ctx, st, recs)
			total += n
			if err != nil {
				return total, err
			}
		}

	default: // histList
		count, err := st.Count(ctx, store.Query{ExcludeID: opts.excludeID})
		if err != nil {
			return 0, err
		}
		n := opts.n
		if n <= 0 {
			n = defaultHistoryLimit
		}
		recs, err := st.Search(ctx, store.Query{ExcludeID: opts.excludeID, Limit: n}) // newest-first
		if err != nil {
			return 0, err
		}
		reverseRecords(recs) // oldest-first, newest at the bottom (bash order)
		startNum := count - len(recs) + 1
		if startNum < 1 {
			startNum = 1
		}
		if opts.asJSON {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return 0, enc.Encode(queryapi.SearchResponse{Records: recs})
		}
		styles := newCLIStyles(opts.color)
		if styles.color {
			return 0, renderHistoryRich(w, recs, startNum, loc, opts, styles)
		}
		return 0, renderHistory(w, recs, startNum, loc, opts, styles)
	}
}

// tombstone marks each record deleted and re-persists it. Put's upsert flips the
// deleted flag and re-marks the row unsynced, so the deletion propagates on the
// next sync to the server and every other machine. It needs only Put, so it
// takes the narrow recordStore — shared by `yas history` (delete/clear) and
// `yas import --prune-live-dupes`.
func tombstone(ctx context.Context, st recordStore, recs []record.Record) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}
	for i := range recs {
		recs[i].Deleted = true
	}
	if err := st.Put(ctx, recs...); err != nil {
		return 0, err
	}
	return len(recs), nil
}

func reverseRecords(recs []record.Record) {
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// cmdHistory implements `yas history`, the shell `history` builtin over the
// local replica: list with absolute numbers, -d to delete, -c to clear.
func cmdHistory(args []string) {
	opts, err := parseHistoryArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas history:", err)
		os.Exit(2)
	}
	st, _, closeStore := openStore()
	defer closeStore()

	// --no-color already cleared opts.color; also disable when stdout isn't a
	// color TTY (piped/redirected) or NO_COLOR is set.
	opts.color = opts.color && colorTerminal(os.Stdout)
	// Don't show/target the in-flight `yas history` command itself (the zsh hook
	// exports its record id before this process runs).
	opts.excludeID = os.Getenv("YAS_RECORD_ID")

	n, err := doHistory(context.Background(), st, opts, os.Stdout, time.Local)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas history:", err)
		os.Exit(1)
	}
	switch opts.mode {
	case histDelete:
		fmt.Fprintf(os.Stderr, "yas history: deleted %d entr%s\n", n, plural(n, "y", "ies"))
	case histClear:
		fmt.Fprintf(os.Stderr, "yas history: cleared %d entr%s\n", n, plural(n, "y", "ies"))
	}
}

// cmdServe runs the localhost HTTP+JSON query API over the local replica — the
// surface UIs target. It blocks until the process is killed; SIGINT/SIGTERM
// drain in-flight requests and exit 0, so systemd's stop isn't a failure.
func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address for the localhost query API")
	_ = fs.Parse(args)

	st, _, closeStore := openStore()
	defer closeStore()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas serve:", err)
		os.Exit(1)
	}
	// Timeouts bound a stuck client so it can't leak a goroutine; responses are
	// tiny and fast, so these never truncate a legitimate query.
	srv := &http.Server{
		Handler:           queryapi.NewHandler(st),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "yas serve: query API at http://%s/v1/search\n", ln.Addr())
	if err := runServe(ctx, srv, ln); err != nil {
		fmt.Fprintln(os.Stderr, "yas serve:", err)
		os.Exit(1)
	}
}

// runServe serves on ln until ctx is canceled (a shutdown signal), then drains
// in-flight requests and returns nil for a clean stop. A server that fails
// before the signal returns its error.
func runServe(ctx context.Context, srv *http.Server, ln net.Listener) error {
	// The API's WriteTimeout is 15s but a stopping service shouldn't hang that
	// long on a stuck client.
	return runServeGrace(ctx, srv, ln, 5*time.Second)
}

func runServeGrace(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err // died on its own — never a clean shutdown
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		// A request outlived the grace period. The stop was still asked for —
		// force-close the stragglers and report clean: a signal-initiated stop
		// is never a failure (systemd would flag exit!=0 on TERM-under-load).
		_ = srv.Close()
	}
	<-errCh // Serve's http.ErrServerClosed once Shutdown/Close completes
	return nil
}

// openStore loads the client config and opens the local SQLite replica, creating
// the data dir if needed. It exits the process on failure (recording must be
// reliable and loud about misconfiguration). Returns the store, the loaded
// config, and a close func.
func openStore() (*sqlitestore.DB, config.Client, func()) {
	cfg, err := config.LoadClient("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas: load config:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "yas: create data dir:", err)
		os.Exit(1)
	}
	db, err := sqlitestore.Open(cfg.DBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas: open store:", err)
		os.Exit(1)
	}
	return db, cfg, func() { _ = db.Close() }
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// syncBatch caps how many records each push/pull round handles.
const syncBatch = 500

// syncLocal is the local store's sync capability (cursor + apply).
type syncLocal interface {
	Unsynced(ctx context.Context, limit int) ([]record.Record, error)
	MarkSynced(ctx context.Context, ids ...string) error
	Put(ctx context.Context, recs ...record.Record) error
	LastPulled(ctx context.Context) (int64, error)
	SetLastPulled(ctx context.Context, seq int64) error
}

// syncRemote is the server's sync API as seen by the client.
type syncRemote interface {
	Push(ctx context.Context, recs []record.Record) (syncproto.PushResponse, error)
	Pull(ctx context.Context, since int64, limit int) (syncproto.PullResponse, error)
}

// doSync pushes local unsynced records to the server, then pulls everything
// after the local cursor and applies it. Push runs first so the server has our
// latest before we apply its view, avoiding a stale overwrite of local changes.
func doSync(ctx context.Context, local syncLocal, remote syncRemote, batch int) (pushed, pulled int, err error) {
	for {
		recs, err := local.Unsynced(ctx, batch)
		if err != nil {
			return pushed, pulled, fmt.Errorf("read unsynced: %w", err)
		}
		if len(recs) == 0 {
			break
		}
		if _, err := remote.Push(ctx, recs); err != nil {
			return pushed, pulled, fmt.Errorf("push: %w", err)
		}
		if err := local.MarkSynced(ctx, recordIDs(recs)...); err != nil {
			return pushed, pulled, fmt.Errorf("mark synced: %w", err)
		}
		pushed += len(recs)
		if len(recs) < batch {
			break
		}
	}

	since, err := local.LastPulled(ctx)
	if err != nil {
		return pushed, pulled, fmt.Errorf("read cursor: %w", err)
	}
	for {
		resp, err := remote.Pull(ctx, since, batch)
		if err != nil {
			return pushed, pulled, fmt.Errorf("pull: %w", err)
		}
		if len(resp.Records) > 0 {
			if err := local.Put(ctx, resp.Records...); err != nil {
				return pushed, pulled, fmt.Errorf("apply pulled: %w", err)
			}
			// Pulled records already live on the server — mark them synced so
			// they aren't pushed straight back.
			if err := local.MarkSynced(ctx, recordIDs(resp.Records)...); err != nil {
				return pushed, pulled, fmt.Errorf("mark applied: %w", err)
			}
			pulled += len(resp.Records)
		}
		since = resp.NextSeq
		if err := local.SetLastPulled(ctx, since); err != nil {
			return pushed, pulled, fmt.Errorf("advance cursor: %w", err)
		}
		if resp.Done {
			break
		}
	}
	return pushed, pulled, nil
}

func recordIDs(recs []record.Record) []string {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
	}
	return ids
}

// cmdSync runs one push/pull sync cycle against the configured server.
func cmdSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	_ = fs.Parse(args)

	st, cfg, closeStore := openStore()
	defer closeStore()
	if cfg.ServerURL == "" {
		fmt.Fprintln(os.Stderr, "yas sync: no server configured (set server_url or YAS_SERVER_URL)")
		os.Exit(1)
	}
	if cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "yas sync: no token configured (set token or YAS_TOKEN)")
		os.Exit(1)
	}

	remote := syncclient.New(cfg.ServerURL, cfg.Token)
	pushed, pulled, err := doSync(context.Background(), st, remote, syncBatch)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas sync:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "yas sync: pushed %d, pulled %d\n", pushed, pulled)
}

// doImport writes parsed history records (see internal/histimport) into the
// store in batches, returning how many were imported and how many were skipped
// as already present — captured live by the hook, or previously imported (from
// any source: cross-source imports of the same event share a deterministic id).
func doImport(ctx context.Context, st recordStore, recs []record.Record) (imported, skipped int, err error) {
	covered, err := importCoverage(ctx, st, recs)
	if err != nil {
		return 0, 0, err
	}
	keep := recs[:0]
	for _, rec := range recs {
		if covered(rec) {
			skipped++
			continue
		}
		keep = append(keep, rec)
	}
	const batch = 500
	for i := 0; i < len(keep); i += batch {
		end := i + batch
		if end > len(keep) {
			end = len(keep)
		}
		if err := st.Put(ctx, keep[i:end]...); err != nil {
			return imported, skipped, err
		}
		imported += end - i
	}
	return imported, skipped, nil
}

// coverageEpoch splits real history timestamps from the synthetic ~1970 ones
// ParseZsh assigns to plain (non-extended) entries; synthetic entries can never
// coincide with a live capture, so they are neither scanned for nor checked.
var coverageEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// liveKeySet indexes SESSION-BEARING rows by (host, command, whole-second) so a
// candidate command can be tested for a live-hook capture within ±1s. zsh
// stamps whole seconds and the hook's clock may land just across a second
// boundary, so a candidate at second S is "covered" by a live row at S-1, S, or
// S+1 on the SAME host with the SAME command. This is the one definition of the
// live-dedup key, shared by importCoverage (skip import candidates already
// captured live) and doPruneLiveDupes (tombstone import skeletons already
// captured live) so the two matchers can never drift.
type liveKeySet map[string]struct{}

// liveKey is the set's composite key. A NUL byte can't appear in a hostname or
// a shell command, so the three fields join into one unambiguous string.
func liveKey(host, cmd string, sec int64) string {
	return host + "\x00" + strconv.FormatInt(sec, 10) + "\x00" + cmd
}

// add records a session-bearing row's (host, command, second).
func (s liveKeySet) add(host, cmd string, sec int64) {
	s[liveKey(host, cmd, sec)] = struct{}{}
}

// covers reports whether a session-bearing row exists on host with the same
// command within ±1s of sec.
func (s liveKeySet) covers(host, cmd string, sec int64) bool {
	for _, x := range []int64{sec - 1, sec, sec + 1} {
		if _, ok := s[liveKey(host, cmd, x)]; ok {
			return true
		}
	}
	return false
}

// importCoverage answers "is this history entry already in the store?" — by
// exact id, or as a live-hook capture of the same event.
//
// Exact id: cross-source imports of one event share a deterministic id, and
// the store's upsert is LWW on mutable fields — re-Putting an existing row
// would let a sparser source (zsh has no exit codes) wipe a richer row's
// metadata, and would dirty synced rows into needless re-pushes. A candidate
// whose id already exists is therefore skipped, never re-Put.
//
// Live capture: the hook records with a random UUIDv7, so id-matching alone
// cannot dedup an import against it (kata h4t6) — a re-run of `yas import`
// would add a session-less skeleton row next to every live record. One paged
// scan of the store over the import's time window builds a (host, command,
// second) set from SESSION-BEARING rows only (a session id is what the hook
// always sets and an import never does), and a candidate is covered when such
// a row exists on ITS host within ±1s — zsh stamps whole seconds and the
// hook's clock may land just across a second boundary. Sessionless rows from
// a previous import don't join this set: a distinct same-command run ±1s away
// in a later import pass is real history (import-vs-import dedup is the exact
// id match above, nothing wider).
func importCoverage(ctx context.Context, st recordStore, recs []record.Record) (func(record.Record) bool, error) {
	if len(recs) == 0 {
		return func(record.Record) bool { return false }, nil
	}
	// The scan window spans ALL candidates — including plain entries' synthetic
	// ~1970 timestamps, so their previously-imported rows are found by id.
	min, max := recs[0].StartTime, recs[0].StartTime
	for _, r := range recs[1:] {
		if r.StartTime.Before(min) {
			min = r.StartTime
		}
		if r.StartTime.After(max) {
			max = r.StartTime
		}
	}

	ids := make(map[string]struct{})
	live := make(liveKeySet)
	const page = 1000
	for offset := 0; ; offset += page {
		rows, err := st.Search(ctx, store.Query{
			Since: min.Add(-time.Second),
			Until: max.Add(2 * time.Second), // Until is exclusive; slop is harmless
			// Deletions must be honored, not resurrected: a tombstone covers
			// its id (and its live key) exactly like a visible row would.
			IncludeDeleted: true,
			Limit:          page,
			Offset:         offset,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			ids[row.ID] = struct{}{}
			if row.Session == "" {
				continue // import-created skeleton, not live coverage
			}
			live.add(row.Hostname, row.Command, row.StartTime.Unix())
		}
		if len(rows) < page {
			break
		}
	}

	return func(r record.Record) bool {
		if _, ok := ids[r.ID]; ok {
			return true // already imported (this or another source)
		}
		if r.StartTime.Before(coverageEpoch) {
			return false // synthetic timestamp can't coincide with live capture
		}
		return live.covers(r.Hostname, r.Command, r.StartTime.Unix())
	}, nil
}

// pruneBatch caps how many skeletons each prune round tombstones at once,
// mirroring histClear's clearBatch.
const pruneBatch = 500

// doPruneLiveDupes is the store-only maintenance path behind
// `yas import --prune-live-dupes`. It tombstones "import skeletons" — rows with
// no session AND no exit code, the exact signature a pre-h4t6 importer left
// beside every live-captured record — but ONLY those for which a session-bearing
// live capture of the SAME command exists on the SAME host within ±1s. Those
// skeletons are historical duplicates of real live rows; tombstoning them
// (re-Put with Deleted=true) re-marks them unsynced, so the prune propagates to
// Postgres and every replica on the next sync — which is the whole point.
//
// apply=false is a dry run: it only COUNTS the matches and mutates nothing.
// apply=true tombstones them. Already-deleted rows are skipped, so it is
// idempotent — a second apply run tombstones 0.
//
// Conservative by construction: the ONLY candidates are rows matching the
// skeleton signature exactly (Session=="" AND ExitCode==nil AND !Deleted), and
// the only ones tombstoned are those with a live ±1s same-host same-command
// peer (via the shared liveKeySet, identical to importCoverage). A skeleton with
// no live peer, and any session-bearing or exit-bearing row, is never touched.
func doPruneLiveDupes(ctx context.Context, st recordStore, apply bool) (n int, err error) {
	live := make(liveKeySet)
	var skeletons []record.Record
	const page = 1000
	for offset := 0; ; offset += page {
		// IncludeDeleted so a session-bearing row still covers its command after
		// a later tombstone (deletions are honored, not resurrected — parity with
		// importCoverage), and already-tombstoned skeletons are visible so the
		// candidate path can skip them. Nothing is mutated during the scan, so
		// offset paging is stable.
		rows, err := st.Search(ctx, store.Query{
			IncludeDeleted: true,
			Limit:          page,
			Offset:         offset,
		})
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if row.Session != "" {
				// A live capture covers its command even after a later tombstone,
				// exactly as importCoverage's set does (that's the shared-helper
				// invariant); add it BEFORE the Deleted check so a redacted live
				// row still prunes its skeleton duplicate.
				live.add(row.Hostname, row.Command, row.StartTime.Unix())
				continue // a live capture, never a candidate
			}
			if row.Deleted {
				continue // already-tombstoned skeleton — skip so prune stays idempotent
			}
			if row.ExitCode == nil {
				skeletons = append(skeletons, row) // exact import-skeleton signature
			}
		}
		if len(rows) < page {
			break
		}
	}

	// A skeleton can precede its live peer in scan order, so filter only after
	// the full scan has built the complete live set.
	var dupes []record.Record
	for _, s := range skeletons {
		if live.covers(s.Hostname, s.Command, s.StartTime.Unix()) {
			dupes = append(dupes, s)
		}
	}
	if !apply {
		return len(dupes), nil
	}
	for i := 0; i < len(dupes); i += pruneBatch {
		end := i + pruneBatch
		if end > len(dupes) {
			end = len(dupes)
		}
		m, err := tombstone(ctx, st, dupes[i:end])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// cmdImport implements `yas import --from <zsh-history|atuin> [--file <path>]`,
// backfilling the local store from an existing history source: a zsh history
// file (default ~/.zsh_history) or an atuin client database (default
// ~/.local/share/atuin/history.db, opened read-only).
func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	from := fs.String("from", "zsh-history", "history source format (zsh-history|atuin)")
	file := fs.String("file", "", "history source path (default: ~/.zsh_history, or ~/.local/share/atuin/history.db with --from atuin)")
	prune := fs.Bool("prune-live-dupes", false, "store-only maintenance: tombstone import skeletons already captured live (dry run unless --yes; ignores --from/--file)")
	yes := fs.Bool("yes", false, "apply --prune-live-dupes (without it the prune is a dry run that only counts)")
	_ = fs.Parse(args)

	// --prune-live-dupes is a PURE STORE OPERATION: it scans the local replica
	// and never reads a history file, so --from/--file are ignored.
	if *prune {
		cmdImportPrune(*yes)
		return
	}

	if *from != "zsh-history" && *from != "atuin" {
		fmt.Fprintf(os.Stderr, "yas import: unsupported --from %q (supported: zsh-history, atuin)\n", *from)
		os.Exit(2)
	}

	st, cfg, closeStore := openStore()
	defer closeStore()

	var (
		recs []record.Record
		path string
		err  error
	)
	switch *from {
	case "zsh-history":
		path = importPath(*file, ".zsh_history")
		var f *os.File
		if f, err = os.Open(path); err != nil {
			fmt.Fprintln(os.Stderr, "yas import:", err)
			os.Exit(1)
		}
		recs, err = histimport.ParseZsh(f, cfg.Hostname)
		_ = f.Close()
	case "atuin":
		// Each atuin row keeps its own hostname (multi-machine atuin dbs exist);
		// cfg.Hostname only fills rows that carry no host of their own.
		path = importPath(*file, ".local", "share", "atuin", "history.db")
		recs, err = histimport.ParseAtuin(path, cfg.Hostname)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas import:", err)
		os.Exit(1)
	}

	imported, skipped, err := doImport(context.Background(), st, recs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas import:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "yas import: imported %d entries from %s (%d skipped, already present)\n",
		imported, path, skipped)
}

// cmdImportPrune runs `yas import --prune-live-dupes` against the local store.
// It is dry-run by default (apply=false): it only counts and reports. With
// --yes (apply=true) it tombstones the matched skeletons. Reports go to stderr,
// matching the other import messages; exit is 0 on success.
func cmdImportPrune(apply bool) {
	st, _, closeStore := openStore()
	defer closeStore()

	n, err := doPruneLiveDupes(context.Background(), st, apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas import:", err)
		os.Exit(1)
	}
	if apply {
		fmt.Fprintf(os.Stderr, "yas import: tombstoned %d import skeleton%s already captured live\n",
			n, plural(n, "", "s"))
		return
	}
	fmt.Fprintf(os.Stderr, "yas import: would tombstone %d import skeleton%s already captured live (dry run; re-run with --yes to apply)\n",
		n, plural(n, "", "s"))
}

// importPath returns the explicit --file path when set, else the source's
// default location under $HOME.
func importPath(file string, underHome ...string) string {
	if file != "" {
		return file
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yas import: locate home:", err)
		os.Exit(1)
	}
	return filepath.Join(append([]string{home}, underHome...)...)
}

func usage() {
	fmt.Fprint(os.Stderr, `yas — local-first shell-history agent

usage:
  yas [n]                               default command: shortcut for `+"`yas history [n]`"+`
  yas record start  --command <c> [--cwd <d>] [--session <s>] [--shell <sh>] [--author <who>]
  yas record finish --id <id> --exit <n> [--duration-ms <ms>]
  yas search [text...] [--host h] [--cwd d] [--session s] [--exit n] [--failed] [--executor e]
              [--since t] [--until t] [--limit n] [--offset n] [--reverse] [--json] [--no-color] [--no-session]
  yas history [n]                       list the last n entries (default 100),
              [--time-format <layout>] [--no-time] [--no-exit] [--no-session] [--no-color] [--json]   numbered, oldest first
  yas history -d <offset|start-end>     delete an entry/range by its number
  yas history -c --yes                  delete ALL history (tombstones sync everywhere)
  yas session <token|session-id> [--json] [--no-color] [--time-format <layout>] [--no-time] [--no-exit]
  yas digest [--since t] [--until t] [--json]   today's commands grouped by host/dir, failures flagged
  yas serve  [--addr 127.0.0.1:8765]   localhost HTTP+JSON query API
  yas sync                              push/pull with the central server
  yas import [--from zsh-history|atuin] [--file <path>]   backfill from shell history or atuin
  yas import --prune-live-dupes [--yes]   tombstone import skeletons already captured live (dry run unless --yes)
  yas mcp    [--http <addr>] [--http-allow-insecure]   MCP server (read-only tools) over stdio/HTTP
  yas completion zsh                    print the zsh completion script (zsh only for now)
  yas version
`)
}
